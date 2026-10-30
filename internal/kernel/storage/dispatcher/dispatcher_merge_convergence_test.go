package dispatcher_test

// dispatcher_merge_convergence_test.go verifies that the entity_redirects
// fold is commutative and idempotent: applying the same set of entityMerged
// events in any arrival order produces identical entity_redirects state and
// identical resolveRedirect output.
//
// entity_redirects has no REFERENCES constraint to entities, so the tests do
// not seed entity rows. applyEntityMerged handles missing entities as no-ops
// throughout (graph MATCH on missing node yields 0 rows, DELETE is a no-op,
// entities_vec miss is logged and ignored). This lets the tests focus on
// redirect-chain semantics without KuzuDB write overhead per trial.

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// makeMergeEvent builds a KindEntityMerged event with an explicit event UID so
// permutation tests can control the MIN-fold winner.
func makeMergeEvent(t *testing.T, eventUID, srcUID, tgtUID string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.EntityMergedEvent{
		SourceEntityUID: srcUID,
		TargetEntityUID: tgtUID,
	})
	if err != nil {
		t.Fatalf("marshal entityMerged: %v", err)
	}
	return kernel.Event{
		UID:        eventUID,
		Kind:       kernel.EventKind(ontology.KindEntityMerged),
		Source:     "test/merge",
		Confidence: 1.0,
		CreateTime: time.Now().UTC(),
		Payload:    payload,
	}
}

