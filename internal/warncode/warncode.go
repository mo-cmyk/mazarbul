// Package warncode is the frozen catalog of stable, machine-readable warning
// codes maz surfaces to agents and scripts. A warning is non-fatal advice — a
// near-duplicate kind, a degraded retrieval path, a merged-on-load record —
// that an agent can branch on by CODE rather than by parsing prose, which can
// change. Codes are MAZ-<AREA>-NNN and are APPEND-ONLY: never renumber or
// repurpose an existing code, only add new ones, so an agent's branch on
// "MAZ-RECALL-001" keeps meaning the same thing across releases.
//
// This package has no Mazarbul dependencies on purpose: every surface that
// emits a warning (the ontology authoring tools, recall, bulk load, the
// feedback loop) can import it without a dependency cycle. The human-readable
// catalog lives in agent-docs/warnings.md and must stay in sync with the consts
// here.
package warncode

import "fmt"

// Warning is the wire shape every warning serializes to: a stable Code plus a
// human-readable Message. Tools surface a `warnings: [{code, message}]` array.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Ontology authoring warnings (defineEntityKind / defineSchema).
const (
	// CodeKindCloseMatch: the candidate kind is a near-duplicate of an existing
	// one; it was minted with a SKOS closeMatch alias rather than reused.
	CodeKindCloseMatch = "MAZ-KIND-001"
	// CodeKindDedupDegraded: the semantic dedup pass was skipped (no embedder
	// wired, or it timed out); only lexical dedup ran, so a semantic synonym may
	// have slipped through. Expected on a cold/offline embedder.
	CodeKindDedupDegraded = "MAZ-KIND-002"
)

// Schema-evolution warnings (schema preview / defineSchema).
const (
	// CodeSchemaSupersede: applying the change supersedes the kind to a new
	// schema version (vN → vN+1).
	CodeSchemaSupersede = "MAZ-SCHEMA-001"
	// CodeSchemaOrphan: the change would orphan entities that rely on a field
	// it removes or renames; the count is in the message.
	CodeSchemaOrphan = "MAZ-SCHEMA-002"
	// CodeSchemaCollapse: the candidate collapses into an existing kind rather
	// than introducing a distinct one.
	CodeSchemaCollapse = "MAZ-SCHEMA-003"
)

// Retrieval warnings (recall).
const (
	// CodeRecallEmbedderDegraded: the embedder was unavailable or exceeded the
	// per-recall deadline, so results are FTS-only (lexical). Ranking quality is
	// reduced but recall still returns.
	CodeRecallEmbedderDegraded = "MAZ-RECALL-001"
	// CodeRecallFuzzyFallback: verbatim full-text matches were sparse, so a
	// prefix/fuzzy fallback widened the query. Useful when a term was misspelled.
	CodeRecallFuzzyFallback = "MAZ-RECALL-002"
)

// Bulk-load warnings (maz load).
const (
	// CodeLoadDuplicateMerged: in merge mode a record collided on an existing
	// UID and was skipped — the existing event is kept and no duplicate is
	// appended (merge is skip-if-present, not a field-level merge).
	CodeLoadDuplicateMerged = "MAZ-LOAD-001"
	// CodeLoadRecordSkipped: a malformed or invalid record was skipped; the
	// count is in the message (dry-run reports without writing).
	CodeLoadRecordSkipped = "MAZ-LOAD-002"
)

// Feedback-loop warnings (agentFeedback / feedback promotion).
const (
	// CodeFeedbackPromoted: a recurring lesson crossed the recurrence threshold
	// and auto-promoted into the per-session guidance; review with `maz feedback
	// gotchas` (pin / edit / veto).
	CodeFeedbackPromoted = "MAZ-FEEDBACK-001"
)

// New builds a Warning with a code and a formatted message.
func New(code, format string, args ...any) Warning {
	return Warning{Code: code, Message: fmt.Sprintf(format, args...)}
}
