package dispatcher_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

const ctxTS = "2026-06-24T09:00:00Z"

// pinnedEvent builds a contextPinned event.
func pinnedEvent(t *testing.T, pinUID, text, targetRef string) kernel.Event {
	t.Helper()
	at, _ := time.Parse(time.RFC3339, ctxTS)
	payload, err := json.Marshal(ontology.ContextPinnedEvent{
		PinUID:    ontology.ULID(pinUID),
		Text:      text,
		TargetRef: targetRef,
		PinnedAt:  at,
	})
	if err != nil {
		t.Fatalf("marshal contextPinned: %v", err)
	}
	return kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindContextPinned),
		Source:     "cli:pin",
		Confidence: 1.0,
		CreateTime: at,
		Payload:    payload,
	}
}

// correctedEvent builds a contextCorrected event.
func correctedEvent(t *testing.T, correctionUID, targetID, text string) kernel.Event {
	t.Helper()
	at, _ := time.Parse(time.RFC3339, ctxTS)
	payload, err := json.Marshal(ontology.ContextCorrectedEvent{
		CorrectionUID: ontology.ULID(correctionUID),
		TargetID:      targetID,
		Text:          text,
		CorrectedAt:   at,
	})
	if err != nil {
		t.Fatalf("marshal contextCorrected: %v", err)
	}
	return kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindContextCorrected),
		Source:     "cli:correct",
		Confidence: 1.0,
		CreateTime: at,
		Payload:    payload,
	}
}

// unpinnedEvent builds a contextUnpinned tombstone for pinUID.
func unpinnedEvent(t *testing.T, pinUID string) kernel.Event {
	t.Helper()
	at, _ := time.Parse(time.RFC3339, ctxTS)
	payload, err := json.Marshal(ontology.ContextUnpinnedEvent{
		PinUID:     ontology.ULID(pinUID),
		UnpinnedAt: at,
	})
	if err != nil {
		t.Fatalf("marshal contextUnpinned: %v", err)
	}
	return kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindContextUnpinned),
		Source:     "cli:unpin",
		Confidence: 1.0,
		CreateTime: at,
		Payload:    payload,
	}
}

type pinRow struct {
	pinType   string
	text      string
	targetRef sql.NullString
	active    int
	scope     string
}

func readPin(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid string) (pinRow, bool) {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	var r pinRow
	err = reader.QueryRowContext(ctx,
		`SELECT pin_type, text, target_ref, active, scope FROM context_pins WHERE uid = ?`, uid,
	).Scan(&r.pinType, &r.text, &r.targetRef, &r.active, &r.scope)
	if errors.Is(err, sql.ErrNoRows) {
		return pinRow{}, false
	}
	if err != nil {
		t.Fatalf("read context_pins: %v", err)
	}
	return r, true
}

// TestApply_ContextPinned_MaterializesRow projects a pin and asserts the row
// lands active with pin_type='pin' and the optional target_ref persisted.
func TestApply_ContextPinned_MaterializesRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	pinUID := ulid.Make().String()
	target := "entities/" + ulid.Make().String()

	if err := d.Apply(ctx, []kernel.Event{pinnedEvent(t, pinUID, "no new Postgres features", target)}); err != nil {
		t.Fatalf("apply pin: %v", err)
	}

	row, ok := readPin(t, ctx, d, pinUID)
	if !ok {
		t.Fatal("no context_pins row after contextPinned")
	}
	if row.pinType != "pin" {
		t.Errorf("pin_type = %q, want pin", row.pinType)
	}
	if row.active != 1 {
		t.Errorf("active = %d, want 1", row.active)
	}
	if row.text != "no new Postgres features" {
		t.Errorf("text = %q", row.text)
	}
	if !row.targetRef.Valid || row.targetRef.String != target {
		t.Errorf("target_ref = %v, want %q", row.targetRef, target)
	}
	if row.scope != "personal" {
		t.Errorf("scope = %q, want personal (default)", row.scope)
	}
}

