package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

// recallDefaultLimit is the cap applied when the caller omits `limit`. The
// kernel composer carries its own default too (currently 10); the tool
// fills its value in so the recorded RecallRequest reflects the contract
// the agent saw rather than an implicit composer default that could shift
// later. 10 matches the issue spec.
const recallDefaultLimit = 10

// compactSnippetRunes caps the snippet emitted in compact mode. It is derived
// from kernel.SnippetRunesDefault rather than hard-coding a copy, so the
// excerpt an agent sees in compact mode is exactly the length it already gets
// verbose and the two can never silently drift. Compact's token savings come
// from dropping the surrounding heavy fields (full text, per-lane score
// components, confidence, citations, createTime), not from a shorter snippet.
const compactSnippetRunes = kernel.SnippetRunesDefault

// bytesPerToken is the coarse tokens≈bytes/4 heuristic used for budget-aware
// trimming. Mirrors contextbrief.bytesPerToken; it over-counts ASCII prose so
// the budget is a conservative ceiling.
const bytesPerToken = 4

// recallInputSchema is the JSON Schema for tools/call arguments. The
// handler relies on the adapter's precompiled validator to reject
// out-of-range limits and empty queries, so Invoke can trust its decoded
// input shape. AIP-compliant naming (camelCase, no `*_at` suffixes).
const recallInputSchema = `{
  "type": "object",
  "properties": {
    "query":         {"type": "string", "minLength": 1, "description": "Natural-language question or keyword phrase to search the event log for."},
    "kinds":         {"type": "array", "items": {"type": "string"}, "description": "Restrict hits to these record kinds (set semantics). Omit for the default mix of episodes + vault documents. Pass [\"documentRevision\"] for document-only recall, [\"episode\"] to exclude documents."},
    "limit":         {"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum number of hits to return. Defaults to 10."},
    "minConfidence": {"type": "number",  "minimum": 0, "maximum": 1, "description": "Filter out hits whose writer confidence is below this value. Defaults to 0."},
    "at":            {"type": "string", "format": "date-time", "description": "RFC 3339 UTC instant to scope the query. Bi-temporal valid-time anchor. Optional."},
    "asOf":          {"type": "string", "format": "date-time", "description": "RFC 3339 UTC instant. Hides any event whose transaction time is later than this anchor, answering 'what did we know on X?'. Bi-temporal transaction-time anchor. Optional."},
    "asOfEventUID":  {"type": "string", "description": "Event ULID. Resolves to that event's createTime and uses it as the asOf anchor. Mutually exclusive with 'asOf' and 'at'; supply at most one of the three."},
    "currentDate":   {"type": "string", "description": "Caller's \"now\" anchor (RFC 3339 UTC or YYYY-MM-DD) the rewriter uses to resolve relative phrases like \"two months ago\" into an absolute timestamp. Folded into 'at' only when 'at' is unset. Requires rewriteQuery=true to take effect."},
    "noGraph":       {"type": "boolean", "description": "Skip the graph traversal lane when true (vector + FTS only). Off by default."},
    "rewriteQuery":  {"type": "boolean", "description": "Expand the query via the query-rewrite-v1 LLM prompt before retrieval. Off by default."},
    "compact":       {"type": "boolean", "description": "Return a reduced per-hit envelope — {name, kind, snippet, fusedScore, entities} plus source for document hits — dropping the full text, the per-lane score components, confidence, citations, and createTime. Use when you only need anchors to seed-then-traverse (feed an entity name into get/neighbors/relations); cuts token cost sharply. Off by default (verbose)."},
    "maxTokens":     {"type": "integer", "minimum": 1, "description": "Soft cap on the total estimated tokens across all returned hits (estimated at ~4 bytes/token). Hits are dropped lowest-fusedScore-first until the payload fits; the single top-ranked hit is always returned even when it alone exceeds the cap. Combine with compact for the smallest payload. Omit for no cap."},
    "anchor":        {"type": "string", "description": "Entity ULID. When set, restricts candidates to episodes that cite entities within anchorDepth hops of this anchor (default depth 1, max 6). Useful when you already hold an entity anchor and want topically-adjacent recalls without widening to all memory. Requires navigate to be wired; gracefully degrades to full recall when it is not."},
    "anchorDepth":   {"type": "integer", "minimum": 1, "maximum": 6, "description": "Hop radius around the anchor entity (1..6). Defaults to 1 (immediate neighbourhood). Ignored when anchor is absent."},
    "embedDeadlineMs": {"type": "integer", "minimum": 1, "description": "Per-call deadline in milliseconds for the query-embedding step. When the embedder does not respond within this window the call degrades to FTS-only and surfaces MAZ-RECALL-001. Absent means no per-call deadline (the session context deadline still applies)."},
    "explain":       {"type": "boolean", "description": "When true, attach per-lane candidate counts and total wall-clock time under the optional '_meta' key in the response. Off by default."},
    "near": {
      "type": "object",
      "description": "Restrict results to episodes citing entities within a geographic radius of a named place. Both fields required when present.",
      "properties": {
        "placeQuery": {"type": "string", "minLength": 1, "description": "Name of the place to resolve, e.g. Berlin. FTS + name-vector lookup."},
        "radiusKm":   {"type": "number", "exclusiveMinimum": 0, "description": "Search radius in kilometres. Must be greater than 0."}
      },
      "required": ["placeQuery", "radiusKm"],
      "additionalProperties": false
    },
    "scopes": {"type": "array", "items": {"type": "string"}, "description": "Scopes to read from: project, personal, team. Defaults to project+personal. Unknown values are rejected."},
    "source": {"type": "array", "items": {"type": "string", "minLength": 1}, "description": "Constrain hits to these origin sources (set semantics, OR across values). Matches a hit's eventSource — the originating event's source tag, e.g. \"remember\", \"extractor/v1\", or a connector tag like \"connector:granola\". A bare connector name (\"granola\") matches its \"connector:granola\" / \"connector/granola\" forms too. Filtered AFTER ranking, so survivors keep their unfiltered rank order. An unknown source yields an empty hits array (not an error)."},
    "hasPrimarySource": {"type": "boolean", "description": "Keep only hits grounded in a primary source: the hit (or one of its entity anchors) is the subject of a recorded source_ref or of an active has_primary_source edge (#207/#208). Filtered AFTER ranking. Off by default."}
  },
  "required": ["query"],
  "additionalProperties": false
}`

