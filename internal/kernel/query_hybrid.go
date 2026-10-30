package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/mo-cmyk/mazarbul/internal/kernel/geo"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

// classEpisodic mirrors ontology.ClassEpisodic without importing the
// ontology package (kernel is upstream of ontology — the reverse import
// would cycle). Keep the three legal class strings in sync with
// internal/ontology/episode.go's EpisodeClass enum; the SQLite CHECK
// constraint in migration 0015 backstops at the schema layer.
const classEpisodic = "episodic"

// ftsMinHitsBeforeFuzzy is the default threshold below which the FTS lane
// retries with a prefix-expanded query (R3). When a verbatim match returns
// fewer than this many hits the fallback widens each token with a trailing
// `*` and surfaces warncode.CodeRecallFuzzyFallback so the caller knows the
// result set was broadened.
const ftsMinHitsBeforeFuzzy = 3

// anchorNavigator is the narrow subset of navigate.Navigator that the
// anchor-scoped recall feature (R11) needs. Declared as an interface (not
// the concrete type) so the kernel package does not import navigate (which
// already imports kernel — the reverse would cycle). The daemon installs
// SetNavigator at wiring time; tests inject a fake.
type anchorNavigator interface {
	// NeighborUIDs returns the UIDs of entities reachable from seed within
	// depth hops. The slice may be empty when seed is unknown or isolated.
	// depth is clamped to 1..MaxHops by the caller.
	NeighborUIDs(ctx context.Context, seed string, depth int) ([]string, error)
}

// Embedder produces a fixed-dimension dense embedding for a single piece of
// text. The interface is declared here, on the consumer side, so HybridQuery
// can accept any provider that satisfies it (the M1 default is
// *ollama.Client) without the kernel importing the provider package.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// nameVectorSearcher is the optional surface a VectorConn can satisfy to
// expose a domain-scoped entity name-vector search. HybridQuery reaches for
// it via a runtime type assertion; a backend that does not implement it
// silently skips the name-vector lane without error. The sqlite.Vector
// implementation satisfies this via SearchByDomain("entities", ...) With a
// discriminator filter for vector_type='name'.
type nameVectorSearcher interface {
	SearchNameVectors(ctx context.Context, query []float32, k int) ([]VectorHit, error)
}

// HybridQuery is the four-lane recall composer for Mazarbul. It fuses a
// dense-vector search, an FTS5 BM25 search, a graph 1-hop (and conditional
// 2-hop) entity traversal, and an entity name-vector lane via Reciprocal Rank
// Fusion (Cormack, Clarke and Buttcher, SIGIR 2009, k=60).
//
// Graph traversal: when a GraphConn is wired, HybridQuery looks up the entities
// cited in the top-K vector+FTS episodes, traverses one hop via RELATES_TO
// edges (temporal filter: now BETWEEN validFrom AND validUntil), and expands to
// 2-hop when the 1-hop set is smaller than twoHopFloor. Edge traversal is
// restricted to the edgeAllow predicate list (empty means all predicates).
//
// Name-vector lane: when the VectorConn satisfies nameVectorSearcher, the
// query embedding is also matched against entities_vec WHERE vector_type='name'.
// Matched entity UIDs are joined back to episodes via the mentions table.
//
// Spatial filter: when RecallRequest.Near is set, the recalled episode set is
// filtered to episodes citing at least one entity within the given radius of
// the resolved place. Entities without coordinates are excluded from
// spatial-filtered results.
//
// Filtering: req.Kinds and req.MinConfidence are applied before fusion,
// matching the industry convention (OpenSearch 3.0 distributes filters to
// each sub-query before RRF). When a candidate is filtered out its rank slot
// vanishes and the surviving items are re-ranked 1..N within their source
// list before the RRF denominator is computed.
//
// Traverse is inherited from NoopQuery so callers see ErrNotImplemented when
// they reach for the unbuilt half of the interface.
type HybridQuery struct {
	// NoopQuery is embedded so Traverse retains its M1 placeholder semantics
	// without requiring a separate definition. The Recall method below
	// shadows the embedded one.
	NoopQuery

	embedder Embedder
	vec      VectorConn
	fts      FTSConn
	reader   *sql.DB

	candidateK   int
	rrfK         int
	defaultLimit int
	rrfMinScore  float32

	// reranker is the optional post-fusion re-ranking step. When nil the
	// pass-through behaviour is preserved so existing callers are unaffected.
	reranker Reranker

	// rerankEnabled gates the reranker so it can be installed at construction
	// time (via SetReranker) without activating until the caller explicitly
	// opts in. Default false matches MMR (mmrLambda=0) and recency
	// (recencyBoost=0): opt-in only. Set via WithRerankEnabled or the
	// [recall] rerank = true config knob.
	rerankEnabled bool

	// snippetRunes caps the rune-count of the snippet returned for vec-only
	// hits (FTS hits already carry a 30-char snippet from the FTS5
	// auxiliary). 200 runes is enough for callers to render or summarize
	// without dragging the full episode text through the iterator.
	snippetRunes int

	// graph is the optional graph connection used for entity traversal.
	// Nil disables graph expansion and the name-vector lane gracefully.
	graph GraphConn

	// twoHopFloor is the minimum number of 1-hop entity results required to
	// skip 2-hop expansion. When the 1-hop set has fewer than twoHopFloor
	// entities, a second traversal at depth 2 is run with the same edge
	// filter applied. Default: 3.
	twoHopFloor int

	// edgeAllow is the predicate allowlist for multi-hop graph traversal.
	// An empty slice means all predicates are followed. Each element is a
	// RELATES_TO.predicate value (e.g. "works_at", "member_of").
	edgeAllow []string

	// graphWeight scales the graph RRF component in the fused score. Default 1.0.
	graphWeight float64

	// nameEmbWeight scales the name-embedding RRF component. Default 1.0.
	nameEmbWeight float64

	// rewriter is the optional query-rewrite-v1 expansion helper. When nil,
	// RecallRequest.RewriteQuery is a no-op.
	rewriter QueryRewriter

	// docProbeOnce / docAvailable cache the one-time documents-projection
	// probe (documentsProjectionAvailable): the document lane only runs when
	// migration 0016's tables exist in the wired reader, so minimal test
	// schemas run recall unchanged.
	docProbeOnce sync.Once
	docAvailable bool

	// stats is the runtime counter sink. Recall calls IncRecall on every
	// invocation and ObserveTopEntity on each hit's primary entity.
	// Defaults to stats.Noop() so callers that do not inject a store
	// incur no overhead and no nil-check is needed at the call site.
	stats stats.Store

	// mmrLambda controls Maximal Marginal Relevance after RRF fusion:
	//   λ=0  → MMR disabled (pre-MMR behaviour; default).
	//   λ=1  → pure relevance, equivalent to FusedScore ordering.
	//   0<λ<1 → trade off relevance vs diversity; production-consensus
	//           default is 0.7 (Carbonell & Goldstein 1998).
	// The motivation is the user-reported "Arwen pollution" — 10×
	// near-identical episodes drowning out the actual answer-bearing
	// episode in the top-K cut. MMR suppresses near-duplicates without
	// changing RRF math.
	mmrLambda float64

	// recencyBoost scales the Gaussian recency multiplier applied after
	// RRF fusion: FusedScore *= (1 + recencyBoost · exp(-Δd²/(2σ²))).
	// Zero (default) disables the multiplier; 0.15 is the production
	// default (recency as tiebreaker, not primary signal).
	recencyBoost float64

	// recencyHalflifeDays is σ (in days) for the Gaussian decay. Default
	// 30 days matches Elastic/OpenSearch function_score gauss with
	// scale=30d, decay≈0.5. Only consulted when recencyBoost > 0.
	recencyHalflifeDays float64

	// fusionMode picks the fusion strategy applied to per-lane ranks.
	// "weighted" (the constructor default) applies WeightedComponents —
	// mem0 v3's adaptive sum/max_possible blend over raw cosine +
	// sigmoid-normalized BM25, which keeps the fused score in [0, 1]
	// (cosine-comparable, so abstain thresholds carry over) AND pins the
	// invariant that a sole keyword match outranks higher-cosine
	// non-matches — the buried-exact-match failure additive RRF has. Any
	// other value ("rrf", or empty) selects additive 1/(k+rank) RRF. The
	// daemon overrides the default only when [recall] fusion_mode is set.
	fusionMode string

	// semanticGateThreshold is the pre-fusion semantic floor: candidates
	// whose raw cosine similarity falls below it are dropped from the
	// fusion loop (so they don't enter the result set, can't be rescued
	// by BM25 or entity matches, and don't influence the abstain gate).
	// Matches mem0/utils/scoring.py:102-103. Zero (the default)
	// disables the gate; production tuning starts around 0.2-0.3.
	// Only consulted when fusionMode == "weighted".
	semanticGateThreshold float64

	// typeWeights maps EpisodeClass to the multiplier applied to the
	// fused score (Phase 3 of the mem0-parity sweep). mem0 v3 defaults
	// to semantic=0.6, episodic=0.3, procedural=0.1 — context-stable
	// facts surface above session chatter on fact-seeking queries
	// without the LLM needing a class-aware prompt. Empty (the default)
	// disables the multiplier so older callers keep their existing
	// scores. Tune via WithTypeWeights.
	typeWeights map[string]float64

	// strengthBoost scales the passive-reinforcement multiplier applied
	// after the type-weight multiplier (issue #231): FusedScore *= (1 +
	// strengthBoost · reinforcement(accessCount) · decay(now − lastAccessed)).
	// 0.15 (the production default) keeps reinforcement a tiebreaker, not a
	// dominant signal — the same magnitude as recencyBoost. Zero disables it.
	strengthBoost float64

	// strengthHalflifeDays is σ (in days) for the Gaussian access-recency
	// decay anchoring the strength multiplier on last_accessed_at. Default
	// 30 days, matching recencyHalflifeDays. Only consulted when
	// strengthBoost > 0.
	strengthHalflifeDays float64

	// salienceBase is the reinforcement curve base r0 in
	// ReinforcementScore(accessCount, salienceBase) = 1 − (1−r0)·0.9^n.
	// Default 0.5 (the memory_strength.salience_base column default). A
	// never-accessed memory has reinforcement = salienceBase.
	salienceBase float64

	// accessSink records that recall surfaced a set of memories, emitting
	// memoryAccessed events that fold into memory_strength (issue #231).
	// Installed by the daemon via SetAccessRecorder; nil disables passive
	// capture (tests, degraded modes). The implementation is responsible for
	// being non-blocking and best-effort — recall never waits on it.
	accessSink AccessRecorder

	// embedDeadline caps the per-recall query-embedding call. When positive
	// and the Embed call exceeds this duration, recall degrades to FTS-only
	// and reports warncode.CodeRecallEmbedderDegraded. Zero means no
	// per-recall deadline (the caller's context deadline still applies).
	// Mirrors the PR-286 fix pattern for defineEntityKind's unbounded embed.
	embedDeadline time.Duration

	// navigator is the optional Navigator used for anchor-scoped recall
	// (RecallRequest.Anchor). When nil, anchor requests fall through to full
	// recall without error — a graceful degradation. Installed by the daemon
	// at wiring time once the SQLite reader is available.
	navigator anchorNavigator

	// fuzzyFallbackThreshold is the minimum number of FTS hits below which
	// a prefix-expanded fallback fires (R3). Default ftsMinHitsBeforeFuzzy.
	// Zero disables the fallback entirely.
	fuzzyFallbackThreshold int
}

// Compile-time assertion that HybridQuery satisfies Query.
var _ Query = (*HybridQuery)(nil)

const (
	// CandidateKDefault is the per-source top-K fed into RRF. 100 sits in
	// the production middle ground between the architecture spec's top-20
	// (too shallow for robust long-tail recall) and the issue text's
	// top-2K (which exceeds the vector layer's maxSearchK=1024 ceiling
	// and wastes compute). Tune via WithCandidateK.
	candidateKDefault = 100

	// RrfKDefault is the canonical RRF constant from Cormack et al. 2009.
	// Defaults to 60 in Elasticsearch, OpenSearch, Vespa, Qdrant, and
	// Azure AI Search. Tune via WithRRFK for tests or future experiments.
	rrfKDefault = 60

	// FusionModeWeighted is the constructor-default fusion strategy: mem0
	// v3's adaptive weighted blend (WeightedComponents). It is the default
	// because additive RRF buries a sole exact-keyword match under
	// entity-connected episodes that accumulate across lanes; weighted
	// fusion pins the opposite invariant. [recall] fusion_mode = "rrf"
	// (or any non-"weighted" value) opts back into additive RRF.
	FusionModeWeighted = "weighted"
	// FusionModeRRF opts back into additive reciprocal-rank fusion via
	// [recall] fusion_mode = "rrf" (or kernel.WithFusionMode).
	FusionModeRRF = "rrf"

	// RecallLimitDefault matches the RecallRequest.Limit godoc on
	// query.go: "Zero means the composer's internal default
	// (currently top-N then re-rank to top-10)".
	recallLimitDefault = 10

	// SnippetRunesDefault is the rune cap for vec-only snippet fallback.
	// Exported so the MCP recall tool can derive its compact-mode snippet cap
	// from the same source of truth instead of hard-coding a copy that drifts.
	SnippetRunesDefault = 200

	// TwoHopFloorDefault is the default minimum 1-hop entity count below
	// which 2-hop expansion is triggered.
	twoHopFloorDefault = 3

	// RecencyHalflifeDaysDefault is the σ for the Gaussian recency
	// decay (units: days). Mirrors Elastic/OpenSearch function_score
	// gauss with scale=30d, decay≈0.5: a 30-day-old episode keeps ~60%
	// of the boost, a 90-day-old episode keeps ~1%.
	recencyHalflifeDaysDefault = 30.0

	// SpatialDefaultRadiusKm is the fallback radius applied when NearFilter
	// is supplied without an explicit RadiusKm.
	spatialDefaultRadiusKm = 50.0

	// DefaultEdgeAllow seeds the allowlist with the most semantically
	// productive traversal predicates when the caller does not specify any.
	// Empty means "all predicates"; production wiring can narrow this via
	// WithEdgeAllowlist to reduce false-positive graph hits.
)

// seedEdgeAllowlist returns the default predicate allowlist for multi-hop graph
// traversal. Returned as a new slice each call so callers that mutate it (via
// WithEdgeAllowlist) do not alias the constructor default.
func seedEdgeAllowlist() []string {
	return []string{"works_at", "member_of", "located_in"}
}

// HybridOption configures NewHybridQuery. All options are optional; production
// callers pass none and accept the defaults documented above.
type HybridOption func(*HybridQuery)

// WithCandidateK overrides the per-source top-K fed into RRF.
func WithCandidateK(k int) HybridOption {
	return func(h *HybridQuery) {
		if k > 0 {
			h.candidateK = k
		}
	}
}

// WithRRFK overrides the RRF k constant. Positive values only; zero or
// negative are ignored so callers cannot accidentally turn the denominator
// into a division-by-zero or sign-flip.
func WithRRFK(k int) HybridOption {
	return func(h *HybridQuery) {
		if k > 0 {
			h.rrfK = k
		}
	}
}

// WithDefaultLimit overrides the default Limit used when RecallRequest.Limit
// is zero. Production code should leave this alone; the option exists so
// tests can pin a deterministic limit without setting it on every request.
func WithDefaultLimit(n int) HybridOption {
	return func(h *HybridQuery) {
		if n > 0 {
			h.defaultLimit = n
		}
	}
}

// WithRRFMinScore sets a minimum fused-score floor. Hits whose fused score
// falls below s after RRF fusion are dropped before the limit cut. A value of
// 0 (the default) disables the floor and preserves the existing pass-through
// behaviour. Negative values are ignored. The floor lets long-tail noise from
// low-confidence lanes be cut before rerank without changing the RRF math.
func WithRRFMinScore(s float32) HybridOption {
	return func(h *HybridQuery) {
		if s >= 0 {
			h.rrfMinScore = s
		}
	}
}

// WithReranker attaches an optional post-fusion Reranker. When r is non-nil
// the reranker runs between the RRF sort (after the min-score filter) and the
// limit cut so it operates on the full candidate window rather than an
// already-truncated slice. Passing nil is a no-op; the existing pass-through
// behaviour is preserved.
func WithReranker(r Reranker) HybridOption {
	return func(h *HybridQuery) {
		h.reranker = r
	}
}

// WithRerankEnabled gates the installed Reranker. When enabled is false
// (the default) the reranker is never invoked even if one has been
// installed via WithReranker or SetReranker, so recall always returns
// hits in deterministic FusedScore-descending order. Set enabled=true to
// activate LLM reranking; this is consistent with MMR (mmrLambda>0) and
// recency (recencyBoost>0) both being opt-in. The [recall] rerank = true
// config knob maps to this option.
func WithRerankEnabled(enabled bool) HybridOption {
	return func(h *HybridQuery) {
		h.rerankEnabled = enabled
	}
}

