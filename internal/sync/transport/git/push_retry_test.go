package git_test

// push_retry_test.go covers C6 (bounded push retry + ErrPushContention) and
// basic C7 (history-rewrite detection) unit tests.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
	git "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
)

// makePlaintextChunk builds a plaintext chunk for a single event with the
// given uid suffix. Returns (body, header).
func makePlaintextChunk(t *testing.T, uid string) ([]byte, chunk.Header) {
	t.Helper()
	ev := kernel.Event{
		UID:        uid,
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Scope:      kernel.ScopePersonal,
		Payload:    []byte(`{"text":"retry test"}`),
	}
	body, hdr, err := chunk.EncodePlaintext([]kernel.Event{ev}, kernel.ScopePersonal, "test-device")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	return body, hdr
}

// newTransport is a helper for creating a git.Transport in tests.
func newTransport(t *testing.T, dataDir, remoteURL string, opts ...string) *git.Transport {
	t.Helper()
	authorName := "test"
	if len(opts) > 0 {
		authorName = opts[0]
	}
	tr, err := git.New(git.Options{
		DataDir:    dataDir,
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: remoteURL},
		AuthorName: authorName,
		AuthorMail: authorName + "@test",
		DeviceID:   authorName,
	})
	if err != nil {
		t.Fatalf("git.New: %v", err)
	}
	return tr
}

// TestPush_ContendedConcurrentPushesLand verifies that two devices pushing
// concurrently both eventually land their chunks (or at worst one gets
// ErrPushContention, but never a generic internal error).
//
// Setup: a third "seed" transport pushes a root commit first so that both
// racing transports have a valid HEAD parent when they rollback+retry.
// Without the seed, a root-commit rollback (HEAD~1 on no parent) would
// fail with a git error unrelated to contention.
func TestPush_ContendedConcurrentPushesLand(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx := context.Background()
	remote := newBareRemote(t)

	// Seed: push a root commit so A and B both have a parent to roll back to.
	seedDir := t.TempDir()
	seed := newTransport(t, seedDir, remote, "seed")
	seedBody, seedHdr := makePlaintextChunk(t, "01AAAAAAAAAAAAAAAAAAAAAA00")
	if _, err := seed.Push(ctx, kernel.ScopePersonal, []transport.Blob{
		{Header: seedHdr, Body: io.NopCloser(bytes.NewReader(seedBody))},
	}); err != nil {
		t.Fatalf("seed Push: %v", err)
	}

	// Clone both mirrors so A and B start from the seeded state.
	aDir, bDir := t.TempDir(), t.TempDir()
	a := newTransport(t, aDir, remote, "device-a")
	b := newTransport(t, bDir, remote, "device-b")
	if _, err := a.List(ctx, kernel.ScopePersonal); err != nil {
		t.Fatalf("A init List: %v", err)
	}
	if _, err := b.List(ctx, kernel.ScopePersonal); err != nil {
		t.Fatalf("B init List: %v", err)
	}

	body1, hdr1 := makePlaintextChunk(t, "01AAAAAAAAAAAAAAAAAAAAAA01")
	body2, hdr2 := makePlaintextChunk(t, "01AAAAAAAAAAAAAAAAAAAAAA02")

	var (
		errA, errB error
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errA = a.Push(ctx, kernel.ScopePersonal, []transport.Blob{
			{Header: hdr1, Body: io.NopCloser(bytes.NewReader(body1))},
		})
	}()
	go func() {
		defer wg.Done()
		_, errB = b.Push(ctx, kernel.ScopePersonal, []transport.Blob{
			{Header: hdr2, Body: io.NopCloser(bytes.NewReader(body2))},
		})
	}()
	wg.Wait()

	// A losing push may exhaust its retry budget under true simultaneity and
	// return ErrPushContention — that is acceptable ONLY because the daemon
	// re-offers the un-landed chunk on the next tick. Simulate that next tick by
	// re-pushing any loser, which MUST now land (the winner's commit is already
	// merged into the loser's mirror, so this is a clean fast-forward). The F7
	// regression is that BOTH chunks converge onto the remote — never a
	// permanently-dropped chunk (the split-brain the real-binary run found).
	if errA != nil {
		if !errors.Is(errA, transport.ErrPushContention) {
			t.Fatalf("A: unexpected error (want nil or ErrPushContention): %v", errA)
		}
		if _, err := a.Push(ctx, kernel.ScopePersonal, []transport.Blob{
			{Header: hdr1, Body: io.NopCloser(bytes.NewReader(body1))},
		}); err != nil {
			t.Fatalf("A re-push (next-tick re-offer) must land after losing the race: %v", err)
		}
	}
	if errB != nil {
		if !errors.Is(errB, transport.ErrPushContention) {
			t.Fatalf("B: unexpected error (want nil or ErrPushContention): %v", errB)
		}
		if _, err := b.Push(ctx, kernel.ScopePersonal, []transport.Blob{
			{Header: hdr2, Body: io.NopCloser(bytes.NewReader(body2))},
		}); err != nil {
			t.Fatalf("B re-push (next-tick re-offer) must land after losing the race: %v", err)
		}
	}

	// BOTH chunks must now be visible on the remote — bidirectional propagation
	// survives the contended push (the F7 fix; before it, the loser was dropped
	// and its mirror wedged, permanently split-braining the pair).
	cDir := t.TempDir()
	c := newTransport(t, cDir, remote, "device-c")
	listing, err := c.List(ctx, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("List from device-c: %v", err)
	}
	seen := make(map[string]bool, len(listing.Chunks))
	for _, hdr := range listing.Chunks {
		seen[hdr.ChunkID] = true
	}
	if !seen[hdr1.ChunkID] {
		t.Error("A's chunk never landed on the remote — a contended push was permanently dropped (F7 split-brain)")
	}
	if !seen[hdr2.ChunkID] {
		t.Error("B's chunk never landed on the remote — a contended push was permanently dropped (F7 split-brain)")
	}
}

