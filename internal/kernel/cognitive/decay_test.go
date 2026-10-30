package cognitive_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/cognitive"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// openDecayDB opens a fresh migrated SQLite projection and a seed helper.
func openDecayDB(t *testing.T) (*sql.DB, func(q string, args ...any)) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, eerr := store.Writer().ExecContext(ctx, q, args...); eerr != nil {
			t.Fatalf("seed %q: %v", q, eerr)
		}
	}
	return store.Writer(), exec
}

func seedInsight(exec func(string, ...any), uid string, conf float64, synthesizedAt time.Time) {
	exec(`INSERT INTO insights
		(uid, statement, insight_type, source_uids, seed_entity_uid,
		 synthesized_confidence, current_confidence, state, source_event_uid,
		 synthesized_at, create_time, scope)
		VALUES (?, ?, 'pattern', '["x"]', 'seed', ?, ?, 'hot', ?, ?, ?, 'personal')`,
		uid, "insight "+uid, conf, conf, "ev_"+uid,
		synthesizedAt.UTC().Format(time.RFC3339Nano),
		synthesizedAt.UTC().Format(time.RFC3339Nano))
}

func readInsightState(t *testing.T, db *sql.DB, uid string) (state string, currConf float64) {
	t.Helper()
	if err := db.QueryRowContext(context.Background(),
		`SELECT state, current_confidence FROM insights WHERE uid = ?`, uid,
	).Scan(&state, &currConf); err != nil {
		t.Fatalf("read insight %q: %v", uid, err)
	}
	return state, currConf
}

// TestSweepInsightDecay_DemotesStaleLowConfidence asserts a hot insight whose
// effective confidence (baseline · Gaussian decay over age) falls below the
// floor is demoted, while a freshly synthesized high-confidence insight stays
// hot. The demote is in-place projection state, never an event.
func TestSweepInsightDecay_DemotesStaleLowConfidence(t *testing.T) {
	t.Parallel()
	db, exec := openDecayDB(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Stale + low baseline: synthesized 180 days ago at 0.3 → decays well below 0.1.
	seedInsight(exec, "stale", 0.3, now.AddDate(0, 0, -180))
	// Fresh + high: synthesized today at 0.9 → effective ~0.9, stays hot.
	seedInsight(exec, "fresh", 0.9, now)

	demoted, err := cognitive.SweepInsightDecay(context.Background(), db, cognitive.SweepOptions{Now: now})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if demoted != 1 {
		t.Errorf("demoted count = %d, want 1", demoted)
	}

	if state, _ := readInsightState(t, db, "stale"); state != "demoted" {
		t.Errorf("stale insight state = %q, want demoted", state)
	}
	if state, conf := readInsightState(t, db, "fresh"); state != "hot" {
		t.Errorf("fresh insight state = %q, want hot (effective conf %v)", state, conf)
	}
}

// TestSweepInsightDecay_AccessReanchorsDecay asserts an insight synthesized long
// ago but accessed recently (memory_strength.last_accessed_at) decays from the
// access anchor, not synthesis — so a re-accessed insight survives the sweep.
// This is the reinforce-on-access re-promotion working through the shared
// memory_strength row.
func TestSweepInsightDecay_AccessReanchorsDecay(t *testing.T) {
	t.Parallel()
	db, exec := openDecayDB(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Synthesized 180 days ago at 0.9 — would decay below floor on synthesis age.
	seedInsight(exec, "reaccessed", 0.9, now.AddDate(0, 0, -180))
	// But accessed yesterday: the sweep anchors on last_accessed_at.
	exec(`INSERT INTO memory_strength (target_uid, access_count, first_accessed_at, last_accessed_at)
	      VALUES (?, ?, ?, ?)`,
		"reaccessed", 3,
		now.AddDate(0, 0, -2).Format(time.RFC3339Nano),
		now.AddDate(0, 0, -1).Format(time.RFC3339Nano))

	if _, err := cognitive.SweepInsightDecay(context.Background(), db, cognitive.SweepOptions{Now: now}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if state, conf := readInsightState(t, db, "reaccessed"); state != "hot" {
		t.Errorf("re-accessed insight state = %q (conf %v), want hot — decay must anchor on last access", state, conf)
	}
}

// TestSweepInsightDecay_IgnoresAlreadyDemoted asserts the sweep only touches hot
// insights — a demoted insight is left alone (its restoration is rebuild's job,
// or reinforce-on-access).
func TestSweepInsightDecay_IgnoresAlreadyDemoted(t *testing.T) {
	t.Parallel()
	db, exec := openDecayDB(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	seedInsight(exec, "already", 0.9, now)
	if _, err := db.ExecContext(context.Background(),
		`UPDATE insights SET state = 'demoted' WHERE uid = 'already'`); err != nil {
		t.Fatalf("pre-demote: %v", err)
	}

	demoted, err := cognitive.SweepInsightDecay(context.Background(), db, cognitive.SweepOptions{Now: now})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if demoted != 0 {
		t.Errorf("demoted count = %d, want 0 (no hot insights)", demoted)
	}
	if state, _ := readInsightState(t, db, "already"); state != "demoted" {
		t.Errorf("already-demoted insight state = %q, want demoted (untouched)", state)
	}
}