// WithGraphConn wires a graph connection for entity traversal. When nil,
// graph expansion and the name-vector lane are skipped without error.
func WithGraphConn(g GraphConn) HybridOption {
	return func(h *HybridQuery) {
		h.graph = g
	}
}

// WithGraphWeight sets the per-source RRF weight for the graph lane. A
// weight of 0 disables the lane's RRF contribution — a valid choice, and
// the one a config explicitly setting graph_weight = 0.0 expects. Only
// negative weights are ignored. RRF-only: weighted fusion consumes the
// graph rank directly, so this weight has no effect there.
func WithGraphWeight(w float64) HybridOption {
	return func(h *HybridQuery) {
		if w >= 0 {
			h.graphWeight = w
		}
	}
}

// WithTwoHopFloor sets the minimum 1-hop entity count below which 2-hop
// expansion fires. A floor of 0 disables 2-hop expansion (the 1-hop count
// is never below 0); only negative values are ignored. Applies in both
// RRF and weighted fusion (the entity lane feeds both).
func WithTwoHopFloor(n int) HybridOption {
	return func(h *HybridQuery) {
		if n >= 0 {
			h.twoHopFloor = n
		}
	}
}

// WithEdgeAllowlist restricts multi-hop graph traversal to the named
// predicates. An empty call sets an empty allowlist (all predicates). At
// least one predicate must be passed; to reset to the default seed list,
// do not call this option.
func WithEdgeAllowlist(preds ...string) HybridOption {
	return func(h *HybridQuery) {
		h.edgeAllow = preds
	}
}

// WithNameEmbWeight sets the per-source RRF weight for the name-embedding
// lane. A weight of 0 disables the lane — a valid choice; only negative
// weights are ignored. RRF-only: weighted fusion consumes the
// name-embedding rank directly, so this weight has no effect there.
func WithNameEmbWeight(w float64) HybridOption {
	return func(h *HybridQuery) {
		if w >= 0 {
			h.nameEmbWeight = w
		}
	}
}

// WithMMRLambda enables Maximal Marginal Relevance after RRF fusion.
// Λ = 0 disables (default; preserves pre-MMR behaviour). Λ = 1 reduces
// to pure FusedScore ordering. 0 < λ < 1 trades relevance off against
// diversity; 0.7 is the production-consensus default for near-duplicate
// suppression (Carbonell & Goldstein 1998). Values outside [0, 1] are
// silently clamped to that interval.
func WithMMRLambda(lambda float64) HybridOption {
	return func(h *HybridQuery) {
		switch {
		case lambda < 0:
			h.mmrLambda = 0
		case lambda > 1:
			h.mmrLambda = 1
		default:
			h.mmrLambda = lambda
		}
	}
}

// WithRecencyBoost enables the Gaussian recency multiplier after RRF
// fusion. Boost is the multiplicative gain in [0, 1]; 0 (default)
// disables, 0.15 is the production default (recency as tiebreaker).
// Values outside [0, 1] are clamped. The σ knob (halflife in days) is
// configured separately via WithRecencyHalflife.
func WithRecencyBoost(boost float64) HybridOption {
	return func(h *HybridQuery) {
		switch {
		case boost < 0:
			h.recencyBoost = 0
		case boost > 1:
			h.recencyBoost = 1
		default:
			h.recencyBoost = boost
		}
	}
}

// WithRecencyHalflife sets σ (in days) for the Gaussian recency decay.
// Non-positive values are ignored; the default (30 days) holds. Larger
// σ = slower decay (older episodes still boosted); smaller σ = sharper
// preference for recent ones.
func WithRecencyHalflife(days float64) HybridOption {
	return func(h *HybridQuery) {
		if days > 0 {
			h.recencyHalflifeDays = days
		}
	}
}

// WithFusionMode selects the fusion strategy. "weighted" (the kernel
// default; see FusionModeWeighted) applies the WeightedComponents blend.
// "rrf" — or any other value, including empty — selects additive
// reciprocal-rank fusion, so a typo in the config degrades to RRF rather
// than taking the recall lane offline. The daemon passes this option only
// when [recall] fusion_mode is set; an absent block leaves the default.
func WithFusionMode(mode string) HybridOption {
	return func(h *HybridQuery) {
		h.fusionMode = mode
	}
}

// WithSemanticGateThreshold enables the pre-fusion semantic floor:
// candidates whose raw cosine similarity (vec lane) falls below `t`
// are dropped from the result set before fusion runs. Matches mem0's
// scoring.py:102-103 — keyword overlap and entity matches cannot
// rescue a semantically-unrelated candidate.
//
// `t` must be in [0, 1]; values outside that range are clamped. Zero
// (the default) disables the gate. Negative values clamp to 0 (same
// effect). Only consulted when fusionMode == "weighted"; in RRF mode
// the gate is silently inactive because RRF doesn't carry raw cosine.
//
// Production tuning starts around 0.2-0.3: low enough to admit
// borderline-relevant evidence the composer can synthesize, high
// enough to filter the long tail of FTS-only false positives.
func WithSemanticGateThreshold(t float64) HybridOption {
	return func(h *HybridQuery) {
		if t < 0 {
			t = 0
		}
		if t > 1 {
			t = 1
		}
		h.semanticGateThreshold = t
	}
}

// WithTypeWeights installs per-EpisodeClass multipliers applied to the
// fused score (Phase 3 of the mem0-parity sweep). The map keys are
// EpisodeClass string values ("semantic", "episodic", "procedural")
// and the values are clamped to [0, 1]. Mem0's documented defaults are
// 0.6 / 0.3 / 0.1: context-stable facts (semantic) outrank session
// turns (episodic) which outrank step-by-step instructions
// (procedural) on fact-seeking queries.
//
// Passing nil or an empty map (the default) disables the multiplier:
// every class scores at its raw fused value, preserving pre-Phase-3
// behavior. Unknown class strings fall through to the episodic
// default (0.3 when the map is set, 1.0 when the map is nil).
func WithTypeWeights(weights map[string]float64) HybridOption {
	return func(h *HybridQuery) {
		if len(weights) == 0 {
			h.typeWeights = nil
			return
		}
		clamped := make(map[string]float64, len(weights))
		for k, v := range weights {
			if v < 0 {
				v = 0
			}
			if v > 1 {
				v = 1
			}
			clamped[k] = v
		}
		h.typeWeights = clamped
	}
}

// WithRewriter wires a QueryRewriter for query-rewrite-v1 expansion. When
// nil, RecallRequest.RewriteQuery is silently ignored (no-op degradation).
func WithRewriter(rw QueryRewriter) HybridOption {
	return func(h *HybridQuery) {
		h.rewriter = rw
	}
}

// WithEmbedDeadline sets the per-recall query-embedding deadline. When the
// Embed call exceeds d, recall degrades to FTS-only and reports
// warncode.CodeRecallEmbedderDegraded in the RecallReport. A non-positive d
// is ignored (no per-recall deadline). Mirrors the PR-286 fix for
// defineEntityKind's unbounded embed.
func WithEmbedDeadline(d time.Duration) HybridOption {
	return func(h *HybridQuery) {
		if d > 0 {
			h.embedDeadline = d
		}
	}
}

// WithNavigator wires the Navigator used for anchor-scoped recall. When nil
// (the default), anchor requests fall through to full recall without error.
// The daemon calls SetNavigator after construction because the Navigator
// depends on the SQLite reader, which is built downstream of NewHybridQuery.
func WithNavigator(nav anchorNavigator) HybridOption {
	return func(h *HybridQuery) {
		h.navigator = nav
	}
}

// WithFuzzyFallbackThreshold sets the minimum FTS hit count below which a
// prefix-expanded retry fires. A threshold of 0 disables the fallback. The
// default (ftsMinHitsBeforeFuzzy = 3) keeps the fallback active for sparse
// results without broadening large result sets.
func WithFuzzyFallbackThreshold(n int) HybridOption {
	return func(h *HybridQuery) {
		if n >= 0 {
			h.fuzzyFallbackThreshold = n
		}
	}
}

// SetRewriter installs a QueryRewriter after construction. Late-binding is
// required because the rewriter depends on the prompts.Registry, which is
// built inside wireMemoryLayer after NewHybridQuery has already run.
// Passing nil clears any previously installed rewriter.
func (h *HybridQuery) SetRewriter(rw QueryRewriter) {
	h.rewriter = rw
}

// SetNavigator installs an anchorNavigator after construction. Late-binding
// is required because the navigator depends on the SQLite reader, which is
// built inside wireMemoryLayer downstream of NewHybridQuery. Passing nil
// disables anchor-scoped recall (full recall is used instead).
func (h *HybridQuery) SetNavigator(nav anchorNavigator) {
	h.navigator = nav
}

// SetReranker installs a post-fusion Reranker after construction. Same
// late-binding rationale as SetRewriter: the reranker depends on the
// chat provider, which is plumbed by wireMemoryLayer downstream of
// NewHybridQuery. Passing nil clears any previously installed reranker.
func (h *HybridQuery) SetReranker(r Reranker) {
	h.reranker = r
}

// SetStats installs a stats.Store after construction. Recall calls
// IncRecall once per invocation and ObserveTopEntity for each hit's
// primary entity. Passing nil falls back to stats.Noop() so the hot
// path never panics on a missing dependency.
func (h *HybridQuery) SetStats(s stats.Store) {
	if s == nil {
		h.stats = stats.Noop()
		return
	}
	h.stats = s
}

