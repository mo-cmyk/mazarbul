package ontology_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestRelation_AcceptsSelfObjectUID reproduces the documents-import bug:
// a note with `entity: self` frontmatter emits a relation with
// objectUid="self" (the seeded canonical self entity), but the Relation
// schema's objectUid pattern previously only accepted a 26-char ULID.
// The atomic import batch then failed schema validation and the ENTIRE
// import was aborted — no document row, no edges.
//
// This test fails until the schema is widened to accept the "self"
// sentinel in the object position, symmetric with subjectUid. It then
// becomes the regression guard.
func TestRelation_AcceptsSelfObjectUID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterRelation(ctx, o); err != nil {
		t.Fatalf("RegisterRelation: %v", err)
	}
	rel := ontology.Relation{
		Name:           "relations/01KSW7DTXYH572XGDQTSKN7Y7M",
		UID:            "01KSW7DTXYH572XGDQTSKN7Y7M",
		SubjectUID:     "01KSW7D56E41085C97M86CD0P7",
		Predicate:      "mentions",
		ObjectUID:      "self",
		Confidence:     1.0,
		SourceEventUID: "01KSW7D56E41085C97M86CD0P7",
		CreateTime:     time.Now().UTC(),
	}
	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, string(ontology.KindRelation), json.RawMessage(raw)); err != nil {
		t.Fatalf("Relation with objectUid='self' must validate, got: %v", err)
	}
}

// TestRelation_RejectsArbitraryNonULIDObject is the safety net: the
// schema must still reject random non-ULID, non-self strings in the
// object position. Only the canonical "self" sentinel is allowed.
func TestRelation_RejectsArbitraryNonULIDObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterRelation(ctx, o); err != nil {
		t.Fatalf("RegisterRelation: %v", err)
	}
	rel := ontology.Relation{
		Name:           "relations/01KSW7DTXYH572XGDQTSKN7Y7M",
		UID:            "01KSW7DTXYH572XGDQTSKN7Y7M",
		SubjectUID:     "01KSW7D56E41085C97M86CD0P7",
		Predicate:      "mentions",
		ObjectUID:      "not-a-ulid",
		Confidence:     1.0,
		SourceEventUID: "01KSW7D56E41085C97M86CD0P7",
		CreateTime:     time.Now().UTC(),
	}
	raw, _ := json.Marshal(rel)
	if err := o.Validate(ctx, string(ontology.KindRelation), json.RawMessage(raw)); err == nil {
		t.Error("Relation with objectUid='not-a-ulid' must NOT validate, but did")
	}
}
