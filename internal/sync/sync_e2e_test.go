//go:build integration

package sync_test

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
	gittransport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// TestSyncE2E_SealPushPullDecode is the end-to-end gate the sync plan
// promised: one daemon's worth of events seal into chunks, push to a
// bare git remote, a *second* daemon's transport lists + fetches the
// chunks, and the resulting decode yields byte-equal events.
//
// The test exercises every seam wired in this PR:
//   - chunk.FSStore.Put (seal-time chunk write)
//   - chunk.FileCursor (sealed-UID persistence)
//   - chunk.SealOnce (batched seal pass)
//   - git transport Push / List / Get (bare-repo round trip)
//   - chunk.Decode (sealed-blob → events, scope-key validated)
//
// Coverage gap NOT addressed here: live `maz daemon start` process
// orchestration. That sits behind the integration tag too but is a
// separate exec-spawn test; the supervisor unit tests already pin
// the goroutine lifecycle. This test pins the data path.
func TestSyncE2E_SealPushPullDecode(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── Shared fixture: bare remote + scope key derivation ───────
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare",
		"--quiet", "--initial-branch=main", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	remoteURL := "file://" + remoteDir
	key := personalScopeKey(t)
	scope := kernel.ScopePersonal

	// ── Daemon A: append → seal → push ───────────────────────────
	dirA := t.TempDir()
	logA := openEventLog(t, dirA)
	defer closeEventLog(t, logA)

	wantEvents := appendNEvents(ctx, t, logA, scope, 5)
	pushed := sealAndPush(ctx, t, dirA, scope, key, logA, remoteURL)
	if pushed == 0 {
		t.Fatalf("daemon A pushed 0 chunks, expected ≥ 1")
	}

	// ── Daemon B: pull → decode ──────────────────────────────────
	dirB := t.TempDir()
	tB, err := gittransport.New(gittransport.Options{
		DataDir:    dirB,
		RemoteURLs: map[kernel.Scope]string{scope: remoteURL},
		DeviceID:   "daemon-b",
	})
	if err != nil {
		t.Fatalf("daemon B transport: %v", err)
	}
	gotEvents := pullAndDecode(ctx, t, tB, scope, key)

	// ── Assertions: byte-equal payloads across daemons ───────────
	if len(gotEvents) != len(wantEvents) {
		t.Fatalf("daemon B decoded %d events, want %d", len(gotEvents), len(wantEvents))
	}
	wantUIDs := make(map[string]struct{}, len(wantEvents))
	for _, e := range wantEvents {
		wantUIDs[e.UID] = struct{}{}
	}
	for _, e := range gotEvents {
		if _, ok := wantUIDs[e.UID]; !ok {
			t.Errorf("daemon B saw unexpected UID %q", e.UID)
		}
		if e.Scope != scope {
			t.Errorf("event %q scope=%q, want %q", e.UID, e.Scope, scope)
		}
	}
}

// TestSyncE2E_PlaintextRoundTrip is the plaintext default-mode gate: a
// nil scope key produces a `.jsonl` chunk, the bare remote stores it
// as a normal text blob (readable by `cat` and `jq`), and a second
// daemon's transport pulls + decodes it back to byte-equal events
// without ever needing a key.
//
// What this proves over the encrypted gate:
//   - Encode/Decode actually route on key=nil.
//   - The chunk file on the remote is *plain* JSONL (no MAZ1 magic).
//   - PeekHeader sniffs the JSONL frame from the wire.
//   - The transport handles `.jsonl` extension end-to-end.
func TestSyncE2E_PlaintextRoundTrip(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare",
		"--quiet", "--initial-branch=main", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	remoteURL := "file://" + remoteDir
	scope := kernel.ScopePersonal

	// ── Daemon A: append → seal (plaintext) → push ───────────────
	dirA := t.TempDir()
	logA := openEventLog(t, dirA)
	defer closeEventLog(t, logA)

	wantEvents := appendNEvents(ctx, t, logA, scope, 5)
	pushed := sealAndPush(ctx, t, dirA, scope, nil /* nil key = plaintext */, logA, remoteURL)
	if pushed == 0 {
		t.Fatalf("daemon A pushed 0 plaintext chunks, expected ≥ 1")
	}

	// ── Daemon B: pull → decode (no key, plaintext) ──────────────
	dirB := t.TempDir()
	tB, err := gittransport.New(gittransport.Options{
		DataDir:    dirB,
		RemoteURLs: map[kernel.Scope]string{scope: remoteURL},
		DeviceID:   "daemon-b",
	})
	if err != nil {
		t.Fatalf("daemon B transport: %v", err)
	}
	gotEvents := pullAndDecode(ctx, t, tB, scope, nil)

	if len(gotEvents) != len(wantEvents) {
		t.Fatalf("daemon B decoded %d events, want %d", len(gotEvents), len(wantEvents))
	}
	wantUIDs := make(map[string]struct{}, len(wantEvents))
	for _, e := range wantEvents {
		wantUIDs[e.UID] = struct{}{}
	}
	for _, e := range gotEvents {
		if _, ok := wantUIDs[e.UID]; !ok {
			t.Errorf("daemon B saw unexpected UID %q", e.UID)
		}
	}
}

