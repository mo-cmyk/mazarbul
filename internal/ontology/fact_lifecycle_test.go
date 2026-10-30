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

// TestFactRetracted_Register_SchemaRoundtrips registers, validates a fully
// populated event, and asserts a byte-stable round-trip. It reuses the
// map-backed fake ontology defined alongside the relationRetracted tests.
func TestFactRetracted_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationRetractedFakeOntology()
	if err := ontology.RegisterFactRetracted(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := fake.Schema(ctx, ontology.KindFactRetracted)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindFactRetracted {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindFactRetracted)
	}
	if got.Version != ontology.VersionFactRetracted {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionFactRetracted)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want core", got.Provenance)
	}

	original := ontology.FactRetractedEvent{
		FactUID:     ulid.Make().String(),
		RetractedAt: time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindFactRetracted, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.FactRetractedEvent
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

// TestFactRetracted_Validate_MissingRequiredField drops each required field and
// asserts validation fails.
func TestFactRetracted_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	complete := map[string]any{
		"factUid":     ulid.Make().String(),
		"retractedAt": "2026-05-25T10:00:00Z",
	}
	for _, field := range []string{"factUid", "retractedAt"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake := newRelationRetractedFakeOntology()
			if err := ontology.RegisterFactRetracted(ctx, fake); err != nil {
				t.Fatalf("register: %v", err)
			}
			bad := make(map[string]any, len(complete))
			for k, v := range complete {
				if k != field {
					bad[k] = v
				}
			}
			payload, err := json.Marshal(bad)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := fake.Validate(ctx, ontology.KindFactRetracted, payload); err == nil {
				t.Errorf("expected validation error for missing %q, got nil", field)
			}
		})
	}
}

// TestFactRetracted_Register_RejectsDoubleRegistration verifies a second
// registration without a higher version returns ErrSchemaConflict.
func TestFactRetracted_Register_RejectsDoubleRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationRetractedFakeOntology()
	if err := ontology.RegisterFactRetracted(ctx, fake); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := ontology.RegisterFactRetracted(ctx, fake); !errors.Is(err, kernel.ErrSchemaConflict) {
		t.Errorf("second register: got %v, want ErrSchemaConflict", err)
	}
}

// TestFactForgotten_Register_SchemaRoundtrips registers + validates the
// hard-delete event, including the optional reason.
func TestFactForgotten_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRelationRetractedFakeOntology()
	if err := ontology.RegisterFactForgotten(ctx, fake); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := fake.Schema(ctx, ontology.KindFactForgotten)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Version != ontology.VersionFactForgotten {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionFactForgotten)
	}

	// Required fields present, optional reason omitted — must validate.
	payload, err := json.Marshal(map[string]any{
		"factUid":     ulid.Make().String(),
		"forgottenAt": "2026-05-25T10:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindFactForgotten, payload); err != nil {
		t.Fatalf("validate (no reason): %v", err)
	}

	// Missing the required factUid fails.
	bad, err := json.Marshal(map[string]any{"forgottenAt": "2026-05-25T10:00:00Z"})
	if err != nil {
		t.Fatalf("marshal bad: %v", err)
	}
	if err := fake.Validate(ctx, ontology.KindFactForgotten, bad); err == nil {
		t.Error("expected validation error for missing factUid, got nil")
	}
}

// TestFactLifecycle_Schema_Cached verifies the schema caches are stable.
func TestFactLifecycle_Schema_Cached(t *testing.T) {
	t.Parallel()
	if !bytes.Equal(ontology.FactRetractedSchema().Definition, ontology.FactRetractedSchema().Definition) {
		t.Error("FactRetractedSchema returned different Definitions across calls")
	}
	if !bytes.Equal(ontology.FactForgottenSchema().Definition, ontology.FactForgottenSchema().Definition) {
		t.Error("FactForgottenSchema returned different Definitions across calls")
	}
}
