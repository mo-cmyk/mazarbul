package dispatcher_test

// dispatcher_entity_dedup_test.go covers two applier features:
//
//  1. dedupRepointedEdges — called inside applyEntityMerged after
//     repointEntityFKs; collapses self-loops, duplicate active relation
//     triples (per status), and duplicate open facts.
//
//  2. applyEntityDeleted — removes the entity row, its identifiers,
//     facts, mentions, relations, and every other FK reference; then
//     detaches the graph node. Idempotent on replay.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// -- shared helpers ----------------------------------------------------------

// seedRelationWithStatus applies a KindRelation event using relUID, subjUID,
// objUID, predicate, and an explicit status string. An empty status defaults
// to asserted inside the dispatcher.
func seedRelationWithStatus(
	t *testing.T,
	ctx context.Context,
	d *dispatcher.Dispatcher,
	relUID, subjUID, objUID, predicate, status string,
) {
	t.Helper()
	rel := ontology.Relation{
		Name:           "relations/" + relUID,
		UID:            relUID,
		SubjectUID:     subjUID,
		Predicate:      predicate,
		ObjectUID:      objUID,
		Confidence:     1.0,
		SourceEventUID: ulid.Make().String(),
		CreateTime:     time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
		Status:         status,
	}
	payload, err := json.Marshal(rel)
	if err != nil {
		t.Fatalf("marshal relation: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "test/dedup",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply relation %s: %v", relUID, err)
	}
}

// seedRelationConf applies a KindRelation event with an explicit confidence,
// for asserting the confidence-ranked merge dedup (highest confidence wins,
// not lowest uid).
func seedRelationConf(
	t *testing.T,
	ctx context.Context,
	d *dispatcher.Dispatcher,
	relUID, subjUID, objUID, predicate string,
	confidence float64,
) {
	t.Helper()
	rel := ontology.Relation{
		Name:           "relations/" + relUID,
		UID:            relUID,
		SubjectUID:     subjUID,
		Predicate:      predicate,
		ObjectUID:      objUID,
		Confidence:     confidence,
		SourceEventUID: ulid.Make().String(),
		CreateTime:     time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(rel)
	if err != nil {
		t.Fatalf("marshal relation: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "test/dedup",
		Confidence: confidence,
		CreateTime: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply relation %s: %v", relUID, err)
	}
}

// seedRelationDirect inserts an active relation row straight into SQLite with an
// explicit confidence and status=asserted. It bypasses the relation applier's
// supersession logic so several duplicate active triples can coexist on the same
// subject at dedup time — which the event path would otherwise collapse to one
// before the merge ever runs.
func seedRelationDirect(
	t *testing.T,
	ctx context.Context,
	d *dispatcher.Dispatcher,
	relUID, subjUID, objUID, predicate string,
	confidence float64,
) {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, status)
		      VALUES (?, ?, ?, ?, ?, ?, ?, 'asserted')`,
		relUID, subjUID, predicate, objUID, confidence,
		ulid.Make().String(), time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed relation %s: %v", relUID, err)
	}
}

// seedFactPriority inserts an open fact row with an explicit source_priority,
// for asserting the priority-ranked fact dedup on merge.
func seedFactPriority(
	t *testing.T,
	ctx context.Context,
	d *dispatcher.Dispatcher,
	factUID, subjectUID, stateKey, value string,
	priority float64,
) {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO facts (uid, subject_uid, state_key, value, create_time, source_event_uid, source_priority)
		      VALUES (?, ?, ?, ?, ?, ?, ?)`,
		factUID, subjectUID, stateKey, value,
		time.Now().UTC().Format(time.RFC3339), ulid.Make().String(), priority,
	); err != nil {
		t.Fatalf("seed fact %s: %v", factUID, err)
	}
}

