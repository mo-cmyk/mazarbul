package dispatcher_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// seedGenesisTeam admits a genesis owner (and any extra members) so the
// team-scope retraction authority has a GenesisOwner to consult. The lowest
// ULID self-grant is the genesis root (see ontology.ReduceMembership); each
// extra member is granted by the genesis owner.
func seedGenesisTeam(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, genesis string, members ...string) {
	t.Helper()
	events := make([]kernel.Event, 0, 1+len(members))
	events = append(events, grantEvent(t, "01EVTEAM00000000000GENES01", genesis, genesis, 0))
	for i, m := range members {
		uid := "01EVTEAM0000000000MEMBER0" + string(rune('A'+i))
		events = append(events, grantEvent(t, uid, genesis, m, 0))
	}
	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("seed genesis team: %v", err)
	}
}

// teamRelationEvent builds a team-scope KindRelation event authored by the
// given principal, so relations.asserted_by_principal records that author.
func teamRelationEvent(t *testing.T, eventUID, author, relUID, subjUID, objUID string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.Relation{
		UID:            relUID,
		SubjectUID:     subjUID,
		Predicate:      "works_at",
		ObjectUID:      objUID,
		Confidence:     1.0,
		SourceEventUID: ulid.Make().String(),
	})
	if err != nil {
		t.Fatalf("marshal relation: %v", err)
	}
	return kernel.Event{
		UID:        eventUID,
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "team",
		Confidence: 1.0,
		Scope:      kernel.ScopeTeam,
		CreateTime: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
		Author:     kernel.EventAuthor{PrincipalUID: author, DeviceID: author + "-dev", SigKeyID: "kid:" + author},
	}
}

// teamRetractedEvent builds a team-scope relationRetracted event authored by
// the given principal closing relUID with supersederUID at retractedAt.
func teamRetractedEvent(t *testing.T, eventUID, author, relUID, supersederUID string, retractedAt time.Time) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.RelationRetractedEvent{
		RelationUID:     ontology.ULID(relUID),
		SupersededByUID: ontology.ULID(supersederUID),
		RetractedAt:     retractedAt,
	})
	if err != nil {
		t.Fatalf("marshal retracted: %v", err)
	}
	return kernel.Event{
		UID:        eventUID,
		Kind:       kernel.EventKind(ontology.KindRelationRetracted),
		Source:     "team",
		Confidence: 1.0,
		Scope:      kernel.ScopeTeam,
		CreateTime: retractedAt,
		Payload:    payload,
		Author:     kernel.EventAuthor{PrincipalUID: author, DeviceID: author + "-dev", SigKeyID: "kid:" + author},
	}
}

