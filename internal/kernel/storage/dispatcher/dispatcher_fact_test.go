package dispatcher_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// factSubject is an arbitrary entity UID; the facts table has no FK to
// entities, so the writer tests need not seed one.
const factSubject = "01J000000000000000000SUBJ"

// makeFactEvent builds a KindFact event. CreateTime drives the supersede
// ordering (reconcileFactSlot orders by effective start), so callers pass
// increasing times to model a slot changing value over time.
func makeFactEvent(t *testing.T, uid, stateKey, value string, createTime time.Time) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"subjectUid": factSubject,
		"stateKey":   stateKey,
		"value":      value,
	})
	if err != nil {
		t.Fatalf("marshal fact payload: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindFact),
		Source:     "test:fact",
		Confidence: 1.0,
		CreateTime: createTime,
		Payload:    payload,
	}
}

// newFactULID returns a fresh monotonic ULID for a test fact event.
func newFactULID(t *testing.T) string {
	t.Helper()
	id, err := ulid.New(ulid.Now(), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		t.Fatalf("ulid: %v", err)
	}
	return id.String()
}

// TestApply_Fact_SupersedeChain asserts that a second value in the same
// (subject, state_key) slot closes the first: the prior Fact gets event_end +
// superseded_by_uid set, and only the latest Fact stays open.
func TestApply_Fact_SupersedeChain(t *testing.T) {
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

	rows, err := dispatcher.FactRowsForTest(ctx, d, factSubject, "state.task.status")
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d fact rows, want 2: %+v", len(rows), rows)
	}
	// Row 0: the superseded "pending" fact — closed, pointing at the active one.
	if rows[0].Value != "pending" {
		t.Errorf("rows[0].Value = %q, want pending", rows[0].Value)
	}
	if rows[0].EventEnd == "" {
		t.Errorf("rows[0] should be closed (event_end set), got open")
	}
	if rows[0].SupersededBy != activeUID {
		t.Errorf("rows[0].SupersededBy = %q, want %q", rows[0].SupersededBy, activeUID)
	}
	// Row 1: the open "active" fact.
	if rows[1].Value != "active" {
		t.Errorf("rows[1].Value = %q, want active", rows[1].Value)
	}
	if rows[1].EventEnd != "" {
		t.Errorf("rows[1] should be open (event_end empty), got closed at %q", rows[1].EventEnd)
	}
	if rows[1].SupersededBy != "" {
		t.Errorf("rows[1].SupersededBy = %q, want empty", rows[1].SupersededBy)
	}
}

// makeScopedFactEvent is makeFactEvent with an explicit scope stamped on the
// event, so the projected facts row lands in that scope's slot partition.
func makeScopedFactEvent(t *testing.T, uid, stateKey, value string, createTime time.Time, scope kernel.Scope) kernel.Event {
	t.Helper()
	ev := makeFactEvent(t, uid, stateKey, value, createTime)
	ev.Scope = scope
	return ev
}