// seedFact inserts an open fact row (event_end IS NULL) directly into SQLite.
// The facts table has no FK to episodes in M1 (source_event_uid is a bare
// TEXT column), so direct insertion is safe for test setup.
func seedFact(
	t *testing.T,
	ctx context.Context,
	d *dispatcher.Dispatcher,
	factUID, subjectUID, stateKey, value string,
) {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO facts (uid, subject_uid, state_key, value, create_time, source_event_uid)
		      VALUES (?, ?, ?, ?, ?, ?)`,
		factUID,
		subjectUID,
		stateKey,
		value,
		time.Now().UTC().Format(time.RFC3339),
		ulid.Make().String(),
	); err != nil {
		t.Fatalf("seed fact %s: %v", factUID, err)
	}
}

// applyMerge applies an entityMerged event (srcUID -> tgtUID).
func applyMerge(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, srcUID, tgtUID string) {
	t.Helper()
	payload, err := json.Marshal(ontology.EntityMergedEvent{
		SourceEntityUID: srcUID,
		TargetEntityUID: tgtUID,
		MergedAt:        time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal entityMerged: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityMerged),
		Source:     "test/dedup",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply entityMerged %s->%s: %v", srcUID, tgtUID, err)
	}
}

// applyDelete applies an entityDeleted event for uid.
func applyDelete(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid string) {
	t.Helper()
	payload, err := json.Marshal(ontology.EntityDeletedEvent{
		EntityUID: uid,
		DeletedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal entityDeleted: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityDeleted),
		Source:     "test/dedup",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply entityDeleted %s: %v", uid, err)
	}
}

// countActiveRelations returns the number of active (superseded_by_uid IS
// NULL) relation rows whose subject or object equals uid and whose predicate
// and object also match when non-empty.
func countActiveRelationsForTriple(
	t *testing.T,
	ctx context.Context,
	d *dispatcher.Dispatcher,
	subj, pred, obj string,
) int {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relations
		  WHERE superseded_by_uid IS NULL
		    AND subject_uid = ? AND predicate = ? AND object_uid = ?`,
		subj, pred, obj,
	).Scan(&n); err != nil {
		t.Fatalf("count active relations for triple (%s,%s,%s): %v", subj, pred, obj, err)
	}
	return n
}

// countActiveRelationsSelf returns the number of active self-loop rows on tgt.
func countActiveRelationsSelf(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, tgt string) int {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relations
		  WHERE superseded_by_uid IS NULL
		    AND subject_uid = ? AND object_uid = ?`,
		tgt, tgt,
	).Scan(&n); err != nil {
		t.Fatalf("count self-loop relations for %s: %v", tgt, err)
	}
	return n
}

// countOpenFacts returns the number of open (event_end IS NULL) fact rows for
// the given subject+stateKey+value combination.
func countOpenFacts(
	t *testing.T,
	ctx context.Context,
	d *dispatcher.Dispatcher,
	subj, stateKey, value string,
) int {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM facts
		  WHERE event_end IS NULL
		    AND subject_uid = ? AND state_key = ? AND value = ?`,
		subj, stateKey, value,
	).Scan(&n); err != nil {
		t.Fatalf("count open facts (%s,%s,%s): %v", subj, stateKey, value, err)
	}
	return n
}

// -- EDGE-DEDUP ON MERGE tests -----------------------------------------------

// TestApply_EntityMerged_DuplicateTripleCollapses verifies that when both src
// and tgt hold an active (sub,knows,X) edge, applying the merge leaves exactly
// one active row — the MIN-uid one.
func TestApply_EntityMerged_DuplicateTripleCollapses(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID = "01HDDPDUP000000000000SRC001"
		tgtUID = "01HDDPDUP000000000000TGT001"
		objUID = "01HDDPDUP000000000000OBJ001"
		// minRel will sort below maxRel lexicographically.
		minRel = "01HDDPDUP000000000000REL001"
		maxRel = "01HDDPDUP000000000000REL002"
	)

	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")
	retractTestEntity(t, ctx, d, objUID, "Object")

	// Seed src->(knows)->obj with minRel, tgt->(knows)->obj with maxRel.
	// After merge src->tgt: both triples become (tgt,knows,obj); dedup keeps minRel.
	seedRelationWithStatus(t, ctx, d, minRel, srcUID, objUID, "knows", "")
	seedRelationWithStatus(t, ctx, d, maxRel, tgtUID, objUID, "knows", "")

	applyMerge(t, ctx, d, srcUID, tgtUID)

	got := countActiveRelationsForTriple(t, ctx, d, tgtUID, "knows", objUID)
	if got != 1 {
		t.Errorf("active (tgt,knows,obj) rows = %d, want 1 (duplicate must collapse)", got)
	}

	// Verify that the surviving row is the MIN-uid one.
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var survivingUID string
	if err := db.QueryRowContext(ctx,
		`SELECT uid FROM relations
		  WHERE superseded_by_uid IS NULL
		    AND subject_uid = ? AND predicate = 'knows' AND object_uid = ?`,
		tgtUID, objUID,
	).Scan(&survivingUID); err != nil {
		t.Fatalf("scan surviving relation uid: %v", err)
	}
	if survivingUID != minRel {
		t.Errorf("surviving relation uid = %q, want %q (MIN uid must survive)", survivingUID, minRel)
	}
}