// TestApply_TeamRetraction_AuthorityClosesByAsserterOrGenesis pins the core
// #213 D3 authority rule: a team-scope relation closes only when the
// retraction's signed author is the original asserter or the genesis owner; an
// unrelated admitted member's tombstone is recorded but never closes the
// relation.
func TestApply_TeamRetraction_AuthorityClosesByAsserterOrGenesis(t *testing.T) {
	t.Parallel()

	const (
		genesis  = "01PRINCIPAL0000000000GENES1"
		asserter = "01PRINCIPAL000000000ASSERT1"
		stranger = "01PRINCIPAL00000000STRANGE1"
		subjUID  = "01HTEAMRET0000000000SUBJ001"
		objUID   = "01HTEAMRET00000000000OBJ001"
		relUID   = "01HTEAMRET00000000000REL001"
		superID  = "01HTEAMRET0000000000SUPER01"
	)
	retractedAt := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	wantVU := retractedAt.UTC().Format(time.RFC3339Nano)

	// seed builds a fresh dispatcher with a team {genesis, asserter, stranger},
	// the endpoint entities, and a team-scope relation asserted by `asserter`.
	seed := func(t *testing.T, ctx context.Context) *dispatcher.Dispatcher {
		d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
		seedGenesisTeam(t, ctx, d, genesis, asserter, stranger)
		retractTestEntity(t, ctx, d, subjUID, "Subject")
		retractTestEntity(t, ctx, d, objUID, "Object")
		if err := d.Apply(ctx, []kernel.Event{
			teamRelationEvent(t, "01HTEAMRET000000000RELEV01", asserter, relUID, subjUID, objUID),
		}); err != nil {
			t.Fatalf("apply team relation: %v", err)
		}
		return d
	}

	t.Run("closes by asserter", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.TestContext(t)
		d := seed(t, ctx)
		if err := d.Apply(ctx, []kernel.Event{
			teamRetractedEvent(t, "01HTEAMRET0000000RETBYASR1", asserter, relUID, superID, retractedAt),
		}); err != nil {
			t.Fatalf("apply asserter retraction: %v", err)
		}
		vu, sb, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
		if err != nil {
			t.Fatalf("read close: %v", err)
		}
		if sb != superID || vu != wantVU {
			t.Errorf("asserter retraction did not close: vu=%q sb=%q, want vu=%q sb=%q", vu, sb, wantVU, superID)
		}
	})

	t.Run("closes by genesis owner", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.TestContext(t)
		d := seed(t, ctx)
		if err := d.Apply(ctx, []kernel.Event{
			teamRetractedEvent(t, "01HTEAMRET0000000RETBYGEN1", genesis, relUID, superID, retractedAt),
		}); err != nil {
			t.Fatalf("apply genesis retraction: %v", err)
		}
		vu, sb, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
		if err != nil {
			t.Fatalf("read close: %v", err)
		}
		if sb != superID || vu != wantVU {
			t.Errorf("genesis retraction did not close: vu=%q sb=%q, want vu=%q sb=%q", vu, sb, wantVU, superID)
		}
	})

	t.Run("unauthorized member does not close but tombstone recorded", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.TestContext(t)
		d := seed(t, ctx)
		const retUID = "01HTEAMRET0000000RETBYSTR1"
		if err := d.Apply(ctx, []kernel.Event{
			teamRetractedEvent(t, retUID, stranger, relUID, superID, retractedAt),
		}); err != nil {
			t.Fatalf("apply stranger retraction: %v", err)
		}
		vu, sb, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
		if err != nil {
			t.Fatalf("read close: %v", err)
		}
		if vu != "" || sb != "" {
			t.Errorf("unauthorized retraction closed the relation: vu=%q sb=%q, want both empty", vu, sb)
		}
		// The tombstone is still durably recorded for audit.
		if got := tombstoneCount(t, ctx, d, relUID); got != 1 {
			t.Errorf("tombstone rows = %d, want 1 (recorded for audit even when unauthorized)", got)
		}
	})
}