// recallOutputSchema documents the response shape for MCP clients that
// honour declared output schemas. The shape mirrors kernel.Hit one-to-one
// so callers can debug ranking without re-reading the kernel package.
//
// Required is the subset guaranteed in BOTH modes: compact mode (compact=true)
// drops the verbose-only fields (hitType, text, confidence, components,
// citations, createTime), so only {name, kind, snippet, fusedScore, entities}
// — plus source for document hits — are promised across modes.
const recallOutputSchema = `{
  "type": "object",
  "properties": {
    "hits": {
      "type": "array",
      "description": "Ranked hits. In compact mode (compact=true) each hit carries only {name, kind, snippet, fusedScore, entities} plus source for document hits; the verbose-only fields (hitType, text, confidence, components, citations, createTime) are omitted, so only that subset is guaranteed present.",
      "items": {
        "type": "object",
        "properties": {
          "hitType":    {"type": "string", "enum": ["episode","entity","insight","consolidated"], "description": "Resource-class discriminator: 'episode' for a stored event row (the common case, name 'episodes/{uid}'); 'entity' for a record at 'entities/{uid}' — either a directly-created bare entity (text is the entity display name) or a vault document (a document IS an entity; then kind='documentRevision' and source locates the section); 'insight' for a synthesized reflection ('insights/{uid}', kind='insightSynthesized') and 'consolidated' for a compressed memory ('consolidatedMemories/{uid}', kind='memoryConsolidated') — both cognitive-lifecycle derived memories whose citations list the source UIDs they were synthesized from. Pass an entities/{uid} name to readDocument/neighbors/relations to pivot. Omitted in compact mode (derive it from the name prefix)."},
          "name":       {"type": "string", "pattern": "^(episodes|entities|insights|consolidatedMemories)/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name: 'episodes/{uid}' for episode hits, 'entities/{uid}' for entity and document hits, 'insights/{uid}' for synthesized insights, 'consolidatedMemories/{uid}' for consolidated memories."},
          "kind":       {"type": "string", "description": "Sub-discriminator: the event kind for episode hits ('remember', 'observation', …), 'documentRevision' for a vault-document hit (then the source field locates the cited section), 'insightSynthesized' for an insight hit, or 'memoryConsolidated' for a consolidated-memory hit."},
          "eventSource": {"type": "string", "description": "Provenance: the originating event's source tag, e.g. 'remember', 'extractor/v1', or a connector tag like 'connector:granola'. Omitted when the underlying record carries no source. This is the field the recall 'source' filter matches against. Present in both verbose and compact modes."},
          "author":     {"type": "string", "description": "Display name of the principal who authored the source event (resolved via the episodes→principals join). Present only in verbose (compact=false) mode when an author is recorded; omitted for anonymous events or when the daemon's author-lookup is not wired."},
          "text":       {"type": "string", "description": "Full clean text: episode body (hitType=episode), entity display name (bare entity), or the winning document chunk / capped body (kind=documentRevision). Agent-facing: ground reasoning on this, not on snippet. Omitted in compact mode (use snippet)."},
          "snippet":    {"type": "string", "description": "Display-oriented excerpt. Verbose: FTS-driven hits carry <b>...</b> highlight markup; vec-only and entity hits return the text verbatim or truncated. Compact: a plain, markup-free excerpt of the full text capped at 200 runes."},

          "confidence": {"type": "number", "description": "Writer confidence in [0,1]. Omitted in compact mode."},
          "fusedScore": {"type": "number", "description": "Blended relevance across the component lanes below. Under the default 'weighted' fusion it is normalized to [0,1] (a strong multi-lane match approaches 1.0); under 'rrf' fusion it is a small reciprocal-rank value (a two-lane top hit is ~2/(60+1) which is about 0.03). Either way, compare hits by rank ORDER, not absolute magnitude across queries."},
          "components": {
            "type": "object",
            "description": "Per-lane score breakdown for ablation/debugging. Omitted in compact mode.",
            "properties": {
              "vector":  {"type": "number", "description": "Per-lane rank signal for the vec lane (1/(rrfK + rank)). Drives the score only under 'rrf' fusion; under the default 'weighted' fusion rawSemantic (raw cosine) drives the blend and this is recorded for ablation."},
              "fts":     {"type": "number", "description": "Per-lane rank signal for the FTS lane. Drives the score only under 'rrf'; under 'weighted', rawBM25 (sigmoid-normalized BM25) drives the blend."},
              "graph":   {"type": "number", "description": "Rank signal for the entity-graph traversal lane (1-hop / 2-hop via RELATES_TO). Under 'weighted' fusion it (combined with nameEmb) feeds the entity-boost term; non-zero means this hit's episode is graph-connected to the query's entities."},
              "nameEmb": {"type": "number", "description": "Rank signal for the entity-name-vector lane (entities_vec, vector_type='name'), combined with graph into the entity-boost term under 'weighted'. Non-zero only when a GraphConn with entities_vec support is wired and the query embeds an entity-name match."},
              "recency": {"type": "number"},
              "rawSemantic": {"type": "number", "description": "Raw cosine similarity in [0, 1]. Drives the pre-fusion gate."},
              "rawBM25": {"type": "number", "description": "Sigmoid-normalized BM25 in [0, 1] (mem0 adaptive sigmoid)."},
              "strength": {"type": "number", "description": "Passive-reinforcement component in [0, 1] (issue #231): reinforcement(accessCount)*decay(now-lastAccessed). Non-zero only once this memory has been recalled before (a memoryAccessed event folded into its memory_strength) and the strength boost is on. Shows how recall frequency moved this hit's rank."}
            },
            "required": ["vector","fts","graph","nameEmb","recency","rawSemantic","rawBM25","strength"],
            "additionalProperties": false
          },
          "citations": {"type": "array", "items": {"type": "string"}, "description": "Source event-log references this hit derives from. Omitted in compact mode."},
          "source": {
            "type": "object",
            "description": "Document hits only: where the cited text lives in the vault — path, nearest preceding markdown heading, and the 1-based line the chunk starts on (cite as path#heading).",
            "properties": {
              "path":      {"type": "string", "description": "Vault-relative document path, e.g. notes/project.md."},
              "heading":   {"type": "string", "description": "Nearest markdown heading preceding the cited chunk; empty before the first heading."},
              "lineStart": {"type": "integer", "description": "1-based line number the cited chunk starts on."}
            },
            "required": ["path","lineStart"],
            "additionalProperties": false
          },
          "trust": {
            "type": "object",
            "description": "Present on an episode-origin hit whose source is an untrusted external connector (e.g. a Granola transcript or a Claude-chats export). Its ABSENCE means a trusted (asserted) origin — the normal case. (Entity-node hits aggregate every source that mentions the entity, so they carry no single trust posture and are not labeled; inspect the entity's relations, which ARE individually labeled, to see which edges are proposed.) When present, treat the hit's content as unverified third-party input: facts extracted from it are recorded status=proposed (they rank on relevance but are excluded from supersession and were never minted as new predicates/kinds). Verify before relying on it; promote a specific edge with promoteRelation(relations/{uid}) to assert it. Present in both verbose and compact modes — the trust posture survives compaction.",
            "properties": {
              "state":  {"type": "string", "enum": ["proposed"], "description": "Trust posture of facts derived from this hit. 'proposed' = untrusted external origin, not auto-trusted."},
              "source": {"type": "string", "description": "The originating connector source tag, e.g. connector:granola or connector:claude-chats."},
              "note":   {"type": "string", "description": "Human-readable explanation of the posture and how to promote."}
            },
            "required": ["state","source","note"],
            "additionalProperties": false
          },
          "freshness": {
            "type": "object",
            "description": "Present on a hit grounded in an external source captured via captureExternal (or a reference pointer). maz stores only a pointer + your summary + a fetch timestamp, never the source body, so YOU own the live connection: use this verdict to decide whether to reuse the cached summary or re-fetch the source live. Its ABSENCE means an ordinary memory with no external grounding. Survives compaction (the stale-vs-fresh decision must not be lost).",
            "properties": {
              "status":      {"type": "string", "enum": ["fresh","stale","access-unverified","unknown"], "description": "fresh = within the connector's freshness budget, reuse the summary; stale = past it, re-fetch via externalUrl and re-capture; access-unverified = the snapshot was tombstoned (404/403), re-verify reachability; unknown = no usable fetch-time signal."},
              "fetchTime":   {"type": "string", "format": "date-time", "description": "When the snapshot was last captured (RFC 3339 UTC). Omitted when unknown."},
              "connector":   {"type": "string", "description": "Source connector whose TTL produced the verdict, e.g. notion, linear, granola. Omitted when none was recorded."},
              "externalUrl": {"type": "string", "description": "Dereferenceable location to re-fetch when stale. Omitted when absent."},
              "note":        {"type": "string", "description": "One sentence of decision guidance keyed to status."}
            },
            "required": ["status"],
            "additionalProperties": false
          },
          "entities": {
            "type": "array",
            "description": "Graph anchors this hit is about. For episode hits: the resolved entities the episode mentions. For entity hits: the entity itself. Pivot point for seed-then-traverse: feed any name straight into get/neighbors/relations. Empty when the episode mentions no resolved entity.",
            "items": {
              "type": "object",
              "properties": {
                "name":        {"type": "string", "pattern": "^entities/[^/]+$", "description": "AIP resource name of the anchor Entity: \"entities/{uid}\". Pass directly to get/neighbors/relations."},
                "kind":        {"type": "string", "description": "Entity kind, e.g. person, place, organization."},
                "displayName": {"type": "string", "description": "Human-readable entity name."}
              },
              "required": ["name","kind","displayName"],
              "additionalProperties": false
            }
          },
          "createTime": {"type": "string", "format": "date-time", "description": "RFC 3339 nanosecond UTC create instant. Omitted in compact mode."}
        },
        "required": ["name","kind","snippet","fusedScore","entities"],
        "additionalProperties": false
      }
    },
    "degraded": {"type": "boolean", "description": "True when recall ran BELOW full hybrid because the embedder was unavailable: FTS(+graph)-only, no dense-vector lane. Semantic matching did not run, so treat low, flat scores cautiously and lean on the entities/get tools to confirm. Restart the embedder (e.g. Ollama) to restore full hybrid."},
    "lanes": {"type": "array", "items": {"type": "string", "enum": ["vector","fts","graph","nameEmb"]}, "description": "The retrieval lanes that actually contributed to this result: [\"vector\",\"fts\",\"graph\",\"nameEmb\"] for full hybrid, or a subset like [\"fts\"] when degraded. Always present."}
  },
  "required": ["hits","degraded","lanes"],
  "additionalProperties": false
}`

// BackgroundWaiter is the subset of kernel.FileEventLog that Recall uses to
// observe a quiescent derived store before searching. Extraction +
// resolution run off the Append hot path, so a caller that issues a burst
// of remembers followed by an immediate recall would otherwise hit empty
// entity / mention / relation tables (Greptile: LongMemEval feeder bug).
// A nil waiter means the tool runs without the wait gate, which is the
// right behaviour for tests that inject a fake kernel.Query and the
// disabled variant.
type BackgroundWaiter interface {
	WaitForBackgroundWork(ctx context.Context) error
}

// EventLookup is the narrow subset of kernel.FileEventLog that Recall uses
// to resolve an asOfEventUID (event ULID) to its createTime timestamp for
// the point-in-time recall path (Feature 10). A nil lookup means the
// asOfEventUID field is a validation error when present.
type EventLookup interface {
	At(ctx context.Context, uid string) (kernel.Event, error)
}

// Recall is the MCP tool that runs the kernel's hybrid retrieval composer
// and returns ranked Hits with citations. It satisfies mcp.Tool; the
// adapter validates arguments against InputSchema before Invoke runs, so
// the handler trusts the decoded shape.
type Recall struct {
	q        kernel.Query
	waiter   BackgroundWaiter
	lookup   EventLookup
	disabled bool
	// freshness is the resolved per-connector freshness policy used to classify a
	// hit's external snapshot (fresh|stale|access-unverified|unknown). The zero
	// value classifies every connector at the default 24h TTL, so an un-wired tool
	// still produces a sane verdict; the daemon injects the user's policy via
	// WithFreshnessPolicy.
	freshness config.FreshnessPolicy
	// now is the clock used for freshness age math, overridable in tests. Nil means
	// time.Now.
	now func() time.Time
	// authorLookup resolves citation event UIDs to their author's display name for
	// the per-hit `author` field in verbose mode. Nil means author is omitted (the
	// daemon wires the real implementation via WithAuthorLookup).
	authorLookup AuthorLookup
}

