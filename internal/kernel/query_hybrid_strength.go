package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// Cognitive-lifecycle ranking defaults (issue #231). All are opt-in-overridable
// via the [recall] config table through applyRecallTuning, mirroring the recency
// knobs.
const (
	// StrengthBoostDefault scales the passive-reinforcement multiplier. 0.15
	// matches recencyBoostDefault — reinforcement is a tiebreaker, not a
	// dominant signal.
	strengthBoostDefault = 0.15

	// StrengthHalflifeDaysDefault is σ (days) for the access-recency Gaussian
	// decay; 30 matches recencyHalflifeDaysDefault.
	strengthHalflifeDaysDefault = 30.0

	// SalienceBaseDefault is the reinforcement curve base r0; matches the
	// memory_strength.salience_base column default.
	salienceBaseDefault = 0.5

	// ReinforcementDecayFactor is the per-access multiplier of the
	// reinforcement curve. The competitor's procedural rule
	// reinforced += 0.1*(1-r) is order-dependent (each step reads the running
	// value); its order-INDEPENDENT closed form after n reinforcements from a
	// base r0 is r_n = 1 - (1-r0)*reinforcementDecayFactor^n, which depends
	// only on the count n. 0.9 = 1 - 0.1 matches the competitor's gain.
	reinforcementDecayFactor = 0.9

	// DomainInsights / domainConsolidated are the vector-domain names the
	// derived-memory recall lanes search. They MUST match
	// sqlite.DomainInsights / sqlite.DomainConsolidated; kernel cannot import
	// the sqlite package (import cycle), so the contract is the string value.
	domainInsights     = "insights"
	domainConsolidated = "consolidated"
)

// ReinforcementScore returns the order-independent reinforcement term for a
// memory with accessCount accesses and the given salience base, in the closed
// interval [salienceBase, 1). It is the closed form of the iterated rule
// r <- r + 0.1*(1-r): r_n = 1 - (1-salienceBase)*0.9^n. Because it is a pure
// function of accessCount (and the constant salienceBase), it is reorder- and
// replay-invariant — the convergence guarantee carries through to the score.
func ReinforcementScore(accessCount int, salienceBase float64) float64 {
	if accessCount < 0 {
		accessCount = 0
	}
	return 1 - (1-salienceBase)*math.Pow(reinforcementDecayFactor, float64(accessCount))
}

// AccessRecorder records that recall surfaced a set of memories. The daemon's
// implementation emits one batched, signed memoryAccessed event set per recall
// that folds into memory_strength (issue #231). The implementation MUST be
// non-blocking and best-effort — recall never waits on it and never fails when
// it errors. RecordAccess is called on the final, capped hit set.
type AccessRecorder interface {
	RecordAccess(ctx context.Context, targetUIDs []string)
}

// domainVectorSearcher is the optional surface a VectorConn satisfies to expose
// a domain-scoped nearest-neighbour search. HybridQuery reaches for it via a
// runtime type assertion; a backend that does not implement it silently skips
// the derived-memory recall lanes. The sqlite.Vector implementation satisfies
// it via SearchByDomain.
type domainVectorSearcher interface {
	SearchByDomain(ctx context.Context, domain string, query []float32, k int) ([]VectorHit, error)
}

// SetAccessRecorder installs an AccessRecorder after construction (late binding,
// like SetStats: the recorder closes over the daemon's event-log appender, which
// is plumbed downstream of NewHybridQuery). Passing nil disables passive capture.
func (h *HybridQuery) SetAccessRecorder(r AccessRecorder) {
	h.accessSink = r
}

// WithStrengthBoost scales the passive-reinforcement multiplier applied during
// fusion: FusedScore *= (1 + boost · reinforcement(accessCount) · decay). Boost
// is the multiplicative gain in [0, 1]; 0 disables the lane entirely (the
// cognitive ranking signal is turned off), strengthBoostDefault (0.15) is the
// production default (reinforcement as a tiebreaker). Values outside [0, 1] are
// clamped. Mirrors WithRecencyBoost. The σ knob is WithStrengthHalflife.
func WithStrengthBoost(boost float64) HybridOption {
	return func(h *HybridQuery) {
		switch {
		case boost < 0:
			h.strengthBoost = 0
		case boost > 1:
			h.strengthBoost = 1
		default:
			h.strengthBoost = boost
		}
	}
}

