package dispatcher_test

// dispatcher_throughput_bench_test.go is the back-to-back benchmark that
// quantifies the Apply-hot-path improvement from Chunks 14, 17, 18 (async
// episode / mention / entity embedding). It applies N Episode events
// sequentially against a dispatcher whose embedder sleeps for a deterministic
// blockDuration per call, then reports wall-clock per Apply.
//
// Pre-chunk-14 shape: Apply blocks on the embedder, so total time is
// approximately N * blockDuration.
// Current shape: Apply returns as soon as the row + graph are durable; the
// embed call lives on a detached goroutine. Total time should be dominated
// by SQLite tx + LadybugDB MERGE per Apply, not by blockDuration.
//
// The benchmark is portable across pre/post fix code lines: it does not
// call WaitForBackgroundVec (which only exists post-fix) and only measures
// the synchronous Apply hot path. Same fixture, same data, same code path —
// only the dispatcher.go internals differ between revisions.

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math/rand"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// benchEmbedder mimics a cold Ollama bge-m3 round-trip. Sleeps for block
// before returning a deterministic vector. Inlined here so the bench file
// compiles standalone against any revision of dispatcher.go (the
// pre-chunk-14 baseline does not ship the async-embedding helpers but uses
// the same Embedder interface).
type benchEmbedder struct {
	dim   int
	block time.Duration
}

func (s *benchEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
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

// BenchmarkApply_EpisodeThroughput reports the wall-clock per Apply call
// for a sequential burst of N=20 episodes against a 100ms blockDuration
// embedder. Run with:
//
//	go test -tags fts5 -bench=BenchmarkApply_EpisodeThroughput -benchtime=1x -run=^$ \
//	    ./internal/kernel/storage/dispatcher/
func BenchmarkApply_EpisodeThroughput(b *testing.B) {
	const episodesPerRun = 20
	const blockDur = 100 * time.Millisecond

	for range b.N {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		runEpisodeBench(ctx, b, episodesPerRun, blockDur)
		cancel()
	}
}

func runEpisodeBench(ctx context.Context, b *testing.B, episodes int, block time.Duration) {
	b.Helper()
	emb := &benchEmbedder{dim: storage.VectorDim, block: block}
	d, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  b.TempDir(),
		Embedder: emb,
	})
	if err != nil {
		b.Fatalf("dispatcher.Open: %v", err)
	}
	defer func() { _ = d.Close() }()

	totalHotPath := time.Duration(0)
	for i := range episodes {
		uid := benchULID(i)
		ev := benchEpisodeEvent(b, uid, "Frodo works at Erebor Inc. based in Berlin.")
		start := time.Now()
		if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
			b.Fatalf("Apply %d: %v", i, err)
		}
		totalHotPath += time.Since(start)
	}
	perApply := totalHotPath / time.Duration(episodes)
	b.ReportMetric(float64(perApply.Microseconds())/1000.0, "ms/apply")
	b.ReportMetric(float64(episodes)/totalHotPath.Seconds(), "applies/sec")
	b.Logf("hot-path total = %v over %d applies (%.2fms/apply, %.2f applies/sec, block=%v)",
		totalHotPath, episodes, float64(perApply.Microseconds())/1000.0,
		float64(episodes)/totalHotPath.Seconds(), block)
}

// benchEpisodeEvent constructs an Episode-payload kernel.Event for the
// benchmark. Mirrors makeEpisodeEvent (used by *testing.T-typed tests) but
// works with *testing.B.
func benchEpisodeEvent(b *testing.B, uid, text string) kernel.Event {
	b.Helper()
	payload, err := json.Marshal(map[string]any{
		"text":       text,
		"source":     "bench",
		"confidence": 0.9,
	})
	if err != nil {
		b.Fatalf("marshal payload: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindEpisode),
		Source:     "bench",
		Confidence: 0.9,
		Payload:    payload,
	}
}

// benchULID returns a 26-char ULID-shaped string deterministic in i. The
// alphabet matches Crockford base32 (no I/L/O/U); the prefix is fixed and
// 8 trailing chars encode i in lower-significant-first base32.
func benchULID(i int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	out := []byte("01HBENCH000000ULID")
	n := uint64(i)
	for range 8 {
		out = append(out, alphabet[n%32])
		n /= 32
	}
	return string(out)
}