// recallDisabledMessage is the exact wire string the disabled-recall tool
// returns. Pinned as a constant so the daemon, the doctor check, and the
// recall test all reference the same source of truth; changing this is a
// user-visible contract break (matched against in agent prompts).
//
// The recovery command names the actual flag (`--embed`, not `--provider`):
// `--provider` selects the chat provider and would not re-enable recall.
const recallDisabledMessage = "embeddings disabled in config.toml; recall needs an embedder — enable it with `maz init --embed=ollama`, or browse without semantic search via `maz list episodes` or `maz query`"

// NewRecall constructs a Recall tool bound to q. Q is typically
// *kernel.HybridQuery in production; tests pass a fake implementation of
// the kernel.Query interface to exercise the projection layer without a
// real SQLite store.
func NewRecall(q kernel.Query) *Recall {
	return &Recall{q: q}
}

// WithBackgroundWaiter installs a BackgroundWaiter that Recall consults
// before searching. Returns r so the daemon can chain it onto NewRecall
// at wiring time. A nil waiter is a silent no-op.
func (r *Recall) WithBackgroundWaiter(w BackgroundWaiter) *Recall {
	if w != nil {
		r.waiter = w
	}
	return r
}

// WithEventLookup installs an EventLookup used to resolve an asOfEventUID
// argument to its createTime for the point-in-time recall path (Feature 10).
// Returns r for chaining. A nil lookup means asOfEventUID is rejected as a
// validation error at call time.
func (r *Recall) WithEventLookup(l EventLookup) *Recall {
	if l != nil {
		r.lookup = l
	}
	return r
}

// WithFreshnessPolicy installs the resolved per-connector freshness policy used
// to classify each hit's external snapshot on the wire. Returns r so the daemon
// can chain it onto NewRecall at wiring time. The zero policy (no call) already
// classifies at the default TTL, so this only swaps in the user's overrides.
func (r *Recall) WithFreshnessPolicy(p config.FreshnessPolicy) *Recall {
	r.freshness = p
	return r
}

// WithAuthorLookup installs the author-lookup service used to resolve each
// verbose hit's source event UID (Citations[0]) to the author's display name.
// Returns r for chaining. Nil means the `author` field is omitted from every
// hit (silent no-op, not an error). The daemon wires the real SQLiteAuthorLookup
// via WithAuthorLookup; tests that inject a fake kernel.Query leave this unset.
func (r *Recall) WithAuthorLookup(l AuthorLookup) *Recall {
	if l != nil {
		r.authorLookup = l
	}
	return r
}

// freshnessClassifier captures the policy and a single "now" so every hit in one
// recall is classified against the same clock. Returns a per-hit classifier that
// yields nil for an ordinary memory (no external grounding) and a freshnessJSON
// for a captureExternal/reference-grounded hit.
func (r *Recall) freshnessClassifier() func(kernel.Hit) *freshnessJSON {
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	policy := r.freshness
	return func(h kernel.Hit) *freshnessJSON {
		return classifyHitFreshness(policy, now, h)
	}
}

// classifyHitFreshness builds the inline freshness verdict for one hit. It
// returns nil unless the hit is grounded in an external source (a non-zero fetch
// time, a connector tag, or a tombstone), so ordinary memories carry no
// freshness block. A tombstoned SourceRef overrides the TTL verdict with
// access-unverified: the agent must re-verify reachability, not just age.
func classifyHitFreshness(p config.FreshnessPolicy, now time.Time, h kernel.Hit) *freshnessJSON {
	if h.FetchTime.IsZero() && h.Tombstone == "" && h.SourceConnector == "" {
		return nil
	}
	status := p.Classify(h.SourceConnector, h.FetchTime, now)
	if h.Tombstone != "" {
		status = config.FreshnessAccessUnverified
	}
	fj := &freshnessJSON{
		Status:      status,
		Connector:   h.SourceConnector,
		ExternalURL: h.ExternalURL,
		Note:        freshnessNote(status, h.Tombstone),
	}
	if !h.FetchTime.IsZero() {
		fj.FetchTime = h.FetchTime.UTC().Format(time.RFC3339Nano)
	}
	return fj
}

// freshnessNote returns one sentence of decision guidance keyed to the status
// (and, for a tombstone, which kind), so the agent knows what to do without a
// second lookup. Unknown gets no note (the status alone is the signal).
func freshnessNote(status, tombstone string) string {
	switch status {
	case config.FreshnessFresh:
		return "Cached snapshot is within its freshness budget; the summary is current, reuse it without re-fetching."
	case config.FreshnessStale:
		return "Cached snapshot is past its freshness budget; re-fetch the source live (see externalUrl) and re-capture before relying on it."
	case config.FreshnessAccessUnverified:
		if tombstone == ontology.TombstoneAccessRevoked {
			return "Source access was revoked (403) on last capture; the summary is the last known state, re-verify access before relying on it."
		}
		return "Source was reported deleted (404) on last capture; the summary is the last known state and may no longer exist."
	default:
		return ""
	}
}

// NewRecallDisabled constructs a Recall tool that always returns the
// "embeddings disabled" validation error. The daemon registers this
// variant when defaults.embed=none so the wire surface still advertises
// the recall tool name (agents can discover it during tools/list) but
// any call surfaces a clear, actionable E_VALIDATION instead of failing
// on a missing Ollama daemon or a nil kernel.Query.
func NewRecallDisabled() *Recall {
	return &Recall{disabled: true}
}

// Name implements mcp.Tool. The wire identifier agents call.
func (*Recall) Name() string { return "recall" }

// Description implements mcp.Tool. Surfaced in tools/list for agent
// discovery; one short sentence per the description-style guidance in
// mcp.types.Tool's godoc.
func (*Recall) Description() string {
	return "Search long-term memory for facts matching a query — about the user OR any person, place, organization, or event in the graph. CALL THIS before answering anything that depends on stored context (names, roles, projects, history, people, places, decisions); Mazarbul persists across sessions and clients, your conversation context does not. It is a SEED-FINDER: ground answers on each hit's `text` and `entities[]` — feed an entity name into get/neighbors/relations for the authoritative structured facts — not on the raw rank. `fusedScore` is a blended relevance score where rank ORDER is the signal, not absolute magnitude (normalized to [0,1] under the default weighted fusion, or a small reciprocal-rank value under rrf), so read each hit's `text` rather than blindly taking hit[0]. A miss is cheap; a confident hallucination is not. Recall ALWAYS returns its best-ranked candidates even when nothing strongly matches, so low, flat scores across unrelated hits usually mean the subject is NOT in memory — confirm a name with `entities`/`get` before treating a hit as fact, and use `relations`/`neighbors` (not recall) for the AUTHORITATIVE list of what mentions or links a known entity. Pass `compact: true`, `maxTokens`, or both to shrink the returned payload when you only need seeds to traverse. Returns ranked hits with citable ULIDs."
}

// InputSchema implements mcp.Tool. Returned to clients during tools/list
// and compiled once by the server registry for per-call validation.
func (*Recall) InputSchema() json.RawMessage { return json.RawMessage(recallInputSchema) }

// OutputSchema implements mcp.Tool. Optional, but supplied so clients
// that honour declared schemas can self-validate responses.
func (*Recall) OutputSchema() json.RawMessage { return json.RawMessage(recallOutputSchema) }

// recallNearInput is the decoded near filter within recallInput. Mirrors
// kernel.NearFilter field-for-field.
type recallNearInput struct {
	PlaceQuery string  `json:"placeQuery"`
	RadiusKm   float64 `json:"radiusKm"`
}

// recallInput is the decoded tools/call arguments shape. Pointers
// distinguish "absent" from the zero value so Invoke can apply defaults
// without overwriting an intentional zero (e.g. minConfidence: 0).
type recallInput struct {
	Query         string   `json:"query"`
	Kinds         []string `json:"kinds,omitempty"`
	Scopes        []string `json:"scopes,omitempty"`
	Limit         *int     `json:"limit,omitempty"`
	MinConfidence *float64 `json:"minConfidence,omitempty"`
	At            *string  `json:"at,omitempty"`
	AsOf          *string  `json:"asOf,omitempty"`
	// AsOfEventUID, when set, resolves the named event ULID to its createTime
	// and uses that as the asOf anchor. Mutually exclusive with At and AsOf.
	AsOfEventUID *string `json:"asOfEventUID,omitempty"`
	CurrentDate  *string `json:"currentDate,omitempty"`
	NoGraph      *bool   `json:"noGraph,omitempty"`
	RewriteQuery *bool   `json:"rewriteQuery,omitempty"`
	// Compact, when true, returns the reduced compactHitJSON envelope per hit
	// instead of the full hitJSON. Orthogonal to MaxTokens.
	Compact *bool `json:"compact,omitempty"`
	// MaxTokens caps the total estimated tokens across returned hits; the
	// lowest-fusedScore hits are dropped first. Absent means no cap (the input
	// schema rejects values <1, so the decoder never sees 0 from the wire).
	MaxTokens       *int             `json:"maxTokens,omitempty"`
	Near            *recallNearInput `json:"near,omitempty"`
	Anchor          *string          `json:"anchor,omitempty"`
	AnchorDepth     *int             `json:"anchorDepth,omitempty"`
	EmbedDeadlineMs *int             `json:"embedDeadlineMs,omitempty"`
	Explain         *bool            `json:"explain,omitempty"`
	// Source constrains hits to these origin sources (matched against each
	// hit's EventSource). Set semantics, OR across values. Filtered after
	// ranking. Empty means no source restriction.
	Source []string `json:"source,omitempty"`
	// HasPrimarySource, when true, keeps only hits grounded in a primary source
	// (a source_ref or an active has_primary_source edge on the hit or one of
	// its entity anchors). Filtered after ranking.
	HasPrimarySource *bool `json:"hasPrimarySource,omitempty"`
}

