package dispatcher_test

// dispatcher_vec_test.go: unit tests for the entity and mention vector write
// paths in the dispatcher. These tests run without the integration build tag
// because they use the real SQLite + sqlite-vec stack (no mocks) while
// staying fully self-contained with t.TempDir().
//
// Acceptance criteria (P4):
//   - EntityCreated with Summary -> two rows in entities_vec_ids (name + summary).
//   - EntityCreated without Summary -> one row (name only).
//   - MentionRecorded -> one row in mentions_vec_ids with non-null embedding.
//   - Embed cache is hit on second Apply for the same display name (smoke check
//     via Stats; the dedup is transparent to the caller).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// nameVectorSearcher mirrors the unexported interface in
// internal/kernel/query_hybrid.go that HybridQuery type-asserts against to
// fire the name-vector fusion lane. Declared here so tests in the
// dispatcher package can prove the production VectorConn satisfies the
// same structural shape; without this regression test the prior interface
// mismatch (sqlite.Vector exposed SearchEntitiesByVectorType but not
// SearchNameVectors) silently disabled the fourth lane in every run.
type nameVectorSearcher interface {
	SearchNameVectors(ctx context.Context, query []float32, k int) ([]kernel.VectorHit, error)
}

// makeEntityEvent builds a kernel.Event whose Payload is a valid Entity.
func makeEntityEvent(t *testing.T, uid string, entity ontology.Entity) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(entity)
	if err != nil {
		t.Fatalf("marshal entity payload: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		CreateTime: time.Unix(1_700_000_000, 0).UTC(),
		Kind:       kernel.EventKind(ontology.KindEntity),
		Source:     "test:dispatcher_vec",
		Confidence: 1.0,
		Payload:    payload,
	}
}

// makeMentionEvent builds a kernel.Event whose Payload is a valid Mention.
func makeMentionEvent(t *testing.T, uid string, mention ontology.Mention) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(mention)
	if err != nil {
		t.Fatalf("marshal mention payload: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		CreateTime: time.Unix(1_700_000_001, 0).UTC(),
		Kind:       kernel.EventKind(ontology.KindMention),
		Source:     "test:dispatcher_vec",
		Confidence: 0.9,
		Payload:    payload,
	}
}

// countEntityVecRows returns the number of rows in entities_vec_ids for the
// given entity UID.
func countEntityVecRows(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, entityUID string) int {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM entities_vec_ids WHERE id = ?`, entityUID,
	).Scan(&n); err != nil {
		t.Fatalf("count entities_vec_ids: %v", err)
	}
	return n
}

// hasMentionVecRow returns true when mentions_vec_ids contains a row for the
// given mention UID.
func hasMentionVecRow(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, mentionUID string) bool {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM mentions_vec_ids WHERE id = ?`, mentionUID,
	).Scan(&n); err != nil {
		t.Fatalf("count mentions_vec_ids: %v", err)
	}
	return n > 0
}

