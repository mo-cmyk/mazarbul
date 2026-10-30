// Package cache wraps kernel-side providers with deduplicating caches that
// live in the relational store. The embedding cache short-circuits
// embedder.Embedder.Embed by content hash; the cache persists in SQLite so
// it survives daemon restarts. Wiring: provider/ollama.Client -> CachingEmbedder
// -> EventLog (orchestrator from #55).
package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/mo-cmyk/mazarbul/internal/kernel/embedder"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// ErrClosed is returned by Embed once Close has run. The cache cannot
// be reopened; construct a new instance via New. Mirrors the
// dispatcher's ErrClosed shape so callers can errors.Is against either.
var ErrClosed = errors.New("cache: closed")

// CachingEmbedder wraps an embedder.Embedder with a SQLite-backed content
// cache. The cache key is sha256 over a length-prefixed canonicalisation of
// (text, model, dim) so two distinct triples cannot collide by accidental
// concatenation; see hashKey for the exact framing.
//
// Concurrent calls for the same key serialise through an in-process
// singleflight so a thundering herd over the same text issues exactly one
// Inner.Embed call. The persisted INSERT uses ON CONFLICT DO NOTHING so a
// race against another process sharing the SQLite file (dev-only edge case)
// is idempotent too: by the embedder.Embedder contract, Inner.Embed is
// deterministic for the same (text, model, dim) triple, so whichever writer
// lands first wins with a correct vector and the loser's identical bytes
// are safely discarded.
//
// Construct with New; the zero value is not usable.
type CachingEmbedder struct {
	inner  embedder.Embedder
	reader *sql.DB
	writer *sql.DB
	now    func() time.Time

	flight singleflight.Group
	hits   atomic.Uint64
	misses atomic.Uint64

	// inflight tracks every in-progress Embed call (including its
	// detached singleflight work) so Close can block a teardown until
	// all SQLite-touching work finishes. Each Embed call increments
	// before its tracker goroutine spawns and decrements when the
	// goroutine returns. The dispatcher calls Close (after seizing
	// its lifecycle Lock, before tearing down the SQLite pools the
	// cache writes through) so a singleflight worker that outlives
	// its caller's Embed cannot race the pool teardown — Greptile
	// #108 P1 (third pass).
	inflight sync.WaitGroup

	// lifecycle bounds the cache's detached workers so a wedged
	// inner embedder cannot pin Dispatcher.Close forever. Singleflight
	// workers run under a ctx whose Done channel is lifecycle.Done(),
	// so cancelling lifecycle propagates context.Canceled into the
	// inner provider's HTTP call and the SQLite read/write —
	// Greptile #108 P1 (fourth pass).
	lifecycle       context.Context
	lifecycleCancel context.CancelFunc

	// closeMu serialises the closed flag against the WaitGroup Adds
	// that arm Embed-body tracking and singleflight-worker tracking.
	// The mutex is held briefly (no SQLite under it): admission
	// checks + WaitGroup increments only. Lookups run OUTSIDE the
	// mutex so a slow SQLite read cannot pin Close (Greptile #108
	// P1, seventh pass).
	closeMu sync.Mutex
	closed  bool

	// inflightEmbeds counts Embed bodies that have passed admission.
	// Close.lifecycleCancel cancels every lookup ctx via the
	// per-call forwarder goroutine (see withLifecycleCancel), so
	// drainage is fast even when the caller passed a non-cancellable
	// context. Separate from c.inflight because Embed can return
	// (caller cancellation) while its singleflight worker is still
	// detached and running.
	inflightEmbeds sync.WaitGroup
}

// Compile-time check that CachingEmbedder satisfies the kernel surface.
var (
	_ embedder.Embedder = (*CachingEmbedder)(nil)
	_ embedder.Batcher  = (*CachingEmbedder)(nil)
)