// TestApply_TeamRetraction_OrderIndependent proves the authority filter is
// convergent: a relation asserted by A closes under A's retraction whether the
// retraction replays before or after the relation event, and an unrelated
// member's retraction never closes it in either order.
func TestApply_TeamRetraction_OrderIndependent(t *testing.T) {
	t.Parallel()

	const (
		genesis  = "01PRINCIPAL000000000OGENES1"
		asserter = "01PRINCIPAL00000000OASSERT1"
		stranger = "01PRINCIPAL0000000OSTRANGE1"
		subjUID  = "01HTEAMORD0000000000SUBJ001"
		objUID   = "01HTEAMORD00000000000OBJ001"
		relUID   = "01HTEAMORD00000000000REL001"
		superID  = "01HTEAMORD0000000000SUPER01"
	)
	retractedAt := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	wantVU := retractedAt.UTC().Format(time.RFC3339Nano)

	relEv := teamRelationEvent(t, "01HTEAMORD000000000RELEV01", asserter, relUID, subjUID, objUID)

	// run applies the team + entities, then the relation and retraction events in
	// the given order, and returns the materialized close.
	run := func(t *testing.T, retractor string, relationFirst bool) (string, string) {
		ctx := testutil.TestContext(t)
		d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
		seedGenesisTeam(t, ctx, d, genesis, asserter, stranger)
		retractTestEntity(t, ctx, d, subjUID, "Subject")
		retractTestEntity(t, ctx, d, objUID, "Object")
		ret := teamRetractedEvent(t, "01HTEAMORD000000000RETEV01", retractor, relUID, superID, retractedAt)
		order := []kernel.Event{relEv, ret}
		if !relationFirst {
			order = []kernel.Event{ret, relEv}
		}
		for _, ev := range order {
			if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
				t.Fatalf("apply: %v", err)
			}
		}
		vu, sb, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
		if err != nil {
			t.Fatalf("read close: %v", err)
		}
		return vu, sb
	}

	t.Run("authorized converges to closed either order", func(t *testing.T) {
		t.Parallel()
		vuFwd, sbFwd := run(t, asserter, true)
		vuRev, sbRev := run(t, asserter, false)
		if vuFwd != vuRev || sbFwd != sbRev {
			t.Fatalf("diverged: relation-first(vu=%q sb=%q) retraction-first(vu=%q sb=%q)",
				vuFwd, sbFwd, vuRev, sbRev)
		}
		if sbFwd != superID || vuFwd != wantVU {
			t.Errorf("authorized close = vu=%q sb=%q, want vu=%q sb=%q", vuFwd, sbFwd, wantVU, superID)
		}
	})

	t.Run("unauthorized stays open either order", func(t *testing.T) {
		t.Parallel()
		vuFwd, sbFwd := run(t, stranger, true)
		vuRev, sbRev := run(t, stranger, false)
		if vuFwd != vuRev || sbFwd != sbRev {
			t.Fatalf("diverged: relation-first(vu=%q sb=%q) retraction-first(vu=%q sb=%q)",
				vuFwd, sbFwd, vuRev, sbRev)
		}
		if vuFwd != "" || sbFwd != "" {
			t.Errorf("unauthorized close = vu=%q sb=%q, want both empty", vuFwd, sbFwd)
		}
	})
}

// TestApply_PersonalRetraction_StillCloses is the regression guard that the
// team authority filter never touches non-team retraction: a personal-scope
// relation closes under any retraction exactly as in #205, with no team seeded.
func TestApply_PersonalRetraction_StillCloses(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		subjUID = "01HPERSRET0000000000SUBJ001"
		objUID  = "01HPERSRET00000000000OBJ001"
		relUID  = "01HPERSRET00000000000REL001"
		superID = "01HPERSRET0000000000SUPER01"
	)
	retractTestEntity(t, ctx, d, subjUID, "Subject")
	retractTestEntity(t, ctx, d, objUID, "Object")
	// Personal-scope relation (the default-scope path retractTestRelation uses).
	retractTestRelation(t, ctx, d, relUID, subjUID, objUID)

	retractedAt := time.Date(2025, 8, 1, 0, 0, 0, 0, time.UTC)
	// A personal-scope retraction with an arbitrary author must still close.
	if err := d.Apply(ctx, []kernel.Event{
		makeRetractedEvent(t, "01HPERSRET000000000RETEV01", relUID, superID, retractedAt),
	}); err != nil {
		t.Fatalf("apply personal retraction: %v", err)
	}
	vu, sb, err := dispatcher.RelationCloseForTest(ctx, d, relUID)
	if err != nil {
		t.Fatalf("read close: %v", err)
	}
	if sb != superID {
		t.Errorf("personal retraction did not close: superseded_by_uid = %q, want %q", sb, superID)
	}
	if want := retractedAt.UTC().Format(time.RFC3339Nano); vu != want {
		t.Errorf("personal retraction valid_until = %q, want %q", vu, want)
	}
}

// tombstoneCount reports how many relation_retractions rows exist for relUID,
// so a test can assert an unauthorized tombstone is still durably recorded.
func tombstoneCount(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, relUID string) int {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var n int
	if err := reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relation_retractions WHERE relation_uid = ?`, relUID,
	).Scan(&n); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	return n
}
