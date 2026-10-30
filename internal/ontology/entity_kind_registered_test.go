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

// TestEntityKindRegistered_Register_SchemaRoundtrips builds an
// EntityKindRegisteredEvent with every field populated, registers the schema,
// validates the payload, and asserts a byte-stable round-trip.
func TestEntityKindRegistered_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEntityKindRegistered(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := o.Schema(ctx, ontology.KindEntityKindRegistered)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindEntityKindRegistered {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindEntityKindRegistered)
	}
	if got.Version != ontology.VersionEntityKindRegistered {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionEntityKindRegistered)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want %q", got.Provenance, "core")
	}

	original := ontology.EntityKindRegisteredEvent{
		Name:           "user:research_paper",
		DisplayName:    "Research Paper",
		Description:    "A scholarly article or preprint.",
		Extends:        "document",
		SameAs:         []string{"schema.org/ScholarlyArticle", "wikidata:Q13442814"},
		PropertySchema: json.RawMessage(`{"type":"object","properties":{"doi":{"type":"string"}}}`),
		Provenance:     "user",
		AddedBy:        ontology.EntityKindAddedByUser,
		AddedAt:        time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindEntityKindRegistered, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.EntityKindRegisteredEvent
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

// TestEntityKindRegistered_Register_Idempotent verifies that registering the
// identical (Kind, Version, Definition) schema twice is a no-op per the
// kernel.Ontology contract, not a conflict.
func TestEntityKindRegistered_Register_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEntityKindRegistered(ctx, o); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := ontology.RegisterEntityKindRegistered(ctx, o); err != nil {
		t.Errorf("idempotent second register: got %v, want nil", err)
	}
}

// TestEntityKindRegistered_Validate_MissingRequiredField table-drives every
// required field: drop it and assert validation fails.
func TestEntityKindRegistered_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	complete := map[string]any{
		"name":        "user:deal",
		"displayName": "Deal",
		"description": "A sales or investment deal.",
		"provenance":  "user",
		"addedBy":     string(ontology.EntityKindAddedByUser),
		"addedAt":     "2026-06-05T10:00:00Z",
	}
	for _, field := range []string{"name", "displayName", "description", "provenance", "addedBy", "addedAt"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := kernel.NewMemoryOntology()
			if err := ontology.RegisterEntityKindRegistered(ctx, o); err != nil {
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
			if err := o.Validate(ctx, ontology.KindEntityKindRegistered, payload); err == nil {
				t.Errorf("expected validation error for missing %q, got nil", field)
			}
		})
	}
}

// TestEntityKindRegistered_Validate_Names asserts the name pattern accepts
// bare core kinds and namespaced custom kinds, and rejects malformed names.
func TestEntityKindRegistered_Validate_Names(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ok   bool
	}{
		{"person", true},
		{"research_paper", true},
		{"user:deal", true},
		{"extension:schema_org:Person", true},
		{"extension:schema_org:ScholarlyArticle", true},
		{"Person", false},    // bare must be lowercase
		{"has space", false}, // no spaces
		{"user:", false},     // empty namespaced tail
		{":deal", false},     // empty prefix
		{"1deal", false},     // must start with a letter
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := kernel.NewMemoryOntology()
			if err := ontology.RegisterEntityKindRegistered(ctx, o); err != nil {
				t.Fatalf("register: %v", err)
			}
			payload, err := json.Marshal(map[string]any{
				"name":        tc.name,
				"displayName": "X",
				"description": "A kind.",
				"provenance":  "user",
				"addedBy":     string(ontology.EntityKindAddedByUser),
				"addedAt":     "2026-06-05T10:00:00Z",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = o.Validate(ctx, ontology.KindEntityKindRegistered, payload)
			if tc.ok && err != nil {
				t.Errorf("name %q: expected valid, got %v", tc.name, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("name %q: expected invalid, got nil", tc.name)
			}
		})
	}
}