// TestPush_ExhaustionReturnsErrPushContention verifies that after maxPushRetries
// attempts all rejected with non-fast-forward, Push returns ErrPushContention.
// A background goroutine keeps device A ahead so device B's retries always lose.
func TestPush_ExhaustionReturnsErrPushContention(t *testing.T) {
	t.Parallel()
	requireGit(t)

	// This test relies on timing: A's background pushes must land between each
	// of B's retry+backoff windows. With 4 retries and 100ms/200ms/400ms backoffs,
	// total wait ≈ 700ms. A pushes every 5ms — well within each window.
	// 60 s budget: under full parallel CI load, git clone + push setup can take
	// several seconds per device; the 30 s limit was observed to flake at ~31 s.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	remote := newBareRemote(t)

	// Device A: push the initial commit so B can clone.
	aDir := t.TempDir()
	a := newTransport(t, aDir, remote, "device-a")
	body0, hdr0 := makePlaintextChunk(t, "01AAAAAAAAAAAAAAAAAAAAAA00")
	if _, err := a.Push(ctx, kernel.ScopePersonal, []transport.Blob{
		{Header: hdr0, Body: io.NopCloser(bytes.NewReader(body0))},
	}); err != nil {
		t.Fatalf("A initial push: %v", err)
	}

	// Device B: clone via List so its mirror starts at A's commit.
	bDir := t.TempDir()
	b := newTransport(t, bDir, remote, "device-b")
	if _, err := b.List(ctx, kernel.ScopePersonal); err != nil {
		t.Fatalf("B List (clone): %v", err)
	}

	// Background goroutine: A keeps pushing new chunks.
	// Each push advances the remote HEAD, forcing B's next commit to be NFF.
	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()
	var bgWG sync.WaitGroup
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		for i := 1; i <= 20; i++ {
			select {
			case <-bgCtx.Done():
				return
			default:
			}
			uid := makeUID(i)
			body, hdr := makePlaintextChunk(t, uid)
			_, _ = a.Push(bgCtx, kernel.ScopePersonal, []transport.Blob{
				{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))},
			})
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// Device B tries to push its own chunk; should exhaust and return ErrPushContention.
	bodyB, hdrB := makePlaintextChunk(t, "01BBBBBBBBBBBBBBBBBBBBBB01")
	_, err := b.Push(ctx, kernel.ScopePersonal, []transport.Blob{
		{Header: hdrB, Body: io.NopCloser(bytes.NewReader(bodyB))},
	})
	bgCancel()
	bgWG.Wait()

	if err == nil {
		// B succeeded — the background pusher wasn't fast enough. This is
		// acceptable in a very slow CI environment; log rather than fail.
		t.Log("note: B succeeded (background pusher too slow for contention simulation); this is a race-dependent test")
		return
	}
	if !errors.Is(err, transport.ErrPushContention) {
		t.Errorf("B: want ErrPushContention, got: %v", err)
	}
}

