package ontology_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestSchemaRegistered_Register_SchemaRoundtrips builds a SchemaRegisteredEvent
// with every field populated, registers the schema, validates the payload, and
// asserts a byte-stable round-trip.
func TestSchemaRegistered_Register_SchemaRoundtrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterSchemaRegistered(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := o.Schema(ctx, ontology.KindSchemaRegistered)
	if err != nil {
		t.Fatalf("lookup schema: %v", err)
	}
	if got.Kind != ontology.KindSchemaRegistered {
		t.Errorf("schema.Kind = %q, want %q", got.Kind, ontology.KindSchemaRegistered)
	}
	if got.Version != ontology.VersionSchemaRegistered {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionSchemaRegistered)
	}

	original := ontology.SchemaRegisteredEvent{
		SchemaKind: "entityKind:vehicle",
		Version:    "v0.1.0",
		Definition: json.RawMessage(`{"type":"object","properties":{"vin":{"type":"string"}}}`),
		Provenance: "user",
		AddedBy:    ontology.EntityKindAddedByUser,
		AddedAt:    time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindSchemaRegistered, payload); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rt ontology.SchemaRegisteredEvent
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

// TestSchemaRegistered_Register_Idempotent verifies that registering the
// identical schema twice is a no-op per the kernel.Ontology contract.
func TestSchemaRegistered_Register_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterSchemaRegistered(ctx, o); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := ontology.RegisterSchemaRegistered(ctx, o); err != nil {
		t.Errorf("idempotent second register: got %v, want nil", err)
	}
}

// TestSchemaRegistered_Validate_MissingRequiredField table-drives every
// required field: drop it and assert validation fails.
func TestSchemaRegistered_Validate_MissingRequiredField(t *testing.T) {
	t.Parallel()
	complete := map[string]any{
		"schemaKind": "entityKind:vehicle",
		"version":    "v0.1.0",
		"definition": map[string]any{"type": "object"},
		"provenance": "user",
		"addedBy":    string(ontology.EntityKindAddedByUser),
		"addedAt":    "2026-06-05T10:00:00Z",
	}
	for _, field := range []string{"schemaKind", "version", "definition", "provenance", "addedBy", "addedAt"} {
		t.Run("missing "+field, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := kernel.NewMemoryOntology()
			if err := ontology.RegisterSchemaRegistered(ctx, o); err != nil {
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
			if err := o.Validate(ctx, ontology.KindSchemaRegistered, payload); err == nil {
				t.Errorf("expected validation error for missing %q, got nil", field)
			}
		})
	}
}

// TestSchemaRegistered_Validate_RejectsBadVersion asserts the version pattern
// rejects non-semver values.
func TestSchemaRegistered_Validate_RejectsBadVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterSchemaRegistered(ctx, o); err != nil {
		t.Fatalf("register: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"schemaKind": "entityKind:vehicle",
		"version":    "0.1",
		"definition": map[string]any{"type": "object"},
		"provenance": "user",
		"addedBy":    string(ontology.EntityKindAddedByUser),
		"addedAt":    "2026-06-05T10:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := o.Validate(ctx, ontology.KindSchemaRegistered, payload); err == nil {
		t.Error("expected validation error for non-semver version, got nil")
	}
}

// TestSchemaRegistered_Validate_Versions asserts the version pattern accepts
// well-formed semver and rejects leading-zero components, matching
// golang.org/x/mod/semver.IsValid which the registry applies later.
func TestSchemaRegistered_Validate_Versions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		version string
		ok      bool
	}{
		{"v0.1.0", true},
		{"v1.0.0", true},
		{"v10.20.30", true},
		{"v01.0.0", false}, // leading zero in major
		{"v0.01.0", false}, // leading zero in minor
		{"v0.1.00", false}, // leading zero in patch
		{"0.1.0", false},   // missing v prefix
		{"v0.1", false},    // not a full triplet
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := kernel.NewMemoryOntology()
			if err := ontology.RegisterSchemaRegistered(ctx, o); err != nil {
				t.Fatalf("register: %v", err)
			}
			payload, err := json.Marshal(map[string]any{
				"schemaKind": "entityKind:vehicle",
				"version":    tc.version,
				"definition": map[string]any{"type": "object"},
				"provenance": "user",
				"addedBy":    string(ontology.EntityKindAddedByUser),
				"addedAt":    "2026-06-05T10:00:00Z",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = o.Validate(ctx, ontology.KindSchemaRegistered, payload)
			if tc.ok && err != nil {
				t.Errorf("version %q: expected valid, got %v", tc.version, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("version %q: expected invalid, got nil", tc.version)
			}
		})
	}
}