// makeFactEventForSubject builds a KindFact event for a specific subject UID
// (not the shared factSubject constant) so merge-then-fact-write tests can
// target a named entity.
func makeFactEventForSubject(t *testing.T, uid, subjectUID, stateKey, value string, createTime time.Time) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"subjectUid": subjectUID,
		"stateKey":   stateKey,
		"value":      value,
	})
	if err != nil {
		t.Fatalf("marshal fact: %v", err)
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

// TestEntityMerge_Cycle_BothOrders verifies that concurrent A→B and B→A merges
// applied in both possible orders produce identical entity_redirects state and
// identical resolveRedirect output. The canonical survivor is MIN(A_uid, B_uid).
//
// A single dispatcher is shared across both order trials; each trial uses
// unique entity and event UIDs so the redirect chains are independent within
// the same SQLite database. This avoids one extra KuzuDB open per trial.
func TestEntityMerge_Cycle_BothOrders(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: opens a KuzuDB dispatcher for cycle-detection proof")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	// Each trial uses unique entity UIDs so the redirect chains are independent
	// within the shared dispatcher's SQLite database. uidA < uidB in both
	// trials so MIN(uidA, uidB) = uidA is the expected survivor.
	trials := []struct {
		name string
		uidA string
		uidB string
		evAB string
		evBA string
		evs  func(ab, ba kernel.Event) []kernel.Event
	}{
		{
			name: "AB then BA",
			uidA: "01HCYCLE0AB000000000000000A",
			uidB: "01HCYCLE0AB000000000000000B",
			evAB: "01HCYCLEEVTAB0000000000001A",
			evBA: "01HCYCLEEVTBA0000000000001B",
			evs:  func(ab, ba kernel.Event) []kernel.Event { return []kernel.Event{ab, ba} },
		},
		{
			name: "BA then AB",
			uidA: "01HCYCLE0BA000000000000000A",
			uidB: "01HCYCLE0BA000000000000000B",
			evAB: "01HCYCLEEVTAB0000000000002A",
			evBA: "01HCYCLEEVTBA0000000000002B",
			evs:  func(ab, ba kernel.Event) []kernel.Event { return []kernel.Event{ba, ab} },
		},
	}

	for _, tc := range trials {
		eventAB := makeMergeEvent(t, tc.evAB, tc.uidA, tc.uidB)
		eventBA := makeMergeEvent(t, tc.evBA, tc.uidB, tc.uidA)
		evs := tc.evs(eventAB, eventBA)

		if err := d.Apply(ctx, evs); err != nil {
			t.Fatalf("%s: Apply: %v", tc.name, err)
		}

		// Cycle: uidA → uidB and uidB → uidA. MIN(uidA, uidB) = uidA (survivor).
		wantSurvivor := tc.uidA
		gotA, err := dispatcher.ResolveRedirectForTest(ctx, d, tc.uidA)
		if err != nil {
			t.Fatalf("%s: ResolveRedirect(A): %v", tc.name, err)
		}
		gotB, err := dispatcher.ResolveRedirectForTest(ctx, d, tc.uidB)
		if err != nil {
			t.Fatalf("%s: ResolveRedirect(B): %v", tc.name, err)
		}

		if gotA != gotB {
			t.Errorf("%s: cycle resolution diverged: resolveRedirect(A)=%q resolveRedirect(B)=%q",
				tc.name, gotA, gotB)
		}
		if gotA != wantSurvivor {
			t.Errorf("%s: cycle survivor = %q, want %q (MIN of cycle)", tc.name, gotA, wantSurvivor)
		}
	}
}

// TestEntityMerge_Chain_AllOrders verifies that the chain A→B, B→C applied in
// both orders converges: resolveRedirect(A) = C and resolveRedirect(B) = C
// regardless of which merge event arrives first.
//
// A single dispatcher is shared across both order trials; each trial uses
// unique entity and event UIDs so the redirect chains are independent within
// the same SQLite database. This avoids one extra KuzuDB open per trial.
func TestEntityMerge_Chain_AllOrders(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: opens a KuzuDB dispatcher for chain-convergence proof")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	// Each trial uses unique entity UIDs so the redirect chains are independent
	// within the shared dispatcher's SQLite database.
	trials := []struct {
		name string
		uidA string
		uidB string
		uidC string
		evAB string
		evBC string
		evs  func(ab, bc kernel.Event) []kernel.Event
	}{
		{
			name: "AB then BC",
			uidA: "01HCHAIN0AB00000000000000A",
			uidB: "01HCHAIN0AB00000000000000B",
			uidC: "01HCHAIN0AB00000000000000C",
			evAB: "01HCEVT0AB000000000000001A",
			evBC: "01HCEVT0AB000000000000001B",
			evs:  func(ab, bc kernel.Event) []kernel.Event { return []kernel.Event{ab, bc} },
		},
		{
			name: "BC then AB",
			uidA: "01HCHAIN0BC00000000000000A",
			uidB: "01HCHAIN0BC00000000000000B",
			uidC: "01HCHAIN0BC00000000000000C",
			evAB: "01HCEVT0BC000000000000002A",
			evBC: "01HCEVT0BC000000000000002B",
			evs:  func(ab, bc kernel.Event) []kernel.Event { return []kernel.Event{bc, ab} },
		},
	}

	for _, tc := range trials {
		eventAB := makeMergeEvent(t, tc.evAB, tc.uidA, tc.uidB)
		eventBC := makeMergeEvent(t, tc.evBC, tc.uidB, tc.uidC)
		evs := tc.evs(eventAB, eventBC)

		if err := d.Apply(ctx, evs); err != nil {
			t.Fatalf("%s: Apply: %v", tc.name, err)
		}

		gotA, err := dispatcher.ResolveRedirectForTest(ctx, d, tc.uidA)
		if err != nil {
			t.Fatalf("%s: ResolveRedirect(A): %v", tc.name, err)
		}
		gotB, err := dispatcher.ResolveRedirectForTest(ctx, d, tc.uidB)
		if err != nil {
			t.Fatalf("%s: ResolveRedirect(B): %v", tc.name, err)
		}
		gotC, err := dispatcher.ResolveRedirectForTest(ctx, d, tc.uidC)
		if err != nil {
			t.Fatalf("%s: ResolveRedirect(C): %v", tc.name, err)
		}

		// C is the chain endpoint with no further redirect.
		if gotC != tc.uidC {
			t.Errorf("%s: resolveRedirect(C) = %q, want %q (no redirect)", tc.name, gotC, tc.uidC)
		}
		if gotA != tc.uidC {
			t.Errorf("%s: resolveRedirect(A) = %q, want %q (chain end)", tc.name, gotA, tc.uidC)
		}
		if gotB != tc.uidC {
			t.Errorf("%s: resolveRedirect(B) = %q, want %q (chain end)", tc.name, gotB, tc.uidC)
		}
	}
}

// TestEntityMerge_FactLandsOnSurvivor verifies the key late-arriving-write
// property: a fact event whose subjectUID references a merged-away entity
// lands on the surviving (target) entity instead. This exercises the
// resolveRedirect call inside writeFactRow. Entity rows are NOT seeded:
// entity_redirects has no FK to entities, and applyEntityMerged handles
// missing entity rows as no-ops throughout, so the redirect is recorded and
// the subsequent fact is rerouted solely by the entity_redirects lookup.
func TestEntityMerge_FactLandsOnSurvivor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: opens a KuzuDB dispatcher for merge-then-fact-write proof")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)

	const (
		srcUID = "01HFACT0SRC000000000000001"
		tgtUID = "01HFACT0TGT000000000000002"
	)

	d := newDispatcher(t, ctx, nil)

	// Apply the merge event without pre-seeding entity rows. applyEntityMerged
	// records the redirect and skips DELETE/graph-detach as no-ops.
	mergeEv := makeMergeEvent(t, ulid.Make().String(), srcUID, tgtUID)
	if err := d.Apply(ctx, []kernel.Event{mergeEv}); err != nil {
		t.Fatalf("Apply merge: %v", err)
	}

	// Write a fact that references the merged-away source. writeFactRow calls
	// resolveRedirect which follows entity_redirects to the target.
	factUID := newFactULID(t)
	factEv := makeFactEventForSubject(t, factUID, srcUID, "state.role", "engineer", time.Now().UTC())
	if err := d.Apply(ctx, []kernel.Event{factEv}); err != nil {
		t.Fatalf("Apply fact: %v", err)
	}

	// The fact's subject_uid must have been resolved to tgtUID.
	gotSubject, err := dispatcher.FactSubjectForTest(ctx, d, factUID)
	if err != nil {
		t.Fatalf("FactSubjectForTest: %v", err)
	}
	if gotSubject != tgtUID {
		t.Errorf("fact subject_uid = %q, want %q (survivor after merge)", gotSubject, tgtUID)
	}
}

