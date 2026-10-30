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

// applyRelationWithPredicate applies an open KindRelation event using the given
// predicate so the auto-registration invariant can be exercised with a brand
// new predicate name. Mirrors retractTestRelation but lets the caller choose
// the predicate.
func applyRelationWithPredicate(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, relUID, subjUID, objUID, predicate string) {
	t.Helper()
	payload, err := json.Marshal(ontology.Relation{
		UID:            relUID,
		SubjectUID:     subjUID,
		Predicate:      predicate,
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

// readPredicate reads the kind/description/added_by columns for one predicate.
// A missing row yields found=false.
func readPredicate(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, predicate string) (kind, description, addedBy string, found bool) {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	err = db.QueryRowContext(ctx,
		`SELECT kind, description, added_by FROM predicates WHERE predicate = ?`, predicate).
		Scan(&kind, &description, &addedBy)
	if err != nil {
		return "", "", "", false
	}
	return kind, description, addedBy, true
}

// TestApply_Relation_AutoRegistersPredicate pins the dispatcher invariant: a
// relation asserted with a brand-new predicate auto-registers that predicate in
// the vocabulary (so `schema predicates` / introspect list it), even though the
// relations table holds no predicate foreign key. This is the single-place fix
// that keeps every write path (CLI, MCP, synced/grafted relations) consistent.
func TestApply_Relation_AutoRegistersPredicate(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		subjUID   = "01HPREDAUTO000000000SUBJ01"
		objUID    = "01HPREDAUTO0000000000OBJ01"
		relUID    = "01HPREDAUTO0000000000REL01"
		predicate = "mentors" // brand-new, not in the seed lexicon
	)
	retractTestEntity(t, ctx, d, subjUID, "Subject")
	retractTestEntity(t, ctx, d, objUID, "Object")

	if _, _, _, found := readPredicate(t, ctx, d, predicate); found {
		t.Fatalf("predicate %q should be absent before the relation is asserted", predicate)
	}

	applyRelationWithPredicate(t, ctx, d, relUID, subjUID, objUID, predicate)

	kind, description, addedBy, found := readPredicate(t, ctx, d, predicate)
	if !found {
		t.Fatalf("predicate %q was not auto-registered by the relation apply", predicate)
	}
	if kind != "non-functional" {
		t.Errorf("auto-registered kind = %q, want non-functional", kind)
	}
	if description != "Auto-registered from an asserted relation." {
		t.Errorf("auto-registered description = %q", description)
	}
	if addedBy != "user" {
		t.Errorf("auto-registered added_by = %q, want user", addedBy)
	}
}

// TestApply_Relation_DoesNotClobberExistingPredicate pins that a relation
// reusing an existing, richly-defined predicate never overwrites that
// predicate's kind/description: the auto-register upsert is ON CONFLICT DO
// NOTHING. A predicate registered as functional with a bespoke description must
// survive a later non-functional auto-asserted edge.
func TestApply_Relation_DoesNotClobberExistingPredicate(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		subjUID   = "01HPREDKEEP000000000SUBJ01"
		objUID    = "01HPREDKEEP0000000000OBJ01"
		relUID    = "01HPREDKEEP0000000000REL01"
		predicate = "leads"
	)
	retractTestEntity(t, ctx, d, subjUID, "Subject")
	retractTestEntity(t, ctx, d, objUID, "Object")

	// Pre-register the predicate via a predicateRegistered event with a rich,
	// non-default definition.
	prPayload, err := json.Marshal(ontology.PredicateRegisteredEvent{
		Predicate:   predicate,
		Kind:        ontology.PredicateKindFunctional,
		Description: "Subject is the current lead of the object team.",
		AddedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		AddedBy:     ontology.PredicateAddedByUser,
	})
	if err != nil {
		t.Fatalf("marshal predicateRegistered: %v", err)
	}
	prEv := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindPredicateRegistered),
		Source:     "cli:schema",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Payload:    prPayload,
	}
	if err := d.Apply(ctx, []kernel.Event{prEv}); err != nil {
		t.Fatalf("apply predicateRegistered: %v", err)
	}

	// Now assert a relation reusing that predicate. The auto-register upsert
	// must not clobber the bespoke definition.
	applyRelationWithPredicate(t, ctx, d, relUID, subjUID, objUID, predicate)

	kind, description, addedBy, found := readPredicate(t, ctx, d, predicate)
	if !found {
		t.Fatalf("predicate %q vanished after the relation apply", predicate)
	}
	if kind != "functional" {
		t.Errorf("kind = %q, want functional (must survive the auto-register)", kind)
	}
	if description != "Subject is the current lead of the object team." {
		t.Errorf("description = %q, want the bespoke one (must survive)", description)
	}
	if addedBy != "user" {
		t.Errorf("added_by = %q, want user", addedBy)
	}
}
