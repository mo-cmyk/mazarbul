package navigate_test

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// seedInsight inserts one insights row via exec.
func seedInsight(exec func(string, ...any), uid, statement, state, scope string, conf float64, sources, synthesizedAt string) {
	exec(`INSERT INTO insights
		(uid, statement, insight_type, source_uids, seed_entity_uid,
		 synthesized_confidence, current_confidence, state, source_event_uid,
		 synthesized_at, create_time, scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		uid, statement, "pattern", sources, "seed1",
		conf, conf, state, "ev_"+uid,
		synthesizedAt, synthesizedAt, scope,
	)
}

// TestListInsights exercises the inbox list reader: state filtering, confidence
// ordering, the "all" (empty-state) path, citation decoding, and the #206 scope
// fail-closed boundary.
func TestListInsights(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	seedInsight(exec, "ins_hot_hi", "Frodo trusts Sam.", "hot", "personal", 0.95, `["ent1","ent2"]`, "2026-06-17T10:00:00Z")
	seedInsight(exec, "ins_hot_lo", "Merry likes the Shire.", "hot", "personal", 0.60, `["ent3"]`, "2026-06-17T09:00:00Z")
	seedInsight(exec, "ins_demoted", "Sam gardens.", "demoted", "personal", 0.30, `["ent4"]`, "2026-06-17T08:00:00Z")
	seedInsight(exec, "ins_dismissed", "Pippin is loud.", "dismissed", "personal", 0.50, `[]`, "2026-06-17T07:00:00Z")
	seedInsight(exec, "ins_team", "Gandalf advises.", "hot", "team", 0.99, `["ent5"]`, "2026-06-17T11:00:00Z")

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	personal := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal))

	t.Run("hot only most confident first", func(t *testing.T) {
		t.Parallel()
		got, err := personal.ListInsights(ctx, "hot", 50)
		if err != nil {
			t.Fatalf("ListInsights(hot): %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("hot count = %d, want 2 (the team hot row is out of scope)", len(got))
		}
		if got[0].UID != "ins_hot_hi" || got[1].UID != "ins_hot_lo" {
			t.Errorf("order = [%s,%s], want most-confident-first [ins_hot_hi ins_hot_lo]", got[0].UID, got[1].UID)
		}
		if got[0].Name != "insights/ins_hot_hi" {
			t.Errorf("Name = %q, want insights/ins_hot_hi", got[0].Name)
		}
		if len(got[0].SourceUIDs) != 2 {
			t.Errorf("SourceUIDs = %v, want 2 decoded citations", got[0].SourceUIDs)
		}
	})

	t.Run("dismissed state is listable", func(t *testing.T) {
		t.Parallel()
		got, err := personal.ListInsights(ctx, "dismissed", 50)
		if err != nil {
			t.Fatalf("ListInsights(dismissed): %v", err)
		}
		if len(got) != 1 || got[0].UID != "ins_dismissed" {
			t.Fatalf("dismissed listing = %+v, want exactly ins_dismissed", got)
		}
	})

	t.Run("empty state returns every state", func(t *testing.T) {
		t.Parallel()
		got, err := personal.ListInsights(ctx, "", 50)
		if err != nil {
			t.Fatalf("ListInsights(all): %v", err)
		}
		// 2 hot + 1 demoted + 1 dismissed (personal scope); the team row is excluded.
		if len(got) != 4 {
			t.Fatalf("all-state count = %d, want 4", len(got))
		}
	})

	t.Run("limit caps the result", func(t *testing.T) {
		t.Parallel()
		got, err := personal.ListInsights(ctx, "", 1)
		if err != nil {
			t.Fatalf("ListInsights(limit 1): %v", err)
		}
		if len(got) != 1 {
			t.Errorf("limit 1 returned %d rows, want 1", len(got))
		}
	})

	t.Run("empty scope set fails closed", func(t *testing.T) {
		t.Parallel()
		empty := nav.WithScopes(kernel.ScopeSet{})
		got, err := empty.ListInsights(ctx, "hot", 50)
		if err != nil {
			t.Fatalf("ListInsights under empty scope: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("empty scope set returned %d rows, want 0 (fail-closed)", len(got))
		}
	})

	t.Run("scope isolates rows", func(t *testing.T) {
		t.Parallel()
		team := nav.WithScopes(kernel.NewScopeSet(kernel.ScopeTeam))
		got, err := team.ListInsights(ctx, "hot", 50)
		if err != nil {
			t.Fatalf("ListInsights(team): %v", err)
		}
		if len(got) != 1 || got[0].UID != "ins_team" {
			t.Fatalf("team hot listing = %+v, want exactly ins_team", got)
		}
	})
}
