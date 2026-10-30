package navigate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// TestFactsForSubjectAndGetFact seeds a slot that changed value (pending →
// active) plus a second open slot, then checks the open-only view, the full
// history, and single-fact fetch.
func TestFactsForSubjectAndGetFact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	now := time.Now().UTC()
	// A realistic past timeline so the "current truth" view (ADR-0001 window-at-
	// now) is well-defined: the slot held "pending" from an hour ago until a
	// minute ago, when it changed to "active" (open). Past create_times keep the
	// active value live under COALESCE(event_start, create_time) <= now.
	startStr := now.Add(-time.Hour).Format(time.RFC3339Nano)
	closeAt := now.Add(-time.Minute).Format(time.RFC3339Nano)
	const ins = `INSERT INTO facts
		(uid, subject_uid, state_key, value, event_start, event_end, create_time,
		 source_event_uid, content_hash, superseded_by_uid, source_priority)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	// state.task.status: pending (closed a minute ago, superseded by active) then
	// active (open since).
	exec(ins, "f_pending", "alice", "state.task.status", "pending", startStr, closeAt,
		startStr, "f_pending", "h1", "f_active", 1.0)
	exec(ins, "f_active", "alice", "state.task.status", "active", closeAt, nil,
		closeAt, "f_active", "h2", nil, 1.0)
	// A second, independent open slot.
	exec(ins, "f_milk", "alice", "preference.coffee.milk", "oat", startStr, nil,
		startStr, "f_milk", "h3", nil, 1.0)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	// Current truth (zero filter): the active status + the milk preference,
	// ordered by state_key.
	open, err := nav.FactsForSubject(ctx, "alice", navigate.FactFilter{})
	if err != nil {
		t.Fatalf("FactsForSubject(current): %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("current facts = %d, want 2: %+v", len(open), open)
	}
	if open[0].StateKey != "preference.coffee.milk" || open[0].Value != "oat" {
		t.Errorf("open[0] = %+v, want preference.coffee.milk=oat", open[0])
	}
	if open[1].StateKey != "state.task.status" || open[1].Value != "active" || open[1].EventEnd != "" {
		t.Errorf("open[1] = %+v, want open state.task.status=active", open[1])
	}

	// Full history includes the superseded pending value (3 rows total).
	all, err := nav.FactsForSubject(ctx, "alice", navigate.FactFilter{IncludeInactive: true})
	if err != nil {
		t.Fatalf("FactsForSubject(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all facts = %d, want 3: %+v", len(all), all)
	}

	// Single fetch: the closed pending fact carries its supersede pointer + name.
	f, err := nav.GetFact(ctx, "f_pending")
	if err != nil {
		t.Fatalf("GetFact: %v", err)
	}
	if f.Value != "pending" || f.Name != "facts/f_pending" || f.SupersededByUID != "f_active" || f.EventEnd == "" {
		t.Errorf("GetFact(f_pending) = %+v", f)
	}
	// SourceEventUID anchors a fact to its origin event for provenance walks.
	if f.SourceEventUID != "f_pending" {
		t.Errorf("GetFact(f_pending).SourceEventUID = %q, want f_pending", f.SourceEventUID)
	}

	if _, err := nav.GetFact(ctx, "missing"); !errors.Is(err, navigate.ErrNotFound) {
		t.Errorf("GetFact(missing): want ErrNotFound, got %v", err)
	}
}

// TestFactsForSubjectBitemporalAnchors pins the ADR-0001 anchors that get threads
// into the fact sub-query: --at time-travels the valid window, --as-of hides facts
// recorded after the instant, and --include-inactive returns the full slot
// history. It mirrors TestEntityRelationsTimeTravel for the facts table — the
// proof the anchor genuinely reaches the SQL, not just the flag parser.
func TestFactsForSubjectBitemporalAnchors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	now := time.Now().UTC()
	from72 := now.Add(-72 * time.Hour).Format(time.RFC3339Nano)
	until24 := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	recordedNow := now.Format(time.RFC3339Nano)
	const ins = `INSERT INTO facts
		(uid, subject_uid, state_key, value, event_start, event_end, create_time,
		 source_event_uid, content_hash, superseded_by_uid, source_priority)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	// state.role: Engineer over a window that closed 24h ago (ended, NOT
	// superseded — the time-travel shape), then Manager open since, recorded now.
	exec(ins, "f_eng", "dave", "state.role", "Engineer", from72, until24, from72, "f_eng", "h1", nil, 1.0)
	exec(ins, "f_mgr", "dave", "state.role", "Manager", until24, nil, recordedNow, "f_mgr", "h2", nil, 1.0)
	// A stable open preference recorded 72h ago.
	exec(ins, "f_milk", "dave", "preference.coffee.milk", "oat", from72, nil, from72, "f_milk", "h3", nil, 1.0)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	values := func(fs []navigate.Fact) map[string]string {
		m := map[string]string{}
		for _, f := range fs {
			m[f.UID] = f.Value
		}
		return m
	}

	// Current truth (zero filter): only each slot's value live now — Manager + oat.
	def, err := nav.FactsForSubject(ctx, "dave", navigate.FactFilter{})
	if err != nil {
		t.Fatalf("FactsForSubject(current): %v", err)
	}
	if got := values(def); len(got) != 2 || got["f_mgr"] != "Manager" || got["f_milk"] != "oat" {
		t.Fatalf("default facts = %+v, want open Manager + oat", def)
	}

	// --at inside the Engineer window (before close): Engineer is active, Manager
	// has not started yet.
	at := now.Add(-48 * time.Hour)
	before := values(mustFacts(t, ctx, nav, "dave", navigate.FactFilter{At: &at}))
	if before["f_eng"] != "Engineer" {
		t.Errorf("--at before close: want f_eng active, got %+v", before)
	}
	if _, ok := before["f_mgr"]; ok {
		t.Errorf("--at before close: f_mgr should not be active yet, got %+v", before)
	}

	// --at after the window closed: Engineer is gone, Manager is the open value.
	after := now.Add(time.Hour)
	post := values(mustFacts(t, ctx, nav, "dave", navigate.FactFilter{At: &after}))
	if _, ok := post["f_eng"]; ok {
		t.Errorf("--at after close: f_eng should be closed/absent, got %+v", post)
	}
	if post["f_mgr"] != "Manager" {
		t.Errorf("--at after close: want f_mgr open, got %+v", post)
	}

	// --as-of before f_mgr was recorded: the later-recorded Manager fact is hidden.
	asOf := now.Add(-12 * time.Hour)
	asof := values(mustFacts(t, ctx, nav, "dave", navigate.FactFilter{AsOf: &asOf}))
	if _, ok := asof["f_mgr"]; ok {
		t.Errorf("--as-of before record: f_mgr (recorded later) should be hidden, got %+v", asof)
	}

	// --include-inactive: the full slot history (all 3 rows).
	all := mustFacts(t, ctx, nav, "dave", navigate.FactFilter{IncludeInactive: true})
	if len(all) != 3 {
		t.Fatalf("includeInactive facts = %d, want 3 (full history): %+v", len(all), all)
	}
}

// mustFacts runs FactsForSubject and fails on error, returning the rows.
func mustFacts(t *testing.T, ctx context.Context, nav *navigate.Navigator, subject string, f navigate.FactFilter) []navigate.Fact {
	t.Helper()
	out, err := nav.FactsForSubject(ctx, subject, f)
	if err != nil {
		t.Fatalf("FactsForSubject(%+v): %v", f, err)
	}
	return out
}
