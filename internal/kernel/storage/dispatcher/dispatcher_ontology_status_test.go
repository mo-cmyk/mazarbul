package dispatcher_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// makeEntityKindReviewedEvent builds a kernel.Event whose payload is a valid
// EntityKindReviewedEvent, ready for Apply.
func makeEntityKindReviewedEvent(t *testing.T, uid string, p ontology.EntityKindReviewedEvent) kernel.Event {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal entity kind reviewed event: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindEntityKindReviewed),
		Source:     "test",
		Confidence: 1.0,
		Payload:    raw,
	}
}

// TestApplyEntityKindRegistered_ProposedStatus verifies that a registration
// event carrying Status="proposed" persists status='proposed' in the
// entity_kinds table.
func TestApplyEntityKindRegistered_ProposedStatus(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	ev := makeEntityKindEvent(t, "01HGOV000000000000STATUS1", ontology.EntityKindRegisteredEvent{
		Name:        "user:gadget",
		DisplayName: "Gadget",
		Description: "A small electronic device.",
		Provenance:  "user",
		AddedBy:     ontology.EntityKindAddedByLLM,
		AddedAt:     now,
		Status:      "proposed",
	})

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(status,'active') FROM entity_kinds WHERE name = 'user:gadget'`,
	).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "proposed" {
		t.Errorf("status = %q, want proposed", status)
	}
}

// TestApplyEntityKindRegistered_DefaultsToActive verifies that a registration
// event with no Status field persists status='active'.
func TestApplyEntityKindRegistered_DefaultsToActive(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	ev := makeEntityKindEvent(t, "01HGOV000000000000ACTIVE1", ontology.EntityKindRegisteredEvent{
		Name:        "user:widget",
		DisplayName: "Widget",
		Description: "A generic user-defined widget.",
		Provenance:  "user",
		AddedBy:     ontology.EntityKindAddedByUser,
		AddedAt:     now,
		// Status intentionally omitted → must default to 'active'.
	})

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(status,'active') FROM entity_kinds WHERE name = 'user:widget'`,
	).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "active" {
		t.Errorf("status = %q, want active", status)
	}
}

// TestApplyEntityKindReviewed_ApprovedBecomesActive verifies that an
// EntityKindReviewedEvent with decision=approved transitions a proposed kind
// to status='active'.
func TestApplyEntityKindReviewed_ApprovedBecomesActive(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)

	// First: register as proposed.
	regEv := makeEntityKindEvent(t, "01HGOV000000000000APPRV1", ontology.EntityKindRegisteredEvent{
		Name:        "user:spaceship",
		DisplayName: "Spaceship",
		Description: "An extraterrestrial vehicle.",
		Provenance:  "user",
		AddedBy:     ontology.EntityKindAddedByLLM,
		AddedAt:     now,
		Status:      "proposed",
	})
	if err := d.Apply(ctx, []kernel.Event{regEv}); err != nil {
		t.Fatalf("Apply registration: %v", err)
	}

	// Then: approve it.
	reviewEv := makeEntityKindReviewedEvent(t, "01HGOV000000000000APPRV2", ontology.EntityKindReviewedEvent{
		Name:       "user:spaceship",
		Decision:   ontology.EntityKindReviewApproved,
		ReviewedBy: "cli",
		ReviewedAt: now,
	})
	if err := d.Apply(ctx, []kernel.Event{reviewEv}); err != nil {
		t.Fatalf("Apply review: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(status,'active') FROM entity_kinds WHERE name = 'user:spaceship'`,
	).Scan(&status); err != nil {
		t.Fatalf("query status after approve: %v", err)
	}
	if status != "active" {
		t.Errorf("status = %q after approve, want active", status)
	}
}

// TestApplyEntityKindReviewed_RejectedStaysRejected verifies that
// decision=rejected transitions a proposed kind to status='rejected'.
func TestApplyEntityKindReviewed_RejectedStaysRejected(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)

	// Register as proposed.
	regEv := makeEntityKindEvent(t, "01HGOV000000000000REJT01", ontology.EntityKindRegisteredEvent{
		Name:        "user:thingy",
		DisplayName: "Thingy",
		Description: "A vague placeholder kind.",
		Provenance:  "user",
		AddedBy:     ontology.EntityKindAddedByLLM,
		AddedAt:     now,
		Status:      "proposed",
	})
	if err := d.Apply(ctx, []kernel.Event{regEv}); err != nil {
		t.Fatalf("Apply registration: %v", err)
	}

	// Reject it.
	reviewEv := makeEntityKindReviewedEvent(t, "01HGOV000000000000REJT02", ontology.EntityKindReviewedEvent{
		Name:       "user:thingy",
		Decision:   ontology.EntityKindReviewRejected,
		ReviewedBy: "cli",
		ReviewedAt: now,
		Note:       "Too vague; merge into document.",
	})
	if err := d.Apply(ctx, []kernel.Event{reviewEv}); err != nil {
		t.Fatalf("Apply review: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(status,'active') FROM entity_kinds WHERE name = 'user:thingy'`,
	).Scan(&status); err != nil {
		t.Fatalf("query status after reject: %v", err)
	}
	if status != "rejected" {
		t.Errorf("status = %q after reject, want rejected", status)
	}
}

