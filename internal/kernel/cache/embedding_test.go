package cache_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/cache"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// fakeEmbedder is a controllable embedder.Embedder for unit tests. It records
// every call so the test can assert that the cache short-circuits on hit,
// returns either a fixed vector or an error per call, and can optionally
// block to exercise the singleflight dedup path.
type fakeEmbedder struct {
	model string
	vec   []float32
	err   error

	// block is closed by the test once it wants in-flight callers to proceed.
	// A nil block channel means "do not block".
	block <-chan struct{}

	calls atomic.Int64
}

func (f *fakeEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	f.calls.Add(1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	out := make([]float32, len(f.vec))
	copy(out, f.vec)
	return out, nil
}

func (f *fakeEmbedder) Model() string { return f.model }

// openStore opens a fresh SQLite store backed by a temp file, with all
// migrations applied (including 0004 which creates embedding_cache).
func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// fixedVec returns a deterministic 1024-dim vector seeded by n; identical
// inputs produce identical vectors so a cache hit returns the same bytes as
// the inner provider.
func fixedVec(n int) []float32 {
	v := make([]float32, storage.VectorDim)
	for i := range v {
		v[i] = float32(n) + float32(i)*0.001
	}
	return v
}

func vecsEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCachingEmbedder_ColdMiss(t *testing.T) {
	store := openStore(t)
	want := fixedVec(1)
	inner := &fakeEmbedder{model: "bge-m3", vec: want}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	got, err := ce.Embed(t.Context(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !vecsEqual(got, want) {
		t.Fatalf("vector mismatch on cold miss")
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner.calls = %d, want 1", got)
	}
	if hits, misses := ce.Stats(); hits != 0 || misses != 1 {
		t.Fatalf("stats = (%d, %d), want (0, 1)", hits, misses)
	}
}

func TestCachingEmbedder_WarmHit(t *testing.T) {
	store := openStore(t)
	want := fixedVec(2)
	inner := &fakeEmbedder{model: "bge-m3", vec: want}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	// Prime the cache.
	if _, err := ce.Embed(t.Context(), "hello"); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// Second call must not invoke the inner provider.
	got, err := ce.Embed(t.Context(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !vecsEqual(got, want) {
		t.Fatalf("vector mismatch on warm hit")
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner.calls = %d, want 1 (second call should have hit cache)", got)
	}
	if hits, misses := ce.Stats(); hits != 1 || misses != 1 {
		t.Fatalf("stats = (%d, %d), want (1, 1)", hits, misses)
	}
}

func TestCachingEmbedder_DifferentModelsIsolated(t *testing.T) {
	store := openStore(t)
	vec1 := fixedVec(10)
	vec2 := fixedVec(20)

	innerA := &fakeEmbedder{model: "bge-m3", vec: vec1}
	innerB := &fakeEmbedder{model: "nomic-embed-text", vec: vec2}
	ceA := cache.New(innerA, store.Reader(), store.Writer(), nil)
	ceB := cache.New(innerB, store.Reader(), store.Writer(), nil)

	// Same text under model A primes A's cache row.
	gotA, err := ceA.Embed(t.Context(), "shared text")
	if err != nil {
		t.Fatalf("A.Embed: %v", err)
	}
	if !vecsEqual(gotA, vec1) {
		t.Fatalf("A: vector mismatch")
	}

	// Same text under model B must miss (different model in hash key),
	// invoking B's inner provider.
	gotB, err := ceB.Embed(t.Context(), "shared text")
	if err != nil {
		t.Fatalf("B.Embed: %v", err)
	}
	if !vecsEqual(gotB, vec2) {
		t.Fatalf("B: vector mismatch")
	}

	if got := innerA.calls.Load(); got != 1 {
		t.Fatalf("innerA.calls = %d, want 1", got)
	}
	if got := innerB.calls.Load(); got != 1 {
		t.Fatalf("innerB.calls = %d, want 1", got)
	}

	// Re-querying A must still hit its row (not B's).
	if _, err := ceA.Embed(t.Context(), "shared text"); err != nil {
		t.Fatalf("A.Embed (replay): %v", err)
	}
	if got := innerA.calls.Load(); got != 1 {
		t.Fatalf("innerA.calls after replay = %d, want 1", got)
	}
}

func TestCachingEmbedder_InnerErrorPropagates(t *testing.T) {
	store := openStore(t)
	sentinel := errors.New("ollama unavailable")
	inner := &fakeEmbedder{model: "bge-m3", err: sentinel}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	_, err := ce.Embed(t.Context(), "boom")
	if err == nil {
		t.Fatal("Embed: want error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want wrap of %v", err, sentinel)
	}
	// No row should have been written for a failed embed.
	if hits, misses := ce.Stats(); hits != 0 || misses != 0 {
		t.Fatalf("stats = (%d, %d), want (0, 0)", hits, misses)
	}

	// Recovery: replacing inner.err with a real vector should let the next
	// call write the row normally (no poisoned-key state in the cache).
	inner.err = nil
	inner.vec = fixedVec(3)
	if _, err := ce.Embed(t.Context(), "boom"); err != nil {
		t.Fatalf("Embed after recovery: %v", err)
	}
}

func TestCachingEmbedder_ConcurrentSameKeyDedups(t *testing.T) {
	store := openStore(t)
	want := fixedVec(4)
	gate := make(chan struct{})
	inner := &fakeEmbedder{model: "bge-m3", vec: want, block: gate}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	const n = 64
	var (
		started sync.WaitGroup
		wg      sync.WaitGroup
		gotMu   sync.Mutex
		results [][]float32
	)
	started.Add(n)
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			started.Done()
			v, err := ce.Embed(t.Context(), "hot text")
			if err != nil {
				t.Errorf("Embed: %v", err)
				return
			}
			gotMu.Lock()
			results = append(results, v)
			gotMu.Unlock()
		}()
	}
	// Wait for every goroutine to have started before opening the gate.
	// The Inner.Embed call (which blocks on gate) is reached after the
	// outer lookup miss + singleflight.Do entry; with all n goroutines
	// alive and racing toward Do, at least one ends up the winner and
	// the rest pile up as waiters. A pre-gate signal is sufficient — the
	// race detector covers the rest.
	started.Wait()
	close(gate)
	wg.Wait()

	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner.calls = %d, want 1 (singleflight failed to dedup)", got)
	}
	if len(results) != n {
		t.Fatalf("collected %d results, want %d", len(results), n)
	}
	for i, v := range results {
		if !vecsEqual(v, want) {
			t.Fatalf("result %d does not match", i)
		}
	}

	// Exactly one row in the cache for this key.
	var rows int
	if err := store.Writer().QueryRowContext(t.Context(),
		`SELECT count(*) FROM embedding_cache WHERE model = 'bge-m3'`,
	).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}
}

func TestCachingEmbedder_PersistsAcrossWrappers(t *testing.T) {
	store := openStore(t)
	want := fixedVec(5)
	inner1 := &fakeEmbedder{model: "bge-m3", vec: want}
	ce1 := cache.New(inner1, store.Reader(), store.Writer(), nil)
	if _, err := ce1.Embed(t.Context(), "persisted"); err != nil {
		t.Fatalf("first wrapper Embed: %v", err)
	}
	if got := inner1.calls.Load(); got != 1 {
		t.Fatalf("first inner.calls = %d, want 1", got)
	}

	// New wrapper, same underlying SQLite file. The second wrapper's inner
	// must not be invoked because the row was persisted by the first.
	inner2 := &fakeEmbedder{model: "bge-m3", vec: fixedVec(999)}
	ce2 := cache.New(inner2, store.Reader(), store.Writer(), nil)
	got, err := ce2.Embed(t.Context(), "persisted")
	if err != nil {
		t.Fatalf("second wrapper Embed: %v", err)
	}
	if !vecsEqual(got, want) {
		t.Fatalf("second wrapper returned vec from inner2, not cached vec")
	}
	if got := inner2.calls.Load(); got != 0 {
		t.Fatalf("inner2.calls = %d, want 0 (should have read from disk)", got)
	}
}

func TestCachingEmbedder_RejectsBadDim(t *testing.T) {
	store := openStore(t)
	// Inner returns a vector of the wrong length: the wrapper must surface
	// the mismatch before writing the cache row.
	bad := make([]float32, storage.VectorDim-1)
	inner := &fakeEmbedder{model: "bge-m3", vec: bad}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	_, err := ce.Embed(t.Context(), "wrong dim")
	if err == nil {
		t.Fatal("Embed: want error for wrong-dim vector, got nil")
	}

	var rows int
	if err := store.Writer().QueryRowContext(t.Context(),
		`SELECT count(*) FROM embedding_cache`,
	).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("rows = %d, want 0 (bad-dim vector must not land)", rows)
	}
}

