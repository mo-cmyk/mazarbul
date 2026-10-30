package kernel

import "context"

// Reranker is an optional post-fusion re-ranking step. Implementations
// receive the fused hit slice (already sorted by RRF score, length already
// capped to the configured limit) and may return a re-sorted slice of the
// same or fewer items. The returned slice must only contain items from the
// input; inserting new UIDs is a contract violation.
//
// When nil, the HybridQuery pass-through behaviour is preserved so existing
// callers are unaffected. The nil guard lives in collectHits, not here.
type Reranker interface {
	// Rerank re-orders hits by topical relevance to query. Implementations
	// may issue network calls (LLM scoring) or compute locally. Context
	// cancellation must be honoured; a cancelled context should return the
	// original slice unchanged alongside ctx.Err() so the caller can decide
	// whether to degrade gracefully or surface the error.
	Rerank(ctx context.Context, query string, hits []Hit) ([]Hit, error)
}
