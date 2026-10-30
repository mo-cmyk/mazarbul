package kernel

import (
	"math"
	"time"
)

// applyRecencyBoost multiplies each hit's FusedScore by a Gaussian
// time-decay term anchored at now, and stamps the unit-normalized
// weight into Components.Recency for observability.
//
// Formula:
//
//	weight     = exp(-(Δdays)² / (2·σ²))
//	multiplier = 1 + boost · weight   ∈ [1, 1+boost]
//	FusedScore = FusedScore · multiplier
//
// σ is the half-life knob (units: days; Elastic/OpenSearch
// function_score gauss with scale=σ, decay≈0.5 around σ). Boost is a
// fixed scalar in [0, 1] controlling how much weight to give recency
// relative to the other lanes. Default σ=30 days, boost=0.15 — recency
// is a *tiebreaker*, never the dominant signal, because the production
// failure mode this addresses is two near-identical hits whose only
// distinguishing feature is age.
//
// Disabled path:
//
//   - boost <= 0: return hits unchanged. Used as the config kill-switch.
//   - hit.CreateTime.IsZero(): the hit's createTime never made it out of
//     storage (typically a synthetic test hit). Pass through so junk
//     timestamps don't penalise the score.
//
// The input slice is mutated in place — callers already own the slice
// (it's the post-RRF working set) and this keeps allocations off the
// hot path.
func applyRecencyBoost(hits []Hit, now time.Time, boost, halflifeDays float64) []Hit {
	if boost <= 0 || halflifeDays <= 0 {
		return hits
	}
	sigmaSq := halflifeDays * halflifeDays
	for i := range hits {
		if hits[i].CreateTime.IsZero() {
			continue
		}
		deltaDays := now.Sub(hits[i].CreateTime).Hours() / 24.0
		if deltaDays < 0 {
			// Future-dated episode (clock skew / pinned test data).
			// Treat as "now" so the recency weight peaks at 1.0
			// rather than over-boosting on a negative delta.
			deltaDays = 0
		}
		weight := math.Exp(-(deltaDays * deltaDays) / (2.0 * sigmaSq))
		multiplier := 1.0 + boost*weight
		hits[i].FusedScore = float32(float64(hits[i].FusedScore) * multiplier)
		hits[i].Components.Recency = float32(weight)
	}
	return hits
}