// TestApply_ContextCorrected_MaterializesRow projects a correction and asserts
// it lands as a pin_type='correction' row with target_ref = targetId.
func TestApply_ContextCorrected_MaterializesRow(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	corrUID := ulid.Make().String()
	targetID := "entities/" + ulid.Make().String()

	if err := d.Apply(ctx, []kernel.Event{correctedEvent(t, corrUID, targetID, "left Acme in May")}); err != nil {
		t.Fatalf("apply correction: %v", err)
	}

	row, ok := readPin(t, ctx, d, corrUID)
	if !ok {
		t.Fatal("no context_pins row after contextCorrected")
	}
	if row.pinType != "correction" {
		t.Errorf("pin_type = %q, want correction", row.pinType)
	}
	if row.active != 1 {
		t.Errorf("active = %d, want 1", row.active)
	}
	if !row.targetRef.Valid || row.targetRef.String != targetID {
		t.Errorf("target_ref = %v, want %q", row.targetRef, targetID)
	}
}

// TestApply_ContextUnpinned_RetiresPin pins then unpins and asserts the pin row
// survives (audit) with active=0.
func TestApply_ContextUnpinned_RetiresPin(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	pinUID := ulid.Make().String()

	if err := d.Apply(ctx, []kernel.Event{pinnedEvent(t, pinUID, "a pin", "")}); err != nil {
		t.Fatalf("apply pin: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{unpinnedEvent(t, pinUID)}); err != nil {
		t.Fatalf("apply unpin: %v", err)
	}

	row, ok := readPin(t, ctx, d, pinUID)
	if !ok {
		t.Fatal("pin row must survive an unpin (tombstone, not delete)")
	}
	if row.active != 0 {
		t.Errorf("active = %d, want 0 after unpin", row.active)
	}
}

// TestApply_ContextUnpinned_ScopeIsolated is the #206 read-isolation AC: a
// contextUnpinned tombstone recorded in one scope must NOT retire a pin that
// lives in a different scope, even though pin_uid is globally unique. A personal
// pin targeted only by a team-scope tombstone stays active.
func TestApply_ContextUnpinned_ScopeIsolated(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	pinUID := ulid.Make().String()

	pin := pinnedEvent(t, pinUID, "a personal pin", "")
	pin.Scope = kernel.ScopePersonal
	unpin := unpinnedEvent(t, pinUID)
	unpin.Scope = kernel.ScopeTeam // different scope — must not retire the personal pin

	if err := d.Apply(ctx, []kernel.Event{pin}); err != nil {
		t.Fatalf("apply pin: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{unpin}); err != nil {
		t.Fatalf("apply cross-scope unpin: %v", err)
	}

	row, ok := readPin(t, ctx, d, pinUID)
	if !ok {
		t.Fatal("pin row missing")
	}
	if row.active != 1 {
		t.Errorf("active = %d, want 1: a team-scope tombstone must not retire a personal pin (#206)", row.active)
	}
}

// TestApply_ContextUnpinned_OrderIndependent is the convergence AC: a tombstone
// that replays BEFORE its pin still retires the pin once the pin row lands. Two
// fresh brains apply the same {pin, unpin} pair in opposite orders and converge
// to active=0.
func TestApply_ContextUnpinned_OrderIndependent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: exhaustive permutation test (~31s)")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)
	pinUID := ulid.Make().String()
	pin := pinnedEvent(t, pinUID, "a pin", "")
	unpin := unpinnedEvent(t, pinUID)

	dA := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	for _, ev := range []kernel.Event{pin, unpin} {
		if err := dA.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("apply A: %v", err)
		}
	}
	dB := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	for _, ev := range []kernel.Event{unpin, pin} {
		if err := dB.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("apply B: %v", err)
		}
	}

	rowA, okA := readPin(t, ctx, dA, pinUID)
	rowB, okB := readPin(t, ctx, dB, pinUID)
	if !okA || !okB {
		t.Fatal("pin row missing on one ordering")
	}
	if rowA.active != 0 || rowB.active != 0 {
		t.Errorf("unpin not order-independent: A.active=%d B.active=%d, want both 0", rowA.active, rowB.active)
	}
}

