package kernel

import (
	"context"
	"time"
)

// RewriteHint carries optional caller context the rewriter may use to
// resolve relative temporal anchors in the original query. Zero-value hint
// is valid and disables date arithmetic in the rewriter.
type RewriteHint struct {
	// CurrentDate is the caller's "now" anchor used to resolve relative
	// phrases ("two months ago", "last Tuesday") to absolute timestamps in
	// RewriteResult.At. Nil disables date arithmetic; the rewriter then
	// only expands synonyms.
	CurrentDate *time.Time
}

// RewriteResult is the structured output of a query rewrite. Query is
// always non-empty on success; At is non-nil only when the rewriter
// resolved a temporal anchor against the supplied CurrentDate hint.
type RewriteResult struct {
	// Query is the expanded search phrase. Non-empty on success.
	Query string

	// At is the resolved absolute timestamp when the original query
	// contained a relative date anchor and CurrentDate was supplied.
	// Callers fold this into RecallRequest.At for bi-temporal filtering.
	At *time.Time
}

// QueryRewriter expands a terse recall query into a richer search phrase
// before the embedding and FTS steps run. Implementations live in
// internal/kernel/rewriter/ so the kernel package stays free of the
// prompts import (prompts imports kernel, not the other way around).
//
// On any error the implementation must return a zero-value RewriteResult
// so callers can treat the (result, error) pair as best-effort and fall
// back to the original query.
type QueryRewriter interface {
	Rewrite(ctx context.Context, original string, hint RewriteHint) (RewriteResult, error)
}
