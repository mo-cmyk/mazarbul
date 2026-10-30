package vault_test

// watcher_test.go exercises the four loop-safety invariants from the P7 spec
// against the real four-layer stack (FileEventLog → dispatcher → SQLite + FTS5
// + sqlite-vec + LadybugDB). The Watcher's testable seam (ReconcilePath) is
// driven directly so the assertions are deterministic without depending on
// real OS fsnotify events or sleep-based synchronization.
//
// All tests run with -race (ensured by the CI `mise run test` gate).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/vault"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// ---- helpers ---------------------------------------------------------------

// fakeEmbedder returns a deterministic zero vector so the dispatcher is happy
// without requiring a live Ollama instance.
type fakeEmbedder struct{ dim int }

func (f fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return make([]float32, f.dim), nil
}

type noopOntology struct{}

func (noopOntology) Schema(context.Context, string) (kernel.Schema, error) {
	return kernel.Schema{}, kernel.ErrNotImplemented
}
func (noopOntology) Register(context.Context, kernel.Schema) error { return kernel.ErrNotImplemented }
func (noopOntology) List(context.Context) ([]kernel.Schema, error) {
	return nil, kernel.ErrNotImplemented
}
func (noopOntology) Validate(context.Context, string, json.RawMessage) error { return nil }

// watchStack is the full wired test world for P7 tests.
type watchStack struct {
	ctx       context.Context
	log       *kernel.FileEventLog
	nav       *navigate.Navigator
	im        *document.Importer
	projector *vault.Projector
	writeback *vault.Writeback
	watcher   *vault.Watcher
	vaultRoot string
	dis       *dispatcher.Dispatcher
}

func newWatchStack(t *testing.T) *watchStack {
	t.Helper()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	vaultRoot := t.TempDir()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.DebugLevel))
	return newWatchStackAt(t, ctx, dataDir, vaultRoot, logger)
}

// newWatchStackAt is the parameterised constructor behind newWatchStack: it
// lets a test pre-populate the vault root (e.g. create nested subdirectories)
// before the watcher is built, which matters for tests that exercise the
// startup tree-walk.
func newWatchStackAt(t *testing.T, ctx context.Context, dataDir, vaultRoot string, logger *zap.Logger) *watchStack {
	t.Helper()

	dis, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = dis.Close() })

	elog := kernel.NewFileEventLog(dataDir, noopOntology{}, kernel.WithStorage(dis))
	t.Cleanup(func() { _ = elog.Close() })

	reader, err := dis.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	im := document.NewImporter(elog, nav, logger)
	projector := vault.NewProjector(nav, vaultRoot, logger)
	writeback := vault.NewWriteback(projector, logger)

	watcher, err := vault.NewWatcher(vault.WatcherConfig{
		Root:     vaultRoot,
		Ingester: im,
		Notifier: writeback,
		Log:      logger,
	})
	if err != nil {
		t.Fatalf("vault.NewWatcher: %v", err)
	}

	return &watchStack{
		ctx:       ctx,
		log:       elog,
		nav:       nav,
		im:        im,
		projector: projector,
		writeback: writeback,
		watcher:   watcher,
		vaultRoot: vaultRoot,
		dis:       dis,
	}
}

// testScope is the partition every watcher/loop test operates in. The vault is
// laid out as <root>/<scope>/<path>, so the helpers below write and read under
// this scope segment and the seam calls (ReconcilePath, SyncPath, Notify) pass
// it explicitly.
const testScope = "personal"

