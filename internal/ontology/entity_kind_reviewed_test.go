package ontology_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestEntityKindReviewed_Register_SchemaRoundtrips registers the
// entityKindReviewed schema, looks it up, validates a complete payload, and
// asserts a byte-stable JSON round-trip.
func TestEntityKindReviewed_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEntityKindReviewed(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := o.Schema(ctx, ontology.KindEntityKindReviewed)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindEntityKindReviewed {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindEntityKindReviewed)
	}
	if got.Version != ontology.VersionEntityKindReviewed {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionEntityKindReviewed)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want core", got.Provenance)
	}

	original := ontology.EntityKindReviewedEvent{
		Name:       "user:vehicle",
		Decision:   ontology.EntityKindReviewApproved,
		ReviewedBy: "cli",
		ReviewedAt: time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC),
		Note:       "Legitimate new kind for the fleet registry.",
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindEntityKindReviewed, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.EntityKindReviewedEvent
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

// TestEntityKindReviewed_Register_Idempotent verifies double-registration is a
// no-op.
func TestEntityKindReviewed_Register_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEntityKindReviewed(ctx, o); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := ontology.RegisterEntityKindReviewed(ctx, o); err != nil {
		t.Errorf("idempotent second register: got %v, want nil", err)
	}
}

// TestEntityKindReviewed_Schema_Cached verifies the lazily-built schema is
// stable across calls (same bytes each time).
func TestEntityKindReviewed_Schema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.EntityKindReviewedSchema()
	second := ontology.EntityKindReviewedSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Error("EntityKindReviewedSchema returned different Definitions across calls")
	}
}

// TestEntityKindReviewed_Validate_MissingRequiredField table-drives every
// required field: drop it and assert validation fails.
func TestEntityKindReviewed_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	complete := map[string]any{
		"name":       "user:vehicle",
		"decision":   "approved",
		"reviewedAt": "2026-06-07T09:00:00Z",
	}
	for _, field := range []string{"name", "decision", "reviewedAt"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := kernel.NewMemoryOntology()
			if err := ontology.RegisterEntityKindReviewed(ctx, o); err != nil {
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
			if err := o.Validate(ctx, ontology.KindEntityKindReviewed, payload); err == nil {
				t.Errorf("expected validation error for missing %q, got nil", field)
			}
		})
	}
}

// TestEntityKindReviewed_Validate_BothDecisions verifies both valid decision
// values pass schema validation.
func TestEntityKindReviewed_Validate_BothDecisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEntityKindReviewed(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}

	for _, d := range []ontology.EntityKindReviewDecision{
		ontology.EntityKindReviewApproved,
		ontology.EntityKindReviewRejected,
	} {
		t.Run(string(d), func(t *testing.T) {
			t.Parallel()
			payload, err := json.Marshal(map[string]any{
				"name":       "user:widget",
				"decision":   string(d),
				"reviewedAt": "2026-06-07T09:00:00Z",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := o.Validate(ctx, ontology.KindEntityKindReviewed, payload); err != nil {
				t.Errorf("decision=%q: expected valid, got %v", d, err)
			}
		})
	}
}

// TestEntityKindReviewed_Validate_BadDecision asserts an out-of-vocabulary
// decision value fails validation.
func TestEntityKindReviewed_Validate_BadDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEntityKindReviewed(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"name":       "user:widget",
		"decision":   "maybe",
		"reviewedAt": "2026-06-07T09:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindEntityKindReviewed, payload); err == nil {
		t.Error("expected validation error for decision=maybe, got nil")
	}
}

// TestEntityKindRegistered_StatusField verifies the v0.2.0 status field on
// EntityKindRegisteredEvent survives a JSON round-trip with both values.
func TestEntityKindRegistered_StatusField(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"active", "proposed", ""} {
		t.Run("status="+status, func(t *testing.T) {
			t.Parallel()
			ev := ontology.EntityKindRegisteredEvent{
				Name:        "user:vehicle",
				DisplayName: "Vehicle",
				Description: "A motorised road vehicle.",
				Provenance:  "user",
				AddedBy:     ontology.EntityKindAddedByUser,
				AddedAt:     time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC),
				Status:      status,
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var rt ontology.EntityKindRegisteredEvent
			if err := json.Unmarshal(raw, &rt); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if rt.Status != status {
				t.Errorf("Status = %q after round-trip, want %q", rt.Status, status)
			}
		})
	}
}

// TestEntityKindRegistered_StatusFieldOmittedWhenEmpty verifies that status is
// omitempty: marshalling with Status="" produces JSON without the status key.
func TestEntityKindRegistered_StatusFieldOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	ev := ontology.EntityKindRegisteredEvent{
		Name:        "user:vehicle",
		DisplayName: "Vehicle",
		Description: "A motorised road vehicle.",
		Provenance:  "user",
		AddedBy:     ontology.EntityKindAddedByUser,
		AddedAt:     time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC),
		// Status is intentionally zero (empty string).
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["status"]; ok {
		t.Errorf("JSON contains status key for empty Status, want omitempty: %s", raw)
	}
}

// TestEntityKindRegistered_VersionIsBumped verifies the schema version constant
// is v0.2.0 as required by the P7 additive field addition.
func TestEntityKindRegistered_VersionIsBumped(t *testing.T) {
	t.Parallel()
	if ontology.VersionEntityKindRegistered != "v0.2.0" {
		t.Errorf("VersionEntityKindRegistered = %q, want v0.2.0", ontology.VersionEntityKindRegistered)
	}
}
