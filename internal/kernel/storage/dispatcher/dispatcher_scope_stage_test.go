package dispatcher_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestApply_ScopePromoted_PromotesDraftEpisode is the ship gate for the
// promote path: a draft episode must be invisible to a {project,personal}
// read, then visible after a scopePromoted event is applied.
func TestApply_ScopePromoted_PromotesDraftEpisode(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const episodeUID = "01HZZZZZZZZZZZZZZZZZZDRF001"

	// Write a draft-scoped episode.
	ev := makeEpisodeEvent(t, episodeUID, "Frodo visits the Grey Havens.", "test")
	ev.Scope = kernel.ScopeDraft
	ev.CreateTime = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("seed draft Apply: %v", err)
	}

	// It must NOT appear in a default {project,personal} read.
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	nav, nerr := navigate.New(db)
	if nerr != nil {
		t.Fatalf("navigate.New: %v", nerr)
	}
	defaultNav := nav.WithScopes(kernel.DefaultReadSet())
	eps, lerr := defaultNav.ListEpisodes(ctx, navigate.EpisodeFilter{})
	if lerr != nil {
		t.Fatalf("ListEpisodes (default): %v", lerr)
	}
	for _, ep := range eps {
		if ep.UID == episodeUID {
			t.Fatalf("LEAK: draft episode %q surfaced through DefaultReadSet before promotion", episodeUID)
		}
	}

	// Also must NOT appear in AllScopes.
	allNav := nav.WithScopes(kernel.AllScopes())
	allEps, aerr := allNav.ListEpisodes(ctx, navigate.EpisodeFilter{})
	if aerr != nil {
		t.Fatalf("ListEpisodes (all): %v", aerr)
	}
	for _, ep := range allEps {
		if ep.UID == episodeUID {
			t.Fatalf("LEAK: draft episode %q surfaced through AllScopes before promotion", episodeUID)
		}
	}

	// Apply a scopePromoted event.
	promotedPayload, err := json.Marshal(ontology.ScopePromotedEvent{
		TargetUID: episodeUID,
		FromScope: string(kernel.ScopeDraft),
		ToScope:   string(kernel.ScopePersonal),
	})
	if err != nil {
		t.Fatalf("marshal scopePromoted: %v", err)
	}
	promoteEv := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZDRF002",
		Kind:       kernel.EventKind(ontology.KindScopePromoted),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		Payload:    promotedPayload,
	}
	if err := d.Apply(ctx, []kernel.Event{promoteEv}); err != nil {
		t.Fatalf("scopePromoted Apply: %v", err)
	}

	// NOW the episode must be visible in personal.
	personalNav := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal))
	personalEps, perr := personalNav.ListEpisodes(ctx, navigate.EpisodeFilter{})
	if perr != nil {
		t.Fatalf("ListEpisodes (personal after promote): %v", perr)
	}
	found := false
	for _, ep := range personalEps {
		if ep.UID == episodeUID {
			found = true
		}
	}
	if !found {
		t.Errorf("promoted episode %q not found in personal scope: %v", episodeUID, episodeUIDs(personalEps))
	}

	// And must NOT still be in draft.
	draftNav := nav.WithScopes(kernel.ScopeDraftReadSet())
	draftEps, derr := draftNav.ListEpisodes(ctx, navigate.EpisodeFilter{})
	if derr != nil {
		t.Fatalf("ListEpisodes (draft after promote): %v", derr)
	}
	for _, ep := range draftEps {
		if ep.UID == episodeUID {
			t.Errorf("episode %q still in draft scope after promotion", episodeUID)
		}
	}
}

