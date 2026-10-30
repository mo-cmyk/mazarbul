package kernel

import (
	"context"
	"iter"
	"time"
)

// RecallRequest is the shape of a single recall query. The kernel composes
// across the four indexes (vector + FTS + graph + relational) per the
// architecture spec's hybrid retrieval pipeline (see section 12 of the spec).
type RecallRequest struct {
	// Query is the natural-language question or keyword phrase. The composer
	// uses it for both vector embedding and FTS tokenization.
	Query string

	// Scopes restricts results to specific scope identifiers. M1 ships with
	// the implicit single-user scope; M5 adds project / personal / team.
	Scopes []string

	// Since narrows to events with CreateTime >= Since. Zero value means
	// "no lower bound".
	Since time.Time

	// Until narrows to events with CreateTime < Until. Zero value means
	// "no upper bound".
	Until time.Time

	// Kinds restricts to specific event kinds.
	Kinds []EventKind

	// Limit caps the number of hits returned. Zero means the composer's
	// internal default (currently top-N then re-rank to top-10).
	Limit int

	// MinConfidence filters out low-confidence hits before re-ranking.
	// Default 0.0 (no filter).
	MinConfidence float64

	// Near restricts results to episodes citing entities within a geographic
	// radius of a named place. When nil, no spatial filter is applied.
	Near *NearFilter

	// At overrides the temporal "now" parameter used in graph-edge validity
	// filtering. When nil, time.Now().UTC() is used. Pass a past time to
	// answer "what was true at time T?" queries.
	At *time.Time

	// AsOf bounds the system clock used for recall: only events whose
	// CreateTime <= AsOf are visible, and only relations whose create_time
	// (= transaction time) <= AsOf participate in graph + relational fusion.
	// Used for historical-lens queries ("what did we know on 2025-01-01?").
	// Orthogonal to At, which scopes valid-time. When nil, no transaction
	// upper bound is applied.
	AsOf *time.Time

	// CurrentDate is the caller's "now" anchor used by the query rewriter to
	// resolve relative temporal phrases ("two months ago", "last Tuesday")
	// into an absolute timestamp. The resolved timestamp is folded into At
	// when the caller did not supply an explicit At. Nil disables date
	// arithmetic in the rewriter; the rewriter then only expands synonyms.
	CurrentDate *time.Time

	// NoGraph bypasses graph expansion (1-hop and 2-hop traversal via
	// LadybugDB) and the name-vector lane. When true, recall degrades to the
	// original vector + FTS fusion. Useful for latency-sensitive callers that
	// do not need graph context.
	NoGraph bool

	// RewriteQuery, when true, expands the Query string via the
	// query-rewrite-v1 LLM prompt before the embedding and FTS steps run.
	// Requires a Rewriter to be wired via WithRewriter; when no Rewriter is
	// configured the flag is silently ignored (no-op degradation). Off by
	// default; enable only after eval shows improvement.
	RewriteQuery bool

	// Anchor, when non-empty, restricts candidates to the N-hop frontier of
	// the given entity ULID before fusion. AnchorDepth controls the hop
	// count (1..6, default 1). The frontier is resolved via
	// navigate.Neighbors and the pre-fusion candidate set is narrowed to
	// episodes that cite a frontier entity. Useful when the agent already
	// holds an entity anchor and wants topically-adjacent recalls without
	// widening the search to all memory.
	Anchor string

	// AnchorDepth is the hop radius around Anchor (1..MaxHops). Zero is
	// treated as 1 (the immediate neighbourhood). Capped at navigate.MaxHops
	// (6) by the composer.
	AnchorDepth int

	// EmbedDeadline, when positive, caps the query embedding call. On
	// context.DeadlineExceeded the composer degrades to FTS-only and reports
	// warncode.CodeRecallEmbedderDegraded. Zero means no per-recall deadline
	// (the caller's context deadline still applies).
	EmbedDeadline time.Duration

	// Explain, when true, adds per-lane candidate counts and timing to the
	// RecallReport under the optional Meta field. Off by default to preserve
	// the compact envelope.
	Explain bool

	// anchorFrontier is the pre-resolved set of entity UIDs that form the
	// N-hop neighbourhood of RecallRequest.Anchor. It is populated by
	// collectHitsWithMeta when Anchor is set and a navigator is wired; the
	// filtering logic in collectHits reads it to narrow the hydrated row set
	// pre-fusion. Unexported: callers always set Anchor/AnchorDepth.
	anchorFrontier map[string]struct{}
}

