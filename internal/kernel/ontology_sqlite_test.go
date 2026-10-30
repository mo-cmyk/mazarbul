package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// openTestSQLiteOntology opens a fresh migrated SQLite store and wraps it in
// a SQLiteOntology. Returns the ontology and a raw exec helper for seeding.
func openTestSQLiteOntology(t *testing.T) (*kernel.SQLiteOntology, func(q string, args ...any)) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ont, err := kernel.NewSQLiteOntology(store.Reader())
	if err != nil {
		t.Fatalf("NewSQLiteOntology: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	return ont, exec
}

// TestSQLiteOntology_CoreOverlay verifies that schemas registered via Register
// (the core boot-time path) are returned by Schema, Validate, and List without
// any database read.
func TestSQLiteOntology_CoreOverlay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, _ := openTestSQLiteOntology(t)

	schema := kernel.Schema{
		Kind:       "testKind",
		Version:    "v0.1.0",
		Definition: []byte(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
		Provenance: "core",
	}
	if err := ont.Register(ctx, schema); err != nil {
		t.Fatalf("Register: %v", err)
	}

	t.Run("Schema hit", func(t *testing.T) {
		t.Parallel()
		got, err := ont.Schema(ctx, "testKind")
		if err != nil {
			t.Fatalf("Schema: %v", err)
		}
		if got.Kind != "testKind" {
			t.Errorf("kind = %q, want testKind", got.Kind)
		}
	})

	t.Run("Validate valid payload", func(t *testing.T) {
		t.Parallel()
		if err := ont.Validate(ctx, "testKind", []byte(`{"name":"Merry"}`)); err != nil {
			t.Errorf("Validate valid: %v", err)
		}
	})

	t.Run("Validate invalid payload", func(t *testing.T) {
		t.Parallel()
		if err := ont.Validate(ctx, "testKind", []byte(`{}`)); err == nil {
			t.Error("Validate missing required field: want error, got nil")
		}
	})

	t.Run("List includes core schema", func(t *testing.T) {
		t.Parallel()
		schemas, err := ont.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		found := false
		for _, s := range schemas {
			if s.Kind == "testKind" {
				found = true
			}
		}
		if !found {
			t.Error("List: testKind not found in results")
		}
	})
}

// TestSQLiteOntology_DynamicReadThrough verifies that a schema inserted
// directly into ontology_schemas is visible via Schema, Validate, and List.
func TestSQLiteOntology_DynamicReadThrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	exec(`INSERT INTO ontology_schemas (schema_kind, version, definition, provenance, added_by, added_at)
	      VALUES (?, ?, ?, ?, ?, ?)`,
		"dynamicKind", "v0.2.0",
		`{"type":"object","properties":{"score":{"type":"number"}},"required":["score"]}`,
		"user", "user", time.Now().UTC().Format(time.RFC3339Nano),
	)

	t.Run("Schema found", func(t *testing.T) {
		t.Parallel()
		s, err := ont.Schema(ctx, "dynamicKind")
		if err != nil {
			t.Fatalf("Schema: %v", err)
		}
		if s.Version != "v0.2.0" {
			t.Errorf("version = %q, want v0.2.0", s.Version)
		}
	})

	t.Run("Schema miss returns ErrNotFound", func(t *testing.T) {
		t.Parallel()
		_, err := ont.Schema(ctx, "notHere")
		if !errors.Is(err, kernel.ErrNotFound) {
			t.Errorf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("Validate valid", func(t *testing.T) {
		t.Parallel()
		if err := ont.Validate(ctx, "dynamicKind", []byte(`{"score":9.5}`)); err != nil {
			t.Errorf("Validate valid: %v", err)
		}
	})

	t.Run("Validate invalid", func(t *testing.T) {
		t.Parallel()
		if err := ont.Validate(ctx, "dynamicKind", []byte(`{"score":"bad"}`)); err == nil {
			t.Error("Validate wrong type: want error, got nil")
		}
	})

	t.Run("List includes dynamic schema", func(t *testing.T) {
		t.Parallel()
		schemas, err := ont.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		found := false
		for _, s := range schemas {
			if s.Kind == "dynamicKind" {
				found = true
			}
		}
		if !found {
			t.Error("List: dynamicKind not found in results")
		}
	})
}

// TestSQLiteOntology_ListSortOrder verifies that List returns schemas sorted
// by (Kind, Version) deterministically.
func TestSQLiteOntology_ListSortOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec(`INSERT INTO ontology_schemas (schema_kind, version, definition, provenance, added_by, added_at) VALUES (?,?,?,?,?,?)`,
		"zzz", "v0.1.0", `{"type":"object"}`, "user", "user", now)
	exec(`INSERT INTO ontology_schemas (schema_kind, version, definition, provenance, added_by, added_at) VALUES (?,?,?,?,?,?)`,
		"aaa", "v0.1.0", `{"type":"object"}`, "user", "user", now)

	schemas, err := ont.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := 1; i < len(schemas); i++ {
		if schemas[i-1].Kind > schemas[i].Kind {
			t.Errorf("List not sorted: %q > %q at index %d", schemas[i-1].Kind, schemas[i].Kind, i)
		}
	}
}

// TestSQLiteOntology_ValidateEntityKind_HitAndMiss verifies that
// ValidateEntityKind returns nil for a known kind and ErrEntityKindNotRegistered
// for an unknown one.
func TestSQLiteOntology_ValidateEntityKind_HitAndMiss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec(`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at)
	      VALUES (?, ?, ?, ?, ?, ?)`,
		"person", "Person", "A human individual.", "core", "seed", now)

	if err := ont.ValidateEntityKind(ctx, "person"); err != nil {
		t.Errorf("ValidateEntityKind(person): %v, want nil", err)
	}
	if err := ont.ValidateEntityKind(ctx, "alien"); !errors.Is(err, kernel.ErrEntityKindNotRegistered) {
		t.Errorf("ValidateEntityKind(alien): want ErrEntityKindNotRegistered, got %v", err)
	}
}

// TestSQLiteOntology_ValidateEntityKind_MissNotCached verifies that a failed
// lookup is NOT negatively cached: a kind registered after a miss is picked up
// on the next query rather than being rejected until process restart.
func TestSQLiteOntology_ValidateEntityKind_MissNotCached(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	// First lookup misses (kind not yet registered).
	if err := ont.ValidateEntityKind(ctx, "vehicle"); !errors.Is(err, kernel.ErrEntityKindNotRegistered) {
		t.Fatalf("first lookup: want ErrEntityKindNotRegistered, got %v", err)
	}

	// Register the kind (simulating a later event/pull applied to the
	// projection table).
	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec(`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at)
	      VALUES (?, ?, ?, ?, ?, ?)`,
		"vehicle", "Vehicle", "A motorised conveyance.", "user", "user", now)

	// The next lookup must succeed; a stuck negative cache would still reject.
	if err := ont.ValidateEntityKind(ctx, "vehicle"); err != nil {
		t.Errorf("post-registration lookup: %v, want nil (miss must not be negatively cached)", err)
	}

	// EntityKindDef must likewise pick up the new row after an earlier miss.
	ont2, exec2 := openTestSQLiteOntology(t)
	if _, err := ont2.EntityKindDef(ctx, "deal"); !errors.Is(err, kernel.ErrNotFound) {
		t.Fatalf("EntityKindDef first lookup: want ErrNotFound, got %v", err)
	}
	exec2(`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at)
	       VALUES (?, ?, ?, ?, ?, ?)`,
		"deal", "Deal", "A sales opportunity.", "user", "user", now)
	if _, err := ont2.EntityKindDef(ctx, "deal"); err != nil {
		t.Errorf("EntityKindDef post-registration: %v, want success (miss must not be negatively cached)", err)
	}
}

// TestSQLiteOntology_EntityKindDef_RoundTrip verifies that EntityKindDef
// returns the correct fields including sameAs unmarshalling.
func TestSQLiteOntology_EntityKindDef_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	addedAt := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	exec(`INSERT INTO entity_kinds (name, display_name, description, same_as, provenance, added_by, added_at)
	      VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"organization", "Organization", "A collective entity.",
		`["https://schema.org/Organization"]`,
		"core", "seed", addedAt.Format(time.RFC3339Nano))

	def, err := ont.EntityKindDef(ctx, "organization")
	if err != nil {
		t.Fatalf("EntityKindDef: %v", err)
	}
	if def.Name != "organization" {
		t.Errorf("name = %q, want organization", def.Name)
	}
	if def.DisplayName != "Organization" {
		t.Errorf("displayName = %q, want Organization", def.DisplayName)
	}
	if len(def.SameAs) != 1 || def.SameAs[0] != "https://schema.org/Organization" {
		t.Errorf("sameAs = %v, want [https://schema.org/Organization]", def.SameAs)
	}
	if !def.AddedAt.Equal(addedAt) {
		t.Errorf("addedAt = %v, want %v", def.AddedAt, addedAt)
	}

	// Miss returns ErrNotFound.
	_, err = ont.EntityKindDef(ctx, "missing")
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("miss: want ErrNotFound, got %v", err)
	}
}

// seedKind is a test helper that inserts one row into entity_kinds.
func seedKind(exec func(q string, args ...any), name, parent, attrSchema string) {
	exec(`INSERT INTO entity_kinds (name, display_name, description, parent, attr_schema, provenance, added_by, added_at)
	      VALUES (?, ?, ?, ?, ?, 'user', 'user', ?)`,
		name, name, name,
		func() any {
			if parent == "" {
				return nil
			}
			return parent
		}(),
		func() any {
			if attrSchema == "" {
				return nil
			}
			return attrSchema
		}(),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
}

// TestSQLiteOntology_ValidateEntityProperties_NoSchema asserts that a kind
// with no attr_schema is a no-op (nil return).
func TestSQLiteOntology_ValidateEntityProperties_NoSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)
	seedKind(exec, "thing", "", "")

	if err := ont.ValidateEntityProperties(ctx, "thing", json.RawMessage(`{"foo":"bar"}`)); err != nil {
		t.Errorf("expected nil for kind with no schema, got: %v", err)
	}
}

// TestSQLiteOntology_ValidateEntityProperties_EmptyProperties asserts that
// empty or nil properties is always accepted regardless of schema.
func TestSQLiteOntology_ValidateEntityProperties_EmptyProperties(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)
	seedKind(exec, "vehicle", "", `{"type":"object","properties":{"vin":{"type":"string"}},"required":["vin"]}`)

	for _, tc := range []struct {
		name  string
		props json.RawMessage
	}{
		{"nil", nil},
		{"empty", json.RawMessage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ont.ValidateEntityProperties(ctx, "vehicle", tc.props); err != nil {
				t.Errorf("expected nil for empty properties, got: %v", err)
			}
		})
	}
}

// TestSQLiteOntology_ValidateEntityProperties_Conforming asserts that
// a conforming properties payload returns nil.
func TestSQLiteOntology_ValidateEntityProperties_Conforming(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)
	seedKind(exec, "vehicle", "", `{"type":"object","properties":{"vin":{"type":"string"}},"required":["vin"]}`)

	if err := ont.ValidateEntityProperties(ctx, "vehicle", json.RawMessage(`{"vin":"1HGCM82633A004352"}`)); err != nil {
		t.Errorf("expected nil for conforming properties, got: %v", err)
	}
}

// TestSQLiteOntology_ValidateEntityProperties_Violation asserts that a
// properties payload missing a required field returns ErrPropertyValidation.
func TestSQLiteOntology_ValidateEntityProperties_Violation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)
	seedKind(exec, "vehicle", "", `{"type":"object","properties":{"vin":{"type":"string"}},"required":["vin"]}`)

	err := ont.ValidateEntityProperties(ctx, "vehicle", json.RawMessage(`{"make":"Toyota"}`))
	if !errors.Is(err, kernel.ErrPropertyValidation) {
		t.Errorf("expected ErrPropertyValidation, got: %v", err)
	}
}

// TestSQLiteOntology_ValidateEntityProperties_AllOfComposition asserts that
// a child kind's schema is composed with the parent's via allOf: both parent
// and child required fields must be present.
func TestSQLiteOntology_ValidateEntityProperties_AllOfComposition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	// Parent "thing" requires "name".
	seedKind(exec, "thing", "", `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	// Child "vehicle" extends "thing" and requires "vin".
	seedKind(exec, "vehicle", "thing", `{"type":"object","properties":{"vin":{"type":"string"}},"required":["vin"]}`)

	cases := []struct {
		desc    string
		props   string
		wantErr bool
	}{
		{"both parent and child fields present", `{"name":"my car","vin":"1HGCM82633A004352"}`, false},
		{"only child vin missing parent name", `{"vin":"1HGCM82633A004352"}`, true},
		{"only parent name missing child vin", `{"name":"my car"}`, true},
		{"neither field present", `{}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			err := ont.ValidateEntityProperties(ctx, "vehicle", json.RawMessage(tc.props))
			if tc.wantErr && !errors.Is(err, kernel.ErrPropertyValidation) {
				t.Errorf("expected ErrPropertyValidation, got: %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected nil, got: %v", err)
			}
		})
	}
}

// TestSQLiteOntology_ValidateEntityProperties_CycleGuard asserts that
// collectAncestorSchemas aborts cleanly when the parent chain exceeds
// the depth limit — it returns a plain error (not ErrPropertyValidation)
// so callers can distinguish a data-integrity issue from a constraint violation.
func TestSQLiteOntology_ValidateEntityProperties_CycleGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	// Build a long linear chain that exceeds maxExtendsDepth (16).
	// Each kind has a simple schema so schemas are collected on every hop.
	schema := `{"type":"object","properties":{"x":{"type":"string"}}}`
	// Insert 18 levels: level0 <- level1 <- ... <- level17
	exec(`INSERT INTO entity_kinds (name, display_name, description, attr_schema, provenance, added_by, added_at)
	      VALUES ('level0','level0','level0',?,'user','user',?)`,
		schema, time.Now().UTC().Format(time.RFC3339Nano))
	for i := 1; i <= 17; i++ {
		exec(`INSERT INTO entity_kinds (name, display_name, description, parent, attr_schema, provenance, added_by, added_at)
		      VALUES (?,?,?,?,?,'user','user',?)`,
			fmt.Sprintf("level%d", i),
			fmt.Sprintf("level%d", i),
			fmt.Sprintf("level%d", i),
			fmt.Sprintf("level%d", i-1),
			schema,
			time.Now().UTC().Format(time.RFC3339Nano),
		)
	}

	err := ont.ValidateEntityProperties(ctx, "level17", json.RawMessage(`{"x":"v"}`))
	if err == nil {
		t.Error("expected error for chain exceeding maxExtendsDepth, got nil")
	}
	// Must NOT be ErrPropertyValidation — this is an infrastructure/depth error.
	if errors.Is(err, kernel.ErrPropertyValidation) {
		t.Errorf("got ErrPropertyValidation for depth-exceeded chain; expected a plain infrastructure error")
	}
}

// TestSQLiteOntology_ValidateEntityProperties_Unattached asserts that
// ValidateEntityProperties is a no-op when the store is not yet attached.
func TestSQLiteOntology_ValidateEntityProperties_Unattached(t *testing.T) {
	t.Parallel()
	ont := kernel.NewSQLiteOntologyUnattached()
	ctx := context.Background()
	if err := ont.ValidateEntityProperties(ctx, "vehicle", json.RawMessage(`{"vin":"X"}`)); err != nil {
		t.Errorf("expected nil when store unattached, got: %v", err)
	}
}