// NewHybridQuery wires the composer. Emb embeds the query text; vec runs the
// dense top-K; fts runs the BM25 top-K; reader is the read-only SQLite pool
// used for hydrating UID -> (kind, createTime, payloadJSON). Vec, fts and
// reader are required; nil arguments return an error so misconfiguration
// surfaces at construction rather than on the first Recall call.
//
// Emb MAY be nil: that builds the composer in degraded FTS(+graph)-only mode.
// Every recall then skips the dense-vector and entity-name-vector lanes (which
// need a query embedding) and reports Degraded=true via RecallWithReport. The
// daemon registers this variant when the embedder is transiently unreachable
// so recall serves BM25 hits instead of hard-failing (#236).
//
// Graph features require WithGraphConn. When no GraphConn is provided the
// composer degrades gracefully to vector + FTS fusion only.
func NewHybridQuery(emb Embedder, vec VectorConn, fts FTSConn, reader *sql.DB, opts ...HybridOption) (*HybridQuery, error) {
	if vec == nil {
		return nil, errors.New("kernel: hybrid: nil vector conn")
	}
	if fts == nil {
		return nil, errors.New("kernel: hybrid: nil fts conn")
	}
	if reader == nil {
		return nil, errors.New("kernel: hybrid: nil reader pool")
	}
	h := &HybridQuery{
		embedder:               emb,
		vec:                    vec,
		fts:                    fts,
		reader:                 reader,
		candidateK:             candidateKDefault,
		rrfK:                   rrfKDefault,
		defaultLimit:           recallLimitDefault,
		snippetRunes:           SnippetRunesDefault,
		twoHopFloor:            twoHopFloorDefault,
		edgeAllow:              seedEdgeAllowlist(),
		graphWeight:            1.0,
		nameEmbWeight:          1.0,
		recencyHalflifeDays:    recencyHalflifeDaysDefault,
		strengthBoost:          strengthBoostDefault,
		strengthHalflifeDays:   strengthHalflifeDaysDefault,
		salienceBase:           salienceBaseDefault,
		fuzzyFallbackThreshold: ftsMinHitsBeforeFuzzy,
		// Weighted fusion (mem0 v3 adaptive blend) is the default: it pins
		// the "a sole keyword match must outrank higher-cosine non-matches"
		// invariant (TestWeightedComponents_SoleMatchBeatsNonMatches) that
		// additive RRF violates — a lone exact hit gets buried under
		// entity-connected episodes that accumulate across lanes. Set
		// [recall] fusion_mode = "rrf" to opt back into additive RRF.
		fusionMode: FusionModeWeighted,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// GraphWeight returns the configured graph-lane RRF weight. It exists so the
// daemon's recall-tuning wiring can be tested end-to-end (config pointer →
// applyRecallTuning → setter → field) — in particular that an explicit
// graph_weight = 0.0 lands as 0 rather than being swallowed; it is not used
// on the recall hot path.
func (h *HybridQuery) GraphWeight() float64 { return h.graphWeight }

// NameEmbWeight returns the configured name-embedding-lane RRF weight.
func (h *HybridQuery) NameEmbWeight() float64 { return h.nameEmbWeight }

// TwoHopFloor returns the configured 1-hop entity count below which the
// graph lane expands to 2-hop.
func (h *HybridQuery) TwoHopFloor() int { return h.twoHopFloor }

// scopeCtxKey is the unexported context key for per-request scope spans.
type scopeCtxKey struct{}

// withScopes stamps the read's scope span onto ctx. Recall sets it once per
// request; the SQLite lane helpers read it via scopesFromContext, so the
// per-request scope is thread-safe on the shared HybridQuery without threading
// a parameter through every helper (#206).
func withScopes(ctx context.Context, s ScopeSet) context.Context {
	return context.WithValue(ctx, scopeCtxKey{}, s)
}

// scopesFromContext returns the scope span Recall stamped, or the empty
// (fail-closed) set when absent — a lane reached without a scoped context
// returns nothing rather than leaking every scope.
func scopesFromContext(ctx context.Context) ScopeSet {
	if s, ok := ctx.Value(scopeCtxKey{}).(ScopeSet); ok {
		return s
	}
	return ScopeSet{}
}

// scopeSetForRequest resolves the scope span for a recall: the caller's scopes
// when given (validated), the default {project,personal} when omitted, and the
// empty fail-closed set when a scope token is unrecognised (the MCP/CLI layer
// validates first; this is a defensive backstop).
func scopeSetForRequest(raw []string) ScopeSet {
	if len(raw) == 0 {
		return DefaultReadSet()
	}
	s, err := ParseScopeSet(raw)
	if err != nil {
		return ScopeSet{}
	}
	return s
}

// Recall implements Query. The iterator yields scored Hits in descending
// FusedScore order, capped to req.Limit (or the composer's default). On
// retrieval failure the iterator yields a single (Hit{}, err) and stops; on
// an empty query string it yields nothing without error.
func (h *HybridQuery) Recall(ctx context.Context, req RecallRequest) iter.Seq2[Hit, error] {
	return func(yield func(Hit, error) bool) {
		hits, _, err := h.runRecall(ctx, req)
		if err != nil {
			yield(Hit{}, err)
			return
		}
		for _, hit := range hits {
			if !yield(hit, nil) {
				return
			}
		}
	}
}

// RecallWithReport implements ReportingQuery: it runs the same composer as
// Recall but returns the full hit slice plus a RecallReport (degraded flag +
// active lanes) in one call. The MCP recall tool uses it to advertise BM25-only
// degradation to agents; the report is a return value, so concurrent callers on
// the shared HybridQuery never race on it.
func (h *HybridQuery) RecallWithReport(ctx context.Context, req RecallRequest) ([]Hit, RecallReport, error) {
	return h.runRecall(ctx, req)
}

// runRecall is the shared body behind Recall (streaming) and RecallWithReport.
// It embeds the query (degrading to FTS-only when the embedder is nil or
// errors), runs the lanes, and returns the capped hit slice with its report.
func (h *HybridQuery) runRecall(ctx context.Context, req RecallRequest) ([]Hit, RecallReport, error) {
	start := time.Now()
	// Always bump the recall counter (even for empty-query short-circuits
	// and lane failures) so the Usage panel reflects every time the recall
	// surface was touched.
	defer func() {
		if h.stats != nil {
			h.stats.IncRecall(nil, time.Since(start))
		}
	}()
	if strings.TrimSpace(req.Query) == "" {
		return nil, RecallReport{Lanes: []string{}}, nil
	}

	ctx = withScopes(ctx, scopeSetForRequest(req.Scopes))
	req = h.maybeRewrite(ctx, req)
	req = applyAsOf(req)
	qvec, report := h.embedQuery(ctx, req)
	hits, meta, warnings, err := h.collectHitsWithMeta(ctx, qvec, req)
	if err != nil {
		return nil, RecallReport{}, err
	}
	// Merge warnings from embedQuery with any raised inside collectHits.
	report.Warnings = append(report.Warnings, warnings...)
	// R2: attach timing + per-lane candidate counts when the caller opted in.
	if req.Explain {
		meta.TookMs = time.Since(start).Milliseconds()
		report.Meta = &meta
	}
	h.observeTopEntities(hits)
	h.recordAccesses(ctx, hits)
	return hits, report, nil
}

// embedQuery embeds the (already-rewritten) query text and reports how recall
// will run. A nil embedder or an Embed error degrades the request to
// FTS(+graph)-only: qvec is returned nil so the vector and name-vector lanes
// are skipped downstream, and the report carries Degraded=true. The Embed error
// is swallowed by design — a BM25 answer beats a hard failure when the embedder
// is transiently unreachable (#236).
//
// When h.embedDeadline > 0 (or req.EmbedDeadline > 0) the embed call is
// bounded by a derived child context; a DeadlineExceeded is treated the same
// as any other Embed error (FTS-only degradation) and additionally surfaces
// warncode.CodeRecallEmbedderDegraded in the returned report.
func (h *HybridQuery) embedQuery(ctx context.Context, req RecallRequest) ([]float32, RecallReport) {
	var qvec []float32
	degraded := false
	var warnings []RecallWarning

	switch h.embedder {
	case nil:
		degraded = true
	default:
		// Per-recall embed deadline: honour the tightest of the field-level
		// and request-level deadlines. Both are optional; zero means "use the
		// caller's context deadline". Mirrors the PR-286 fix pattern that
		// bounded defineEntityKind's unbounded embed.
		embedCtx := ctx
		deadline := h.embedDeadline
		if req.EmbedDeadline > 0 && (deadline <= 0 || req.EmbedDeadline < deadline) {
			deadline = req.EmbedDeadline
		}
		var cancel context.CancelFunc
		if deadline > 0 {
			embedCtx, cancel = context.WithTimeout(ctx, deadline)
			defer cancel()
		}

		v, err := h.embedder.Embed(embedCtx, req.Query)
		if err != nil {
			logging.FromContext(ctx).Warn(
				"kernel: hybrid: embed failed; degrading to FTS-only recall",
				zap.Error(err),
			)
			degraded = true
			if errors.Is(err, context.DeadlineExceeded) {
				warnings = append(warnings, RecallWarning{
					Code:    warncode.CodeRecallEmbedderDegraded,
					Message: fmt.Sprintf("embedder exceeded %s deadline; running FTS-only recall", deadline),
				})
			}
		} else {
			qvec = v
		}
	}
	return qvec, RecallReport{
		Degraded: degraded,
		Lanes:    h.activeLanes(qvec, req),
		Warnings: warnings,
	}
}

// activeLanes lists, in canonical order, the retrieval lanes that will run for
// this request: the vector and name-vector lanes only when a query embedding is
// present (qvec != nil), FTS always, and graph when a GraphConn is wired and the
// caller did not pass NoGraph.
func (h *HybridQuery) activeLanes(qvec []float32, req RecallRequest) []string {
	lanes := make([]string, 0, 4)
	if qvec != nil {
		lanes = append(lanes, LaneVector)
	}
	lanes = append(lanes, LaneFTS)
	if !req.NoGraph && h.graph != nil {
		lanes = append(lanes, LaneGraph)
	}
	if qvec != nil && !req.NoGraph {
		if _, ok := h.vec.(nameVectorSearcher); ok {
			lanes = append(lanes, LaneNameEmb)
		}
	}
	return lanes
}

// observeTopEntities records each hit's primary entity anchor so the usage
// stats reflect which entities recall actually surfaces. This feeds stats.Top,
// which backs both the entities tool's recallCount sort and the context brief's
// "Key entities" ranking. Called on the full capped hit set before the yield
// loop so a consumer that stops early (yield false) still contributes to usage.
// Empty-anchor hits (free-text notes) are skipped.
func (h *HybridQuery) observeTopEntities(hits []Hit) {
	if h.stats == nil {
		return
	}
	for i := range hits {
		if len(hits[i].Entities) == 0 {
			continue
		}
		primary := hits[i].Entities[0]
		h.stats.ObserveTopEntity(primary.UID, primary.DisplayName)
	}
}

// collectHits is the iterator-free body of Recall. It returns the final
// ordered slice (already capped to the effective limit) so the iterator
// wrapper can honour an early `yield false` cleanly. Named `collectHits`
// (not a case-variant of Recall) so revive's confusing-naming check stays
// quiet.
// MaybeRewrite expands req.Query and (when the rewriter resolves a relative
// temporal anchor against req.CurrentDate) folds the inferred timestamp into
// req.At. On any rewriter error, empty expansion, or nil rewriter the request
// is returned unchanged (graceful degradation).
func (h *HybridQuery) maybeRewrite(ctx context.Context, req RecallRequest) RecallRequest {
	if !req.RewriteQuery || h.rewriter == nil {
		return req
	}
	hint := RewriteHint{CurrentDate: req.CurrentDate}
	result, err := h.rewriter.Rewrite(ctx, req.Query, hint)
	if err != nil || strings.TrimSpace(result.Query) == "" {
		return req
	}
	req.Query = result.Query
	// Only adopt the rewriter's inferred timestamp when the caller did not
	// supply an explicit At. An explicit At wins; the rewriter is a fallback
	// for callers that didn't compute one themselves.
	if result.At != nil && req.At == nil {
		req.At = result.At
	}
	return req
}

// applyAsOf folds RecallRequest.AsOf into the standard time-window fields so
// every lane inherits the transaction-time bound for free. When AsOf is set
// and Until is unset, Until becomes AsOf (vec / FTS / hydration filters
// already honour Until via createTime < Until). When Until is already tighter
// than AsOf, it wins. The graph lane reads AsOf directly via req.AsOf when
// it needs to scope relation createTime; this helper only handles the
// episode-side projection.
func applyAsOf(req RecallRequest) RecallRequest {
	if req.AsOf == nil {
		return req
	}
	asOf := req.AsOf.UTC()
	if req.Until.IsZero() || asOf.Before(req.Until) {
		req.Until = asOf
	}
	return req
}

// collectHitsWithMeta is the R2/R11 wrapper around collectHits. It resolves
// the anchor frontier when RecallRequest.Anchor is set (R11), injects the
// anchor filter into the request, runs collectHits, and returns per-lane
// candidate counts for R2 telemetry alongside any accumulated warnings.
func (h *HybridQuery) collectHitsWithMeta(
	ctx context.Context,
	qvec []float32,
	req RecallRequest,
) ([]Hit, RecallMeta, []RecallWarning, error) {
	// R11: anchor-scoped recall. When the request carries an anchor UID and
	// a navigator is wired, resolve the N-hop frontier and constrain the
	// candidate set pre-fusion via the AnchorFilter field (passed through
	// req). When the navigator is not wired, fall through to full recall.
	if req.Anchor != "" && h.navigator != nil {
		depth := req.AnchorDepth
		if depth <= 0 {
			depth = 1
		}
		if depth > MaxAnchorDepth {
			depth = MaxAnchorDepth
		}
		neighborUIDs, nerr := h.navigator.NeighborUIDs(ctx, req.Anchor, depth)
		if nerr != nil {
			logging.FromContext(ctx).Warn("kernel: hybrid: anchor neighbor lookup failed; falling back to full recall",
				zap.String("anchor", req.Anchor),
				zap.Error(nerr),
			)
			// Graceful degradation: full recall without the anchor constraint.
		} else {
			// Include the anchor entity itself in the frontier.
			frontier := make(map[string]struct{}, len(neighborUIDs)+1)
			frontier[req.Anchor] = struct{}{}
			for _, uid := range neighborUIDs {
				frontier[uid] = struct{}{}
			}
			req.anchorFrontier = frontier
		}
	}

	hits, meta, hitWarnings, err := h.collectHits(ctx, qvec, req)
	if err != nil {
		return nil, RecallMeta{}, nil, err
	}
	return hits, meta, hitWarnings, nil
}

// MaxAnchorDepth caps the AnchorDepth field in RecallRequest. It mirrors
// navigate.MaxHops so agents referencing either surface see the same ceiling.
const MaxAnchorDepth = 6

// collectHits runs the retrieval lanes for an already-embedded query and fuses
// them into the final ordered hit slice. Qvec is the query embedding, or nil
// when recall is degraded to FTS-only (the vector and name-vector lanes are
// then skipped — see embedQuery). MaybeRewrite/applyAsOf and the embed step run
// in recall() before this is called.
//
//nolint:gocognit,gocyclo // WIP: collectHits orchestrates the full hybrid pipeline; refactor staged for separate PR.
func (h *HybridQuery) collectHits(ctx context.Context, qvec []float32, req RecallRequest) ([]Hit, RecallMeta, []RecallWarning, error) {
	vecHits, ftsHits, graphHits, nameEmbHits, orphanEntityHits, laneWarnings, err := h.fetchAllCandidates(ctx, qvec, req)
	if err != nil {
		return nil, RecallMeta{}, nil, err
	}
	// R2: capture pre-fusion candidate counts before hydration/filtering reduces them.
	meta := RecallMeta{
		VecCandidates:     len(vecHits),
		FTSCandidates:     len(ftsHits),
		GraphCandidates:   len(graphHits),
		NameEmbCandidates: len(nameEmbHits),
	}

	// An empty episode-candidate union no longer short-circuits: the
	// document lane below can still produce hits (e.g. a Kinds filter of
	// [documentRevision], or a vault-only corpus). Episode scoring over an
	// empty keep map is a no-op.
	keep := map[string]hydratedRow{}
	allEpisodeUIDs := unionAllUIDs(vecHits, ftsHits, graphHits, nameEmbHits)
	if len(allEpisodeUIDs) > 0 {
		// Hydrate the union of UIDs to apply Kind / Confidence filters.
		rows, herr := h.hydrate(ctx, allEpisodeUIDs)
		if herr != nil {
			return nil, meta, laneWarnings, herr
		}

		// Spatial filter: when Near is set, narrow hydrated rows to those that
		// cite at least one entity within the radius of the resolved place.
		if req.Near != nil {
			rows, herr = h.applySpatialFilter(ctx, rows, req.Near)
			if herr != nil {
				return nil, meta, laneWarnings, herr
			}
		}

		// Anchor filter: when an anchor frontier is set (R11), narrow
		// hydrated rows to those that cite at least one entity in the
		// frontier. Mirrors the spatial filter's pre-fusion pattern.
		if len(req.anchorFrontier) > 0 {
			rows, herr = h.applyAnchorFilter(ctx, rows, req.anchorFrontier)
			if herr != nil {
				return nil, meta, laneWarnings, herr
			}
		}

		// Pre-fuse filtering with gap-free re-ranking within each source.
		keep = filterHydrated(rows, req)
	}
	vecRank := rerankSurvivorsVec(vecHits, keep)
	ftsRank := rerankSurvivorsFTS(ftsHits, keep)
	graphRank := rerankSurvivorsGeneric(graphHits, keep)
	nameEmbRank := rerankSurvivorsGeneric(nameEmbHits, keep)

	// Raw-score maps for the weighted fusion path. The semantic lane uses
	// cosine similarity derived from the vec0 L2 distance; the BM25 lane
	// uses mem0's query-length-adaptive sigmoid. Both are in [0, 1] so
	// they feed WeightedComponents directly without lane-specific
	// rescaling. Empty when fusionMode != "weighted" — the RRF path
	// doesn't consume them and the rerank loop above already produces
	// rank-based components.
	rawSemByUID := rawSemanticByUID(vecHits, keep)
	queryTokens := countQueryTokens(req.Query)
	rawBM25ByUID := rawBM25NormByUID(ftsHits, keep, queryTokens)

	// Passive-reinforcement strength (issue #231): one batched read of the
	// memory_strength accumulator for the surviving candidates, applied as a
	// multiplicative [≥1] tiebreaker in the build loop below. The anchor is
	// shared with the recency boost so both decays use one "now".
	anchor := recallAnchor(req)
	strengthUIDs := make([]string, 0, len(keep)+len(orphanEntityHits))
	for uid := range keep {
		strengthUIDs = append(strengthUIDs, uid)
	}
	// Bare-entity hits are reinforcement targets too (issue #231): a frequently-
	// recalled entity should outrank a stale equal-relevance peer, exactly like
	// episodes. Fold their UIDs into the same batched strength fetch so the bare-
	// entity build loop below can apply the multiplier.
	for _, evh := range orphanEntityHits {
		strengthUIDs = append(strengthUIDs, evh.ID)
	}
	strengthByUID, serr := h.fetchStrength(ctx, strengthUIDs)
	if serr != nil {
		// Reinforcement is an additive tiebreaker, never a hard recall
		// dependency. If the memory_strength accumulator is unavailable
		// (e.g. a partial/un-migrated schema), degrade to neutral multipliers
		// and still return base-ranked results rather than failing recall.
		logging.FromContext(ctx).Warn("kernel: hybrid: strength fetch failed; ranking without reinforcement", zap.Error(serr))
		strengthByUID = nil
	}

	// Fuse via weighted RRF and project episode Hits.
	scored := make([]Hit, 0, len(keep)+len(orphanEntityHits))
	for uid, row := range keep {
		vc, fc, gc, nc := rrfComponents(
			vecRank[uid], ftsRank[uid], graphRank[uid], nameEmbRank[uid],
			h.rrfK, h.graphWeight, h.nameEmbWeight,
		)
		// Phase 4: when fusionMode == "weighted", consume raw cosine
		// (vec) + sigmoid-normalized BM25 (fts) + per-lane entity ranks
		// via mem0 v3's adaptive sum/max_possible blend. The Components
		// struct below still records the RRF values for ablation reporting
		// alongside the raw scores.
		var fused float32
		rawSem := rawSemByUID[uid]
		rawBM25 := rawBM25ByUID[uid]
		if h.fusionMode == FusionModeWeighted {
			// Pre-fusion semantic gate: drop candidates whose raw cosine
			// falls below the configured threshold. Matches mem0's
			// scoring.py:102-103 — keyword overlap (FTS) and entity
			// rescue cannot resurrect a semantically-unrelated candidate.
			// When all candidates are filtered, `scored` ends empty and
			// the composer's empty-hits abstain gate fires the
			// AbstainSentinel downstream.
			if h.semanticGateThreshold > 0 && float64(rawSem) < h.semanticGateThreshold {
				continue
			}
			fused = WeightedComponents(rawSem, rawBM25, graphRank[uid], nameEmbRank[uid])
		} else {
			fused = vc + fc + gc + nc
		}
		// Phase 3: per-EpisodeClass multiplier applied AFTER fusion but
		// BEFORE the recency multiplier and MMR. Order matters because
		// all three multipliers must compose multiplicatively in [0, 1]
		// — applying typeWeight first keeps semantic facts above
		// episodic chatter even when the latter is more recent. When
		// typeWeights is unset, every class scores at its raw fused
		// value (pre-Phase-3 behavior).
		if len(h.typeWeights) > 0 {
			class := row.class
			if class == "" {
				class = classEpisodic
			}
			weight, ok := h.typeWeights[class]
			if !ok {
				// Unknown class → episodic default. Treat as a
				// data-quality issue rather than a hard failure;
				// the migration's CHECK constraint already keeps
				// new writes in the legal set, so this branch
				// only fires for legacy rows or schema drift.
				weight = h.typeWeights[classEpisodic]
			}
			fused *= float32(weight)
		}
		// Passive reinforcement (issue #231): a frequently- and recently-
		// recalled memory outranks a stale equal-relevance peer. Composed
		// multiplicatively in [≥1] AFTER the type weight, like the recency
		// boost, so it is a tiebreaker rather than a dominant signal. The
		// decay term is computed from the stored last_accessed_at at query
		// time (never stored), keeping the projection replay-stable.
		strengthMult, strength := h.strengthMultiplier(strengthByUID[uid], anchor)
		fused *= strengthMult
		scored = append(scored, Hit{
			HitType:     HitTypeEpisode,
			UID:         uid,
			Kind:        EventKind(row.kind),
			Text:        row.text,
			Snippet:     pickSnippet(uid, row, ftsHits, h.snippetRunes),
			Confidence:  row.confidence,
			CreateTime:  row.createTime,
			EventSource: row.source,
			FusedScore:  fused,
			Components: ScoreComponents{
				Vector:      vc,
				FTS:         fc,
				Graph:       gc,
				NameEmb:     nc,
				RawSemantic: rawSem,
				RawBM25:     rawBM25,
				Strength:    strength,
			},
			// Cite the episode under its get-able projection name. The episode
			// uid is its source-event uid (episodes project 1:1 from their
			// remember/observation event), so episodes/{uid} resolves via get —
			// unlike a bare events/{uid}, which no read tool accepts. Keeps the
			// episode lane consistent with the derived/entity lanes' citations.
			Citations: []string{"episodes/" + uid},
		})
	}

	// Entity hits: bare entities matched by name-vector that have no
	// associated episode rows.  Each is emitted as a HitTypeEntity hit.
	// They bypass the episode-hydration + filterHydrated pipeline, so we
	// apply the equivalent filters here before appending to scored.
	//
	//   nameEmbWeight=0 disables the name-emb lane entirely, so bare entity
	//   hits must be suppressed too.
	//
	//   AsOf: an entity created after req.AsOf is not visible at that point
	//   in transaction time, so drop it.
	//
	//   Near: when a spatial filter is active, bare entities that are not
	//   in the nearby set must be excluded.  We resolve the nearby set only
	//   when orphan hits are present and a Near filter is requested.
	//
	//   Fusion mode: in weighted mode the cosine similarity is in [0,1] and
	//   comparable to episode FusedScores.  In RRF mode, episode scores are
	//   ~1/(k+rank) ≪ 1; we use the same formula for bare entities (rank by
	//   their position in the name-vector hit list) so they compete fairly.
	if len(orphanEntityHits) > 0 && h.nameEmbWeight > 0 {
		entityMeta, metaErr := h.hydrateEntityMeta(ctx, orphanEntityHits)
		if metaErr != nil {
			return nil, meta, laneWarnings, metaErr
		}

		// Build nearby-UID allowlist when a spatial Near filter is active.
		var nearbyEntityUIDs map[string]struct{}
		if req.Near != nil {
			nearbyEntityUIDs, err = h.resolveNearbyEntityUIDs(ctx, req.Near)
			if err != nil {
				return nil, meta, laneWarnings, err
			}
		}

		// Build kind allowlist once, mirroring filterHydrated's Kinds predicate.
		var kindSet map[string]struct{}
		if len(req.Kinds) > 0 {
			kindSet = make(map[string]struct{}, len(req.Kinds))
			for _, k := range req.Kinds {
				kindSet[string(k)] = struct{}{}
			}
		}

		for rank, evh := range orphanEntityHits {
			meta, ok := entityMeta[evh.ID]
			if !ok {
				// Entity row missing (deleted between vector search and here);
				// skip silently, same policy as hydrate for episodes.
				continue
			}

			// Kinds filter: when a kind restriction is active, only emit entity
			// hits whose kind is in the allowed set (mirrors filterHydrated).
			if kindSet != nil {
				if _, inSet := kindSet[meta.kind]; !inSet {
					continue
				}
			}

			// MinConfidence filter: drop low-confidence entities (mirrors filterHydrated).
			if meta.confidence < req.MinConfidence {
				continue
			}

			// Since / Until / AsOf time-range filters, all using the entity's
			// create_time.  When create_time is absent we skip the filter
			// conservatively (keep visible rather than silently drop).
			if meta.createTimeSet {
				if !req.Since.IsZero() && meta.createTime.Before(req.Since) {
					continue
				}
				if !req.Until.IsZero() && !meta.createTime.Before(req.Until) {
					continue
				}
				if req.AsOf != nil && meta.createTime.After(*req.AsOf) {
					continue
				}
			}

			// Spatial filter: skip entities outside the requested radius.
			if nearbyEntityUIDs != nil {
				if _, inNearby := nearbyEntityUIDs[evh.ID]; !inNearby {
					continue
				}
			}

			cosSim := CosineSimilarityFromDistance(evh.Score)
			var fusedScore float32
			var nc float32
			if h.fusionMode == FusionModeWeighted {
				// Weighted mode: cosine similarity in [0,1] is comparable to
				// episode FusedScores.
				// Pre-fusion semantic gate: a bare entity match cannot rescue a
				// semantically-unrelated candidate either, so apply the same
				// floor episode hits enforce above (line ~736).
				if h.semanticGateThreshold > 0 && float64(cosSim) < h.semanticGateThreshold {
					continue
				}
				fusedScore = cosSim
				nc = cosSim
			} else {
				// RRF mode: use the same 1/(k+rank) formula as episode name-emb
				// contributions so bare entity hits compete on equal footing.
				nc = float32(h.nameEmbWeight) / float32(h.rrfK+rank+1)
				fusedScore = nc
			}
			// Carry the entity's own metadata onto the hit: without Kind,
			// projectHit defaults the empty kind to "episode"; without
			// CreateTime, it formats the zero time as year 0001. createTime
			// is gated on createTimeSet so an unparseable/empty column leaves
			// the zero value rather than a bogus epoch.
			createTime := time.Time{}
			if meta.createTimeSet {
				createTime = meta.createTime
			}
			// Passive reinforcement (issue #231): a frequently-recalled bare
			// entity outranks a stale equal-relevance peer, composed
			// multiplicatively in [≥1] like the episode and derived-memory lanes.
			strengthMult, strength := h.strengthMultiplier(strengthByUID[evh.ID], anchor)
			fusedScore *= strengthMult
			scored = append(scored, Hit{
				HitType:    HitTypeEntity,
				UID:        evh.ID,
				Kind:       EventKind(meta.kind),
				Text:       meta.displayName,
				Snippet:    meta.displayName,
				Confidence: meta.confidence,
				CreateTime: createTime,
				FusedScore: fusedScore,
				Components: ScoreComponents{
					NameEmb:     nc,
					RawSemantic: cosSim,
					Strength:    strength,
				},
				Citations: []string{"entities/" + evh.ID},
				Entities: []EntityRef{{
					UID:         evh.ID,
					Kind:        meta.kind,
					DisplayName: meta.displayName,
				}},
			})
		}
	}

	// Document lane: documents co-rank with episodes through the same fusion
	// math (same WeightedComponents / rrfComponents / semantic gate), merged
	// here so the whole sort → recency → MMR → rerank → limit pipeline below
	// sees ONE candidate pool — co-ranked, not two searches stapled together.
	if documentLaneEnabled(req.Kinds) {
		docHits, derr := h.collectDocumentHits(ctx, qvec, req, vecHits, ftsHits)
		if derr != nil {
			return nil, meta, laneWarnings, derr
		}
		scored = append(scored, docHits...)
	}

	// Derived-memory lanes (issue #231): hot synthesized insights and
	// consolidated memories co-rank with episodes through the same
	// sort → recency → MMR → rerank → limit pipeline. Demoted insights are
	// filtered out in SQL (state != 'hot'), so they leave hot recall.
	derivedHits, dmErr := h.collectDerivedMemoryHits(ctx, qvec, req, anchor)
	if dmErr != nil {
		return nil, meta, laneWarnings, dmErr
	}
	scored = append(scored, derivedHits...)

	if len(scored) == 0 {
		return []Hit{}, meta, laneWarnings, nil
	}

	sortHitsByScore(scored)

	scored = h.applyRRFMinScore(scored)
	// Gaussian recency multiplier between RRF-floor and MMR. Anchored
	// at req.CurrentDate when the caller supplied a "now" reference;
	// otherwise the wall clock. Re-sort by FusedScore after — the
	// multiplier preserves order for hits with identical CreateTime but
	// can swap two near-tied hits whose ages differ.
	if h.recencyBoost > 0 {
		// Shares the anchor computed for the strength multiplier above so both
		// query-time decays use one "now".
		scored = applyRecencyBoost(scored, anchor, h.recencyBoost, h.recencyHalflifeDays)
		sortHitsByScore(scored)
	}
	// MMR pass between RRF-floor and reranker. The reranker (LLM call)
	// is the expensive step; running it over a diversified candidate
	// set is cheaper and produces better answers when near-duplicates
	// would otherwise occupy multiple slots in the top-K.
	if h.mmrLambda > 0 {
		// Pool size is bounded by candidateK upstream; pick top-N after
		// MMR so we still hand the reranker a non-trivial set to rank.
		mmrK := len(scored)
		scored = applyMMR(scored, h.mmrLambda, mmrK)
	}
	scored, rerankErr := h.applyReranker(ctx, req.Query, scored)
	if rerankErr != nil {
		return nil, meta, laneWarnings, rerankErr
	}

	limit := req.Limit
	if limit <= 0 {
		limit = h.defaultLimit
	}
	if len(scored) > limit {
		scored = scored[:limit]
	}
	if err := h.markSuperseded(ctx, scored); err != nil {
		return nil, meta, laneWarnings, err
	}
	if err := h.attachEntities(ctx, scored); err != nil {
		return nil, meta, laneWarnings, err
	}
	if err := h.attachFreshness(ctx, scored); err != nil {
		return nil, meta, laneWarnings, err
	}
	if err := h.attachDocumentEntities(ctx, scored, req.At, req.AsOf); err != nil {
		return nil, meta, laneWarnings, err
	}
	// Defensive final sort: re-apply FusedScore desc / UID asc after all
	// post-fusion steps (markSuperseded, attachEntities, attachDocumentEntities)
	// have run.  Any intermediate step that mutates FusedScore without
	// re-sorting (e.g. applyRecencyBoost when a future caller enables it
	// mid-pipeline) would silently expose the non-deterministic map-iteration
	// order from the scored-slice build loop.  MMR and reranker intentionally
	// reorder; this final sort must not clobber their output.
	if h.mmrLambda == 0 && (h.reranker == nil || !h.rerankEnabled) {
		sort.SliceStable(scored, func(i, j int) bool {
			if scored[i].FusedScore != scored[j].FusedScore {
				return scored[i].FusedScore > scored[j].FusedScore
			}
			return scored[i].UID < scored[j].UID
		})
	}
	return scored, meta, laneWarnings, nil
}

// markSuperseded sets Hit.Superseded=true for each hit whose episode has
// at least one relation with valid_until set (i.e. the fact has been
// invalidated by a later remember). Runs one batch query against
// relations.source_event_uid; cost is one indexed lookup on the
// post-limit shortlist. Caller is the composer's "currency" rule: a
// supersede-aware compose-v1 prefers non-superseded episodes when the
// question is phrased in the present tense.
//
// On an empty input the function is a no-op (avoids building a
// zero-arg IN clause).
func (h *HybridQuery) markSuperseded(ctx context.Context, hits []Hit) error {
	if len(hits) == 0 {
		return nil
	}
	uids := make([]string, len(hits))
	for i, hit := range hits {
		uids[i] = hit.UID
	}
	placeholders, args := inClauseStrings(uids)
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	q := fmt.Sprintf( //nolint:gosec // G201/G202: only "?" placeholders splice in via inClauseStrings
		`SELECT DISTINCT source_event_uid
		 FROM relations
		 WHERE source_event_uid IN (%s)
		   AND valid_until IS NOT NULL
		   AND %s`,
		placeholders, scopePred,
	)
	args = append(args, scopeArgs...)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("kernel: hybrid: markSuperseded: %w", err)
	}
	defer func() { _ = rows.Close() }()
	superseded := make(map[string]struct{})
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return fmt.Errorf("kernel: hybrid: markSuperseded scan: %w", err)
		}
		superseded[uid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("kernel: hybrid: markSuperseded rows: %w", err)
	}
	for i := range hits {
		if _, ok := superseded[hits[i].UID]; ok {
			hits[i].Superseded = true
		}
	}
	return nil
}