func TestCachingEmbedder_Model(t *testing.T) {
	store := openStore(t)
	inner := &fakeEmbedder{model: "bge-m3", vec: fixedVec(0)}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)
	if got, want := ce.Model(), "bge-m3"; got != want {
		t.Fatalf("Model() = %q, want %q", got, want)
	}
}

// fixedClock returns a now func that always returns the given time. Used to
// make the create_time column deterministic for the persistence assertion
// below.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestCachingEmbedder_CreateTimeUsesClock(t *testing.T) {
	store := openStore(t)
	want := fixedVec(6)
	stamp := time.Date(2026, 5, 24, 12, 34, 56, 0, time.UTC)
	inner := &fakeEmbedder{model: "bge-m3", vec: want}
	ce := cache.New(inner, store.Reader(), store.Writer(), fixedClock(stamp))
	if _, err := ce.Embed(t.Context(), "stamped"); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	var got string
	if err := store.Writer().QueryRowContext(t.Context(),
		`SELECT create_time FROM embedding_cache LIMIT 1`,
	).Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if want := stamp.Format(time.RFC3339Nano); got != want {
		t.Fatalf("create_time = %q, want %q", got, want)
	}
}

// TestCachingEmbedder_CancellingOneCallerDoesNotPoisonPeers exercises the
// classic singleflight footgun: if the goroutine that wins the slot has its
// own ctx cancelled mid-flight, naive Do() returns ctx.Err() to every peer
// waiting on the same key even though their own ctxs are still live. The
// wrapper guards against this by detaching the closure's ctx with
// context.WithoutCancel and re-checking the caller's ctx after Do.
func TestCachingEmbedder_CancellingOneCallerDoesNotPoisonPeers(t *testing.T) {
	store := openStore(t)
	want := fixedVec(11)
	gate := make(chan struct{})
	inner := &fakeEmbedder{model: "bge-m3", vec: want, block: gate}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	// Winner has a cancellable ctx that we cancel before the gate opens;
	// peer has a fresh ctx that must complete normally.
	winnerCtx, winnerCancel := context.WithCancel(t.Context())
	peerCtx, peerCancel := context.WithCancel(t.Context())
	defer peerCancel()

	var (
		started sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		peerVec []float32
		peerErr error
	)
	started.Add(2)
	done.Add(2)
	go func() {
		defer done.Done()
		started.Done()
		_, _ = ce.Embed(winnerCtx, "shared")
	}()
	go func() {
		defer done.Done()
		started.Done()
		v, err := ce.Embed(peerCtx, "shared")
		mu.Lock()
		peerVec, peerErr = v, err
		mu.Unlock()
	}()

	started.Wait()
	// Give Embed a beat to enter the singleflight slot before we cancel.
	// 10ms is generous and the assertion below tolerates either ordering.
	time.Sleep(10 * time.Millisecond)
	winnerCancel()
	close(gate)
	done.Wait()

	mu.Lock()
	defer mu.Unlock()
	if peerErr != nil {
		t.Fatalf("peer Embed: %v (winner's cancel poisoned peer)", peerErr)
	}
	if !vecsEqual(peerVec, want) {
		t.Fatalf("peer received wrong vector after winner cancel")
	}
}

