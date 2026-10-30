package ontology_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// predicateRegisteredFakeOntology is a map-backed test double for
// kernel.Ontology, following the same pattern as the sibling fake types.
type predicateRegisteredFakeOntology struct {
	schemas map[string]kernel.Schema
}

func newPredicateRegisteredFakeOntology() *predicateRegisteredFakeOntology {
	return &predicateRegisteredFakeOntology{schemas: make(map[string]kernel.Schema)}
}

func (f *predicateRegisteredFakeOntology) Schema(_ context.Context, kind string) (kernel.Schema, error) {
	s, ok := f.schemas[kind]
	if !ok {
		return kernel.Schema{}, kernel.ErrNotFound
	}
	return s, nil
}

func (f *predicateRegisteredFakeOntology) Register(_ context.Context, s kernel.Schema) error {
	if _, ok := f.schemas[s.Kind]; ok {
		return kernel.ErrSchemaConflict
	}
	f.schemas[s.Kind] = s
	return nil
}

func (f *predicateRegisteredFakeOntology) List(_ context.Context) ([]kernel.Schema, error) {
	out := make([]kernel.Schema, 0, len(f.schemas))
	for _, s := range f.schemas {
		out = append(out, s)
	}
	return out, nil
}

func (f *predicateRegisteredFakeOntology) Validate(ctx context.Context, kind string, payload json.RawMessage) error {
	s, err := f.Schema(ctx, kind)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(s.Definition, &doc); err != nil {
		return fmt.Errorf("unmarshal schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	const resourceURL = "inline://schema.json"
	if err := c.AddResource(resourceURL, doc); err != nil {
		return fmt.Errorf("add resource: %w", err)
	}
	built, err := c.Compile(resourceURL)
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
	return built.Validate(decoded)
}

// TestPredicateRegistered_Register_SchemaRoundtrips builds a
// PredicateRegisteredEvent with every field populated, registers the
// schema, marshals, validates, and asserts byte-stable round-trip.
func TestPredicateRegistered_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newPredicateRegisteredFakeOntology()
	if err := ontology.RegisterPredicateRegistered(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := fake.Schema(ctx, ontology.KindPredicateRegistered)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindPredicateRegistered {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindPredicateRegistered)
	}
	if got.Version != ontology.VersionPredicateRegistered {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionPredicateRegistered)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want %q", got.Provenance, "core")
	}

	original := ontology.PredicateRegisteredEvent{
		Predicate:   "works_at",
		Kind:        ontology.PredicateKindFunctional,
		Description: "The subject currently works at the object organization.",
		AddedBy:     ontology.PredicateAddedBySeed,
		AddedAt:     time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindPredicateRegistered, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.PredicateRegisteredEvent
	if err := json.Unmarshal(payload, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rePayload, err := json.Marshal(rt)
	if err != nil {
		t.Fatalf("marshal rt: %v", err)
	}
	if !bytes.Equal(payload, rePayload) {
		t.Errorf("round-trip mismatch\n  original: %s\n  got:      %s", payload, rePayload)
	}
	if rt.Predicate != original.Predicate {
		t.Errorf("predicate drift: %q vs %q", rt.Predicate, original.Predicate)
	}
	if rt.Kind != original.Kind {
		t.Errorf("kind drift: %q vs %q", rt.Kind, original.Kind)
	}
	if rt.Description != original.Description {
		t.Errorf("description drift: %q vs %q", rt.Description, original.Description)
	}
	if rt.AddedBy != original.AddedBy {
		t.Errorf("addedBy drift: %q vs %q", rt.AddedBy, original.AddedBy)
	}
	if !rt.AddedAt.Equal(original.AddedAt) {
		t.Errorf("addedAt drift: %v vs %v", rt.AddedAt, original.AddedAt)
	}
}

// TestPredicateRegistered_Register_AgainstRealMemoryOntology exercises
// RegisterPredicateRegistered against the production kernel.MemoryOntology.
func TestPredicateRegistered_Register_AgainstRealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterPredicateRegistered(ctx, o); err != nil {
		t.Fatalf("RegisterPredicateRegistered: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindPredicateRegistered)
	if err != nil {
		t.Fatalf("Schema lookup: %v", err)
	}
	if got.Version != ontology.VersionPredicateRegistered {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionPredicateRegistered)
	}
}

// TestPredicateRegistered_Register_RejectsDoubleRegistration verifies
// second call without higher version returns ErrSchemaConflict.
func TestPredicateRegistered_Register_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newPredicateRegisteredFakeOntology()
	if err := ontology.RegisterPredicateRegistered(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := ontology.RegisterPredicateRegistered(ctx, fake); !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: got %v, want ErrSchemaConflict", err)
	}
}

// TestPredicateRegistered_Validate_MissingRequiredField table-drives
// every required field: drop it and assert validation fails.
func TestPredicateRegistered_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	complete := map[string]any{
		"predicate":   "works_at",
		"kind":        string(ontology.PredicateKindFunctional),
		"description": "The subject works at the object organization.",
		"addedBy":     string(ontology.PredicateAddedBySeed),
		"addedAt":     "2026-05-25T10:00:00Z",
	}
	for _, field := range []string{"predicate", "kind", "description", "addedBy", "addedAt"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newPredicateRegisteredFakeOntology()
			if err := ontology.RegisterPredicateRegistered(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			bad := make(map[string]any, len(complete))
			for k, v := range complete {
				if k == field {
					continue
				}
				bad[k] = v
			}
			payload, err := json.Marshal(bad)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindPredicateRegistered, payload); err == nil {
				t.Errorf("expected validation error for missing %q, got nil", field)
			}
		})
	}
}