// scoreComponentsJSON is the projection of kernel.ScoreComponents. The
// per-source field names match the kernel struct one-to-one so a caller
// debugging ranking can correlate without a mapping table.
type scoreComponentsJSON struct {
	Vector float32 `json:"vector"`
	FTS    float32 `json:"fts"`
	Graph  float32 `json:"graph"`
	// NameEmb is the RRF-rank score from the entity-name-vector lane
	// (entities_vec, vector_type='name'). The kernel fuses this lane
	// alongside Vector / FTS / Graph; omitting it from the wire shape
	// hides a component that affected ranking, so agents reading the
	// breakdown to debug a hit cannot reconcile FusedScore.
	NameEmb float32 `json:"nameEmb"`
	Recency float32 `json:"recency"`
	// RawSemantic is the raw cosine similarity (or its proxy for the
	// vector lane) — the value that feeds WeightedComponents and the
	// pre-fusion semantic gate. Agents tuning the gate or debugging
	// "why didn't this rank?" need this; the RRF-rank Vector column is
	// not the same number. Range [0, 1].
	RawSemantic float32 `json:"rawSemantic"`
	// RawBM25 is the sigmoid-normalized BM25 score (mem0's adaptive
	// query-length sigmoid). Range [0, 1]. Distinct from FTS, which
	// is the RRF-rank component.
	RawBM25 float32 `json:"rawBM25"`
	// Strength is the passive-reinforcement component (issue #231):
	// reinforcement(accessCount)·decay(now − lastAccessed), range [0, 1].
	// Non-zero only when this memory has been recalled before (so a
	// memoryAccessed event folded into its memory_strength) and the
	// strength boost is enabled. Surfacing it lets an agent see how
	// recall frequency moved the rank — the cognitive-lifecycle signal.
	Strength float32 `json:"strength"`
}

// hitJSON is the projection of kernel.Hit into the MCP wire shape. The
// `name` field carries the AIP resource name: "episodes/{uid}" for episode
// hits, "entities/{uid}" for entity and document hits (a document IS an
// entity, so its name pivots straight into readDocument/neighbors/relations),
// "insights/{uid}" for synthesized insights, and "consolidatedMemories/{uid}"
// for consolidated memories (issue #231 cognitive-lifecycle derived memories).
// Times are RFC 3339 nanosecond UTC per AGENTS.md rule 7.
type hitJSON struct {
	// HitType is the resource-class discriminator: "episode" (name
	// episodes/{uid}) or "entity" (name entities/{uid}) — the latter covers
	// both bare entities and documents.
	HitType string `json:"hitType"`
	Name    string `json:"name"`
	// Kind is the sub-discriminator: the event kind for episode hits, or
	// "documentRevision" for a vault-document hit.
	Kind string `json:"kind"`
	// EventSource is the originating event's source tag (the episodes.source
	// column), e.g. "remember", "extractor/v1", or "connector:granola". It is
	// the field the recall `source` filter matches against, surfaced so a
	// caller can see WHY a hit survived (or read provenance generally). Omitted
	// when empty.
	EventSource string `json:"eventSource,omitempty"`
	// Author is the display name of the principal who authored the source event
	// (resolved from episodes.payload_json → $.author.principalUid → principals
	// table). Present only in verbose (non-compact) mode when an author is
	// recorded; omitted for anonymous events or when the author lookup is not
	// wired. Agents can use this for provenance ("who stored this memory?").
	Author string `json:"author,omitempty"`
	// Text is the full clean episode text, entity display name, or document
	// winning chunk / capped body. Agent-facing: ground reasoning on this
	// field without HTML stripping or snippet-window inflation; Snippet
	// remains for human UIs.
	Text       string              `json:"text"`
	Snippet    string              `json:"snippet"`
	Confidence float64             `json:"confidence"`
	FusedScore float32             `json:"fusedScore"`
	Components scoreComponentsJSON `json:"components"`
	Citations  []string            `json:"citations"`
	// Entities are the graph anchors this hit is about — the seed-then-
	// traverse pivot. Always an array (never null) so agents can iterate
	// unconditionally; empty when the episode mentions no resolved entity.
	Entities   []entityRefJSON `json:"entities"`
	CreateTime string          `json:"createTime"`
	// Source anchors a document hit to its place in the vault: path +
	// nearest preceding heading + 1-based start line of the cited chunk.
	// Omitted for episode hits.
	Source *sourceLocationJSON `json:"source,omitempty"`
	// Trust labels a hit whose originating event came from an untrusted external
	// connector source. Nil (and omitted) for a trusted asserted origin, so the
	// label only appears where it matters (#211, #212).
	Trust *trustJSON `json:"trust,omitempty"`
	// Freshness is the inline staleness verdict for a hit grounded in an external
	// source captured via captureExternal (or a reference()-style pointer). Nil
	// (omitted) for an ordinary memory with no external provenance, so it appears
	// only where the agent must decide cached-summary-vs-refetch.
	Freshness *freshnessJSON `json:"freshness,omitempty"`
}

// compactHitJSON is the reduced projection of kernel.Hit returned under
// compact mode (recallInput.Compact). It carries only the anchors an agent
// needs to seed-then-traverse — name, kind, snippet, fusedScore, entities,
// and source for document hits — and omits the verbose envelope's full text,
// per-lane score components, confidence, citations, and createTime. Field
// names and JSON tags are kept identical to hitJSON so a caller that toggles
// compact reads the same keys, just fewer of them.
type compactHitJSON struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// EventSource mirrors hitJSON.EventSource: the originating event's source
	// tag (provenance + the recall `source` filter key). Kept in compact mode
	// because it is tiny and is what the source filter selects on. Omitted when
	// empty.
	EventSource string          `json:"eventSource,omitempty"`
	Snippet     string          `json:"snippet"`
	FusedScore  float32         `json:"fusedScore"`
	Entities    []entityRefJSON `json:"entities"`
	// Source is present only for document hits (path#heading citation); it is
	// tiny and the whole value of a document hit, so it survives compaction.
	Source *sourceLocationJSON `json:"source,omitempty"`
	// Trust survives compaction: a security-relevant untrusted-provenance label
	// is exactly what a compact-mode consumer must not lose. Nil (omitted) for a
	// trusted asserted origin.
	Trust *trustJSON `json:"trust,omitempty"`
	// Freshness survives compaction: the stale-vs-fresh verdict is the load-bearing
	// signal for whether the agent can reuse the cached external summary, so a
	// compact-mode consumer must not lose it. Nil (omitted) for a non-external hit.
	Freshness *freshnessJSON `json:"freshness,omitempty"`
}

// sourceLocationJSON is the wire shape of kernel.SourceLocation.
type sourceLocationJSON struct {
	Path      string `json:"path"`
	Heading   string `json:"heading,omitempty"`
	LineStart int    `json:"lineStart"`
}

// trustJSON is the untrusted-provenance label attached to a recall hit whose
// originating event came from an external ingestion connector (#211, #212). It
// is emitted only for such hits; its absence means a trusted (asserted) origin.
type trustJSON struct {
	// State is the trust posture of facts derived from the hit. Always
	// "proposed" when the label is present.
	State string `json:"state"`
	// Source is the originating connector source tag, e.g. "connector:granola".
	Source string `json:"source"`
	// Note explains the posture and points at promoteRelation.
	Note string `json:"note"`
}

// freshnessJSON is the inline staleness verdict attached to a recall hit grounded
// in an external source captured via captureExternal (#C). Maz never holds the
// external item's body or tokens; it stores a pointer + the agent's summary +
// this provenance, so the agent owns the live connection and decides, from this
// block, whether the cached summary is current or must be re-fetched live.
type freshnessJSON struct {
	// Status is the verdict: "fresh" (within the connector's TTL, so reuse the
	// summary), "stale" (past TTL, so re-fetch via externalUrl and re-capture),
	// "access-unverified" (the snapshot was tombstoned 404/403, so re-verify
	// access), or "unknown" (no usable fetch-time signal).
	Status string `json:"status"`
	// FetchTime is when the snapshot was last captured (RFC 3339 UTC). Omitted
	// when unknown (no SourceRef fetch time).
	FetchTime string `json:"fetchTime,omitempty"`
	// Connector is the source connector (notion, linear, …) whose TTL produced
	// the verdict. Omitted when the SourceRef carried none.
	Connector string `json:"connector,omitempty"`
	// ExternalURL is the dereferenceable location to re-fetch when stale. Omitted
	// when absent (never emitted to plaintext-scope render, but recall is a live
	// read, not a render).
	ExternalURL string `json:"externalUrl,omitempty"`
	// Note is one sentence of decision guidance keyed to Status.
	Note string `json:"note,omitempty"`
}