// WithStrengthHalflife sets σ (in days) for the Gaussian access-recency decay
// of the reinforcement multiplier. Non-positive values are ignored; the default
// (strengthHalflifeDaysDefault, 30 days, matching the recency halflife) holds.
func WithStrengthHalflife(days float64) HybridOption {
	return func(h *HybridQuery) {
		if days > 0 {
			h.strengthHalflifeDays = days
		}
	}
}

// WithSalienceBase sets the reinforcement curve base r0 used when a target's
// memory_strength row carries no per-row salience (the common case). Values
// outside (0, 1) are ignored; the default (salienceBaseDefault, 0.5) holds.
// A never-accessed memory has reinforcement = salienceBase.
func WithSalienceBase(base float64) HybridOption {
	return func(h *HybridQuery) {
		if base > 0 && base < 1 {
			h.salienceBase = base
		}
	}
}

// recallAnchor is the "now" reference for the recency and strength decays: the
// caller's CurrentDate when supplied, else the wall clock. Computed once per
// recall so both decays share one anchor.
func recallAnchor(req RecallRequest) time.Time {
	if req.CurrentDate != nil && !req.CurrentDate.IsZero() {
		return req.CurrentDate.UTC()
	}
	return time.Now().UTC()
}

// strengthRow is the materialized memory_strength accumulator for one target,
// read on the recall hydration path.
type strengthRow struct {
	accessCount    int
	lastAccessedAt time.Time
	salienceBase   float64
}

// strengthMultiplier composes the passive-reinforcement multiplier for a
// candidate: 1 + strengthBoost · reinforcement(accessCount) · decay(now −
// lastAccessed), with the decay a Gaussian over days-since-last-access (same
// shape as the recency boost). The decay is computed HERE at query time from
// the stored last_accessed_at — it is never stored, which keeps the
// memory_strength projection replay-stable. Returns the multiplier (>= 1) and
// the raw strength in [0, 1] for the score-component breakdown.
func (h *HybridQuery) strengthMultiplier(s strengthRow, anchor time.Time) (mult, strength float32) {
	if h.strengthBoost <= 0 || s.accessCount <= 0 {
		return 1, 0
	}
	base := s.salienceBase
	if base <= 0 {
		base = h.salienceBase
	}
	reinf := ReinforcementScore(s.accessCount, base)
	decay := 1.0
	if !s.lastAccessedAt.IsZero() {
		days := anchor.Sub(s.lastAccessedAt).Hours() / 24.0
		if days < 0 {
			days = 0
		}
		sigma := h.strengthHalflifeDays
		if sigma <= 0 {
			sigma = strengthHalflifeDaysDefault
		}
		decay = math.Exp(-(days * days) / (2.0 * sigma * sigma))
	}
	st := reinf * decay
	return float32(1.0 + h.strengthBoost*st), float32(st)
}

