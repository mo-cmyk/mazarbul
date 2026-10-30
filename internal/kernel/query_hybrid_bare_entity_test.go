package kernel

import (
	"context"
	"testing"
	"time"
)

// TestHybridQuery_Recall_BareEntityHit is the failing-first test for Bug 2:
// a directly-created entity (no surrounding episode text) must appear in
// recall results as an entity-kind hit when its name vector matches the query.
//
// Root cause: the name-vector lane (fetchNameVectorCandidates) calls
// SearchNameVectors then episodesForEntities — if the entity has no episodes
// (no mentions row) the join returns nothing and the entity is silently
// dropped from the results.  The fix is to detect orphan entity UIDs (matched
// by name-vector but backed by no episodes) and emit first-class entity hits.
//
// This test also asserts that normal episode recall is not regressed.
func TestHybridQuery_Recall_BareEntityHit(t *testing.T) {
	t.Parallel()

	// Seed one episode (normal recall path) and one bare entity (no episode).
	episodes := []seedEpisode{
		{uid: "ep1", kind: "remember", confidence: 1.0, text: "Aragorn works at Mithril Corp in Berlin"},
	}

	// stubNameVectorSearcher wraps stubVector and also satisfies
	// nameVectorSearcher: it returns a fixed list of entity VectorHits.
	// The entity "bareuid" has no mentions/episodes rows.
	nvs := &stubVectorWithNameSearch{
		stubVector: stubVector{
			hits: []VectorHit{{ID: "ep1", Score: 0.2}},
		},
		nameHits: []VectorHit{
			// "bareuid" entity matched by name-vector; no episode rows for it.
			{ID: "bareuid", Score: 0.1},
		},
	}
	fts := &stubFTS{}

	db := openMemoryEpisodes(t)
	seedEpisodes(t, db, episodes)

	// Seed the bare entity in the entities table (no mentions row).
	if _, err := db.ExecContext(t.Context(), `INSERT INTO entities (uid, kind, display_name) VALUES ('bareuid', 'person', 'Peanut Bareentity')`); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	h, err := NewHybridQuery(stubEmbedder{}, nvs, fts, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Peanut", Limit: 10}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}

	// Must return at least one hit.
	if len(hits) == 0 {
		t.Fatal("recall returned no hits; expected at least the bare entity hit")
	}

	// Find the entity hit for "Peanut Bareentity".
	var entityHit *Hit
	for i := range hits {
		if hits[i].HitType == HitTypeEntity && hits[i].UID == "bareuid" {
			entityHit = &hits[i]
			break
		}
	}
	if entityHit == nil {
		t.Errorf("no entity hit for uid=bareuid; got hits: %+v", hits)
	} else {
		if entityHit.Text == "" {
			t.Errorf("entity hit Text is empty, want entity display name or name")
		}
		if entityHit.FusedScore <= 0 {
			t.Errorf("entity hit FusedScore = %v, want > 0", entityHit.FusedScore)
		}
	}

	// Episode recall must still work (no regression).
	var episodeHit *Hit
	for i := range hits {
		if hits[i].HitType == HitTypeEpisode && hits[i].UID == "ep1" {
			episodeHit = &hits[i]
			break
		}
	}
	if episodeHit == nil {
		t.Errorf("ep1 episode hit missing; entity-hit feature must not drop episode hits")
	}
}

// TestHybridQuery_Recall_EntityHitCarriesMetadata verifies that a bare entity
// hit carries the entity's own Kind, Confidence and CreateTime — not the zero
// values. Regression: the entity-hit branch omitted these, so projectHit
// defaulted the empty Kind to "episode" and formatted the zero CreateTime as
// year 0001, mislabelling directly-created entities in the unified hit contract.
func TestHybridQuery_Recall_EntityHitCarriesMetadata(t *testing.T) {
	t.Parallel()

	nvs := &stubVectorWithNameSearch{
		stubVector: stubVector{hits: []VectorHit{}},
		nameHits:   []VectorHit{{ID: "bareuid", Score: 0.1}},
	}
	fts := &stubFTS{}

	db := openMemoryEpisodes(t)
	const wantCreate = "2026-01-02T15:04:05Z"
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO entities (uid, kind, display_name, confidence, create_time)
		 VALUES ('bareuid', 'person', 'Peanut Bareentity', 0.75, ?)`,
		wantCreate,
	); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	h, err := NewHybridQuery(stubEmbedder{}, nvs, fts, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Peanut", Limit: 10}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}

	var entityHit *Hit
	for i := range hits {
		if hits[i].HitType == HitTypeEntity && hits[i].UID == "bareuid" {
			entityHit = &hits[i]
			break
		}
	}
	if entityHit == nil {
		t.Fatalf("no entity hit for uid=bareuid; got hits: %+v", hits)
	}
	if entityHit.Kind != EventKind("person") {
		t.Errorf("entity hit Kind = %q, want %q", entityHit.Kind, "person")
	}
	if entityHit.Confidence != 0.75 {
		t.Errorf("entity hit Confidence = %v, want 0.75", entityHit.Confidence)
	}
	wantT, _ := time.Parse(time.RFC3339, wantCreate)
	if !entityHit.CreateTime.Equal(wantT) {
		t.Errorf("entity hit CreateTime = %v, want %v", entityHit.CreateTime, wantT)
	}
}

// TestHybridQuery_Recall_EpisodeHitsRetainEpisodeKind verifies that existing
// episode hits receive HitTypeEpisode (not empty or entity) so the
// discriminator field is consistently populated even when no entity hits are
// present.
func TestHybridQuery_Recall_EpisodeHitsRetainEpisodeKind(t *testing.T) {
	t.Parallel()

	episodes := []seedEpisode{
		{uid: "ep1", kind: "remember", confidence: 1.0, text: "some episode text"},
		{uid: "ep2", kind: "remember", confidence: 1.0, text: "another episode"},
	}
	vec := &stubVector{hits: []VectorHit{{ID: "ep1"}, {ID: "ep2"}}}
	fts := &stubFTS{}
	h := newTestHybrid(t, episodes, vec, fts)

	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "episode", Limit: 5}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	for _, hit := range hits {
		if hit.HitType != HitTypeEpisode {
			t.Errorf("hit %q HitType = %q, want %q", hit.UID, hit.HitType, HitTypeEpisode)
		}
	}
}

// TestHybridQuery_Recall_EntityHitEntitiesPreserved verifies that attachEntities
// does not replace a bare entity hit's Entities anchor with [].
// Regression test for Greptile finding: attachEntities used to treat every hit
// UID as a source_event_uid; bare entities have no mentions row so the anchor
// was replaced with an empty slice.
func TestHybridQuery_Recall_EntityHitEntitiesPreserved(t *testing.T) {
	t.Parallel()

	nvs := &stubVectorWithNameSearch{
		stubVector: stubVector{hits: []VectorHit{}},
		nameHits:   []VectorHit{{ID: "bareuid", Score: 0.1}},
	}
	fts := &stubFTS{}
	db := openMemoryEpisodes(t)
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO entities (uid, kind, display_name) VALUES ('bareuid', 'person', 'Bare Entity')`); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	h, err := NewHybridQuery(stubEmbedder{}, nvs, fts, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}
	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Bare", Limit: 5}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	var entityHit *Hit
	for i := range hits {
		if hits[i].HitType == HitTypeEntity && hits[i].UID == "bareuid" {
			entityHit = &hits[i]
			break
		}
	}
	if entityHit == nil {
		t.Fatal("entity hit not found")
	}
	// The Entities anchor must carry the entity itself, not an empty slice.
	if len(entityHit.Entities) == 0 {
		t.Error("entity hit Entities is empty; attachEntities must not clobber entity hits")
	} else if entityHit.Entities[0].UID != "bareuid" {
		t.Errorf("entity hit Entities[0].UID = %q, want %q", entityHit.Entities[0].UID, "bareuid")
	}
}

