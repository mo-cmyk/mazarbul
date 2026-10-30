package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// seedPredicateDef inserts one predicates row with the P5 domain/range columns.
// Empty slices / "" leave the corresponding column NULL (open-world).
func seedPredicateDef(exec func(q string, args ...any), name string, subjectKinds, objectKinds []string, propertySchema string) {
	toJSON := func(s []string) any {
		if len(s) == 0 {
			return nil
		}
		b, _ := json.Marshal(s)
		return string(b)
	}
	var ps any
	if propertySchema != "" {
		ps = propertySchema
	}
	exec(`INSERT INTO predicates (predicate, kind, description, added_at, added_by,
	          subject_kinds, object_kinds, property_schema)
	      VALUES (?, 'non-functional', '', ?, 'user', ?, ?, ?)`,
		name, time.Now().UTC().Format(time.RFC3339Nano),
		toJSON(subjectKinds), toJSON(objectKinds), ps)
}

// seedEntity inserts one entities row with the given uid and kind.
func seedEntity(exec func(q string, args ...any), uid, kind string) {
	exec(`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status)
	      VALUES (?, ?, ?, 1.0, 'confirmed')`, uid, kind, kind)
}

// TestSQLiteOntology_ValidateRelation_UnknownPredicate asserts that a relation
// whose predicate has no row passes (open-world default).
func TestSQLiteOntology_ValidateRelation_UnknownPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)
	seedEntity(exec, "subj1", "person")
	seedEntity(exec, "obj1", "organization")

	if err := ont.ValidateRelation(ctx, "no_such_predicate", "subj1", "obj1", nil); err != nil {
		t.Errorf("unknown predicate should pass open-world, got: %v", err)
	}
}

// TestSQLiteOntology_ValidateRelation_DomainRange exercises the subject/object
// kind constraints: in-range passes, out-of-range returns ErrRelationValidation.
func TestSQLiteOntology_ValidateRelation_DomainRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)
	seedPredicateDef(exec, "employed_by", []string{"person"}, []string{"organization"}, "")
	seedEntity(exec, "alice", "person")
	seedEntity(exec, "erebor", "organization")
	seedEntity(exec, "berlin", "place")

	t.Run("in range passes", func(t *testing.T) {
		t.Parallel()
		if err := ont.ValidateRelation(ctx, "employed_by", "alice", "erebor", nil); err != nil {
			t.Errorf("person employed_by organization should pass, got: %v", err)
		}
	})

	t.Run("subject out of range fails", func(t *testing.T) {
		t.Parallel()
		err := ont.ValidateRelation(ctx, "employed_by", "berlin", "erebor", nil)
		if !errors.Is(err, kernel.ErrRelationValidation) {
			t.Errorf("place employed_by organization: want ErrRelationValidation, got: %v", err)
		}
	})

	t.Run("object out of range fails", func(t *testing.T) {
		t.Parallel()
		err := ont.ValidateRelation(ctx, "employed_by", "alice", "berlin", nil)
		if !errors.Is(err, kernel.ErrRelationValidation) {
			t.Errorf("person employed_by place: want ErrRelationValidation, got: %v", err)
		}
	})
}

// TestSQLiteOntology_ValidateRelation_UnresolvedEndpointSkips asserts that an
// endpoint whose kind cannot be resolved (not yet materialized, or the self
// sentinel) skips its domain/range check rather than failing — the
// eventually-consistent sync tolerance.
func TestSQLiteOntology_ValidateRelation_UnresolvedEndpointSkips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)
	seedPredicateDef(exec, "employed_by", []string{"person"}, []string{"organization"}, "")
	seedEntity(exec, "erebor", "organization")

	t.Run("missing subject row skips", func(t *testing.T) {
		t.Parallel()
		// "ghost" has no entities row: domain check cannot resolve its kind.
		if err := ont.ValidateRelation(ctx, "employed_by", "ghost", "erebor", nil); err != nil {
			t.Errorf("unresolved subject should skip, got: %v", err)
		}
	})

	t.Run("self sentinel subject skips", func(t *testing.T) {
		t.Parallel()
		if err := ont.ValidateRelation(ctx, "employed_by", "self", "erebor", nil); err != nil {
			t.Errorf("self sentinel subject should skip, got: %v", err)
		}
	})
}