// TestApply_Fact_ScopeIsolatedSlots reproduces the #206 fact-slot leak: facts
// for the same (subject, state_key) but DIFFERENT scopes must form independent
// supersede chains. A later team-scope write must not close or supersede the
// open personal-scope fact (which would make a personal read see the wrong, or
// no, current value), and a later personal write must close only the personal
// chain.
func TestApply_Fact_ScopeIsolatedSlots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	const key = "state.employer"
	t0 := time.Now().UTC()
	personalUID := newFactULID(t)
	teamUID := newFactULID(t)
	// Personal "Mithril" first, then a LATER team "Globex" for the same slot.
	if err := d.Apply(ctx, []kernel.Event{
		makeScopedFactEvent(t, personalUID, key, "Mithril", t0, kernel.ScopePersonal),
		makeScopedFactEvent(t, teamUID, key, "Globex", t0.Add(time.Hour), kernel.ScopeTeam),
	}); err != nil {
		t.Fatalf("apply scoped facts: %v", err)
	}

	rows, err := dispatcher.FactRowsForTest(ctx, d, factSubject, key)
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	byUID := map[string]dispatcher.FactRowForTest{}
	for _, r := range rows {
		byUID[r.UID] = r
	}
	// The team write must NOT have closed the personal fact.
	if p := byUID[personalUID]; p.EventEnd != "" || p.SupersededBy != "" {
		t.Errorf("LEAK: personal fact closed by a team write (end=%q supersededBy=%q)", p.EventEnd, p.SupersededBy)
	}
	if tm := byUID[teamUID]; tm.EventEnd != "" || tm.SupersededBy != "" {
		t.Errorf("team fact unexpectedly closed (end=%q supersededBy=%q)", tm.EventEnd, tm.SupersededBy)
	}

	// A later personal write closes ONLY the personal chain, leaving team open.
	personal2UID := newFactULID(t)
	if err := d.Apply(ctx, []kernel.Event{
		makeScopedFactEvent(t, personal2UID, key, "Beta", t0.Add(2*time.Hour), kernel.ScopePersonal),
	}); err != nil {
		t.Fatalf("apply later personal fact: %v", err)
	}
	rows, err = dispatcher.FactRowsForTest(ctx, d, factSubject, key)
	if err != nil {
		t.Fatalf("read facts after personal update: %v", err)
	}
	byUID = map[string]dispatcher.FactRowForTest{}
	for _, r := range rows {
		byUID[r.UID] = r
	}
	if p := byUID[personalUID]; p.SupersededBy != personal2UID {
		t.Errorf("first personal fact should be superseded by the second personal fact, got supersededBy=%q", p.SupersededBy)
	}
	if p2 := byUID[personal2UID]; p2.EventEnd != "" {
		t.Errorf("latest personal fact should be open, got end=%q", p2.EventEnd)
	}
	if tm := byUID[teamUID]; tm.EventEnd != "" || tm.SupersededBy != "" {
		t.Errorf("LEAK: team fact disturbed by a personal write (end=%q supersededBy=%q)", tm.EventEnd, tm.SupersededBy)
	}
}

// TestApply_Fact_ReassertPersistsRow verifies that re-asserting the same value
// in a slot produces a second row (the suppression gate was removed to fix
// cross-device convergence). The HLC-latest row is the open current value; the
// earlier re-assertion is superseded. Both rows survive so the full causal
// history is auditable.
func TestApply_Fact_ReassertPersistsRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	t0 := time.Now().UTC()
	uid1, uid2 := newFactULID(t), newFactULID(t)
	if err := d.Apply(ctx, []kernel.Event{
		makeFactEvent(t, uid1, "preference.coffee.milk", "oat", t0),
		makeFactEvent(t, uid2, "preference.coffee.milk", "oat", t0.Add(time.Second)),
	}); err != nil {
		t.Fatalf("apply facts: %v", err)
	}

	rows, err := dispatcher.FactRowsForTest(ctx, d, factSubject, "preference.coffee.milk")
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	// Two distinct events produce two rows; both carry the same value.
	if len(rows) != 2 {
		t.Fatalf("got %d fact rows, want 2 (each event inserts its row): %+v", len(rows), rows)
	}
	// The HLC-earlier row is superseded by the later one.
	if rows[0].Value != "oat" || rows[0].EventEnd == "" {
		t.Errorf("rows[0] = %+v, want oat closed (superseded)", rows[0])
	}
	if rows[0].SupersededBy != uid2 {
		t.Errorf("rows[0].SupersededBy = %q, want %q", rows[0].SupersededBy, uid2)
	}
	// The HLC-later row is the current open value.
	if rows[1].Value != "oat" || rows[1].EventEnd != "" {
		t.Errorf("rows[1] = %+v, want oat open (current)", rows[1])
	}
}

// TestApply_Fact_ReplayIdempotent asserts the writer converges to the same
// state when the event log is replayed (Rebuild): re-applying the same fact
// events leaves the supersede chain unchanged.
func TestApply_Fact_ReplayIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	t0 := time.Now().UTC()
	pendingUID, activeUID := newFactULID(t), newFactULID(t)
	events := []kernel.Event{
		makeFactEvent(t, pendingUID, "state.task.status", "pending", t0),
		makeFactEvent(t, activeUID, "state.task.status", "active", t0.Add(time.Second)),
	}
	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("apply facts (first pass): %v", err)
	}
	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("apply facts (replay): %v", err)
	}

	rows, err := dispatcher.FactRowsForTest(ctx, d, factSubject, "state.task.status")
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d fact rows after replay, want 2: %+v", len(rows), rows)
	}
	if rows[0].Value != "pending" || rows[0].SupersededBy != activeUID {
		t.Errorf("rows[0] = %+v, want pending superseded by %q", rows[0], activeUID)
	}
	if rows[1].Value != "active" || rows[1].EventEnd != "" {
		t.Errorf("rows[1] = %+v, want open active", rows[1])
	}
}