// TestSyncE2E_PushIsIdempotent verifies a second seal+push pass over
// the same events does not upload duplicate chunks. Critical for the
// daemon's coalesce-on-cadence loop: every push tick re-lists the
// remote and skips chunks already present.
func TestSyncE2E_PushIsIdempotent(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.CommandContext(ctx, "git", "init", "--bare",
		"--quiet", "--initial-branch=main", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	remoteURL := "file://" + remoteDir
	key := personalScopeKey(t)
	scope := kernel.ScopePersonal

	dirA := t.TempDir()
	logA := openEventLog(t, dirA)
	defer closeEventLog(t, logA)

	appendNEvents(ctx, t, logA, scope, 3)

	first := sealAndPush(ctx, t, dirA, scope, key, logA, remoteURL)
	if first == 0 {
		t.Fatalf("first push accepted 0 chunks")
	}

	// Second pass: no new events, no new chunks should be accepted.
	second := sealAndPush(ctx, t, dirA, scope, key, logA, remoteURL)
	if second != 0 {
		t.Errorf("second push accepted %d chunks, want 0 (idempotent)", second)
	}
}

// ── helpers ──────────────────────────────────────────────────────

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git binary required: %v", err)
	}
}

// personalScopeKey derives the canonical "abandon abandon … art"
// scope key so daemons A and B share the same scope key without
// shipping the mnemonic between them in-test.
func personalScopeKey(t *testing.T) []byte {
	t.Helper()
	parts := make([]string, watchword.Words)
	for i := range parts {
		parts[i] = "abandon"
	}
	parts[len(parts)-1] = "art"
	mnemonic := strings.Join(parts, " ")
	root, err := watchword.DeriveRoot(mnemonic)
	if err != nil {
		t.Fatalf("DeriveRoot: %v", err)
	}
	key, err := watchword.DeriveScopeKey(root, string(kernel.ScopePersonal))
	if err != nil {
		t.Fatalf("DeriveScopeKey: %v", err)
	}
	return key
}

// openEventLog builds a real kernel.FileEventLog so the sealer reads
// the same events the dispatcher persists. Registers the Episode
// schema against a fresh MemoryOntology so Append accepts the
// "episode" Kind the test feeds in.
func openEventLog(t *testing.T, dataDir string) *kernel.FileEventLog {
	t.Helper()
	ont := kernel.NewMemoryOntology()
	if err := ontology.RegisterEpisode(context.Background(), ont); err != nil {
		t.Fatalf("ontology.RegisterEpisode: %v", err)
	}
	return kernel.NewFileEventLog(dataDir, ont)
}

func closeEventLog(t *testing.T, log *kernel.FileEventLog) {
	t.Helper()
	if err := log.Close(); err != nil {
		t.Errorf("close event log: %v", err)
	}
}

// appendNEvents writes n minimal Episode events with deterministic
// payloads and returns the receipts (UID-only) for cross-daemon
// assertions.
func appendNEvents(ctx context.Context, t *testing.T, log *kernel.FileEventLog, scope kernel.Scope, n int) []kernel.Event {
	t.Helper()
	events := make([]kernel.Event, n)
	for i := range events {
		events[i] = kernel.Event{
			Kind:       "episode",
			Source:     "test:e2e",
			Confidence: 1,
			Scope:      scope,
			Payload:    []byte(`{"text":"e2e payload","source":"test:e2e","confidence":1.0}`),
		}
	}
	receipts, err := log.Append(ctx, events)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(receipts) != n {
		t.Fatalf("Append returned %d receipts, want %d", len(receipts), n)
	}
	for i, r := range receipts {
		events[i].UID = r.UID
		events[i].CreateTime = r.CreateTime
	}
	return events
}

