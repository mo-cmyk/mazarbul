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

// proposedRelation applies an open KindRelation event carrying status=proposed
// — the trust posture the resolver stamps on a connector-ingested edge (#211).
func proposedRelation(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, relUID, subjUID, objUID string) {
	t.Helper()
	payload, err := json.Marshal(ontology.Relation{
		UID:            relUID,
		SubjectUID:     subjUID,
		Predicate:      "works_at",
		ObjectUID:      objUID,
		Confidence:     1.0,
		SourceEventUID: ulid.Make().String(),
		Status:         ontology.RelationStatusProposed,
	})
	if err != nil {
		t.Fatalf("marshal relation: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     ontology.ConnectorSourceColonPrefix + "granola",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply proposed relation %s: %v", relUID, err)
	}
}

// makePromotedEvent builds a relationPromoted event targeting relUID, authored
// by promoterPrincipal (empty = legacy/unsigned).
func makePromotedEvent(t *testing.T, relUID, promoterPrincipal string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.RelationPromotedEvent{
		RelationUID: ontology.ULID(relUID),
		PromotedAt:  time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal promoted: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindRelationPromoted),
		Source:     "mcp:promoteRelation",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if promoterPrincipal != "" {
		ev.Author = kernel.EventAuthor{PrincipalUID: promoterPrincipal}
	}
	return ev
}

// TestApply_RelationPromoted_FlipsProposedToAsserted is the #212 core: a
// connector-proposed edge flips to asserted and records the signed promoter.
func TestApply_RelationPromoted_FlipsProposedToAsserted(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	const (
		subjUID  = "01HPROMOTE0000000000SUBJ01"
		objUID   = "01HPROMOTE00000000000OBJ01"
		relUID   = "01HPROMOTE00000000000REL01"
		promoter = "01HPROMOTEPRINCIPAL00000001"
	)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	retractTestEntity(t, ctx, d, subjUID, "Subject")
	retractTestEntity(t, ctx, d, objUID, "Object")
	proposedRelation(t, ctx, d, relUID, subjUID, objUID)

	// Precondition: lands proposed.
	if status, _, err := dispatcher.RelationStatusForTest(ctx, d, relUID); err != nil {
		t.Fatalf("read status: %v", err)
	} else if status != ontology.RelationStatusProposed {
		t.Fatalf("seed status = %q, want proposed", status)
	}

	if err := d.Apply(ctx, []kernel.Event{makePromotedEvent(t, relUID, promoter)}); err != nil {
		t.Fatalf("apply promotion: %v", err)
	}

	status, assertedBy, err := dispatcher.RelationStatusForTest(ctx, d, relUID)
	if err != nil {
		t.Fatalf("read status after promote: %v", err)
	}
	if status != ontology.RelationStatusAsserted {
		t.Errorf("status = %q, want asserted", status)
	}
	if assertedBy != promoter {
		t.Errorf("asserted_by_principal = %q, want promoter %q", assertedBy, promoter)
	}
}

// TestApply_RelationPromoted_Idempotent pins that replaying the same promotion
// (the rebuild path) and promoting an already-asserted edge are both no-ops:
// the status stays asserted and the original asserter is preserved.
func TestApply_RelationPromoted_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	const (
		subjUID  = "01HPROMOTEIDEM000000SUBJ01"
		objUID   = "01HPROMOTEIDEM0000000OBJ01"
		relUID   = "01HPROMOTEIDEM0000000REL01"
		promoter = "01HPROMOTEIDEMPRINCIPAL0001"
	)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	retractTestEntity(t, ctx, d, subjUID, "Subject")
	retractTestEntity(t, ctx, d, objUID, "Object")
	proposedRelation(t, ctx, d, relUID, subjUID, objUID)

	promo := makePromotedEvent(t, relUID, promoter)
	if err := d.Apply(ctx, []kernel.Event{promo}); err != nil {
		t.Fatalf("apply promotion: %v", err)
	}
	// Replay the same event, then a fresh promotion by a DIFFERENT principal:
	// the guard is status='proposed', so neither touches the already-asserted row.
	if err := d.Apply(ctx, []kernel.Event{promo}); err != nil {
		t.Fatalf("replay promotion: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{makePromotedEvent(t, relUID, "01HOTHERPRINCIPAL000000001")}); err != nil {
		t.Fatalf("second promotion: %v", err)
	}

	status, assertedBy, err := dispatcher.RelationStatusForTest(ctx, d, relUID)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != ontology.RelationStatusAsserted {
		t.Errorf("status = %q, want asserted", status)
	}
	if assertedBy != promoter {
		t.Errorf("asserted_by_principal = %q, want the FIRST promoter %q (no clobber)", assertedBy, promoter)
	}
}

// TestApply_RelationPromoted_UnsignedAuthorBindsNull pins that an unsigned
// (legacy/empty-author) promotion binds asserted_by_principal to SQL NULL, not
// the empty string. The COALESCE that preserves a prior asserter on replay
// hinges on the distinction — an empty string would silently overwrite it.
func TestApply_RelationPromoted_UnsignedAuthorBindsNull(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	const (
		subjUID = "01HPROMOTENULL000000SUBJ01"
		objUID  = "01HPROMOTENULL0000000OBJ01"
		relUID  = "01HPROMOTENULL0000000REL01"
	)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	retractTestEntity(t, ctx, d, subjUID, "Subject")
	retractTestEntity(t, ctx, d, objUID, "Object")
	proposedRelation(t, ctx, d, relUID, subjUID, objUID)

	// Promote with an empty author (legacy/unsigned).
	if err := d.Apply(ctx, []kernel.Event{makePromotedEvent(t, relUID, "")}); err != nil {
		t.Fatalf("apply unsigned promotion: %v", err)
	}

	status, assertedBy, err := dispatcher.RelationStatusForTest(ctx, d, relUID)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != ontology.RelationStatusAsserted {
		t.Errorf("status = %q, want asserted", status)
	}
	if assertedBy != "" {
		t.Errorf("asserted_by_principal = %q, want empty for an unsigned promoter", assertedBy)
	}
	isNull, err := dispatcher.RelationAssertedByNullForTest(ctx, d, relUID)
	if err != nil {
		t.Fatalf("read nullability: %v", err)
	}
	if !isNull {
		t.Error("asserted_by_principal should be SQL NULL for an unsigned promoter, not an empty string")
	}
}

// TestApply_RelationPromoted_AbsentRelationNoOp pins that a promotion replaying
// before its relation exists (a cross-device reorder) affects zero rows and
// errors no event apply — the relation later lands proposed and a re-promotion
// reconverges it.
func TestApply_RelationPromoted_AbsentRelationNoOp(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	const relUID = "01HPROMOTEABSENT00000REL01"
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// Apply the promotion against a relation that does not exist yet.
	if err := d.Apply(ctx, []kernel.Event{makePromotedEvent(t, relUID, "01HPRINCIPAL0000000000ABSENT")}); err != nil {
		t.Fatalf("apply promotion against absent relation should be a no-op, got: %v", err)
	}

	// No row exists; the read returns sql.ErrNoRows wrapped — assert it is not
	// silently present as asserted.
	if _, _, err := dispatcher.RelationStatusForTest(ctx, d, relUID); err == nil {
		t.Error("expected no relation row for an absent-target promotion")
	}
}
