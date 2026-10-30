package navigate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// TestGetInsight seeds an insights row and exercises the happy path, not-found,
// demoted-still-fetchable, and the #206 scope fail-closed boundary.
func TestGetInsight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	exec(`INSERT INTO insights
		(uid, statement, insight_type, source_uids, seed_entity_uid,
		 synthesized_confidence, current_confidence, state, source_event_uid,
		 synthesized_at, create_time, scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"ins1", "Merry prefers the Shire.", "preference",
		`["ent1","ent2"]`, "ent1",
		0.9, 0.9, "hot", "ev1",
		"2026-06-17T10:00:00Z", "2026-06-17T10:00:00Z", "personal",
	)
	exec(`INSERT INTO insights
		(uid, statement, insight_type, source_uids, seed_entity_uid,
		 synthesized_confidence, current_confidence, state, source_event_uid,
		 synthesized_at, create_time, scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"ins_demoted", "Sam is a gardener.", "pattern",
		`["ent3"]`, "ent3",
		0.7, 0.3, "demoted", "ev2",
		"2026-06-17T09:00:00Z", "2026-06-17T09:00:00Z", "personal",
	)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	t.Run("happy path fields are populated", func(t *testing.T) {
		t.Parallel()
		ins, err := nav.GetInsight(ctx, "ins1")
		if err != nil {
			t.Fatalf("GetInsight: %v", err)
		}
		if ins.Name != "insights/ins1" {
			t.Errorf("Name = %q, want insights/ins1", ins.Name)
		}
		if ins.Statement != "Merry prefers the Shire." {
			t.Errorf("Statement = %q", ins.Statement)
		}
		if ins.InsightType != "preference" {
			t.Errorf("InsightType = %q, want preference", ins.InsightType)
		}
		if len(ins.SourceUIDs) != 2 || ins.SourceUIDs[0] != "ent1" || ins.SourceUIDs[1] != "ent2" {
			t.Errorf("SourceUIDs = %v, want [ent1 ent2]", ins.SourceUIDs)
		}
		if ins.State != "hot" {
			t.Errorf("State = %q, want hot", ins.State)
		}
		if ins.Scope != "personal" {
			t.Errorf("Scope = %q, want personal", ins.Scope)
		}
	})

	t.Run("demoted insight is still fetchable", func(t *testing.T) {
		t.Parallel()
		ins, err := nav.GetInsight(ctx, "ins_demoted")
		if err != nil {
			t.Fatalf("GetInsight(demoted): %v", err)
		}
		if ins.State != "demoted" {
			t.Errorf("State = %q, want demoted", ins.State)
		}
		if ins.Statement != "Sam is a gardener." {
			t.Errorf("Statement = %q", ins.Statement)
		}
	})

	t.Run("unknown uid returns ErrNotFound", func(t *testing.T) {
		t.Parallel()
		if _, err := nav.GetInsight(ctx, "missing"); !errors.Is(err, navigate.ErrNotFound) {
			t.Errorf("GetInsight(missing): want ErrNotFound, got %v", err)
		}
	})

	t.Run("empty scope set fails closed", func(t *testing.T) {
		t.Parallel()
		empty := nav.WithScopes(kernel.ScopeSet{})
		if _, err := empty.GetInsight(ctx, "ins1"); !errors.Is(err, navigate.ErrNotFound) {
			t.Errorf("GetInsight under empty scope: want ErrNotFound (fail-closed), got %v", err)
		}
	})

	t.Run("out of scope returns ErrNotFound", func(t *testing.T) {
		t.Parallel()
		team := nav.WithScopes(kernel.NewScopeSet(kernel.ScopeTeam))
		if _, err := team.GetInsight(ctx, "ins1"); !errors.Is(err, navigate.ErrNotFound) {
			t.Errorf("GetInsight of personal row under team scope: want ErrNotFound, got %v", err)
		}
	})
}