// HitType discriminates the two first-class recall hit shapes: an episode hit
// (the result of matching an event row) and an entity hit (a directly-created
// entity that has no associated episode, surfaced when its name-vector matches
// the query).  All hits carried in a Recall result set have a non-empty HitType.
type HitType string

const (
	// HitTypeEpisode is the default: the hit is an Episode event row.
	HitTypeEpisode HitType = "episode"
	// HitTypeEntity is set when the hit is a bare entity (no surrounding
	// episode text).  Text carries the entity's display name; Citations
	// carries "entities/{uid}".
	HitTypeEntity HitType = "entity"
	// HitTypeInsight is set when the hit is a synthesized insight (issue
	// #231). Text carries the insight statement; Citations carries the
	// insight's sourceUids. Only hot insights (state='hot') surface; a
	// demoted insight leaves hot recall until reinforced or rebuilt.
	HitTypeInsight HitType = "insight"
	// HitTypeConsolidated is set when the hit is a consolidated memory
	// (issue #231). Text carries the summary; Citations carries the
	// compressed episode sourceUids.
	HitTypeConsolidated HitType = "consolidated"
)

// Hit is a single recall result with the score breakdown that fed into the
// final ranking. The breakdown is observable so callers can debug ranking.
type Hit struct {
	// HitType discriminates episode hits (the common case) from bare-entity
	// hits (entities created directly with no surrounding episode text).
	// Always populated; defaults to HitTypeEpisode for backward compatibility.
	HitType HitType
	UID     string
	Kind    EventKind

	// Text is the full clean episode text (no FTS5 highlight markup,
	// no truncation). This is the agent-facing field — every MCP
	// caller can ground its reasoning on Text without HTML stripping
	// or window inflation. Snippet remains for human-rendered UIs
	// that want the FTS highlight excerpt. Populated by HybridQuery
	// from the hydrated row's text column.
	Text string

	// Snippet is the FTS-driven highlight excerpt (with <b>...</b>
	// markup) when a lexical match exists, or a runesMax-truncated
	// substring of Text for vec-only hits. Useful for TUI rendering;
	// agents should prefer Text.
	Snippet    string
	Confidence float64
	CreateTime time.Time

	// FusedScore is the final rank score (higher is better).
	FusedScore float32

	// Components records the per-index scores that fed into FusedScore.
	Components ScoreComponents

	// Citations link back to the source events that contributed this hit.
	// Always at least one entry; multi-event hits (e.g., entity-driven
	// recall) cite every contributing event.
	Citations []string

	// Superseded is true when at least one relation derived from this
	// episode (relations.source_event_uid = UID) has a non-NULL
	// valid_until, i.e. its fact has been invalidated by a later
	// remember/supersede. Composer renders this as a "superseded" marker
	// so the LLM can prefer current evidence on "now / currently"
	// questions. Populated by HybridQuery after the final top-K cut.
	Superseded bool

	// Entities are the graph anchors this hit is "about" — the entities the
	// source episode mentions, recovered by reverse-joining the mentions
	// table on the episode UID. They are the seam of the seed-then-traverse
	// model: an agent runs one recall, then pivots into structured
	// navigation (neighbors/relations/get) from these anchors without any
	// further LLM in the loop. Empty when the episode mentions no resolved
	// entity (e.g. a free-text note). Recovered by a single indexed
	// mentions→entities batch join over the post-limit shortlist — it is
	// independent of the LadybugDB expansion lanes, so it is populated even
	// when RecallRequest.NoGraph is set. Populated by HybridQuery after the
	// final top-K cut. For document hits the anchors are the entities the
	// document's wikilinks resolve to (mentions/links_to edges).
	Entities []EntityRef

	// Source locates the hit inside a source document when the hit is a
	// document chunk (Kind = documentRevision): the vault-relative path, the
	// nearest preceding markdown heading, and the 1-based line the winning
	// chunk starts on — enough for a `notes/foo.md › ## Goals` citation. Nil
	// for episode hits.
	Source *SourceLocation

	// EventSource is the originating event's Source string (the episodes.source
	// column), e.g. "remember", "extractor/v1", or a "connector:<name>" tag for
	// content pulled by an ingestion connector. The MCP tool layer classifies it
	// (ontology.IsConnectorSource) to attach an untrusted-provenance trust label
	// to the hit: a connector-sourced episode is external input whose extracted
	// facts are status=proposed, not auto-trusted (#211, #212). Carried as the
	// raw string because the kernel package cannot import ontology (the import
	// runs the other way); the classification lives where ontology is in scope.
	EventSource string

	// FetchTime is when the external source grounding this hit was last fetched:
	// the source_refs.fetch_time of the SourceRef whose subject is this hit, or
	// of the external_ref the hit has_primary_source (captureExternal, #G). Zero
	// when the hit has no external provenance. The MCP recall tool classifies it
	// against the per-connector freshness TTL into a freshnessStatus
	// (fresh|stale|access-unverified|unknown); the kernel stays config-free and
	// carries only the raw inputs. Populated by attachFreshness after the top-K
	// cut, the same shape as markSuperseded.
	FetchTime time.Time

	// SourceConnector is the connector that produced the grounding SourceRef
	// (e.g. "notion", "linear"). Empty when the hit has no external provenance or
	// the SourceRef omitted it. Drives the recall tool's per-connector TTL lookup.
	SourceConnector string

	// ExternalURL is the dereferenceable location of the external source item,
	// surfaced so the agent can re-fetch live when the cached snapshot is stale.
	// Empty when absent. Never emitted to plaintext-scope render output.
	ExternalURL string

	// Tombstone marks the grounding SourceRef as reported gone ("deleted") or
	// inaccessible ("access-revoked") on a prior re-capture; empty for a live
	// snapshot. The recall tool maps a non-empty value to freshnessStatus
	// access-unverified so the agent re-checks access rather than trusting the
	// cached summary.
	Tombstone string
}

