package dispatcher_test

// dispatcher_fact_convergence_test.go verifies that the fact-slot fold is
// commutative: applying the same set of fact events in any order produces
// identical slot state. The property is guaranteed by the HLC-based
// readFactSlot ordering — the fold's "latest" row is determined by the HLC
// stamp fixed at write time, not by arrival order.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
)

// makeHLCFactEvent builds a KindFact event whose HLC stamp is set explicitly
// so permutation tests can assign distinct causal positions independently of
// CreateTime. uid, stateKey, value, and createTime follow makeFactEvent; hlc
// overrides the automatic EffectiveHLC.
func makeHLCFactEvent(t *testing.T, uid, stateKey, value string, createTime time.Time, hlc string) kernel.Event {
	t.Helper()
	ev := makeFactEvent(t, uid, stateKey, value, createTime)
	ev.HLC = hlc
	return ev
}

// openValueForKey returns the current (open, event_end == "") value for the
// named (subjectUID, stateKey) slot, or "" if the slot is empty. Fails the
// test if more than one row is open — reconcileFactSlot guarantees at most one.
func openValueForKey(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, subjectUID, stateKey string) string {
	t.Helper()
	rows, err := dispatcher.FactRowsForTest(ctx, d, subjectUID, stateKey)
	if err != nil {
		t.Fatalf("FactRowsForTest: %v", err)
	}
	var cur string
	count := 0
	for _, r := range rows {
		if r.EventEnd == "" && r.RetractedAt == "" {
			cur = r.Value
			count++
		}
	}
	if count > 1 {
		t.Fatalf("slot has %d open rows, want at most 1: %+v", count, rows)
	}
	return cur
}

// permute3 returns all 6 permutations of a 3-element event slice.
func permute3(s []kernel.Event) [][]kernel.Event {
	a, b, c := s[0], s[1], s[2]
	return [][]kernel.Event{
		{a, b, c},
		{a, c, b},
		{b, a, c},
		{b, c, a},
		{c, a, b},
		{c, b, a},
	}
}

// permLabel returns a short label for a permutation index (abc, acb, …).
func permLabel(i int) string {
	labels := [6]string{"abc", "acb", "bac", "bca", "cab", "cba"}
	if i < len(labels) {
		return labels[i]
	}
	return "other"
}

// TestFact_SlotConverges_ABA verifies that three events on the same slot with
// values A, B, A applied in every permutation converge to the same current
// value. The canonical order is determined by HLC, so the third event (the
// HLC-latest one) wins regardless of arrival order. The repeated "alpha" value
// exercises the removed suppression gate: both alpha rows must be stored and
// the HLC-latest row must be the open current value.
//
// All six permutation sub-tests share one dispatcher. Each permutation uses a
// unique state key and unique event UIDs so the slots are independent within
// the same SQLite database, avoiding the cost of opening a KuzuDB connection
// per permutation.
func TestFact_SlotConverges_ABA(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: opens one dispatcher + 6 permutation sub-tests")
	}
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// HLC stamps are sortable 16-hex strings; these are strictly increasing.
	const (
		hlc1 = "000001977420dc100000"
		hlc2 = "000001977420dc200000"
		hlc3 = "000001977420dc300000"
		want = "alpha" // uid3 is HLC-latest and holds "alpha"
	)

	for i := range 6 {
		// Unique state key and event UIDs per permutation to avoid slot
		// collision within the shared dispatcher.
		key := fmt.Sprintf("state.status.perm%d", i)
		uid1 := fmt.Sprintf("01HCONV%04d000000000000001", i)
		uid2 := fmt.Sprintf("01HCONV%04d000000000000002", i)
		uid3 := fmt.Sprintf("01HCONV%04d000000000000003", i)

		base := []kernel.Event{
			makeHLCFactEvent(t, uid1, key, "alpha", t0, hlc1),
			makeHLCFactEvent(t, uid2, key, "beta", t0.Add(time.Second), hlc2),
			makeHLCFactEvent(t, uid3, key, "alpha", t0.Add(2*time.Second), hlc3),
		}
		perm := permute3(base)[i]

		t.Run(permLabel(i), func(t *testing.T) {
			if err := d.Apply(ctx, perm); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			got := openValueForKey(t, ctx, d, factSubject, key)
			if got != want {
				rows, _ := dispatcher.FactRowsForTest(ctx, d, factSubject, key)
				t.Errorf("permutation %d: current value = %q, want %q; rows: %+v",
					i, got, want, rows)
			}
		})
	}
}