// writeVaultFile writes raw bytes to the vault at the given relative path under
// testScope using a non-atomic write (simulating $EDITOR behavior). The watcher
// is driven via ReconcilePath, not via fsnotify, so the write method does not
// matter for loop-safety tests.
func writeVaultFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, testScope, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// countRevisions returns the number of documentRevision events for the
// document at the given vault-relative path by scanning the event log.
func countRevisions(t *testing.T, s *watchStack, rel string) int {
	t.Helper()
	doc, err := s.nav.GetDocumentByPath(s.ctx, rel)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return 0
		}
		t.Fatalf("GetDocumentByPath(%q): %v", rel, err)
	}
	wantUID := doc.DocumentUID
	kinds := []kernel.EventKind{kernel.EventKind(ontology.KindDocumentRevision)}
	count := 0
	for ev, evErr := range s.log.Read(s.ctx, kernel.ReadOpts{Kinds: kinds}) {
		if evErr != nil {
			t.Fatalf("event log read: %v", evErr)
		}
		var p ontology.DocumentRevision
		if json.Unmarshal(ev.Payload, &p) == nil && p.DocumentUID == wantUID {
			count++
		}
	}
	return count
}

// driveWriteback synchronously drains one pending notification from the
// writeback by calling SyncPath directly. This avoids depending on the
// ticker-based writeback loop timing in tests.
func driveWriteback(t *testing.T, s *watchStack, rel string) {
	t.Helper()
	_, err := s.projector.SyncPath(s.ctx, rel, testScope)
	if err != nil && err.Error() != navigate.ErrNotFound.Error() {
		t.Fatalf("SyncPath(%q): %v", rel, err)
	}
}

// readVaultFile reads the on-disk vault file at the given relative path under
// testScope's partition.
func readVaultFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, testScope, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read vault file %s: %v", rel, err)
	}
	return string(b)
}

// ---- test cases ------------------------------------------------------------

// TestWatcher_SingleFileWriteProducesExactlyOneRevision (case a)
// A single external file write → EXACTLY ONE new revision; no infinite loop;
// the writeback's SyncPath after the ingest is a no-op (I1 hash-equal skip).
func TestWatcher_SingleFileWriteProducesExactlyOneRevision(t *testing.T) {
	s := newWatchStack(t)
	const rel = "notes/alpha.md"
	const body = "# Alpha\n\nFirst revision.\n"

	writeVaultFile(t, s.vaultRoot, rel, body)

	// Drive the watcher seam directly (deterministic, no OS event needed).
	s.watcher.ReconcilePath(s.ctx, testScope, rel)

	// Exactly one revision must be stored.
	if got := countRevisions(t, s, rel); got != 1 {
		t.Fatalf("after one file write: revisions = %d, want 1", got)
	}

	// Drive the writeback manually to simulate the materialise-after-ingest path.
	// The writeback must NOT produce a second revision (I1 hash-equal skip).
	driveWriteback(t, s, rel)

	if got := countRevisions(t, s, rel); got != 1 {
		t.Fatalf("after writeback reconcile: revisions = %d, want 1 (loop detected)", got)
	}
}

// TestWatcher_ExternalRevisionRematerializesFileWithoutReIngest (case b)
// An externally-appended revision (via ImportContent, simulating writeDocument)
// → the file re-materializes on disk with byte-identical content; the watcher
// does NOT re-ingest it (I1 hash-equal skip).
func TestWatcher_ExternalRevisionRematerializesFileWithoutReIngest(t *testing.T) {
	s := newWatchStack(t)
	const rel = "notes/beta.md"
	const body = "# Beta\n\nBody from an MCP tool write.\n"

	// External ingest (simulates writeDocument MCP tool).
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, rel, []byte(body)); err != nil {
		t.Fatalf("ImportContent: %v", err)
	}

	// Revision count before writeback: 1.
	if got := countRevisions(t, s, rel); got != 1 {
		t.Fatalf("after external ingest: revisions = %d, want 1", got)
	}

	// Drive the writeback to materialise the file.
	driveWriteback(t, s, rel)

	// On-disk bytes must be byte-identical to the ingested body.
	if got := readVaultFile(t, s.vaultRoot, rel); got != body {
		t.Fatalf("on-disk body = %q, want %q", got, body)
	}

	// Now call ReconcilePath on the freshly materialised file: the watcher must
	// detect hash equality and skip — no second revision (I1 cycle break).
	s.watcher.ReconcilePath(s.ctx, testScope, rel)

	if got := countRevisions(t, s, rel); got != 1 {
		t.Fatalf("after watcher reconcile of materialised file: revisions = %d, want 1 (loop detected)", got)
	}
}