// TestPredicateRegistered_Validate_RejectsInvalidKind asserts the kind
// enum rejects values other than functional and non-functional.
func TestPredicateRegistered_Validate_RejectsInvalidKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newPredicateRegisteredFakeOntology()
	if err := ontology.RegisterPredicateRegistered(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	bad := map[string]any{
		"predicate":   "works_at",
		"kind":        "semi-functional",
		"description": "Something.",
		"addedBy":     string(ontology.PredicateAddedBySeed),
		"addedAt":     "2026-05-25T10:00:00Z",
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindPredicateRegistered, payload); err == nil {
		t.Error("expected validation error for invalid kind, got nil")
	}
}

// TestPredicateRegistered_Validate_RejectsInvalidAddedBy asserts the
// addedBy enum rejects values other than user, llm, seed.
func TestPredicateRegistered_Validate_RejectsInvalidAddedBy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newPredicateRegisteredFakeOntology()
	if err := ontology.RegisterPredicateRegistered(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	bad := map[string]any{
		"predicate":   "works_at",
		"kind":        string(ontology.PredicateKindFunctional),
		"description": "Something.",
		"addedBy":     "robot",
		"addedAt":     "2026-05-25T10:00:00Z",
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindPredicateRegistered, payload); err == nil {
		t.Error("expected validation error for invalid addedBy, got nil")
	}
}

// TestPredicateRegistered_AllKindAndAddedByValues asserts that every
// valid PredicateKind and PredicateAddedBy value combination passes
// schema validation.
func TestPredicateRegistered_AllKindAndAddedByValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newPredicateRegisteredFakeOntology()
	if err := ontology.RegisterPredicateRegistered(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	kinds := []ontology.PredicateKind{ontology.PredicateKindFunctional, ontology.PredicateKindNonFunctional}
	addedBys := []ontology.PredicateAddedBy{ontology.PredicateAddedByUser, ontology.PredicateAddedByLLM, ontology.PredicateAddedBySeed}
	for _, k := range kinds {
		for _, a := range addedBys {
			t.Run(string(k)+"/"+string(a), func(t *testing.T) {
				t.Parallel()
				payloadMap := map[string]any{
					"predicate":   "member_of",
					"kind":        string(k),
					"description": "Test predicate.",
					"addedBy":     string(a),
					"addedAt":     "2026-05-25T10:00:00Z",
				}
				payload, err := json.Marshal(payloadMap)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if err := fake.Validate(ctx, ontology.KindPredicateRegistered, payload); err != nil {
					t.Errorf("expected success for kind=%q addedBy=%q, got: %v", k, a, err)
				}
			})
		}
	}
}

// TestPredicateRegistered_TypedRelationFields_Roundtrip asserts the P5
// domain/range fields (subjectKinds, objectKinds, sameAs, propertySchema)
// marshal, validate against the current schema version, and round-trip byte-stably.
func TestPredicateRegistered_TypedRelationFields_Roundtrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newPredicateRegisteredFakeOntology()
	if err := ontology.RegisterPredicateRegistered(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	if ontology.VersionPredicateRegistered != "v0.2.1" {
		t.Errorf("VersionPredicateRegistered = %q, want v0.2.1", ontology.VersionPredicateRegistered)
	}

	original := ontology.PredicateRegisteredEvent{
		Predicate:      "employed_by",
		Kind:           ontology.PredicateKindNonFunctional,
		Description:    "The subject is employed by the object organization.",
		AddedBy:        ontology.PredicateAddedByUser,
		AddedAt:        time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC),
		SubjectKinds:   []string{"person"},
		ObjectKinds:    []string{"organization"},
		SameAs:         []string{"https://schema.org/worksFor", "wikidata:P108"},
		PropertySchema: json.RawMessage(`{"type":"object","properties":{"role":{"type":"string"}}}`),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindPredicateRegistered, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.PredicateRegisteredEvent
	if err := json.Unmarshal(payload, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rePayload, err := json.Marshal(rt)
	if err != nil {
		t.Fatalf("marshal rt: %v", err)
	}
	if !bytes.Equal(payload, rePayload) {
		t.Errorf("round-trip mismatch\n  original: %s\n  got:      %s", payload, rePayload)
	}
	if len(rt.SubjectKinds) != 1 || rt.SubjectKinds[0] != "person" {
		t.Errorf("subjectKinds drift: %v", rt.SubjectKinds)
	}
	if len(rt.ObjectKinds) != 1 || rt.ObjectKinds[0] != "organization" {
		t.Errorf("objectKinds drift: %v", rt.ObjectKinds)
	}
	if len(rt.SameAs) != 2 {
		t.Errorf("sameAs drift: %v", rt.SameAs)
	}
}

// TestPredicateRegistered_TypedRelationFields_OmittedStayValid asserts that an
// event without any P5 field is still valid (the fields are optional/omitempty),
// preserving backward compatibility with v0.1.0 events.
func TestPredicateRegistered_TypedRelationFields_OmittedStayValid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newPredicateRegisteredFakeOntology()
	if err := ontology.RegisterPredicateRegistered(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"predicate":   "owns",
		"kind":        string(ontology.PredicateKindFunctional),
		"description": "Subject owns the object.",
		"addedBy":     string(ontology.PredicateAddedBySeed),
		"addedAt":     "2026-05-25T10:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindPredicateRegistered, payload); err != nil {
		t.Errorf("v0.1.0-shaped event should still validate, got: %v", err)
	}
}

// TestPredicateRegistered_Schema_Cached verifies the schema cache is
// stable across calls.
func TestPredicateRegistered_Schema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.PredicateRegisteredSchema()
	second := ontology.PredicateRegisteredSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Errorf("PredicateRegisteredSchema returned different Definitions across calls")
	}
}
