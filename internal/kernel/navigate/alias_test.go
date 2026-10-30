package navigate_test

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// seedAlias inserts one row into entity_aliases for the given entity.
func seedAlias(exec func(string, ...any), alias, entityUID string) {
	exec(
		`INSERT INTO entity_aliases (alias, entity_uid, scope, create_time) VALUES (?,?,?,?)`,
		alias, entityUID, "personal", time.Now().UTC().Format(time.RFC3339Nano),
	)
}

// TestResolveTarget_AliasLane verifies that an entity is resolved via its
// alternate name when the exact display-name lane finds nothing, using no
// semantic embedder. The test seeds one entity whose display name is
// "Samwise Gamgee" but also carries the alias "Sam"; resolving "Sam" must
// find it deterministically.
func TestResolveTarget_AliasLane(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	seedEntity(exec, "p-sam", "person", "Samwise Gamgee")
	seedAlias(exec, "Sam", "p-sam")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	ctx := context.Background()

	t.Run("exact display name resolves directly", func(t *testing.T) {
		t.Parallel()
		result := nav.ResolveDisplayName(ctx, "Samwise Gamgee", navigate.ResolveStrict, "")
		if result.Status != navigate.ResolvedOK {
			t.Fatalf("status = %v, want ResolvedOK", result.Status)
		}
		if result.UID != "p-sam" {
			t.Fatalf("uid = %q, want p-sam", result.UID)
		}
	})

	t.Run("alias resolves when display name misses", func(t *testing.T) {
		t.Parallel()
		result := nav.ResolveDisplayName(ctx, "Sam", navigate.ResolveStrict, "")
		if result.Status != navigate.ResolvedOK {
			t.Fatalf("status = %v, want ResolvedOK (alias lane should find p-sam)", result.Status)
		}
		if result.UID != "p-sam" {
			t.Fatalf("uid = %q, want p-sam", result.UID)
		}
	})

	t.Run("unknown name is not found after both lanes", func(t *testing.T) {
		t.Parallel()
		result := nav.ResolveDisplayName(ctx, "Gandalf", navigate.ResolveStrict, "")
		if result.Status != navigate.ResolvedNotFound {
			t.Fatalf("status = %v, want ResolvedNotFound", result.Status)
		}
	})
}

// TestResolveTarget_AliasLane_AmbiguousMultiple verifies that when two
// entities share the same alias, the alias lane returns ResolvedAmbiguous
// rather than arbitrarily picking one, matching the same-display-name
// ambiguity contract.
func TestResolveTarget_AliasLane_AmbiguousMultiple(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	seedEntity(exec, "p-sam1", "person", "Samwise Gamgee")
	seedEntity(exec, "p-sam2", "person", "Sam the Brave")
	seedAlias(exec, "Sam", "p-sam1")
	seedAlias(exec, "Sam", "p-sam2")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	ctx := context.Background()

	result := nav.ResolveDisplayName(ctx, "Sam", navigate.ResolveStrict, "")
	if result.Status != navigate.ResolvedAmbiguous {
		t.Fatalf("status = %v, want ResolvedAmbiguous (two entities share alias)", result.Status)
	}
	if len(result.Candidates) < 2 {
		t.Fatalf("candidates = %d, want >= 2", len(result.Candidates))
	}
}

// TestResolveTarget_AliasLane_ExactWins verifies that when an entity's
// display name exactly matches the query the alias lane is not consulted:
// the exact lane returns the display-name winner.
func TestResolveTarget_AliasLane_ExactWins(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	// "Sam" is both a display name (for org-uid) and an alias (for p-sam).
	seedEntity(exec, "org-uid", "organization", "Sam")
	seedEntity(exec, "p-sam", "person", "Samwise Gamgee")
	seedAlias(exec, "Sam", "p-sam")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	ctx := context.Background()

	result := nav.ResolveDisplayName(ctx, "Sam", navigate.ResolveStrict, "")
	// Exact display-name lane finds "org-uid" (organization named "Sam").
	// The alias lane should not be reached.
	if result.Status != navigate.ResolvedOK {
		t.Fatalf("status = %v, want ResolvedOK (exact display name wins)", result.Status)
	}
	if result.UID != "org-uid" {
		t.Fatalf("uid = %q, want org-uid (exact display name takes precedence over alias)", result.UID)
	}
}

// TestAliasProjection_CompositeKeyIdempotency verifies the entity_aliases
// projection's replay-idempotency and multi-entity semantics:
//
//  1. Inserting the same (alias, entity_uid) pair twice yields exactly one row.
//  2. Two different entities sharing the same alias each get their own row (two
//     rows total, no constraint violation).
func TestAliasProjection_CompositeKeyIdempotency(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	seedEntity(exec, "e1", "person", "Entity One")
	seedEntity(exec, "e2", "person", "Entity Two")

	insertAlias := func(alias, uid string) {
		exec(
			`INSERT INTO entity_aliases (alias, entity_uid, scope, create_time)
			 VALUES (?,?,?,?)
			 ON CONFLICT(alias, entity_uid) DO NOTHING`,
			alias, uid, "personal", now,
		)
	}

	// Insert the same (alias, entity_uid) pair twice: idempotent replay simulation.
	insertAlias("Buddy", "e1")
	insertAlias("Buddy", "e1")

	// Insert the same alias for a second entity: must succeed (composite key,
	// not a unique alias constraint).
	insertAlias("Buddy", "e2")

	// Verify exactly two rows exist: one per (alias, entity_uid) pair.
	ctx := context.Background()
	var count int
	if err := store.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_aliases WHERE alias = ?`, "Buddy",
	).Scan(&count); err != nil {
		t.Fatalf("count aliases: %v", err)
	}
	if count != 2 {
		t.Fatalf("alias row count = %d, want 2 (one per entity, idempotent on replay)", count)
	}

	// Confirm each entity has exactly one row.
	for _, uid := range []string{"e1", "e2"} {
		var n int
		if err := store.Reader().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entity_aliases WHERE alias = ? AND entity_uid = ?`, "Buddy", uid,
		).Scan(&n); err != nil {
			t.Fatalf("count for %s: %v", uid, err)
		}
		if n != 1 {
			t.Fatalf("alias rows for entity %s = %d, want 1", uid, n)
		}
	}
}
