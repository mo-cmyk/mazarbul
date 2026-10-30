//go:build integration

package dispatcher_test

// cognitive_lifecycle_e2e_test.go is the temporary end-to-end validation for the
// cognitive memory lifecycle (issue #231). It drives the FULL event-sourced
// stack — dispatcher apply arms projecting into real SQLite + sqlite-vec + FTS,
// then a real HybridQuery recall over the SAME store — to prove the headline
// acceptance criteria without any LLM dependency (a deterministic keyword
// embedder stands in for Ollama):
//
//  1. Reinforcement ranking: a frequently-accessed episode outranks an
//     equal-relevance peer after N memoryAccessed events.
//  2. Provenance surfacing: a synthesized insight surfaces in hot recall citing
//     its sourceUids.
//  3. Demote-not-delete: the projection-side decay sweep demotes a low-confidence
//     insight so it leaves hot recall — without touching the event log.
//  4. Rebuild restores: replaying the insightSynthesized event into a fresh brain
//     re-projects it hot (the sweep has not run), so the demoted insight is
//     restored to hot recall.
//
// Run with:
//
//	go test -tags 'sqlite_fts5 integration' \
//	    -run='^TestE2E_CognitiveLifecycle$' -v ./internal/kernel/storage/dispatcher/

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/cognitive"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// keywordEmbedder maps text to a one-hot unit vector by keyword, so equal
// keywords yield equal embeddings (cosine 1) and recall relevance is fully
// controlled. Satisfies both dispatcher.Embedder and kernel.Embedder.
type keywordEmbedder struct{}

func (keywordEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, storage.VectorDim)
	idx := 2
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "cat"):
		idx = 0
	case strings.Contains(lower, "dog"):
		idx = 1
	}
	v[idx] = 1
	return v, nil
}

// hybridOverDispatcher builds a HybridQuery reading the dispatcher's own store,
// so recall observes exactly what the apply arms projected.
func hybridOverDispatcher(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, emb keywordEmbedder) *kernel.HybridQuery {
	t.Helper()
	vec, err := d.OpenVector(ctx)
	if err != nil {
		t.Fatalf("OpenVector: %v", err)
	}
	fts, err := d.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	h, err := kernel.NewHybridQuery(emb, vec, fts, reader)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}
	return h
}

// recallCat runs a "cat" recall and returns the hits indexed by UID.
func recallCat(t *testing.T, ctx context.Context, h *kernel.HybridQuery) map[string]kernel.Hit {
	t.Helper()
	hits, _, err := h.RecallWithReport(ctx, kernel.RecallRequest{Query: "cat", Limit: 20})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	byUID := make(map[string]kernel.Hit, len(hits))
	for _, hit := range hits {
		byUID[hit.UID] = hit
	}
	return byUID
}