// TestApplyEntityKindReviewed_CoreRowGuard verifies that reviewing a core kind
// is a safe no-op: the core row's status remains 'active'.
func TestApplyEntityKindReviewed_CoreRowGuard(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)

	// Seed a core row.
	coreEv := makeEntityKindEvent(t, "01HGOV000000000000COREG1", ontology.EntityKindRegisteredEvent{
		Name:        "animal",
		DisplayName: "Animal",
		Description: "A living creature.",
		Provenance:  "core",
		AddedBy:     ontology.EntityKindAddedBySeed,
		AddedAt:     now,
	})
	if err := d.Apply(ctx, []kernel.Event{coreEv}); err != nil {
		t.Fatalf("Apply core: %v", err)
	}

	// Attempt to reject the core row — must be silently ignored.
	reviewEv := makeEntityKindReviewedEvent(t, "01HGOV000000000000COREG2", ontology.EntityKindReviewedEvent{
		Name:       "animal",
		Decision:   ontology.EntityKindReviewRejected,
		ReviewedBy: "cli",
		ReviewedAt: now,
	})
	if err := d.Apply(ctx, []kernel.Event{reviewEv}); err != nil {
		t.Fatalf("Apply review against core: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(status,'active') FROM entity_kinds WHERE name = 'animal'`,
	).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "active" {
		t.Errorf("core kind status = %q after review, want active (core guard failed)", status)
	}
}

// TestApplyEntityKindReviewed_UnknownNameNoOp verifies that a review event for
// a kind name that does not exist in entity_kinds is a safe no-op and does not
// error (idempotent replay contract).
func TestApplyEntityKindReviewed_UnknownNameNoOp(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)

	reviewEv := makeEntityKindReviewedEvent(t, "01HGOV000000000000NOOPEV", ontology.EntityKindReviewedEvent{
		Name:       "user:nonexistent_kind",
		Decision:   ontology.EntityKindReviewApproved,
		ReviewedBy: "cli",
		ReviewedAt: now,
	})
	// Must not error.
	if err := d.Apply(ctx, []kernel.Event{reviewEv}); err != nil {
		t.Fatalf("Apply review for unknown kind: %v (must be a no-op)", err)
	}
}

// TestApplyEntityKindReviewed_IdempotentReplay verifies that replaying an
// approved review event twice is idempotent: the kind stays active.
func TestApplyEntityKindReviewed_IdempotentReplay(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)

	regEv := makeEntityKindEvent(t, "01HGOV000000000000IDEM01", ontology.EntityKindRegisteredEvent{
		Name:        "user:drone",
		DisplayName: "Drone",
		Description: "An unmanned aerial vehicle.",
		Provenance:  "user",
		AddedBy:     ontology.EntityKindAddedByLLM,
		AddedAt:     now,
		Status:      "proposed",
	})
	if err := d.Apply(ctx, []kernel.Event{regEv}); err != nil {
		t.Fatalf("Apply registration: %v", err)
	}

	reviewEv := makeEntityKindReviewedEvent(t, "01HGOV000000000000IDEM02", ontology.EntityKindReviewedEvent{
		Name:       "user:drone",
		Decision:   ontology.EntityKindReviewApproved,
		ReviewedBy: "cli",
		ReviewedAt: now,
	})

	// Apply twice: idempotent.
	for i := range 2 {
		if err := d.Apply(ctx, []kernel.Event{reviewEv}); err != nil {
			t.Fatalf("Apply review (round %d): %v", i+1, err)
		}
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(status,'active') FROM entity_kinds WHERE name = 'user:drone'`,
	).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "active" {
		t.Errorf("status = %q after idempotent approve replay, want active", status)
	}
}

// TestApplyEntityKindReviewed_NonProposedIsNoOp verifies the review-lifecycle
// invariant: once a kind has left 'proposed', a later entityKindReviewed event
// (e.g. replayed or synced out of order) must not flip its status. An approved
// (active) kind must stay active under a subsequent reject — the event log is the
// replayed source of truth, so the dispatcher, not just the CLI, enforces this.
func TestApplyEntityKindReviewed_NonProposedIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)

	// Register proposed, then approve → active.
	regEv := makeEntityKindEvent(t, "01HGOV000000000000NOOP01", ontology.EntityKindRegisteredEvent{
		Name:        "user:hovercraft",
		DisplayName: "Hovercraft",
		Description: "A vehicle that rides on a cushion of air.",
		Provenance:  "user",
		AddedBy:     ontology.EntityKindAddedByLLM,
		AddedAt:     now,
		Status:      "proposed",
	})
	approveEv := makeEntityKindReviewedEvent(t, "01HGOV000000000000NOOP02", ontology.EntityKindReviewedEvent{
		Name:       "user:hovercraft",
		Decision:   ontology.EntityKindReviewApproved,
		ReviewedBy: "cli",
		ReviewedAt: now,
	})
	if err := d.Apply(ctx, []kernel.Event{regEv, approveEv}); err != nil {
		t.Fatalf("Apply register+approve: %v", err)
	}

	// A later reject on the now-active kind must be a no-op.
	lateReject := makeEntityKindReviewedEvent(t, "01HGOV000000000000NOOP03", ontology.EntityKindReviewedEvent{
		Name:       "user:hovercraft",
		Decision:   ontology.EntityKindReviewRejected,
		ReviewedBy: "cli",
		ReviewedAt: now,
	})
	if err := d.Apply(ctx, []kernel.Event{lateReject}); err != nil {
		t.Fatalf("Apply late reject: %v", err)
	}

	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(status,'active') FROM entity_kinds WHERE name = 'user:hovercraft'`,
	).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "active" {
		t.Errorf("status = %q after late reject on active kind, want active (review must not re-transition a non-proposed kind)", status)
	}
}