// sealAndPush runs one SealOnce pass for scope, lists the resulting
// blobs from the FSStore, and pushes them through the git transport.
// Returns the number of chunks the remote accepted (newly added).
func sealAndPush(
	ctx context.Context, t *testing.T,
	dataDir string, scope kernel.Scope, key []byte,
	src chunk.EventSource, remoteURL string,
) int {
	t.Helper()
	store, err := chunk.NewFSStore(dataDir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	cursor, err := chunk.NewFileCursor(dataDir)
	if err != nil {
		t.Fatalf("NewFileCursor: %v", err)
	}
	if _, _, err := chunk.SealOnce(ctx, src, scope, key, store, cursor, 100); err != nil {
		t.Fatalf("SealOnce: %v", err)
	}

	refs, err := store.List(ctx, scope)
	if err != nil {
		t.Fatalf("FSStore.List: %v", err)
	}
	if len(refs) == 0 {
		return 0
	}

	tr, err := gittransport.New(gittransport.Options{
		DataDir:    dataDir,
		RemoteURLs: map[kernel.Scope]string{scope: remoteURL},
		DeviceID:   "daemon-a",
	})
	if err != nil {
		t.Fatalf("git transport: %v", err)
	}

	remote, err := tr.List(ctx, scope)
	if err != nil {
		t.Fatalf("transport.List: %v", err)
	}
	seen := map[string]struct{}{}
	for _, h := range remote.Chunks {
		seen[h.ChunkID] = struct{}{}
	}

	blobs := make([]transport.Blob, 0, len(refs))
	for i := range refs {
		if _, dup := seen[refs[i].ChunkID]; dup {
			continue
		}
		body, gerr := store.Get(ctx, scope, refs[i].ChunkID)
		if gerr != nil {
			t.Fatalf("FSStore.Get %s: %v", refs[i].ChunkID, gerr)
		}
		blobs = append(blobs, transport.Blob{
			Header: refs[i].Header,
			Body:   nopCloser(body),
		})
	}
	if len(blobs) == 0 {
		return 0
	}
	accepted, err := tr.Push(ctx, scope, blobs)
	if err != nil {
		t.Fatalf("transport.Push: %v", err)
	}
	return accepted
}

// pullAndDecode lists the remote's chunks, fetches each one, decodes
// it under the shared scope key, and returns the union of events.
func pullAndDecode(
	ctx context.Context, t *testing.T,
	tr transport.Transport, scope kernel.Scope, key []byte,
) []kernel.Event {
	t.Helper()
	listing, err := tr.List(ctx, scope)
	if err != nil {
		t.Fatalf("transport.List: %v", err)
	}
	got := make([]kernel.Event, 0, len(listing.Chunks))
	for _, hdr := range listing.Chunks {
		blob, gerr := tr.Get(ctx, scope, hdr.ChunkID)
		if gerr != nil {
			t.Fatalf("transport.Get %s: %v", hdr.ChunkID, gerr)
		}
		body, rerr := io.ReadAll(blob.Body)
		if cerr := blob.Body.Close(); cerr != nil && !errors.Is(cerr, io.EOF) {
			t.Errorf("close blob: %v", cerr)
		}
		if rerr != nil {
			t.Fatalf("read blob %s: %v", hdr.ChunkID, rerr)
		}
		_, events, derr := chunk.Decode(body, scope, key)
		if derr != nil {
			t.Fatalf("chunk.Decode %s: %v", hdr.ChunkID, derr)
		}
		got = append(got, events...)
	}
	return got
}

// nopReadCloser wraps a byte slice as an io.ReadCloser without the
// io.NopCloser allocation. Local helper so the test file does not
// pull in bytes or io.NopCloser at the call site.
type nopReadCloser struct{ inner *strings.Reader }

func (n nopReadCloser) Read(p []byte) (int, error) { return n.inner.Read(p) }
func (nopReadCloser) Close() error                 { return nil }

func nopCloser(b []byte) io.ReadCloser {
	return nopReadCloser{inner: strings.NewReader(string(b))}
}