// TestApply_EntityMerged_SelfLoopDropped verifies that a same_as edge between
// the merged pair (src same_as tgt) is deleted after the merge produces a
// tgt->tgt self-loop.
func TestApply_EntityMerged_SelfLoopDropped(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID = "01HDDPSLP000000000000SRC001"
		tgtUID = "01HDDPSLP000000000000TGT001"
		relUID = "01HDDPSLP000000000000REL001"
	)

	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")

	// Seed a same_as edge: src -> tgt. After merge this becomes tgt -> tgt.
	seedRelationWithStatus(t, ctx, d, relUID, srcUID, tgtUID, "same_as", "")

	applyMerge(t, ctx, d, srcUID, tgtUID)

	got := countActiveRelationsSelf(t, ctx, d, tgtUID)
	if got != 0 {
		t.Errorf("self-loop relations on tgt = %d, want 0 (self-loops must be dropped)", got)
	}
}

// TestApply_EntityMerged_FactDedup verifies that when src and tgt each hold an
// open fact with the same state_key and value, the merge leaves exactly one
// open fact for tgt.
func TestApply_EntityMerged_FactDedup(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID = "01HDDPFCT000000000000SRC001"
		tgtUID = "01HDDPFCT000000000000TGT001"
		fact1  = "01HDDPFCT000000000000FCT001"
		fact2  = "01HDDPFCT000000000000FCT002"
		stateK = "state.role"
		stateV = "maintainer"
	)

	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")

	// Identical open facts on both entities.
	seedFact(t, ctx, d, fact1, srcUID, stateK, stateV)
	seedFact(t, ctx, d, fact2, tgtUID, stateK, stateV)

	applyMerge(t, ctx, d, srcUID, tgtUID)

	got := countOpenFacts(t, ctx, d, tgtUID, stateK, stateV)
	if got != 1 {
		t.Errorf("open facts for (tgt,%s,%s) = %d, want 1 (duplicate must collapse)", stateK, stateV, got)
	}
}