// TestFact_SlotConverges_RebuildEqualsLive verifies that applying events in
// canonical (HLC) order produces the same slot state as applying them in
// reverse order and then replaying all events again (simulating a rebuild).
func TestFact_SlotConverges_RebuildEqualsLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: opens two dispatchers for live-vs-rebuild comparison")
	}
	t.Parallel()
	ctx := context.Background()

	const key = "state.employer"
	t0 := time.Date(2026, 2, 1, 8, 0, 0, 0, time.UTC)

	uid1 := "01HREBLD0000000000000000001"
	uid2 := "01HREBLD0000000000000000002"
	uid3 := "01HREBLD0000000000000000003"
	hlc1 := "000001977420dd100000"
	hlc2 := "000001977420dd200000"
	hlc3 := "000001977420dd300000"
	events := []kernel.Event{
		makeHLCFactEvent(t, uid1, key, "alpha", t0, hlc1),
		makeHLCFactEvent(t, uid2, key, "beta", t0.Add(time.Second), hlc2),
		makeHLCFactEvent(t, uid3, key, "alpha", t0.Add(2*time.Second), hlc3),
	}

	// Live apply in canonical (HLC) order.
	dLive := newDispatcher(t, ctx, nil)
	if err := dLive.Apply(ctx, events); err != nil {
		t.Fatalf("live Apply: %v", err)
	}
	wantValue := openValueForKey(t, ctx, dLive, factSubject, key)
	wantRows, err := dispatcher.FactRowsForTest(ctx, dLive, factSubject, key)
	if err != nil {
		t.Fatalf("FactRowsForTest (live): %v", err)
	}

	// Rebuild: apply in reverse order then replay the full set.
	dRebuild := newDispatcher(t, ctx, nil)
	reversed := make([]kernel.Event, len(events))
	copy(reversed, events)
	for lo, hi := 0, len(reversed)-1; lo < hi; lo, hi = lo+1, hi-1 {
		reversed[lo], reversed[hi] = reversed[hi], reversed[lo]
	}
	if err := dRebuild.Apply(ctx, reversed); err != nil {
		t.Fatalf("rebuild Apply (reversed): %v", err)
	}
	if err := dRebuild.Apply(ctx, events); err != nil {
		t.Fatalf("rebuild Apply (replay): %v", err)
	}

	gotValue := openValueForKey(t, ctx, dRebuild, factSubject, key)
	gotRows, err := dispatcher.FactRowsForTest(ctx, dRebuild, factSubject, key)
	if err != nil {
		t.Fatalf("FactRowsForTest (rebuild): %v", err)
	}

	if gotValue != wantValue {
		t.Errorf("rebuild current value = %q, want %q", gotValue, wantValue)
	}
	if len(gotRows) != len(wantRows) {
		t.Errorf("rebuild row count = %d, want %d", len(gotRows), len(wantRows))
	}
}

// TestFact_ShuffledConvergence is the exhaustive-shuffle proof: many random
// permutations of a 4-event slot must all converge to the same current value.
// A single dispatcher is shared across all trials; each trial uses unique event
// UIDs and a unique state key so the slots are independent within the same
// SQLite database. This avoids the overhead of opening a new KuzuDB connection
// per trial.
func TestFact_ShuffledConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: opens 20 full dispatcher+KuzuDB instances")
	}
	t.Parallel()
	ctx := context.Background()
	d := newDispatcher(t, ctx, nil)

	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	hlcs := [4]string{
		"000001977420de100000",
		"000001977420de200000",
		"000001977420de300000",
		"000001977420de400000",
	}
	vals := [4]string{"alpha", "beta", "gamma", "beta"}
	// The HLC-latest event is index 3 (hlcs[3]), which holds "beta".
	const want = "beta"

	rng := rand.New(rand.NewPCG(0xDEADBEEF, 0xCAFEBABE))
	for trial := range 20 {
		// Unique state key and event UIDs per trial: each trial forms an
		// independent slot within the shared dispatcher's SQLite database.
		key := fmt.Sprintf("state.role.trial%d", trial)
		events := make([]kernel.Event, 4)
		for i := range 4 {
			uid := fmt.Sprintf("01HSHUF%06d%06d000000000", trial, i)
			events[i] = makeHLCFactEvent(t, uid, key, vals[i],
				t0.Add(time.Duration(i)*time.Second), hlcs[i])
		}

		shuffled := make([]kernel.Event, len(events))
		copy(shuffled, events)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		if err := d.Apply(ctx, shuffled); err != nil {
			t.Fatalf("trial %d Apply: %v", trial, err)
		}
		got := openValueForKey(t, ctx, d, factSubject, key)
		if got != want {
			t.Errorf("trial %d: current value = %q, want %q", trial, got, want)
		}
	}
}
