package cache_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/cache"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
)

// batchingFakeEmbedder is a fakeEmbedder that also satisfies embedder.Batcher.
// It records single-call and batch-call counts separately so a test can
// assert which path the cache took.
type batchingFakeEmbedder struct {
	model       string
	embedCalls  atomic.Int64
	batchCalls  atomic.Int64
	batchInputs atomic.Int64
}

func (b *batchingFakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	b.embedCalls.Add(1)
	return seededVec(text), nil
}

func (b *batchingFakeEmbedder) Model() string { return b.model }

func (b *batchingFakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	b.batchCalls.Add(1)
	b.batchInputs.Add(int64(len(texts)))
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = seededVec(t)
	}
	return out, nil
}

// seededVec produces a deterministic VectorDim-length vector seeded from text
// so persisted hits round-trip identically.
func seededVec(text string) []float32 {
	v := make([]float32, storage.VectorDim)
	var sum uint64
	for _, r := range text {
		sum = sum*131 + uint64(r)
	}
	for i := range v {
		v[i] = float32((sum+uint64(i))%1000) / 1000.0
	}
	return v
}

// TestCachingEmbedder_EmbedBatch_RoutesMissesThroughBatcher asserts that when
// the inner satisfies embedder.Batcher, the cache forwards the batch of
// missed texts in a single EmbedBatch call rather than N Embed calls. This is
// the LongMemEval-write-path win: N mention names hit Ollama in one HTTP
// round trip.
func TestCachingEmbedder_EmbedBatch_RoutesMissesThroughBatcher(t *testing.T) {
	store := openStore(t)
	inner := &batchingFakeEmbedder{model: "bge-m3"}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	texts := []string{"Frodo", "Erebor Inc.", "Berlin", "Rivendell"}
	vecs, err := ce.EmbedBatch(t.Context(), texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if got, want := len(vecs), len(texts); got != want {
		t.Fatalf("returned %d vectors, want %d", got, want)
	}
	for i, v := range vecs {
		if !vecsEqual(v, seededVec(texts[i])) {
			t.Errorf("vec[%d] does not match seededVec for text %q", i, texts[i])
		}
	}
	if got := inner.batchCalls.Load(); got != 1 {
		t.Errorf("inner.EmbedBatch calls = %d, want 1 (single round trip for cold misses)", got)
	}
	if got := inner.embedCalls.Load(); got != 0 {
		t.Errorf("inner.Embed calls = %d, want 0 (Batcher branch should not call per-text Embed)", got)
	}
	if got, want := inner.batchInputs.Load(), int64(len(texts)); got != want {
		t.Errorf("inner.EmbedBatch input length = %d, want %d", got, want)
	}
}

// TestCachingEmbedder_EmbedBatch_HitsBypassInner verifies the cache-aware path:
// pre-warm with a single Embed, then EmbedBatch that includes the warmed text
// alongside fresh ones. Only the fresh ones must hit inner.EmbedBatch.
func TestCachingEmbedder_EmbedBatch_HitsBypassInner(t *testing.T) {
	store := openStore(t)
	inner := &batchingFakeEmbedder{model: "bge-m3"}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	if _, err := ce.Embed(t.Context(), "Frodo"); err != nil {
		t.Fatalf("warm Embed: %v", err)
	}
	// Reset counters so the batch assertions only cover the EmbedBatch call.
	inner.embedCalls.Store(0)
	inner.batchCalls.Store(0)
	inner.batchInputs.Store(0)

	vecs, err := ce.EmbedBatch(t.Context(), []string{"Frodo", "Erebor", "Berlin"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("len(vecs) = %d, want 3", len(vecs))
	}
	if got := inner.batchCalls.Load(); got != 1 {
		t.Errorf("inner.EmbedBatch calls = %d, want 1", got)
	}
	if got, want := inner.batchInputs.Load(), int64(2); got != want {
		t.Errorf("inner.EmbedBatch input length = %d, want %d (only the 2 misses)", got, want)
	}
}

// TestCachingEmbedder_EmbedBatch_FallbackWhenInnerNotBatcher proves the cache
// degrades to per-text Embed calls when the inner does NOT satisfy
// embedder.Batcher. Order of returned vectors must still match input order.
func TestCachingEmbedder_EmbedBatch_FallbackWhenInnerNotBatcher(t *testing.T) {
	store := openStore(t)
	// fakeEmbedder (no EmbedBatch) — defined in embedding_test.go.
	inner := &fakeEmbedder{model: "bge-m3", vec: fixedVec(7)}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	texts := []string{"a", "b", "c"}
	vecs, err := ce.EmbedBatch(t.Context(), texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if got, want := len(vecs), len(texts); got != want {
		t.Fatalf("returned %d vectors, want %d", got, want)
	}
	if got, want := inner.calls.Load(), int64(len(texts)); got != want {
		t.Errorf("inner.Embed calls = %d, want %d (fallback branch)", got, want)
	}
	for _, v := range vecs {
		if !vecsEqual(v, fixedVec(7)) {
			t.Errorf("vector does not match the fixed inner output")
		}
	}
}

// TestCachingEmbedder_EmbedBatch_Empty returns nil without any inner call.
func TestCachingEmbedder_EmbedBatch_Empty(t *testing.T) {
	store := openStore(t)
	inner := &batchingFakeEmbedder{model: "bge-m3"}
	ce := cache.New(inner, store.Reader(), store.Writer(), nil)

	vecs, err := ce.EmbedBatch(t.Context(), nil)
	if err != nil {
		t.Fatalf("EmbedBatch(nil): %v", err)
	}
	if len(vecs) != 0 {
		t.Errorf("len(vecs) = %d, want 0", len(vecs))
	}
	if got := inner.batchCalls.Load(); got != 0 {
		t.Errorf("inner.EmbedBatch calls = %d, want 0 (empty input is a no-op)", got)
	}
}