// TestAttachEntities_PreservesDerivedAnchors verifies that attachEntities keeps
// the pre-populated seed/cited entity anchor on insight and consolidated-memory
// hits. Regression for issue #231: derived hits have no mentions row keyed by
// their own UID, so the reverse-join found nothing and the assignment loop
// clobbered the anchor finalizeDerivedHits attached with an empty slice. The
// mix includes an episode hit so the assignment loop actually runs (an
// all-derived slice early-returns before it).
func TestAttachEntities_PreservesDerivedAnchors(t *testing.T) {
	t.Parallel()

	db := openMemoryEpisodes(t)
	// Stamp the default read scope onto ctx exactly as Recall does; without it
	// the scoped mentions/entities join is fail-closed (empty span → no rows).
	ctx := withScopes(t.Context(), DefaultReadSet())
	// Episode ep1 mentions epEnt; the derived hits anchor on insAnchor/consAnchor.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO entities (uid, kind, display_name) VALUES
			('epEnt', 'person', 'Episode Entity'),
			('insAnchor', 'project', 'Mithril'),
			('consAnchor', 'person', 'Aragorn');
		INSERT INTO mentions (uid, source_event_uid, entity_uid) VALUES
			('m1', 'ep1', 'epEnt');`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h, err := NewHybridQuery(stubEmbedder{}, &stubVectorWithNameSearch{}, &stubFTS{}, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	hits := []Hit{
		{HitType: HitTypeEpisode, UID: "ep1"},
		{HitType: HitTypeInsight, UID: "ins1", Entities: []EntityRef{{UID: "insAnchor", Kind: "project", DisplayName: "Mithril"}}},
		{HitType: HitTypeConsolidated, UID: "cons1", Entities: []EntityRef{{UID: "consAnchor", Kind: "person", DisplayName: "Aragorn"}}},
	}
	if err := h.attachEntities(ctx, hits); err != nil {
		t.Fatalf("attachEntities: %v", err)
	}

	// Episode hit picks up its mentioned entity via the reverse-join.
	if len(hits[0].Entities) != 1 || hits[0].Entities[0].UID != "epEnt" {
		t.Errorf("episode hit Entities = %+v, want [epEnt]", hits[0].Entities)
	}
	// Insight hit keeps its seed anchor; not clobbered to [].
	if len(hits[1].Entities) != 1 || hits[1].Entities[0].UID != "insAnchor" {
		t.Errorf("insight hit Entities = %+v, want [insAnchor]", hits[1].Entities)
	}
	// Consolidated hit keeps its cited anchor.
	if len(hits[2].Entities) != 1 || hits[2].Entities[0].UID != "consAnchor" {
		t.Errorf("consolidated hit Entities = %+v, want [consAnchor]", hits[2].Entities)
	}
}

// TestHybridQuery_Recall_NameEmbWeightZeroSuppressesEntityHits verifies that
// setting nameEmbWeight=0 suppresses bare entity hits entirely.
// Regression test for Greptile finding: entity hits were appended unconditionally,
// ignoring nameEmbWeight.
func TestHybridQuery_Recall_NameEmbWeightZeroSuppressesEntityHits(t *testing.T) {
	t.Parallel()

	nvs := &stubVectorWithNameSearch{
		stubVector: stubVector{hits: []VectorHit{}},
		nameHits:   []VectorHit{{ID: "bareuid", Score: 0.1}},
	}
	fts := &stubFTS{}
	db := openMemoryEpisodes(t)
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO entities (uid, kind, display_name) VALUES ('bareuid', 'person', 'Bare Entity')`); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	h, err := NewHybridQuery(stubEmbedder{}, nvs, fts, db, WithNameEmbWeight(0))
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}
	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Bare", Limit: 5}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, hit := range hits {
		if hit.HitType == HitTypeEntity {
			t.Errorf("got entity hit uid=%q but nameEmbWeight=0 should suppress entity hits", hit.UID)
		}
	}
}

