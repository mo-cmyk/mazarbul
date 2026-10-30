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

// entityDeletedFakeOntology is a map-backed test double for kernel.Ontology,
// following the same pattern as the sibling fake types in this package.
type entityDeletedFakeOntology struct {
	schemas map[string]kernel.Schema
}

func newEntityDeletedFakeOntology() *entityDeletedFakeOntology {
	return &entityDeletedFakeOntology{schemas: make(map[string]kernel.Schema)}
}

func (f *entityDeletedFakeOntology) Schema(_ context.Context, kind string) (kernel.Schema, error) {
	s, ok := f.schemas[kind]
	if !ok {
		return kernel.Schema{}, kernel.ErrNotFound
	}
	return s, nil
}

func (f *entityDeletedFakeOntology) Register(_ context.Context, s kernel.Schema) error {
	if _, ok := f.schemas[s.Kind]; ok {
		return kernel.ErrSchemaConflict
	}
	f.schemas[s.Kind] = s
	return nil
}

func (f *entityDeletedFakeOntology) List(_ context.Context) ([]kernel.Schema, error) {
	out := make([]kernel.Schema, 0, len(f.schemas))
	for _, s := range f.schemas {
		out = append(out, s)
	}
	return out, nil
}

func (f *entityDeletedFakeOntology) Validate(ctx context.Context, kind string, payload json.RawMessage) error {
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

// TestEntityDeleted_Schema_KindVersionDefinition checks that EntityDeletedSchema
// returns the expected Kind, Version, and a non-empty, valid JSON Definition.
func TestEntityDeleted_Schema_KindVersionDefinition(t *testing.T) {
	t.Parallel()
	s := ontology.EntityDeletedSchema()
	if s.Kind != ontology.KindEntityDeleted {
		t.Errorf("Kind = %q, want %q", s.Kind, ontology.KindEntityDeleted)
	}
	if s.Version != ontology.VersionEntityDeleted {
		t.Errorf("Version = %q, want %q", s.Version, ontology.VersionEntityDeleted)
	}
	if len(s.Definition) == 0 {
		t.Fatal("Definition is empty")
	}
	if !json.Valid(s.Definition) {
		t.Errorf("Definition is not valid JSON: %s", s.Definition)
	}
}

// TestEntityDeleted_Schema_Cached verifies the schema cache is stable across
// calls (sync.OnceValue guarantees identical Definition bytes).
func TestEntityDeleted_Schema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.EntityDeletedSchema()
	second := ontology.EntityDeletedSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Errorf("EntityDeletedSchema returned different Definitions across calls")
	}
}

// TestEntityDeleted_Register_FirstRegistrationWins verifies RegisterEntityDeleted
// registers without error and a second call returns ErrSchemaConflict.
func TestEntityDeleted_Register_FirstRegistrationWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityDeletedFakeOntology()
	if err := ontology.RegisterEntityDeleted(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := ontology.RegisterEntityDeleted(ctx, fake); !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: got %v, want ErrSchemaConflict", err)
	}
}

// TestEntityDeleted_Register_AgainstRealMemoryOntology exercises
// RegisterEntityDeleted against the production kernel.MemoryOntology.
func TestEntityDeleted_Register_AgainstRealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEntityDeleted(ctx, o); err != nil {
		t.Fatalf("RegisterEntityDeleted: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindEntityDeleted)
	if err != nil {
		t.Fatalf("Schema lookup: %v", err)
	}
	if got.Version != ontology.VersionEntityDeleted {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionEntityDeleted)
	}
}

// TestEntityDeleted_Payload_RoundTrip marshals an EntityDeletedEvent with all
// fields populated, validates it against the registered schema, then
// unmarshals and asserts field preservation.
func TestEntityDeleted_Payload_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityDeletedFakeOntology()
	if err := ontology.RegisterEntityDeleted(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	original := ontology.EntityDeletedEvent{
		EntityUID: ulid.Make().String(),
		Reason:    "merged into a canonical entity",
		DeletedAt: time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindEntityDeleted, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.EntityDeletedEvent
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
	if rt.EntityUID != original.EntityUID {
		t.Errorf("entityUid drift: %q vs %q", rt.EntityUID, original.EntityUID)
	}
	if rt.Reason != original.Reason {
		t.Errorf("reason drift: %q vs %q", rt.Reason, original.Reason)
	}
	if !rt.DeletedAt.Equal(original.DeletedAt) {
		t.Errorf("deletedAt drift: %v vs %v", rt.DeletedAt, original.DeletedAt)
	}
}

// TestEntityDeleted_Payload_OptionalReason verifies a payload with no reason
// field validates correctly (reason is omitempty / not required).
func TestEntityDeleted_Payload_OptionalReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newEntityDeletedFakeOntology()
	if err := ontology.RegisterEntityDeleted(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"entityUid": ulid.Make().String(),
		"deletedAt": "2026-06-26T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindEntityDeleted, payload); err != nil {
		t.Fatalf("payload without reason must validate, got: %v", err)
	}
	// The Go struct with empty Reason omits the field (omitempty).
	body, err := json.Marshal(ontology.EntityDeletedEvent{
		EntityUID: ulid.Make().String(),
		DeletedAt: time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal struct: %v", err)
	}
	if bytes.Contains(body, []byte("reason")) {
		t.Errorf("empty Reason must be omitted from the wire form, got: %s", body)
	}
}

// TestEntityDeleted_Validate_MissingRequiredField asserts that removing either
// of the two required fields (entityUid, deletedAt) from the payload causes
// validation to fail.
func TestEntityDeleted_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	entityUID := ulid.Make().String()
	complete := map[string]any{
		"entityUid": entityUID,
		"deletedAt": "2026-06-26T12:00:00Z",
	}
	for _, field := range []string{"entityUid", "deletedAt"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newEntityDeletedFakeOntology()
			if err := ontology.RegisterEntityDeleted(ctx, fake); err != nil {
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
			if err := fake.Validate(ctx, ontology.KindEntityDeleted, payload); err == nil {
				t.Errorf("expected validation error for missing %q, got nil", field)
			}
		})
	}
}
