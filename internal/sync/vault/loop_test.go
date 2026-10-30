package vault_test

// loop_test.go contains two tests that close the coverage gap left by the
// seam-only watcher_test.go:
//
//  1. TestLiveLoop_SingleWriteProducesExactlyOneRevision: exercises the REAL
//     Watcher.Run + Writeback.Run goroutines (not just ReconcilePath) against a
//     temp vault with one shared Projector, writes a file via os.WriteFile and
//     drives the ingest via ReconcilePath (which is what Run calls internally),
//     then lets the Writeback goroutine materialise it. Asserts exactly one
//     revision lands and no rewrite cycle occurs. Uses deterministic polling
//     (no fixed sleeps) capped by a 5-second timeout.
//
//  2. TestSharedProjector_SidecarRetainsBothPaths: the sidecar (state.json)
//     regression test for the two-projector bug. With a shared Projector,
//     concurrent SyncPath calls for two independent paths must both survive in
//     the sidecar. The test would FAIL if two separate Projector instances were
//     used (each reading a stale sidecar snapshot and racing their os.Rename
//     writes on .mazarbul/state.json).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// pollUntil repeatedly calls check until it returns true or the deadline
// expires. It yields the goroutine between attempts to avoid busy-spinning.
func pollUntil(t *testing.T, deadline time.Duration, check func() bool) bool {
	t.Helper()
	until := time.Now().Add(deadline)
	for time.Now().Before(until) {
		if check() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// readSidecar decodes the .mazarbul/state.json sidecar from root and returns
// the projected map. An absent sidecar returns an empty map.
func readSidecar(root string) map[string]string {
	raw, err := os.ReadFile(filepath.Join(root, ".mazarbul", "state.json"))
	if err != nil {
		return map[string]string{}
	}
	var st struct {
		Projected map[string]string `json:"projected"`
	}
	if json.Unmarshal(raw, &st) != nil || st.Projected == nil {
		return map[string]string{}
	}
	return st.Projected
}

// TestLiveLoop_SingleWriteProducesExactlyOneRevision starts the real
// Watcher.Run + Writeback.Run goroutines (the production loop) against a temp
// vault backed by a single shared Projector. It writes a markdown file, drives
// ingest via ReconcilePath (the same codepath Run uses internally), then
// waits — by polling the event log — for exactly one revision and confirms
// no rewrite cycle occurs.
func TestLiveLoop_SingleWriteProducesExactlyOneRevision(t *testing.T) {
	s := newWatchStack(t)
	logger := zaptest.NewLogger(t)

	watcherCtx, cancelWatcher := context.WithCancel(s.ctx)
	writebackCtx, cancelWriteback := context.WithCancel(s.ctx)
	t.Cleanup(func() {
		cancelWatcher()
		cancelWriteback()
		s.writeback.Stop()
	})

	// Start the production goroutines.
	go s.writeback.Run(writebackCtx)
	go func() {
		if err := s.watcher.Run(watcherCtx); err != nil {
			logger.Error("watcher exited", zap.Error(err))
		}
	}()

	const rel = "live/zeta.md"
	const body = "# Zeta\n\nLive loop test.\n"

	writeVaultFile(t, s.vaultRoot, rel, body)

	// Drive ingest through the same codepath Run uses — ReconcilePath. This is
	// deterministic: we know the ingest has completed before we check the count.
	s.watcher.ReconcilePath(s.ctx, testScope, rel)

	// After the ingest there must be exactly one revision.
	if got := countRevisions(t, s, rel); got != 1 {
		t.Fatalf("after ingest: revisions = %d, want 1", got)
	}

	// The Writeback goroutine will materialise the document. Wait (via poll)
	// until the sidecar registers the path — proof the writeback ran.
	if !pollUntil(t, 5*time.Second, func() bool {
		m := readSidecar(s.vaultRoot)
		// The sidecar keys by "<scope>/<rel>" now that the vault is partitioned.
		_, ok := m[testScope+"/"+rel]
		return ok
	}) {
		t.Fatal("timed out waiting for writeback to materialise the file (sidecar never recorded the path)")
	}

	// After writeback, ReconcilePath again. The hash equals the stored revision,
	// so ImportContent must return Skipped — no second revision appended.
	s.watcher.ReconcilePath(s.ctx, testScope, rel)

	if got := countRevisions(t, s, rel); got != 1 {
		t.Fatalf("after second reconcile: revisions = %d, want 1 (live loop cycle detected)", got)
	}
}

// TestSharedProjector_SidecarRetainsBothPaths is the sidecar-clobber regression
// test for the two-Projector bug described in daemon/wiring.go. With ONE shared
// Projector, concurrent SyncPath calls for two independent paths must both
// survive in the sidecar — neither clobbers the other.
//
// Why this would fail with two projectors: each Projector reads the sidecar into
// its own in-memory snapshot under its own mutex, modifies that snapshot, and
// atomically renames a temp file over state.json. Whichever rename lands second
// wins and silently discards the other projector's path entry. One projector +
// one mutex ensures the read-modify-write is fully serialized.
func TestSharedProjector_SidecarRetainsBothPaths(t *testing.T) {
	s := newWatchStack(t)

	const pathA = "shared/alpha.md"
	const pathB = "shared/beta.md"
	const bodyA = "# Alpha\n\nShared projector test.\n"
	const bodyB = "# Beta\n\nShared projector test.\n"

	// Ingest both documents into the event log.
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, pathA, []byte(bodyA)); err != nil {
		t.Fatalf("ImportContent(%q): %v", pathA, err)
	}
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, pathB, []byte(bodyB)); err != nil {
		t.Fatalf("ImportContent(%q): %v", pathB, err)
	}

	// Drive SyncPath for both paths concurrently through the SAME projector.
	// With a single Projector the mutex serializes the two sidecar writes; both
	// entries must survive.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := s.projector.SyncPath(s.ctx, pathA, testScope); err != nil {
			t.Errorf("SyncPath(%q): %v", pathA, err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := s.projector.SyncPath(s.ctx, pathB, testScope); err != nil {
			t.Errorf("SyncPath(%q): %v", pathB, err)
		}
	}()
	wg.Wait()

	sidecar := readSidecar(s.vaultRoot)

	// The sidecar keys by "<scope>/<rel>" now that the vault is partitioned.
	if _, ok := sidecar[testScope+"/"+pathA]; !ok {
		t.Errorf("sidecar missing entry for %q (clobbered by concurrent SyncPath for %q)", pathA, pathB)
	}
	if _, ok := sidecar[testScope+"/"+pathB]; !ok {
		t.Errorf("sidecar missing entry for %q (clobbered by concurrent SyncPath for %q)", pathB, pathA)
	}
	if t.Failed() {
		t.Logf("sidecar contents: %v", sidecar)
	}
}