// TestHasHotInsightForSeed exercises the saturation-guard read method: a hot
// insight present → true; only a demoted insight present → false; unknown seed →
// false; empty scope set fails closed → false.
func TestHasHotInsightForSeed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	exec(`INSERT INTO insights
		(uid, statement, insight_type, source_uids, seed_entity_uid,
		 synthesized_confidence, current_confidence, state, source_event_uid,
		 synthesized_at, create_time, scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"ins_hot", "The Shire is central to Frodo's journey.", "pattern",
		`["ent1"]`, "seed_hot",
		0.9, 0.9, "hot", "ev1",
		"2026-06-17T10:00:00Z", "2026-06-17T10:00:00Z", "personal",
	)
	exec(`INSERT INTO insights
		(uid, statement, insight_type, source_uids, seed_entity_uid,
		 synthesized_confidence, current_confidence, state, source_event_uid,
		 synthesized_at, create_time, scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"ins_demoted2", "Sam rarely leaves Bag End.", "summary",
		`["ent2"]`, "seed_demoted",
		0.7, 0.3, "demoted", "ev2",
		"2026-06-17T09:00:00Z", "2026-06-17T09:00:00Z", "personal",
	)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	t.Run("hot insight present returns true", func(t *testing.T) {
		t.Parallel()
		got, err := nav.HasHotInsightForSeed(ctx, "seed_hot")
		if err != nil {
			t.Fatalf("HasHotInsightForSeed: %v", err)
		}
		if !got {
			t.Error("expected true for seed with hot insight, got false")
		}
	})

	t.Run("only demoted insight returns false", func(t *testing.T) {
		t.Parallel()
		got, err := nav.HasHotInsightForSeed(ctx, "seed_demoted")
		if err != nil {
			t.Fatalf("HasHotInsightForSeed: %v", err)
		}
		if got {
			t.Error("expected false for seed with only a demoted insight, got true")
		}
	})

	t.Run("unknown seed returns false", func(t *testing.T) {
		t.Parallel()
		got, err := nav.HasHotInsightForSeed(ctx, "seed_unknown")
		if err != nil {
			t.Fatalf("HasHotInsightForSeed: %v", err)
		}
		if got {
			t.Error("expected false for unknown seed, got true")
		}
	})

	t.Run("empty scope set fails closed", func(t *testing.T) {
		t.Parallel()
		empty := nav.WithScopes(kernel.ScopeSet{})
		got, err := empty.HasHotInsightForSeed(ctx, "seed_hot")
		if err != nil {
			t.Fatalf("HasHotInsightForSeed under empty scope: %v", err)
		}
		if got {
			t.Error("expected false (fail-closed) under empty scope set, got true")
		}
	})

	t.Run("out of scope returns false", func(t *testing.T) {
		t.Parallel()
		// The seeded insight is in scope 'personal'; a team-scoped navigator must
		// not see it.
		team := nav.WithScopes(kernel.NewScopeSet(kernel.ScopeTeam))
		got, err := team.HasHotInsightForSeed(ctx, "seed_hot")
		if err != nil {
			t.Fatalf("HasHotInsightForSeed under team scope: %v", err)
		}
		if got {
			t.Error("expected false for personal row under team scope, got true")
		}
	})
}

// TestGetConsolidatedMemory seeds a consolidated_memories row and exercises the
// happy path, not-found, and the #206 scope fail-closed boundary.
func TestGetConsolidatedMemory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	exec(`INSERT INTO consolidated_memories
		(uid, summary, memory_type, source_uids, cited_entity_uid,
		 confidence, state, source_event_uid, consolidated_at, create_time, scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"cm1", "Merry's activities in the Shire.", "semantic",
		`["ep1","ep2"]`, "ent1",
		0.85, "hot", "ev1",
		"2026-06-17T10:00:00Z", "2026-06-17T10:00:00Z", "project",
	)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	t.Run("happy path fields are populated", func(t *testing.T) {
		t.Parallel()
		cm, err := nav.GetConsolidatedMemory(ctx, "cm1")
		if err != nil {
			t.Fatalf("GetConsolidatedMemory: %v", err)
		}
		if cm.Name != "consolidatedMemories/cm1" {
			t.Errorf("Name = %q, want consolidatedMemories/cm1", cm.Name)
		}
		if cm.Summary != "Merry's activities in the Shire." {
			t.Errorf("Summary = %q", cm.Summary)
		}
		if cm.MemoryType != "semantic" {
			t.Errorf("MemoryType = %q, want semantic", cm.MemoryType)
		}
		if len(cm.SourceUIDs) != 2 || cm.SourceUIDs[0] != "ep1" || cm.SourceUIDs[1] != "ep2" {
			t.Errorf("SourceUIDs = %v, want [ep1 ep2]", cm.SourceUIDs)
		}
		if cm.CitedEntityUID != "ent1" {
			t.Errorf("CitedEntityUID = %q, want ent1", cm.CitedEntityUID)
		}
		if cm.State != "hot" {
			t.Errorf("State = %q, want hot", cm.State)
		}
		if cm.Scope != "project" {
			t.Errorf("Scope = %q, want project", cm.Scope)
		}
	})

	t.Run("unknown uid returns ErrNotFound", func(t *testing.T) {
		t.Parallel()
		if _, err := nav.GetConsolidatedMemory(ctx, "missing"); !errors.Is(err, navigate.ErrNotFound) {
			t.Errorf("GetConsolidatedMemory(missing): want ErrNotFound, got %v", err)
		}
	})

	t.Run("empty scope set fails closed", func(t *testing.T) {
		t.Parallel()
		empty := nav.WithScopes(kernel.ScopeSet{})
		if _, err := empty.GetConsolidatedMemory(ctx, "cm1"); !errors.Is(err, navigate.ErrNotFound) {
			t.Errorf("GetConsolidatedMemory under empty scope: want ErrNotFound (fail-closed), got %v", err)
		}
	})

	t.Run("out of scope returns ErrNotFound", func(t *testing.T) {
		t.Parallel()
		personal := nav.WithScopes(kernel.NewScopeSet(kernel.ScopePersonal))
		if _, err := personal.GetConsolidatedMemory(ctx, "cm1"); !errors.Is(err, navigate.ErrNotFound) {
			t.Errorf("GetConsolidatedMemory of project row under personal scope: want ErrNotFound, got %v", err)
		}
	})
}
