package kernel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// seedKindWithStatus inserts a row into entity_kinds with the given status
// (bypassing the event log to keep tests fast and focused).
func seedKindWithStatus(t *testing.T, exec func(string, ...any), name, displayName, description, provenance, addedBy, status string) {
	t.Helper()
	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	exec(`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status)
	      VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, displayName, description, provenance, addedBy, now, status)
}

// TestSQLiteOntology_ValidateEntityKind_ExcludesProposed verifies that a
// proposed kind is NOT considered active: ValidateEntityKind must return
// ErrEntityKindNotRegistered for it.
func TestSQLiteOntology_ValidateEntityKind_ExcludesProposed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	seedKindWithStatus(t, exec, "user:gadget", "Gadget", "A small device.", "user", "llm", "proposed")

	err := ont.ValidateEntityKind(ctx, "user:gadget")
	if !errors.Is(err, kernel.ErrEntityKindNotRegistered) {
		t.Errorf("proposed kind: want ErrEntityKindNotRegistered, got %v", err)
	}
}

// TestSQLiteOntology_ValidateEntityKind_ExcludesRejected verifies that a
// rejected kind is NOT considered active.
func TestSQLiteOntology_ValidateEntityKind_ExcludesRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	seedKindWithStatus(t, exec, "user:thingy", "Thingy", "A vague placeholder.", "user", "llm", "rejected")

	err := ont.ValidateEntityKind(ctx, "user:thingy")
	if !errors.Is(err, kernel.ErrEntityKindNotRegistered) {
		t.Errorf("rejected kind: want ErrEntityKindNotRegistered, got %v", err)
	}
}

// TestSQLiteOntology_ValidateEntityKind_IncludesActive verifies that an active
// kind passes ValidateEntityKind.
func TestSQLiteOntology_ValidateEntityKind_IncludesActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	seedKindWithStatus(t, exec, "user:vehicle", "Vehicle", "A motorised conveyance.", "user", "user", "active")

	if err := ont.ValidateEntityKind(ctx, "user:vehicle"); err != nil {
		t.Errorf("active kind: want nil, got %v", err)
	}
}

// TestSQLiteOntology_ProposedEntityKinds_ReturnsThem verifies that
// ProposedEntityKinds returns only rows with status='proposed', not active or
// rejected ones.
func TestSQLiteOntology_ProposedEntityKinds_ReturnsThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	seedKindWithStatus(t, exec, "user:proposed_kind", "Proposed Kind", "Awaiting review.", "user", "llm", "proposed")
	seedKindWithStatus(t, exec, "user:active_kind", "Active Kind", "Already approved.", "user", "user", "active")
	seedKindWithStatus(t, exec, "user:rejected_kind", "Rejected Kind", "Was rejected.", "user", "llm", "rejected")

	kinds, err := ont.ProposedEntityKinds(ctx)
	if err != nil {
		t.Fatalf("ProposedEntityKinds: %v", err)
	}

	// Only the proposed kind must appear.
	if len(kinds) != 1 {
		t.Fatalf("ProposedEntityKinds returned %d kinds, want 1: %+v", len(kinds), kinds)
	}
	if kinds[0].Name != "user:proposed_kind" {
		t.Errorf("ProposedEntityKinds[0].Name = %q, want user:proposed_kind", kinds[0].Name)
	}
	if kinds[0].Status != "proposed" {
		t.Errorf("ProposedEntityKinds[0].Status = %q, want proposed", kinds[0].Status)
	}
}

// TestSQLiteOntology_ProposedEntityKinds_EmptyWhenNone verifies that
// ProposedEntityKinds returns an empty (non-nil) slice when no proposed kinds
// exist.
func TestSQLiteOntology_ProposedEntityKinds_EmptyWhenNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	seedKindWithStatus(t, exec, "user:vehicle", "Vehicle", "A road vehicle.", "user", "user", "active")

	kinds, err := ont.ProposedEntityKinds(ctx)
	if err != nil {
		t.Fatalf("ProposedEntityKinds: %v", err)
	}
	if len(kinds) != 0 {
		t.Errorf("ProposedEntityKinds = %v, want empty slice", kinds)
	}
}

// TestSQLiteOntology_ValidateEntityKind_StaleCache verifies that
// ValidateEntityKind reflects the current SQLite status even after a prior
// successful validation. Specifically: if a kind starts as active, passes
// validation, then has its status changed to "rejected" in the database
// (simulating an entityKindReviewed reject event being dispatched), a
// subsequent ValidateEntityKind call must return ErrEntityKindNotRegistered —
// not a stale cached "active" result. This is the Issue-1 fix regression test.
func TestSQLiteOntology_ValidateEntityKind_StaleCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	// Seed an active kind.
	seedKindWithStatus(t, exec, "user:widget", "Widget", "A small reusable component.", "user", "user", "active")

	// First validation: must pass (kind is active).
	if err := ont.ValidateEntityKind(ctx, "user:widget"); err != nil {
		t.Fatalf("first ValidateEntityKind (active): want nil, got %v", err)
	}

	// Simulate the dispatcher applying an entityKindReviewed reject event by
	// directly updating the status column. In production the dispatcher writes
	// this via UPSERT after processing the event.
	exec(`UPDATE entity_kinds SET status = 'rejected' WHERE name = ?`, "user:widget")

	// Second validation: must reflect the rejection, NOT a cached active result.
	err := ont.ValidateEntityKind(ctx, "user:widget")
	if !errors.Is(err, kernel.ErrEntityKindNotRegistered) {
		t.Errorf("re-validate after reject: want ErrEntityKindNotRegistered, got %v", err)
	}
}

// TestSQLiteOntology_ProposedEntityKinds_MultipleResults verifies that when
// multiple proposed kinds exist all are returned.
func TestSQLiteOntology_ProposedEntityKinds_MultipleResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ont, exec := openTestSQLiteOntology(t)

	for _, name := range []string{"user:alpha", "user:beta", "user:gamma"} {
		seedKindWithStatus(t, exec, name, name, "A test kind.", "user", "llm", "proposed")
	}
	// Active and rejected must be excluded.
	seedKindWithStatus(t, exec, "user:active", "Active", "Active kind.", "user", "user", "active")
	seedKindWithStatus(t, exec, "user:rejected", "Rejected", "Rejected kind.", "user", "llm", "rejected")

	kinds, err := ont.ProposedEntityKinds(ctx)
	if err != nil {
		t.Fatalf("ProposedEntityKinds: %v", err)
	}
	if len(kinds) != 3 {
		t.Fatalf("ProposedEntityKinds returned %d kinds, want 3: %+v", len(kinds), kinds)
	}
	// All returned kinds must carry status="proposed".
	for _, k := range kinds {
		if k.Status != "proposed" {
			t.Errorf("kind %q has Status=%q, want proposed", k.Name, k.Status)
		}
	}
}
