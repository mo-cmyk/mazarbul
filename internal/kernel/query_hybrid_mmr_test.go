package kernel

import (
	"testing"
	"time"
)

// TestApplyMMR_DiversifiesNearDuplicates reproduces the user-reported
// "Arwen pollution" failure: 5 near-identical episodes sitting
// side-by-side in the RRF-fused top-K drown out the actual answer for
// an unrelated query because the LLM reranker can't disambiguate
// duplicates faster than it can ignore the one distinct hit. MMR with
// λ=0.7 should keep the top scorer of the near-duplicate cluster but
// elevate the diverse hit into the top-2.
//
// This test fails before applyMMR lands and passes after.
func TestApplyMMR_DiversifiesNearDuplicates(t *testing.T) {
	now := time.Now().UTC()
	// Five near-identical pollution hits, ranked highest in fused score.
	pollution := []Hit{
		{UID: "01POLLUTION0000000000000001", Snippet: "Arwen working at Rivendell 1", FusedScore: 0.95, CreateTime: now.Add(-7 * 24 * time.Hour)},
		{UID: "01POLLUTION0000000000000002", Snippet: "Arwen working at Rivendell 2", FusedScore: 0.93, CreateTime: now.Add(-7 * 24 * time.Hour)},
		{UID: "01POLLUTION0000000000000003", Snippet: "Arwen working at Rivendell 3", FusedScore: 0.91, CreateTime: now.Add(-7 * 24 * time.Hour)},
		{UID: "01POLLUTION0000000000000004", Snippet: "Arwen working at Rivendell 4", FusedScore: 0.89, CreateTime: now.Add(-7 * 24 * time.Hour)},
		{UID: "01POLLUTION0000000000000005", Snippet: "Arwen working at Rivendell 5", FusedScore: 0.87, CreateTime: now.Add(-7 * 24 * time.Hour)},
	}
	// The diverse hit — lower base score, but textually unrelated.
	diverse := Hit{UID: "01DIVERSE0000000000000000A1", Snippet: "I live in Berlin", FusedScore: 0.75, CreateTime: now}
	pool := append(append([]Hit{}, pollution...), diverse)

	out := applyMMR(pool, 0.7, 3)

	// The diverse hit MUST sit in the top-2 — without MMR it lands at
	// rank 6, with MMR it should appear after the highest-scoring
	// pollution representative.
	if len(out) < 2 {
		t.Fatalf("MMR returned %d hits, want >= 2", len(out))
	}
	foundDiverseInTop2 := false
	for _, h := range out[:2] {
		if h.UID == diverse.UID {
			foundDiverseInTop2 = true
			break
		}
	}
	if !foundDiverseInTop2 {
		t.Errorf("diverse hit %q not in top-2 after MMR; got %v", diverse.UID, uidsOf(out))
	}

	// The top-1 should still be a pollution representative (highest
	// fused score wins the first slot — MMR diversifies subsequent picks).
	if out[0].UID != pollution[0].UID {
		t.Errorf("top-1 = %q, want %q (highest fused score should always be picked first)",
			out[0].UID, pollution[0].UID)
	}
}

// TestApplyMMR_LambdaOneEqualsRRFOrdering confirms that with λ=1 MMR
// reduces to the original fused-score ordering (no diversity term).
// This is the safety check: setting MMRLambda=1.0 in config recovers
// the pre-MMR behaviour exactly.
func TestApplyMMR_LambdaOneEqualsRRFOrdering(t *testing.T) {
	hits := []Hit{
		{UID: "01A", Snippet: "alpha", FusedScore: 0.9},
		{UID: "01B", Snippet: "alpha 2", FusedScore: 0.8},
		{UID: "01C", Snippet: "beta", FusedScore: 0.7},
	}
	out := applyMMR(hits, 1.0, 3)
	for i, want := range hits {
		if out[i].UID != want.UID {
			t.Errorf("λ=1: out[%d]=%q, want %q (should match RRF order)", i, out[i].UID, want.UID)
		}
	}
}

// TestApplyMMR_DisabledWhenLambdaZero confirms that with λ=0 the
// algorithm short-circuits and returns the input unchanged. Used to
// disable MMR via the config knob without code changes.
func TestApplyMMR_DisabledWhenLambdaZero(t *testing.T) {
	hits := []Hit{
		{UID: "01A", FusedScore: 0.9},
		{UID: "01B", FusedScore: 0.8},
	}
	out := applyMMR(hits, 0.0, 5)
	if len(out) != len(hits) {
		t.Errorf("λ=0 should pass through, got %d hits, want %d", len(out), len(hits))
	}
	for i := range hits {
		if out[i].UID != hits[i].UID {
			t.Errorf("λ=0: out[%d]=%q, want %q", i, out[i].UID, hits[i].UID)
		}
	}
}

func uidsOf(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.UID
	}
	return out
}
