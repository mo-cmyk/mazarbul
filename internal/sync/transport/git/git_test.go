package git_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
	git "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// requireGit skips the test if the git binary is unavailable; CI runs
// always have git but some sandboxed harnesses might not.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not in PATH: %v", err)
	}
}

// newBareRemote initialises an empty bare git repository under
// t.TempDir() and returns a file:// URL pointing at it. Suitable as a
// stand-in for github / gitea / self-hosted in unit tests.
func newBareRemote(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.CommandContext(t.Context(), "git", "init", "--bare", "--quiet", "--initial-branch=main", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	return "file://" + dir
}

// scopeKey derives a deterministic personal-scope key for tests so
// chunks created across two Transport instances roundtrip cleanly.
func scopeKey(t *testing.T) []byte {
	t.Helper()
	parts := make([]string, watchword.Words)
	for i := range parts {
		parts[i] = "abandon"
	}
	parts[len(parts)-1] = "art"
	mnemonic := joinWords(parts)
	root, err := watchword.DeriveRoot(mnemonic)
	if err != nil {
		t.Fatalf("DeriveRoot: %v", err)
	}
	key, err := watchword.DeriveScopeKey(root, "personal")
	if err != nil {
		t.Fatalf("DeriveScopeKey: %v", err)
	}
	return key
}

func joinWords(parts []string) string {
	var b bytes.Buffer
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p)
	}
	return b.String()
}

// fakeEvent builds one minimal kernel.Event so chunk.Encode can run.
// Payload is empty JSON; the encryption + framing exercise the seam,
// not the payload semantics.
func fakeEvent(scope kernel.Scope) kernel.Event {
	return kernel.Event{
		UID:        "01HKJZNB" + string(scope)[0:1] + "1111111111111111",
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Scope:      scope,
		Payload:    []byte(`{"text":"hello"}`),
	}
}

func TestNew_RequiresDataDir(t *testing.T) {
	t.Parallel()
	requireGit(t)
	_, err := git.New(git.Options{RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: "file:///nope"}})
	if err == nil {
		t.Fatalf("New must require DataDir")
	}
}

func TestNew_RequiresAtLeastOneRemote(t *testing.T) {
	t.Parallel()
	requireGit(t)
	_, err := git.New(git.Options{DataDir: t.TempDir()})
	if err == nil {
		t.Fatalf("New must require at least one remote URL")
	}
}

func TestPush_RejectsInvalidScope(t *testing.T) {
	t.Parallel()
	requireGit(t)
	tr, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: newBareRemote(t)},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tr.Push(context.Background(), "organization", nil); err == nil {
		t.Fatalf("Push must reject invalid scope")
	}
}

func TestList_UnconfiguredScopeReturnsErrNotConfigured(t *testing.T) {
	t.Parallel()
	requireGit(t)
	tr, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: newBareRemote(t)},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tr.List(context.Background(), kernel.ScopeTeam); !errors.Is(err, transport.ErrNotConfigured) {
		t.Errorf("List on unconfigured scope = %v, want ErrNotConfigured", err)
	}
}

// TestPushPull_Roundtrip is the load-bearing integration test: device A
// seals one chunk and pushes it to a shared bare repo; device B
// initialises against the same remote and lists + gets the chunk.
// Asserts byte-equality of the body and equality of the parsed header.
func TestPushPull_Roundtrip(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx := context.Background()
	remote := newBareRemote(t)
	key := scopeKey(t)

	// Encode one chunk locally.
	ev := fakeEvent(kernel.ScopePersonal)
	body, hdr, err := chunk.Encode([]kernel.Event{ev}, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Device A pushes.
	aData := t.TempDir()
	a, err := git.New(git.Options{
		DataDir:    aData,
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "device-a",
		AuthorMail: "a@test",
		DeviceID:   "device-a",
	})
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	blobs := []transport.Blob{{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))}}
	n, err := a.Push(ctx, kernel.ScopePersonal, blobs)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if n != 1 {
		t.Errorf("Push accepted = %d, want 1", n)
	}

	// Device B initialises against the same remote, lists, gets.
	bData := t.TempDir()
	b, err := git.New(git.Options{
		DataDir:    bData,
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "device-b",
		AuthorMail: "b@test",
		DeviceID:   "device-b",
	})
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	listing, err := b.List(ctx, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("List B: %v", err)
	}
	if len(listing.Chunks) != 1 {
		t.Fatalf("List len = %d, want 1", len(listing.Chunks))
	}
	if listing.Chunks[0].ChunkID != hdr.ChunkID {
		t.Errorf("List ChunkID = %q, want %q", listing.Chunks[0].ChunkID, hdr.ChunkID)
	}

	got, err := b.Get(ctx, kernel.ScopePersonal, hdr.ChunkID)
	if err != nil {
		t.Fatalf("Get B: %v", err)
	}
	defer func() { _ = got.Body.Close() }()
	gotBytes, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(gotBytes, body) {
		t.Errorf("body byte-mismatch on roundtrip")
	}

	// Confirm decryption with the same key reproduces the event.
	_, events, err := chunk.Decode(gotBytes, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Decode received chunk: %v", err)
	}
	if len(events) != 1 || events[0].UID != ev.UID {
		t.Errorf("decoded event mismatch: %+v", events)
	}
}