// TestEntityMerge_ShuffledChain_Converges exercises many random arrival orders
// of a 3-merge chain (A→B, B→C, C→D) and asserts that resolveRedirect returns
// D for every source in every shuffle. A single dispatcher is shared across all
// trials; each trial uses unique entity and event UIDs so the redirect chains
// are independent within the same SQLite database. Entity rows are not seeded
// — entity_redirects has no FK constraint — so each trial is pure SQLite and
// runs without KuzuDB write overhead.
func TestEntityMerge_ShuffledChain_Converges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: 40-trial shuffled-chain permutation test (~42s)")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	rng := rand.New(rand.NewPCG(0xABCDEF01, 0x12345678))
	for trial := range 40 {
		// Unique entity and event UIDs per trial: each trial is an independent
		// redirect chain within the shared dispatcher's SQLite database.
		uidA := fmt.Sprintf("01HSHMCA%018d", trial)
		uidB := fmt.Sprintf("01HSHMCB%018d", trial)
		uidC := fmt.Sprintf("01HSHMCC%018d", trial)
		uidD := fmt.Sprintf("01HSHMCD%018d", trial)
		evAB := fmt.Sprintf("01HEVTAB%018d", trial)
		evBC := fmt.Sprintf("01HEVTBC%018d", trial)
		evCD := fmt.Sprintf("01HEVTCD%018d", trial)

		events := []kernel.Event{
			makeMergeEvent(t, evAB, uidA, uidB),
			makeMergeEvent(t, evBC, uidB, uidC),
			makeMergeEvent(t, evCD, uidC, uidD),
		}
		shuffled := make([]kernel.Event, len(events))
		copy(shuffled, events)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		if err := d.Apply(ctx, shuffled); err != nil {
			t.Fatalf("trial %d Apply: %v", trial, err)
		}

		for _, uid := range []string{uidA, uidB, uidC} {
			got, err := dispatcher.ResolveRedirectForTest(ctx, d, uid)
			if err != nil {
				t.Fatalf("trial %d ResolveRedirect(%s): %v", trial, uid, err)
			}
			if got != uidD {
				t.Errorf("trial %d: resolveRedirect(%s) = %q, want %q", trial, uid, got, uidD)
			}
		}
		// D itself has no redirect and must resolve to itself.
		gotD, err := dispatcher.ResolveRedirectForTest(ctx, d, uidD)
		if err != nil {
			t.Fatalf("trial %d ResolveRedirect(D): %v", trial, err)
		}
		if gotD != uidD {
			t.Errorf("trial %d: resolveRedirect(D) = %q, want %q (no redirect)", trial, gotD, uidD)
		}
	}
}