// TestWatcher_ConcurrentAgentWriteAndFileEditLastULIDWins (case c)
// Concurrent agent write (new revision via ImportContent) + a file edit →
// last-ULID-wins; the ingested edit is not clobbered by a stale materialize (I2).
func TestWatcher_ConcurrentAgentWriteAndFileEditLastULIDWins(t *testing.T) {
	s := newWatchStack(t)
	const rel = "notes/gamma.md"
	const agentBody = "# Gamma\n\nAgent version.\n"
	const editorBody = "# Gamma\n\nEditor version (later edit).\n"

	// Step 1: agent writes a revision via ImportContent.
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, rel, []byte(agentBody)); err != nil {
		t.Fatalf("ImportContent (agent): %v", err)
	}
	if got := countRevisions(t, s, rel); got != 1 {
		t.Fatalf("after agent write: revisions = %d, want 1", got)
	}

	// Step 2: user edits the file in $EDITOR BEFORE the writeback materialises
	// the agent's version. The on-disk bytes differ from the stored revision.
	writeVaultFile(t, s.vaultRoot, rel, editorBody)

	// Step 3: drive the writeback. The projector must detect that the on-disk
	// bytes do not match its last-projected hash (it never wrote anything here),
	// classify the file as a pending edit, and skip the materialize (I2).
	_, err := s.projector.SyncPath(s.ctx, rel, testScope)
	if err != nil && err.Error() != navigate.ErrNotFound.Error() {
		t.Fatalf("SyncPath: %v", err)
	}

	// The editor's bytes must survive on disk (I2: never clobber un-ingested edit).
	if got := readVaultFile(t, s.vaultRoot, rel); got != editorBody {
		t.Fatalf("on-disk body = %q after writeback, want editor version %q (I2 violated)", got, editorBody)
	}

	// Step 4: drive the watcher. The editor's version is new → a second revision
	// is appended. Last-ULID-wins is satisfied: the editor version is now head.
	s.watcher.ReconcilePath(s.ctx, testScope, rel)

	if got := countRevisions(t, s, rel); got != 2 {
		t.Fatalf("after watcher ingest of editor version: revisions = %d, want 2", got)
	}

	// Head revision body must be the editor version.
	doc, err := s.nav.GetDocumentByPath(s.ctx, rel)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	// GetDocumentByPath returns metadata; fetch full body via GetDocumentByUID.
	full, err := s.nav.GetDocumentByUID(s.ctx, doc.DocumentUID)
	if err != nil {
		t.Fatalf("GetDocumentByUID: %v", err)
	}
	if full.Body != editorBody {
		t.Fatalf("head body = %q, want editor version %q", full.Body, editorBody)
	}
}

// TestWatcher_CrashRestartDoesNotDoubleIngest (case d)
// Crash-restart simulation: replaying ReconcilePath on an already-ingested file
// must not append a second revision. Content-hash dedup in ImportContent is the
// guarantee (I1).
func TestWatcher_CrashRestartDoesNotDoubleIngest(t *testing.T) {
	s := newWatchStack(t)
	const rel = "notes/delta.md"
	const body = "# Delta\n\nOriginal.\n"

	writeVaultFile(t, s.vaultRoot, rel, body)

	// First ingest (first "daemon run").
	s.watcher.ReconcilePath(s.ctx, testScope, rel)
	if got := countRevisions(t, s, rel); got != 1 {
		t.Fatalf("first ingest: revisions = %d, want 1", got)
	}

	// Simulate crash-restart: reconcile the same file again (daemon re-watches
	// on boot and sees the unchanged file). Must be a no-op.
	s.watcher.ReconcilePath(s.ctx, testScope, rel)
	if got := countRevisions(t, s, rel); got != 1 {
		t.Fatalf("after restart reconcile: revisions = %d, want 1 (double-ingest detected)", got)
	}
}