// TestDispatcher_EntityCreatedWithSummary_TwoVecRows verifies that an entity
// event carrying a non-empty Summary produces two rows in entities_vec_ids:
// one with vector_type='name' and one with vector_type='summary'.
func TestDispatcher_EntityCreatedWithSummary_TwoVecRows(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	entity := ontology.Entity{
		UID:              "01HVEC0000000000000000001",
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Frodo Baggins",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
		Summary:          "Frodo Baggins is an engineer at Erebor Inc. based in Berlin.",
	}
	ev := makeEntityEvent(t, entity.UID, entity)

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Entity embeddings (name + summary) are written asynchronously off
	// the writer hot path; drain bgVec before asserting on vec rows.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	rows := countEntityVecRows(t, ctx, d, entity.UID)
	if rows != 2 {
		t.Errorf("entities_vec_ids rows = %d, want 2 (name + summary)", rows)
	}

	// Verify both vector_type values are present.
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	for _, wantType := range []string{"name", "summary"} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM entities_vec_ids WHERE id = ? AND vector_type = ?`,
			entity.UID, wantType,
		).Scan(&n); err != nil {
			t.Fatalf("count vector_type=%s: %v", wantType, err)
		}
		if n != 1 {
			t.Errorf("vector_type=%s count = %d, want 1", wantType, n)
		}
	}
}

// TestDispatcher_EntityCreatedWithoutSummary_OneVecRow verifies that an entity
// event without a Summary produces exactly one row in entities_vec_ids
// (vector_type='name' only).
func TestDispatcher_EntityCreatedWithoutSummary_OneVecRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	entity := ontology.Entity{
		UID:              "01HVEC0000000000000000002",
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Boromir",
		Confidence:       0.9,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
		// No Summary field set.
	}
	ev := makeEntityEvent(t, entity.UID, entity)

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Entity name embedding is written asynchronously off the writer hot
	// path; drain bgVec before asserting on the vec row.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	rows := countEntityVecRows(t, ctx, d, entity.UID)
	if rows != 1 {
		t.Errorf("entities_vec_ids rows = %d, want 1 (name only)", rows)
	}

	// The single row must be vector_type='name'.
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var vecType string
	if err := db.QueryRowContext(ctx,
		`SELECT vector_type FROM entities_vec_ids WHERE id = ?`, entity.UID,
	).Scan(&vecType); err != nil {
		t.Fatalf("scan vector_type: %v", err)
	}
	if vecType != "name" {
		t.Errorf("vector_type = %q, want %q", vecType, "name")
	}
}

// TestVector_SatisfiesNameVectorSearcher is a regression test for the dead
// name-vector lane. The kernel's HybridQuery type-asserts its VectorConn
// against an unexported `nameVectorSearcher` interface to opt into the
// fourth fusion lane; before this fix sqlite.Vector exposed only
// SearchEntitiesByVectorType and not SearchNameVectors, so the assertion
// silently returned ok=false and three-lane fusion was the production
// reality. The test (a) proves *sqlite.Vector satisfies the interface
// structurally and (b) exercises the lane end-to-end: an entity created
// with vector_type='name' is retrievable via SearchNameVectors and a
// nearest-neighbour query against a different summary embedding does NOT
// surface it through the name lane.
func TestVector_SatisfiesNameVectorSearcher(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	entity := ontology.Entity{
		UID:              "01HVEC0000000000000000003",
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Frodo Baggins",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
		Summary:          "Engineer at Erebor Inc. based in Berlin.",
	}
	ev := makeEntityEvent(t, entity.UID, entity)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Name-vector lane uses the async-written name vector; drain bgVec so
	// SearchNameVectors sees a populated index.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	vec, err := d.OpenVector(ctx)
	if err != nil {
		t.Fatalf("OpenVector: %v", err)
	}
	// Concrete-type assertion: the dispatcher must hand back the sqlite
	// implementation. If this fails the production wiring has changed and
	// the test below would not be exercising the real path.
	sv, ok := vec.(*sqlite.Vector)
	if !ok {
		t.Fatalf("VectorConn is %T, want *sqlite.Vector (production wiring assumption)", vec)
	}
	// Structural assertion: mirrors the unexported assertion inside
	// HybridQuery.fetchAllCandidates. If this fails the kernel falls
	// silently back to three-lane fusion.
	nvs, ok := any(sv).(nameVectorSearcher)
	if !ok {
		t.Fatal("*sqlite.Vector no longer satisfies nameVectorSearcher; the kernel's HybridQuery will silently drop the name-vector lane")
	}

	// fakeEmbedder is deterministic — re-embed the display name to get the
	// exact vector the dispatcher wrote.
	emb := &fakeEmbedder{dim: storage.VectorDim}
	nameVec, err := emb.Embed(ctx, entity.DisplayName)
	if err != nil {
		t.Fatalf("Embed name: %v", err)
	}

	hits, err := nvs.SearchNameVectors(ctx, nameVec, 5)
	if err != nil {
		t.Fatalf("SearchNameVectors: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchNameVectors returned zero hits; lane is wired but indexer did not populate name vectors")
	}
	if hits[0].ID != entity.UID {
		t.Errorf("top hit ID = %q, want %q", hits[0].ID, entity.UID)
	}

	// Cross-check: a query for the summary text must not surface this
	// entity via the name lane. The dispatcher wrote one row with
	// vector_type='name' and another with vector_type='summary'; the lane
	// must filter to the former.
	summaryVec, err := emb.Embed(ctx, entity.Summary)
	if err != nil {
		t.Fatalf("Embed summary: %v", err)
	}
	hitsBySummary, err := nvs.SearchNameVectors(ctx, summaryVec, 5)
	if err != nil {
		t.Fatalf("SearchNameVectors (summary query): %v", err)
	}
	for _, h := range hitsBySummary {
		// Allow the row to appear, but only with a distance > 0 — the
		// summary embedding lives on a different vec0 row.
		if h.ID == entity.UID && h.Score < 0.001 {
			t.Errorf("name lane returned the summary embedding (distance %v) for entity %s; vector_type filter is broken", h.Score, entity.UID)
		}
	}
}

// TestDispatcher_MentionRecorded_VecRowWithEmbedding verifies that applying a
// Mention event produces a row in mentions_vec_ids and that the corresponding
// vec0 slot is non-empty (rowid present in both tables).
func TestDispatcher_MentionRecorded_VecRowWithEmbedding(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	sourceUID := "01HSRC0000000000000000001"
	// First write a source episode so the FK constraint is satisfied.
	epEv := makeEpisodeEvent(t, sourceUID, "Frodo works at Erebor.", "test")
	if err := d.Apply(ctx, []kernel.Event{epEv}); err != nil {
		t.Fatalf("Apply episode: %v", err)
	}

	mention := ontology.Mention{
		Kind:           ontology.EntityKindPerson,
		DisplayName:    "Frodo",
		SourceEventUID: ontology.ULID(sourceUID),
		ExtractionText: "Frodo",
		SourceSpan:     ontology.SourceSpan{StartChar: 0, EndChar: 7},
		GroundingScore: 1.0,
	}
	mentionUID := "01HMNT0000000000000000001"
	ev := makeMentionEvent(t, mentionUID, mention)

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply mention: %v", err)
	}

	// Mention embeddings are written asynchronously to keep the Apply hot
	// path off the embedder's network round-trip; drain the bgVec
	// WaitGroup before asserting the vec row materialised.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	if !hasMentionVecRow(t, ctx, d, mentionUID) {
		t.Error("mentions_vec_ids missing row for mention UID")
	}

	// Confirm the vec0 slot exists via the sidecar join (same pattern as
	// HasEvent uses for episodes_vec).
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var joined int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*)
		 FROM mentions_vec_ids AS ids
		 JOIN mentions_vec     AS v ON v.rowid = ids.rowid
		 WHERE ids.id = ?`, mentionUID,
	).Scan(&joined); err != nil {
		t.Fatalf("join mentions_vec: %v", err)
	}
	if joined != 1 {
		t.Errorf("mentions_vec sidecar+vec joined count = %d, want 1", joined)
	}
}

// TestDispatcher_EntityVec_NilEmbedderSkipsVectors verifies that applying an
// entity event without a configured embedder writes the relational row but
// does not touch entities_vec_ids.
func TestDispatcher_EntityVec_NilEmbedderSkipsVectors(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil) // no embedder

	entity := ontology.Entity{
		UID:              "01HVEC0000000000000000003",
		Kind:             ontology.EntityKindOrganization,
		DisplayName:      "Erebor Inc.",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
	}
	ev := makeEntityEvent(t, entity.UID, entity)

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Relational row must exist.
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM entities WHERE uid = ?`, entity.UID,
	).Scan(&n); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if n != 1 {
		t.Errorf("entities row count = %d, want 1", n)
	}

	// No vector rows.
	rows := countEntityVecRows(t, ctx, d, entity.UID)
	if rows != 0 {
		t.Errorf("entities_vec_ids rows = %d, want 0 (no embedder)", rows)
	}
}