// TestSQLiteOntology_ValidateRelation_Attributes exercises the predicate's
// propertySchema against the relation's attributes bag.
func TestSQLiteOntology_ValidateRelation_Attributes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)
	schema := `{"type":"object","properties":{"role":{"type":"string"}},"required":["role"]}`
	seedPredicateDef(exec, "employed_by", nil, nil, schema)
	// A second predicate whose schema imposes no required fields, for the
	// open-schema subtest. Uses a name not in the migration-0006 seed list so
	// the INSERT does not collide. Seeded here (not inside the parallel subtest)
	// so the seed helper's t.Fatalf binds to the still-running parent test.
	openSchema := `{"type":"object","properties":{"role":{"type":"string"}}}`
	seedPredicateDef(exec, "affiliated_with", nil, nil, openSchema)

	t.Run("conforming attributes pass", func(t *testing.T) {
		t.Parallel()
		attrs := json.RawMessage(`{"role":"CEO"}`)
		if err := ont.ValidateRelation(ctx, "employed_by", "a", "b", attrs); err != nil {
			t.Errorf("conforming attributes should pass, got: %v", err)
		}
	})

	t.Run("missing required attribute fails", func(t *testing.T) {
		t.Parallel()
		attrs := json.RawMessage(`{"title":"boss"}`)
		err := ont.ValidateRelation(ctx, "employed_by", "a", "b", attrs)
		if !errors.Is(err, kernel.ErrRelationValidation) {
			t.Errorf("missing required role: want ErrRelationValidation, got: %v", err)
		}
	})

	t.Run("empty attributes fail required schema", func(t *testing.T) {
		t.Parallel()
		// No attributes supplied: an empty object is validated against the
		// schema so a required field (role) still rejects the incomplete
		// relation rather than being bypassed by the len==0 short-circuit.
		err := ont.ValidateRelation(ctx, "employed_by", "a", "b", nil)
		if !errors.Is(err, kernel.ErrRelationValidation) {
			t.Errorf("empty attributes against required schema: want ErrRelationValidation, got: %v", err)
		}
	})

	t.Run("empty attributes pass open schema", func(t *testing.T) {
		t.Parallel()
		// affiliated_with's propertySchema imposes no required fields, so a
		// relation with no attributes validates cleanly: the empty object is
		// accepted.
		if err := ont.ValidateRelation(ctx, "affiliated_with", "a", "b", nil); err != nil {
			t.Errorf("empty attributes against open schema should pass, got: %v", err)
		}
	})
}

// TestSQLiteOntology_KindForEntity covers the UID→kind lookup: a present row
// resolves, the self sentinel and empty/missing UIDs report not-found.
func TestSQLiteOntology_KindForEntity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)
	seedEntity(exec, "alice", "person")

	t.Run("present resolves", func(t *testing.T) {
		t.Parallel()
		kind, ok, err := ont.KindForEntity(ctx, "alice")
		if err != nil {
			t.Fatalf("KindForEntity: %v", err)
		}
		if !ok || kind != "person" {
			t.Errorf("got (%q,%v), want (person,true)", kind, ok)
		}
	})

	t.Run("missing reports not found", func(t *testing.T) {
		t.Parallel()
		_, ok, err := ont.KindForEntity(ctx, "ghost")
		if err != nil {
			t.Fatalf("KindForEntity: %v", err)
		}
		if ok {
			t.Error("missing uid: want ok=false")
		}
	})

	t.Run("self sentinel reports not found", func(t *testing.T) {
		t.Parallel()
		_, ok, err := ont.KindForEntity(ctx, "self")
		if err != nil {
			t.Fatalf("KindForEntity: %v", err)
		}
		if ok {
			t.Error("self sentinel: want ok=false")
		}
	})
}

// TestSQLiteOntology_ValidateRelation_Unattached asserts that a detached
// ontology (no reader) is a no-op rather than an error.
func TestSQLiteOntology_ValidateRelation_Unattached(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont := kernel.NewSQLiteOntologyUnattached()
	if err := ont.ValidateRelation(ctx, "employed_by", "a", "b", json.RawMessage(`{"role":"x"}`)); err != nil {
		t.Errorf("unattached ontology should be a no-op, got: %v", err)
	}
}