// entityRefJSON projects a kernel.EntityRef into the MCP wire shape. Name is
// the AIP resource name ("entities/{uid}") so an agent can pipe an anchor
// straight into get/neighbors/relations without string surgery.
type entityRefJSON struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
}

// recallMetaJSON is the optional per-lane telemetry block emitted under the
// top-level "_meta" key when RecallRequest.Explain is true. It mirrors
// kernel.RecallMeta field-for-field. The JSON field name uses the "_meta"
// sentinel so agents can distinguish it from hit fields; it is always
// omitted when explain=false to preserve the compact envelope.
type recallMetaJSON struct {
	// TookMs is the total wall-clock time for the recall in milliseconds.
	TookMs int64 `json:"took_ms"`
	// Lanes is the per-lane candidate counts before the limit cut. Each
	// entry names a retrieval lane and its pre-fusion candidate count.
	Lanes []recallMetaLaneJSON `json:"lanes"`
}

// recallMetaLaneJSON is one entry in recallMetaJSON.Lanes: a named lane
// and its pre-fusion candidate count.
type recallMetaLaneJSON struct {
	// Name is the lane identifier: "fts", "vector", "graph", or "nameEmb".
	Name string `json:"name"`
	// Candidates is the number of pre-fusion candidates this lane contributed.
	Candidates int `json:"candidates"`
}

// recallOutput is the verbose top-level response. Always serialises an empty
// array on no results (never null) so agents can rely on `hits` being a
// JSON array.
type recallOutput struct {
	Hits []hitJSON `json:"hits"`
	// Degraded is true when recall ran below full hybrid because the embedder
	// was unavailable (FTS+graph-only, no dense-vector lane). Surfaced so an
	// agent never mistakes a BM25-only result for full semantic recall (#236).
	Degraded bool `json:"degraded"`
	// Lanes lists the retrieval lanes that actually contributed, in canonical
	// order. Always a non-nil array so agents can iterate unconditionally.
	Lanes []string `json:"lanes"`
	// Meta carries optional per-lane telemetry. Populated only when
	// explain=true was set on the request; nil (and omitted) otherwise.
	Meta *recallMetaJSON `json:"_meta,omitempty"`
	// Warnings surfaces stable machine-readable recall warning codes
	// (MAZ-RECALL-001 degraded/embedder-down, MAZ-RECALL-002 fuzzy fallback)
	// so an agent can branch on a code instead of inferring from `degraded`.
	// Omitted when empty.
	Warnings []warncode.Warning `json:"warnings,omitempty"`
}

// compactRecallOutput is the compact-mode top-level response. Like
// recallOutput it always serialises an empty array (never null) for no hits.
type compactRecallOutput struct {
	Hits []compactHitJSON `json:"hits"`
	// Degraded mirrors recallOutput.Degraded: true when recall ran below full
	// hybrid (FTS+graph-only) because the embedder was unavailable (#236).
	Degraded bool `json:"degraded"`
	// Lanes mirrors recallOutput.Lanes: the retrieval lanes that actually
	// contributed, in canonical order. Always a non-nil array.
	Lanes []string `json:"lanes"`
	// Meta carries optional per-lane telemetry. Populated only when
	// explain=true; nil (and omitted) otherwise.
	Meta *recallMetaJSON `json:"_meta,omitempty"`
	// Warnings mirrors recallOutput.Warnings: stable recall warning codes
	// (MAZ-RECALL-001/002). Omitted when empty.
	Warnings []warncode.Warning `json:"warnings,omitempty"`
}

// Invoke implements mcp.Tool. It iterates kernel.Query.Recall, projects
// each Hit into the requested wire shape (verbose or compact), applies
// budget-aware trimming, and returns the response as JSON. An iterator error
// terminates the call with mcp.CodeInternal; empty results are not an error.
func (r *Recall) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	if r.disabled {
		// E_VALIDATION (not E_INTERNAL) because the request shape is
		// well-formed; the *server* is intentionally configured to not
		// serve recall. The message names config.toml and the exact
		// command to re-enable so an agent can route the user to the
		// fix without further round-trips.
		return nil, mcp.NewValidationError(recallDisabledMessage, nil)
	}
	var in recallInput
	if err := json.Unmarshal(req, &in); err != nil {
		// The adapter already validated `req` against the schema, so
		// reaching this branch implies an upstream encoding bug. Surface
		// it as E_VALIDATION so the agent sees an actionable error code
		// rather than a generic internal failure.
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}

	// Drain background extraction + resolution before searching so a
	// feeder loop's burst of remembers is fully reflected in the four
	// derived stores. Cheap to call when no work is in flight. Respects
	// ctx: if the caller's deadline expires while extraction is still
	// running, the wait returns early and the search proceeds against
	// whatever state has landed; a partial-state recall is more useful
	// than a timeout error, and the search itself will also honour ctx.
	if r.waiter != nil {
		if werr := r.waiter.WaitForBackgroundWork(ctx); werr != nil {
			logging.FromContext(ctx).Debug(
				"recall: background drain incomplete, searching partial state",
				zap.Error(werr),
			)
		}
	}

	kreq, err := buildRecallRequest(ctx, in, r.lookup)
	if err != nil {
		return nil, err
	}

	// Source / has-primary-source filtering runs AFTER ranking (below), so
	// over-fetch from the kernel: requesting more than the page limit lets a
	// full page survive the post-filter without distorting BM25/semantic/RRF
	// scoring (which a SQL-side EXISTS would). The page limit is restored after
	// filtering. When no filter is active, the request is left untouched.
	pageLimit := kreq.Limit
	if recallSourceFilterActive(in) {
		kreq.Limit = recallOverFetchLimit(pageLimit)
	}

	hits, report, err := drainRecallWithReport(ctx, r.q, kreq)
	if err != nil {
		return nil, readErr("recall", err)
	}

	if recallSourceFilterActive(in) {
		hits, err = r.applyRecallSourceFilter(ctx, in, hits)
		if err != nil {
			// readErr preserves an already-classified ToolError (the fail-loud
			// E_INTERNAL when the primary-source checker is unwired) and maps a
			// transient lock to E_UNAVAILABLE instead of flattening to E_INTERNAL.
			return nil, readErr("recall", err)
		}
		if len(hits) > pageLimit {
			hits = hits[:pageLimit]
		}
	}

	// Author lookup is skipped in compact mode (author field is verbose-only).
	compact := in.Compact != nil && *in.Compact
	var authors map[string]string
	if !compact {
		authors = r.lookupHitAuthors(ctx, hits)
	}

	body, err := r.marshalRecallResponse(in, hits, report, authors)
	if err != nil {
		// json.Marshal of a fully-typed struct cannot fail in practice;
		// the branch exists so a future schema change that introduces an
		// unencodable field surfaces a clear E_INTERNAL rather than a
		// silent empty response. The helper already contextualises err.
		return nil, mcp.NewInternalError(err)
	}
	return body, nil
}

// collectCitationUIDs returns the deduplicated first-citation UIDs across all
// hits. These are the event UIDs passed to AuthorLookup.BatchEventAuthors for
// the per-hit `author` field in verbose mode.
func collectCitationUIDs(hits []kernel.Hit) []string {
	seen := make(map[string]struct{}, len(hits))
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if len(h.Citations) == 0 {
			continue
		}
		uid := h.Citations[0]
		if _, ok := seen[uid]; !ok {
			seen[uid] = struct{}{}
			out = append(out, uid)
		}
	}
	return out
}

// hitAuthor looks up the author display name for h using the first citation
// UID as the event key. Returns "" when citations are absent or the UID is
// not in the pre-fetched authors map.
func hitAuthor(h kernel.Hit, authors map[string]string) string {
	if len(h.Citations) == 0 || len(authors) == 0 {
		return ""
	}
	return authors[h.Citations[0]]
}

// lookupHitAuthors batch-resolves citation event UIDs to their author display
// names. Returns nil when no lookup is wired, hits carry no citations, or the
// lookup fails (best-effort: errors are logged at debug and author fields are
// silently omitted rather than failing the entire recall). Extracted from
// Invoke to keep that function's cognitive complexity under the gocognit gate.
func (r *Recall) lookupHitAuthors(ctx context.Context, hits []kernel.Hit) map[string]string {
	if r.authorLookup == nil {
		return nil
	}
	uids := collectCitationUIDs(hits)
	if len(uids) == 0 {
		return nil
	}
	authors, aerr := r.authorLookup.BatchEventAuthors(ctx, uids)
	if aerr != nil {
		logging.FromContext(ctx).Debug(
			"recall: author lookup failed, author fields will be empty",
			zap.Error(aerr),
		)
	}
	return authors
}

// toEventKinds converts the wire `kinds` array into kernel.EventKind values,
// returning nil for an absent/empty filter (no restriction) so the kernel's
// "empty Kinds means all kinds" contract holds. Shared by recall and ask.
func toEventKinds(kinds []string) []kernel.EventKind {
	if len(kinds) == 0 {
		return nil
	}
	out := make([]kernel.EventKind, len(kinds))
	for i, k := range kinds {
		out[i] = kernel.EventKind(k)
	}
	return out
}