// freshnessRow is the raw external-source freshness read off the source_refs row
// grounding a recall hit. AttachFreshness fills it; the MCP recall tool layer
// classifies it against the per-connector TTL config (the kernel stays
// config-free and carries only the raw inputs).
type freshnessRow struct {
	fetchTime   time.Time
	connector   string
	externalURL string
	tombstone   string
}

// attachFreshness populates the inline freshness fields (FetchTime,
// SourceConnector, ExternalURL, Tombstone) on each hit grounded in an external
// source, so the MCP recall tool can stamp a freshnessStatus the agent uses to
// decide whether to trust the cached summary or re-fetch live (#C captureExternal).
//
// It mirrors SubjectsWithPrimarySource's two legs. A hit is grounded either
// directly (it is itself the subject of a source_ref row: the captureExternal
// summary episode, whose SourceRef.subjectUid is the episode) or transitively
// (via an active has_primary_source edge to an external_ref that itself carries a
// source_ref, a reference()-style pointer the hit cites). The direct leg is the
// more specific grounding, so it is applied last and wins; within a leg the
// latest fetch_time wins (rows are ordered ascending and overwrite).
//
// One indexed batch query per leg over the post-limit shortlist, the same cost
// shape as markSuperseded/attachEntities. No-op on empty input.
func (h *HybridQuery) attachFreshness(ctx context.Context, hits []Hit) error {
	if len(hits) == 0 {
		return nil
	}
	uids := make([]string, len(hits))
	for i := range hits {
		uids[i] = hits[i].UID
	}
	placeholders, inArgs := inClauseStrings(uids)
	ss := scopesFromContext(ctx)
	scopePred, scopeArgs := ss.Predicate("")
	srScopePred, srScopeArgs := ss.Predicate("sr")
	relScopePred, relScopeArgs := ss.Predicate("r")

	fresh := make(map[string]freshnessRow, len(hits))

	// Leg 2 (transitive): hit --has_primary_source--> external_ref <-- source_ref.
	// Run first so the more-specific direct leg below overwrites it.
	relQ := fmt.Sprintf(
		`SELECT r.subject_uid, sr.fetch_time,
			COALESCE(sr.connector, ''), COALESCE(sr.external_url, ''), COALESCE(sr.tombstone, '')
		 FROM relations r
		 JOIN source_refs sr ON sr.subject_uid = r.object_uid
		 WHERE r.predicate = ?
		   AND (r.valid_until IS NULL OR r.valid_until = '')
		   AND r.subject_uid IN (%s)
		   AND %s AND %s
		 ORDER BY sr.fetch_time`,
		placeholders, relScopePred, srScopePred,
	)
	relArgs := append([]any{hasPrimarySourcePredicate}, inArgs...)
	relArgs = append(relArgs, relScopeArgs...)
	relArgs = append(relArgs, srScopeArgs...)
	if err := h.collectFreshness(ctx, "relation", relQ, relArgs, fresh); err != nil {
		return err
	}

	// Leg 1 (direct): hit is itself the subject of a source_ref.
	srQ := fmt.Sprintf(
		`SELECT subject_uid, fetch_time,
			COALESCE(connector, ''), COALESCE(external_url, ''), COALESCE(tombstone, '')
		 FROM source_refs
		 WHERE subject_uid IN (%s) AND %s
		 ORDER BY fetch_time`,
		placeholders, scopePred,
	)
	srArgs := append(append([]any{}, inArgs...), scopeArgs...)
	if err := h.collectFreshness(ctx, "source_ref", srQ, srArgs, fresh); err != nil {
		return err
	}

	for i := range hits {
		if fr, ok := fresh[hits[i].UID]; ok {
			hits[i].FetchTime = fr.fetchTime
			hits[i].SourceConnector = fr.connector
			hits[i].ExternalURL = fr.externalURL
			hits[i].Tombstone = fr.tombstone
		}
	}
	return nil
}