// TestList_ReportsChunkContentHashes pins the producer→List half of the
// integrity contract: after a push, writeManifest records each chunk's
// canonical content hash, and List surfaces it in Listing.ContentHashes
// keyed by chunkID. The consumer uses this to reject a chunk hand-edited
// outside Mazarbul before grafting it.
func TestList_ReportsChunkContentHashes(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx := context.Background()
	remote := newBareRemote(t)

	// A plaintext chunk so the framed bytes are exactly what lands on disk.
	ev := fakeEvent(kernel.ScopePersonal)
	body, hdr, err := chunk.EncodePlaintext([]kernel.Event{ev}, kernel.ScopePersonal, "device-a")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}

	tr, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		DeviceID:   "device-a",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tr.Push(ctx, kernel.ScopePersonal, []transport.Blob{{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))}}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	listing, err := tr.List(ctx, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got, ok := listing.ContentHashes[hdr.ChunkID]
	if !ok {
		t.Fatalf("List omitted a content hash for chunk %s; want one recorded", hdr.ChunkID)
	}
	if want := chunk.ContentHash(body); got != want {
		t.Errorf("ContentHash = %q, want %q (sha256 of the pushed bytes)", got, want)
	}
}

// TestPush_IdempotentOnRepeat exercises the "blob already on disk"
// skip path: the second Push of the same chunk must be a no-op
// (accepted == 0).
func TestPush_IdempotentOnRepeat(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx := context.Background()
	remote := newBareRemote(t)
	key := scopeKey(t)
	ev := fakeEvent(kernel.ScopePersonal)
	body, hdr, err := chunk.Encode([]kernel.Event{ev}, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	a, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Push(ctx, kernel.ScopePersonal, []transport.Blob{{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))}}); err != nil {
		t.Fatalf("first Push: %v", err)
	}
	n, err := a.Push(ctx, kernel.ScopePersonal, []transport.Blob{{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))}})
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if n != 0 {
		t.Errorf("second Push accepted = %d, want 0 (idempotent)", n)
	}
}

// TestGet_RejectsPathTraversalChunkID covers Greptile finding #8.
// A malicious caller (or remote serving up a forged header) tries
// to read outside the scope chunks directory via "../" in the
// chunk ID. validateChunkID must reject anything that is not a
// canonical 26-char Crockford base32 ULID.
func TestGet_RejectsPathTraversalChunkID(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx := context.Background()
	tr, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: newBareRemote(t)},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []string{
		"../../etc/passwd",
		"..",
		"/absolute/path",
		"short",
		"01HKJZNB3T000000000000000",   // 25 chars — too short
		"01HKJZNB3T000000000000000XX", // 27 chars — too long
		"01hkjznb3t0000000000000000",  // lowercase — not Crockford
		"01HKJZNB3T00000000000000I0",  // contains I, excluded by Crockford
	}
	for _, id := range cases {
		t.Run("rejects "+id, func(t *testing.T) {
			t.Parallel()
			_, err := tr.Get(ctx, kernel.ScopePersonal, id)
			if err == nil {
				t.Fatalf("Get must reject malformed chunk id %q", id)
			}
		})
	}
}