// New returns a CachingEmbedder that reads from reader and writes through
// writer. The split mirrors the sqlite.Store pool topology
// (Reader() scales with NumCPU, Writer() is serialised with MaxOpenConns=1)
// and the vec0 layer's pattern (vector.go:177 reads via Reader; writes go
// through Writer in a transaction). Both args must point at the same
// database file; passing two unrelated handles is a programming error the
// caller is trusted not to make.
//
// The now arg defaults to time.Now when nil; tests inject a fixed clock so
// create_time is deterministic.
func New(inner embedder.Embedder, reader, writer *sql.DB, now func() time.Time) *CachingEmbedder {
	if now == nil {
		now = time.Now
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	return &CachingEmbedder{
		inner:           inner,
		reader:          reader,
		writer:          writer,
		now:             now,
		lifecycle:       lifecycle,
		lifecycleCancel: cancel,
	}
}

// Embed returns the embedding for text. On a cache hit the inner provider
// is not called; on a miss the inner result is persisted before returning.
// Concurrent callers for the same key serialise so only one Inner.Embed
// fires per unique (text, model, dim) triple.
//
// Cancellation handling: the singleflight closure runs with a detached
// context (context.WithoutCancel) so the shared work survives a cancelled
// winner. Callers wait via flight.DoChan and select on their own ctx, so a
// caller with a short deadline returns immediately at its deadline rather
// than blocking on the (potentially long-running) shared embed call. Peers
// not yet cancelled keep waiting for the shared result.
//
// Errors from Inner.Embed are propagated wrapped. A failure to store the
// vector after a successful Embed surfaces as a wrapped error and the miss
// is still counted (the work was done; the next call will redo it because
// no row landed).
func (c *CachingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// Brief admission: hold closeMu just long enough to check the
	// closed flag and increment c.inflightEmbeds. NO SQLite under
	// the mutex — a slow lookup must not be able to pin Close
	// (Greptile #108 P1, seventh pass).
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return nil, ErrClosed
	}
	c.inflightEmbeds.Add(1)
	c.closeMu.Unlock()
	defer c.inflightEmbeds.Done()

	model := c.inner.Model()
	dim := storage.VectorDim
	key := hashKey(text, model, dim)
	log := logging.FromContext(ctx)

	// Lookup ctx fires on EITHER caller cancellation OR cache
	// lifecycle cancellation. A non-cancellable caller ctx + a slow
	// SQLite read would otherwise pin Close on inflightEmbeds.Wait;
	// the forwarder goroutine inside withLifecycleCancel propagates
	// c.lifecycle.Done into the derived ctx so Close's
	// lifecycleCancel unblocks the lookup.
	lookupCtx, cancelLookup := withLifecycleCancel(ctx, c.lifecycle)
	defer cancelLookup()

	vec, ok, err := c.lookup(lookupCtx, key, dim)
	if err != nil {
		return nil, err
	}
	if ok {
		c.hits.Add(1)
		log.Debug("embed cache hit",
			zap.String("cache.hit", key),
			zap.String("model", model),
			zap.Int("dim", dim),
		)
		return vec, nil
	}

	// Miss path: re-check the closed flag and arm the tracker under
	// closeMu. The admission check at the top of Embed could have
	// passed before Close flipped closed; this re-check ensures we
	// don't spawn a worker after shutdown has begun.
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return nil, ErrClosed
	}
	c.inflight.Add(1)
	c.closeMu.Unlock()

	// Worker ctx: cancellation/deadline comes from the cache's lifecycle
	// (so Close can drain stuck workers — Greptile #108 fourth pass),
	// values come from the caller (so the inner provider sees the
	// caller's logger / tracing keys). This shape replaces the previous
	// context.WithoutCancel(ctx) which was unbounded by definition.
	// contextcheck disabled: the worker ctx is intentionally NOT
	// derived from the caller's ctx — the whole point is to survive
	// caller cancellation while still being bounded by lifecycle.
	work := workerContext{Context: c.lifecycle, values: ctx}

	resCh := c.spawnTracker(work, key, text, model, dim) //nolint:contextcheck

	// Wait for either the shared result or the caller's own cancellation,
	// whichever lands first. If ctx fires first the tracker keeps blocking
	// inside flight.Do and the leader keeps running until its closure
	// returns; Wait observes the tracker until then.
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("cache: caller ctx done: %w", ctx.Err())
	case r := <-resCh:
		if r.err != nil {
			return nil, r.err
		}
		log.Debug("embed cache miss",
			zap.String("cache.miss", key),
			zap.String("model", model),
			zap.Int("dim", dim),
		)
		return r.vec, nil
	}
}