// collectFreshness runs query and folds each row into fresh keyed by subject
// uid, parsing fetch_time as RFC3339. Rows are expected in ascending fetch_time
// order so a later (fresher) row overwrites an earlier one for the same subject.
// `what` names the leg for error context.
func (h *HybridQuery) collectFreshness(ctx context.Context, what, query string, args []any, fresh map[string]freshnessRow) error {
	rows, err := h.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("kernel: hybrid: attachFreshness %s query: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid, fetchTime, connector, externalURL, tombstone string
		if err := rows.Scan(&uid, &fetchTime, &connector, &externalURL, &tombstone); err != nil {
			return fmt.Errorf("kernel: hybrid: attachFreshness %s scan: %w", what, err)
		}
		// A malformed fetch_time leaves the zero time, which the recall tool
		// classifies as unknown rather than failing the whole recall.
		ft, err := time.Parse(time.RFC3339Nano, fetchTime)
		if err != nil {
			ft = time.Time{}
		}
		fresh[uid] = freshnessRow{
			fetchTime:   ft,
			connector:   connector,
			externalURL: externalURL,
			tombstone:   tombstone,
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("kernel: hybrid: attachFreshness %s rows: %w", what, err)
	}
	return nil
}

// attachEntities populates Hit.Entities with the graph anchors each hit is
// "about", recovered by reverse-joining the mentions table on the episode UID
// and hydrating display data from entities. This is the seed of the
// seed-then-traverse model: one recall hands the agent the entity UIDs it then
// navigates from (neighbors/relations/get) with no further LLM in the loop.
//
// Cost is one indexed batch query (idx_mentions_source_event_uid) over the
// post-limit shortlist — the same shape as markSuperseded — so it is cheap
// enough to always run, independent of the graph-expansion lanes. Mentions
// with a NULL entity_uid (unresolved surface forms) are skipped; duplicate
// mentions of the same entity within one episode collapse to a single ref.
//
// On an empty input the function is a no-op (avoids a zero-arg IN clause).
func (h *HybridQuery) attachEntities(ctx context.Context, hits []Hit) error {
	if len(hits) == 0 {
		return nil
	}
	// Entity and derived-memory hits already carry their Entities anchor: an
	// entity hit's own self-reference (from the build loop), and an insight or
	// consolidated-memory hit's seed/cited entity (from finalizeDerivedHits).
	// None of them have a mentions row keyed by their own UID, so passing them
	// through the mentions.source_event_uid join finds nothing and would
	// replace the pre-populated anchor with [], breaking the schema promise.
	// Pre-seed the byEpisode map from these hits so the assignment loop below
	// keeps their anchors intact.
	byEpisode := make(map[string][]EntityRef, len(hits))
	seen := make(map[string]struct{}) // "episodeUID\x00entityUID"
	episodeUIDs := make([]string, 0, len(hits))
	for _, hit := range hits {
		switch hit.HitType {
		case HitTypeEntity, HitTypeInsight, HitTypeConsolidated:
			// Preserve the pre-populated anchor; normalize a missing anchor to a
			// non-nil empty slice so the wire contract still marshals [] not null.
			refs := hit.Entities
			if refs == nil {
				refs = []EntityRef{}
			}
			byEpisode[hit.UID] = refs
		default:
			episodeUIDs = append(episodeUIDs, hit.UID)
		}
	}

	if len(episodeUIDs) == 0 {
		// All hits are entity/derived hits whose anchors are already attached;
		// there is nothing to reverse-join from the mentions table.
		return nil
	}

	placeholders, args := inClauseStrings(episodeUIDs)
	// Scope BOTH the mention (m, the statement proving the episode→entity link)
	// and the hydrated entity (e): a team-scoped mention must not decorate an
	// in-scope episode hit with an entity anchor, even when that entity's own
	// row is visible in the default span.
	ss := scopesFromContext(ctx)
	scopePredM, scopeArgsM := ss.Predicate("m")
	scopePredE, scopeArgsE := ss.Predicate("e")
	q := fmt.Sprintf( //nolint:gosec // G201/G202: only "?" placeholders splice in via inClauseStrings; scope is constant fragments + bound ? params; all values parameterized
		// The inner JOIN already drops mentions whose entity_uid is NULL
		// (an unresolved surface form), so no explicit NOT NULL is needed.
		// e.uid is the final ORDER BY key so the dedup below picks a stable
		// representative regardless of display-name collisions.
		`SELECT m.source_event_uid, e.uid, e.kind, e.display_name
		 FROM mentions m
		 JOIN entities e ON e.uid = m.entity_uid
		 WHERE m.source_event_uid IN (%s)
		   AND %s
		   AND %s
		 ORDER BY m.source_event_uid, e.display_name, e.uid`,
		placeholders, scopePredM, scopePredE,
	)
	args = append(args, scopeArgsM...)
	args = append(args, scopeArgsE...)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("kernel: hybrid: attachEntities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var episodeUID string
		var ref EntityRef
		if err := rows.Scan(&episodeUID, &ref.UID, &ref.Kind, &ref.DisplayName); err != nil {
			return fmt.Errorf("kernel: hybrid: attachEntities scan: %w", err)
		}
		key := episodeUID + "\x00" + ref.UID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		byEpisode[episodeUID] = append(byEpisode[episodeUID], ref)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("kernel: hybrid: attachEntities rows: %w", err)
	}
	for i := range hits {
		if refs, ok := byEpisode[hits[i].UID]; ok {
			hits[i].Entities = refs
		} else {
			// Normalize to a non-nil empty slice so Hit.Entities has a single
			// representation for "no resolved anchors" — matching the recall
			// wire contract, which always marshals [] rather than null.
			hits[i].Entities = []EntityRef{}
		}
	}
	return nil
}

// applyRRFMinScore drops long-tail hits below the configured fused-score
// floor. Returns the input unchanged when rrfMinScore is 0 (the default).
// Extracted from collectHits to keep its cognitive complexity inside the
// linter threshold.
func (h *HybridQuery) applyRRFMinScore(scored []Hit) []Hit {
	if h.rrfMinScore <= 0 {
		return scored
	}
	n := 0
	for _, hit := range scored {
		if hit.FusedScore >= h.rrfMinScore {
			scored[n] = hit
			n++
		}
	}
	return scored[:n]
}

// applyReranker runs the optional post-fusion Reranker. When the reranker
// is nil this is a no-op.
//
// Error policy: the reranker is an optional refinement. Per its godoc it
// returns the original ordering alongside any error so the caller can fall
// back to the RRF ranking rather than fail recall. We honor that: a
// malformed model response (short permutation, duplicate index, etc.) or a
// transient chat failure warn-logs and degrades to the original ordering.
//
// The one exception is context cancellation, which always propagates so
// the caller's cancel is honored.
func (h *HybridQuery) applyReranker(ctx context.Context, query string, scored []Hit) ([]Hit, error) {
	if h.reranker == nil || !h.rerankEnabled {
		return scored, nil
	}
	reranked, err := h.reranker.Rerank(ctx, query, scored)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return scored, fmt.Errorf("kernel: hybrid: rerank: %w", ctxErr)
		}
		logging.FromContext(ctx).Warn("kernel: hybrid: rerank degraded to RRF ordering",
			zap.Error(err),
			zap.Int("candidates", len(scored)),
		)
		return scored, nil
	}
	return reranked, nil
}

// fetchAllCandidates runs all four retrieval lanes concurrently: vector
// (episode summary embeddings), FTS (BM25), graph (1-hop/2-hop entity
// traversal returning episode UIDs), and name-vector (entity name embeddings
// joined back to episodes).
//
// The graph + name-vector-to-episode lanes are gated by !req.NoGraph &&
// h.graph != nil.  The orphan-entity detection (name-vector hits with no
// episode rows) runs whenever the VectorConn satisfies nameVectorSearcher,
// regardless of whether a GraphConn is wired, so bare entities are visible
// in recall results even in graph-less configurations.
//
// OrphanEntityHits are entity VectorHits from the name-vector lane that had no
// associated episode rows; collectHits emits these as HitTypeEntity so
// directly-created entities surface in recall results.
func (h *HybridQuery) fetchAllCandidates(
	ctx context.Context,
	qvec []float32,
	req RecallRequest,
) (vecHits []VectorHit, ftsHits []FTSHit, graphHits []string, nameEmbHits []string, orphanEntityHits []VectorHit, warnings []RecallWarning, err error) {
	var ftsWarnings []RecallWarning
	vecHits, ftsHits, ftsWarnings, err = h.fetchVecAndFTS(ctx, qvec, req)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	warnings = append(warnings, ftsWarnings...)

	// Name-vector lane: always run when the VectorConn satisfies
	// nameVectorSearcher and the caller has not disabled graph lanes.
	// Produces both episode UIDs (for fusion ranking) and orphan entity hits
	// (entities with no episodes, emitted as HitTypeEntity).  We run this
	// here — outside the graph gate — so bare-entity recall works even when
	// no GraphConn is wired.
	var precomputedNameEmbHits []string
	// The name-vector lane needs the query embedding; skip it (and orphan-
	// entity detection) entirely when degraded to FTS-only (qvec == nil).
	if qvec != nil && !req.NoGraph {
		if nvs, ok := h.vec.(nameVectorSearcher); ok {
			precomputedNameEmbHits, orphanEntityHits, err = h.fetchNameVectorCandidates(ctx, nvs, qvec, h.candidateK)
			if err != nil {
				return nil, nil, nil, nil, nil, nil, fmt.Errorf("kernel: hybrid: name-vector candidates: %w", err)
			}
		}
	}

	if req.NoGraph || h.graph == nil {
		return vecHits, ftsHits, nil, precomputedNameEmbHits, orphanEntityHits, warnings, nil
	}

	now := time.Now().UTC()
	if req.At != nil {
		now = req.At.UTC()
	}
	var asOf *time.Time
	if req.AsOf != nil {
		u := req.AsOf.UTC()
		asOf = &u
	}

	graphHits, err = h.fetchGraphHitsOnly(ctx, qvec, vecHits, ftsHits, now, asOf)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	return vecHits, ftsHits, graphHits, precomputedNameEmbHits, orphanEntityHits, warnings, nil
}

// fetchVecAndFTS runs the vector and FTS lanes concurrently.
func (h *HybridQuery) fetchVecAndFTS(
	ctx context.Context,
	qvec []float32,
	req RecallRequest,
) (vecHits []VectorHit, ftsHits []FTSHit, warnings []RecallWarning, err error) {
	k := h.candidateK
	// Scopes come from the recall context (withScopes); the sidecar filter is
	// defense-in-depth — the SQLite join-back remains the authoritative gate.
	vecFilter := VectorFilter{MinTime: req.Since, MaxTime: req.Until, Scopes: scopesFromContext(ctx)}
	verbatimQuery := buildVerbatimFTSQuery(req.Query)
	prefixQuery := escapeFTSQuery(req.Query)

	var mu sync.Mutex // protects warnings from concurrent goroutine writes
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		// Degraded recall: a nil query embedding means the embedder was
		// unavailable, so the dense-vector lane is skipped and only FTS
		// (and graph, seeded from FTS) contributes (#236).
		if qvec == nil {
			return nil
		}
		hits, ferr := h.vec.Search(gctx, qvec, k, vecFilter)
		if ferr != nil {
			return fmt.Errorf("vec search: %w", ferr)
		}
		vecHits = hits
		return nil
	})

	g.Go(func() error {
		hits, w, ferr := h.runFTSLane(gctx, verbatimQuery, prefixQuery, k, req)
		if ferr != nil {
			return ferr
		}
		ftsHits = hits
		if len(w) > 0 {
			mu.Lock()
			warnings = append(warnings, w...)
			mu.Unlock()
		}
		return nil
	})

	if werr := g.Wait(); werr != nil {
		return nil, nil, nil, fmt.Errorf("kernel: hybrid: %w", werr)
	}
	return vecHits, ftsHits, warnings, nil
}

// runFTSLane dispatches to the time-bounded FTS path when both bounds are
// present and the connection supports it; otherwise runs the plain search.
//
// R3 fuzzy fallback: when verbatimQuery returns fewer than
// h.fuzzyFallbackThreshold hits, the lane re-runs with the prefix-expanded
// prefixQuery and appends warncode.CodeRecallFuzzyFallback to the warnings.
func (h *HybridQuery) runFTSLane(
	ctx context.Context,
	verbatimQuery, prefixQuery string,
	k int,
	req RecallRequest,
) ([]FTSHit, []RecallWarning, error) {
	// Choose the effective query: verbatim first when both are non-empty;
	// fall back to prefix if verbatim is blank (i.e. all short tokens).
	effectiveQuery := verbatimQuery
	if effectiveQuery == "" {
		effectiveQuery = prefixQuery
	}
	if effectiveQuery == "" {
		return nil, nil, nil
	}

	timeBounded := !req.Since.IsZero() || !req.Until.IsZero()
	runSearch := func(ctx context.Context, q string) ([]FTSHit, error) {
		if ranger, ok := h.fts.(ftsTimeRanger); ok && timeBounded {
			hits, ferr := ranger.SearchInTimeRange(ctx, q, k, req.Since, req.Until)
			if ferr != nil {
				return nil, fmt.Errorf("fts search (time-bounded): %w", ferr)
			}
			return hits, nil
		}
		hits, ferr := h.fts.Search(ctx, q, k)
		if ferr != nil {
			return nil, fmt.Errorf("fts search: %w", ferr)
		}
		return hits, nil
	}

	hits, ferr := runSearch(ctx, effectiveQuery)
	if ferr != nil {
		return nil, nil, ferr
	}

	// R3: when the verbatim query returned too few hits AND a richer prefix
	// query is available, retry. Only fire the fallback when it would actually
	// differ from the verbatim query (i.e. the caller supplied both).
	if len(hits) < h.fuzzyFallbackThreshold &&
		verbatimQuery != "" &&
		prefixQuery != "" &&
		verbatimQuery != prefixQuery {
		fallbackHits, ferr2 := runSearch(ctx, prefixQuery)
		if ferr2 != nil {
			return nil, nil, ferr2
		}
		if len(fallbackHits) > len(hits) {
			hits = fallbackHits
			warn := RecallWarning{
				Code:    warncode.CodeRecallFuzzyFallback,
				Message: "FTS verbatim match returned too few results; retried with prefix expansion",
			}
			return hits, []RecallWarning{warn}, nil
		}
	}

	return hits, nil, nil
}

// buildVerbatimFTSQuery builds a FTS5 MATCH expression for exact (non-prefix)
// token matching. Like escapeFTSQuery it sanitises punctuation, but it never
// appends the trailing `*` wildcard so the query matches whole words only.
// Tokens shorter than 3 runes are kept as-is (exact); 3+ rune tokens are
// quoted but NOT prefixed — this is the "verbatim first" pass for R3.
func buildVerbatimFTSQuery(q string) string {
	if q == "" {
		return ""
	}
	var tokens []string
	var b strings.Builder
	for _, r := range q {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
			continue
		}
		if b.Len() > 0 {
			tokens = append(tokens, `"`+b.String()+`"`)
			b.Reset()
		}
	}
	if b.Len() > 0 {
		tokens = append(tokens, `"`+b.String()+`"`)
	}
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " OR ")
}

// fetchGraphHitsOnly runs the graph-traversal lane from the vector+FTS seed
// set. AsOf, when non-nil, further restricts graph relations to those whose
// createTime (transaction time) is on-or-before that instant; nil means "no
// transaction-time bound".
//
// The name-vector lane is handled separately in fetchAllCandidates so that
// orphan-entity detection fires regardless of whether a GraphConn is wired.
func (h *HybridQuery) fetchGraphHitsOnly(
	ctx context.Context,
	_ []float32,
	vecHits []VectorHit,
	ftsHits []FTSHit,
	now time.Time,
	asOf *time.Time,
) (graphHits []string, err error) {
	seedUIDs := collectEpisodeUIDs(vecHits, ftsHits, h.candidateK)
	entityUIDs, lookupErr := h.entityUIDsForEpisodes(ctx, seedUIDs)
	if lookupErr != nil {
		return nil, fmt.Errorf("kernel: hybrid: entity lookup: %w", lookupErr)
	}
	graphHits, err = h.fetchGraphCandidates(ctx, entityUIDs, now, asOf)
	if err != nil {
		return nil, fmt.Errorf("kernel: hybrid: graph candidates: %w", err)
	}
	return graphHits, nil
}

// fetchGraphCandidates traverses the graph from the given entity UIDs, finding
// 1-hop neighbours via RELATES_TO edges. Returns the episode UIDs that cite
// those neighbour entities (via MENTIONS edges). When the 1-hop entity count
// is below twoHopFloor, expands to 2-hop with the same edge filter.
//
// The temporal filter ((validFrom IS NULL OR validFrom <= now) AND
// (validUntil IS NULL OR now < validUntil)) is applied inside the Cypher
// query, so relations that were true in the past but have since been
// superseded do not pollute recall results. NULL validFrom is treated as
// "valid since beginning of time" because the extractor frequently has no
// way to set a concrete lower bound (it only fires when the source text
// supplies "since YEAR" or similar), and an exact-NULL filter would
// otherwise drop the majority of resolver-emitted relations from graph
// traversal, killing the 1-hop and 2-hop lanes entirely.
func (h *HybridQuery) fetchGraphCandidates(ctx context.Context, entityUIDs []string, now time.Time, asOf *time.Time) ([]string, error) {
	if len(entityUIDs) == 0 {
		return nil, nil
	}

	hopEntities, err := h.graphHop(ctx, entityUIDs, 1, now, asOf)
	if err != nil {
		return nil, fmt.Errorf("1-hop traversal: %w", err)
	}

	if len(hopEntities) < h.twoHopFloor {
		hop2Entities, err := h.graphHop(ctx, entityUIDs, 2, now, asOf)
		if err != nil {
			return nil, fmt.Errorf("2-hop traversal: %w", err)
		}
		// Merge 2-hop into 1-hop (deduplicated).
		seen := make(map[string]struct{}, len(hopEntities))
		for _, uid := range hopEntities {
			seen[uid] = struct{}{}
		}
		for _, uid := range hop2Entities {
			if _, dup := seen[uid]; dup {
				continue
			}
			seen[uid] = struct{}{}
			hopEntities = append(hopEntities, uid)
		}
	}

	return h.episodesForEntities(ctx, hopEntities)
}