// TestPresence_RecordsCommitAuthors verifies one Device row per
// distinct author after two devices push to the same remote.
func TestPresence_RecordsCommitAuthors(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx := context.Background()
	remote := newBareRemote(t)
	key := scopeKey(t)

	// Two events so each device has something to push.
	evA := fakeEvent(kernel.ScopePersonal)
	evA.UID = "01HKJZNAA111111111111AAAAA"
	bodyA, hdrA, err := chunk.Encode([]kernel.Event{evA}, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode A: %v", err)
	}
	evB := fakeEvent(kernel.ScopePersonal)
	evB.UID = "01HKJZNBB222222222222BBBBB"
	bodyB, hdrB, err := chunk.Encode([]kernel.Event{evB}, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode B: %v", err)
	}

	a, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "Device A",
		AuthorMail: "a@test",
		DeviceID:   "device-a",
	})
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	if _, err := a.Push(ctx, kernel.ScopePersonal, []transport.Blob{{Header: hdrA, Body: io.NopCloser(bytes.NewReader(bodyA))}}); err != nil {
		t.Fatalf("Push A: %v", err)
	}

	b, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "Device B",
		AuthorMail: "b@test",
		DeviceID:   "device-b",
	})
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	if _, err := b.Push(ctx, kernel.ScopePersonal, []transport.Blob{{Header: hdrB, Body: io.NopCloser(bytes.NewReader(bodyB))}}); err != nil {
		t.Fatalf("Push B: %v", err)
	}

	devs, err := a.Presence(ctx, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("Presence: %v", err)
	}
	if len(devs) < 2 {
		t.Errorf("Presence devices = %d, want >= 2", len(devs))
	}
	names := make(map[string]bool, len(devs))
	for _, d := range devs {
		names[d.Name] = true
	}
	if !names["Device A"] || !names["Device B"] {
		t.Errorf("Presence missing one of {Device A, Device B}: %v", names)
	}
}

// eventWithUID builds a minimal event with a caller-chosen UID so concurrent
// pushers can produce distinct chunks (chunk IDs are content-addressed, so a
// distinct UID yields a distinct chunk).
func eventWithUID(scope kernel.Scope, uid string) kernel.Event {
	return kernel.Event{
		UID:        uid,
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Scope:      scope,
		Payload:    []byte(`{"text":"hello"}`),
	}
}

// TestConcurrentMirrorOps_NoRefLock is the F7b regression. The daemon holds one
// Transport and runs a push loop and a consume loop against the SAME scope
// mirror; both call fastForward (git fetch), which races on
// refs/remotes/origin/main and wedges the mirror ("cannot lock ref"). Once
// wedged, List errors every tick so the daemon can neither push its own chunk
// nor pull the peer's — the permanent split-brain. The transport must serialize
// per-scope mirror access so concurrent List/Get/Push never collide on the ref
// lock.
func TestConcurrentMirrorOps_NoRefLock(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	remote := newBareRemote(t)
	key := scopeKey(t)

	// Seed the remote with one chunk so every fetch has a ref to update.
	seed, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "seed", AuthorMail: "seed@test", DeviceID: "seed",
	})
	if err != nil {
		t.Fatalf("New seed: %v", err)
	}
	body0, hdr0, err := chunk.Encode([]kernel.Event{eventWithUID(kernel.ScopePersonal, "01HKJZNBP0000000000000SEED")}, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode seed: %v", err)
	}
	if _, err := seed.Push(ctx, kernel.ScopePersonal, []transport.Blob{{Header: hdr0, Body: io.NopCloser(bytes.NewReader(body0))}}); err != nil {
		t.Fatalf("seed Push: %v", err)
	}

	// One shared transport over one mirror — as the daemon holds it.
	tr, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remote},
		AuthorName: "dev", AuthorMail: "dev@test", DeviceID: "dev",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const iters = 25
	var wg sync.WaitGroup
	errCh := make(chan error, iters*2)

	// Consume-like loop: repeated List (each does fastForward → git fetch).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range iters {
			if _, lerr := tr.List(ctx, kernel.ScopePersonal); lerr != nil {
				errCh <- fmt.Errorf("List[%d]: %w", i, lerr)
				return
			}
		}
	}()
	// Push-like loop: repeated Push of distinct chunks (fastForward → fetch →
	// commit → push).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range iters {
			body, hdr, eerr := chunk.Encode(
				[]kernel.Event{eventWithUID(kernel.ScopePersonal, fmt.Sprintf("01HKJZNBP%016d", i))},
				kernel.ScopePersonal, key)
			if eerr != nil {
				errCh <- fmt.Errorf("Encode[%d]: %w", i, eerr)
				return
			}
			if _, perr := tr.Push(ctx, kernel.ScopePersonal, []transport.Blob{{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))}}); perr != nil {
				errCh <- fmt.Errorf("Push[%d]: %w", i, perr)
				return
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for cerr := range errCh {
		t.Errorf("concurrent mirror op failed (ref-lock wedge): %v", cerr)
	}
}
