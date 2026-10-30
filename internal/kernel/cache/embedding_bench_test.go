package cache_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/cache"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// Benchmarks for the embedding cache hot path. Per agent-docs/profiling.md
// and AGENTS.md rule 10, hot-path code lands with baseline numbers in the
// PR description. The acceptance criterion for issue #87 requires the hit
// path to be at least 100x faster than the miss path for a 1024-dim vector;
// these benchmarks supply both numbers.

// slowEmbedder approximates real-Ollama latency by sleeping the configured
// duration before returning a fixed vector. The default 10ms is much faster
// than real BGE-M3 inference (~50-200ms) but enough to make the cache-hit
// vs cache-miss ratio meaningful in a CI-affordable bench run.
type slowEmbedder struct {
	model string
	vec   []float32
	delay time.Duration
}

func (s *slowEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([]float32, len(s.vec))
	copy(out, s.vec)
	return out, nil
}

func (s *slowEmbedder) Model() string { return s.model }

func openBenchStore(b *testing.B) *sqlite.Store {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.db")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		b.Fatalf("sqlite.Open: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

func benchVec() []float32 {
	v := make([]float32, storage.VectorDim)
	for i := range v {
		v[i] = float32(i) * 0.001
	}
	return v
}

// BenchmarkCachingEmbedder_Hit measures the warm-cache path: SQLite SELECT
// + blob decode + return. The inner embedder is wired in but is never called
// after the warm-up; if the cache logic regressed and called Inner.Embed,
// the slowEmbedder.delay would dominate the timing and the regression would
// be impossible to miss.
func BenchmarkCachingEmbedder_Hit(b *testing.B) {
	store := openBenchStore(b)
	inner := &slowEmbedder{model: "bge-m3", vec: benchVec(), delay: 10 * time.Millisecond}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)
	ctx := context.Background()

	// Warm-up: one Embed call per key persists the row.
	if _, err := ce.Embed(ctx, "warmup"); err != nil {
		b.Fatalf("warmup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		v, err := ce.Embed(ctx, "warmup")
		if err != nil {
			b.Fatalf("Embed: %v", err)
		}
		if len(v) != storage.VectorDim {
			b.Fatalf("len = %d, want %d", len(v), storage.VectorDim)
		}
	}
}

// BenchmarkCachingEmbedder_Miss measures the cold-cache path: SELECT (miss)
// + Inner.Embed (slow) + INSERT + return. Each iteration uses a unique key
// so every call is a miss; the 10ms inner delay dominates.
//
// This benchmark is the baseline the cache-hit numbers are compared against
// for the >=100x acceptance criterion. The inner delay is configurable via
// the slowEmbedder.delay field if a longer/shorter approximation of real
// Ollama inference is preferred.
func BenchmarkCachingEmbedder_Miss(b *testing.B) {
	store := openBenchStore(b)
	inner := &slowEmbedder{model: "bge-m3", vec: benchVec(), delay: 10 * time.Millisecond}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		v, err := ce.Embed(ctx, fmt.Sprintf("miss-%d", i))
		if err != nil {
			b.Fatalf("Embed: %v", err)
		}
		if len(v) != storage.VectorDim {
			b.Fatalf("len = %d, want %d", len(v), storage.VectorDim)
		}
	}
}

// BenchmarkCachingEmbedder_HitParallel measures the warm-cache path under
// concurrent readers, which is the realistic recall workload (the composer
// fans out reads over the goroutine pool). Slowdown vs the serial Hit
// number reflects contention on the reader pool + the SQLite WAL.
func BenchmarkCachingEmbedder_HitParallel(b *testing.B) {
	store := openBenchStore(b)
	inner := &slowEmbedder{model: "bge-m3", vec: benchVec(), delay: 10 * time.Millisecond}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)
	ctx := context.Background()

	if _, err := ce.Embed(ctx, "warmup"); err != nil {
		b.Fatalf("warmup: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := ce.Embed(ctx, "warmup"); err != nil {
				b.Fatalf("Embed: %v", err)
			}
		}
	})
}

// BenchmarkCachingEmbedder_MissNoDelay isolates the SQLite write overhead
// the cache adds on top of Inner.Embed. Inner returns instantly; the delta
// between this number and BenchmarkCachingEmbedder_Hit measures the cache
// machinery itself (hash + lookup + serialise + insert).
func BenchmarkCachingEmbedder_MissNoDelay(b *testing.B) {
	store := openBenchStore(b)
	inner := &slowEmbedder{model: "bge-m3", vec: benchVec(), delay: 0}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		if _, err := ce.Embed(ctx, fmt.Sprintf("nd-%d", i)); err != nil {
			b.Fatalf("Embed: %v", err)
		}
	}
}