// graphHop performs a depth-limited graph traversal from the given seed entity
// UIDs. For depth=1, returns direct RELATES_TO neighbours via a bidirectional
// UNION. For depth=2, returns entities reachable in exactly two hops via a
// 4-way UNION that covers every combination of outgoing/incoming edges,
// matching the 1-hop lane's symmetry. The edge allowlist (h.edgeAllow) is
// enforced in the WHERE clause; an empty allowlist means all predicates. The
// temporal validity filter is always applied. When asOf is non-nil, the
// relation's transaction-time (createTime) is also constrained to be on-or-
// before asOf so a query like "what did we know on 2025-01-01?" excludes
// relations learned after that date even if their valid-time overlaps it.
func (h *HybridQuery) graphHop(ctx context.Context, seedUIDs []string, depth int, now time.Time, asOf *time.Time) (uids []string, err error) {
	if len(seedUIDs) == 0 {
		return nil, nil
	}

	// LadybugDB binds named parameters via map[string]any; we expand the UID
	// list into individual parameter names (uid0, uid1, ...) because LadybugDB
	// does not support list-valued parameters.
	//
	// Edge allowlist: when non-empty, the WHERE clause includes a predicate
	// filter using IN with individual string parameters (pred0, pred1, ...).
	// An empty allowlist means all predicates are followed (no filter applied).

	nowStr := now.Format(time.RFC3339Nano)
	args := map[string]any{"now": nowStr}
	if asOf != nil {
		args["asOf"] = asOf.Format(time.RFC3339Nano)
	}

	asOfClause := func(rel string) string {
		if asOf == nil {
			return ""
		}
		return fmt.Sprintf("  AND (%s.createTime IS NULL OR %s.createTime <= $asOf)\n", rel, rel)
	}

	// Build uid parameter names.
	uidParams := make([]string, len(seedUIDs))
	for i, uid := range seedUIDs {
		key := fmt.Sprintf("uid%d", i)
		args[key] = uid
		uidParams[i] = "$" + key
	}
	uidList := strings.Join(uidParams, ", ")

	var cypher string
	switch depth {
	case 1:
		// LadybugDB requires directed arrows for typed relationship binding;
		// an undirected pattern leaves the binder unable to introduce r into
		// scope. The directed form covers both (subject->object) and
		// (object->subject) by issuing the same query twice in a UNION,
		// because RELATES_TO has a fixed FROM Entity TO Entity orientation
		// and the resolver always writes the s->o direction.
		edgeFilter := h.buildEdgeFilterClause(args, "r")
		asOfR := asOfClause("r")
		cypher = fmt.Sprintf(`
			MATCH (e:Entity)-[r:RELATES_TO]->(neighbor:Entity)
			WHERE e.uid IN [%s]
			  AND (r.validFrom IS NULL OR r.validFrom <= $now)
			  AND (r.validUntil IS NULL OR $now < r.validUntil)
			%s  %s
			RETURN DISTINCT neighbor.uid
			UNION
			MATCH (neighbor:Entity)-[r:RELATES_TO]->(e:Entity)
			WHERE e.uid IN [%s]
			  AND (r.validFrom IS NULL OR r.validFrom <= $now)
			  AND (r.validUntil IS NULL OR $now < r.validUntil)
			%s  %s
			RETURN DISTINCT neighbor.uid
		`, uidList, asOfR, edgeFilter, uidList, asOfR, edgeFilter)
	case 2:
		// The 2-hop pattern binds r1 and r2; the edge predicate allowlist
		// applies to both hops, so the filter clause is rendered twice.
		// Like the 1-hop lane, we mirror the traversal across edge directions:
		// a seed entity that is the OBJECT of the first relation must still
		// reach its second-hop neighbours, so we UNION outgoing-outgoing,
		// outgoing-incoming, incoming-outgoing, and incoming-incoming
		// patterns. Without this, graph recall systematically misses
		// reverse-chained context (e.g. "Eowyn -> works_at -> Erebor <- CEO -
		// Frodo" returns Frodo only via the 4-way UNION).
		edgeFilterR1 := h.buildEdgeFilterClause(args, "r1")
		edgeFilterR2 := h.buildEdgeFilterClause(args, "r2")
		asOfR1 := asOfClause("r1")
		asOfR2 := asOfClause("r2")
		twoHopBlock := func(p1, p2 string) string {
			return fmt.Sprintf(`MATCH %s%s
			WHERE e.uid IN [%s]
			  AND (r1.validFrom IS NULL OR r1.validFrom <= $now)
			  AND (r1.validUntil IS NULL OR $now < r1.validUntil)
			  AND (r2.validFrom IS NULL OR r2.validFrom <= $now)
			  AND (r2.validUntil IS NULL OR $now < r2.validUntil)
			%s%s  %s
			  %s
			  AND NOT (neighbor.uid IN [%s])
			RETURN DISTINCT neighbor.uid`, p1, p2, uidList, asOfR1, asOfR2, edgeFilterR1, edgeFilterR2, uidList)
		}
		cypher = strings.Join([]string{
			// out -> out (original direction)
			twoHopBlock("(e:Entity)-[r1:RELATES_TO]->(mid:Entity)", "-[r2:RELATES_TO]->(neighbor:Entity)"),
			// out -> in
			twoHopBlock("(e:Entity)-[r1:RELATES_TO]->(mid:Entity)", "<-[r2:RELATES_TO]-(neighbor:Entity)"),
			// in -> out
			twoHopBlock("(e:Entity)<-[r1:RELATES_TO]-(mid:Entity)", "-[r2:RELATES_TO]->(neighbor:Entity)"),
			// in -> in
			twoHopBlock("(e:Entity)<-[r1:RELATES_TO]-(mid:Entity)", "<-[r2:RELATES_TO]-(neighbor:Entity)"),
		}, "\nUNION\n")
	default:
		return nil, fmt.Errorf("unsupported hop depth %d", depth)
	}

	result, execErr := h.graph.Execute(ctx, cypher, args)
	if execErr != nil {
		return nil, fmt.Errorf("execute cypher: %w", execErr)
	}
	defer func() {
		if cerr := result.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close graph result: %w", cerr)
		}
	}()

	var entityUIDs []string
	for result.Next() {
		var uid string
		if scanErr := result.Scan(&uid); scanErr != nil {
			return nil, fmt.Errorf("scan neighbor uid: %w", scanErr)
		}
		entityUIDs = append(entityUIDs, uid)
	}
	if iterErr := result.Err(); iterErr != nil {
		return nil, fmt.Errorf("iterate graph result: %w", iterErr)
	}
	return entityUIDs, nil
}

// buildEdgeFilterClause returns a Cypher AND clause restricting the relation
// predicate to h.edgeAllow. When the allowlist is empty, returns an empty
// string (no restriction). Parameters for the predicate values are added to
// args under names "pred0", "pred1", etc. relVar selects which relation
// variable bound in the MATCH pattern the clause filters on; pass "r" for
// the single-hop pattern, or pass each of "r1" and "r2" once per call when
// composing the two-hop pattern.
func (h *HybridQuery) buildEdgeFilterClause(args map[string]any, relVar string) string {
	if len(h.edgeAllow) == 0 {
		return ""
	}
	if relVar == "" {
		relVar = "r"
	}
	predParams := make([]string, len(h.edgeAllow))
	for i, pred := range h.edgeAllow {
		key := fmt.Sprintf("pred%d", i)
		args[key] = pred
		predParams[i] = "$" + key
	}
	return fmt.Sprintf("AND %s.predicate IN [%s]", relVar, strings.Join(predParams, ", "))
}

// fetchNameVectorCandidates searches entities_vec for name-vector nearest
// neighbours of the query embedding, then joins those entity UIDs back to
// episodes via the mentions table.
//
// Returns:
//   - episodeUIDs: episode UIDs ranked by entity name similarity (the normal
//     name-emb lane contribution to fusion)
//   - orphanEntityHits: VectorHits for entity UIDs that matched by name-vector
//     but have no associated episode rows; collectHits emits these as
//     first-class entity hits (HitTypeEntity) so bare entities are visible in
//     recall results
func (h *HybridQuery) fetchNameVectorCandidates(
	ctx context.Context,
	nvs nameVectorSearcher,
	qvec []float32,
	k int,
) (episodeUIDs []string, orphanEntityHits []VectorHit, err error) {
	entityHits, err := nvs.SearchNameVectors(ctx, qvec, k)
	if err != nil {
		return nil, nil, fmt.Errorf("name-vector search: %w", err)
	}
	if len(entityHits) == 0 {
		return nil, nil, nil
	}

	entityUIDs := make([]string, len(entityHits))
	for i, hit := range entityHits {
		entityUIDs[i] = hit.ID
	}

	epUIDs, err := h.episodesForEntities(ctx, entityUIDs)
	if err != nil {
		return nil, nil, err
	}

	// Find orphans: entity UIDs that have no episode rows.
	epSet := make(map[string]struct{}, len(epUIDs))
	// episodesForEntities returns episode UIDs, not entity UIDs.
	// We need to know which entity UIDs contributed episodes.
	// Re-query: which of the matched entity UIDs appear in at least one episode?
	entityInEpisodes, err := h.entityUIDsInEpisodes(ctx, entityUIDs)
	if err != nil {
		return nil, nil, err
	}
	inEpisodes := make(map[string]struct{}, len(entityInEpisodes))
	for _, uid := range entityInEpisodes {
		inEpisodes[uid] = struct{}{}
	}

	_ = epSet // epSet was a leftover; entityInEpisodes is the authoritative check.

	for _, hit := range entityHits {
		if _, ok := inEpisodes[hit.ID]; !ok {
			orphanEntityHits = append(orphanEntityHits, hit)
		}
	}

	return epUIDs, orphanEntityHits, nil
}

// entityUIDsInEpisodes returns the subset of entityUIDs that appear in at
// least one mentions row (i.e. are cited by at least one episode). Used by
// fetchNameVectorCandidates to identify orphan entities.
func (h *HybridQuery) entityUIDsInEpisodes(ctx context.Context, entityUIDs []string) ([]string, error) {
	if len(entityUIDs) == 0 {
		return nil, nil
	}
	placeholders, args := inClauseStrings(entityUIDs)
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	q := fmt.Sprintf( //nolint:gosec // G201/G202: only "?" placeholders splice in; scope is constant fragments + bound ? params; all values parameterized
		`SELECT DISTINCT entity_uid FROM mentions WHERE entity_uid IN (%s) AND %s`,
		placeholders, scopePred,
	)
	args = append(args, scopeArgs...)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("entity-uids-in-episodes query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("entity-uids-in-episodes scan: %w", err)
		}
		out = append(out, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entity-uids-in-episodes iterate: %w", err)
	}
	return out, nil
}

// entityUIDsForEpisodes returns the entity UIDs cited by the given episode
// UIDs, via the mentions table (display_name match on entities table). This is
// a best-effort join: once the resolver wires entity_uid onto mentions, this
// query will be upgraded to a direct join. Until then, we match by display_name
// so the graph traversal lane produces results even before the resolver lands.
func (h *HybridQuery) entityUIDsForEpisodes(ctx context.Context, episodeUIDs []string) ([]string, error) {
	if len(episodeUIDs) == 0 {
		return nil, nil
	}
	const batchSize = 500
	var out []string
	seen := make(map[string]struct{})
	for start := 0; start < len(episodeUIDs); start += batchSize {
		end := start + batchSize
		if end > len(episodeUIDs) {
			end = len(episodeUIDs)
		}
		batch, err := h.entityUIDsForEpisodesBatch(ctx, episodeUIDs[start:end])
		if err != nil {
			return nil, err
		}
		for _, uid := range batch {
			if _, dup := seen[uid]; dup {
				continue
			}
			seen[uid] = struct{}{}
			out = append(out, uid)
		}
	}
	return out, nil
}

func (h *HybridQuery) entityUIDsForEpisodesBatch(ctx context.Context, episodeUIDs []string) ([]string, error) {
	placeholders, args := inClauseStrings(episodeUIDs)
	// Joins on m.entity_uid (migration 0008), not m.display_name. The old
	// surface-form join collapsed homonyms: two entities both named
	// "Frodo" had the same join key and recall could not tell them apart.
	// Scope BOTH the mention (m, the statement) and the entity (e): an
	// out-of-scope mention must not feed the graph-expansion lane an entity to
	// traverse from, even when that entity's own row is in the default span.
	ss := scopesFromContext(ctx)
	scopePredE, scopeArgsE := ss.Predicate("e")
	scopePredM, scopeArgsM := ss.Predicate("m")
	q := fmt.Sprintf( //nolint:gosec // G201/G202: only "?" placeholders splice in; scope is constant fragments + bound ? params; all values parameterized
		`SELECT DISTINCT e.uid
		 FROM entities e
		 JOIN mentions m ON m.entity_uid = e.uid
		 WHERE m.source_event_uid IN (%s)
		   AND %s
		   AND %s`,
		placeholders, scopePredE, scopePredM,
	)
	args = append(args, scopeArgsE...)
	args = append(args, scopeArgsM...)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("entity uid lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("entity uid scan: %w", err)
		}
		out = append(out, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entity uid iterate: %w", err)
	}
	return out, nil
}

// episodesForEntities returns episode UIDs that mention any of the given entity
// UIDs, via the mentions table joined on display_name. This is the reverse join
// of entityUIDsForEpisodes. The result is deduplicated and ordered by the
// entity's position in entityUIDs (preserving graph-rank ordering).
func (h *HybridQuery) episodesForEntities(ctx context.Context, entityUIDs []string) ([]string, error) {
	if len(entityUIDs) == 0 {
		return nil, nil
	}
	const batchSize = 500
	seen := make(map[string]struct{})
	var out []string
	for start := 0; start < len(entityUIDs); start += batchSize {
		end := start + batchSize
		if end > len(entityUIDs) {
			end = len(entityUIDs)
		}
		batch, err := h.episodesForEntitiesBatch(ctx, entityUIDs[start:end])
		if err != nil {
			return nil, err
		}
		for _, uid := range batch {
			if _, dup := seen[uid]; dup {
				continue
			}
			seen[uid] = struct{}{}
			out = append(out, uid)
		}
	}
	return out, nil
}

func (h *HybridQuery) episodesForEntitiesBatch(ctx context.Context, entityUIDs []string) ([]string, error) {
	placeholders, args := inClauseStrings(entityUIDs)
	// Joins on m.entity_uid (migration 0008); see entityUIDsForEpisodesBatch.
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("m")
	q := fmt.Sprintf( //nolint:gosec // G201/G202: only "?" placeholders splice in; scope is constant fragments + bound ? params; all values parameterized
		`SELECT DISTINCT m.source_event_uid
		 FROM mentions m
		 WHERE m.entity_uid IN (%s)
		   AND %s`,
		placeholders, scopePred,
	)
	args = append(args, scopeArgs...)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("episodes-for-entities query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("episodes-for-entities scan: %w", err)
		}
		out = append(out, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("episodes-for-entities iterate: %w", err)
	}
	return out, nil
}

// applySpatialFilter narrows hydrated rows to those that cite at least one
// entity within the Near radius of the resolved place. Resolution order:
//
//  1. Look up the placeQuery entity via FTS + name-vector match on entities
//     WHERE kind='place'.
//  2. If the resolved entity has coordinates, call geo.NearbyEntities and
//     filter rows by the resulting entity UID set.
//  3. If the resolved entity has no coordinates, log a warning and fall back
//     to a name-match filter on displayName / alternateNames.
//  4. Entities without coordinates are excluded from spatial-filtered results.
func (h *HybridQuery) applySpatialFilter(
	ctx context.Context,
	rows map[string]hydratedRow,
	near *NearFilter,
) (map[string]hydratedRow, error) {
	logger := logging.FromContext(ctx)

	radiusKm := near.RadiusKm
	if radiusKm <= 0 {
		radiusKm = spatialDefaultRadiusKm
	}

	// Resolve the place query to an entity with coordinates.
	lat, lon, res, err := h.resolvePlaceCoords(ctx, near.PlaceQuery)
	if err != nil {
		return nil, fmt.Errorf("resolve place %q: %w", near.PlaceQuery, err)
	}

	switch res {
	case placeUnknown:
		// No matching place entity at all (typo or unseen location): the
		// filter cannot resolve to a meaningful geographic region, so we
		// return an empty result instead of degrading to a substring scan
		// that would surface unrelated episodes.
		logger.Warn("Spatial filter: no place entity matches query; returning empty result",
			zap.String("placeQuery", near.PlaceQuery),
		)
		return map[string]hydratedRow{}, nil
	case placeWithoutCoord:
		// The place entity exists but has no coordinates: degrade to name match.
		logger.Warn("Spatial filter: place entity has no coordinates, degrading to name-match filter",
			zap.String("placeQuery", near.PlaceQuery),
		)
		return h.applyNameMatchSpatialFilter(ctx, rows, near.PlaceQuery)
	case placeWithCoord:
		// fall through to the haversine path below
	}

	// Brute-force haversine scan: find all entities within radiusKm, scoped to
	// the read span so out-of-scope coordinates cannot influence recall (#206).
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	nearby, err := geo.NearbyEntities(ctx, h.reader, lat, lon, radiusKm, scopePred, scopeArgs)
	if err != nil {
		return nil, fmt.Errorf("nearby entities: %w", err)
	}

	if len(nearby) == 0 {
		return map[string]hydratedRow{}, nil
	}

	nearbyUIDs := make(map[string]struct{}, len(nearby))
	for _, ec := range nearby {
		nearbyUIDs[ec.UID] = struct{}{}
	}

	// For each surviving episode, check whether it cites a nearby entity.
	episodeUIDs := make([]string, 0, len(rows))
	for uid := range rows {
		episodeUIDs = append(episodeUIDs, uid)
	}

	cited, err := h.episodesCitingEntities(ctx, episodeUIDs, nearbyUIDs)
	if err != nil {
		return nil, fmt.Errorf("episodes-citing-entities: %w", err)
	}

	out := make(map[string]hydratedRow, len(cited))
	for uid := range cited {
		if row, ok := rows[uid]; ok {
			out[uid] = row
		}
	}
	return out, nil
}

