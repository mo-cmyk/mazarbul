package ontology_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func TestRelationSupersedeConflict_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationRetractedFakeOntology()
	if err := ontology.RegisterRelationSupersedeConflict(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := fake.Schema(ctx, ontology.KindRelationSupersedeConflict)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindRelationSupersedeConflict {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindRelationSupersedeConflict)
	}
	if got.Version != ontology.VersionRelationSupersedeConflict {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionRelationSupersedeConflict)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want %q", got.Provenance, "core")
	}

	original := ontology.RelationSupersedeConflictEvent{
		PrimaryRelationUID:     ontology.ULID(ulid.Make().String()),
		ConflictingRelationUID: ontology.ULID(ulid.Make().String()),
		SubjectUID:             ontology.ULID(ulid.Make().String()),
		Predicate:              "works_at",
		Reason:                 "multiple_open_functional",
		DetectedAt:             time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindRelationSupersedeConflict, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.RelationSupersedeConflictEvent
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
}

func TestRelationSupersedeConflict_Register_AgainstRealMemoryOntology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterRelationSupersedeConflict(ctx, o); err != nil {
		t.Fatalf("RegisterRelationSupersedeConflict: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindRelationSupersedeConflict)
	if err != nil {
		t.Fatalf("Schema lookup: %v", err)
	}
	if got.Version != ontology.VersionRelationSupersedeConflict {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionRelationSupersedeConflict)
	}
}

func TestRelationSupersedeConflict_Register_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationRetractedFakeOntology()
	if err := ontology.RegisterRelationSupersedeConflict(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := ontology.RegisterRelationSupersedeConflict(ctx, fake); !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: got %v, want ErrSchemaConflict", err)
	}
}

func TestRelationSupersedeConflict_Validate_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationRetractedFakeOntology()
	if err := ontology.RegisterRelationSupersedeConflict(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	// missing predicate, reason, detectedAt
	payload := []byte(`{
		"primaryRelationUid": "01HKJZNB3T0000000000000000",
		"conflictingRelationUid": "01HKJZNB3T1111111111111111",
		"subjectUid": "01HKJZNB3T2222222222222222"
	}`)
	if err := fake.Validate(ctx, ontology.KindRelationSupersedeConflict, payload); err == nil {
		t.Fatalf("Validate must reject payload missing required fields")
	}
}