// TestApply_ContextPin_Idempotent re-applies a pin and its unpin twice and
// asserts the projection is stable (INSERT OR IGNORE on uid / tombstone event
// uid plus the deterministic active fold make a Rebuild replay a no-op).
func TestApply_ContextPin_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	pinUID := ulid.Make().String()
	pin := pinnedEvent(t, pinUID, "a pin", "")
	unpin := unpinnedEvent(t, pinUID)

	for i := range 2 {
		if err := d.Apply(ctx, []kernel.Event{pin, unpin}); err != nil {
			t.Fatalf("apply round %d: %v", i, err)
		}
	}

	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	var pins, tombstones int
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_pins WHERE uid = ?`, pinUID).Scan(&pins); err != nil {
		t.Fatalf("count pins: %v", err)
	}
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_pin_tombstones WHERE pin_uid = ?`, pinUID).Scan(&tombstones); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if pins != 1 {
		t.Errorf("context_pins rows = %d, want 1 (idempotent)", pins)
	}
	if tombstones != 1 {
		t.Errorf("context_pin_tombstones rows = %d, want 1 (idempotent)", tombstones)
	}
	row, _ := readPin(t, ctx, d, pinUID)
	if row.active != 0 {
		t.Errorf("active = %d, want 0", row.active)
	}
}

// TestApply_ContextEvents_ReplayStable applies a mixed event set in two
// different orders to two fresh dispatchers and asserts byte-identical
// projection rows — the replay-stability AC across all three context kinds.
func TestApply_ContextEvents_ReplayStable(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	pinA := ulid.Make().String()
	pinB := ulid.Make().String()
	corr := ulid.Make().String()
	target := "entities/" + ulid.Make().String()

	events := []kernel.Event{
		pinnedEvent(t, pinA, "pin A text", target),
		pinnedEvent(t, pinB, "pin B text", ""),
		correctedEvent(t, corr, target, "corrected text"),
		unpinnedEvent(t, pinB),
	}
	reversed := make([]kernel.Event, len(events))
	for i, ev := range events {
		reversed[len(events)-1-i] = ev
	}

	apply := func(order []kernel.Event) map[string]pinRow {
		d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
		for _, ev := range order {
			if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
				t.Fatalf("apply: %v", err)
			}
		}
		out := make(map[string]pinRow, 3)
		for _, uid := range []string{pinA, pinB, corr} {
			if row, ok := readPin(t, ctx, d, uid); ok {
				out[uid] = row
			}
		}
		return out
	}

	forward := apply(events)
	backward := apply(reversed)

	for _, uid := range []string{pinA, pinB, corr} {
		f, b := forward[uid], backward[uid]
		if f != b {
			t.Errorf("uid %s projection diverged by order: forward=%+v backward=%+v", uid, f, b)
		}
	}
	// Spot-check the expected end state regardless of order: pinB is unpinned.
	if forward[pinB].active != 0 {
		t.Errorf("pinB active = %d, want 0 (unpinned)", forward[pinB].active)
	}
	if forward[pinA].active != 1 || forward[corr].active != 1 {
		t.Errorf("pinA/corr should stay active: pinA=%d corr=%d", forward[pinA].active, forward[corr].active)
	}
}

// TestApply_ContextPin_RejectsInvalid pins the decode-time validation: blank
// required fields fail Apply with no row written.
func TestApply_ContextPin_RejectsInvalid(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	cases := []struct {
		name    string
		kind    string
		payload map[string]any
	}{
		{"pin missing pinUid", ontology.KindContextPinned, map[string]any{"text": "x", "pinnedAt": ctxTS}},
		{"pin missing text", ontology.KindContextPinned, map[string]any{"pinUid": ulid.Make().String(), "pinnedAt": ctxTS}},
		{"unpin missing pinUid", ontology.KindContextUnpinned, map[string]any{"unpinnedAt": ctxTS}},
		{"correction missing targetId", ontology.KindContextCorrected, map[string]any{"correctionUid": ulid.Make().String(), "text": "x", "correctedAt": ctxTS}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload, _ := json.Marshal(tc.payload)
			ev := kernel.Event{
				UID: ulid.Make().String(), Kind: kernel.EventKind(tc.kind),
				Source: "test", Confidence: 1.0, Payload: payload,
			}
			if err := d.Apply(ctx, []kernel.Event{ev}); err == nil {
				t.Errorf("expected Apply to reject %s, got nil", tc.name)
			}
		})
	}
}