// TestApply_ScopePromoted_IsIdempotent proves replaying the same scopePromoted
// event twice is a no-op (idempotent via WHERE scope='draft').
func TestApply_ScopePromoted_IsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const episodeUID = "01HZZZZZZZZZZZZZZZZZZDRF003"
	ev := makeEpisodeEvent(t, episodeUID, "Idempotent test episode.", "test")
	ev.Scope = kernel.ScopeDraft
	ev.CreateTime = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	promotedPayload, err := json.Marshal(ontology.ScopePromotedEvent{
		TargetUID: episodeUID,
		FromScope: string(kernel.ScopeDraft),
		ToScope:   string(kernel.ScopePersonal),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	promoteEv := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZDRF004",
		Kind:       kernel.EventKind(ontology.KindScopePromoted),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 1, 12, 1, 0, 0, time.UTC),
		Payload:    promotedPayload,
	}
	if err := d.Apply(ctx, []kernel.Event{promoteEv}); err != nil {
		t.Fatalf("first promote Apply: %v", err)
	}
	// Replay the same event — must not error.
	if err := d.Apply(ctx, []kernel.Event{promoteEv}); err != nil {
		t.Fatalf("second promote Apply (replay): %v", err)
	}

	// Still visible in personal.
	db, oerr := d.OpenSQLite(ctx)
	if oerr != nil {
		t.Fatalf("OpenSQLite: %v", oerr)
	}
	nav, nerr := navigate.New(db)
	if nerr != nil {
		t.Fatalf("navigate.New: %v", nerr)
	}
	if _, gerr := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal)).GetEpisode(ctx, episodeUID); gerr != nil {
		t.Errorf("episode not visible in personal after idempotent replay: %v", gerr)
	}
}

// TestApply_ScopePromoted_SecurityGate_NonDraftFromScopeIsNoop proves the
// hard security gate: a scopePromoted event with fromScope != "draft" must
// not change any scope column.
func TestApply_ScopePromoted_SecurityGate_NonDraftFromScopeIsNoop(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const episodeUID = "01HZZZZZZZZZZZZZZZZZZDRF005"
	// Write a personal-scoped episode (not draft).
	ev := makeEpisodeEvent(t, episodeUID, "Personal episode, must not be demoted.", "test")
	ev.Scope = kernel.ScopePersonal
	ev.CreateTime = time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	// Attempt to "promote" from team to personal — the security gate must ignore this.
	attackPayload, err := json.Marshal(ontology.ScopePromotedEvent{
		TargetUID: episodeUID,
		FromScope: "team", // NOT draft — must be rejected
		ToScope:   "project",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	attackEv := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZDRF006",
		Kind:       kernel.EventKind(ontology.KindScopePromoted),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 1, 13, 1, 0, 0, time.UTC),
		Payload:    attackPayload,
	}
	// Must not return an error (silent no-op is the contract).
	if err := d.Apply(ctx, []kernel.Event{attackEv}); err != nil {
		t.Fatalf("security gate Apply returned error (want no-op): %v", err)
	}

	// The episode must still be in personal, not moved.
	db, oerr := d.OpenSQLite(ctx)
	if oerr != nil {
		t.Fatalf("OpenSQLite: %v", oerr)
	}
	nav, nerr := navigate.New(db)
	if nerr != nil {
		t.Fatalf("navigate.New: %v", nerr)
	}
	if _, gerr := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal)).GetEpisode(ctx, episodeUID); gerr != nil {
		t.Errorf("episode left personal scope after security-gate no-op: %v", gerr)
	}
	// Must NOT appear in project.
	if _, gerr := nav.WithScopes(kernel.NewScopeSet(kernel.ScopeProject)).GetEpisode(ctx, episodeUID); gerr == nil {
		t.Errorf("SECURITY LEAK: episode moved to project after security-gate attack")
	}
}

