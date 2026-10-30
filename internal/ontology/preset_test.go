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

func TestPresetInstalled_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterPresetInstalled(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := o.Schema(ctx, ontology.KindPresetInstalled)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindPresetInstalled || got.Version != ontology.VersionPresetInstalled {
		t.Errorf("schema = {%q, %q}, want {%q, %q}", got.Kind, got.Version,
			ontology.KindPresetInstalled, ontology.VersionPresetInstalled)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want core", got.Provenance)
	}

	original := ontology.PresetInstalledEvent{
		Name:         "crm",
		Version:      "0.1.0",
		Description:  "Customer-relationship vocabulary and operating playbook.",
		Instructions: "# CRM\nFile deals under crm:Account.\n",
		AddedAt:      time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindPresetInstalled, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.PresetInstalledEvent
	if err := json.Unmarshal(payload, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rePayload, err := json.Marshal(rt)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if !bytes.Equal(payload, rePayload) {
		t.Errorf("round-trip mismatch\n  original: %s\n  got:      %s", payload, rePayload)
	}
}

// TestPresetInstalled_Register_IdempotentReRegister asserts the production
// kernel.Ontology contract: re-registering the identical schema (same version,
// same definition) is a no-op, so booting twice / registering in both the
// kernelboot and daemon ontologies never conflicts.
func TestPresetInstalled_Register_IdempotentReRegister(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterPresetInstalled(ctx, o); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := ontology.RegisterPresetInstalled(ctx, o); err != nil {
		t.Errorf("second identical register: got %v, want nil (idempotent)", err)
	}
}

func TestPresetInstalled_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	complete := map[string]any{
		"name":         "crm",
		"version":      "0.1.0",
		"description":  "desc",
		"instructions": "body",
		"addedAt":      "2026-06-16T12:00:00Z",
	}
	for _, field := range []string{"name", "version", "description", "instructions", "addedAt"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := kernel.NewMemoryOntology()
			if err := ontology.RegisterPresetInstalled(ctx, o); err != nil {
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
			if err := o.Validate(ctx, ontology.KindPresetInstalled, payload); err == nil {
				t.Errorf("validate without %q: got nil, want error", field)
			}
		})
	}
}

func TestPresetInstalled_Validate_NamePattern(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterPresetInstalled(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}
	bad := map[string]any{
		"name":         "CRM Deluxe", // uppercase + space: violates ^[a-z][a-z0-9_-]*$
		"version":      "0.1.0",
		"description":  "desc",
		"instructions": "body",
		"addedAt":      "2026-06-16T12:00:00Z",
	}
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindPresetInstalled, payload); err == nil {
		t.Error("validate with invalid name pattern: got nil, want error")
	}
}

func TestPresetDeactivated_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterPresetDeactivated(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := o.Schema(ctx, ontology.KindPresetDeactivated)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindPresetDeactivated || got.Version != ontology.VersionPresetDeactivated {
		t.Errorf("schema = {%q, %q}, want {%q, %q}", got.Kind, got.Version,
			ontology.KindPresetDeactivated, ontology.VersionPresetDeactivated)
	}

	original := ontology.PresetDeactivatedEvent{
		Name:      "crm",
		RemovedAt: time.Date(2026, 6, 16, 13, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindPresetDeactivated, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.PresetDeactivatedEvent
	if err := json.Unmarshal(payload, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rePayload, err := json.Marshal(rt)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if !bytes.Equal(payload, rePayload) {
		t.Errorf("round-trip mismatch\n  original: %s\n  got:      %s", payload, rePayload)
	}
}