// TestSchemaRegistered_Validate_SchemaKinds asserts the schemaKind pattern
// accepts bare camelCase event kinds and colon-namespaced keys, rejects keys
// with spaces or path separators that would corrupt the registry lookup key
// and the rendered filename, and caps the key length (maxLength=128) so a
// durable event cannot carry a key that no filesystem could materialize.
func TestSchemaRegistered_Validate_SchemaKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		schemaKind string
		ok         bool
	}{
		{"bare camelCase", "myCustomEvent", true},
		{"namespaced", "entityKind:vehicle", true},
		{"deep namespaced", "entityKind:user:deal", true},
		{"space rejected", "has space", false},
		{"slash rejected", "a/b", false},
		{"uppercase start rejected", "Vehicle", false},
		{"empty tail rejected", "entityKind:", false},
		{"max length accepted", "entityKind:" + strings.Repeat("a", 117), true},       // 128 chars
		{"over max length rejected", "entityKind:" + strings.Repeat("a", 118), false}, // 129 chars
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := kernel.NewMemoryOntology()
			if err := ontology.RegisterSchemaRegistered(ctx, o); err != nil {
				t.Fatalf("register: %v", err)
			}
			payload, err := json.Marshal(map[string]any{
				"schemaKind": tc.schemaKind,
				"version":    "v0.1.0",
				"definition": map[string]any{"type": "object"},
				"provenance": "user",
				"addedBy":    string(ontology.EntityKindAddedByUser),
				"addedAt":    "2026-06-05T10:00:00Z",
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			err = o.Validate(ctx, ontology.KindSchemaRegistered, payload)
			if tc.ok && err != nil {
				t.Errorf("schemaKind %q: expected valid, got %v", tc.schemaKind, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("schemaKind %q: expected invalid, got nil", tc.schemaKind)
			}
		})
	}
}