// buildRecallRequest projects a decoded recallInput into the kernel-side
// RecallRequest, applying defaults and parsing the optional date fields. A
// non-nil error is an E_VALIDATION mcp.Error with a field-specific message;
// callers return it verbatim. Split out of Invoke to keep that function's
// cyclomatic complexity under the linter's gocyclo threshold.
func buildRecallRequest(ctx context.Context, in recallInput, lookup EventLookup) (kernel.RecallRequest, error) {
	limit := recallDefaultLimit
	if in.Limit != nil {
		limit = *in.Limit
	}
	minConfidence := 0.0
	if in.MinConfidence != nil {
		minConfidence = *in.MinConfidence
	}

	kreq := kernel.RecallRequest{
		Query:         in.Query,
		Limit:         limit,
		MinConfidence: minConfidence,
		Kinds:         toEventKinds(in.Kinds),
	}
	if err := applyRecallTemporalFields(ctx, in, &kreq, lookup); err != nil {
		return kernel.RecallRequest{}, err
	}
	if in.NoGraph != nil {
		kreq.NoGraph = *in.NoGraph
	}
	if in.RewriteQuery != nil {
		kreq.RewriteQuery = *in.RewriteQuery
	}
	if in.Near != nil {
		kreq.Near = &kernel.NearFilter{
			PlaceQuery: in.Near.PlaceQuery,
			RadiusKm:   in.Near.RadiusKm,
		}
	}
	if len(in.Scopes) > 0 {
		ss, perr := kernel.ParseScopeSet(in.Scopes)
		if perr != nil {
			return kernel.RecallRequest{}, mcp.NewValidationError(
				fmt.Sprintf("invalid scopes: %v (allowed: project, personal, team)", perr), nil)
		}
		// Draft is staging-only: recall must never surface unreviewed draft data.
		if ss.Contains(kernel.ScopeDraft) {
			return kernel.RecallRequest{}, mcp.NewValidationError(
				`scope "draft" is not recallable; use the draftStage tool to inspect the staging area`, nil)
		}
		kreq.Scopes = in.Scopes
	}
	if in.Anchor != nil {
		kreq.Anchor = *in.Anchor
	}
	if in.AnchorDepth != nil {
		kreq.AnchorDepth = *in.AnchorDepth
	}
	if in.EmbedDeadlineMs != nil && *in.EmbedDeadlineMs > 0 {
		kreq.EmbedDeadline = time.Duration(*in.EmbedDeadlineMs) * time.Millisecond
	}
	if in.Explain != nil {
		kreq.Explain = *in.Explain
	}
	return kreq, nil
}

// applyRecallTemporalFields parses the optional at / asOf / asOfEventUID /
// currentDate strings onto kreq. AsOfEventUID is mutually exclusive with
// at and asOf; supplying more than one is a validation error. Split out of
// buildRecallRequest to keep that function under the gocyclo threshold;
// each parse failure is an E_VALIDATION error returned verbatim to the caller.
func applyRecallTemporalFields(ctx context.Context, in recallInput, kreq *kernel.RecallRequest, lookup EventLookup) error {
	hasAt := in.At != nil && *in.At != ""
	hasAsOf := in.AsOf != nil && *in.AsOf != ""
	hasAsOfUID := in.AsOfEventUID != nil && *in.AsOfEventUID != ""

	// AsOfEventUID is mutually exclusive with both at and asOf.
	if hasAsOfUID && (hasAt || hasAsOf) {
		return mcp.NewValidationError(
			"'asOfEventUID' is mutually exclusive with 'at' and 'asOf'; supply at most one", nil)
	}

	if hasAt {
		t, perr := parseRFC3339Field("at", *in.At)
		if perr != nil {
			return perr
		}
		kreq.At = &t
	}
	if hasAsOf {
		t, perr := parseRFC3339Field("asOf", *in.AsOf)
		if perr != nil {
			return perr
		}
		kreq.AsOf = &t
	}
	if hasAsOfUID {
		if err := applyAsOfEventUID(ctx, kreq, *in.AsOfEventUID, lookup); err != nil {
			return err
		}
	}
	return applyCurrentDateField(in, kreq)
}

// applyCurrentDateField parses the optional currentDate hint onto kreq.
// Extracted from applyRecallTemporalFields to keep cyclomatic complexity
// within the gocyclo budget.
func applyCurrentDateField(in recallInput, kreq *kernel.RecallRequest) error {
	if in.CurrentDate == nil || *in.CurrentDate == "" {
		return nil
	}
	t, perr := parseCurrentDate(*in.CurrentDate)
	if perr != nil {
		return mcp.NewValidationError(fmt.Sprintf("parse 'currentDate': %v", perr), nil)
	}
	kreq.CurrentDate = &t
	return nil
}

// parseRFC3339Field parses a named input field as an RFC3339Nano timestamp and
// returns the value in UTC. On failure it returns an MCP validation error that
// names the offending field.
func parseRFC3339Field(field, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, mcp.NewValidationError(fmt.Sprintf("parse %q: %v", field, err), nil)
	}
	return t.UTC(), nil
}

// applyAsOfEventUID resolves asOfEventUID via lookup and sets kreq.AsOf to
// the matching event's CreateTime plus 1 nanosecond. The +1ns makes the
// boundary inclusive: the kernel's Until filter is exclusive (createTime <
// Until), so without the adjustment the named event itself would be excluded.
// An inclusive as-of means "the state the system was in right after this
// event was appended — including the event itself." Split from
// applyRecallTemporalFields to keep its cyclomatic complexity within budget.
func applyAsOfEventUID(ctx context.Context, kreq *kernel.RecallRequest, uid string, lookup EventLookup) error {
	if lookup == nil {
		return mcp.NewValidationError(
			"'asOfEventUID' is not supported: event lookup not available", nil)
	}
	ev, lerr := lookup.At(ctx, uid)
	if lerr != nil {
		// Classify the error so the caller sees a clean, actionable message
		// without raw driver internals (e.g. "ulid: bad data size").
		if errors.Is(lerr, kernel.ErrInvalidEvent) {
			return mcp.NewValidationError(
				fmt.Sprintf("'asOfEventUID' %q is not a valid event ULID", uid), nil)
		}
		if errors.Is(lerr, kernel.ErrNotFound) {
			return mcp.NewValidationError(
				fmt.Sprintf("'asOfEventUID' %q: event not found in the log", uid), nil)
		}
		return mcp.NewValidationError(
			fmt.Sprintf("'asOfEventUID' %q: lookup failed", uid), nil)
	}
	// Add 1ns to make the upper bound inclusive: the kernel's Until filter
	// uses createTime < Until (exclusive), so +1ns includes the named event.
	t := ev.CreateTime.UTC().Add(time.Nanosecond)
	kreq.AsOf = &t
	return nil
}

// recallSourceOverFetchFactor and recallSourceOverFetchMax bound the over-fetch
// applied when a source / has-primary-source filter is active: the kernel is
// asked for up to factor× the page limit (capped at the schema max) so a full
// page can survive the post-rank filter. The kernel cuts to this Limit from its
// larger internal candidate pool, so over-fetching only widens the final cut, it
// does not re-run retrieval and does not change per-hit scores.
const (
	recallSourceOverFetchFactor = 5
	recallSourceOverFetchMax    = 100
)

// recallOverFetchLimit returns the kernel Limit to request when post-rank
// filtering will trim the result: factor× the page limit, capped at the schema
// maximum, never below the page limit itself.
func recallOverFetchLimit(pageLimit int) int {
	n := pageLimit * recallSourceOverFetchFactor
	if n > recallSourceOverFetchMax {
		n = recallSourceOverFetchMax
	}
	if n < pageLimit {
		n = pageLimit
	}
	return n
}

// recallSourceFilterActive reports whether the request carries a source or
// has-primary-source constraint that must be applied after ranking.
func recallSourceFilterActive(in recallInput) bool {
	return len(in.Source) > 0 || (in.HasPrimarySource != nil && *in.HasPrimarySource)
}

// applyRecallSourceFilter keeps only the hits that satisfy the request's source
// and/or has-primary-source constraints, preserving the kernel's rank order. An
// unknown source simply yields an empty slice (not an error). The
// has-primary-source check consults the kernel's PrimarySourceQuery surface; a
// Query that does not implement it (a fake / degraded path) yields no
// primary-source-bearing hits rather than silently passing everything through.
func (r *Recall) applyRecallSourceFilter(ctx context.Context, in recallInput, hits []kernel.Hit) ([]kernel.Hit, error) {
	if len(in.Source) > 0 {
		hits = filterHitsByEventSource(hits, in.Source)
	}
	if in.HasPrimarySource != nil && *in.HasPrimarySource {
		filtered, err := r.filterHitsByPrimarySource(ctx, in, hits)
		if err != nil {
			return nil, err
		}
		hits = filtered
	}
	return hits, nil
}

