package ontology_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestRelation_AcceptsSelfSubjectUID reproduces the self-subject silent-failure:
// the lexical self-statement detector emits a candidate with subject
// pre-bound to "self" (the seeded canonical entity UID), but the
// Relation event schema's subjectUid pattern only accepts a 26-char
// ULID. The resolver's appendRelation call then fails schema
// validation and the synthetic relation never lands in JSONL.
//
// This test fails until the schema is relaxed to accept the "self"
// sentinel alongside ULIDs. It then becomes the regression guard.
func TestRelation_AcceptsSelfSubjectUID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterRelation(ctx, o); err != nil {
		t.Fatalf("RegisterRelation: %v", err)
	}
	rel := ontology.Relation{
		Name:           "relations/01KSW7DTXYH572XGDQTSKN7Y7M",
		UID:            "01KSW7DTXYH572XGDQTSKN7Y7M",
		SubjectUID:     "self",
		Predicate:      "has_name",
		ObjectUID:      "01KSW7DTXYH572XGDQTSKN7Y7M",
		Confidence:     0.95,
		SourceEventUID: "01KSW7D56E41085C97M86CD0P7",
		CreateTime:     time.Now().UTC(),
	}
	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, string(ontology.KindRelation), json.RawMessage(raw)); err != nil {
		t.Fatalf("Relation with subjectUid='self' must validate, got: %v", err)
	}
}

// TestRelation_RejectsArbitraryNonULIDSubject is the safety net: the
// schema must still reject random non-ULID strings — only the
// canonical "self" sentinel is allowed. Without this guard, a typo
// in the resolver's binding logic could write garbage subjectUids.
func TestRelation_RejectsArbitraryNonULIDSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterRelation(ctx, o); err != nil {
		t.Fatalf("RegisterRelation: %v", err)
	}
	rel := ontology.Relation{
		Name:           "relations/01KSW7DTXYH572XGDQTSKN7Y7M",
		UID:            "01KSW7DTXYH572XGDQTSKN7Y7M",
		SubjectUID:     "not-a-ulid",
		Predicate:      "has_name",
		ObjectUID:      "01KSW7DTXYH572XGDQTSKN7Y7M",
		Confidence:     0.95,
		SourceEventUID: "01KSW7D56E41085C97M86CD0P7",
		CreateTime:     time.Now().UTC(),
	}
	raw, _ := json.Marshal(rel)
	if err := o.Validate(ctx, string(ontology.KindRelation), json.RawMessage(raw)); err == nil {
		t.Error("Relation with subjectUid='not-a-ulid' must NOT validate, but did")
	}
}
