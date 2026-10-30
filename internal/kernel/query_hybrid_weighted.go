package kernel

import "math"

// Phase 4 of the mem0-parity plan: weighted score fusion as an
// alternative to reciprocal-rank fusion (RRF).
//
// Mem0 v3's published recipe ("0.5·semantic + 0.3·BM25 + 0.2·entity")
// is marketing; the actual algorithm in mem0/utils/scoring.py is an
// adaptive normalization that keeps the fused score in [0, 1] — directly
// comparable to a single lane's cosine score — by dividing the per-lane
// sum by `max_possible`, where `max_possible` grows only for lanes that
// fired for *this* candidate. The result is that an abstain threshold
// tuned against semantic cosine scores carries over to the fused output
// without rescaling.
//
// Two iterations:
//
//  1. Initial port (2026-06-01): per-lane scores were rank-based
//     (1/rank). This kept the [0, 1] range but lost magnitude — a
//     "barely-related rank-1" and a "perfect-match rank-1" landed on
//     the same fused score. Discovered via the LongMemEval 3q dry-run
//     where Q2 ("how many antique items") returned a real grounded
//     answer pre-fix but abstained post-rank-quantization.
//
//  2. Raw-score plumbing (2026-06-01, Phase 1 of the parity sweep):
//     semantic now consumes raw cosine similarity in [0, 1] derived from
//     the vec0 L2 distance via `1 - L2²/2` for L2-normalized embeddings
//     (BGE-M3 is the M1 default and L2-normalizes). BM25 consumes the
//     mem0 query-length-adaptive sigmoid (3 tokens → midpoint 5.0,
//     steepness 0.7; 15 tokens → 12.0, 0.5). Entity stays rank-based
//     because the graph + name-vector lanes don't expose a meaningful
//     raw similarity at the fusion site — they're boolean-with-rank
//     signals capped at entityBoostWeight = 0.5.

// entityBoostWeight matches mem0's ENTITY_BOOST_WEIGHT = 0.5: the
// entity-overlap lane is treated as a half-weight booster on top of
// semantic + lexical evidence, not an independent ranker. Two
// consequences:
//
//   - The maximum entity contribution is 0.5, ensuring entity matches
//     cannot dominate fusion when semantic similarity is weak.
//   - The divisor adds 0.5 (not 1.0) when the entity lane fires, so
//     entity-only candidates can't artificially inflate to a top score.
const entityBoostWeight = 0.5

// WeightedComponents folds raw semantic + raw BM25 (both already
// normalized to [0, 1]) and the per-lane entity ranks (graph,
// nameEmb) into a single fused score in [0, 1] using mem0 v3's
// adaptive normalization. A rank of 0 means the candidate did not
// appear in that entity lane; a raw score of 0 means the candidate
// did not appear in that lane (or was orthogonal).
//
// Algorithm:
//
//	sem_score   = rawSemantic                                 (already in [0, 1])
//	bm25_score  = rawBM25                                     (already in [0, 1], sigmoid-normalized upstream)
//	entity_best = min nonzero of graphRank, nameEmbRank
//	entity_score = entityBoostWeight * (1/entity_best) if any entity lane fired else 0
//
//	max_possible = 1.0                                        (semantic always counted — mem0 contract)
//	             + 1.0 if BM25 fired                          (rawBM25 > 0)
//	             + entityBoostWeight if entity fired
//
//	fused = (sem_score + bm25_score + entity_score) / max_possible    clamped to [0, 1]
//
// The semantic divisor is always present even when rawSemantic = 0
// because mem0's scoring treats the vector lane as the primary signal
// and uses `max_possible = 1.0` as the floor (a candidate that only
// matched via FTS still gets compared on the "semantic-was-possible"
// scale). This is the load-bearing detail that keeps thresholds stable
// across lane-sparsity regimes.
func WeightedComponents(rawSemantic, rawBM25 float32, graphRank, nameEmbRank int) float32 {
	semScore := float64(clampUnit(rawSemantic))
	bm25Score := float64(clampUnit(rawBM25))

	entityBest := graphRank
	if nameEmbRank > 0 && (entityBest == 0 || nameEmbRank < entityBest) {
		entityBest = nameEmbRank
	}
	entityScore := 0.0
	if entityBest > 0 {
		entityScore = entityBoostWeight * rankScore(entityBest)
	}

	// maxPossible scales with the magnitude of each contribution rather
	// than whether the lane fired at all. The pre-fix `if bm25 > 0:
	// max += 1.0` inflation halved the fused score of an only-matching
	// document when its BM25 sigmoid was tiny (0.07 for a single token
	// in a short doc), pushing it below non-matching documents whose
	// semantic-only score divided by max=1.0. Discovered 2026-06-01
	// stress-testing recall on real personal memories ("Rosie" query
	// landed Rosie at rank 8/8). Pinned by
	// TestWeightedComponents_BM25BonusIsMonotonic and
	// TestWeightedComponents_SoleMatchBeatsNonMatches.
	//
	// Magnitude-scaled formulation preserves every existing invariant
	// (perfect-match=1.0, semantic-only-stays-cosine-comparable,
	// entity-only-capped, all-empty=0) while restoring the load-bearing
	// monotonicity contract: adding evidence cannot decrease the score.
	maxPossible := 1.0 + bm25Score + entityScore

	fused := (semScore + bm25Score + entityScore) / maxPossible
	if fused > 1.0 {
		fused = 1.0
	}
	if fused < 0 {
		fused = 0
	}
	return float32(fused)
}