// TestApply_ScopeDiscarded_RemovesDraftEpisode proves the discard path removes
// the draft episode from the projection while leaving durable episodes intact.
func TestApply_ScopeDiscarded_RemovesDraftEpisode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: heavy dispatcher+KuzuDB setup (~25s)")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const draftUID = "01HZZZZZZZZZZZZZZZZZZDRF007"
	const personalUID = "01HZZZZZZZZZZZZZZZZZZDRF008"

	// Seed both a draft and a personal episode.
	draftEv := makeEpisodeEvent(t, draftUID, "Draft to discard.", "test")
	draftEv.Scope = kernel.ScopeDraft
	draftEv.CreateTime = time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)

	personalEv := makeEpisodeEvent(t, personalUID, "Personal, must survive.", "test")
	personalEv.Scope = kernel.ScopePersonal
	personalEv.CreateTime = time.Date(2026, 6, 1, 14, 1, 0, 0, time.UTC)

	if err := d.Apply(ctx, []kernel.Event{draftEv, personalEv}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	discardPayload, err := json.Marshal(ontology.ScopeDiscardedEvent{
		TargetUID: draftUID,
		Reason:    "test discard",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	discardEv := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZDRF009",
		Kind:       kernel.EventKind(ontology.KindScopeDiscarded),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 1, 14, 2, 0, 0, time.UTC),
		Payload:    discardPayload,
	}
	if err := d.Apply(ctx, []kernel.Event{discardEv}); err != nil {
		t.Fatalf("scopeDiscarded Apply: %v", err)
	}

	db, oerr := d.OpenSQLite(ctx)
	if oerr != nil {
		t.Fatalf("OpenSQLite: %v", oerr)
	}
	nav, nerr := navigate.New(db)
	if nerr != nil {
		t.Fatalf("navigate.New: %v", nerr)
	}

	// Draft episode must be gone.
	draftNav := nav.WithScopes(kernel.ScopeDraftReadSet())
	if _, gerr := draftNav.GetEpisode(ctx, draftUID); gerr == nil {
		t.Errorf("discarded draft episode %q still present in draft scope", draftUID)
	}

	// Personal episode must survive.
	personalNav := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal))
	if _, gerr := personalNav.GetEpisode(ctx, personalUID); gerr != nil {
		t.Errorf("personal episode %q disappeared after unrelated discard: %v", personalUID, gerr)
	}
}

// TestApply_ScopeDiscarded_IsIdempotent ensures replaying a scopeDiscarded
// event twice does not error.
func TestApply_ScopeDiscarded_IsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const draftUID = "01HZZZZZZZZZZZZZZZZZZDRF010"
	draftEv := makeEpisodeEvent(t, draftUID, "Idempotent discard test.", "test")
	draftEv.Scope = kernel.ScopeDraft
	draftEv.CreateTime = time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)
	if err := d.Apply(ctx, []kernel.Event{draftEv}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}

	discardPayload, err := json.Marshal(ontology.ScopeDiscardedEvent{TargetUID: draftUID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	discardEv := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZDRF011",
		Kind:       kernel.EventKind(ontology.KindScopeDiscarded),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 1, 15, 1, 0, 0, time.UTC),
		Payload:    discardPayload,
	}
	if err := d.Apply(ctx, []kernel.Event{discardEv}); err != nil {
		t.Fatalf("first discard Apply: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{discardEv}); err != nil {
		t.Fatalf("second discard Apply (replay): %v", err)
	}
}

// TestApply_ScopePromoted_RejectsTeamToScope is the F3 security gate: a
// scopePromoted whose toScope is "team" must be a silent no-op (no error, no
// write). Promoting unreviewed draft data straight into a shared team scope is
// the highest-risk leak; team writes must go through the explicit grant path.
func TestApply_ScopePromoted_RejectsTeamToScope(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const episodeUID = "01HZZZZZZZZZZZZZZZZZZDRF020"
	ev := makeEpisodeEvent(t, episodeUID, "Draft must not reach team scope.", "test")
	ev.Scope = kernel.ScopeDraft
	ev.CreateTime = time.Date(2026, 6, 1, 16, 0, 0, 0, time.UTC)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("seed draft Apply: %v", err)
	}

	// Attempt to promote the draft straight into team — must be ignored.
	attackPayload, err := json.Marshal(ontology.ScopePromotedEvent{
		TargetUID: episodeUID,
		FromScope: string(kernel.ScopeDraft),
		ToScope:   string(kernel.ScopeTeam),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	attackEv := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZDRF021",
		Kind:       kernel.EventKind(ontology.KindScopePromoted),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 1, 16, 1, 0, 0, time.UTC),
		Payload:    attackPayload,
	}
	// Must NOT error (a returned error would abort the whole dispatch loop).
	if err := d.Apply(ctx, []kernel.Event{attackEv}); err != nil {
		t.Fatalf("team-promotion Apply returned error (want silent no-op): %v", err)
	}

	db, oerr := d.OpenSQLite(ctx)
	if oerr != nil {
		t.Fatalf("OpenSQLite: %v", oerr)
	}
	nav, nerr := navigate.New(db)
	if nerr != nil {
		t.Fatalf("navigate.New: %v", nerr)
	}

	// The episode must NOT be visible in team scope.
	if _, gerr := nav.WithScopes(kernel.NewScopeSet(kernel.ScopeTeam)).GetEpisode(ctx, episodeUID); gerr == nil {
		t.Errorf("SECURITY LEAK: draft episode %q reached team scope", episodeUID)
	}
	// It must still be in draft (untouched).
	if _, gerr := nav.WithScopes(kernel.ScopeDraftReadSet()).GetEpisode(ctx, episodeUID); gerr != nil {
		t.Errorf("draft episode %q left draft scope after rejected team promotion: %v", episodeUID, gerr)
	}
}