// TestCachingEmbedder_CallerDeadlineFiresBeforeInnerCompletes locks the
// fix for the P1 finding "Cancellation waits too long" (Greptile review on
// PR #96): the wrapper must let each caller return when its own ctx expires
// without waiting for the shared singleflight work to finish. The previous
// implementation used singleflight.Do (blocking) and only checked ctx.Err()
// after the closure returned, which turned a short caller deadline into a
// long wait. The current implementation uses DoChan + select on ctx.
func TestCachingEmbedder_CallerDeadlineFiresBeforeInnerCompletes(t *testing.T) {
	store := openStore(t)
	gate := make(chan struct{})
	t.Cleanup(func() {
		// Release the inner call so the background goroutine doesn't leak
		// past the test boundary.
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	inner := &fakeEmbedder{model: "bge-m3", vec: fixedVec(12), block: gate}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ce.Embed(ctx, "slow embed")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Embed: want ctx-deadline error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Embed: err = %v, want wrap of context.DeadlineExceeded", err)
	}
	// Generous upper bound: the caller deadline is 25ms; inner is blocked
	// forever (until t.Cleanup releases the gate). Anything close to 25ms
	// is correct; anything > 500ms indicates the singleflight is blocking
	// past the caller deadline.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Embed returned after %v; expected to bail near the 25ms deadline (singleflight blocked the caller)", elapsed)
	}
}