// TestEntityKindRegistered_Validate_Extends asserts the extends pattern accepts
// valid parent kind names and rejects malformed parent references, matching the
// name grammar so a durable event cannot carry an unresolvable parent.
func TestEntityKindRegistered_Validate_Extends(t *testing.T) {
	t.Parallel()
	cases := []struct {
		extends string
		ok      bool
	}{
		{"document", true},
		{"user:deal", true},
		{"extension:schema_org:Person", true},
		{"not a valid kind", false}, // spaces
		{"Document", false},         // bare must be lowercase
		{"user:", false},            // empty namespaced tail
	}
	for _, tc := range cases {
		t.Run(tc.extends, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := kernel.NewMemoryOntology()
			if err := ontology.RegisterEntityKindRegistered(ctx, o); err != nil {
				t.Fatalf("register: %v", err)
			}
			payload, err := json.Marshal(map[string]any{
				"name":        "user:research_paper",
				"displayName": "Research Paper",
				"description": "A scholarly article.",
				"extends":     tc.extends,
				"provenance":  "user",
				"addedBy":     string(ontology.EntityKindAddedByUser),
				"addedAt":     "2026-06-05T10:00:00Z",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = o.Validate(ctx, ontology.KindEntityKindRegistered, payload)
			if tc.ok && err != nil {
				t.Errorf("extends %q: expected valid, got %v", tc.extends, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("extends %q: expected invalid, got nil", tc.extends)
			}
		})
	}
}

// TestEntityKindRegistered_Validate_RejectsBadProvenanceAndAddedBy asserts the
// provenance pattern and addedBy enum reject out-of-vocabulary values.
func TestEntityKindRegistered_Validate_RejectsBadProvenanceAndAddedBy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEntityKindRegistered(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}
	for name, mutate := range map[string]func(m map[string]any){
		"bad provenance": func(m map[string]any) { m["provenance"] = "team" },
		"bad addedBy":    func(m map[string]any) { m["addedBy"] = "robot" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := map[string]any{
				"name":        "user:deal",
				"displayName": "Deal",
				"description": "A deal.",
				"provenance":  "user",
				"addedBy":     string(ontology.EntityKindAddedByUser),
				"addedAt":     "2026-06-05T10:00:00Z",
			}
			mutate(m)
			payload, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := o.Validate(ctx, ontology.KindEntityKindRegistered, payload); err == nil {
				t.Errorf("%s: expected validation error, got nil", name)
			}
		})
	}
}

// TestEntityKindRegistered_AllAddedByValues asserts every AddedBy value passes
// schema validation.
func TestEntityKindRegistered_AllAddedByValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEntityKindRegistered(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, a := range []ontology.EntityKindAddedBy{
		ontology.EntityKindAddedBySeed,
		ontology.EntityKindAddedByLLM,
		ontology.EntityKindAddedByUser,
		ontology.EntityKindAddedByExtension,
	} {
		t.Run(string(a), func(t *testing.T) {
			t.Parallel()
			prov := "user"
			if a == ontology.EntityKindAddedBySeed {
				prov = "core"
			}
			if a == ontology.EntityKindAddedByExtension {
				prov = "extension:business_de"
			}
			payload, err := json.Marshal(map[string]any{
				"name":        "vehicle",
				"displayName": "Vehicle",
				"description": "A means of transport.",
				"provenance":  prov,
				"addedBy":     string(a),
				"addedAt":     "2026-06-05T10:00:00Z",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := o.Validate(ctx, ontology.KindEntityKindRegistered, payload); err != nil {
				t.Errorf("addedBy=%q: expected success, got %v", a, err)
			}
		})
	}
}

// TestEntityKindRegistered_Schema_Cached verifies the schema cache is stable
// across calls.
func TestEntityKindRegistered_Schema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.EntityKindRegisteredSchema()
	second := ontology.EntityKindRegisteredSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Errorf("EntityKindRegisteredSchema returned different Definitions across calls")
	}
}