// applyAnchorFilter narrows hydrated rows to those that cite at least one
// entity in the anchor frontier set. The frontier is the N-hop neighbourhood
// of the anchor entity resolved before recall enters the fusion step. This
// mirrors applySpatialFilter's pre-fusion pattern so the two filters compose.
func (h *HybridQuery) applyAnchorFilter(
	ctx context.Context,
	rows map[string]hydratedRow,
	frontier map[string]struct{},
) (map[string]hydratedRow, error) {
	if len(rows) == 0 || len(frontier) == 0 {
		return map[string]hydratedRow{}, nil
	}

	episodeUIDs := make([]string, 0, len(rows))
	for uid := range rows {
		episodeUIDs = append(episodeUIDs, uid)
	}

	cited, err := h.episodesCitingEntities(ctx, episodeUIDs, frontier)
	if err != nil {
		return nil, fmt.Errorf("anchor-filter episodes-citing-entities: %w", err)
	}

	out := make(map[string]hydratedRow, len(cited))
	for uid := range cited {
		if row, ok := rows[uid]; ok {
			out[uid] = row
		}
	}
	return out, nil
}

// placeResolution captures the three outcomes of resolving a place query: a
// concrete match with coordinates, a matching entity that lacks coordinates,
// or no matching entity at all. The previous boolean signature conflated the
// last two cases, so a typo would degrade to substring name-match against
// every entity and return unrelated episodes; callers can now branch
// explicitly on the unknown case and return an empty filter result.
type placeResolution int

const (
	placeUnknown      placeResolution = iota // no place entity matches placeQuery
	placeWithoutCoord                        // a place entity matched but has no lat/lon
	placeWithCoord                           // a place entity matched and has lat/lon
)

// resolvePlaceCoords resolves a place query string to geographic coordinates.
// Returns (lat, lon, placeWithCoord, nil) when a coordinate-bearing place
// entity is found, (0, 0, placeWithoutCoord, nil) when a place entity exists
// but lacks coordinates, (0, 0, placeUnknown, nil) when no place entity
// matches, and (0, 0, placeUnknown, err) on query failure. When multiple
// entities match, the one with the highest display_name LIKE score is taken.
func (h *HybridQuery) resolvePlaceCoords(ctx context.Context, placeQuery string) (lat, lon float64, res placeResolution, err error) {
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	like := "%" + placeQuery + "%"
	//nolint:gosec // G202: scope is constant fragments + bound ? params; all values parameterized
	coordQ := fmt.Sprintf(`
		SELECT latitude, longitude
		FROM   entities
		WHERE  kind = 'place'
		  AND  (display_name LIKE ? OR display_name = ?)
		  AND  latitude IS NOT NULL
		  AND  longitude IS NOT NULL
		  AND  %s
		ORDER BY CASE WHEN display_name = ? THEN 0 ELSE 1 END
		LIMIT 1
	`, scopePred)
	// SQL placeholder sequence: ?, ?, <scope ?s>, ?
	// [like, placeQuery, ...scopeArgs, placeQuery]
	coordArgs := append([]any{like, placeQuery}, scopeArgs...)
	coordArgs = append(coordArgs, placeQuery)
	row := h.reader.QueryRowContext(ctx, coordQ, coordArgs...)
	if scanErr := row.Scan(&lat, &lon); scanErr != nil {
		if !errors.Is(scanErr, sql.ErrNoRows) {
			return 0, 0, placeUnknown, fmt.Errorf("resolve place coords: %w", scanErr)
		}
		// Disambiguate "no place entity at all" from "place exists w/o coords".
		// exists probe args: [like, placeQuery, ...scopeArgs]
		//nolint:gosec // G202: scope is constant fragments + bound ? params; all values parameterized
		existsQ := fmt.Sprintf(
			`SELECT 1 FROM entities WHERE kind = 'place' AND (display_name LIKE ? OR display_name = ?) AND %s LIMIT 1`,
			scopePred,
		)
		existsArgs := append([]any{like, placeQuery}, scopeArgs...)
		var exists int
		if existsErr := h.reader.QueryRowContext(ctx, existsQ, existsArgs...).Scan(&exists); existsErr != nil {
			if errors.Is(existsErr, sql.ErrNoRows) {
				return 0, 0, placeUnknown, nil
			}
			return 0, 0, placeUnknown, fmt.Errorf("probe place entity: %w", existsErr)
		}
		return 0, 0, placeWithoutCoord, nil
	}
	return lat, lon, placeWithCoord, nil
}

// applyNameMatchSpatialFilter is the degraded spatial filter used when the
// resolved place entity has no coordinates. It retains episodes citing entities
// whose display_name contains the place query string as a substring.
func (h *HybridQuery) applyNameMatchSpatialFilter(
	ctx context.Context,
	rows map[string]hydratedRow,
	placeQuery string,
) (map[string]hydratedRow, error) {
	episodeUIDs := make([]string, 0, len(rows))
	for uid := range rows {
		episodeUIDs = append(episodeUIDs, uid)
	}
	if len(episodeUIDs) == 0 {
		return map[string]hydratedRow{}, nil
	}

	const batchSize = 500
	surviving := make(map[string]struct{})
	for start := 0; start < len(episodeUIDs); start += batchSize {
		end := start + batchSize
		if end > len(episodeUIDs) {
			end = len(episodeUIDs)
		}
		batch, err := h.nameMatchEpisodes(ctx, episodeUIDs[start:end], placeQuery)
		if err != nil {
			return nil, err
		}
		for _, uid := range batch {
			surviving[uid] = struct{}{}
		}
	}

	out := make(map[string]hydratedRow, len(surviving))
	for uid := range surviving {
		if row, ok := rows[uid]; ok {
			out[uid] = row
		}
	}
	return out, nil
}

func (h *HybridQuery) nameMatchEpisodes(ctx context.Context, episodeUIDs []string, placeQuery string) ([]string, error) {
	placeholders, args := inClauseStrings(episodeUIDs)
	// placeQuery is used in a LIKE pattern. The pattern itself is assembled
	// in Go before passing to SQLite as a bound parameter, so the query
	// string is not vulnerable to injection.
	like := "%" + placeQuery + "%"
	args = append(args, like)
	// Scope BOTH the mention (m, the statement) and the named entity (e): the
	// LIKE on e.display_name is a keep/drop filter, so an out-of-scope entity
	// name must not surface an in-scope episode — mirrors docsCitingNamedEntities.
	ss := scopesFromContext(ctx)
	scopePredM, scopeArgsM := ss.Predicate("m")
	scopePredE, scopeArgsE := ss.Predicate("e")
	args = append(args, scopeArgsM...)
	args = append(args, scopeArgsE...)
	// Joins on m.entity_uid (migration 0008); see entityUIDsForEpisodesBatch.
	// The LIKE on display_name remains: this is a surface-form fallback for
	// spatial queries where the place entity has no coordinates, and the
	// intent is intentionally permissive ("any entity whose surface form
	// matches the place query").
	q := fmt.Sprintf( //nolint:gosec // G201/G202: only "?" placeholders splice in; scope is constant fragments + bound ? params; all values parameterized
		`SELECT DISTINCT m.source_event_uid
		 FROM mentions m
		 JOIN entities e ON e.uid = m.entity_uid
		 WHERE m.source_event_uid IN (%s)
		   AND (e.display_name LIKE ?)
		   AND %s
		   AND %s`,
		placeholders, scopePredM, scopePredE,
	)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("name-match spatial filter: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("name-match spatial filter scan: %w", err)
		}
		out = append(out, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("name-match spatial filter iterate: %w", err)
	}
	return out, nil
}

// episodesCitingEntities returns the subset of episodeUIDs that mention at
// least one entity whose UID is in nearbyUIDs.
func (h *HybridQuery) episodesCitingEntities(
	ctx context.Context,
	episodeUIDs []string,
	nearbyUIDs map[string]struct{},
) (map[string]struct{}, error) {
	if len(episodeUIDs) == 0 || len(nearbyUIDs) == 0 {
		return map[string]struct{}{}, nil
	}

	nearbyList := make([]string, 0, len(nearbyUIDs))
	for uid := range nearbyUIDs {
		nearbyList = append(nearbyList, uid)
	}

	const batchSize = 250 // two IN clauses, both batched to stay under 499 params each
	result := make(map[string]struct{})

	for epStart := 0; epStart < len(episodeUIDs); epStart += batchSize {
		epEnd := epStart + batchSize
		if epEnd > len(episodeUIDs) {
			epEnd = len(episodeUIDs)
		}
		for nStart := 0; nStart < len(nearbyList); nStart += batchSize {
			nEnd := nStart + batchSize
			if nEnd > len(nearbyList) {
				nEnd = len(nearbyList)
			}
			batch, err := h.episodesCitingEntitiesBatch(ctx, episodeUIDs[epStart:epEnd], nearbyList[nStart:nEnd])
			if err != nil {
				return nil, err
			}
			for _, uid := range batch {
				result[uid] = struct{}{}
			}
		}
	}
	return result, nil
}

func (h *HybridQuery) episodesCitingEntitiesBatch(ctx context.Context, episodeUIDs, entityUIDs []string) ([]string, error) {
	epPH, epArgs := inClauseStrings(episodeUIDs)
	entPH, entArgs := inClauseStrings(entityUIDs)
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("m")
	args := append(epArgs, entArgs...)
	args = append(args, scopeArgs...)
	// Joins on m.entity_uid (migration 0008); see entityUIDsForEpisodesBatch.
	q := fmt.Sprintf( //nolint:gosec // G201/G202: only "?" placeholders splice in; scope is constant fragments + bound ? params; all values parameterized
		`SELECT DISTINCT m.source_event_uid
		 FROM mentions m
		 WHERE m.source_event_uid IN (%s)
		   AND m.entity_uid IN (%s)
		   AND %s`,
		epPH, entPH, scopePred,
	)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("episodes-citing-entities query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("episodes-citing-entities scan: %w", err)
		}
		out = append(out, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("episodes-citing-entities iterate: %w", err)
	}
	return out, nil
}

// ftsTimeRanger is the optional surface a concrete FTSConn can satisfy to
// accept time bounds. The recall composer reaches for it via a runtime type
// assertion when RecallRequest carries Since/Until, this keeps the public
// FTSConn interface unchanged while letting the sqlite implementation push
// the time predicate INTO the FTS5 MATCH rather than post-filter. A backend
// that does not implement this falls back to top-K + filter, which is
// correct up to the candidate window but bounded.
//
// The bounds are passed as time.Time (not unix seconds) so RFC3339Nano
// precision survives the push-down boundary; the SQLite implementation
// formats them as RFC3339Nano strings and compares via julianday().
type ftsTimeRanger interface {
	SearchInTimeRange(ctx context.Context, query string, k int, minBound, maxBound time.Time) ([]FTSHit, error)
}

// hydratedRow holds the per-UID metadata we need to filter and project a Hit.
// Payload_json is decoded once into ontology.Episode to extract confidence and
// the fallback text; we copy the bits the composer cares about so a future
// schema change to Episode does not have to ripple through this file.
type hydratedRow struct {
	kind       string
	createTime time.Time
	text       string
	confidence float64
	// source is the episodes.source column — the originating event's Source
	// string ("remember", "extractor/v1", a "connector:<name>" tag). Carried so
	// the composer can stamp Hit.EventSource and the MCP layer can label a
	// connector-sourced (untrusted) hit's trust posture (#211, #212).
	source string
	// class is the EpisodeClass column (semantic | episodic |
	// procedural). Defaults to "episodic" for rows written before the
	// migration 0015 column landed. Recall consults it for the
	// per-type weight multiplier.
	class string
}

// episodePayload is the minimum subset of ontology.Episode the composer needs.
// We decode against a private struct here rather than importing the ontology
// package because (a) the kernel package is imported by ontology, not the
// other way around, and (b) it keeps the kernel package free of ontology
// version churn for M1.
type episodePayload struct {
	Confidence float64 `json:"confidence"`
	Text       string  `json:"text"`
}