// TestLiveLoop_IngestInExistingNestedSubdir is the regression for "watch
// existing subdirs": a markdown file saved under a nested folder that existed
// BEFORE the watcher started must still be ingested. fsnotify is not recursive,
// so without addTree walking the tree at startup the inner save would emit no
// event and the document would never reach the event log.
func TestLiveLoop_IngestInExistingNestedSubdir(t *testing.T) {
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()
	vaultRoot := t.TempDir()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.DebugLevel))

	// Pre-create a nested subdirectory (under its scope partition) BEFORE the
	// watcher is constructed so it is only picked up by the startup tree-walk,
	// never a Create event.
	nestedRel := "notes/deep"
	if err := os.MkdirAll(filepath.Join(vaultRoot, testScope, nestedRel), 0o700); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	s := newWatchStackAt(t, ctx, dataDir, vaultRoot, logger)

	watcherCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		if err := s.watcher.Run(watcherCtx); err != nil {
			logger.Error("watcher exited", zap.Error(err))
		}
	}()

	const rel = "notes/deep/nested.md"
	const body = "# Nested\n\nSaved in a pre-existing subdir.\n"
	writeVaultFile(t, vaultRoot, rel, body)

	// The watcher must ingest the nested file via the real fsnotify path.
	if !pollUntil(t, 5*time.Second, func() bool {
		return countRevisions(t, s, rel) == 1
	}) {
		t.Fatalf("nested file in existing subdir never ingested (revisions=%d, want 1)", countRevisions(t, s, rel))
	}
}
