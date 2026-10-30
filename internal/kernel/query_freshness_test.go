package kernel

import (
	"database/sql"
	"testing"
	"time"
)

// openMemoryFreshness builds an in-memory SQLite DB with the source_refs and
// relations columns attachFreshness reads, so the two-leg freshness join (direct
// SourceRef subject + transitive has_primary_source edge) can be exercised
// without the full four-layer store.
func openMemoryFreshness(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `
		CREATE TABLE source_refs (
			uid          TEXT PRIMARY KEY,
			subject_uid  TEXT NOT NULL,
			connector    TEXT,
			external_url TEXT,
			fetch_time   TEXT NOT NULL,
			tombstone    TEXT,
			scope        TEXT NOT NULL DEFAULT 'personal'
		);
		CREATE TABLE relations (
			uid          TEXT PRIMARY KEY,
			subject_uid  TEXT NOT NULL,
			predicate    TEXT NOT NULL,
			object_uid   TEXT NOT NULL DEFAULT '',
			valid_until  TEXT,
			scope        TEXT NOT NULL DEFAULT 'personal'
		)`); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

// TestHybridQuery_AttachFreshness pins the two-leg freshness join: a hit grounded
// DIRECTLY (it is the subject of a source_ref) gets that ref's fetch time /
// connector / url / tombstone; a hit grounded TRANSITIVELY (an active
// has_primary_source edge to an external_ref that itself carries a source_ref)
// gets the ref via the edge; the direct leg wins when both exist; a tombstoned
// ref surfaces its marker; and a hit with no grounding is left untouched.
func TestHybridQuery_AttachFreshness(t *testing.T) {
	t.Parallel()
	db := openMemoryFreshness(t)
	ctx := withScopes(t.Context(), DefaultReadSet())
	now := time.Now().UTC().Format(time.RFC3339Nano)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}

	// A: direct source_ref grounding.
	exec(`INSERT INTO source_refs (uid, subject_uid, connector, external_url, fetch_time, scope) VALUES ('sr_a','A','notion','https://n.so/a',?,'personal')`, now)
	// B: transitive grounding via an active has_primary_source edge to external_ref X.
	exec(`INSERT INTO relations (uid, subject_uid, predicate, object_uid, valid_until, scope) VALUES ('rel_b','B','has_primary_source','X',NULL,'personal')`)
	exec(`INSERT INTO source_refs (uid, subject_uid, connector, external_url, fetch_time, scope) VALUES ('sr_x','X','linear','https://l.so/x',?,'personal')`, now)
	// C: tombstoned direct source_ref.
	exec(`INSERT INTO source_refs (uid, subject_uid, connector, fetch_time, tombstone, scope) VALUES ('sr_c','C','notion',?,'deleted','personal')`, now)
	// E: BOTH a direct source_ref (drive) AND a transitive edge to Y (granola) -> direct wins.
	exec(`INSERT INTO source_refs (uid, subject_uid, connector, fetch_time, scope) VALUES ('sr_e','E','drive',?,'personal')`, now)
	exec(`INSERT INTO relations (uid, subject_uid, predicate, object_uid, valid_until, scope) VALUES ('rel_e','E','has_primary_source','Y',NULL,'personal')`)
	exec(`INSERT INTO source_refs (uid, subject_uid, connector, fetch_time, scope) VALUES ('sr_y','Y','granola',?,'personal')`, now)

	h, err := NewHybridQuery(stubEmbedder{}, &stubVector{}, &stubFTS{}, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	hits := []Hit{{UID: "A"}, {UID: "B"}, {UID: "C"}, {UID: "D"}, {UID: "E"}}
	if err := h.attachFreshness(ctx, hits); err != nil {
		t.Fatalf("attachFreshness: %v", err)
	}
	by := map[string]Hit{}
	for _, hit := range hits {
		by[hit.UID] = hit
	}

	// A: direct leg.
	if by["A"].SourceConnector != "notion" || by["A"].ExternalURL != "https://n.so/a" || by["A"].FetchTime.IsZero() {
		t.Errorf("A = %+v, want notion/url/non-zero fetch via direct source_ref", by["A"])
	}
	if by["A"].Tombstone != "" {
		t.Errorf("A tombstone = %q, want empty (live)", by["A"].Tombstone)
	}
	// B: transitive leg.
	if by["B"].SourceConnector != "linear" || by["B"].ExternalURL != "https://l.so/x" {
		t.Errorf("B = %+v, want linear/url via the has_primary_source edge", by["B"])
	}
	// C: tombstoned.
	if by["C"].Tombstone != "deleted" || by["C"].SourceConnector != "notion" {
		t.Errorf("C = %+v, want tombstone=deleted connector=notion", by["C"])
	}
	// D: ungrounded, left zero.
	if by["D"].SourceConnector != "" || !by["D"].FetchTime.IsZero() || by["D"].Tombstone != "" {
		t.Errorf("D = %+v, want untouched (no grounding)", by["D"])
	}
	// E: direct leg wins over the transitive one.
	if by["E"].SourceConnector != "drive" {
		t.Errorf("E connector = %q, want drive (direct leg wins over transitive granola)", by["E"].SourceConnector)
	}
}

// TestHybridQuery_AttachFreshness_ScopeIsolated proves the freshness join is
// scope-isolated: a source_ref in a scope outside the read set does not ground
// the hit. An empty input is a no-op.
func TestHybridQuery_AttachFreshness_ScopeIsolated(t *testing.T) {
	t.Parallel()
	db := openMemoryFreshness(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO source_refs (uid, subject_uid, connector, fetch_time, scope) VALUES ('sr_t','T','notion',?,'team')`, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h, err := NewHybridQuery(stubEmbedder{}, &stubVector{}, &stubFTS{}, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	// Default read set {project,personal} excludes the team-scope ref.
	ctx := withScopes(t.Context(), DefaultReadSet())
	hits := []Hit{{UID: "T"}}
	if err := h.attachFreshness(ctx, hits); err != nil {
		t.Fatalf("attachFreshness: %v", err)
	}
	if hits[0].SourceConnector != "" {
		t.Errorf("T grounded under default scope = %q, want excluded (team-scope ref)", hits[0].SourceConnector)
	}

	// Widening to team surfaces it.
	teamCtx := withScopes(t.Context(), NewScopeSet(ScopeTeam))
	hits = []Hit{{UID: "T"}}
	if err := h.attachFreshness(teamCtx, hits); err != nil {
		t.Fatalf("attachFreshness(team): %v", err)
	}
	if hits[0].SourceConnector != "notion" {
		t.Errorf("T under team scope = %q, want notion", hits[0].SourceConnector)
	}

	// Empty input is a no-op.
	if err := h.attachFreshness(ctx, nil); err != nil {
		t.Fatalf("attachFreshness(empty): %v", err)
	}
}