// TestApply_ScopeDiscarded_RemovesDraftEntity is the F2 gate for the entity
// cascade: a scopeDiscarded targeting a draft entity must delete the entity row
// (and its vector sidecar), leaving no orphaned draft data.
func TestApply_ScopeDiscarded_RemovesDraftEntity(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		draftEntityUID    = "01HZZZZZZZZZZZZZZZZZZDRF022"
		personalEntityUID = "01HZZZZZZZZZZZZZZZZZZDRF023"
	)

	// Seed a draft entity and a personal entity (the personal one must survive).
	seedScopedEntity(t, ctx, d, draftEntityUID, "Draft Person", kernel.ScopeDraft)
	seedScopedEntity(t, ctx, d, personalEntityUID, "Real Person", kernel.ScopePersonal)

	discardPayload, err := json.Marshal(ontology.ScopeDiscardedEvent{TargetUID: draftEntityUID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	discardEv := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZDRF024",
		Kind:       kernel.EventKind(ontology.KindScopeDiscarded),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 1, 17, 0, 0, 0, time.UTC),
		Payload:    discardPayload,
	}
	if err := d.Apply(ctx, []kernel.Event{discardEv}); err != nil {
		t.Fatalf("scopeDiscarded Apply: %v", err)
	}

	db, oerr := d.OpenSQLite(ctx)
	if oerr != nil {
		t.Fatalf("OpenSQLite: %v", oerr)
	}

	// Draft entity row must be gone.
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM entities WHERE uid = ?`, draftEntityUID); got != 0 {
		t.Errorf("discarded draft entity %q still present (%d rows)", draftEntityUID, got)
	}
	// Its vector sidecar must be gone too (no orphan).
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM entities_vec_ids WHERE id = ?`, draftEntityUID); got != 0 {
		t.Errorf("orphaned vector sidecar for discarded entity %q (%d rows)", draftEntityUID, got)
	}
	// Personal entity must survive.
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM entities WHERE uid = ?`, personalEntityUID); got != 1 {
		t.Errorf("personal entity %q disappeared after unrelated discard (%d rows)", personalEntityUID, got)
	}
}

// TestApply_ScopeDiscarded_PreservesPromotedEpisodeProvenance is the F2 gate for
// the provenance DELETE: once a draft episode is promoted (now personal), a stray
// scopeDiscarded naming the same uid must NOT strip the live episode's
// provenance. The provenance delete is gated on the target still being a draft
// episode.
func TestApply_ScopeDiscarded_PreservesPromotedEpisodeProvenance(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const episodeUID = "01HZZZZZZZZZZZZZZZZZZDRF025"
	ev := makeEpisodeEvent(t, episodeUID, "Draft that gets promoted then mis-discarded.", "test")
	ev.Scope = kernel.ScopeDraft
	ev.CreateTime = time.Date(2026, 6, 1, 18, 0, 0, 0, time.UTC)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("seed draft Apply: %v", err)
	}

	// Promote it to personal.
	promotePayload, err := json.Marshal(ontology.ScopePromotedEvent{
		TargetUID: episodeUID,
		FromScope: string(kernel.ScopeDraft),
		ToScope:   string(kernel.ScopePersonal),
	})
	if err != nil {
		t.Fatalf("marshal promote: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        "01HZZZZZZZZZZZZZZZZZZDRF026",
		Kind:       kernel.EventKind(ontology.KindScopePromoted),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 1, 18, 1, 0, 0, time.UTC),
		Payload:    promotePayload,
	}}); err != nil {
		t.Fatalf("promote Apply: %v", err)
	}

	db, oerr := d.OpenSQLite(ctx)
	if oerr != nil {
		t.Fatalf("OpenSQLite: %v", oerr)
	}
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM provenance WHERE event_uid = ?`, episodeUID); got == 0 {
		t.Fatalf("precondition: promoted episode %q has no provenance row to protect", episodeUID)
	}

	// Now a stray discard names the same (now personal) uid.
	discardPayload, err := json.Marshal(ontology.ScopeDiscardedEvent{TargetUID: episodeUID})
	if err != nil {
		t.Fatalf("marshal discard: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        "01HZZZZZZZZZZZZZZZZZZDRF027",
		Kind:       kernel.EventKind(ontology.KindScopeDiscarded),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 1, 18, 2, 0, 0, time.UTC),
		Payload:    discardPayload,
	}}); err != nil {
		t.Fatalf("stray discard Apply: %v", err)
	}

	// Provenance must survive (gate held), and the episode is still personal.
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM provenance WHERE event_uid = ?`, episodeUID); got == 0 {
		t.Errorf("SECURITY BUG: stray discard stripped provenance of promoted episode %q", episodeUID)
	}
	nav, nerr := navigate.New(db)
	if nerr != nil {
		t.Fatalf("navigate.New: %v", nerr)
	}
	if _, gerr := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal)).GetEpisode(ctx, episodeUID); gerr != nil {
		t.Errorf("promoted episode %q disappeared after stray discard: %v", episodeUID, gerr)
	}
}

// TestApply_ScopeDiscarded_RemovesDraftEntityGraphNode proves discard also drops
// the :Entity graph node for a discarded draft entity. The graph is NOT
// scope-filtered, so a lingering node would remain reachable via traverse /
// neighbors — a leak of explicitly-discarded content.
func TestApply_ScopeDiscarded_RemovesDraftEntityGraphNode(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const draftEntityUID = "01HZZZZZZZZZZZZZZZZZZDRF030"
	seedScopedEntity(t, ctx, d, draftEntityUID, "Graph Draft Person", kernel.ScopeDraft)

	// Precondition: seeding an Entity event materializes an :Entity graph node.
	if got := graphEntityNodeCount(t, ctx, d, draftEntityUID); got != 1 {
		t.Fatalf("precondition: draft entity graph node count = %d, want 1", got)
	}

	discardPayload, err := json.Marshal(ontology.ScopeDiscardedEvent{TargetUID: draftEntityUID})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        "01HZZZZZZZZZZZZZZZZZZDRF031",
		Kind:       kernel.EventKind(ontology.KindScopeDiscarded),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 1, 19, 0, 0, 0, time.UTC),
		Payload:    discardPayload,
	}}); err != nil {
		t.Fatalf("scopeDiscarded Apply: %v", err)
	}

	if got := graphEntityNodeCount(t, ctx, d, draftEntityUID); got != 0 {
		t.Errorf("discarded draft entity %q still has a graph node (count=%d)", draftEntityUID, got)
	}
}

// graphEntityNodeCount returns the number of :Entity graph nodes with the uid.
func graphEntityNodeCount(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid string) int64 {
	t.Helper()
	g, err := d.OpenGraph(ctx)
	if err != nil {
		t.Fatalf("OpenGraph: %v", err)
	}
	r, err := g.Execute(ctx, "MATCH (e:Entity {uid: $uid}) RETURN count(e)", map[string]any{"uid": uid})
	if err != nil {
		t.Fatalf("graph execute: %v", err)
	}
	defer func() { _ = r.Close() }()
	var n int64
	if r.Next() {
		if serr := r.Scan(&n); serr != nil {
			t.Fatalf("scan graph count: %v", serr)
		}
	} else if ierr := r.Err(); ierr != nil {
		t.Fatalf("graph iter: %v", ierr)
	}
	return n
}

// seedScopedEntity applies an Entity event at the given scope so draft-cascade
// tests can stage entities directly.
func seedScopedEntity(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, uid, name string, scope kernel.Scope) {
	t.Helper()
	payload, err := json.Marshal(ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      name,
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal entity: %v", err)
	}
	ev := kernel.Event{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindEntity),
		Source:     "resolver/v1",
		Confidence: 1.0,
		Scope:      scope,
		CreateTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("apply entity %s: %v", uid, err)
	}
}

// countRows runs a COUNT(*) query and returns the single integer result.
func countRows(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("countRows %q: %v", query, err)
	}
	return n
}

// episodeUIDs extracts UIDs from a Navigate episode slice for error messages.
func episodeUIDs(eps []navigate.Episode) []string {
	out := make([]string, len(eps))
	for i, ep := range eps {
		out[i] = ep.UID
	}
	return out
}

// seedDraftEpisodeWithDerivedRows seeds a draft episode and simulates what
// resolveInScope now produces: draft-scoped Mention events (linking entities to
// the episode via source_event_uid), draft-scoped EntityCreated events (with
// MentionUIDs referencing the mention events so backfillMentionEntityUID stamps
// entity_uid into the mentions rows), and a draft-scoped Relation event.
//
// Returns the mention UIDs so callers can verify the link columns.
func seedDraftEpisodeWithDerivedRows(
	t *testing.T,
	ctx context.Context,
	d *dispatcher.Dispatcher,
	episodeUID, subjUID, objUID, relUID string,
) (mentionUID1, mentionUID2 string) {
	t.Helper()
	ts := time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)

	// Draft episode.
	epEv := makeEpisodeEvent(t, episodeUID, "Saruman rules Isengard.", "test")
	epEv.Scope = kernel.ScopeDraft
	epEv.CreateTime = ts
	if err := d.Apply(ctx, []kernel.Event{epEv}); err != nil {
		t.Fatalf("seed draft episode: %v", err)
	}

	// Draft Mention events — source_event_uid = episodeUID so the discard and
	// promote cascades can find them and their linked entities.
	mentionUID1 = ulid.Make().String()
	mentionUID2 = ulid.Make().String()

	m1Payload, err := json.Marshal(ontology.Mention{
		Kind:           ontology.EntityKindPerson,
		DisplayName:    "Saruman",
		SourceEventUID: ontology.ULID(episodeUID),
		ExtractionText: "Saruman",
	})
	if err != nil {
		t.Fatalf("marshal mention 1: %v", err)
	}
	m2Payload, err := json.Marshal(ontology.Mention{
		Kind:           ontology.EntityKindPlace,
		DisplayName:    "Isengard",
		SourceEventUID: ontology.ULID(episodeUID),
		ExtractionText: "Isengard",
	})
	if err != nil {
		t.Fatalf("marshal mention 2: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{
		{
			UID:        mentionUID1,
			Kind:       kernel.EventKind(ontology.KindMention),
			Source:     "extractor/v1",
			Scope:      kernel.ScopeDraft,
			Confidence: 0.9,
			CreateTime: ts,
			Payload:    m1Payload,
		},
		{
			UID:        mentionUID2,
			Kind:       kernel.EventKind(ontology.KindMention),
			Source:     "extractor/v1",
			Scope:      kernel.ScopeDraft,
			Confidence: 0.9,
			CreateTime: ts,
			Payload:    m2Payload,
		},
	}); err != nil {
		t.Fatalf("apply draft mentions: %v", err)
	}

	// Draft EntityCreated events with MentionUIDs — backfillMentionEntityUID
	// stamps entity_uid onto the mention rows so the discard/promote cascade
	// can find entities via the mentions subquery.
	subjPayload, err := json.Marshal(ontology.Entity{
		Name:             "entities/" + subjUID,
		UID:              subjUID,
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Saruman",
		Confidence:       0.9,
		ResolutionStatus: ontology.ResolutionConfirmed,
		MentionUIDs:      []ontology.ULID{ontology.ULID(mentionUID1)},
		CreateTime:       ts,
	})
	if err != nil {
		t.Fatalf("marshal subject entity: %v", err)
	}
	objPayload, err := json.Marshal(ontology.Entity{
		Name:             "entities/" + objUID,
		UID:              objUID,
		Kind:             ontology.EntityKindPlace,
		DisplayName:      "Isengard",
		Confidence:       0.9,
		ResolutionStatus: ontology.ResolutionConfirmed,
		MentionUIDs:      []ontology.ULID{ontology.ULID(mentionUID2)},
		CreateTime:       ts,
	})
	if err != nil {
		t.Fatalf("marshal object entity: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{
		{
			UID:        ulid.Make().String(),
			Kind:       kernel.EventKind(ontology.KindEntity),
			Source:     "resolver/v1",
			Scope:      kernel.ScopeDraft,
			Confidence: 0.9,
			CreateTime: ts,
			Payload:    subjPayload,
		},
		{
			UID:        ulid.Make().String(),
			Kind:       kernel.EventKind(ontology.KindEntity),
			Source:     "resolver/v1",
			Scope:      kernel.ScopeDraft,
			Confidence: 0.9,
			CreateTime: ts,
			Payload:    objPayload,
		},
	}); err != nil {
		t.Fatalf("apply draft entities: %v", err)
	}

	// Draft Relation event.
	relPayload, err := json.Marshal(ontology.Relation{
		Name:           "relations/" + relUID,
		UID:            relUID,
		SubjectUID:     subjUID,
		Predicate:      "rules",
		ObjectUID:      objUID,
		Confidence:     0.9,
		SourceEventUID: episodeUID,
		CreateTime:     ts,
		Status:         ontology.RelationStatusAsserted,
	})
	if err != nil {
		t.Fatalf("marshal relation: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        ulid.Make().String(),
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "resolver/v1",
		Scope:      kernel.ScopeDraft,
		Confidence: 0.9,
		CreateTime: ts,
		Payload:    relPayload,
	}}); err != nil {
		t.Fatalf("apply draft relation: %v", err)
	}

	return mentionUID1, mentionUID2
}

// TestDraftScope_DerivedEventsAreScoped is the core ship gate for the draft-scope
// privacy fix. It proves three properties without any LLM or Ollama call:
//
//  1. Draft-scoped entity and relation events produce draft-scoped projection rows
//     (no personal-scope leak): the resolver now stamps Scope=draft on events it
//     mints from a draft episode.
//  2. Draft-scoped entities and relations are excluded from the default
//     {personal, project} read set.
//  3. applyScopeDiscarded removes draft entities, relations, and the episode with no
//     FK violation — the deletion completes atomically.
func TestDraftScope_DerivedEventsAreScoped(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		episodeUID = "01HZDRAFTSCOPE0000000EP001"
		subjUID    = "01HZDRAFTSCOPE0000000SB001"
		objUID     = "01HZDRAFTSCOPE0000000OB001"
		relUID     = "01HZDRAFTSCOPE0000000RL001"
	)

	seedDraftEpisodeWithDerivedRows(t, ctx, d, episodeUID, subjUID, objUID, relUID)

	db, oerr := d.OpenSQLite(ctx)
	if oerr != nil {
		t.Fatalf("OpenSQLite: %v", oerr)
	}

	// Property 1: entities and relations are draft-scoped in the projection.
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM entities WHERE uid = ? AND scope = 'draft'`, subjUID); got != 1 {
		t.Errorf("subject entity not draft-scoped: count=%d", got)
	}
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM entities WHERE uid = ? AND scope = 'draft'`, objUID); got != 1 {
		t.Errorf("object entity not draft-scoped: count=%d", got)
	}
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM relations WHERE uid = ? AND scope = 'draft'`, relUID); got != 1 {
		t.Errorf("relation not draft-scoped: count=%d", got)
	}

	// Property 2: excluded from the default {personal, project} read set.
	nav, nerr := navigate.New(db)
	if nerr != nil {
		t.Fatalf("navigate.New: %v", nerr)
	}
	defaultNav := nav.WithScopes(kernel.DefaultReadSet())
	ents, eerr := defaultNav.ListEntities(ctx, navigate.EntityFilter{})
	if eerr != nil {
		t.Fatalf("ListEntities (default): %v", eerr)
	}
	for _, e := range ents {
		if e.UID == subjUID || e.UID == objUID {
			t.Errorf("LEAK: draft entity %q visible in default read set before promotion", e.UID)
		}
	}

	t.Run("discard removes draft rows with no FK error", func(t *testing.T) {
		discardPayload, derr := json.Marshal(ontology.ScopeDiscardedEvent{TargetUID: episodeUID})
		if derr != nil {
			t.Fatalf("marshal: %v", derr)
		}
		discardEv := kernel.Event{
			UID:        "01HZDRAFTSCOPE0000000DC001",
			Kind:       kernel.EventKind(ontology.KindScopeDiscarded),
			Source:     "test",
			Confidence: 1.0,
			Scope:      kernel.ScopePersonal,
			CreateTime: time.Date(2026, 6, 10, 11, 0, 0, 0, time.UTC),
			Payload:    discardPayload,
		}
		// Must not error — the FK violation fix is that draft entities are now
		// draft-scoped, so the draft-gated DELETE finds and removes them before
		// the episode row is deleted.
		if aerr := d.Apply(ctx, []kernel.Event{discardEv}); aerr != nil {
			t.Fatalf("scopeDiscarded Apply: %v (FK cascade bug?)", aerr)
		}

		db2, oerr2 := d.OpenSQLite(ctx)
		if oerr2 != nil {
			t.Fatalf("OpenSQLite after discard: %v", oerr2)
		}
		// Episode gone.
		if got := countRows(t, ctx, db2, `SELECT COUNT(*) FROM episodes WHERE uid = ?`, episodeUID); got != 0 {
			t.Errorf("draft episode still present after discard: count=%d", got)
		}
		// Draft entities gone.
		if got := countRows(t, ctx, db2, `SELECT COUNT(*) FROM entities WHERE uid = ? OR uid = ?`, subjUID, objUID); got != 0 {
			t.Errorf("draft entities still present after discard: count=%d", got)
		}
		// Draft relation gone.
		if got := countRows(t, ctx, db2, `SELECT COUNT(*) FROM relations WHERE uid = ?`, relUID); got != 0 {
			t.Errorf("draft relation still present after discard: count=%d", got)
		}
	})
}