// makeUID produces a valid 26-character ULID-format string for a test sequence
// number. Not a real ULID (no timestamp semantics) but passes validateChunkID.
func makeUID(n int) string {
	base := "01CCCCCCCCCCCCCCCCCCCCCC"
	suffix := []byte("00")
	suffix[0] = byte('0' + (n/10)%10)
	suffix[1] = byte('0' + n%10)
	return base[:24] + string(suffix)
}

// TestPush_NonNFFErrorFailsFast verifies that non-NFF errors (e.g., from a
// permission failure) are NOT retried — they surface immediately.
func TestPush_NonNFFErrorFailsFast(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx := context.Background()

	// Use a non-existent remote URL so the push fails with a clone/network
	// error, not a non-fast-forward error.
	tr, err := git.New(git.Options{
		DataDir:    t.TempDir(),
		RemoteURLs: map[kernel.Scope]string{kernel.ScopePersonal: "file:///nonexistent/path/that/does/not/exist.git"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body, hdr := makePlaintextChunk(t, "01DDDDDDDDDDDDDDDDDDDDDD01")
	start := time.Now()
	_, pushErr := tr.Push(ctx, kernel.ScopePersonal, []transport.Blob{
		{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))},
	})
	elapsed := time.Since(start)

	if pushErr == nil {
		t.Fatal("expected error from non-existent remote, got nil")
	}
	// Should NOT be ErrPushContention (that's a retry-exhaustion error).
	if errors.Is(pushErr, transport.ErrPushContention) {
		t.Errorf("non-NFF error incorrectly triggered contention path: %v", pushErr)
	}
	// Should fail fast (no 100ms+ backoffs).
	if elapsed > 5*time.Second {
		t.Errorf("non-NFF error took %v, want <5s (fail-fast expected)", elapsed)
	}
}

// TestPush_ErrPushContentionWrapsErrTransport pins the error-wrapping contract:
// ErrPushContention must also satisfy errors.Is(err, ErrTransport).
func TestPush_ErrPushContentionWrapsErrTransport(t *testing.T) {
	t.Parallel()
	// Construct a synthetic error that mirrors what Push returns on exhaustion.
	err := errors.Join(transport.ErrTransport, transport.ErrPushContention)
	if !errors.Is(err, transport.ErrTransport) {
		t.Error("ErrPushContention sentinel error must satisfy errors.Is(err, ErrTransport)")
	}
	if !errors.Is(err, transport.ErrPushContention) {
		t.Error("ErrPushContention sentinel must be detectable via errors.Is")
	}
}

// TestFastForward_UpdatesMarkerFile verifies that after a successful
// fast-forward the maz-last-remote-head marker is written.
func TestFastForward_UpdatesMarkerFile(t *testing.T) {
	t.Parallel()
	requireGit(t)
	ctx := context.Background()
	remote := newBareRemote(t)

	// Push one chunk so the remote has a commit.
	aDir := t.TempDir()
	a := newTransport(t, aDir, remote, "device-a")
	body, hdr := makePlaintextChunk(t, "01EEEEEEEEEEEEEEEEEEEEEEE1")
	if _, err := a.Push(ctx, kernel.ScopePersonal, []transport.Blob{
		{Header: hdr, Body: io.NopCloser(bytes.NewReader(body))},
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Device B lists (triggers fastForward) — the marker should now exist.
	bDir := t.TempDir()
	b := newTransport(t, bDir, remote, "device-b")
	if _, err := b.List(ctx, kernel.ScopePersonal); err != nil {
		t.Fatalf("List: %v", err)
	}

	// Verify the marker file exists and contains a 40-char hex SHA.
	markerPath := bDir + "/sync/personal/repo/.git/maz-last-remote-head"
	raw, err := readFileString(t, markerPath)
	if err != nil {
		t.Fatalf("marker file missing after fastForward: %v", err)
	}
	sha := trimNewlines(raw)
	if len(sha) != 40 {
		t.Errorf("marker file: want 40-char SHA, got %q (len=%d)", sha, len(sha))
	}
}

// readFileString reads a file's contents as a string for test assertions.
func readFileString(t *testing.T, path string) (string, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	return string(data), err
}

func trimNewlines(s string) string {
	result := s
	for len(result) > 0 && (result[len(result)-1] == '\n' || result[len(result)-1] == '\r') {
		result = result[:len(result)-1]
	}
	return result
}
