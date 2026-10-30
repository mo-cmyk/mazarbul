package resolver_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// openTestStore opens a fresh SQLite store (with all migrations applied) in a
// temporary directory and registers a cleanup. It returns the store so callers
// can access Writer() and Reader() pools.
func openTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestRegistry_LoadsSeeds verifies that NewRegistry loads all ten seed
// predicates from the predicates table and that every seed is functional.
func TestRegistry_LoadsSeeds(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := t.Context()

	reg, err := resolver.NewRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	seeds := []string{
		"works_at", "lives_in", "born_in", "child_of", "parent_of",
		"married_to", "member_of", "located_in", "has_role", "owns",
	}
	for _, name := range seeds {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kind, ok := reg.Kind(name)
			if !ok {
				t.Errorf("Kind(%q): not found in registry", name)
				return
			}
			if kind != resolver.Functional {
				t.Errorf("Kind(%q) = %q, want %q", name, kind, resolver.Functional)
			}
		})
	}
}

// TestRegistry_KindMiss verifies that Kind returns ("", false) for an unknown
// predicate and does not panic.
func TestRegistry_KindMiss(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)

	reg, err := resolver.NewRegistry(t.Context(), store.Reader())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	kind, ok := reg.Kind("unknown_predicate_xyz")
	if ok {
		t.Errorf("Kind(unknown): ok = true, want false")
	}
	if kind != "" {
		t.Errorf("Kind(unknown): kind = %q, want empty string", kind)
	}
}

// TestRegistry_IsFunctional_UnknownReturnsFalse verifies the conservative
// default: unknown predicates are treated as non-functional to avoid
// accidental auto-supersede.
func TestRegistry_IsFunctional_UnknownReturnsFalse(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)

	reg, err := resolver.NewRegistry(t.Context(), store.Reader())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if reg.IsFunctional("totally_unknown") {
		t.Error("IsFunctional(unknown) = true, want false")
	}
}

// TestRegistry_Register persists a new predicate and updates the in-memory
// cache. A second NewRegistry on the same DB must see the persisted row.
func TestRegistry_Register(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := t.Context()

	reg, err := resolver.NewRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	p := resolver.Predicate{
		Name:        "collaborated_with",
		Kind:        resolver.NonFunctional,
		Description: "Subject has collaborated with the object person.",
		AddedAt:     time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		AddedBy:     "user",
	}

	tx, err := store.Writer().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := reg.Register(ctx, tx, p); err != nil {
		_ = tx.Rollback()
		t.Fatalf("Register: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Cache must be updated immediately.
	kind, ok := reg.Kind("collaborated_with")
	if !ok {
		t.Fatal("Kind(collaborated_with) not found after Register")
	}
	if kind != resolver.NonFunctional {
		t.Errorf("Kind = %q, want %q", kind, resolver.NonFunctional)
	}
	if reg.IsFunctional("collaborated_with") {
		t.Error("IsFunctional(collaborated_with) = true, want false (non-functional)")
	}

	// Open a fresh registry on the same DB to confirm the row was persisted.
	reg2, err := resolver.NewRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewRegistry (second): %v", err)
	}
	kind2, ok2 := reg2.Kind("collaborated_with")
	if !ok2 {
		t.Fatal("Kind(collaborated_with) not found in fresh registry after persist")
	}
	if kind2 != resolver.NonFunctional {
		t.Errorf("persisted kind = %q, want %q", kind2, resolver.NonFunctional)
	}
}

// TestRegistry_Register_Idempotent verifies that calling Register twice with
// the same (predicate, kind) pair succeeds without error and without adding a
// duplicate row.
func TestRegistry_Register_Idempotent(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := t.Context()

	reg, err := resolver.NewRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	p := resolver.Predicate{
		Name:    "mentors",
		Kind:    resolver.NonFunctional,
		AddedAt: time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		AddedBy: "llm",
	}

	for i := range 2 {
		tx, err := store.Writer().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx (call %d): %v", i, err)
		}
		if err := reg.Register(ctx, tx, p); err != nil {
			_ = tx.Rollback()
			t.Fatalf("Register (call %d): %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit (call %d): %v", i, err)
		}
	}

	// Exactly one row for "mentors" in the DB.
	var count int
	if err := store.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM predicates WHERE predicate = 'mentors'`,
	).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("predicate row count = %d, want 1", count)
	}
}