// TestApply_EntityMerged_DistinctStatusNotCollapsed verifies that two active
// relation rows with the same (subj,pred,obj) triple but different status
// values survive the merge — status is part of the dedup key.
func TestApply_EntityMerged_DistinctStatusNotCollapsed(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID  = "01HDDPSTS000000000000SRC001"
		tgtUID  = "01HDDPSTS000000000000TGT001"
		objUID  = "01HDDPSTS000000000000OBJ001"
		relAUID = "01HDDPSTS000000000000REL001"
		relBUID = "01HDDPSTS000000000000REL002"
	)

	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")
	retractTestEntity(t, ctx, d, objUID, "Object")

	// src carries an asserted edge; tgt carries a proposed edge.
	seedRelationWithStatus(t, ctx, d, relAUID, srcUID, objUID, "knows", ontology.RelationStatusAsserted)
	seedRelationWithStatus(t, ctx, d, relBUID, tgtUID, objUID, "knows", ontology.RelationStatusProposed)

	applyMerge(t, ctx, d, srcUID, tgtUID)

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var total int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relations
		  WHERE superseded_by_uid IS NULL
		    AND subject_uid = ? AND predicate = 'knows' AND object_uid = ?`,
		tgtUID, objUID,
	).Scan(&total); err != nil {
		t.Fatalf("count all active (tgt,knows,obj) rows: %v", err)
	}
	if total != 2 {
		t.Errorf("active (tgt,knows,obj) rows = %d, want 2 (distinct status must NOT collapse)", total)
	}
}

// TestApply_EntityMerged_DistinctObjectNotCollapsed verifies that two active
// relations from tgt pointing to different objects are both preserved.
func TestApply_EntityMerged_DistinctObjectNotCollapsed(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID  = "01HDDPDOB000000000000SRC001"
		tgtUID  = "01HDDPDOB000000000000TGT001"
		objX    = "01HDDPDOB000000000000OBX001"
		objY    = "01HDDPDOB000000000000OBY001"
		relXUID = "01HDDPDOB000000000000REL001"
		relYUID = "01HDDPDOB000000000000REL002"
	)

	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")
	retractTestEntity(t, ctx, d, objX, "ObjectX")
	retractTestEntity(t, ctx, d, objY, "ObjectY")

	// Both edges are on tgt already (no repoint collision expected).
	seedRelationWithStatus(t, ctx, d, relXUID, tgtUID, objX, "knows", "")
	seedRelationWithStatus(t, ctx, d, relYUID, tgtUID, objY, "knows", "")

	applyMerge(t, ctx, d, srcUID, tgtUID)

	gotX := countActiveRelationsForTriple(t, ctx, d, tgtUID, "knows", objX)
	gotY := countActiveRelationsForTriple(t, ctx, d, tgtUID, "knows", objY)
	if gotX != 1 {
		t.Errorf("active (tgt,knows,objX) rows = %d, want 1", gotX)
	}
	if gotY != 1 {
		t.Errorf("active (tgt,knows,objY) rows = %d, want 1", gotY)
	}
}

// TestApply_EntityMerged_DedupIdempotent verifies that applying the same
// entityMerged event twice does not error and does not over-delete rows.
func TestApply_EntityMerged_DedupIdempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID = "01HDDPIDM000000000000SRC001"
		tgtUID = "01HDDPIDM000000000000TGT001"
		objUID = "01HDDPIDM000000000000OBJ001"
		relUID = "01HDDPIDM000000000000REL001"
	)

	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")
	retractTestEntity(t, ctx, d, objUID, "Object")

	seedRelationWithStatus(t, ctx, d, relUID, srcUID, objUID, "knows", "")

	payload, err := json.Marshal(ontology.EntityMergedEvent{
		SourceEntityUID: srcUID,
		TargetEntityUID: tgtUID,
		MergedAt:        time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal entityMerged: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityMerged),
		Source:     "test/dedup",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// Replay: source is already gone; must not error and must not touch tgt data.
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("replay Apply: %v", err)
	}

	got := countActiveRelationsForTriple(t, ctx, d, tgtUID, "knows", objUID)
	if got != 1 {
		t.Errorf("active (tgt,knows,obj) rows after idempotent replay = %d, want 1", got)
	}
}

// TestApply_EntityMerged_DedupKeepsHigherConfidence verifies that when a merge
// collapses two duplicate active triples, the HIGHEST-confidence row survives
// even when it has the larger uid — a later, stronger reassertion must not be
// downgraded to an older, weaker duplicate.
func TestApply_EntityMerged_DedupKeepsHigherConfidence(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID = "01HDDPCNF000000000000SRC001"
		tgtUID = "01HDDPCNF000000000000TGT001"
		objUID = "01HDDPCNF000000000000OBJ001"
		// relLo sorts BELOW relHi, so MIN(uid) would have kept the low-confidence
		// relLo. Confidence ranking must keep relHi instead.
		relLo = "01HDDPCNF000000000000REL001"
		relHi = "01HDDPCNF000000000000REL002"
	)

	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")
	retractTestEntity(t, ctx, d, objUID, "Object")

	// Low-confidence edge on tgt (small uid); high-confidence edge on src (large uid).
	seedRelationConf(t, ctx, d, relLo, tgtUID, objUID, "knows", 0.40)
	seedRelationConf(t, ctx, d, relHi, srcUID, objUID, "knows", 0.95)

	applyMerge(t, ctx, d, srcUID, tgtUID)

	got := countActiveRelationsForTriple(t, ctx, d, tgtUID, "knows", objUID)
	if got != 1 {
		t.Fatalf("active (tgt,knows,obj) rows = %d, want 1 (duplicate must collapse)", got)
	}
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var survivingUID string
	if err := db.QueryRowContext(ctx,
		`SELECT uid FROM relations
		  WHERE superseded_by_uid IS NULL
		    AND subject_uid = ? AND predicate = 'knows' AND object_uid = ?`,
		tgtUID, objUID,
	).Scan(&survivingUID); err != nil {
		t.Fatalf("scan surviving relation uid: %v", err)
	}
	if survivingUID != relHi {
		t.Errorf("surviving relation uid = %q, want %q (higher confidence must win over lower uid)", survivingUID, relHi)
	}
}

// TestApply_EntityMerged_DedupTieBreaksByLowestUID verifies the ROW_NUMBER dedup
// collapses THREE duplicate active triples (not just a pair) to a single row and
// breaks a top-confidence tie by lowest uid. relTieA and relTieB tie at the top
// confidence; the lower-confidence relLo and the higher-uid tie member relTieB
// must both be deleted, leaving only relTieA.
func TestApply_EntityMerged_DedupTieBreaksByLowestUID(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID  = "01HDDPTIE000000000000SRC001"
		tgtUID  = "01HDDPTIE000000000000TGT001"
		objUID  = "01HDDPTIE000000000000OBJ001"
		relLo   = "01HDDPTIE000000000000REL001" // lowest confidence
		relTieA = "01HDDPTIE000000000000REL002" // top confidence, lower uid → survivor
		relTieB = "01HDDPTIE000000000000REL003" // top confidence, higher uid → dropped
	)

	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")
	retractTestEntity(t, ctx, d, objUID, "Object")

	// All three duplicate triples sit on tgt already (direct insert bypasses
	// pre-merge supersession). The merge src->tgt repoints nothing (src is empty)
	// but still runs dedupRepointedEdges over tgt, exercising the window dedup.
	seedRelationDirect(t, ctx, d, relLo, tgtUID, objUID, "knows", 0.40)
	seedRelationDirect(t, ctx, d, relTieA, tgtUID, objUID, "knows", 0.95)
	seedRelationDirect(t, ctx, d, relTieB, tgtUID, objUID, "knows", 0.95)

	applyMerge(t, ctx, d, srcUID, tgtUID)

	if got := countActiveRelationsForTriple(t, ctx, d, tgtUID, "knows", objUID); got != 1 {
		t.Fatalf("active (tgt,knows,obj) rows = %d, want 1 (all three duplicates collapse to one)", got)
	}
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var survivingUID string
	if err := db.QueryRowContext(ctx,
		`SELECT uid FROM relations
		  WHERE superseded_by_uid IS NULL
		    AND subject_uid = ? AND predicate = 'knows' AND object_uid = ?`,
		tgtUID, objUID,
	).Scan(&survivingUID); err != nil {
		t.Fatalf("scan surviving relation uid: %v", err)
	}
	if survivingUID != relTieA {
		t.Errorf("surviving relation uid = %q, want %q (top confidence, lowest uid among the tie)", survivingUID, relTieA)
	}
}

// TestApply_EntityMerged_FactDedupKeepsHigherPriority verifies the fact dedup
// keeps the highest source_priority row when two identical open facts collapse.
func TestApply_EntityMerged_FactDedupKeepsHigherPriority(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID = "01HDDPFPR000000000000SRC001"
		tgtUID = "01HDDPFPR000000000000TGT001"
		factLo = "01HDDPFPR000000000000FCT001"
		factHi = "01HDDPFPR000000000000FCT002"
		stateK = "state.role"
		stateV = "maintainer"
	)

	retractTestEntity(t, ctx, d, srcUID, "Duplicate")
	retractTestEntity(t, ctx, d, tgtUID, "Canonical")

	// Low-priority fact on tgt (small uid); high-priority identical fact on src.
	seedFactPriority(t, ctx, d, factLo, tgtUID, stateK, stateV, 0.30)
	seedFactPriority(t, ctx, d, factHi, srcUID, stateK, stateV, 0.90)

	applyMerge(t, ctx, d, srcUID, tgtUID)

	if got := countOpenFacts(t, ctx, d, tgtUID, stateK, stateV); got != 1 {
		t.Fatalf("open facts for (tgt,%s,%s) = %d, want 1", stateK, stateV, got)
	}
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var survivingUID string
	if err := db.QueryRowContext(ctx,
		`SELECT uid FROM facts
		  WHERE event_end IS NULL AND subject_uid = ? AND state_key = ? AND value = ?`,
		tgtUID, stateK, stateV,
	).Scan(&survivingUID); err != nil {
		t.Fatalf("scan surviving fact uid: %v", err)
	}
	if survivingUID != factHi {
		t.Errorf("surviving fact uid = %q, want %q (higher source_priority must win)", survivingUID, factHi)
	}
}

// -- ENTITY DELETED APPLIER tests --------------------------------------------

// TestApply_EntityDeleted_FullCleanup verifies that applying an entityDeleted
// event removes the entity row, its identifier rows, its fact rows, its
// mention rows, and its relation rows.
func TestApply_EntityDeleted_FullCleanup(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		targetUID = "01HDELTGT000000000000ENT001"
		otherUID  = "01HDELTGT000000000000OTH001"
		relUID    = "01HDELTGT000000000000REL001"
		factUID   = "01HDELTGT000000000000FCT001"
	)

	// Seed the entity to be deleted plus a bystander.
	retractTestEntity(t, ctx, d, targetUID, "Target")
	retractTestEntity(t, ctx, d, otherUID, "Other")

	// Seed an identifier via the event log.
	identPayload, err := json.Marshal(ontology.EntityIdentifierAddedEvent{
		EntityUID:  targetUID,
		Identifier: "target@example.com",
		Kind:       ontology.IdentifierKindEmail,
		Source:     ontology.IdentifierSourceUser,
		AddedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal identifier: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityIdentifierAdded),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    identPayload,
	}}); err != nil {
		t.Fatalf("apply identifier: %v", err)
	}

	// Seed an open fact.
	seedFact(t, ctx, d, factUID, targetUID, "state.role", "admin")

	// Seed a relation (targetUID is the subject).
	seedRelationWithStatus(t, ctx, d, relUID, targetUID, otherUID, "knows", "")

	// Seed a mention row pointing to targetUID's entity_uid via direct SQL.
	// The mention requires a source_event_uid that is a valid episode uid.
	// Use a throwaway episode as the anchor.
	episodeUID := ulid.Make().String()
	if err := d.Apply(ctx, []kernel.Event{makeEpisodeEvent(t, episodeUID, "target mentioned here", "test")}); err != nil {
		t.Fatalf("apply anchor episode: %v", err)
	}
	mentionUID := ulid.Make().String()
	db, dbErr := d.OpenSQLite(ctx)
	if dbErr != nil {
		t.Fatalf("OpenSQLite: %v", dbErr)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO mentions (uid, kind, display_name, source_event_uid, create_time, entity_uid)
		      VALUES (?, 'person', 'Target', ?, ?, ?)`,
		mentionUID,
		episodeUID,
		time.Now().UTC().Format(time.RFC3339),
		targetUID,
	); err != nil {
		t.Fatalf("seed mention: %v", err)
	}

	// Apply the delete.
	applyDelete(t, ctx, d, targetUID)

	// Entity row must be gone.
	var entityCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entities WHERE uid = ?`, targetUID,
	).Scan(&entityCount); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if entityCount != 0 {
		t.Errorf("entity row still present after delete (count=%d)", entityCount)
	}

	// Identifier rows must be gone.
	var identCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_identifiers WHERE entity_uid = ?`, targetUID,
	).Scan(&identCount); err != nil {
		t.Fatalf("count entity_identifiers: %v", err)
	}
	if identCount != 0 {
		t.Errorf("entity_identifiers rows still present (count=%d)", identCount)
	}

	// Fact rows must be gone.
	var factCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM facts WHERE subject_uid = ?`, targetUID,
	).Scan(&factCount); err != nil {
		t.Fatalf("count facts: %v", err)
	}
	if factCount != 0 {
		t.Errorf("facts rows still present (count=%d)", factCount)
	}

	// Relation rows must be gone.
	var relCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relations WHERE subject_uid = ? OR object_uid = ?`,
		targetUID, targetUID,
	).Scan(&relCount); err != nil {
		t.Fatalf("count relations: %v", err)
	}
	if relCount != 0 {
		t.Errorf("relation rows still present (count=%d)", relCount)
	}

	// Mention rows must be gone.
	var mentionCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mentions WHERE entity_uid = ?`, targetUID,
	).Scan(&mentionCount); err != nil {
		t.Fatalf("count mentions: %v", err)
	}
	if mentionCount != 0 {
		t.Errorf("mention rows still present (count=%d)", mentionCount)
	}
}

// TestApply_EntityDeleted_Idempotent verifies that applying the same
// entityDeleted event twice does not error and leaves zero rows.
func TestApply_EntityDeleted_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const uid = "01HDELIDM000000000000ENT001"
	retractTestEntity(t, ctx, d, uid, "Target")

	payload, err := json.Marshal(ontology.EntityDeletedEvent{
		EntityUID: uid,
		DeletedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal entityDeleted: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntityDeleted),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("replay Apply: %v", err)
	}

	db, dbErr := d.OpenSQLite(ctx)
	if dbErr != nil {
		t.Fatalf("OpenSQLite: %v", dbErr)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entities WHERE uid = ?`, uid,
	).Scan(&count); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if count != 0 {
		t.Errorf("entity row still present after idempotent delete (count=%d)", count)
	}
}

