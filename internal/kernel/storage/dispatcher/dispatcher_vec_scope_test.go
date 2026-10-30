package dispatcher_test

// dispatcher_vec_scope_test.go: proves the dispatcher stamps vector sidecar
// rows with the scope of the event that produced them (C5). The sidecar scope
// column is what lets the KNN layer pre-filter candidates by ScopeSet; the
// SQLite join-back remains the authoritative gate, but a wrong sidecar scope
// would silently starve scoped recall of candidates, so the write path is
// pinned here for both sidecar shapes (fixed-id episodes_vec_ids and
// (id, vector_type) entities_vec_ids) plus the personal default.

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// sidecarScopes returns the distinct scope values on sidecar rows for one id.
func sidecarScopes(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, table, id string) []string {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT scope FROM `+table+` WHERE id = ? ORDER BY scope`, id)
	if err != nil {
		t.Fatalf("query %s scopes: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var scopes []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan scope: %v", err)
		}
		scopes = append(scopes, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return scopes
}

// TestDispatcher_VecSidecarScopeStamping applies a team-scoped episode, a
// team-scoped entity (with summary, so both vector_type rows exist), and a
// scope-less entity, then asserts every sidecar row carries the event's scope
// ('personal' when unset).
func TestDispatcher_VecSidecarScopeStamping(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	episode := makeEpisodeEvent(t, "01HVSCEP0000000000000000T1", "team scoped episode", "cli:test")
	episode.Scope = kernel.ScopeTeam

	teamEntity := ontology.Entity{
		UID:              "01HVSCEN0000000000000000T1",
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Samwise Gamgee",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
		Summary:          "Samwise Gamgee is a gardener at Bag End in Hobbiton.",
	}
	teamEntityEv := makeEntityEvent(t, teamEntity.UID, teamEntity)
	teamEntityEv.Scope = kernel.ScopeTeam

	personalEntity := ontology.Entity{
		UID:              "01HVSCEN0000000000000000P1",
		Kind:             ontology.EntityKindPerson,
		DisplayName:      "Rosie Cotton",
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Unix(1_700_000_000, 0).UTC(),
	}
	personalEntityEv := makeEntityEvent(t, personalEntity.UID, personalEntity)

	if err := d.Apply(ctx, []kernel.Event{episode, teamEntityEv, personalEntityEv}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Episode and entity embeddings land asynchronously; drain before asserting.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	t.Run("team episode sidecar stamped team", func(t *testing.T) {
		got := sidecarScopes(t, ctx, d, "episodes_vec_ids", episode.UID)
		if len(got) != 1 || got[0] != "team" {
			t.Errorf("episodes_vec_ids scopes = %v, want [team]", got)
		}
	})

	t.Run("team entity sidecar stamped team for both vector types", func(t *testing.T) {
		if rows := countEntityVecRows(t, ctx, d, teamEntity.UID); rows != 2 {
			t.Fatalf("entities_vec_ids rows = %d, want 2 (name + summary)", rows)
		}
		got := sidecarScopes(t, ctx, d, "entities_vec_ids", teamEntity.UID)
		if len(got) != 1 || got[0] != "team" {
			t.Errorf("entities_vec_ids scopes = %v, want [team]", got)
		}
	})

	t.Run("scope-less entity defaults to personal", func(t *testing.T) {
		got := sidecarScopes(t, ctx, d, "entities_vec_ids", personalEntity.UID)
		if len(got) != 1 || got[0] != "personal" {
			t.Errorf("entities_vec_ids scopes = %v, want [personal]", got)
		}
	})
}