// EmbedBatch returns one vector per input text in a single round trip, in
// matching order. Cache-aware: looks up each text in SQLite first, then
// batches only the misses through Inner (when Inner satisfies
// embedder.Batcher) or falls back to per-text Embed when not. Misses are
// persisted on the way back so a follow-up call lands on the fast path.
//
// Empty input returns nil, nil without any underlying call. Ctx cancellation
// short-circuits at the first dispatch boundary; in-flight inner calls
// observe ctx.Err and return their own error.
//
// EmbedBatch does NOT use the singleflight Embed uses for the single-text
// path. The intended caller is the write-path resolver where each text
// already corresponds to a freshly-emitted Mention (unique by construction
// within a single batch); pairing batches with singleflight requires
// per-key dedup at construction time and is deferred until a measurable
// benefit is observed.
func (c *CachingEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return nil, ErrClosed
	}
	c.inflightEmbeds.Add(1)
	c.closeMu.Unlock()
	defer c.inflightEmbeds.Done()

	model := c.inner.Model()
	dim := storage.VectorDim

	lookupCtx, cancelLookup := withLifecycleCancel(ctx, c.lifecycle)
	defer cancelLookup()

	out := make([][]float32, len(texts))
	missIdx := make([]int, 0, len(texts))
	missTexts := make([]string, 0, len(texts))
	for i, t := range texts {
		key := hashKey(t, model, dim)
		vec, ok, err := c.lookup(lookupCtx, key, dim)
		if err != nil {
			return nil, err
		}
		if ok {
			c.hits.Add(1)
			out[i] = vec
			continue
		}
		missIdx = append(missIdx, i)
		missTexts = append(missTexts, t)
	}
	if len(missTexts) == 0 {
		return out, nil
	}

	missVecs, err := c.fetchMisses(ctx, missTexts)
	if err != nil {
		return nil, err
	}
	if got, want := len(missVecs), len(missTexts); got != want {
		return nil, fmt.Errorf("cache: inner batch returned %d vectors for %d misses", got, want)
	}

	// Worker ctx for stores mirrors Embed: bounded by lifecycle, carries
	// caller values. A store failure surfaces wrapped; the miss is already
	// counted in fetchMisses so the next call will redo the work.
	work := workerContext{Context: c.lifecycle, values: ctx}
	for j, vec := range missVecs {
		if got := len(vec); got != dim {
			return nil, fmt.Errorf("cache: inner batch[%d] returned %d-dim vector, want %d", j, got, dim)
		}
		key := hashKey(missTexts[j], model, dim)
		c.misses.Add(1)
		if serr := c.store(work, key, model, dim, vec); serr != nil { //nolint:contextcheck // see Embed: store ctx is lifecycle-bounded by design
			return nil, serr
		}
		out[missIdx[j]] = vec
	}
	return out, nil
}