// dismissedEvent builds an insightDismissed event for insightUID.
func dismissedEvent(t *testing.T, insightUID, reason string) kernel.Event {
	t.Helper()
	at, _ := time.Parse(time.RFC3339, ctxTS)
	payload, err := json.Marshal(ontology.InsightDismissedEvent{
		InsightUID:  ontology.ULID(insightUID),
		Reason:      reason,
		DismissedAt: at,
	})
	if err != nil {
		t.Fatalf("marshal insightDismissed: %v", err)
	}
	return kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindInsightDismissed),
		Source:     "cli:dismiss",
		Confidence: 1.0,
		CreateTime: at,
		Payload:    payload,
	}
}

// readInsightState reads the insights.state column for uid.
func readInsightState(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid string) string {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	var state string
	if err := reader.QueryRowContext(ctx, `SELECT state FROM insights WHERE uid = ?`, uid).Scan(&state); err != nil {
		t.Fatalf("read insight state: %v", err)
	}
	return state
}

// TestApply_InsightDismissed_SetsTerminalState synthesizes an insight, dismisses
// it, and asserts state flips to the terminal 'dismissed' — distinct from the
// decay sweep's 'demoted'.
func TestApply_InsightDismissed_SetsTerminalState(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	insightUID := ulid.Make().String()

	if err := d.Apply(ctx, []kernel.Event{insightEvent(t, insightUID, "a durable insight", []string{ulid.Make().String()})}); err != nil {
		t.Fatalf("apply insight: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{dismissedEvent(t, insightUID, "incorrect inference")}); err != nil {
		t.Fatalf("apply dismiss: %v", err)
	}

	if got := readInsightState(t, ctx, d, insightUID); got != "dismissed" {
		t.Errorf("state = %q, want dismissed", got)
	}
}

// TestApply_InsightDismissed_NotRepromotedByAccess proves dismissal is NOT
// conflated with the projection-side demote: a dismissed insight that is later
// accessed stays dismissed (reinforce-on-access lifts only 'demoted'), so a
// user's explicit dismissal is sticky.
func TestApply_InsightDismissed_NotRepromotedByAccess(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	insightUID := ulid.Make().String()

	if err := d.Apply(ctx, []kernel.Event{insightEvent(t, insightUID, "an insight", []string{ulid.Make().String()})}); err != nil {
		t.Fatalf("apply insight: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{dismissedEvent(t, insightUID, "")}); err != nil {
		t.Fatalf("apply dismiss: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{accessEvent(t, insightUID, time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC))}); err != nil {
		t.Fatalf("apply access: %v", err)
	}

	if got := readInsightState(t, ctx, d, insightUID); got != "dismissed" {
		t.Errorf("state = %q, want dismissed (access must not re-promote a dismissed insight)", got)
	}
}

// TestApply_InsightDismissed_Idempotent re-applies the same dismissal twice and
// asserts the state stays 'dismissed' (a Rebuild replay is a no-op).
func TestApply_InsightDismissed_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	insightUID := ulid.Make().String()

	if err := d.Apply(ctx, []kernel.Event{insightEvent(t, insightUID, "an insight", []string{ulid.Make().String()})}); err != nil {
		t.Fatalf("apply insight: %v", err)
	}
	dismiss := dismissedEvent(t, insightUID, "stale")
	for i := range 2 {
		if err := d.Apply(ctx, []kernel.Event{dismiss}); err != nil {
			t.Fatalf("apply dismiss round %d: %v", i, err)
		}
	}
	if got := readInsightState(t, ctx, d, insightUID); got != "dismissed" {
		t.Errorf("state = %q, want dismissed", got)
	}
}