// TestCachingEmbedder_CloseCancelsAndDrainsStuckWorker pins the
// Greptile-#108 fourth-pass fix: Close must NOT block forever on a
// wedged inner embedder. The cache owns a lifecycle ctx that Close
// cancels before draining; cancellation propagates into the inner
// provider's ctx-aware call (here the fakeEmbedder's `gate` select)
// so the worker exits with ctx.Err() rather than waiting on `gate`
// to be released externally.
func TestCachingEmbedder_CloseCancelsAndDrainsStuckWorker(t *testing.T) {
	store := openStore(t)
	// gate is never closed by the test — only the cache's lifecycle
	// cancel via Close should release the inner provider.
	gate := make(chan struct{})
	inner := &fakeEmbedder{model: "bge-m3", vec: fixedVec(7), block: gate}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	// Caller deadline fires fast; the inner provider stays blocked
	// inside `select{ case <-gate; case <-ctx.Done() }`. The worker
	// ctx is the cache's lifecycle ctx — NOT the caller's ctx — so
	// the inner blocks on `gate` regardless of caller cancellation.
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if _, err := ce.Embed(ctx, "wait-test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Embed: want DeadlineExceeded, got %v", err)
	}

	// Worker is still blocked inside flight.Do via fakeEmbedder.Embed's
	// select; the tracker goroutine is still alive. A bare Wait()-style
	// drain would hang forever here. Close must cancel the lifecycle to
	// let the worker exit.
	closeDone := make(chan error, 1)
	go func() { closeDone <- ce.Close() }()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return; lifecycle cancellation must drain stuck workers")
	}
}

// TestCachingEmbedder_CloseDoesNotHangOnInFlightEmbed pins the
// Greptile-#108 seventh-pass fix: Close must NOT block on an Embed
// whose lookup is still in flight under a non-cancellable caller
// ctx. The fix derives the lookup ctx from c.lifecycle via a
// forwarder goroutine, so Close's lifecycleCancel propagates into
// the lookup. We exercise that by running Embed against a blocked
// inner provider with a Background caller ctx, then asserting both
// Close and Embed return quickly once lifecycleCancel fires.
//
// Coverage note: SQLite's QueryRowContext on the embedding_cache
// table is too fast to "stick" in lookup with a real reader, so we
// trigger the lifecycle-cancel path via the singleflight wait (the
// inner provider is gated). This proves the cross-cutting invariant
// — Close drains in-flight Embeds via lifecycle cancellation — even
// without a contrived slow-lookup harness.
func TestCachingEmbedder_CloseDoesNotHangOnInFlightEmbed(t *testing.T) {
	store := openStore(t)
	gate := make(chan struct{})
	inner := &fakeEmbedder{model: "bge-m3", vec: fixedVec(17), block: gate}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	// Background ctx — no deadline, no cancel from caller side.
	embedDone := make(chan struct{})
	go func() {
		defer close(embedDone)
		_, _ = ce.Embed(context.Background(), "non-cancellable-caller")
	}()

	// Give the Embed call a beat to land in the singleflight wait.
	time.Sleep(20 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() { closeDone <- ce.Close() }()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung; lifecycle cancellation must unblock in-flight Embeds")
	}

	select {
	case <-embedDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Embed hung after Close returned")
	}
}

// TestCachingEmbedder_WarmHitAfterCloseReturnsErrClosed pins the
// Greptile-#108 sixth-pass fix: a warm cache hit must NOT skip the
// closed check and return a vector after Close. The previous shape
// did the SQLite lookup before reserveSlot, so a direct caller
// holding Orchestrator.Embedder could get back a vector even after
// CachingEmbedder.Close ran — meaning the dispatcher had been told
// the cache was drained while a fresh consumer still believed it
// was live. We seed a row, Close the cache, and assert Embed
// returns ErrClosed instead of the cached vector.
func TestCachingEmbedder_WarmHitAfterCloseReturnsErrClosed(t *testing.T) {
	store := openStore(t)
	inner := &fakeEmbedder{model: "bge-m3", vec: fixedVec(11)}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	// Seed a warm hit.
	if _, err := ce.Embed(t.Context(), "warm-hit-text"); err != nil {
		t.Fatalf("seed Embed: %v", err)
	}

	if err := ce.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Same key — would otherwise hit the cache and return the seeded
	// vector. After Close, the closed-check guard MUST short-circuit
	// before touching SQLite.
	if _, err := ce.Embed(t.Context(), "warm-hit-text"); !errors.Is(err, cache.ErrClosed) {
		t.Fatalf("warm-hit Embed after Close: got %v, want errors.Is ErrClosed", err)
	}
}

