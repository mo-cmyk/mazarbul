package dispatcher_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// makeFactRetractedEvent builds a factRetracted event closing factUID as of
// retractedAt.
func makeFactRetractedEvent(t *testing.T, factUID string, retractedAt time.Time) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.FactRetractedEvent{
		FactUID:     factUID,
		RetractedAt: retractedAt,
	})
	if err != nil {
		t.Fatalf("marshal factRetracted: %v", err)
	}
	return kernel.Event{
		UID:        newFactULID(t),
		Kind:       kernel.EventKind(ontology.KindFactRetracted),
		Source:     "test:retract",
		Confidence: 1.0,
		CreateTime: retractedAt,
		Payload:    payload,
	}
}

// makeFactForgottenEvent builds a factForgotten event hard-deleting factUID.
func makeFactForgottenEvent(t *testing.T, factUID string, forgottenAt time.Time) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.FactForgottenEvent{
		FactUID:     factUID,
		ForgottenAt: forgottenAt,
	})
	if err != nil {
		t.Fatalf("marshal factForgotten: %v", err)
	}
	return kernel.Event{
		UID:        newFactULID(t),
		Kind:       kernel.EventKind(ontology.KindFactForgotten),
		Source:     "test:forget",
		Confidence: 1.0,
		CreateTime: forgottenAt,
		Payload:    payload,
	}
}

// openValue returns the value of the single open (current) row in the slot, or
// "" if none. Fails if more than one row is open — the fold's invariant is at
// most one current value per slot.
func openValue(t *testing.T, rows []dispatcher.FactRowForTest) string {
	t.Helper()
	var open string
	count := 0
	for _, r := range rows {
		if r.EventEnd == "" {
			open = r.Value
			count++
		}
	}
	if count > 1 {
		t.Fatalf("slot has %d open rows, want at most 1: %+v", count, rows)
	}
	return open
}

// TestApply_FactRetracted_EmptiesSlotKeepsHistory closes the open value of a
// slot that has history: the retracted value drops out of current reads and the
// slot reads as EMPTY — it does NOT fall back to the prior value. History is
// preserved (both rows survive).
func TestApply_FactRetracted_EmptiesSlotKeepsHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	t0 := time.Now().UTC()
	pendingUID, activeUID := newFactULID(t), newFactULID(t)
	if err := d.Apply(ctx, []kernel.Event{
		makeFactEvent(t, pendingUID, "state.task.status", "pending", t0),
		makeFactEvent(t, activeUID, "state.task.status", "active", t0.Add(time.Second)),
	}); err != nil {
		t.Fatalf("apply facts: %v", err)
	}

	// Retract the open "active" value.
	if err := d.Apply(ctx, []kernel.Event{
		makeFactRetractedEvent(t, activeUID, t0.Add(2*time.Second)),
	}); err != nil {
		t.Fatalf("apply factRetracted: %v", err)
	}

	rows, err := dispatcher.FactRowsForTest(ctx, d, factSubject, "state.task.status")
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (history preserved): %+v", len(rows), rows)
	}
	if got := openValue(t, rows); got != "" {
		t.Errorf("slot should be empty after retracting the current value, got current %q", got)
	}
	// The retracted row is closed, carries a tombstone, and has no superseder.
	for _, r := range rows {
		if r.UID != activeUID {
			continue
		}
		if r.EventEnd == "" {
			t.Errorf("retracted row should be closed, got open")
		}
		if r.RetractedAt == "" {
			t.Errorf("retracted row should carry retracted_at, got empty")
		}
		if r.SupersededBy != "" {
			t.Errorf("retracted row superseded_by = %q, want empty", r.SupersededBy)
		}
	}
}

