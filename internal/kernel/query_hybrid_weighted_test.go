package kernel_test

import (
	"math"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// TestWeightedComponents_PerfectMatchIsCosineComparable pins the load-bearing
// architectural invariant from mem0 v3's scoring.py: a candidate that maxes
// every lane (cosine 1.0, normalized BM25 1.0, rank-1 in entity lanes) should
// produce a fused score of exactly 1.0 — cosine-comparable. This is the
// signal that an abstain threshold tuned against raw cosine scores carries
// over to the fused output without rescaling.
func TestWeightedComponents_PerfectMatchIsCosineComparable(t *testing.T) {
	t.Parallel()
	// rawSemantic=1, rawBM25=1, entity rank-1 in both graph + nameEmb.
	// max_possible = 1.0 (sem) + 1.0 (bm25) + 0.5 (entity) = 2.5
	// raw          = 1.0 + 1.0 + 0.5 = 2.5
	// combined     = 2.5 / 2.5 = 1.0
	got := kernel.WeightedComponents(1.0, 1.0, 1, 1)
	const want = float32(1.0)
	if math.Abs(float64(got-want)) > 1e-6 {
		t.Errorf("perfect-match fused score = %v, want %v (sum/max_possible normalization)", got, want)
	}
}

// TestWeightedComponents_RawCosineMagnitudePreserved is the headline Phase 1
// regression test. Two candidates rank #1 in every lane but differ on raw
// cosine (0.95 vs 0.60). The previous rank-quantized implementation produced
// identical fused scores for both. The mem0-faithful version must
// distinguish them.
//
// Discovered 2026-06-01 reviewing mem0/utils/scoring.py: BM25 and entity
// scores rescue magnitude-distinguished candidates only if the semantic
// lane keeps the magnitude. Rank-based scoring throws it away.
func TestWeightedComponents_RawCosineMagnitudePreserved(t *testing.T) {
	t.Parallel()
	// Both rank-1 in every other lane; only rawSemantic differs.
	strong := kernel.WeightedComponents(0.95, 1.0, 1, 1)
	weak := kernel.WeightedComponents(0.60, 1.0, 1, 1)
	if !(strong > weak) {
		t.Errorf("magnitude must propagate through fusion: strong (cos 0.95)=%v <= weak (cos 0.60)=%v", strong, weak)
	}
	// Quantitative sanity: ratio should reflect the input difference.
	// strong: (0.95 + 1.0 + 0.5) / 2.5 = 0.98
	// weak:   (0.60 + 1.0 + 0.5) / 2.5 = 0.84
	const wantStrong, wantWeak = float32(0.98), float32(0.84)
	if math.Abs(float64(strong-wantStrong)) > 1e-4 {
		t.Errorf("strong fused = %v, want %v", strong, wantStrong)
	}
	if math.Abs(float64(weak-wantWeak)) > 1e-4 {
		t.Errorf("weak fused = %v, want %v", weak, wantWeak)
	}
}

// TestWeightedComponents_OnlySemanticFiresStaysInUnitInterval pins
// adaptive-divisor semantics: when only the semantic lane fires for a
// candidate, max_possible shrinks to 1.0 so the fused score still reflects
// the lane's raw cosine rather than being diluted by zero contributions
// from absent lanes. mem0's `has_bm25 / has_entity` flags drive this; our
// equivalent is "rank > 0" (or "rawBM25 > 0").
func TestWeightedComponents_OnlySemanticFiresStaysInUnitInterval(t *testing.T) {
	t.Parallel()
	// Pure semantic, cosine 0.8. Expected: 0.8 / 1.0 = 0.8.
	got := kernel.WeightedComponents(0.8, 0, 0, 0)
	const want = float32(0.8)
	if math.Abs(float64(got-want)) > 1e-6 {
		t.Errorf("semantic-only fused score = %v, want %v (adaptive divisor must shrink with absent lanes)", got, want)
	}
}

// TestWeightedComponents_AllLanesEmptyIsZero pins the "no lanes fired"
// floor: a candidate that's absent from every lane scores 0. This is
// load-bearing for the pre-fusion gate: filtered candidates simply
// vanish from the result set, and any consumer that scans for the top
// fused score gets 0 (not a NaN).
func TestWeightedComponents_AllLanesEmptyIsZero(t *testing.T) {
	t.Parallel()
	got := kernel.WeightedComponents(0, 0, 0, 0)
	if got != 0 {
		t.Errorf("all-lanes-empty fused score = %v, want 0", got)
	}
}

// TestWeightedComponents_EntityLaneCappedAtHalf pins that the entity
// contribution (graph + nameEmb best rank) is capped at the entity boost
// weight (0.5), mirroring mem0's ENTITY_BOOST_WEIGHT. The two graph-side
// lanes share the cap so they cannot dominate a candidate that's
// semantically weak but entity-rich.
func TestWeightedComponents_EntityLaneCappedAtHalf(t *testing.T) {
	t.Parallel()
	// Pure entity, rank-1 in both graph + nameEmb. Expected entity
	// contribution = 0.5 * (1/1) = 0.5; divisor = 1.0 + 0.5 = 1.5;
	// fused = 0.5 / 1.5 = 0.333.
	got := kernel.WeightedComponents(0, 0, 1, 1)
	const want = float32(0.5 / 1.5)
	if math.Abs(float64(got-want)) > 1e-6 {
		t.Errorf("entity-only fused score = %v, want %v (cap at entityBoostWeight=0.5)", got, want)
	}
}

// TestWeightedComponents_BM25BonusIsMonotonic pins the load-bearing
// recall-quality invariant: adding a BM25 contribution to a candidate
// must NEVER decrease its fused score. The pre-fix formula divided by
// max_possible=2.0 whenever any BM25 fired (even a sigmoid value of
// 0.07 from one keyword in a short document), which halved the fused
// score and pushed the only-matching document below non-matching
// documents whose semantic alone divided by max_possible=1.0.
//
// Discovered 2026-06-01 stress-testing recall on real personal
// memories: query "Rosie" returned Rosie's row at rank 8/8 despite her
// row being the only FTS match. rawSem=0.514, rawBM25=0.071 →
// fused=(0.514+0.071)/2 = 0.293, beaten by Luna's rawSem=0.543/1 =
// 0.543.
//
// Contract: WeightedComponents(s, 0, ·, ·) <= WeightedComponents(s, b, ·, ·)
// for all b >= 0. Adding evidence cannot hurt.
func TestWeightedComponents_BM25BonusIsMonotonic(t *testing.T) {
	t.Parallel()
	const baseSem = float32(0.5)
	withoutBM25 := kernel.WeightedComponents(baseSem, 0, 0, 0)
	withTinyBM25 := kernel.WeightedComponents(baseSem, 0.07, 0, 0)
	withMediumBM25 := kernel.WeightedComponents(baseSem, 0.5, 0, 0)
	withStrongBM25 := kernel.WeightedComponents(baseSem, 0.9, 0, 0)

	if !(withTinyBM25 >= withoutBM25) {
		t.Errorf("tiny BM25 must not decrease fused score: 0.5+0.07 -> %v, 0.5+0 -> %v", withTinyBM25, withoutBM25)
	}
	if !(withMediumBM25 >= withTinyBM25) {
		t.Errorf("monotonicity: medium BM25 should >= tiny BM25; medium=%v tiny=%v", withMediumBM25, withTinyBM25)
	}
	if !(withStrongBM25 >= withMediumBM25) {
		t.Errorf("monotonicity: strong BM25 should >= medium BM25; strong=%v medium=%v", withStrongBM25, withMediumBM25)
	}
}

// TestWeightedComponents_SoleMatchBeatsNonMatches pins the production
// scenario the monotonic test abstracts: when only one document matches
// a keyword query (any BM25 > 0), it must outrank documents with no
// keyword match, all else equal. This is the actual user-visible bug:
// "Rosie" query landed Rosie at rank 8/8 because the divisor inflation
// outweighed the FTS contribution.
func TestWeightedComponents_SoleMatchBeatsNonMatches(t *testing.T) {
	t.Parallel()
	// Rosie: rawSem 0.514 (her self-text's cosine with the query),
	// rawBM25 0.071 (single token match in a 7-word doc, sigmoid low).
	sarah := kernel.WeightedComponents(0.514, 0.071, 0, 0)
	// Luna: rawSem 0.543 (slightly higher cosine because qwen3 is a
	// weak differentiator on short personal facts), rawBM25 0 (no
	// keyword overlap with the query).
	luna := kernel.WeightedComponents(0.543, 0, 0, 0)
	if !(sarah > luna) {
		t.Errorf("FTS-matching candidate (sarah %v) must outrank non-matching (luna %v) — recall is broken otherwise", sarah, luna)
	}
}

// TestBM25Sigmoid_AdaptsToQueryLength pins that the sigmoid normalizer
// uses different midpoint/steepness for 3-term vs 15-term queries (mem0's
// get_bm25_params behaviour). The same raw BM25 magnitude should produce
// a higher normalized score on a short query than on a long query
// (because long queries are expected to score higher on BM25 by raw
// magnitude, so the sigmoid sits further right).
func TestBM25Sigmoid_AdaptsToQueryLength(t *testing.T) {
	t.Parallel()
	const rawBM25 = float32(8.0) // somewhere between the 3-term and 15-term midpoints
	short := kernel.BM25Sigmoid(rawBM25, 3)
	long := kernel.BM25Sigmoid(rawBM25, 15)
	if !(short > long) {
		t.Errorf("3-term query should normalize the same raw BM25 higher than 15-term: short=%v long=%v", short, long)
	}
	// Sanity: both must be in [0, 1].
	if short < 0 || short > 1 {
		t.Errorf("short normalized = %v out of [0,1]", short)
	}
	if long < 0 || long > 1 {
		t.Errorf("long normalized = %v out of [0,1]", long)
	}
}

// TestBM25Sigmoid_RangeBounds pins the sigmoid output bounds for
// pathological inputs: extremely large BM25 → ~1.0; extremely small or
// zero → near 0 / sigmoid(0 - midpoint, steepness). These bounds
// guarantee the normalized score participates in fusion without
// overwhelming the adaptive divisor.
func TestBM25Sigmoid_RangeBounds(t *testing.T) {
	t.Parallel()
	huge := kernel.BM25Sigmoid(1000, 5)
	if !(huge > 0.99) {
		t.Errorf("huge BM25 should saturate near 1.0; got %v", huge)
	}
	// At the midpoint, sigmoid returns 0.5.
	mid := kernel.BM25Sigmoid(5.0, 3) // 3-term query → midpoint 5.0
	if math.Abs(float64(mid-0.5)) > 1e-4 {
		t.Errorf("at midpoint sigmoid should return 0.5; got %v", mid)
	}
}

// TestCosineSimilarityFromDistance pins the L2-distance → cosine-similarity
// conversion that the fusion site uses to feed raw semantic scores into
// WeightedComponents. BGE-M3 emits L2-normalized embeddings, so the
// identity `cos_sim = 1 - L2² / 2` holds. Vectors with distance 0 are
// identical (sim 1.0); distance sqrt(2) is orthogonal (sim 0); distance
// 2 is opposite (sim -1, but we clamp to 0 because fusion treats negative
// similarity as "absent lane").
func TestCosineSimilarityFromDistance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		distance float32
		want     float32
	}{
		{"identical", 0, 1.0},
		{"orthogonal", float32(math.Sqrt(2)), 0},
		{"opposite_clamped", 2.0, 0},
		{"close_match", 0.5, 1.0 - 0.5*0.5/2.0}, // sim = 0.875
	}
	for _, tc := range cases {
		got := kernel.CosineSimilarityFromDistance(tc.distance)
		if math.Abs(float64(got-tc.want)) > 1e-5 {
			t.Errorf("%s: distance=%v → similarity=%v, want %v", tc.name, tc.distance, got, tc.want)
		}
	}
}