// TestHybridQuery_Recall_RRFModeEntityHitScore verifies that in RRF fusion mode
// a bare entity hit's FusedScore uses the 1/(rrfK+rank) formula rather than the
// raw cosine similarity, so it competes fairly with episode hits.
// Regression test for Greptile finding: entity hits always used cosine similarity
// regardless of fusion mode.
func TestHybridQuery_Recall_RRFModeEntityHitScore(t *testing.T) {
	t.Parallel()

	nvs := &stubVectorWithNameSearch{
		stubVector: stubVector{hits: []VectorHit{}},
		// Score 0.1 → cosSim ≈ 0.995; in RRF mode this must NOT become the FusedScore.
		nameHits: []VectorHit{{ID: "bareuid", Score: 0.1}},
	}
	fts := &stubFTS{}
	db := openMemoryEpisodes(t)
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO entities (uid, kind, display_name) VALUES ('bareuid', 'person', 'Bare Entity')`); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	h, err := NewHybridQuery(stubEmbedder{}, nvs, fts, db, WithFusionMode(FusionModeRRF))
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}
	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Bare", Limit: 5}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	var entityHit *Hit
	for i := range hits {
		if hits[i].HitType == HitTypeEntity && hits[i].UID == "bareuid" {
			entityHit = &hits[i]
			break
		}
	}
	if entityHit == nil {
		t.Fatal("entity hit not found in RRF mode")
	}
	// In RRF mode the FusedScore is 1/(rrfK+1) ≈ 0.0163 for rank=0, nameEmbWeight=1.
	// It must not be close to 1.0 (cosine similarity).
	if entityHit.FusedScore > 0.1 {
		t.Errorf("RRF mode entity FusedScore = %v, want ~1/(rrfK+1) ≈ 0.016; got cosine-like value",
			entityHit.FusedScore)
	}
}

// TestHybridQuery_Recall_EntityHitSemanticGate verifies that in weighted mode a
// bare entity hit whose cosine similarity falls below the configured semantic
// gate threshold is suppressed, mirroring the pre-fusion floor episode hits
// enforce. A bare entity cannot rescue a semantically-unrelated candidate.
// Regression test for Greptile finding: entity hits skipped the weighted-mode
// semantic gate entirely.
func TestHybridQuery_Recall_EntityHitSemanticGate(t *testing.T) {
	t.Parallel()

	nvs := &stubVectorWithNameSearch{
		stubVector: stubVector{hits: []VectorHit{}},
		nameHits: []VectorHit{
			// dist 1.0 → cosSim 0.5 (below gate 0.6, must be dropped).
			{ID: "below-gate", Score: 1.0},
			// dist 0.1 → cosSim ≈ 0.995 (above gate, must survive).
			{ID: "above-gate", Score: 0.1},
		},
	}
	fts := &stubFTS{}
	db := openMemoryEpisodes(t)
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO entities (uid, kind, display_name) VALUES
		  ('below-gate', 'person', 'Below Gate'),
		  ('above-gate', 'person', 'Above Gate')
	`); err != nil {
		t.Fatalf("seed entities: %v", err)
	}

	h, err := NewHybridQuery(stubEmbedder{}, nvs, fts, db, WithSemanticGateThreshold(0.6))
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}
	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Gate", Limit: 10}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}

	sawBelow := false
	sawAbove := false
	for _, hit := range hits {
		if hit.HitType != HitTypeEntity {
			continue
		}
		switch hit.UID {
		case "below-gate":
			sawBelow = true
		case "above-gate":
			sawAbove = true
		}
	}
	if sawBelow {
		t.Error("below-gate entity (cosSim 0.5) should be dropped by semantic gate 0.6")
	}
	if !sawAbove {
		t.Error("above-gate entity (cosSim ≈ 0.995) should survive the semantic gate 0.6")
	}
}

