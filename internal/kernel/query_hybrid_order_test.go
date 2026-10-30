package kernel

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// reversingReranker is a stub Reranker that reverses the hit order to prove
// the reranker fired when rerankEnabled=true.
type reversingReranker struct{}

func (r *reversingReranker) Rerank(_ context.Context, _ string, hits []Hit) ([]Hit, error) {
	out := make([]Hit, len(hits))
	for i, h := range hits {
		out[len(hits)-1-i] = h
	}
	return out, nil
}

// TestApplyReranker_GateControlsReranker pins the reranker-gate contract that
// #195 introduces: a reranker is configured unconditionally (chatModel != ""),
// but it only reorders hits when rerankEnabled is true. With the gate off (the
// default) applyReranker is a no-op and the deterministic fused order survives;
// with the gate on the reversingReranker fires and inverts the order.
func TestApplyReranker_GateControlsReranker(t *testing.T) {
	t.Parallel()
	in := []Hit{{UID: "a"}, {UID: "b"}, {UID: "c"}}

	// Gate off (default): reranker present but must not fire.
	off := &HybridQuery{reranker: &reversingReranker{}}
	got, err := off.applyReranker(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("gate off: applyReranker err = %v, want nil", err)
	}
	if len(got) != 3 || got[0].UID != "a" || got[2].UID != "c" {
		t.Errorf("gate off: order = %v, want [a b c] unchanged", uidsOf(got))
	}

	// Gate on: reranker fires and reverses the order.
	on := &HybridQuery{reranker: &reversingReranker{}, rerankEnabled: true}
	got2, err := on.applyReranker(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("gate on: applyReranker err = %v, want nil", err)
	}
	if len(got2) != 3 || got2[0].UID != "c" || got2[2].UID != "a" {
		t.Errorf("gate on: order = %v, want [c b a] reversed (reranker fired)", uidsOf(got2))
	}
}