// TestApply_EntityDeleted_UnknownUID verifies that deleting a never-seeded
// entity is a no-op (all DELETEs touch zero rows, no error).
func TestApply_EntityDeleted_UnknownUID(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const uid = "01HDELUNK000000000000ENT001"
	// Do not seed the entity.
	applyDelete(t, ctx, d, uid)

	db, dbErr := d.OpenSQLite(ctx)
	if dbErr != nil {
		t.Fatalf("OpenSQLite: %v", dbErr)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entities WHERE uid = ?`, uid,
	).Scan(&count); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if count != 0 {
		t.Errorf("unexpected entity row after delete-of-unknown uid (count=%d)", count)
	}
}

// -- entity_aliases (migration 0040) on merge / delete ----------------------

// seedAlias inserts one entity_aliases row directly. The table has no FK to
// entities (it is a projection rebuilt on each entity event), so a direct
// insert is safe for test setup.
func seedAlias(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, alias, entityUID string) {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO entity_aliases (alias, entity_uid, scope, create_time)
		      VALUES (?, ?, 'personal', ?)`,
		alias, entityUID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed alias %q->%s: %v", alias, entityUID, err)
	}
}

// countAliasesForEntity returns the number of entity_aliases rows owned by uid.
func countAliasesForEntity(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid string) int {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_aliases WHERE entity_uid = ?`, uid,
	).Scan(&n); err != nil {
		t.Fatalf("count aliases for %s: %v", uid, err)
	}
	return n
}

// TestApply_EntityMerged_AliasesRepointed verifies that a merge moves the
// source's entity_aliases rows onto the target (so [[Old Name]] wikilinks and
// get-by-name keep resolving to the survivor) and leaves none stranded on the
// vanished source. An alias both entities already share collapses to a single
// row via the composite-PK INSERT OR IGNORE.
func TestApply_EntityMerged_AliasesRepointed(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		srcUID = "01HDDPALS000000000000SRC001"
		tgtUID = "01HDDPALS000000000000TGT001"
	)

	retractTestEntity(t, ctx, d, srcUID, "Erebor")
	retractTestEntity(t, ctx, d, tgtUID, "Erebor Trading Company")

	// src-only alias, tgt-only alias, and an alias both already share.
	seedAlias(t, ctx, d, "Erebor Global", srcUID)
	seedAlias(t, ctx, d, "ETC", tgtUID)
	seedAlias(t, ctx, d, "Erebor", srcUID)
	seedAlias(t, ctx, d, "Erebor", tgtUID)

	applyMerge(t, ctx, d, srcUID, tgtUID)

	if got := countAliasesForEntity(t, ctx, d, srcUID); got != 0 {
		t.Errorf("aliases still stranded on merged source = %d, want 0", got)
	}
	// tgt now owns: ETC, Erebor Global, Erebor (the shared one collapsed to one row).
	if got := countAliasesForEntity(t, ctx, d, tgtUID); got != 3 {
		t.Errorf("aliases on target after merge = %d, want 3 (src aliases moved, shared collapsed)", got)
	}
}

// TestApply_EntityDeleted_AliasesRemoved verifies that deleting an entity
// removes its entity_aliases rows so a stale alias never resolves to a
// vanished uid.
func TestApply_EntityDeleted_AliasesRemoved(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const uid = "01HDELALS000000000000ENT001"
	retractTestEntity(t, ctx, d, uid, "Doomed")
	seedAlias(t, ctx, d, "Doomed Alias", uid)
	seedAlias(t, ctx, d, "Another Alias", uid)

	applyDelete(t, ctx, d, uid)

	if got := countAliasesForEntity(t, ctx, d, uid); got != 0 {
		t.Errorf("entity_aliases rows still present after delete = %d, want 0", got)
	}
}
