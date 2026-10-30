package dispatcher_test

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

// retractTestEntity applies a minimal KindEntity event so the relation edge has
// graph endpoints to MERGE against.
func retractTestEntity(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid, name string) {
	t.Helper()
	payload, err := json.Marshal(ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      name,
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal entity: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntity),
		Source:     "resolver/v1",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply entity %s: %v", uid, err)
	}
}

// retractTestRelation applies an open KindRelation event (valid_until NULL).
func retractTestRelation(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, relUID, subjUID, objUID string) {
	t.Helper()
	payload, err := json.Marshal(ontology.Relation{
		UID:            relUID,
		SubjectUID:     subjUID,
		Predicate:      "works_at",
		ObjectUID:      objUID,
		Confidence:     1.0,
		SourceEventUID: ulid.Make().String(),
	})
	if err != nil {
		t.Fatalf("marshal relation: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "resolver/v1",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply relation %s: %v", relUID, err)
	}
}

// makeRetractedEvent builds a relationRetracted event closing relUID with
// supersederUID at retractedAt. eventUID is the OR-set element key.
func makeRetractedEvent(t *testing.T, eventUID, relUID, supersederUID string, retractedAt time.Time) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.RelationRetractedEvent{
		RelationUID:     ontology.ULID(relUID),
		SupersededByUID: ontology.ULID(supersederUID),
		RetractedAt:     retractedAt,
	})
	if err != nil {
		t.Fatalf("marshal retracted: %v", err)
	}
	return kernel.Event{
		UID:        eventUID,
		Kind:       kernel.EventKind(ontology.KindRelationRetracted),
		Source:     "resolver/v1",
		Confidence: 1.0,
		CreateTime: retractedAt,
		Payload:    payload,
	}
}

// TestApply_RelationRetracted_ConvergesRegardlessOfOrder is the core #205
// guarantee: two devices that close the same relation arm with different
// superseders converge to identical materialized state no matter which
// retraction replays first. The fold picks the lowest superseder ULID and the
// earliest retractedAt.
func TestApply_RelationRetracted_ConvergesRegardlessOfOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: exhaustive permutation convergence (~32s)")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)

	const (
		subjUID = "01HRETCONVERGE000000SUBJ01"
		objUID  = "01HRETCONVERGE0000000OBJ01"
		relUID  = "01HRETCONVERGE0000000REL01"
	)
	// Two distinct superseders; the lowest ULID is the canonical pointer.
	a := ulid.Make().String()
	b := ulid.Make().String()
	superLow, superHigh := a, b
	if superHigh < superLow {
		superLow, superHigh = superHigh, superLow
	}
	earliest := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Each device pairs its retraction's superseder with a distinct time; the
	// fold must independently pick lowest ULID + earliest time, so we cross
	// them: the lower-ULID superseder carries the LATER time to prove the two
	// columns reduce independently.
	retLow := makeRetractedEvent(t, "01HRET00000000000000EVENTLO", relUID, superLow, later)
	retHigh := makeRetractedEvent(t, "01HRET00000000000000EVENTHI", relUID, superHigh, earliest)

	apply := func(order []kernel.Event) (string, string) {
		d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
		retractTestEntity(t, ctx, d, subjUID, "Subject")
		retractTestEntity(t, ctx, d, objUID, "Object")
		retractTestRelation(t, ctx, d, relUID, subjUID, objUID)
		for _, ev := range order {
			if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
				t.Fatalf("apply retraction: %v", err)
			}
		}
		vu, sb, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
		if err != nil {
			t.Fatalf("read close: %v", err)
		}
		return vu, sb
	}

	vu1, sb1 := apply([]kernel.Event{retLow, retHigh})
	vu2, sb2 := apply([]kernel.Event{retHigh, retLow})

	if vu1 != vu2 || sb1 != sb2 {
		t.Fatalf("diverged across replay order: A(vu=%q sb=%q) B(vu=%q sb=%q)", vu1, sb1, vu2, sb2)
	}
	if sb1 != superLow {
		t.Errorf("superseded_by_uid = %q, want lowest ULID %q", sb1, superLow)
	}
	wantVU := earliest.UTC().Format(time.RFC3339Nano)
	if vu1 != wantVU {
		t.Errorf("valid_until = %q, want earliest %q", vu1, wantVU)
	}
}

