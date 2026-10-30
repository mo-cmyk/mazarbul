// Package embedder defines the embedding-provider contract consumed by the
// kernel writer and recall composer. Implementations: provider/ollama.Client
// (real, talks to a local Ollama daemon) and kernel/cache.CachingEmbedder
// (wraps any Embedder and deduplicates calls by sha256(text || model || dim)).
//
// The interface lives in the kernel rather than in internal/plugin because
// the cache wraps any future provider with the same dedup semantics; pinning
// the contract here keeps that wrap point obvious. The package is named
// "embedder" rather than "embed" to avoid shadowing the stdlib embed package
// at every call site.
package embedder

import "context"

// Embedder produces a fixed-dimension dense vector for a piece of text.
// Implementations must return a slice of length storage.VectorDim; any other
// length is a bug the caller may treat as fatal (the Ollama client and the
// vector layer both enforce the same invariant at their respective
// boundaries, so a mismatch surfaces close to the source).
type Embedder interface {
	// Embed sends text through the underlying model and returns the dense
	// embedding. Honours ctx cancellation.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Model returns the canonical model identifier used by Embed. The cache
	// includes this string in the content-hash key so two providers (or two
	// model versions) cannot collide on the same cache row.
	Model() string
}

// Batcher is the optional capability for embedders that can serve N inputs
// in a single round trip. Implementations: provider/ollama.Client (talks to
// /api/embed) and kernel/cache.CachingEmbedder (cache-aware: drops cached
// inputs, batches only the misses through Inner).
//
// Callers type-assert against this interface and fall back to per-call Embed
// when the assertion fails. A LongMemEval-style write fan-out that embeds N
// mention names + N entity names benefits from batching at two levels: one
// HTTP round trip instead of N, and one OLLAMA_NUM_PARALLEL slot instead of
// queueing N requests through Ollama's per-model serialisation.
//
// Order is preserved: the returned slice is len(texts), and index i
// corresponds to texts[i]. Empty input returns (nil, nil) without any
// underlying call.
type Batcher interface {
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}