// filterHitsByEventSource keeps hits whose EventSource matches any requested
// source value (OR semantics, connector-prefix-insensitive). Rank order is
// preserved because the kernel returned hits already ordered.
func filterHitsByEventSource(hits []kernel.Hit, sources []string) []kernel.Hit {
	kept := make([]kernel.Hit, 0, len(hits))
	for _, h := range hits {
		if eventSourceMatches(h.EventSource, sources) {
			kept = append(kept, h)
		}
	}
	return kept
}

// eventSourceMatches reports whether eventSource matches any filter value. A
// match is exact (case-insensitive) OR equal after stripping the connector
// prefix from both sides, so `--source granola` matches "connector:granola",
// "connector/granola", and a bare "granola" alike.
func eventSourceMatches(eventSource string, filters []string) bool {
	es := strings.ToLower(strings.TrimSpace(eventSource))
	if es == "" {
		return false
	}
	esBare := stripConnectorPrefix(es)
	for _, f := range filters {
		fn := strings.ToLower(strings.TrimSpace(f))
		if fn == "" {
			continue
		}
		if es == fn || esBare == stripConnectorPrefix(fn) {
			return true
		}
	}
	return false
}

// stripConnectorPrefix removes a leading "connector:" or "connector/" tag from s
// so a connector source and its bare name compare equal.
func stripConnectorPrefix(s string) string {
	for _, p := range []string{
		ontology.ConnectorSourceColonPrefix,
		ontology.ConnectorSourceSlashPrefix,
	} {
		if strings.HasPrefix(s, p) {
			return strings.TrimPrefix(s, p)
		}
	}
	return s
}

// filterHitsByPrimarySource keeps only hits grounded in a primary source: the
// hit's own UID or any of its entity anchors is the subject of a source_ref or
// an active has_primary_source edge. The lookup is one batched, scope-isolated
// kernel call over all candidate UIDs.
func (r *Recall) filterHitsByPrimarySource(ctx context.Context, in recallInput, hits []kernel.Hit) ([]kernel.Hit, error) {
	psq, ok := r.q.(kernel.PrimarySourceQuery)
	if !ok {
		// The has-primary-source check needs the kernel's PrimarySourceQuery
		// surface. Production always provides it (the daemon wires NewRecall with
		// the HybridQuery; the only non-implementing case is NewRecallDisabled,
		// which never reaches this filter). A Query that does not implement it is
		// a misconfiguration — fail loud rather than silently returning zero hits,
		// which a caller would misread as "nothing has a primary source".
		return nil, mcp.NewInternalError(errors.New(
			"recall: --has-primary-source requires the full kernel query stack (PrimarySourceQuery unavailable)"))
	}
	if len(hits) == 0 {
		// Nothing to filter: a legitimately empty page, not a capability gap.
		return nil, nil
	}
	uids := make([]string, 0, len(hits)*2)
	for _, h := range hits {
		uids = append(uids, h.UID)
		for _, e := range h.Entities {
			uids = append(uids, e.UID)
		}
	}
	have, err := psq.SubjectsWithPrimarySource(ctx, uids, in.Scopes)
	if err != nil {
		return nil, err
	}
	kept := make([]kernel.Hit, 0, len(hits))
	for _, h := range hits {
		if hitHasPrimarySource(h, have) {
			kept = append(kept, h)
		}
	}
	return kept, nil
}

// hitHasPrimarySource reports whether the hit's own UID or any of its entity
// anchors is in the have set (a subject with a recorded primary source).
func hitHasPrimarySource(h kernel.Hit, have map[string]bool) bool {
	if have[h.UID] {
		return true
	}
	for _, e := range h.Entities {
		if have[e.UID] {
			return true
		}
	}
	return false
}

// drainRecallWithReport drains the query into raw kernel.Hit values plus the
// recall report (degraded flag + active lanes). When the query implements
// kernel.ReportingQuery (HybridQuery does) it uses RecallWithReport so the
// report is available even on zero hits; otherwise it falls back to collectHits
// and an empty report. Raw hits (not projected) are returned so the marshalers
// can apply token-budget trimming on FusedScore before projecting (#247).
func drainRecallWithReport(ctx context.Context, q kernel.Query, req kernel.RecallRequest) ([]kernel.Hit, kernel.RecallReport, error) {
	if rq, ok := q.(kernel.ReportingQuery); ok {
		return rq.RecallWithReport(ctx, req)
	}
	hits, err := collectHits(ctx, q, req)
	if err != nil {
		return nil, kernel.RecallReport{}, err
	}
	return hits, kernel.RecallReport{}, nil
}

// marshalRecallResponse projects the drained hits into the requested wire
// shape (compact when recallInput.Compact is set, else verbose), applies
// budget-aware trimming when maxTokens is set, and marshals the response. The
// recall report's degraded flag + active lanes are attached to the top-level
// envelope so an agent never mistakes a BM25-only result for full hybrid (#236).
// When explain=true the optional _meta block is populated from report.Meta.
// Authors is the pre-fetched citation-UID → display-name map used for the
// per-hit `author` field in verbose mode; nil means author is omitted.
// Split out of Invoke to keep that function's cyclomatic complexity under the
// linter threshold.
func (r *Recall) marshalRecallResponse(in recallInput, hits []kernel.Hit, report kernel.RecallReport, authors map[string]string) ([]byte, error) {
	maxTokens := 0
	if in.MaxTokens != nil {
		maxTokens = *in.MaxTokens
	}
	explain := in.Explain != nil && *in.Explain
	lanes := report.Lanes
	if lanes == nil {
		// Never serialise null: agents rely on `lanes` being an array, and a
		// fallback (non-reporting) query leaves the report zero-valued.
		lanes = []string{}
	}
	meta := buildRecallMeta(report.Meta, explain)
	warnings := recallWarnings(report.Warnings)
	classify := r.freshnessClassifier()
	if in.Compact != nil && *in.Compact {
		return marshalCompactHits(hits, maxTokens, report.Degraded, lanes, meta, warnings, classify)
	}
	return marshalVerboseHits(hits, maxTokens, report.Degraded, lanes, meta, explain, warnings, classify, authors)
}

// recallWarnings converts the kernel's recall warnings into the wire warncode
// shape so stable codes (MAZ-RECALL-001/002) reach the agent. Returns nil for
// the empty case so the `warnings` key is omitted from the response.
func recallWarnings(ws []kernel.RecallWarning) []warncode.Warning {
	if len(ws) == 0 {
		return nil
	}
	out := make([]warncode.Warning, len(ws))
	for i, w := range ws {
		out[i] = warncode.Warning{Code: w.Code, Message: w.Message}
	}
	return out
}

// buildRecallMeta converts a kernel.RecallMeta into the wire recallMetaJSON
// when explain is true and km is non-nil; returns nil otherwise so the _meta
// key is omitted from the response.
func buildRecallMeta(km *kernel.RecallMeta, explain bool) *recallMetaJSON {
	if !explain || km == nil {
		return nil
	}
	laneOrder := []struct {
		name  string
		count int
	}{
		{kernel.LaneFTS, km.FTSCandidates},
		{kernel.LaneVector, km.VecCandidates},
		{kernel.LaneGraph, km.GraphCandidates},
		{kernel.LaneNameEmb, km.NameEmbCandidates},
	}
	lanes := make([]recallMetaLaneJSON, 0, len(laneOrder))
	for _, l := range laneOrder {
		if l.count > 0 {
			lanes = append(lanes, recallMetaLaneJSON{Name: l.name, Candidates: l.count})
		}
	}
	return &recallMetaJSON{TookMs: km.TookMs, Lanes: lanes}
}

// marshalVerboseHits projects to the full hitJSON envelope, trims to the token
// budget, and marshals. The projected slice is always non-nil so the response
// is {"hits":[]} rather than {"hits":null} on no results.
// Authors maps citation-event UID → author display name; nil is a no-op.
func marshalVerboseHits(hits []kernel.Hit, maxTokens int, degraded bool, lanes []string, meta *recallMetaJSON, explain bool, warnings []warncode.Warning, classify func(kernel.Hit) *freshnessJSON, authors map[string]string) ([]byte, error) {
	_ = explain // reserved for future per-hit gating; all fields always emitted now
	projected := make([]hitJSON, 0, len(hits))
	scores := make([]float32, 0, len(hits))
	for _, h := range hits {
		projected = append(projected, projectHit(h, classify(h), hitAuthor(h, authors)))
		scores = append(scores, h.FusedScore)
	}
	projected = trimToTokenBudget(projected, scores, maxTokens)
	body, err := json.Marshal(recallOutput{Hits: projected, Degraded: degraded, Lanes: lanes, Meta: meta, Warnings: warnings})
	if err != nil {
		return nil, fmt.Errorf("marshal recall hits: %w", err)
	}
	return body, nil
}

// marshalCompactHits is marshalVerboseHits' compact-mode sibling: the reduced
// per-hit envelope, the same trimming and non-nil-slice guarantees.
func marshalCompactHits(hits []kernel.Hit, maxTokens int, degraded bool, lanes []string, meta *recallMetaJSON, warnings []warncode.Warning, classify func(kernel.Hit) *freshnessJSON) ([]byte, error) {
	projected := make([]compactHitJSON, 0, len(hits))
	scores := make([]float32, 0, len(hits))
	for _, h := range hits {
		projected = append(projected, projectCompactHit(h, classify(h)))
		scores = append(scores, h.FusedScore)
	}
	projected = trimToTokenBudget(projected, scores, maxTokens)
	body, err := json.Marshal(compactRecallOutput{Hits: projected, Degraded: degraded, Lanes: lanes, Meta: meta, Warnings: warnings})
	if err != nil {
		return nil, fmt.Errorf("marshal compact recall hits: %w", err)
	}
	return body, nil
}