// hydrate fetches the (kind, create_time, text, payload_json) tuple for every
// UID in the union of candidate lists. Batches when the union exceeds the
// SQLite parameter cap so a future high-K configuration cannot trip
// SQLITE_MAX_VARIABLE_NUMBER (default 999). Returns a map keyed by UID;
// missing UIDs (e.g., a vec hit whose episode row was just deleted) are
// silently dropped -- they cannot project a complete Hit.
func (h *HybridQuery) hydrate(ctx context.Context, uids []string) (map[string]hydratedRow, error) {
	out := make(map[string]hydratedRow, len(uids))
	if len(uids) == 0 {
		return out, nil
	}
	const batch = 500
	for start := 0; start < len(uids); start += batch {
		end := start + batch
		if end > len(uids) {
			end = len(uids)
		}
		if err := h.hydrateBatch(ctx, uids[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (h *HybridQuery) hydrateBatch(ctx context.Context, uids []string, out map[string]hydratedRow) error {
	placeholders, args := inClauseStrings(uids)
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	args = append(args, scopeArgs...)
	// Composed of a literal SELECT plus a placeholder string built
	// exclusively from "?" characters in inClauseStrings (no caller input
	// reaches the SQL). The suppression directive documents that audit.
	q := fmt.Sprintf( //nolint:gosec // G201/G202: only "?" placeholders splice in; scope is constant fragments + bound ? params; all values parameterized
		"SELECT uid, kind, create_time, text, payload_json, class, source FROM episodes WHERE uid IN (%s) AND %s",
		placeholders, scopePred,
	)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("kernel: hybrid: hydrate query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			uid         string
			kind        string
			createTime  string
			text        string
			payloadJSON string
			class       string
			source      sql.NullString
		)
		if err := rows.Scan(&uid, &kind, &createTime, &text, &payloadJSON, &class, &source); err != nil {
			return fmt.Errorf("kernel: hybrid: hydrate scan: %w", err)
		}
		row := hydratedRow{kind: kind, text: text, class: class, source: source.String}
		if t, err := parseCreateTime(createTime); err == nil {
			row.createTime = t
		}
		// payload_json may be empty for episodes written before the text
		// column existed; tolerate that and fall back to confidence=0.
		if strings.TrimSpace(payloadJSON) != "" {
			var p episodePayload
			if err := json.Unmarshal([]byte(payloadJSON), &p); err == nil {
				row.confidence = p.Confidence
				if row.text == "" {
					row.text = p.Text
				}
			}
		}
		out[uid] = row
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("kernel: hybrid: hydrate iterate: %w", err)
	}
	return nil
}

// parseCreateTime parses an RFC 3339 string from the create_time column.
// Tolerates both Nano and second precision because callers across the
// codebase use Format(time.RFC3339Nano) but some seed data uses RFC3339.
func parseCreateTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("kernel: hybrid: empty create_time")
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("kernel: hybrid: parse create_time %q: %w", s, err)
	}
	return t, nil
}

// filterHydrated drops rows that fail any of the pre-fuse predicates:
// req.Kinds (set semantics; empty means no restriction), req.MinConfidence
// (row.confidence < bound drops the row), req.Since (inclusive lower bound
// on createTime; zero value means no lower bound), and req.Until (exclusive
// upper bound on createTime; zero value means no upper bound). Time bounds
// match the RecallRequest godoc on query.go: Since >= and Until <.
func filterHydrated(rows map[string]hydratedRow, req RecallRequest) map[string]hydratedRow {
	if len(rows) == 0 {
		return rows
	}
	var kindSet map[EventKind]struct{}
	if len(req.Kinds) > 0 {
		kindSet = make(map[EventKind]struct{}, len(req.Kinds))
		for _, k := range req.Kinds {
			kindSet[k] = struct{}{}
		}
	}
	out := make(map[string]hydratedRow, len(rows))
	for uid, row := range rows {
		if kindSet != nil {
			if _, ok := kindSet[EventKind(row.kind)]; !ok {
				continue
			}
		}
		if row.confidence < req.MinConfidence {
			continue
		}
		if !req.Since.IsZero() && row.createTime.Before(req.Since) {
			continue
		}
		if !req.Until.IsZero() && !row.createTime.Before(req.Until) {
			continue
		}
		out[uid] = row
	}
	return out
}

// rerankSurvivorsVec walks the vector candidate list in original order and
// assigns a 1-based rank to every UID that survived the filter step.
// Filtered items vanish; survivors are renumbered 1..N within the source so
// the RRF denominator reflects the corrected ordering. UIDs not present in
// the source map to 0, matching rrfComponents' "absent from this source"
// convention.
func rerankSurvivorsVec(source []VectorHit, keep map[string]hydratedRow) map[string]int {
	out := make(map[string]int, len(source))
	rank := 0
	for _, h := range source {
		if _, ok := keep[h.ID]; !ok {
			continue
		}
		if _, dup := out[h.ID]; dup {
			continue
		}
		rank++
		out[h.ID] = rank
	}
	return out
}

// rerankSurvivorsFTS is the FTS sibling of rerankSurvivorsVec. The two stay
// separate (rather than collapsed behind a small interface) because the only
// shared field is the ID string and a generic abstraction over two
// five-line loops is more noise than reuse.
func rerankSurvivorsFTS(source []FTSHit, keep map[string]hydratedRow) map[string]int {
	out := make(map[string]int, len(source))
	rank := 0
	for _, h := range source {
		if _, ok := keep[h.ID]; !ok {
			continue
		}
		if _, dup := out[h.ID]; dup {
			continue
		}
		rank++
		out[h.ID] = rank
	}
	return out
}

// rerankSurvivorsGeneric assigns 1-based ranks to episode UIDs from a string
// slice (graph and name-vector lanes produce []string, not typed hit slices).
// Order is preserved; duplicates are skipped after the first occurrence.
func rerankSurvivorsGeneric(source []string, keep map[string]hydratedRow) map[string]int {
	out := make(map[string]int, len(source))
	rank := 0
	for _, id := range source {
		if _, ok := keep[id]; !ok {
			continue
		}
		if _, dup := out[id]; dup {
			continue
		}
		rank++
		out[id] = rank
	}
	return out
}

// rawSemanticByUID returns the cosine similarity per surviving UID,
// derived from the vec0 L2 distance via CosineSimilarityFromDistance.
// UIDs absent from the vector lane (or filtered out of `keep`) map to
// 0 — fusion treats that as "lane absent" and the adaptive divisor
// drops the semantic term's weight accordingly. On the first appearance
// of a UID wins (duplicate hits within the vector lane are ignored,
// mirroring rerankSurvivorsVec's dedup behaviour).
func rawSemanticByUID(source []VectorHit, keep map[string]hydratedRow) map[string]float32 {
	out := make(map[string]float32, len(source))
	for _, h := range source {
		if _, ok := keep[h.ID]; !ok {
			continue
		}
		if _, dup := out[h.ID]; dup {
			continue
		}
		out[h.ID] = CosineSimilarityFromDistance(h.Score)
	}
	return out
}

// rawBM25NormByUID returns the sigmoid-normalized BM25 score per
// surviving UID. The sigmoid midpoint and steepness adapt to query
// token count (mem0 v3's get_bm25_params). UIDs absent from the FTS
// lane map to 0.
func rawBM25NormByUID(source []FTSHit, keep map[string]hydratedRow, queryTokens int) map[string]float32 {
	out := make(map[string]float32, len(source))
	for _, h := range source {
		if _, ok := keep[h.ID]; !ok {
			continue
		}
		if _, dup := out[h.ID]; dup {
			continue
		}
		out[h.ID] = BM25Sigmoid(h.BM25, queryTokens)
	}
	return out
}

// countQueryTokens returns the number of whitespace-separated tokens
// in the recall query. Cheap and good-enough — mem0's BM25 sigmoid
// parameter table is keyed off "3 vs 15 tokens", not exact morphology,
// so unicode segmentation isn't load-bearing here.
func countQueryTokens(q string) int {
	return len(strings.Fields(q))
}

// rrfComponents computes the per-source weighted RRF contributions for one
// item. A rank of zero means "absent from this source"; the corresponding
// term contributes zero. GraphWeight and nameEmbWeight scale the graph and
// name-embedding contributions relative to the vector and FTS lanes (which
// are always weight=1.0 for backward compatibility).
func rrfComponents(
	vecRank, ftsRank, graphRank, nameEmbRank int,
	rrfK int,
	graphWeight, nameEmbWeight float64,
) (vc, fc, gc, nc float32) {
	if vecRank > 0 {
		vc = 1.0 / float32(rrfK+vecRank)
	}
	if ftsRank > 0 {
		fc = 1.0 / float32(rrfK+ftsRank)
	}
	if graphRank > 0 {
		gc = float32(graphWeight) / float32(rrfK+graphRank)
	}
	if nameEmbRank > 0 {
		nc = float32(nameEmbWeight) / float32(rrfK+nameEmbRank)
	}
	return
}

// fuseRRF is the original two-source RRF helper kept for backward compatibility
// with existing tests that call it directly. New callers should use rrfComponents.
func fuseRRF(vecRank, ftsRank, rrfK int) (fused, vecComp, ftsComp float32) {
	if vecRank > 0 {
		vecComp = 1.0 / float32(rrfK+vecRank)
	}
	if ftsRank > 0 {
		ftsComp = 1.0 / float32(rrfK+ftsRank)
	}
	fused = vecComp + ftsComp
	return
}

// collectEpisodeUIDs returns up to k unique episode UIDs from the union of
// vec and FTS hits, in vec-first order. Used to seed the graph traversal with
// the best candidates from the primary retrieval lanes.
func collectEpisodeUIDs(vecHits []VectorHit, ftsHits []FTSHit, k int) []string {
	seen := make(map[string]struct{}, k)
	out := make([]string, 0, k)
	add := func(id string) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, h := range vecHits {
		if len(out) >= k {
			break
		}
		add(h.ID)
	}
	for _, h := range ftsHits {
		if len(out) >= k {
			break
		}
		add(h.ID)
	}
	return out
}

// unionAllUIDs returns the deduplicated set of UIDs across all four candidate
// sources, preserving first-seen order. Vec first, then FTS, then graph, then
// name-embedding.
func unionAllUIDs(vecHits []VectorHit, ftsHits []FTSHit, graphHits, nameEmbHits []string) []string {
	seen := make(map[string]struct{},
		len(vecHits)+len(ftsHits)+len(graphHits)+len(nameEmbHits))
	out := make([]string, 0, len(vecHits)+len(ftsHits)+len(graphHits)+len(nameEmbHits))
	add := func(id string) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, h := range vecHits {
		add(h.ID)
	}
	for _, h := range ftsHits {
		add(h.ID)
	}
	for _, id := range graphHits {
		add(id)
	}
	for _, id := range nameEmbHits {
		add(id)
	}
	return out
}

// unionUIDs returns the deduplicated set of UIDs across the two primary
// candidate lists. Kept for use by existing tests.
func unionUIDs(vecHits []VectorHit, ftsHits []FTSHit) []string {
	return unionAllUIDs(vecHits, ftsHits, nil, nil)
}

// pickSnippet returns the snippet to expose on a Hit. FTS-driven hits inherit
// the FTS auxiliary's 30-char window with <b>...</b> markup; vec-only hits
// fall back to the first runesMax runes of the episode's full text (no
// markup, so consumers can distinguish lexical-match snippets from semantic-
// match ones at render time).
func pickSnippet(uid string, row hydratedRow, ftsHits []FTSHit, runesMax int) string {
	for _, h := range ftsHits {
		if h.ID == uid && h.Snippet != "" {
			return h.Snippet
		}
	}
	return truncateRunes(row.text, runesMax)
}

// truncateRunes returns s capped at runesMax runes (not bytes). When the
// input is shorter, it is returned unchanged. A negative or zero cap returns
// the empty string -- defensive against a misconfigured option.
func truncateRunes(s string, runesMax int) string {
	if runesMax <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= runesMax {
		return s
	}
	return string(r[:runesMax])
}

// escapeFTSQuery sanitises a natural-language recall query into a safe FTS5
// MATCH expression. FTSStore.Search passes its input verbatim to MATCH, and
// FTS5 treats characters like `"`, `*`, `:`, `(`, `^` and the bare tokens
// `AND` / `OR` / `NOT` / `NEAR` as syntax. A plain user phrase that happens
// to contain an unmatched quote or one of those keywords causes the FTS lane
// to fail with a syntax error and, because errgroup aborts on first
// failure, drops the otherwise-valid vector results too.
//
// Tokenisation mirrors FTS5's unicode61 tokenizer (the tokenizer configured
// in migration 0002_fts5_episodes.up.sql, with `_` as the only extra
// tokenchar): a token is a maximal run of {letter, digit, underscore}.
// Every other rune is a boundary. Each surviving token is wrapped in
// double quotes so reserved words like `AND` become phrase literals, and
// tokens are joined by explicit `OR`. Recall queries are typically
// question-form ("where does X work?"); space-joined tokens in FTS5 mean
// implicit AND, which requires every question word ("where", "does") to
// appear in the candidate episode. That predicate never matches stored
// facts, so the FTS lane stays silent and the hybrid degenerates to
// vec-only. OR-joining lets any content token light up the lane; BM25
// ranks by overlap and RRF (k=60) deranks single-word noise hits against
// the vec lane.
//
// AGENTS.md rule 8 / Decision log: the OR flip was chosen over stopword
// filtering because RRF + BM25 already handle precision; adding a stopword
// list would couple us to a language-specific resource and would not help
// multilingual queries (German question words are not in any English
// stopword list).
//
// Tokens of three or more runes get a trailing `*`, turning each into an
// FTS5 prefix token so the lane recalls morphological variants: a query
// "peanut" matches a stored "peanuts", "allergy" matches "allergies". One-
// and two-rune tokens stay exact — they are overwhelmingly function words
// and initials where a prefix scan is pure noise (and a broad term-index
// walk). The threshold is language-agnostic (no stopword list), and BM25 +
// weighted fusion derank the looser prefix hits, the same reasoning that
// justifies the OR join above.
//
// Returns the empty string when the input has no token-able content; the
// caller is expected to skip the FTS sub-query in that case rather than
// passing an empty MATCH to FTS5.
func escapeFTSQuery(q string) string {
	if q == "" {
		return ""
	}
	var tokens []string
	var b strings.Builder
	runes := 0
	flush := func() {
		if b.Len() == 0 {
			return
		}
		// 3+ rune tokens become prefix tokens (see godoc); shorter exact.
		tok := `"` + b.String() + `"`
		if runes >= 3 {
			tok += "*"
		}
		tokens = append(tokens, tok)
		b.Reset()
		runes = 0
	}
	for _, r := range q {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
			runes++
			continue
		}
		flush()
	}
	flush()
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " OR ")
}

// inClauseStrings builds the placeholder list ("?, ?, ?, ...") and the []any
// args slice for a SQL `IN` clause keyed by string. Kept local to this file
// because sqlite.inClause is unexported and reaching into another package
// just for a five-line helper is not worth a public surface.
func inClauseStrings(vals []string) (string, []any) {
	parts := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		parts[i] = "?"
		args[i] = v
	}
	return strings.Join(parts, ","), args
}

// entityMetaRow holds the entity fields needed to project an entity recall hit
// and apply the same pre-fuse predicates as filterHydrated applies to episodes.
type entityMetaRow struct {
	kind        string
	displayName string
	// confidence mirrors the entities.confidence column; used for MinConfidence
	// filtering, matching filterHydrated's policy for episodes.
	confidence float64
	// createTime is the entity's transaction-time origin; used for Since/Until
	// and AsOf filtering.  createTimeSet is false when the column is empty or
	// unparseable — in that case time-based filters are skipped for this entity
	// (conservative: keep it visible rather than silently drop it).
	createTime    time.Time
	createTimeSet bool
}

// resolveNearbyEntityUIDs resolves the NearFilter to a set of entity UIDs that
// are within the requested radius of the named place.
//
//   - placeUnknown → empty set (suppress all bare entity hits).
//   - placeWithoutCoord → name-match set: only entity UIDs whose display_name
//     contains the place query as a substring, mirroring the episode path's
//     applyNameMatchSpatialFilter degradation.
//   - placeWithCoord → haversine set from geo.NearbyEntities.
func (h *HybridQuery) resolveNearbyEntityUIDs(ctx context.Context, near *NearFilter) (map[string]struct{}, error) {
	radiusKm := near.RadiusKm
	if radiusKm <= 0 {
		radiusKm = spatialDefaultRadiusKm
	}

	lat, lon, res, err := h.resolvePlaceCoords(ctx, near.PlaceQuery)
	if err != nil {
		return nil, fmt.Errorf("resolve nearby entity UIDs for place %q: %w", near.PlaceQuery, err)
	}

	switch res {
	case placeUnknown:
		// No matching place entity at all; suppress all bare entity hits (mirrors
		// the episode path returning an empty map for an unknown place).
		return map[string]struct{}{}, nil
	case placeWithoutCoord:
		// Place exists but has no coordinates; degrade to name-match, mirroring
		// applySpatialFilter → applyNameMatchSpatialFilter for episodes.
		return h.entityNameMatchUIDs(ctx, near.PlaceQuery)
	case placeWithCoord:
		// fall through to the haversine scan below
	}

	// Scoped to the read span: out-of-scope coordinates must not seed the
	// spatial entity set (#206).
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	nearby, nerr := geo.NearbyEntities(ctx, h.reader, lat, lon, radiusKm, scopePred, scopeArgs)
	if nerr != nil {
		return nil, fmt.Errorf("nearby entity UIDs: %w", nerr)
	}
	out := make(map[string]struct{}, len(nearby))
	for _, ec := range nearby {
		out[ec.UID] = struct{}{}
	}
	return out, nil
}

// entityNameMatchUIDs returns the set of entity UIDs whose display_name
// contains placeQuery as a case-insensitive substring.  Used as the degraded
// spatial filter when the resolved place entity has no coordinates, mirroring
// applyNameMatchSpatialFilter for episodes.
func (h *HybridQuery) entityNameMatchUIDs(ctx context.Context, placeQuery string) (map[string]struct{}, error) {
	like := "%" + placeQuery + "%"
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	//nolint:gosec // G202: scope is constant fragments + bound ? params; all values parameterized
	q := "SELECT uid FROM entities WHERE display_name LIKE ? AND " + scopePred
	args := append([]any{like}, scopeArgs...)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("kernel: hybrid: entityNameMatchUIDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]struct{})
	for rows.Next() {
		var uid string
		if scanErr := rows.Scan(&uid); scanErr != nil {
			return nil, fmt.Errorf("kernel: hybrid: entityNameMatchUIDs scan: %w", scanErr)
		}
		out[uid] = struct{}{}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("kernel: hybrid: entityNameMatchUIDs rows: %w", rowsErr)
	}
	return out, nil
}

// hydrateEntityMeta fetches the kind, display_name, confidence, and
// create_time for each entity UID in hits.  Missing UIDs (entity deleted
// between the vector search and this call) are silently dropped, matching
// hydrate's policy for episodes.
func (h *HybridQuery) hydrateEntityMeta(ctx context.Context, hits []VectorHit) (map[string]entityMetaRow, error) {
	if len(hits) == 0 {
		return map[string]entityMetaRow{}, nil
	}
	uids := make([]string, len(hits))
	for i, hit := range hits {
		uids[i] = hit.ID
	}
	placeholders, args := inClauseStrings(uids)
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	args = append(args, scopeArgs...)
	q := fmt.Sprintf( //nolint:gosec // G201/G202: only "?" placeholders splice in; scope is constant fragments + bound ? params; all values parameterized
		`SELECT uid, kind, display_name,
		        COALESCE(confidence, 0.0),
		        COALESCE(create_time, '')
		   FROM entities WHERE uid IN (%s) AND %s`,
		placeholders, scopePred,
	)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("kernel: hybrid: hydrateEntityMeta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]entityMetaRow, len(hits))
	for rows.Next() {
		var uid string
		var ctStr string
		var meta entityMetaRow
		if err := rows.Scan(&uid, &meta.kind, &meta.displayName, &meta.confidence, &ctStr); err != nil {
			return nil, fmt.Errorf("kernel: hybrid: hydrateEntityMeta scan: %w", err)
		}
		if ctStr != "" {
			if ct, perr := time.Parse(time.RFC3339Nano, ctStr); perr == nil {
				meta.createTime = ct.UTC()
				meta.createTimeSet = true
			} else if ct, perr = time.Parse(time.RFC3339, ctStr); perr == nil {
				meta.createTime = ct.UTC()
				meta.createTimeSet = true
			}
		}
		out[uid] = meta
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kernel: hybrid: hydrateEntityMeta rows: %w", err)
	}
	return out, nil
}