// TestCachingEmbedder_EmbedAfterCloseReturnsErrClosed pins the
// Greptile-#108 fifth-pass fix: once Close has flipped the closed
// flag, every subsequent Embed must reject before incrementing the
// inflight WaitGroup. Otherwise a direct caller holding
// Orchestrator.Embedder could race Close into the sync.WaitGroup
// Add-after-Wait misuse path, or spawn a SQLite-touching worker
// after Close has returned and the dispatcher is mid-pool-teardown.
func TestCachingEmbedder_EmbedAfterCloseReturnsErrClosed(t *testing.T) {
	store := openStore(t)
	inner := &fakeEmbedder{model: "bge-m3", vec: fixedVec(3)}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	if err := ce.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := ce.Embed(t.Context(), "post-close")
	if !errors.Is(err, cache.ErrClosed) {
		t.Fatalf("Embed after Close: got %v, want errors.Is ErrClosed", err)
	}
}

// TestCachingEmbedder_CloseIsIdempotent confirms Close can be called
// multiple times without panicking on the underlying WaitGroup or the
// lifecycle cancel func.
func TestCachingEmbedder_CloseIsIdempotent(t *testing.T) {
	store := openStore(t)
	inner := &fakeEmbedder{model: "bge-m3", vec: fixedVec(4)}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	for i := range 3 {
		if err := ce.Close(); err != nil {
			t.Fatalf("Close iteration %d: %v", i, err)
		}
	}
}

// TestCachingEmbedder_LookupRejectsMalformedBlob asserts that a row written
// out-of-band with a wrong-length vector blob surfaces an error from the
// lookup path rather than being silently swallowed. The scenario is what
// a future migration that changes the vector encoding without bumping the
// cache table would look like; we want the failure to be loud.
func TestCachingEmbedder_LookupRejectsMalformedBlob(t *testing.T) {
	store := openStore(t)
	inner := &fakeEmbedder{model: "bge-m3", vec: fixedVec(8)}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	// Plant a row whose vector blob is too short to decode at storage.VectorDim.
	if _, err := store.Writer().ExecContext(t.Context(),
		`INSERT INTO embedding_cache (content_hash, model, dim, vector, create_time)
         VALUES (?, ?, ?, ?, ?)`,
		"deadbeef", "bge-m3", storage.VectorDim, []byte{0x00, 0x01, 0x02}, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("plant row: %v", err)
	}

	// Force the hash lookup to hit the planted row by crafting the input
	// that produces the "deadbeef" key, except we cannot easily collide
	// sha256. Instead: read the row directly via the lookup path by
	// inserting a row keyed on the actual hashKey for some text; replace
	// its blob with a malformed one and re-Embed.
	if _, err := ce.Embed(t.Context(), "valid"); err != nil {
		t.Fatalf("seed Embed: %v", err)
	}
	if _, err := store.Writer().ExecContext(t.Context(),
		`UPDATE embedding_cache SET vector = ? WHERE model = ? AND content_hash != ?`,
		[]byte{0x01, 0x02, 0x03}, "bge-m3", "deadbeef",
	); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}
	if _, err := ce.Embed(t.Context(), "valid"); err == nil {
		t.Fatal("Embed: want error on malformed cached blob, got nil")
	}
}

// TestCachingEmbedder_StatsRaceClean exercises the Stats accessor under
// concurrent Embed calls. The race detector is the actual assertion; the
// post-loop bounds just guard against an accounting regression that would
// make Stats() useless to maz doctor.
//
// Accounting note: misses are incremented only inside the singleflight
// closure (the winner does the work), so peers that piggyback on a shared
// result are neither hits nor misses. hits + misses therefore sits between
// "one miss per unique key" and "n" depending on how aggressively the
// goroutines coalesce. The bounds below capture that envelope without
// pinning a value that depends on scheduler timing.
func TestCachingEmbedder_StatsRaceClean(t *testing.T) {
	store := openStore(t)
	inner := &fakeEmbedder{model: "bge-m3", vec: fixedVec(7)}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	const (
		n        = 32
		uniqKeys = 4
	)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			_, _ = ce.Embed(t.Context(), fmt.Sprintf("key-%d", i%uniqKeys))
			_, _ = ce.Stats()
		}()
	}
	wg.Wait()

	hits, misses := ce.Stats()
	total := hits + misses
	if total < uniqKeys {
		t.Fatalf("hits(%d) + misses(%d) = %d, want >= %d (at least one accounted call per unique key)", hits, misses, total, uniqKeys)
	}
	if total > n {
		t.Fatalf("hits(%d) + misses(%d) = %d, want <= %d", hits, misses, total, n)
	}
}