// TestApply_RelationRetracted_Idempotent pins that re-applying the same
// retraction event (the maz rebuild path) leaves the close columns unchanged —
// INSERT OR IGNORE + the commutative fold are stable under replay.
func TestApply_RelationRetracted_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		subjUID = "01HRETIDEMP000000000SUBJ01"
		objUID  = "01HRETIDEMP0000000000OBJ01"
		relUID  = "01HRETIDEMP0000000000REL01"
		superID = "01HRETIDEMP00000000SUPER01"
	)
	retractTestEntity(t, ctx, d, subjUID, "Subject")
	retractTestEntity(t, ctx, d, objUID, "Object")
	retractTestRelation(t, ctx, d, relUID, subjUID, objUID)

	ret := makeRetractedEvent(t, "01HRETIDEMP000000000EVENT1", relUID, superID,
		time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	if err := d.Apply(ctx, []kernel.Event{ret}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	vu1, sb1, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
	if err != nil {
		t.Fatalf("read close: %v", err)
	}
	// Replay the identical event.
	if err := d.Apply(ctx, []kernel.Event{ret}); err != nil {
		t.Fatalf("replay apply: %v", err)
	}
	vu2, sb2, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
	if err != nil {
		t.Fatalf("read close after replay: %v", err)
	}
	if vu1 != vu2 || sb1 != sb2 {
		t.Errorf("not idempotent: before(vu=%q sb=%q) after(vu=%q sb=%q)", vu1, sb1, vu2, sb2)
	}
	if sb1 != superID {
		t.Errorf("superseded_by_uid = %q, want %q", sb1, superID)
	}
}

// TestApply_RelationRetracted_PureTemporalClose covers the unrelate tool's
// path: a relationRetracted with NO superseder (the relationship simply ended,
// no replacement) must close the edge with valid_until = retractedAt while
// leaving superseded_by_uid NULL, and the relation row must SURVIVE (history
// preserved — unlike forget, which drops the row). This is the classic
// "left the company" scenario: "X worked at Y until <date>" must remain
// visible to time-travel reads while dropping out of the current view.
func TestApply_RelationRetracted_PureTemporalClose(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		subjUID = "01HRETPURE0000000000SUBJ01"
		objUID  = "01HRETPURE00000000000OBJ01"
		relUID  = "01HRETPURE00000000000REL01"
	)
	retractTestEntity(t, ctx, d, subjUID, "Samwise")
	retractTestEntity(t, ctx, d, objUID, "Erebor Global")
	retractTestRelation(t, ctx, d, relUID, subjUID, objUID)

	endedAt := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
	// Empty superseder == pure temporal close (what the unrelate tool emits).
	ret := makeRetractedEvent(t, "01HRETPURE000000000EVENT01", relUID, "", endedAt)
	if err := d.Apply(ctx, []kernel.Event{ret}); err != nil {
		t.Fatalf("apply pure close: %v", err)
	}

	// A successful read proves the relation row SURVIVES the close (forget would
	// have removed it).
	vu, sb, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
	if err != nil {
		t.Fatalf("read close (relation row must still exist after a temporal close): %v", err)
	}
	if want := endedAt.UTC().Format(time.RFC3339Nano); vu != want {
		t.Errorf("valid_until = %q, want %q (closed as of endedAt)", vu, want)
	}
	if sb != "" {
		t.Errorf("superseded_by_uid = %q, want empty (pure close has no replacement)", sb)
	}
}