// TestDraftScope_PromoteReScopesAllDerivedRows proves that applyScopePromoted
// moves draft-scoped entities and relations into personal scope alongside the
// episode. After promotion every row must be personal-scoped and visible
// through the default read set.
func TestDraftScope_PromoteReScopesAllDerivedRows(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	const (
		episodeUID = "01HZDRAFTSCOPE0000000EP002"
		subjUID2   = "01HZDRAFTSCOPE0000000SB002"
		objUID2    = "01HZDRAFTSCOPE0000000OB002"
		relUID2    = "01HZDRAFTSCOPE0000000RL002"
	)

	seedDraftEpisodeWithDerivedRows(t, ctx, d, episodeUID, subjUID2, objUID2, relUID2)

	// Apply scopePromoted.
	promotePayload, err := json.Marshal(ontology.ScopePromotedEvent{
		TargetUID: episodeUID,
		FromScope: string(kernel.ScopeDraft),
		ToScope:   string(kernel.ScopePersonal),
	})
	if err != nil {
		t.Fatalf("marshal promote: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        "01HZDRAFTSCOPE0000000PR002",
		Kind:       kernel.EventKind(ontology.KindScopePromoted),
		Source:     "test",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		CreateTime: time.Date(2026, 6, 10, 13, 0, 0, 0, time.UTC),
		Payload:    promotePayload,
	}}); err != nil {
		t.Fatalf("scopePromoted Apply: %v", err)
	}

	db, oerr := d.OpenSQLite(ctx)
	if oerr != nil {
		t.Fatalf("OpenSQLite: %v", oerr)
	}

	// Entities must be personal-scoped.
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM entities WHERE uid = ? AND scope = 'personal'`, subjUID2); got != 1 {
		t.Errorf("subject entity scope not promoted: count=%d", got)
	}
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM entities WHERE uid = ? AND scope = 'personal'`, objUID2); got != 1 {
		t.Errorf("object entity scope not promoted: count=%d", got)
	}

	// Relations must be personal-scoped.
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM relations WHERE uid = ? AND scope = 'personal'`, relUID2); got != 1 {
		t.Errorf("relation scope not promoted: count=%d", got)
	}

	// Episode must be personal-scoped and visible through default nav.
	nav, nerr := navigate.New(db)
	if nerr != nil {
		t.Fatalf("navigate.New: %v", nerr)
	}
	if _, gerr := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal)).GetEpisode(ctx, episodeUID); gerr != nil {
		t.Errorf("promoted episode not visible in personal: %v", gerr)
	}
	// No draft entities should remain.
	if got := countRows(t, ctx, db, `SELECT COUNT(*) FROM entities WHERE scope = 'draft'`); got != 0 {
		t.Errorf("draft entity rows still present after promote: count=%d", got)
	}
}