// TestApply_FactRetracted_WriteAfterRetraction is the write-following-retraction
// path: assert A, assert B, retract B, assert C. C is the current value; the
// interval between B's retraction and C is genuinely empty (the slot does not
// revert to A), so the fold leaves no spurious second open row and A's window
// ends where B superseded it — there is no temporal hole to fill.
func TestApply_FactRetracted_WriteAfterRetraction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	t0 := time.Now().UTC()
	aUID, bUID, cUID := newFactULID(t), newFactULID(t), newFactULID(t)
	if err := d.Apply(ctx, []kernel.Event{
		makeFactEvent(t, aUID, "state.task.status", "A", t0),
		makeFactEvent(t, bUID, "state.task.status", "B", t0.Add(time.Second)),
		makeFactRetractedEvent(t, bUID, t0.Add(2*time.Second)),
		makeFactEvent(t, cUID, "state.task.status", "C", t0.Add(3*time.Second)),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rows, err := dispatcher.FactRowsForTest(ctx, d, factSubject, "state.task.status")
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	// Exactly one open row, and it is C (openValue fatals on >1 open).
	if got := openValue(t, rows); got != "C" {
		t.Errorf("current value = %q, want C", got)
	}
	for _, r := range rows {
		switch r.UID {
		case cUID:
			if r.EventEnd != "" {
				t.Errorf("C should be open, got event_end %q", r.EventEnd)
			}
		case bUID:
			if r.RetractedAt == "" || r.EventEnd == "" {
				t.Errorf("B should be retracted+closed, got retracted_at=%q event_end=%q", r.RetractedAt, r.EventEnd)
			}
			if r.SupersededBy != "" {
				t.Errorf("retracted B superseded_by = %q, want empty", r.SupersededBy)
			}
		case aUID:
			if r.EventEnd == "" {
				t.Errorf("A should be closed (superseded by B), got open")
			}
			if r.SupersededBy != bUID {
				t.Errorf("A superseded_by = %q, want B (%s) — the faithful history link", r.SupersededBy, bUID)
			}
		}
	}
}

// TestApply_FactRetracted_SingleValueEmptiesSlot retracting the only value in a
// slot leaves NO current value (the slot reads as absent), while the row
// survives in history.
func TestApply_FactRetracted_SingleValueEmptiesSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	t0 := time.Now().UTC()
	uid := newFactULID(t)
	if err := d.Apply(ctx, []kernel.Event{
		makeFactEvent(t, uid, "preference.coffee.milk", "oat", t0),
		makeFactRetractedEvent(t, uid, t0.Add(time.Second)),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rows, err := dispatcher.FactRowsForTest(ctx, d, factSubject, "preference.coffee.milk")
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (history preserved): %+v", len(rows), rows)
	}
	if got := openValue(t, rows); got != "" {
		t.Errorf("slot should be empty after retract, current = %q", got)
	}
	if rows[0].RetractedAt == "" {
		t.Errorf("row should carry retracted_at")
	}
}

// TestApply_FactRetracted_ReplayIdempotent applying the same retraction twice
// converges to the same state.
func TestApply_FactRetracted_ReplayIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	t0 := time.Now().UTC()
	uid := newFactULID(t)
	events := []kernel.Event{
		makeFactEvent(t, uid, "state.x", "v1", t0),
		makeFactRetractedEvent(t, uid, t0.Add(time.Second)),
	}
	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("replay apply: %v", err)
	}
	rows, err := dispatcher.FactRowsForTest(ctx, d, factSubject, "state.x")
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 after replay: %+v", len(rows), rows)
	}
	if got := openValue(t, rows); got != "" {
		t.Errorf("slot should remain empty after replay, current = %q", got)
	}
}

// TestApply_FactRetracted_MissingFactNoOp retracting a fact uid that does not
// exist is a clean no-op (idempotent: the tombstone may replay ahead of, or
// after the forget of, its target).
func TestApply_FactRetracted_MissingFactNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	if err := d.Apply(ctx, []kernel.Event{
		makeFactRetractedEvent(t, newFactULID(t), time.Now().UTC()),
	}); err != nil {
		t.Fatalf("retract missing fact should no-op, got: %v", err)
	}
}

// TestApply_FactForgotten_DeletesAndReconciles hard-deletes the open value and
// re-derives the slot: the row is gone and the prior value becomes current.
func TestApply_FactForgotten_DeletesAndReconciles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	t0 := time.Now().UTC()
	v1UID, v2UID := newFactULID(t), newFactULID(t)
	if err := d.Apply(ctx, []kernel.Event{
		makeFactEvent(t, v1UID, "state.role", "ic", t0),
		makeFactEvent(t, v2UID, "state.role", "manager", t0.Add(time.Second)),
		makeFactForgottenEvent(t, v2UID, t0.Add(2*time.Second)),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rows, err := dispatcher.FactRowsForTest(ctx, d, factSubject, "state.role")
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (forgotten row deleted): %+v", len(rows), rows)
	}
	if rows[0].UID != v1UID {
		t.Errorf("surviving row uid = %q, want %q", rows[0].UID, v1UID)
	}
	if got := openValue(t, rows); got != "ic" {
		t.Errorf("current value = %q, want ic (prior re-opened)", got)
	}
}

// TestApply_FactForgotten_MissingNoOp forgetting a missing fact is a no-op.
func TestApply_FactForgotten_MissingNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	if err := d.Apply(ctx, []kernel.Event{
		makeFactForgottenEvent(t, newFactULID(t), time.Now().UTC()),
	}); err != nil {
		t.Fatalf("forget missing fact should no-op, got: %v", err)
	}
}
