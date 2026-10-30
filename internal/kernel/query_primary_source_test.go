package kernel

import (
	"database/sql"
	"testing"
)

// openMemoryPrimarySource builds an in-memory SQLite DB with just the two
// tables SubjectsWithPrimarySource reads (source_refs + relations), each with a
// scope column so the scope-isolation path is exercised. Kept separate from
// openMemoryEpisodes, whose minimal relations table lacks the subject_uid /
// predicate columns this query needs.
func openMemoryPrimarySource(t *testing.T) *sql.DB {
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

// TestHybridQuery_SubjectsWithPrimarySource pins the recall `--has-primary-source`
// kernel reader: a subject counts as grounded when it is the subject of a
// source_ref OR of an ACTIVE has_primary_source edge, scope-isolated. A
// superseded edge (valid_until set), an out-of-scope edge, and a non-
// has_primary_source predicate must all be excluded.
func TestHybridQuery_SubjectsWithPrimarySource(t *testing.T) {
	t.Parallel()
	db := openMemoryPrimarySource(t)
	ctx := t.Context()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	// A: source_ref. B: active has_primary_source edge.
	exec(`INSERT INTO source_refs (uid, subject_uid, scope) VALUES ('sr1','A','personal')`)
	exec(`INSERT INTO relations (uid, subject_uid, predicate, valid_until, scope) VALUES ('r1','B','has_primary_source',NULL,'project')`)
	// C: only a SUPERSEDED has_primary_source edge (valid_until set) -> excluded.
	exec(`INSERT INTO relations (uid, subject_uid, predicate, valid_until, scope) VALUES ('r2','C','has_primary_source','2026-01-01T00:00:00Z','personal')`)
	// D: an active edge but in the team scope -> excluded under the default read set.
	exec(`INSERT INTO relations (uid, subject_uid, predicate, valid_until, scope) VALUES ('r3','D','has_primary_source',NULL,'team')`)
	// E: a non-has_primary_source predicate -> must not count.
	exec(`INSERT INTO relations (uid, subject_uid, predicate, valid_until, scope) VALUES ('r4','E','works_at',NULL,'personal')`)

	h, err := NewHybridQuery(stubEmbedder{}, &stubVector{}, &stubFTS{}, db)
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	// Default read set {project,personal}: A + B grounded; C/D/E not.
	have, err := h.SubjectsWithPrimarySource(ctx, []string{"A", "B", "C", "D", "E"}, nil)
	if err != nil {
		t.Fatalf("SubjectsWithPrimarySource: %v", err)
	}
	for _, u := range []string{"A", "B"} {
		if !have[u] {
			t.Errorf("subject %q: want grounded in a primary source, got none", u)
		}
	}
	for _, u := range []string{"C", "D", "E"} {
		if have[u] {
			t.Errorf("subject %q: want NOT grounded, but it was reported", u)
		}
	}
	if len(have) != 2 {
		t.Errorf("have = %v, want exactly {A,B}", have)
	}

	// Widening to the team scope surfaces D's active edge.
	haveTeam, err := h.SubjectsWithPrimarySource(ctx, []string{"D"}, []string{"team"})
	if err != nil {
		t.Fatalf("SubjectsWithPrimarySource(team): %v", err)
	}
	if !haveTeam["D"] {
		t.Errorf("subject D under team scope: want grounded, got %v", haveTeam)
	}

	// Empty input short-circuits to an empty (non-nil) map, no error.
	empty, err := h.SubjectsWithPrimarySource(ctx, nil, nil)
	if err != nil {
		t.Fatalf("SubjectsWithPrimarySource(empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty input: have = %v, want empty", empty)
	}
}