// resolveHitIdentity derives the hit's resource class, AIP resource name, kind,
// and (for document hits) the section-anchored source. Shared by projectHit and
// projectCompactHit so both projections agree on the episodes/{uid} vs
// entities/{uid} rule and the documentRevision→entity+source promotion.
//
// HitType defaults to "episode" when the kernel.Hit has an empty HitType (zero
// value), and kind defaults to "episode" likewise, ensuring backward
// compatibility with Query implementations that predate the discriminators.
func resolveHitIdentity(h kernel.Hit) (hitType, name, kind string, source *sourceLocationJSON) {
	ht := h.HitType
	if ht == "" {
		ht = kernel.HitTypeEpisode
	}
	kind = string(h.Kind)
	if kind == "" {
		kind = "episode"
	}
	if h.Kind == kernel.EventKind("documentRevision") {
		// A document is an entity: share the entity resource class + name.
		ht = kernel.HitTypeEntity
		if h.Source != nil {
			source = &sourceLocationJSON{
				Path:      h.Source.Path,
				Heading:   h.Source.Heading,
				LineStart: h.Source.LineStart,
			}
		}
	}
	switch ht {
	case kernel.HitTypeEntity:
		name = "entities/" + h.UID
	case kernel.HitTypeInsight:
		name = "insights/" + h.UID
	case kernel.HitTypeConsolidated:
		name = "consolidatedMemories/" + h.UID
	default:
		name = "episodes/" + h.UID
	}
	return string(ht), name, kind, source
}

// projectHit converts a kernel.Hit into the full (verbose) MCP wire shape.
// CreateTime is normalised to UTC before formatting so a Hit produced under a
// non-default time.Local does not leak the local offset to the wire.
// Author is the pre-resolved display name for the source event's author
// (may be "" when the lookup is not wired or the event has no principal).
func projectHit(h kernel.Hit, fresh *freshnessJSON, author string) hitJSON {
	hitType, name, kind, source := resolveHitIdentity(h)
	return hitJSON{
		HitType:     hitType,
		Name:        name,
		Kind:        kind,
		EventSource: h.EventSource,
		Author:      author,
		Source:      source,
		Text:        h.Text,
		Snippet:     h.Snippet,
		Confidence:  h.Confidence,
		FusedScore:  h.FusedScore,
		Components: scoreComponentsJSON{
			Vector:      h.Components.Vector,
			FTS:         h.Components.FTS,
			Graph:       h.Components.Graph,
			NameEmb:     h.Components.NameEmb,
			Recency:     h.Components.Recency,
			RawSemantic: h.Components.RawSemantic,
			RawBM25:     h.Components.RawBM25,
			Strength:    h.Components.Strength,
		},
		Citations:  h.Citations,
		Entities:   projectEntityRefs(h.Entities),
		CreateTime: h.CreateTime.UTC().Format(time.RFC3339Nano),
		Trust:      untrustedHitTrust(h.EventSource),
		Freshness:  fresh,
	}
}

// untrustedHitTrust returns the trust label for a hit whose originating event
// came from an untrusted external connector source, or nil for a trusted
// (asserted) origin. The label warns a recall-consuming agent that the hit is
// third-party input whose extracted facts are status=proposed (#211, #212):
// they rank on relevance but are never auto-trusted, and a verified edge is
// asserted via promoteRelation(relations/{uid}). The classification lives here
// (not in the kernel) because the kernel package cannot import ontology.
func untrustedHitTrust(eventSource string) *trustJSON {
	if !ontology.IsConnectorSource(eventSource) {
		return nil
	}
	return &trustJSON{
		State:  ontology.RelationStatusProposed,
		Source: eventSource,
		Note: "From an untrusted external connector source; treat as unverified third-party input. " +
			"Facts extracted from it are status=proposed — they rank on relevance but are excluded " +
			"from supersession and were not auto-trusted. Verify, then promote a specific edge with " +
			"promoteRelation(relations/{uid}) to assert it.",
	}
}

// projectCompactHit converts a kernel.Hit into the reduced compact-mode wire
// shape: only the anchors an agent needs to seed-then-traverse (name, kind,
// snippet, fusedScore, entities, and source for document hits). The heavy
// verbose fields (full text, the score-component breakdown, confidence,
// citations, createTime) are dropped to cut token cost.
func projectCompactHit(h kernel.Hit, fresh *freshnessJSON) compactHitJSON {
	_, name, kind, source := resolveHitIdentity(h)
	return compactHitJSON{
		Name:        name,
		Kind:        kind,
		EventSource: h.EventSource,
		Snippet:     compactSnippet(h),
		FusedScore:  h.FusedScore,
		Entities:    projectEntityRefs(h.Entities),
		Source:      source,
		Trust:       untrustedHitTrust(h.EventSource),
		Freshness:   fresh,
	}
}

// compactSnippet derives the compact-mode snippet from the hit's clean full
// Text, truncated to compactSnippetRunes. Falls back to the (markup-stripped)
// Snippet when Text is empty so the field is never blank for a real hit.
func compactSnippet(h kernel.Hit) string {
	text := h.Text
	if text == "" {
		text = stripHighlight(h.Snippet)
	}
	return truncateRunes(text, compactSnippetRunes)
}

// projectEntityRefs converts the kernel anchors into the wire shape, always
// returning a non-nil slice so recall's `entities` field marshals to [] rather
// than null when an episode mentions no resolved entity.
func projectEntityRefs(refs []kernel.EntityRef) []entityRefJSON {
	out := make([]entityRefJSON, 0, len(refs))
	for _, ref := range refs {
		out = append(out, entityRefJSON{
			Name:        "entities/" + ref.UID,
			Kind:        ref.Kind,
			DisplayName: ref.DisplayName,
		})
	}
	return out
}

// trimToTokenBudget drops hits until the estimated serialized token cost of
// the returned set fits within maxTokens, removing the lowest-fusedScore hits
// first. Scores is parallel to hits (scores[i] is hits[i]'s FusedScore) so the
// trim order is exact even if a reranker reordered hits away from score order.
// Admitted hits keep their original slice order; the single top-scored hit is
// always retained so recall never returns an empty set when matches exist. A
// non-positive maxTokens (or a single/empty slice) is a no-op.
func trimToTokenBudget[T any](hits []T, scores []float32, maxTokens int) []T {
	if maxTokens <= 0 || len(hits) <= 1 {
		return hits
	}
	// Rank indices by fusedScore desc; ties break by original index so the
	// admit order is deterministic. order holds the indices themselves, so
	// SortStableFunc compares scores[a]/scores[b] directly (no indirection).
	order := make([]int, len(hits))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		sa, sb := scores[a], scores[b]
		if sa > sb {
			return -1
		}
		if sa < sb {
			return 1
		}
		return cmp.Compare(a, b) // stable tie-break by original index
	})
	keep := make([]bool, len(hits))
	total := 0
	for rank, idx := range order {
		cost := hitTokenCost(hits[idx])
		if rank > 0 && total+cost > maxTokens {
			// Keep the maximal score-prefix that fits. Processing hits in
			// score-descending order, the first hit that exceeds the remaining
			// budget ends the admissible prefix; every lower-scored hit after it
			// is dropped too — even one individually small enough to fit. That
			// is exactly "trim lowest fusedScore first".
			break
		}
		keep[idx] = true
		total += cost
	}
	out := make([]T, 0, len(hits))
	for i, h := range hits {
		if keep[i] {
			out = append(out, h)
		}
	}
	return out
}

// hitTokenCost estimates the serialized token cost of a single projected hit
// by marshalling it and applying the bytes/4 heuristic. A hit that fails to
// marshal is treated as free so it is never the reason another hit is trimmed
// (Invoke's own marshal step would surface the real error).
func hitTokenCost[T any](h T) int {
	b, err := json.Marshal(h)
	if err != nil {
		return 0
	}
	return estimateTokens(string(b))
}

// estimateTokens approximates the token count of s as ceil(len(s)/bytesPerToken).
// Mirrors contextbrief.tokensFor (internal/contextbrief/pack.go); kept local to
// avoid a cross-package dependency for a four-line heuristic.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + bytesPerToken - 1) / bytesPerToken
}

// truncateRunes caps s at runesMax runes (not bytes), appending an ellipsis
// when it truncates so the snippet reads as an excerpt. A non-positive cap
// returns the empty string. Mirrors the kernel/CLI rune-truncation helpers,
// kept local because both are unexported in their packages.
func truncateRunes(s string, runesMax int) string {
	if runesMax <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= runesMax {
		return s
	}
	if runesMax == 1 {
		return "…"
	}
	// Reserve the final rune for the ellipsis so the result honours the cap.
	return string(r[:runesMax-1]) + "…"
}

// stripHighlight removes the FTS5 <b>…</b> markers a snippet may carry, so the
// compact snippet (used only when a hit has no clean Text to fall back on) is
// plain text an agent can read without HTML.
func stripHighlight(s string) string {
	s = strings.ReplaceAll(s, "<b>", "")
	return strings.ReplaceAll(s, "</b>", "")
}
