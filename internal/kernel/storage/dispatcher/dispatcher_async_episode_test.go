package dispatcher_test

// dispatcher_async_episode_test.go pins the latency contract for the episode
// write path: dispatcher.Apply MUST return as soon as the SQLite row and the
// graph node are durable, with the embedder call deferred to a background
// goroutine. Holding the writer goroutine on a 200ms+ embed (typical for
// Ollama over HTTP against a non-batched 7B model) made the LongMemEval
// feeder spend the bulk of every haystack inside the synchronous embed and
// pushed end-to-end run time to the bound of the feeder's recall timeout,
// even though the embedding has no role in episode durability or in FTS
// recall.
//
// The test uses a slowEmbedder that sleeps for blockDuration; before the
// fix, Apply took ~blockDuration per episode. After the fix, Apply returns
// well under hotPathBudget and the vec row materialises asynchronously
// (observed via dispatcher.WaitForBackgroundVec).

import (
	"context"
	"hash/fnv"
	"math/rand"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// slowEmbedder produces a deterministic vector but only after sleeping for
// blockDuration. Mimics the worst-case round-trip latency of a cold Ollama
// instance plus model load.
type slowEmbedder struct {
	dim   int
	block time.Duration
}

func (s *slowEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	select {
	case <-time.After(s.block):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	r := rand.New(rand.NewSource(int64(h.Sum64())))
	out := make([]float32, s.dim)
	for i := range out {
		out[i] = r.Float32()
	}
	return out, nil
}

const (
	// blockDuration is the slowEmbedder's per-call latency. 200ms mirrors a
	// realistic Ollama bge-m3 round-trip on the eval feeder's machine.
	blockDuration = 200 * time.Millisecond
	// hotPathBudget caps the synchronous Apply latency. The fix moves the
	// 200ms embed off the writer, so the synchronous path should land well
	// under blockDuration. 150ms gives headroom for LadybugDB cold-start and
	// SQLite cache miss while still failing if the embedder regresses back
	// inline (Inline would push the hot path to >=200ms).
	hotPathBudget = 150 * time.Millisecond
	// bgVecDeadline guards the background-vec drain. Two entity embeds
	// (name + summary) at blockDuration=200ms each is 400ms of ideal
	// embed wall-clock; the deadline holds the rest for LadybugDB
	// cold-start, SQLite cache warmup, and goroutine-pool contention
	// from parallel sibling tests. 5s gives enough headroom that the
	// canary flags real regressions (the goroutine never starts, the
	// embedder leaks, etc.) without flaking under heavy package-test
	// load. Pre-fix the actual budget was tens of ms — anything that
	// pushes us past 5s is genuinely broken, not noisy.
	bgVecDeadline = 5 * time.Second
)

// TestApply_Episode_AsyncEmbedding_ReturnsBeforeEmbedderCompletes is the
// canary for the async-embed contract. NOT t.Parallel: the test measures
// wall-clock latency, and parallel-test pressure on the Go scheduler +
// LadybugDB cold-start adds up to noisy >150ms hot paths even when the
// embed itself is fully detached. Without the serial guard, the canary
// flakes on a busy machine despite the production behaviour being correct.
func TestApply_Episode_AsyncEmbedding_ReturnsBeforeEmbedderCompletes(t *testing.T) {
	ctx := testutil.TestContext(t)

	emb := &slowEmbedder{dim: storage.VectorDim, block: blockDuration}
	d := newDispatcher(t, ctx, emb)

	ev := makeEpisodeEvent(t, "01JEPISODE0000000000000001", "Frodo works at Erebor.", "test")

	start := time.Now()
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > hotPathBudget {
		t.Errorf("Apply hot path took %v, want <= %v (embedder is async, not inline)", elapsed, hotPathBudget)
	}

	// The vec0 row should materialise within the background deadline, well
	// after Apply returned but well before the test times out.
	waitCtx, cancel := context.WithTimeout(ctx, bgVecDeadline)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}
}
