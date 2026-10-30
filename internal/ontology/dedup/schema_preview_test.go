package dedup_test

import (
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ontology/dedup"
	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

// schemaJSON is a helper to build a json.RawMessage from an object literal.
func schemaJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("schemaJSON: marshal: %v", err)
	}
	return b
}

// hasWarningCode returns true when any of the warnings carries the given code.
func hasWarningCode(warnings []warncode.Warning, code string) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// --- PlanKind tests ---

// TestPlanKind_CleanMint_NoExisting returns action=mint and no warnings for a
// genuinely novel kind against an empty registry.
func TestPlanKind_CleanMint_NoExisting(t *testing.T) {
	t.Parallel()
	req := dedup.KindPreviewRequest{
		Name:        "nebula",
		Description: "An interstellar gas cloud.",
		// SemanticChecked=true: a fully-checked clean mint emits no warnings.
		// (The lexical-only path is covered by TestPlanKind_Mint_NoEmbedder_*.)
		SemanticChecked: true,
	}
	result := dedup.PlanKind(req, nil)
	if result.Action != "mint" {
		t.Errorf("action = %q, want mint", result.Action)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for a clean mint", result.Warnings)
	}
}

// TestPlanKind_Collapse_ExactNameMatch returns action=collapse with
// MAZ-SCHEMA-003 when the candidate name matches an existing kind.
func TestPlanKind_Collapse_ExactNameMatch(t *testing.T) {
	t.Parallel()
	existing := []dedup.ExistingKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle.", EntityCount: 5},
	}
	req := dedup.KindPreviewRequest{
		Name:        "vehicle",
		Description: "A road vehicle.",
	}
	result := dedup.PlanKind(req, existing)
	if result.Action != "collapse" {
		t.Errorf("action = %q, want collapse", result.Action)
	}
	if result.CollapsesInto != "user:vehicle" {
		t.Errorf("collapsesInto = %q, want user:vehicle", result.CollapsesInto)
	}
	if !hasWarningCode(result.Warnings, warncode.CodeSchemaCollapse) {
		t.Errorf("expected MAZ-SCHEMA-003 warning, got: %v", result.Warnings)
	}
}

// TestPlanKind_Supersede_EmitsSupersede returns action=supersede and
// MAZ-SCHEMA-001 when an existing kind already carries a propertySchema
// and the candidate also provides one.
func TestPlanKind_Supersede_EmitsSupersede(t *testing.T) {
	t.Parallel()
	oldSchema := schemaJSON(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"color": map[string]any{"type": "string"},
		},
	})
	newSchema := schemaJSON(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"color": map[string]any{"type": "string"},
			"make":  map[string]any{"type": "string"},
		},
	})
	existing := []dedup.ExistingKind{
		{Name: "user:vehicle", PropertySchema: oldSchema, EntityCount: 3},
	}
	req := dedup.KindPreviewRequest{
		Name:           "vehicle",
		PropertySchema: newSchema,
	}
	result := dedup.PlanKind(req, existing)
	if result.Action != "supersede" {
		t.Errorf("action = %q, want supersede", result.Action)
	}
	if !hasWarningCode(result.Warnings, warncode.CodeSchemaSupersede) {
		t.Errorf("expected MAZ-SCHEMA-001 warning, got: %v", result.Warnings)
	}
	// Backward-compatible change (only addition): orphan count must be zero.
	if result.OrphanCount != 0 {
		t.Errorf("orphanCount = %d, want 0 for a backward-compatible addition", result.OrphanCount)
	}
}

// TestPlanKind_Orphan_RemovedField emits MAZ-SCHEMA-002 when a field present in
// the existing schema is absent from the candidate schema.
func TestPlanKind_Orphan_RemovedField(t *testing.T) {
	t.Parallel()
	oldSchema := schemaJSON(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"color":  map[string]any{"type": "string"},
			"weight": map[string]any{"type": "number"},
		},
	})
	// New schema drops "weight".
	newSchema := schemaJSON(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"color": map[string]any{"type": "string"},
		},
	})
	existing := []dedup.ExistingKind{
		{Name: "user:vehicle", PropertySchema: oldSchema, EntityCount: 7},
	}
	req := dedup.KindPreviewRequest{
		Name:           "vehicle",
		PropertySchema: newSchema,
	}
	result := dedup.PlanKind(req, existing)
	if result.Action != "supersede" {
		t.Errorf("action = %q, want supersede", result.Action)
	}
	if !hasWarningCode(result.Warnings, warncode.CodeSchemaOrphan) {
		t.Errorf("expected MAZ-SCHEMA-002 warning, got: %v", result.Warnings)
	}
	// OrphanCount is the EntityCount of the affected kind (conservative upper bound).
	if result.OrphanCount != 7 {
		t.Errorf("orphanCount = %d, want 7", result.OrphanCount)
	}
}