// TestWatcher_RaceReconcileAndWriteback exercises both the watcher and writeback
// concurrently to verify the -race detector finds no data races in the shared
// projector state and navigator.
func TestWatcher_RaceReconcileAndWriteback(t *testing.T) {
	s := newWatchStack(t)
	const rel = "notes/epsilon.md"

	writeVaultFile(t, s.vaultRoot, rel, "# Epsilon\n\nInitial.\n")
	s.watcher.ReconcilePath(s.ctx, testScope, rel)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.watcher.ReconcilePath(s.ctx, testScope, rel)
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			driveWriteback(t, s, rel)
		}()
	}
	wg.Wait()

	// At most one revision must exist (all reconciles see the same hash).
	if got := countRevisions(t, s, rel); got != 1 {
		t.Fatalf("after concurrent reconcile+writeback: revisions = %d, want 1", got)
	}
}

// TestWriteback_NotifyDoesNotBlock verifies Notify never blocks under a heavy
// burst. The queue is a path-keyed set behind a non-blocking wake signal, so a
// flood of notifications neither blocks the caller nor (for distinct paths)
// drops work.
func TestWriteback_NotifyDoesNotBlock(t *testing.T) {
	s := newWatchStack(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 512 {
			s.writeback.Notify(testScope, "any/path.md")
		}
	}()
	select {
	case <-done:
		// pass
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked for >2s under a burst")
	}
}

// TestWriteback_NotifyAfterStopIsNoOp verifies that calling Notify after Stop
// neither panics (the old channel-based queue could panic on a closed-channel
// send) nor blocks. This is the shutdown-safety regression for the
// stop-watcher-first teardown ordering in daemon wiring.
func TestWriteback_NotifyAfterStopIsNoOp(t *testing.T) {
	s := newWatchStack(t)
	s.writeback.Stop()
	// A second Stop must be idempotent, and Notify after Stop must be a silent
	// no-op (no panic, no block).
	s.writeback.Stop()
	s.writeback.Notify(testScope, "late/edit.md")
	s.writeback.Notify(testScope, "another/edit.md")
}

// TestWriteback_DrainsAllDistinctPathsUnderBurst is the lossless-queue
// regression for the bounded-channel drop: a burst across many DISTINCT paths
// (more than any former buffer size) must all reach SyncPath. Each path is
// ingested first, then notified in a burst; after the worker runs, every path's
// sidecar entry must be present.
func TestWriteback_DrainsAllDistinctPathsUnderBurst(t *testing.T) {
	s := newWatchStack(t)

	const n = 400 // exceeds the former 256-entry channel buffer
	paths := make([]string, n)
	for i := range paths {
		rel := "burst/doc" + strconv.Itoa(i) + ".md"
		paths[i] = rel
		if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, rel, []byte("# Doc "+strconv.Itoa(i)+"\n\nbody\n")); err != nil {
			t.Fatalf("ImportContent(%q): %v", rel, err)
		}
	}

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	go s.writeback.Run(ctx)
	t.Cleanup(s.writeback.Stop)

	// Fire the whole burst before the worker can finish draining.
	for _, rel := range paths {
		s.writeback.Notify(testScope, rel)
	}

	// Every distinct path must eventually materialise (no drops).
	// 60 s: in isolation the worker finishes in ~28 s; under full parallel
	// CI load (race, all packages) each SyncPath call competes for the CGO
	// lock — the 10 s limit was observed to expire before all 400 paths drained.
	deadline := time.Now().Add(60 * time.Second)
	for {
		sidecar := readSidecar(s.vaultRoot)
		missing := 0
		for _, rel := range paths {
			// The sidecar keys by "<scope>/<rel>" now that the vault is partitioned.
			if _, ok := sidecar[testScope+"/"+rel]; !ok {
				missing++
			}
		}
		if missing == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("writeback dropped paths under burst: %d/%d never materialised", missing, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