// TestSchemaRegistered_RenderFilename asserts the frozen render-path contract:
// every schemaKind the pattern accepts maps to a render filename with no path
// separators and no characters illegal on Windows (notably the namespace
// colon), carries a lowercased readable stem followed by the full SHA-256 hex
// digest, is all-lowercase, length-bounded, and deterministic, so a registered
// schema is always materializable and sync-safe.
func TestSchemaRegistered_RenderFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		schemaKind string
		wantStem   string // lowercased, hyphen-mapped readable stem
	}{
		{"myCustomEvent", "mycustomevent"},
		{"entityKind:vehicle", "entitykind-vehicle"},
		{"entityKind:user:deal", "entitykind-user-deal"},
		{"entityKind:user__deal", "entitykind-user__deal"}, // underscores in a segment survive
	}
	for _, tc := range cases {
		t.Run(tc.schemaKind, func(t *testing.T) {
			t.Parallel()
			got := ontology.SchemaKindRenderFilename(tc.schemaKind)
			wantPrefix := tc.wantStem + "-"
			if !strings.HasPrefix(got, wantPrefix) || !strings.HasSuffix(got, ".json") {
				t.Errorf("SchemaKindRenderFilename(%q) = %q, want %q<64 hex>.json", tc.schemaKind, got, wantPrefix)
			}
			// The whole filename must be lowercase so it cannot fold to a
			// different name on a case-insensitive filesystem.
			if got != strings.ToLower(got) {
				t.Errorf("filename %q is not all-lowercase (would fold on case-insensitive filesystems)", got)
			}
			if strings.ContainsAny(got, `:/\`) {
				t.Errorf("filename %q contains a path separator or colon (not Windows-safe)", got)
			}
			if got != ontology.SchemaKindRenderFilename(tc.schemaKind) {
				t.Errorf("filename for %q is not deterministic", tc.schemaKind)
			}
		})
	}

	// Pin the documented example so the doc comment stays truthful and the
	// encoding (lowercased stem + full SHA-256 hex digest) does not drift.
	if got, want := ontology.SchemaKindRenderFilename("entityKind:vehicle"),
		"entitykind-vehicle-1c33562edd542f87b0cff715ff214f3a61de82d2d1f9754b5dd1aec0d486a7a8.json"; got != want {
		t.Errorf("documented example: SchemaKindRenderFilename(%q) = %q, want %q", "entityKind:vehicle", got, want)
	}
}

// TestSchemaRegistered_RenderFilename_LengthBounded asserts the render filename
// stays under the 255-byte single-path-component limit common to ext4, APFS,
// and NTFS, even for the longest key the maxLength=128 contract admits. The
// digest is constant-width and the stem is capped, so the basename is bounded.
func TestSchemaRegistered_RenderFilename_LengthBounded(t *testing.T) {
	t.Parallel()
	// Longest grammar-valid, maxLength-valid key: "entityKind:" + 117 'a' = 128.
	longKey := "entityKind:" + strings.Repeat("a", 117)
	if len(longKey) != 128 {
		t.Fatalf("test setup: longKey is %d chars, want 128", len(longKey))
	}
	got := ontology.SchemaKindRenderFilename(longKey)
	if len(got) > 255 {
		t.Errorf("filename for max-length key is %d bytes, want <= 255: %q", len(got), got)
	}
}

// TestSchemaRegistered_RenderFilename_Injective asserts the mapping is provably
// collision-free on every filesystem, including case-insensitive defaults
// (Windows, macOS APFS). It folds every rendered name to lowercase — exactly
// how a case-insensitive filesystem compares paths — and asserts no two keys
// collide. The key set includes the separator-vs-underscore pair
// (entityKind:user:deal vs entityKind:user__deal), a case-only variant
// (entityKind:vehicle vs entityKind:Vehicle), and the adversarial pair the
// reviewer constructed for the earlier truncated-hash design (which shared an
// 8-hex prefix and case-folded stems) to prove the full hex identity defeats
// it.
func TestSchemaRegistered_RenderFilename_Injective(t *testing.T) {
	t.Parallel()
	keys := []string{
		"entityKind:user:deal",
		"entityKind:user__deal",
		"entityKind:user_deal",
		"entityKind:vehicle",
		"entityKind:Vehicle", // case-only variant
		"entityKind:user:deal:eu",
		"myCustomEvent",
		// Reviewer's adversarial pair: case-only-variant stems that collided
		// under the previous truncated-hash scheme (shared prefix 9e8f209c).
		"eNTityKInd:aaaAaAaAaaaaaAaAaAaaAaAAAAAaaaaAAaaAAaaaAaAAA",
		"enTitYKiND:AAAAaAAaaAaaAAaaaaAAAaAaAAAaaAaaAAAAAAAAAAaaa",
	}
	seen := make(map[string]string, len(keys))
	for _, k := range keys {
		// Fold to lowercase to model a case-insensitive filesystem.
		folded := strings.ToLower(ontology.SchemaKindRenderFilename(k))
		if prev, dup := seen[folded]; dup {
			t.Errorf("case-insensitive collision: %q and %q both render to %q", prev, k, folded)
		}
		seen[folded] = k
	}
}

// TestSchemaRegistered_Schema_Cached verifies the schema cache is stable
// across calls.
func TestSchemaRegistered_Schema_Cached(t *testing.T) {
	t.Parallel()
	first := ontology.SchemaRegisteredSchema()
	second := ontology.SchemaRegisteredSchema()
	if !bytes.Equal(first.Definition, second.Definition) {
		t.Errorf("SchemaRegisteredSchema returned different Definitions across calls")
	}
}