// SourceLocation is a section-anchored pointer into a vault document.
type SourceLocation struct {
	// Path is the vault-relative document path, e.g. notes/project.md.
	Path string
	// Heading is the nearest markdown heading preceding the cited chunk
	// (empty when the chunk precedes the first heading).
	Heading string
	// LineStart is the 1-based line number the cited chunk starts on.
	LineStart int
}

// EntityRef is a lightweight graph anchor attached to a recall Hit. It carries
// just enough to (a) display the entity inline and (b) pivot into traversal via
// the Navigator (GetEntity/Neighbors/EntityRelations) — the full hydrated
// record is one structured call away, so the ref intentionally stays minimal.
type EntityRef struct {
	UID         string
	Kind        string
	DisplayName string
}

// ScoreComponents is the per-index score breakdown for a Hit.
type ScoreComponents struct {
	Vector float32
	FTS    float32
	// Graph is the RRF rank score from the entity-graph traversal lane (1-hop
	// and conditional 2-hop expansion via RELATES_TO edges in LadybugDB).
	Graph   float32
	Recency float32
	// NameEmb is the RRF rank score from the entity-name-vector lane
	// (entities_vec, vector_type='name'). Non-zero only when a GraphConn
	// with entities_vec support is wired and the query embeds at least one
	// entity name match.
	NameEmb float32

	// RawSemantic is the cosine similarity in [0, 1] between the query
	// embedding and this candidate's episode embedding. Populated by the
	// weighted-fusion path; zero in RRF mode. Mem0 v3 uses this directly
	// (not the RRF rank) so a "barely-related rank-1" and a "perfect-match
	// rank-1" produce different fused scores. Observable for ablation
	// reporting + the pre-fusion semantic gate.
	RawSemantic float32

	// RawBM25 is the sigmoid-normalized BM25 score in [0, 1]. The sigmoid
	// midpoint and steepness adapt to query token count (3 tokens →
	// midpoint 5.0, steepness 0.7; 15 tokens → midpoint 12.0, steepness
	// 0.5) so short and long queries produce comparable magnitudes —
	// matching mem0/utils/scoring.py's get_bm25_params.
	RawBM25 float32

	// Strength is the passive-reinforcement multiplier component applied to
	// FusedScore (issue #231): reinforcement(accessCount)·decay(now −
	// lastAccessed), in [0, 1]. Zero when the memory has never been recalled
	// or the strength boost is disabled. Observable so callers can see how
	// recall frequency moved the rank.
	Strength float32
}

// NearFilter restricts recall to episodes citing entities within a geographic
// radius of a named place. Both fields are required when the filter is non-nil.
type NearFilter struct {
	// PlaceQuery is the name of the place to resolve (FTS + name-vector lookup).
	// Required when NearFilter is non-nil.
	PlaceQuery string

	// RadiusKm is the search radius in kilometres. Required when NearFilter is
	// non-nil. The default applied by the MCP tool when the caller omits it is
	// 50 km.
	RadiusKm float64
}

// TraverseRequest is a graph walk request. Used for "find related entities
// within K hops" queries.
type TraverseRequest struct {
	// StartUIDs are the starting entity UIDs.
	StartUIDs []string

	// MaxHops caps the traversal depth. Required (no default).
	MaxHops int

	// Predicates restricts to specific relation predicates. Empty means
	// "follow any edge".
	Predicates []string

	// Limit caps the number of nodes returned. Zero means the composer's
	// internal default (currently 100).
	Limit int
}