func TestE2E_CognitiveLifecycle(t *testing.T) {
	ctx := testutil.TestContext(t)
	emb := keywordEmbedder{}
	d := newDispatcher(t, ctx, emb)

	epA := ulid.Make().String()
	epB := ulid.Make().String()
	insightUID := ulid.Make().String()

	// Two equal-relevance "cat" episodes (identical embeddings, dedup is off).
	if err := d.Apply(ctx, []kernel.Event{makeEpisodeEvent(t, epA, "the cat sat on the mat", "cli:test")}); err != nil {
		t.Fatalf("apply epA: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{makeEpisodeEvent(t, epB, "a cat ran across the yard", "cli:test")}); err != nil {
		t.Fatalf("apply epB: %v", err)
	}
	// One synthesized insight citing epA, of the same "cat" topic so it surfaces.
	if err := d.Apply(ctx, []kernel.Event{insightEvent(t, insightUID, "cat behaviors recur across sessions", []string{epA})}); err != nil {
		t.Fatalf("apply insight: %v", err)
	}
	if err := d.WaitForBackgroundVec(ctx); err != nil {
		t.Fatalf("drain vec: %v", err)
	}

	h := hybridOverDispatcher(t, ctx, d, emb)

	// --- Phase 1: baseline recall surfaces both episodes + the cited insight ---
	base := recallCat(t, ctx, h)
	if _, ok := base[epA]; !ok {
		t.Error("baseline recall missing episode A")
	}
	if _, ok := base[epB]; !ok {
		t.Error("baseline recall missing episode B")
	}
	insightHit, ok := base[insightUID]
	if !ok {
		t.Fatal("baseline recall did not surface the synthesized insight")
	}
	if insightHit.HitType != kernel.HitTypeInsight {
		t.Errorf("insight hit type = %q, want %q", insightHit.HitType, kernel.HitTypeInsight)
	}
	if !containsUID(insightHit.Citations, epA) {
		t.Errorf("insight citations = %v, want to include source episode %s", insightHit.Citations, epA)
	}

	// --- Phase 2: reinforce episode A; it outranks the equal-relevance peer B ---
	now := time.Now().UTC()
	for i := range 5 {
		if err := d.Apply(ctx, []kernel.Event{accessEvent(t, epA, now.Add(time.Duration(i)*time.Second))}); err != nil {
			t.Fatalf("apply access %d: %v", i, err)
		}
	}
	reinforced := recallCat(t, ctx, h)
	hitA, hitB := reinforced[epA], reinforced[epB]
	if hitA.Components.Strength <= 0 {
		t.Errorf("reinforced episode A strength = %v, want > 0", hitA.Components.Strength)
	}
	if hitB.Components.Strength != 0 {
		t.Errorf("un-accessed episode B strength = %v, want 0", hitB.Components.Strength)
	}
	if hitA.FusedScore <= hitB.FusedScore {
		t.Errorf("reinforced A (%.5f) did not outrank equal-relevance peer B (%.5f)",
			hitA.FusedScore, hitB.FusedScore)
	}

	// --- Phase 3: decay sweep demotes the insight out of hot recall ---
	writer, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	// Anchor the sweep far past synthesis so the Gaussian decay drops the insight
	// below the demote floor. The insight was synthesized 2026-01-01.
	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	demoted, err := cognitive.SweepInsightDecay(ctx, writer, cognitive.SweepOptions{Now: future})
	if err != nil {
		t.Fatalf("decay sweep: %v", err)
	}
	if demoted < 1 {
		t.Fatalf("decay sweep demoted %d insights, want >= 1", demoted)
	}
	afterDemote := recallCat(t, ctx, h)
	if _, ok := afterDemote[insightUID]; ok {
		t.Error("demoted insight still surfaces in hot recall (demote filter failed)")
	}
	// The episodes are unaffected by the insight demotion.
	if _, ok := afterDemote[epA]; !ok {
		t.Error("episode A vanished after insight demotion (should be unaffected)")
	}

	// --- Phase 4: rebuild restores the insight (replay re-projects it hot) ---
	d2 := newDispatcher(t, ctx, emb)
	for _, ev := range []kernel.Event{
		makeEpisodeEvent(t, epA, "the cat sat on the mat", "cli:test"),
		makeEpisodeEvent(t, epB, "a cat ran across the yard", "cli:test"),
		insightEvent(t, insightUID, "cat behaviors recur across sessions", []string{epA}),
	} {
		if err := d2.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("rebuild apply: %v", err)
		}
	}
	if err := d2.WaitForBackgroundVec(ctx); err != nil {
		t.Fatalf("rebuild drain vec: %v", err)
	}
	h2 := hybridOverDispatcher(t, ctx, d2, emb)
	restored := recallCat(t, ctx, h2)
	restoredHit, ok := restored[insightUID]
	if !ok {
		t.Fatal("rebuild did not restore the insight to hot recall")
	}
	if restoredHit.HitType != kernel.HitTypeInsight {
		t.Errorf("restored insight hit type = %q, want %q", restoredHit.HitType, kernel.HitTypeInsight)
	}
}

func containsUID(uids []string, want string) bool {
	for _, u := range uids {
		if u == want {
			return true
		}
	}
	return false
}