// rankScore is the per-lane scoring kernel for the entity lane:
// 1/rank when the candidate appeared (rank-1 = 1.0, rank-2 = 0.5,
// rank-10 = 0.1), zero otherwise. The entity lane stays rank-based
// because the graph + name-vector traversal lanes don't expose a
// meaningful raw similarity at the fusion site.
func rankScore(rank int) float64 {
	if rank <= 0 {
		return 0
	}
	return 1.0 / float64(rank)
}

// clampUnit clips a raw score to [0, 1] without surfacing the clip as
// an error. A negative input becomes 0 (treated as "lane absent");
// a >1 input becomes 1 (a defensive ceiling — neither cosine nor the
// sigmoid should produce >1, but the floating-point path can sneak in
// rounding noise on tightly-clustered inputs).
func clampUnit(x float32) float32 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// CosineSimilarityFromDistance converts an sqlite-vec L2 distance into
// a cosine similarity in [0, 1]. BGE-M3 — the M1 default embedder —
// emits L2-normalized vectors, so the identity `cos_sim = 1 - L2²/2`
// holds exactly. Negative similarity (vectors > 90° apart) is clamped
// to 0 because fusion treats negative similarity as "absent lane"; a
// negative semantic contribution would otherwise drag the fused score
// below the pre-fusion gate inconsistently.
//
// If a future embedder lands that does NOT L2-normalize, the migration
// adds a distance_metric=cosine constraint on the vec0 DDL and this
// function returns `1 - distance` directly (linear cosine distance).
func CosineSimilarityFromDistance(distance float32) float32 {
	sim := 1.0 - float64(distance)*float64(distance)/2.0
	if sim < 0 {
		return 0
	}
	if sim > 1 {
		return 1
	}
	return float32(sim)
}

// BM25Sigmoid normalizes a raw FTS5 BM25 score into [0, 1] using a
// logistic sigmoid whose midpoint and steepness adapt to the query
// length. Matches mem0/utils/scoring.py get_bm25_params:
//
//	queryTokens =  3  → midpoint 5.0,  steepness 0.7
//	queryTokens = 15  → midpoint 12.0, steepness 0.5
//	in-between values interpolate linearly; outside values clamp.
//
// The intuition: a 3-token query that matches BM25 ≈ 5 is "strong";
// a 15-token query that matches BM25 ≈ 5 is "weak" because longer
// queries naturally produce higher raw scores. Without this
// adaptation, short queries underweight BM25 and long queries
// overweight it — a known mem0 v3 finding.
//
// The output is in [0, 1] regardless of input magnitude, which lets
// WeightedComponents add it to the semantic lane's cosine without
// extra rescaling.
func BM25Sigmoid(rawBM25 float32, queryTokens int) float32 {
	t := float64(queryTokens)
	if t < 3 {
		t = 3
	}
	if t > 15 {
		t = 15
	}
	frac := (t - 3) / 12.0
	midpoint := 5.0 + frac*(12.0-5.0)
	steepness := 0.7 - frac*(0.7-0.5)
	x := (float64(rawBM25) - midpoint) / steepness
	// math.Exp on large negative arguments returns 0 cleanly; on large
	// positive returns +Inf and the (1 + Inf) denominator drives the
	// expression to 0. Both extremes saturate as expected.
	return float32(1.0 / (1.0 + math.Exp(-x)))
}