// TestHybridQuery_Recall_EntityHitMinConfidenceFilter verifies that entity
// hits below the MinConfidence threshold are suppressed, mirroring
// filterHydrated's behaviour for episode hits.
func TestHybridQuery_Recall_EntityHitMinConfidenceFilter(t *testing.T) {
	t.Parallel()

	nvs := &stubVectorWithNameSearch{
		stubVector: stubVector{hits: []VectorHit{}},
		nameHits:   []VectorHit{{ID: "lowconf", Score: 0.1}},
	}
	fts := &stubFTS{}
	db := openMemoryEpisodes(t)
	// confidence=0.3, below the MinConfidence=0.5 threshold below.
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO entities (uid, kind, display_name, confidence) VALUES ('lowconf', 'person', 'Low Conf', 0.3)`); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	h, err := NewHybridQuery(stubEmbedder{}, nvs, fts, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}
	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Low", Limit: 5, MinConfidence: 0.5}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, hit := range hits {
		if hit.HitType == HitTypeEntity && hit.UID == "lowconf" {
			t.Error("low-confidence entity hit should be suppressed by MinConfidence filter")
		}
	}
}

// TestHybridQuery_Recall_EntityHitKindsFilter verifies that entity hits with
// a kind not in the Kinds allowlist are suppressed, mirroring filterHydrated's
// Kinds predicate for episode hits.
func TestHybridQuery_Recall_EntityHitKindsFilter(t *testing.T) {
	t.Parallel()

	nvs := &stubVectorWithNameSearch{
		stubVector: stubVector{hits: []VectorHit{}},
		nameHits:   []VectorHit{{ID: "org1", Score: 0.1}},
	}
	fts := &stubFTS{}
	db := openMemoryEpisodes(t)
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO entities (uid, kind, display_name) VALUES ('org1', 'organisation', 'Mithril Corp')`); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	h, err := NewHybridQuery(stubEmbedder{}, nvs, fts, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}
	// Kinds restricts to "remember"; organisation entity must be filtered out.
	hits, err := drain(h.Recall(t.Context(), RecallRequest{Query: "Mithril", Limit: 5, Kinds: []EventKind{"remember"}}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	for _, hit := range hits {
		if hit.HitType == HitTypeEntity && hit.UID == "org1" {
			t.Error("entity hit with non-matching kind should be suppressed by Kinds filter")
		}
	}
}

// TestHybridQuery_Recall_EntityHitSinceUntilFilter verifies that entity hits
// outside the Since/Until window are suppressed, mirroring filterHydrated's
// time-range predicates for episode hits.
func TestHybridQuery_Recall_EntityHitSinceUntilFilter(t *testing.T) {
	t.Parallel()

	nvs := &stubVectorWithNameSearch{
		stubVector: stubVector{hits: []VectorHit{}},
		nameHits: []VectorHit{
			{ID: "old-entity", Score: 0.1},
			{ID: "new-entity", Score: 0.1},
		},
	}
	fts := &stubFTS{}
	db := openMemoryEpisodes(t)
	// old-entity: create_time 2020-01-01, new-entity: 2025-01-01.
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO entities (uid, kind, display_name, create_time) VALUES
		  ('old-entity', 'person', 'Old Entity', '2020-01-01T00:00:00Z'),
		  ('new-entity', 'person', 'New Entity', '2025-01-01T00:00:00Z')
	`); err != nil {
		t.Fatalf("seed entities: %v", err)
	}

	h, err := NewHybridQuery(stubEmbedder{}, nvs, fts, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	since := mustParseTime(t, "2023-01-01T00:00:00Z")
	until := mustParseTime(t, "2026-01-01T00:00:00Z")
	hits, err := drain(h.Recall(t.Context(), RecallRequest{
		Query: "entity", Limit: 10,
		Since: since, Until: until,
	}))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}

	sawOld := false
	sawNew := false
	for _, hit := range hits {
		if hit.HitType != HitTypeEntity {
			continue
		}
		switch hit.UID {
		case "old-entity":
			sawOld = true
		case "new-entity":
			sawNew = true
		}
	}
	if sawOld {
		t.Error("old-entity (2020) should be excluded by Since=2023 filter")
	}
	if !sawNew {
		t.Error("new-entity (2025) should pass the Since=2023/Until=2026 window")
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("mustParseTime(%q): %v", s, err)
	}
	return ts
}

// stubVectorWithNameSearch extends stubVector by also implementing
// nameVectorSearcher so the name-vector lane fires during tests without
// needing a real sqlite-vec backend.
type stubVectorWithNameSearch struct {
	stubVector
	nameHits []VectorHit
}

func (s *stubVectorWithNameSearch) SearchNameVectors(_ context.Context, _ []float32, _ int) ([]VectorHit, error) {
	return s.nameHits, nil
}