// fetchMisses dispatches the batch of cache-missed texts to the inner
// embedder. When the inner satisfies embedder.Batcher the call collapses to a
// single round trip; otherwise it serialises through per-text Embed calls so
// the contract still holds (one vector per text, same order). Inner-side
// errors are wrapped so caller-side debugging surfaces "cache: inner batch:".
func (c *CachingEmbedder) fetchMisses(ctx context.Context, texts []string) ([][]float32, error) {
	if b, ok := c.inner.(embedder.Batcher); ok {
		vecs, err := b.EmbedBatch(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("cache: inner batch: %w", err)
		}
		return vecs, nil
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := c.inner.Embed(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("cache: inner embed [%d]: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// embedResult bundles the singleflight outcome a tracker goroutine
// sends back to its Embed caller.
type embedResult struct {
	vec []float32
	err error
}

// spawnTracker starts a goroutine that runs the singleflight call for
// key. The caller must have already armed the tracker by calling
// c.inflight.Add(1) under closeMu (see Embed); this goroutine
// releases the slot via defer c.inflight.Done.
func (c *CachingEmbedder) spawnTracker(work context.Context, key, text, model string, dim int) <-chan embedResult {
	resCh := make(chan embedResult, 1)
	go func() {
		defer c.inflight.Done()
		v, ferr, _ := c.flight.Do(key, func() (any, error) {
			return c.embedAndStore(work, key, text, model, dim)
		})
		if ferr != nil {
			resCh <- embedResult{err: fmt.Errorf("cache: singleflight: %w", ferr)}
			return
		}
		vec, ok := v.([]float32)
		if !ok {
			resCh <- embedResult{err: fmt.Errorf("cache: singleflight returned %T, want []float32", v)}
			return
		}
		resCh <- embedResult{vec: vec}
	}()
	return resCh
}

// embedAndStore is the singleflight closure body: a double-check
// lookup (another caller for the same key may have landed the row
// between the outer Embed lookup and our turn here), then the inner
// embed + store. Extracted from Embed to keep gocognit complexity
// inside the lint budget and to isolate the cache's write path for
// targeted testing.
func (c *CachingEmbedder) embedAndStore(work context.Context, key, text, model string, dim int) (any, error) {
	if vec, ok, lerr := c.lookup(work, key, dim); lerr != nil {
		return nil, lerr
	} else if ok {
		return vec, nil
	}
	vec, ierr := c.inner.Embed(work, text)
	if ierr != nil {
		return nil, fmt.Errorf("cache: inner embed: %w", ierr)
	}
	if got := len(vec); got != dim {
		return nil, fmt.Errorf("cache: inner returned %d-dim vector, want %d", got, dim)
	}
	// Count the miss before store: the inner cost is already paid, so
	// even a store failure should count (the next call will redo the
	// work and that's also a miss).
	c.misses.Add(1)
	if serr := c.store(work, key, model, dim, vec); serr != nil {
		return nil, serr
	}
	return vec, nil
}

// Close cancels the cache's lifecycle context (cascading into every
// in-flight singleflight worker so a wedged inner embedder or SQLite
// call surfaces context.Canceled instead of hanging) and waits for
// every tracker goroutine to exit. Used by Dispatcher.Close as the
// drain step before tearing down the SQLite pools the cache writes
// through, so no detached worker can race the pool teardown and no
// shutdown can be pinned by an unbounded worker — Greptile #108 P1
// (fourth pass).
//
// Safe to call multiple times; idempotent. The cache is unusable
// after Close: subsequent Embed calls observe the canceled lifecycle
// and return an error from the inner provider's first ctx-checked
// operation.
func (c *CachingEmbedder) Close() error {
	// Brief mutex: flip closed so subsequent admission checks reject.
	// We never hold this mutex across SQLite work, so a slow lookup
	// cannot pin us here.
	c.closeMu.Lock()
	already := c.closed
	c.closed = true
	c.closeMu.Unlock()
	if already {
		return nil
	}
	// Cancel the cache's lifecycle. This propagates into:
	//   - every in-flight lookup ctx (via withLifecycleCancel's
	//     forwarder goroutine), so a stalled SQLite read returns
	//     context.Canceled and its Embed call exits;
	//   - every singleflight worker ctx (workerContext shares
	//     c.lifecycle as its Done source), so a wedged inner
	//     embedder returns ctx.Err and its tracker exits.
	c.lifecycleCancel()
	// Drain Embed bodies first: they include the lookup window and
	// the singleflight wait. After they return, no SQLite call from
	// the Embed path is in flight.
	c.inflightEmbeds.Wait()
	// Drain detached trackers (Embeds whose callers bailed early).
	// lifecycleCancel above has already prodded them; this wait
	// observes their exit.
	c.inflight.Wait()
	return nil
}

// withLifecycleCancel returns a context that fires when EITHER the
// caller's ctx OR the cache's lifecycle ctx fires, plus a cancel
// function the caller should defer. A small forwarder goroutine
// watches lifecycle.Done and cancels the derived ctx; the cancel
// closes a stop channel so the forwarder exits cleanly when the
// caller is done. The pair is allocated per Embed call (~one extra
// goroutine while the call is in flight); the alternative — a
// shared multiplexer — would require an additional sync layer just
// to enumerate live waiters at Close time.
func withLifecycleCancel(callerCtx, lifecycle context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(callerCtx)
	if lifecycle.Done() == nil {
		// Lifecycle is non-cancellable (Background): no forwarder
		// needed; just hand back the standard pair.
		return ctx, cancel
	}
	stop := make(chan struct{})
	var once sync.Once
	closeFn := func() {
		once.Do(func() {
			cancel()
			close(stop)
		})
	}
	go func() {
		select {
		case <-lifecycle.Done():
			closeFn()
		case <-stop:
		}
	}()
	return ctx, closeFn
}

// workerContext gives the cache's singleflight workers a ctx whose
// Done/Deadline/Err come from `Context` (the cache's lifecycle, so
// Close can drain stuck workers) and whose Value comes from `values`
// (the caller's request ctx, so the inner provider sees the caller's
// logger / tracing). The shape is a manual merge because the stdlib
// context package exposes neither "values-from-X, cancel-from-Y" nor
// a way to attach an external cancel source to context.WithoutCancel.
type workerContext struct {
	context.Context // lifecycle: Done / Err / Deadline
	values          context.Context
}

// Value returns the caller's request-scoped value. The Context.Value
// chain on the lifecycle ctx is empty (it descends from Background),
// so caller-supplied keys like *zap.Logger only surface when read off
// values.
func (w workerContext) Value(key any) any { return w.values.Value(key) }

// Model returns the wrapped provider's model identifier. The wrapper is
// transparent at this seam so callers using the cache as an
// embedder.Embedder see the same model name as the inner provider.
func (c *CachingEmbedder) Model() string { return c.inner.Model() }

// Stats returns the running hit and miss counters. Reads are race-clean via
// atomics; consumed by maz doctor (#52) to surface the cache hit rate.
func (c *CachingEmbedder) Stats() (hits, misses uint64) {
	return c.hits.Load(), c.misses.Load()
}

// hashKey computes the content-hash key. The framing is a length-prefixed
// canonicalisation: 4-byte big-endian dim, then 4-byte big-endian model
// length followed by model bytes, then text bytes. With explicit lengths
// each (text, model, dim) triple has exactly one byte representation, so
// two distinct triples cannot produce the same digest by re-arranging the
// boundaries (the failure mode a NUL-separator framing only partially
// guards against).
func hashKey(text, model string, dim int) string {
	h := sha256.New()
	var dimBuf [4]byte
	binary.BigEndian.PutUint32(dimBuf[:], uint32(dim))
	h.Write(dimBuf[:])
	var modelLen [4]byte
	binary.BigEndian.PutUint32(modelLen[:], uint32(len(model)))
	h.Write(modelLen[:])
	h.Write([]byte(model))
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

// lookup returns the cached vector for key, or ok=false on a miss. Reads
// go through the reader pool which allows multiple concurrent readers
// against the WAL; writes on a miss land via the writer pool in store. The
// pool topology matches sqlite.Store (reader = NumCPU connections, writer
// = MaxOpenConns 1) and the vec0 layer's read/write split.
func (c *CachingEmbedder) lookup(ctx context.Context, key string, dim int) ([]float32, bool, error) {
	const q = `SELECT vector FROM embedding_cache WHERE content_hash = ?`
	var blob []byte
	if err := c.reader.QueryRowContext(ctx, q, key).Scan(&blob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cache: lookup %s: %w", key, err)
	}
	vec, err := decodeVector(blob, dim)
	if err != nil {
		return nil, false, fmt.Errorf("cache: decode %s: %w", key, err)
	}
	return vec, true, nil
}

// store persists a freshly-computed vector. ON CONFLICT DO NOTHING is safe
// because the embedder.Embedder contract requires Inner.Embed to be
// deterministic for the same (text, model, dim) triple: every losing writer
// would have produced the same bytes the winner already wrote.
func (c *CachingEmbedder) store(ctx context.Context, key, model string, dim int, vec []float32) error {
	blob, err := sqlite_vec.SerializeFloat32(vec)
	if err != nil {
		return fmt.Errorf("cache: serialize %s: %w", key, err)
	}
	const q = `
		INSERT INTO embedding_cache (content_hash, model, dim, vector, create_time)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(content_hash) DO NOTHING
	`
	if _, err := c.writer.ExecContext(ctx, q, key, model, dim, blob, c.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("cache: store %s: %w", key, err)
	}
	return nil
}

// decodeVector reverses sqlite_vec.SerializeFloat32 (little-endian float32
// stream with no header). The bindings ship no Deserialize counterpart so
// the codec lives here, private to the cache package. If a second consumer
// appears it should be promoted into internal/kernel/storage.
func decodeVector(blob []byte, dim int) ([]float32, error) {
	if got := len(blob); got != dim*4 {
		return nil, fmt.Errorf("vector blob length %d, want %d", got, dim*4)
	}
	out := make([]float32, dim)
	if err := binary.Read(bytes.NewReader(blob), binary.LittleEndian, out); err != nil {
		return nil, fmt.Errorf("read vector: %w", err)
	}
	return out, nil
}