// Node is a single graph node visited during a traversal.
type Node struct {
	UID      string
	Kind     EventKind
	Distance int
	Path     []string
}

// Query is the kernel's read surface. Real implementations compose recall
// and traverse across the four storage layers via the dispatcher; this
// interface hides the composition from callers.
type Query interface {
	Recall(ctx context.Context, req RecallRequest) iter.Seq2[Hit, error]
	Traverse(ctx context.Context, req TraverseRequest) iter.Seq2[Node, error]
}

// Lane names for RecallReport.Lanes, listed in a stable canonical order. They
// name the four retrieval lanes a hybrid recall can run; a degraded
// (embedder-unavailable) recall reports only the lanes that do not need a
// query embedding (LaneFTS and, when seeded, LaneGraph).
const (
	LaneVector  = "vector"
	LaneFTS     = "fts"
	LaneGraph   = "graph"
	LaneNameEmb = "nameEmb"
)

// RecallReport carries per-request metadata about how a recall ran: whether it
// degraded below full hybrid and which lanes actually contributed. It is
// surfaced to agents (via the recall MCP tool) so a BM25-only result is never
// mistaken for full hybrid recall.
//
// Degraded is true when the dense-vector lanes were skipped because the
// embedder was nil or its Embed call failed for the request — recall then ran
// FTS(+graph)-only. Lanes lists the active lanes in LaneVector/LaneFTS/
// LaneGraph/LaneNameEmb order; it is always non-nil (empty for an empty query).
type RecallReport struct {
	Degraded bool
	Lanes    []string

	// Warnings is the list of machine-readable advisory codes that
	// accumulated during the recall (e.g. MAZ-RECALL-001 when the embedder
	// degraded, MAZ-RECALL-002 when a prefix fallback fired). Nil when no
	// warnings were raised; always omitted from the compact envelope.
	Warnings []RecallWarning

	// Meta carries optional per-lane telemetry, populated only when
	// RecallRequest.Explain is true. Nil otherwise (off by default).
	Meta *RecallMeta
}

// RecallWarning is one machine-readable warning raised during a recall.
// Code is a stable MAZ-<AREA>-NNN identifier; Message is a human-readable
// explanation of the specific condition.
type RecallWarning struct {
	// Code is the stable machine-readable warning code, e.g. "MAZ-RECALL-001".
	Code string
	// Message is a human-readable description of the condition.
	Message string
}

// RecallMeta carries optional per-lane telemetry surfaced when
// RecallRequest.Explain is true. All counts are pre-fusion candidate counts
// before the limit cut.
type RecallMeta struct {
	// TookMs is the total wall-clock time for the recall in milliseconds.
	TookMs int64
	// VecCandidates is the candidate count from the dense-vector lane.
	VecCandidates int
	// FTSCandidates is the candidate count from the FTS lane.
	FTSCandidates int
	// GraphCandidates is the candidate count from the graph traversal lane.
	GraphCandidates int
	// NameEmbCandidates is the candidate count from the name-embedding lane.
	NameEmbCandidates int
}

// ReportingQuery is the optional richer recall surface: it returns the full hit
// slice plus a RecallReport in a single call (no iterator). HybridQuery
// implements it; the MCP recall tool type-asserts for it to advertise the
// degraded + lanes markers. Kept separate from Query so the streaming iterator
// contract and existing Query fakes stay untouched. Concurrency-safe: the
// report is a return value, never shared mutable state on the query.
type ReportingQuery interface {
	RecallWithReport(ctx context.Context, req RecallRequest) ([]Hit, RecallReport, error)
}

// NoopQuery is the placeholder that returns empty iterators. Replaced in
// M1 by the real composer.
type NoopQuery struct{}

// Recall implements Query. The iterator yields ErrNotImplemented once and
// stops, so callers cannot mistake an unwired composer for a real empty
// result set.
func (NoopQuery) Recall(_ context.Context, _ RecallRequest) iter.Seq2[Hit, error] {
	return func(yield func(Hit, error) bool) {
		yield(Hit{}, ErrNotImplemented)
	}
}

// Traverse implements Query. The iterator yields ErrNotImplemented once and
// stops, so callers cannot mistake an unwired composer for a real traversal
// dead-end.
func (NoopQuery) Traverse(_ context.Context, _ TraverseRequest) iter.Seq2[Node, error] {
	return func(yield func(Node, error) bool) {
		yield(Node{}, ErrNotImplemented)
	}
}