// --- PlanSchema tests ---

// TestPlanSchema_Register_FirstTime returns action=register and no warnings
// when no existing schema is registered for the kind.
func TestPlanSchema_Register_FirstTime(t *testing.T) {
	t.Parallel()
	req := dedup.SchemaPreviewRequest{
		SchemaKind: "entityKind:vehicle",
		Version:    "v0.1.0",
		Definition: schemaJSON(t, map[string]any{"type": "object"}),
	}
	result := dedup.PlanSchema(req, nil)
	if result.Action != "register" {
		t.Errorf("action = %q, want register", result.Action)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for a first registration", result.Warnings)
	}
}

// TestPlanSchema_Supersede_EmitsSupersede returns action=supersede with
// MAZ-SCHEMA-001 when a lower version is already registered.
func TestPlanSchema_Supersede_EmitsSupersede(t *testing.T) {
	t.Parallel()
	existing := []dedup.ExistingSchema{
		{
			SchemaKind: "entityKind:vehicle",
			Version:    "v0.1.0",
			Definition: schemaJSON(t, map[string]any{
				"type":       "object",
				"properties": map[string]any{"color": map[string]any{"type": "string"}},
			}),
		},
	}
	req := dedup.SchemaPreviewRequest{
		SchemaKind: "entityKind:vehicle",
		Version:    "v0.2.0",
		Definition: schemaJSON(t, map[string]any{
			"type":       "object",
			"properties": map[string]any{"color": map[string]any{"type": "string"}},
		}),
	}
	result := dedup.PlanSchema(req, existing)
	if result.Action != "supersede" {
		t.Errorf("action = %q, want supersede", result.Action)
	}
	if result.SupersedesFrom != "v0.1.0" {
		t.Errorf("supersedesFrom = %q, want v0.1.0", result.SupersedesFrom)
	}
	if !hasWarningCode(result.Warnings, warncode.CodeSchemaSupersede) {
		t.Errorf("expected MAZ-SCHEMA-001, got: %v", result.Warnings)
	}
}

// TestPlanSchema_Orphan_RemovedField counts fields removed from the existing
// definition and emits MAZ-SCHEMA-002 with the count.
func TestPlanSchema_Orphan_RemovedField(t *testing.T) {
	t.Parallel()
	existing := []dedup.ExistingSchema{
		{
			SchemaKind: "entityKind:deal",
			Version:    "v1.0.0",
			Definition: schemaJSON(t, map[string]any{
				"type": "object",
				"properties": map[string]any{
					"value":  map[string]any{"type": "number"},
					"stage":  map[string]any{"type": "string"},
					"closed": map[string]any{"type": "boolean"},
				},
			}),
		},
	}
	// New definition drops "stage" and "closed" — 2 orphaned fields.
	req := dedup.SchemaPreviewRequest{
		SchemaKind: "entityKind:deal",
		Version:    "v2.0.0",
		Definition: schemaJSON(t, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"value": map[string]any{"type": "number"},
			},
		}),
	}
	result := dedup.PlanSchema(req, existing)
	if result.OrphanCount != 2 {
		t.Errorf("orphanCount = %d, want 2", result.OrphanCount)
	}
	if !hasWarningCode(result.Warnings, warncode.CodeSchemaOrphan) {
		t.Errorf("expected MAZ-SCHEMA-002, got: %v", result.Warnings)
	}
}

