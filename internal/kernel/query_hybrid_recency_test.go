package kernel

import (
	"math"
	"testing"
	"time"
)

// TestApplyRecencyBoost_PrefersNewerEpisodes reproduces the recency
// failure mode behind TEST 5 ("how long ago did I work at X?"): two
// hits with identical vec/FTS/graph scores rank identically without a
// time-decay term, so the LLM has no signal to prefer the newer one.
// After the Gaussian multiplier lands, the newer episode's FusedScore
// is strictly higher and its Components.Recency is populated.
func TestApplyRecencyBoost_PrefersNewerEpisodes(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	older := Hit{UID: "01OLDER0000000000000000001", FusedScore: 0.5, CreateTime: now.AddDate(0, -6, 0)} // 6 months back
	newer := Hit{UID: "01NEWER0000000000000000001", FusedScore: 0.5, CreateTime: now.AddDate(0, 0, -3)} // 3 days back
	hits := []Hit{older, newer}

	out := applyRecencyBoost(hits, now, 0.15, 30.0)

	if len(out) != 2 {
		t.Fatalf("got %d hits, want 2", len(out))
	}
	// Find each by UID; out is order-preserving so [0] is older, [1] newer.
	if out[0].FusedScore >= out[1].FusedScore {
		t.Errorf("newer hit not boosted above older: older=%.4f newer=%.4f",
			out[0].FusedScore, out[1].FusedScore)
	}
	if out[1].Components.Recency <= 0 {
		t.Errorf("newer hit Components.Recency = %.4f, want > 0", out[1].Components.Recency)
	}
	// Newer episode is 3 days from anchor; with σ=30 the weight is
	// exp(-(3/30)²/2) ≈ 0.995, so the multiplier ≈ 1 + 0.15·0.995 ≈ 1.149.
	want := 0.5 * (1.0 + 0.15*math.Exp(-(3.0*3.0)/(2.0*30.0*30.0)))
	if math.Abs(float64(out[1].FusedScore)-want) > 1e-4 {
		t.Errorf("newer FusedScore = %.6f, want ≈ %.6f", out[1].FusedScore, want)
	}
}

// TestApplyRecencyBoost_DisabledWhenBoostZero confirms the
// short-circuit: setting recencyBoost=0 leaves the input unchanged
// (the kernel default and the config-disable path).
func TestApplyRecencyBoost_DisabledWhenBoostZero(t *testing.T) {
	now := time.Now().UTC()
	hits := []Hit{
		{UID: "01A", FusedScore: 0.9, CreateTime: now.AddDate(-1, 0, 0)},
		{UID: "01B", FusedScore: 0.5, CreateTime: now},
	}
	out := applyRecencyBoost(hits, now, 0.0, 30.0)
	for i := range hits {
		if out[i].FusedScore != hits[i].FusedScore {
			t.Errorf("boost=0: out[%d].FusedScore=%.4f, want %.4f (unchanged)",
				i, out[i].FusedScore, hits[i].FusedScore)
		}
		if out[i].Components.Recency != 0 {
			t.Errorf("boost=0: out[%d].Components.Recency=%.4f, want 0", i, out[i].Components.Recency)
		}
	}
}

// TestApplyRecencyBoost_ZeroCreateTimeSkipped guards against junk
// CreateTime values (a Hit with no createTime in storage) producing
// nonsensical millennium-old Δt and zeroing the multiplier. Those
// hits should pass through unchanged rather than be penalised.
func TestApplyRecencyBoost_ZeroCreateTimeSkipped(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	hits := []Hit{
		{UID: "01ZERO00000000000000000001", FusedScore: 0.5}, // CreateTime zero
	}
	out := applyRecencyBoost(hits, now, 0.15, 30.0)
	if out[0].FusedScore != 0.5 {
		t.Errorf("zero CreateTime should pass through: got %.4f, want 0.5", out[0].FusedScore)
	}
}