// fetchStrength loads the memory_strength accumulator for the given target UIDs.
// Keyed by target UID (already scope-filtered upstream via the episode/derived
// hydration), so no scope predicate is needed here. Missing UIDs (never
// accessed) simply have no map entry → a zero strengthRow → multiplier 1.
func (h *HybridQuery) fetchStrength(ctx context.Context, uids []string) (map[string]strengthRow, error) {
	out := make(map[string]strengthRow, len(uids))
	if len(uids) == 0 || h.strengthBoost <= 0 {
		return out, nil
	}
	const batch = 500
	for start := 0; start < len(uids); start += batch {
		end := start + batch
		if end > len(uids) {
			end = len(uids)
		}
		if err := h.fetchStrengthBatch(ctx, uids[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (h *HybridQuery) fetchStrengthBatch(ctx context.Context, uids []string, out map[string]strengthRow) error {
	placeholders, args := inClauseStrings(uids)
	// Only "?" placeholders splice into the SQL; all values are bound.
	q := fmt.Sprintf( //nolint:gosec // G201: placeholders are "?" only; all values parameterized
		"SELECT target_uid, access_count, last_accessed_at, salience_base FROM memory_strength WHERE target_uid IN (%s)",
		placeholders,
	)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("kernel: hybrid: strength query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			uid      string
			count    int
			lastRaw  *string
			salience float64
		)
		if err := rows.Scan(&uid, &count, &lastRaw, &salience); err != nil {
			return fmt.Errorf("kernel: hybrid: strength scan: %w", err)
		}
		sr := strengthRow{accessCount: count, salienceBase: salience}
		if lastRaw != nil && *lastRaw != "" {
			if t, perr := parseCreateTime(*lastRaw); perr == nil {
				sr.lastAccessedAt = t
			}
		}
		out[uid] = sr
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("kernel: hybrid: strength iterate: %w", err)
	}
	return nil
}

// collectDerivedMemoryHits runs the insight + consolidated-memory recall lanes:
// a vector search over each derived domain, hydrating only HOT rows (the
// demote filter — a demoted insight has state != 'hot' and so leaves hot
// recall) within the request's scope, scored by cosine similarity and the
// passive-reinforcement multiplier. Returns nil (no error) when the request has
// no query embedding, the vector backend lacks domain search, or the caller
// filtered Kinds — the derived lanes are an additive enrichment, never a hard
// dependency.
func (h *HybridQuery) collectDerivedMemoryHits(ctx context.Context, qvec []float32, req RecallRequest, anchor time.Time) ([]Hit, error) {
	if qvec == nil {
		return nil, nil
	}
	searcher, ok := h.vec.(domainVectorSearcher)
	if !ok {
		return nil, nil
	}
	var out []Hit
	// Kinds gating: an empty filter means "no restriction" (run every lane);
	// otherwise a lane runs only when its event kind is explicitly requested.
	// This lets a caller recall ONLY insights (kinds=["insightSynthesized"]) or
	// ONLY consolidated memories, matching the episode + entity lanes' Kinds
	// semantics — rather than the previous behavior of dropping all derived
	// hits whenever any Kinds filter was set.
	if derivedKindWanted(req.Kinds, "insightSynthesized") {
		insightHits, err := h.collectDerivedDomain(ctx, searcher, qvec, derivedLane{
			domain: domainInsights, table: "insights", textCol: "statement",
			timeCol: "synthesized_at", confCol: "current_confidence",
			anchorCol: "seed_entity_uid", citationPrefix: "entities/",
			hitType: HitTypeInsight, kind: "insightSynthesized",
		}, req, anchor)
		if err != nil {
			return nil, err
		}
		out = append(out, insightHits...)
	}
	if derivedKindWanted(req.Kinds, "memoryConsolidated") {
		consolidatedHits, err := h.collectDerivedDomain(ctx, searcher, qvec, derivedLane{
			domain: domainConsolidated, table: "consolidated_memories", textCol: "summary",
			timeCol: "consolidated_at", confCol: "confidence",
			anchorCol: "cited_entity_uid", citationPrefix: "episodes/",
			hitType: HitTypeConsolidated, kind: "memoryConsolidated",
		}, req, anchor)
		if err != nil {
			return nil, err
		}
		out = append(out, consolidatedHits...)
	}
	return out, nil
}

// withinRequestedTimeRange reports whether a derived memory stamped at ct falls
// within the request's Since/Until/AsOf transaction-time bounds. The derived
// lanes previously ignored these entirely, leaking insights and consolidated
// memories across the bi-temporal boundary that the episode and bare-entity
// lanes both honor; this restores parity (mirrors query_hybrid.go's bare-entity
// create-time checks against the derived timeCol synthesized_at/consolidated_at).
func withinRequestedTimeRange(ct time.Time, req RecallRequest) bool {
	if !req.Since.IsZero() && ct.Before(req.Since) {
		return false
	}
	if !req.Until.IsZero() && !ct.Before(req.Until) {
		return false
	}
	if req.AsOf != nil && ct.After(*req.AsOf) {
		return false
	}
	return true
}

// derivedKindWanted reports whether a derived lane carrying event kind `want`
// should run for this request. An empty Kinds filter means no restriction (run
// every lane); a non-empty filter runs the lane only when its kind is listed.
func derivedKindWanted(kinds []EventKind, want EventKind) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// derivedLane parameterizes one derived-memory recall lane.
type derivedLane struct {
	domain  string
	table   string
	textCol string
	timeCol string
	// confCol is the column holding the derived memory's current confidence
	// (insights.current_confidence / consolidated_memories.confidence). Surfaced
	// as Hit.Confidence so MinConfidence filtering and agent inspection work.
	confCol string
	// anchorCol is the entity the derived memory is "about"
	// (insights.seed_entity_uid / consolidated_memories.cited_entity_uid).
	// Hydrated into Hit.Entities so an agent can seed-then-traverse from a
	// derived memory, and used to apply the spatial (Near) filter.
	anchorCol string
	// citationPrefix turns the bare source ULIDs stored on the derived memory
	// into typed AIP names an agent can feed to get/neighbors/relations:
	// insight sources are entities ("entities/"), consolidated sources are the
	// episodes they compressed ("episodes/").
	citationPrefix string
	hitType        HitType
	// kind is the canonical event kind that produced this derived memory
	// ("insightSynthesized" / "memoryConsolidated"). Stamped onto each Hit so
	// the MCP/CLI projection labels the hit by its real kind rather than
	// defaulting an empty kind to "episode". Held as a literal because the
	// kernel cannot import the ontology package (it would cycle); the strings
	// match ontology.KindInsightSynthesized / KindMemoryConsolidated.
	kind EventKind
}

// derivedPending is a hydrated derived-memory row awaiting the post-scan
// anchor-entity attachment and spatial filtering passes.
type derivedPending struct {
	hit       Hit
	anchorUID string
}

func (h *HybridQuery) collectDerivedDomain(ctx context.Context, searcher domainVectorSearcher, qvec []float32, lane derivedLane, req RecallRequest, anchor time.Time) ([]Hit, error) {
	vhits, err := searcher.SearchByDomain(ctx, lane.domain, qvec, h.candidateK)
	if err != nil {
		return nil, fmt.Errorf("kernel: hybrid: %s lane search: %w", lane.domain, err)
	}
	if len(vhits) == 0 {
		return nil, nil
	}
	cosByUID := make(map[string]float32, len(vhits))
	uids := make([]string, 0, len(vhits))
	for _, vh := range vhits {
		cosByUID[vh.ID] = CosineSimilarityFromDistance(vh.Score)
		uids = append(uids, vh.ID)
	}

	strengthByUID, serr := h.fetchStrength(ctx, uids)
	if serr != nil {
		// Mirror collectHits: reinforcement is an additive tiebreaker, never a
		// hard recall dependency. A partial/un-migrated memory_strength table
		// degrades the derived lane to neutral multipliers rather than failing
		// recall outright.
		logging.FromContext(ctx).Warn("kernel: hybrid: derived-lane strength fetch failed; ranking without reinforcement",
			zap.String("domain", lane.domain), zap.Error(serr))
		strengthByUID = nil
	}

	pend, anchorSet, err := h.hydrateDerivedRows(ctx, lane, req, uids, cosByUID, strengthByUID, anchor)
	if err != nil {
		return nil, err
	}
	if len(pend) == 0 {
		return nil, nil
	}

	// Spatial filter: when Near is set, keep only derived memories whose anchor
	// entity is within the requested radius. An unresolvable place yields an
	// empty set, so all derived hits are suppressed (mirrors the episode + bare
	// entity lanes) rather than silently ignoring the constraint.
	var nearby map[string]struct{}
	if req.Near != nil {
		nearby, err = h.resolveNearbyEntityUIDs(ctx, req.Near)
		if err != nil {
			return nil, err
		}
	}

	// Anchor-entity hydration: attach the seed/cited entity so an agent can pivot
	// a derived memory straight into get/neighbors/relations (seed-then-traverse).
	// A failed lookup degrades to no anchor rather than failing the lane.
	refs, rerr := h.lookupEntityRefs(ctx, anchorSet)
	if rerr != nil {
		logging.FromContext(ctx).Warn("kernel: hybrid: derived-lane anchor hydrate failed; emitting without entity anchor",
			zap.String("domain", lane.domain), zap.Error(rerr))
		refs = nil
	}
	return finalizeDerivedHits(pend, nearby, refs), nil
}

// hydrateDerivedRows runs the scope-filtered, hot-only hydration query for a
// derived lane and scans each surviving (MinConfidence-passing) row into a
// derivedPending carrying its scored Hit and anchor entity UID. Extracted from
// collectDerivedDomain to keep that function's cognitive complexity in budget.
func (h *HybridQuery) hydrateDerivedRows(ctx context.Context, lane derivedLane, req RecallRequest, uids []string, cosByUID map[string]float32, strengthByUID map[string]strengthRow, anchor time.Time) ([]derivedPending, []string, error) {
	placeholders, args := inClauseStrings(uids)
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	args = append(args, scopeArgs...)
	// Only "?" placeholders + constant column names splice into the SQL; the
	// state='hot' demote filter and scope predicate are constant fragments and
	// bound params. confCol + anchorCol carry the hit's confidence and the
	// entity it is "about".
	q := fmt.Sprintf( //nolint:gosec // G201: identifiers are in-tree constants; placeholders are "?" only; all values parameterized
		"SELECT uid, %s, source_uids, %s, %s, %s FROM %s WHERE uid IN (%s) AND state = 'hot' AND %s",
		lane.textCol, lane.timeCol, lane.confCol, lane.anchorCol, lane.table, placeholders, scopePred,
	)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("kernel: hybrid: %s lane hydrate: %w", lane.domain, err)
	}
	defer func() { _ = rows.Close() }()

	var (
		pend      []derivedPending
		anchorSet []string
	)
	for rows.Next() {
		var uid, text, sourceUidsJSON, tsRaw, anchorUID string
		var conf float64
		if err := rows.Scan(&uid, &text, &sourceUidsJSON, &tsRaw, &conf, &anchorUID); err != nil {
			return nil, nil, fmt.Errorf("kernel: hybrid: %s lane scan: %w", lane.domain, err)
		}
		// MinConfidence filter: now that derived hits carry a real confidence,
		// honor the same floor episodes do rather than always passing as 0.
		if conf < req.MinConfidence {
			continue
		}
		// Since / Until / AsOf transaction-time filter against the derived
		// memory's own timestamp (synthesized_at / consolidated_at), restoring
		// parity with the episode and bare-entity lanes. A row outside the
		// window is dropped; an unparseable timestamp is kept conservatively,
		// mirroring the bare-entity createTimeSet guard.
		ct, ctErr := parseCreateTime(tsRaw)
		if ctErr == nil && !withinRequestedTimeRange(ct, req) {
			continue
		}
		cos := cosByUID[uid]
		mult, strength := h.strengthMultiplier(strengthByUID[uid], anchor)
		hit := Hit{
			HitType:    lane.hitType,
			Kind:       lane.kind,
			UID:        uid,
			Text:       text,
			Snippet:    truncateRunes(text, h.snippetRunes),
			Confidence: conf,
			FusedScore: cos * mult,
			Components: ScoreComponents{Vector: cos, RawSemantic: cos, Strength: strength},
			Citations:  prefixCitations(lane.citationPrefix, decodeSourceUids(sourceUidsJSON)),
		}
		if ctErr == nil {
			hit.CreateTime = ct
		}
		pend = append(pend, derivedPending{hit: hit, anchorUID: anchorUID})
		if anchorUID != "" {
			anchorSet = append(anchorSet, anchorUID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("kernel: hybrid: %s lane iterate: %w", lane.domain, err)
	}
	return pend, anchorSet, nil
}

// finalizeDerivedHits applies the spatial filter (dropping hits whose anchor
// entity is not in the nearby set, when one is provided) and attaches the
// anchor entity ref so an agent can seed-then-traverse from a derived memory.
func finalizeDerivedHits(pend []derivedPending, nearby map[string]struct{}, refs map[string]EntityRef) []Hit {
	hits := make([]Hit, 0, len(pend))
	for i := range pend {
		anchorUID := pend[i].anchorUID
		if nearby != nil {
			if _, ok := nearby[anchorUID]; !ok {
				continue
			}
		}
		if ref, ok := refs[anchorUID]; ok {
			pend[i].hit.Entities = []EntityRef{ref}
		}
		hits = append(hits, pend[i].hit)
	}
	return hits
}

// lookupEntityRefs batch-loads the display name + kind for the given entity
// UIDs, scope-filtered, so a derived memory can carry its anchor entity as a
// seed-then-traverse pivot. UIDs that are missing or out of scope are simply
// absent from the result; the caller emits the hit without an anchor.
func (h *HybridQuery) lookupEntityRefs(ctx context.Context, uids []string) (map[string]EntityRef, error) {
	if len(uids) == 0 {
		return map[string]EntityRef{}, nil
	}
	placeholders, args := inClauseStrings(uids)
	scopePred, scopeArgs := scopesFromContext(ctx).Predicate("")
	args = append(args, scopeArgs...)
	q := fmt.Sprintf( //nolint:gosec // G201: only "?" placeholders splice in; scope is constant fragments + bound ? params; all values parameterized
		"SELECT uid, kind, display_name FROM entities WHERE uid IN (%s) AND %s",
		placeholders, scopePred,
	)
	rows, err := h.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("kernel: hybrid: lookupEntityRefs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]EntityRef, len(uids))
	for rows.Next() {
		var ref EntityRef
		if err := rows.Scan(&ref.UID, &ref.Kind, &ref.DisplayName); err != nil {
			return nil, fmt.Errorf("kernel: hybrid: lookupEntityRefs scan: %w", err)
		}
		out[ref.UID] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kernel: hybrid: lookupEntityRefs rows: %w", err)
	}
	return out, nil
}

// prefixCitations turns bare source ULIDs into typed AIP resource names so an
// agent can pivot a derived memory's provenance straight into get/neighbors/
// relations. Entries that are already typed (contain a "/") or empty pass
// through unchanged.
func prefixCitations(prefix string, uids []string) []string {
	if len(uids) == 0 {
		return nil
	}
	out := make([]string, 0, len(uids))
	for _, u := range uids {
		switch {
		case u == "":
			continue
		case strings.Contains(u, "/"):
			out = append(out, u)
		default:
			out = append(out, prefix+u)
		}
	}
	return out
}

// decodeSourceUids parses the JSON array of source ULIDs stored on a derived
// memory into citation strings. Returns nil on a malformed value (the row still
// surfaces; the provenance is simply unavailable for that hit).
func decodeSourceUids(raw string) []string {
	if raw == "" {
		return nil
	}
	var uids []string
	if err := json.Unmarshal([]byte(raw), &uids); err != nil {
		return nil
	}
	return uids
}

// recordAccesses fires the AccessRecorder for the final, capped hit set so the
// surfaced memories are reinforced. Best-effort and non-blocking: the recorder
// implementation owns batching, debouncing, and async dispatch, so recall never
// waits. No-op when no recorder is installed or the hit set is empty.
func (h *HybridQuery) recordAccesses(ctx context.Context, hits []Hit) {
	if h.accessSink == nil || len(hits) == 0 {
		return
	}
	uids := make([]string, 0, len(hits))
	seen := make(map[string]struct{}, len(hits))
	for i := range hits {
		uid := hits[i].UID
		if uid == "" {
			continue
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return
	}
	h.accessSink.RecordAccess(ctx, uids)
}