// TestPlanKind_Mint_NearDuplicate_SurfacesCloseMatch verifies that a brand-new
// kind name with no exact/normalized match but a high lexical similarity to an
// existing kind still mints, but surfaces the nearest kinds, sets CloseMatch,
// and emits a MAZ-KIND-001 warning (so "would this collapse?" is answerable).
func TestPlanKind_Mint_NearDuplicate_SurfacesCloseMatch(t *testing.T) {
	t.Parallel()
	existing := []dedup.ExistingKind{
		{Name: "user:vehicle", Description: "A motorised road vehicle."},
		{Name: "user:deal", Description: "A sales deal."},
	}
	// "vehicles" (plural) has ~0.875 lexical similarity to "vehicle" — a
	// near-duplicate above MintAliasThreshold but with no exact/normalized match.
	req := dedup.KindPreviewRequest{
		Name:            "vehicles",
		Description:     "A road vehicle.",
		SemanticChecked: true, // isolate the closeMatch signal from MAZ-KIND-002
	}
	result := dedup.PlanKind(req, existing)
	if result.Action != "mint" {
		t.Errorf("action = %q, want mint", result.Action)
	}
	if result.CloseMatch != "user:vehicle" {
		t.Errorf("closeMatch = %q, want user:vehicle", result.CloseMatch)
	}
	if len(result.NearestKinds) == 0 {
		t.Fatal("nearestKinds is empty, want at least one ranked entry")
	}
	if result.NearestKinds[0].Kind != "user:vehicle" {
		t.Errorf("nearestKinds[0].kind = %q, want user:vehicle (highest similarity first)",
			result.NearestKinds[0].Kind)
	}
	// Sorted descending: the deal entry (if present) must not outrank vehicle.
	for i := 1; i < len(result.NearestKinds); i++ {
		if result.NearestKinds[i].Similarity > result.NearestKinds[i-1].Similarity {
			t.Errorf("nearestKinds not sorted desc at index %d: %v", i, result.NearestKinds)
		}
	}
	if !hasWarningCode(result.Warnings, warncode.CodeKindCloseMatch) {
		t.Errorf("expected MAZ-KIND-001 warning, got: %v", result.Warnings)
	}
}

// TestPlanKind_Mint_NoEmbedder_SetsChecksSkipped verifies that when the caller
// declares no semantic pass ran (SemanticChecked=false), the mint plan records
// ChecksSkipped=["semantic"] and emits MAZ-KIND-002.
func TestPlanKind_Mint_NoEmbedder_SetsChecksSkipped(t *testing.T) {
	t.Parallel()
	req := dedup.KindPreviewRequest{
		Name:        "nebula",
		Description: "An interstellar gas cloud.",
		// SemanticChecked defaults to false → semantic layer was skipped.
	}
	result := dedup.PlanKind(req, nil)
	if result.Action != "mint" {
		t.Errorf("action = %q, want mint", result.Action)
	}
	if len(result.ChecksSkipped) != 1 || result.ChecksSkipped[0] != "semantic" {
		t.Errorf("checksSkipped = %v, want [semantic]", result.ChecksSkipped)
	}
	if !hasWarningCode(result.Warnings, warncode.CodeKindDedupDegraded) {
		t.Errorf("expected MAZ-KIND-002 warning, got: %v", result.Warnings)
	}
}

// TestPlanKind_Mint_SemanticChecked_NoDegradedWarning verifies that when the
// caller declares the semantic pass ran, no MAZ-KIND-002 warning is emitted and
// ChecksSkipped stays nil.
func TestPlanKind_Mint_SemanticChecked_NoDegradedWarning(t *testing.T) {
	t.Parallel()
	req := dedup.KindPreviewRequest{
		Name:            "nebula",
		Description:     "An interstellar gas cloud.",
		SemanticChecked: true,
	}
	result := dedup.PlanKind(req, nil)
	if len(result.ChecksSkipped) != 0 {
		t.Errorf("checksSkipped = %v, want none when semantic pass ran", result.ChecksSkipped)
	}
	if hasWarningCode(result.Warnings, warncode.CodeKindDedupDegraded) {
		t.Errorf("unexpected MAZ-KIND-002 when semantic pass ran: %v", result.Warnings)
	}
}

// TestPlanKind_NoPropertySchema_NoOrphanWarning verifies that when neither the
// existing kind nor the candidate carry a propertySchema, no orphan warning is
// emitted and the action is mint.
func TestPlanKind_NoPropertySchema_NoOrphanWarning(t *testing.T) {
	t.Parallel()
	existing := []dedup.ExistingKind{
		{Name: "user:task", Description: "A task item.", EntityCount: 12},
	}
	// Different name → mint; no schemas involved.
	req := dedup.KindPreviewRequest{
		Name:        "milestone",
		Description: "A project milestone.",
	}
	result := dedup.PlanKind(req, existing)
	if result.Action != "mint" {
		t.Errorf("action = %q, want mint", result.Action)
	}
	if hasWarningCode(result.Warnings, warncode.CodeSchemaOrphan) {
		t.Errorf("unexpected MAZ-SCHEMA-002 when no propertySchemas are involved")
	}
}
