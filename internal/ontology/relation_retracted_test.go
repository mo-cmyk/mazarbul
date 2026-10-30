package ontology_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// relationRetractedFakeOntology is a map-backed test double for
// kernel.Ontology, following the same pattern as the sibling fake types in
// this package.
type relationRetractedFakeOntology struct {
	schemas map[string]kernel.Schema
}

func newRelationRetractedFakeOntology() *relationRetractedFakeOntology {
	return &relationRetractedFakeOntology{schemas: make(map[string]kernel.Schema)}
}

func (f *relationRetractedFakeOntology) Schema(_ context.Context, kind string) (kernel.Schema, error) {
	s, ok := f.schemas[kind]
	if !ok {
		return kernel.Schema{}, kernel.ErrNotFound
	}
	return s, nil
}

func (f *relationRetractedFakeOntology) Register(_ context.Context, s kernel.Schema) error {
	if _, ok := f.schemas[s.Kind]; ok {
		return kernel.ErrSchemaConflict
	}
	f.schemas[s.Kind] = s
	return nil
}

func (f *relationRetractedFakeOntology) List(_ context.Context) ([]kernel.Schema, error) {
	out := make([]kernel.Schema, 0, len(f.schemas))
	for _, s := range f.schemas {
		out = append(out, s)
	}
	return out, nil
}

func (f *relationRetractedFakeOntology) Validate(ctx context.Context, kind string, payload json.RawMessage) error {
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

// TestRelationRetracted_Register_SchemaRoundtrips builds a
// RelationRetractedEvent with every field populated, registers the schema,
// marshals, validates, and asserts byte-stable round-trip.
func TestRelationRetracted_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationRetractedFakeOntology()
	if err := ontology.RegisterRelationRetracted(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := fake.Schema(ctx, ontology.KindRelationRetracted)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindRelationRetracted {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindRelationRetracted)
	}
	if got.Version != ontology.VersionRelationRetracted {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionRelationRetracted)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want %q", got.Provenance, "core")
	}

	original := ontology.RelationRetractedEvent{
		RelationUID:     ontology.ULID(ulid.Make().String()),
		SupersededByUID: ontology.ULID(ulid.Make().String()),
		RetractedAt:     time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindRelationRetracted, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.RelationRetractedEvent
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
	if rt.RelationUID != original.RelationUID {
		t.Errorf("relationUid drift: %q vs %q", rt.RelationUID, original.RelationUID)
	}
	if rt.SupersededByUID != original.SupersededByUID {
		t.Errorf("supersededByUid drift: %q vs %q", rt.SupersededByUID, original.SupersededByUID)
	}
	if !rt.RetractedAt.Equal(original.RetractedAt) {
		t.Errorf("retractedAt drift: %v vs %v", rt.RetractedAt, original.RetractedAt)
	}
}

// TestRelationRetracted_Register_AgainstRealMemoryOntology exercises
// RegisterRelationRetracted against the production kernel.MemoryOntology.
func TestRelationRetracted_Register_AgainstRealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterRelationRetracted(ctx, o); err != nil {
		t.Fatalf("RegisterRelationRetracted: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindRelationRetracted)
	if err != nil {
		t.Fatalf("Schema lookup: %v", err)
	}
	if got.Version != ontology.VersionRelationRetracted {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionRelationRetracted)
	}
}

// TestRelationRetracted_Register_RejectsDoubleRegistration verifies a second
// call without a higher version returns ErrSchemaConflict.
func TestRelationRetracted_Register_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationRetractedFakeOntology()
	if err := ontology.RegisterRelationRetracted(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := ontology.RegisterRelationRetracted(ctx, fake); !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: got %v, want ErrSchemaConflict", err)
	}
}

// TestRelationRetracted_Validate_MissingRequiredField table-drives every
// required field: drop it, assert validation fails. supersededByUid is NOT
// required (it is empty for a pure temporal close — see
// TestRelationRetracted_Validate_OptionalSuperseder).
func TestRelationRetracted_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	relUID := ulid.Make().String()
	superUID := ulid.Make().String()
	complete := map[string]any{
		"relationUid":     relUID,
		"supersededByUid": superUID,
		"retractedAt":     "2026-05-25T10:00:00Z",
	}
	for _, field := range []string{"relationUid", "retractedAt"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newRelationRetractedFakeOntology()
			if err := ontology.RegisterRelationRetracted(ctx, fake); err != nil {
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
			if err := fake.Validate(ctx, ontology.KindRelationRetracted, payload); err == nil {
				t.Errorf("expected validation error for missing %q, got nil", field)
			}
		})
	}
}

// TestRelationRetracted_Validate_OptionalSuperseder is the regression for the
// pure-temporal-close path (the unrelate tool): a relationRetracted WITHOUT a
// supersededByUid — the relationship simply ended, no replacement — must
// validate. The fold (ReduceRetractions) closes it by valid_until alone and
// leaves superseded_by_uid NULL.
func TestRelationRetracted_Validate_OptionalSuperseder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationRetractedFakeOntology()
	if err := ontology.RegisterRelationRetracted(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	// No supersededByUid key at all — the pure-close shape unrelate emits.
	payload, err := json.Marshal(map[string]any{
		"relationUid": ulid.Make().String(),
		"retractedAt": "2026-05-22T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindRelationRetracted, payload); err != nil {
		t.Fatalf("pure temporal close (no supersededByUid) must validate, got: %v", err)
	}

	// And the Go struct with an empty SupersededByUID marshals WITHOUT the key
	// (omitempty), so the wire shape matches what was just validated.
	body, err := json.Marshal(ontology.RelationRetractedEvent{
		RelationUID: ontology.ULID(ulid.Make().String()),
		RetractedAt: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal struct: %v", err)
	}
	if bytes.Contains(body, []byte("supersededByUid")) {
		t.Errorf("empty SupersededByUID must be omitted from the wire form, got: %s", body)
	}
}

// TestRelationRetracted_Schema_Cached verifies the schema cache is stable
// across calls.
func TestRelationRetracted_Schema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.RelationRetractedSchema()
	second := ontology.RelationRetractedSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Errorf("RelationRetractedSchema returned different Definitions across calls")
	}
}