// TestHybridQuery_Recall_DeterministicOrder is the regression guard for the
// non-determinism bug: same query, same settled DB, hits must come out in the
// same order across every run, and that order must be monotonically
// non-increasing by FusedScore (with UID-ascending as the tiebreak for equal
// scores).
//
// Root cause: collectHits builds the scored []Hit by ranging over a
// map[string]hydratedRow (keep).  Go randomises map iteration, so the
// pre-sort input order is non-deterministic.  The intermediate sort at the
// top of the pipeline (before attachEntities) uses sort.SliceStable with a
// (FusedScore desc, UID asc) comparator, which IS a total order and therefore
// deterministic for any input — but only when the comparator key (FusedScore)
// is not mutated by a later step (such as applyRecencyBoost) that does not
// re-apply the same total-order sort afterwards.
//
// The defensive final sort (added after attachEntities) guarantees the output
// order is correct and deterministic regardless of what intermediate steps do
// to FusedScore or slice permutation, as long as MMR and reranker are inactive
// (when they ARE configured their reordering is intentional).
//
// Scenario A: five episodes with equal FusedScores (Score:0 → rawSemantic
// 1.0 for all).  The ONLY deterministic ordering is UID ascending.  Runs 20×
// and asserts order is identical every time.
//
// Scenario B: five episodes with different FusedScores; recency boost ON so
// applyRecencyBoost mutates FusedScore AFTER the intermediate sort.  With all
// hits having IsZero CreateTime the recency multiplier is a no-op, so the
// original ordering is expected.  Asserts monotonic non-increasing across 20
// runs.
func TestHybridQuery_Recall_DeterministicOrder(t *testing.T) {
	t.Parallel()

	t.Run("equal fused scores UID tiebreak is stable", func(t *testing.T) {
		t.Parallel()

		const runs = 20

		// Five episodes with UIDs ep0..ep4.  Score:0 → rawSemantic = 1.0 for all
		// under weighted fusion, so FusedScores are all identical (1.0).
		// The ONLY discriminating key is UID; expect ep0 < ep1 < … < ep4.
		episodes := make([]seedEpisode, 5)
		vecHits := make([]VectorHit, 5)
		for i := range 5 {
			uid := fmt.Sprintf("ep%d", i)
			episodes[i] = seedEpisode{uid: uid, kind: "remember", confidence: 1.0, text: uid + " text"}
			vecHits[i] = VectorHit{ID: uid, Score: 0}
		}

		vec := &stubVector{hits: vecHits}
		fts := &stubFTS{}
		h := newTestHybrid(t, episodes, vec, fts)

		var reference []string
		for run := range runs {
			hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "text", Limit: 5}))
			if err != nil {
				t.Fatalf("run %d: recall error: %v", run, err)
			}
			if len(hits) != 5 {
				t.Fatalf("run %d: got %d hits, want 5", run, len(hits))
			}

			// Scores must be monotonically non-increasing.
			for i := 1; i < len(hits); i++ {
				if hits[i].FusedScore > hits[i-1].FusedScore {
					t.Errorf("run %d: hit[%d].FusedScore %v > hit[%d].FusedScore %v (not sorted)",
						run, i, hits[i].FusedScore, i-1, hits[i-1].FusedScore)
				}
			}

			order := make([]string, len(hits))
			for i, h := range hits {
				order[i] = h.UID
			}
			if run == 0 {
				reference = order
				continue
			}
			for i := range order {
				if order[i] != reference[i] {
					t.Errorf("run %d: position %d = %q, run 0 had %q (non-deterministic order)",
						run, i, order[i], reference[i])
					break
				}
			}
		}

		// The expected stable order is UID ascending (aaa < bbb < … for equal scores).
		for i := 1; i < len(reference); i++ {
			if reference[i] <= reference[i-1] {
				t.Errorf("reference order at %d: %q not > %q (expected UID-asc tiebreak)",
					i, reference[i], reference[i-1])
			}
		}
	})

	t.Run("distinct scores monotonically non-increasing across runs", func(t *testing.T) {
		t.Parallel()

		const runs = 20

		// Three episodes with clearly different L2 distances → different rawSemantic
		// → different FusedScores under weighted fusion.
		//   "best":   Score 0.1 → cos 1 - 0.01/2 = 0.995
		//   "middle": Score 0.7 → cos 1 - 0.49/2 = 0.755
		//   "worst":  Score 1.2 → cos 1 - 1.44/2 = 0.28
		episodes := []seedEpisode{
			{uid: "best", kind: "remember", confidence: 1.0, text: "best match text"},
			{uid: "middle", kind: "remember", confidence: 1.0, text: "middle match text"},
			{uid: "worst", kind: "remember", confidence: 1.0, text: "worst match text"},
		}
		vec := &stubVector{hits: []VectorHit{
			{ID: "best", Score: 0.1},
			{ID: "middle", Score: 0.7},
			{ID: "worst", Score: 1.2},
		}}
		fts := &stubFTS{}
		h := newTestHybridWithEmbedder(t, stubEmbedder{}, vec, fts, episodes,
			WithFusionMode(FusionModeWeighted))

		var reference []string
		for run := range runs {
			hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "match", Limit: 3}))
			if err != nil {
				t.Fatalf("run %d: recall error: %v", run, err)
			}
			if len(hits) != 3 {
				t.Fatalf("run %d: got %d hits, want 3", run, len(hits))
			}

			for i := 1; i < len(hits); i++ {
				if hits[i].FusedScore > hits[i-1].FusedScore {
					t.Errorf("run %d: hit[%d].FusedScore %v > hit[%d].FusedScore %v (not sorted)",
						run, i, hits[i].FusedScore, i-1, hits[i-1].FusedScore)
				}
			}

			order := make([]string, len(hits))
			for i, h := range hits {
				order[i] = h.UID
			}
			if run == 0 {
				reference = order
				continue
			}
			for i := range order {
				if order[i] != reference[i] {
					t.Errorf("run %d: position %d = %q, run 0 had %q (non-deterministic order)",
						run, i, order[i], reference[i])
					break
				}
			}
		}

		// The reference order must be best → middle → worst (descending score).
		if len(reference) == 3 {
			if reference[0] != "best" || reference[1] != "middle" || reference[2] != "worst" {
				t.Errorf("reference order = %v, want [best middle worst]", reference)
			}
		}
	})

	t.Run("recency boost does not scramble order", func(t *testing.T) {
		t.Parallel()

		const runs = 20

		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		// Three episodes with different L2 distances AND different CreateTimes.
		// recencyBoost > 0 will mutate FusedScore after the intermediate sort;
		// the final defensive sort must restore deterministic ordering.
		episodes := []seedEpisode{
			{uid: "alpha", kind: "remember", confidence: 1.0, text: "alpha"},
			{uid: "beta", kind: "remember", confidence: 1.0, text: "beta"},
			{uid: "gamma", kind: "remember", confidence: 1.0, text: "gamma"},
		}
		db := openMemoryEpisodes(t)
		// Overwrite create_time for each episode to known values so the
		// recency multiplier is deterministic.
		seedEpisodes(t, db, episodes)

		// Re-seed with explicit create_time values (seedEpisodes uses time.Now()).
		for i, uid := range []string{"alpha", "beta", "gamma"} {
			ct := now.AddDate(0, 0, -i*10).Format("2006-01-02T15:04:05Z")
			if _, err := db.ExecContext(t.Context(),
				"UPDATE episodes SET create_time = ? WHERE uid = ?", ct, uid); err != nil {
				t.Fatalf("update create_time %s: %v", uid, err)
			}
		}

		vec := &stubVector{hits: []VectorHit{
			{ID: "alpha", Score: 0.1},
			{ID: "beta", Score: 0.6},
			{ID: "gamma", Score: 1.0},
		}}
		fts := &stubFTS{}
		h2, err := NewHybridQuery(stubEmbedder{}, vec, fts, db,
			WithFusionMode(FusionModeWeighted),
			WithRecencyBoost(0.15),
			WithRecencyHalflife(30.0),
		)
		if err != nil {
			t.Fatalf("NewHybridQuery: %v", err)
		}

		cd := now
		var reference []string
		for run := range runs {
			hits, err := drain(h2.Recall(t.Context(), RecallRequest{
				Query:       "text",
				Limit:       3,
				CurrentDate: &cd,
			}))
			if err != nil {
				t.Fatalf("run %d: recall error: %v", run, err)
			}
			if len(hits) != 3 {
				t.Fatalf("run %d: got %d hits, want 3", run, len(hits))
			}

			for i := 1; i < len(hits); i++ {
				if hits[i].FusedScore > hits[i-1].FusedScore {
					t.Errorf("run %d: hit[%d].FusedScore %v > hit[%d].FusedScore %v (not sorted)",
						run, i, hits[i].FusedScore, i-1, hits[i-1].FusedScore)
				}
			}

			order := make([]string, len(hits))
			for i, h := range hits {
				order[i] = h.UID
			}
			if run == 0 {
				reference = order
				continue
			}
			for i := range order {
				if order[i] != reference[i] {
					t.Errorf("run %d: position %d = %q, run 0 had %q (non-deterministic order)",
						run, i, order[i], reference[i])
					break
				}
			}
		}
	})
}