// TestApply_RelationRetracted_BeforeRelation covers the retracted-before-relation
// replay hazard: a tombstone applied before its target relation event must still
// materialize the close once the relation lands.
func TestApply_RelationRetracted_BeforeRelation(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		subjUID = "01HRETBEFORE00000000SUBJ01"
		objUID  = "01HRETBEFORE000000000OBJ01"
		relUID  = "01HRETBEFORE000000000REL01"
		superID = "01HRETBEFORE0000000SUPER01"
	)
	retractTestEntity(t, ctx, d, subjUID, "Subject")
	retractTestEntity(t, ctx, d, objUID, "Object")

	retractedAt := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	ret := makeRetractedEvent(t, "01HRETBEFORE00000000EVENT1", relUID, superID, retractedAt)
	// Tombstone first — the relation row does not exist yet.
	if err := d.Apply(ctx, []kernel.Event{ret}); err != nil {
		t.Fatalf("apply retraction before relation: %v", err)
	}
	// Now the relation lands; applyRelation must fold the pending tombstone in.
	retractTestRelation(t, ctx, d, relUID, subjUID, objUID)

	vu, sb, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
	if err != nil {
		t.Fatalf("read close: %v", err)
	}
	if sb != superID {
		t.Errorf("superseded_by_uid = %q, want %q", sb, superID)
	}
	if want := retractedAt.UTC().Format(time.RFC3339Nano); vu != want {
		t.Errorf("valid_until = %q, want %q", vu, want)
	}
}

// TestApply_Relation_ReapplyDoesNotReopenRetracted covers the duplicate-relation
// replay hazard (the second Rebuild pass): re-applying a relation event after it
// was retracted must NOT reset the close columns back to the payload's open
// state. The tombstone fold is the sole writer.
func TestApply_Relation_ReapplyDoesNotReopenRetracted(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		subjUID = "01HRETREAPPLY00000000SUBJ1"
		objUID  = "01HRETREAPPLY000000000OBJ1"
		relUID  = "01HRETREAPPLY000000000REL1"
		superID = "01HRETREAPPLY00000000SUPR1"
	)
	retractTestEntity(t, ctx, d, subjUID, "Subject")
	retractTestEntity(t, ctx, d, objUID, "Object")

	// Build the relation event once so we can replay the identical event later.
	relPayload, err := json.Marshal(ontology.Relation{
		UID:            relUID,
		SubjectUID:     subjUID,
		Predicate:      "works_at",
		ObjectUID:      objUID,
		Confidence:     1.0,
		SourceEventUID: "01HRETREAPPLY0000000SRCEV1",
	})
	if err != nil {
		t.Fatalf("marshal relation: %v", err)
	}
	relEv := kernel.Event{
		UID:        "01HRETREAPPLY000000RELEV01",
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "resolver/v1",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Payload:    relPayload,
	}
	if err := d.Apply(ctx, []kernel.Event{relEv}); err != nil {
		t.Fatalf("apply relation: %v", err)
	}

	retractedAt := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	ret := makeRetractedEvent(t, "01HRETREAPPLY00000EVENT001", relUID, superID, retractedAt)
	if err := d.Apply(ctx, []kernel.Event{ret}); err != nil {
		t.Fatalf("apply retraction: %v", err)
	}

	// Re-apply the identical relation event (simulates the second Rebuild pass).
	if err := d.Apply(ctx, []kernel.Event{relEv}); err != nil {
		t.Fatalf("re-apply relation: %v", err)
	}

	vu, sb, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
	if err != nil {
		t.Fatalf("read close: %v", err)
	}
	if sb != superID {
		t.Errorf("re-apply reopened the relation: superseded_by_uid = %q, want %q", sb, superID)
	}
	if want := retractedAt.UTC().Format(time.RFC3339Nano); vu != want {
		t.Errorf("re-apply cleared the close: valid_until = %q, want %q", vu, want)
	}
}
