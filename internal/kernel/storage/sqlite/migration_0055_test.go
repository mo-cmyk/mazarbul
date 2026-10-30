package sqlite

// migration_0055_test.go: tests for the 0055_documents_path_scope_unique
// migration, which rebuilds the documents table to replace the inline
// `path UNIQUE` constraint with a composite UNIQUE(path, scope).
//
// The load-bearing assertions:
//   - The rebuild preserves every existing row and column losslessly (applied on
//     a table already populated at the pre-0055 schema, not an empty fresh one).
//   - documents_vec_ids (keyed on document_uid, NOT rowid) survives the rebuild:
//     a join by document_uid still resolves.
//   - The FTS index is rebuilt for LIVE rows and the "tombstones excluded"
//     invariant is restored (step 8): a soft-deleted document does not surface.
//   - The new composite key lets the SAME path coexist across scopes while still
//     rejecting a duplicate (path, scope) pair.

import (
	"database/sql"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/migrations"
)

// migrationsUpTo returns the production migrations with version <= max, so a
// test can stage a DB at a pre-migration schema, seed rows, then apply the
// remaining migrations to exercise a rebuild on populated data.
func migrationsUpTo(t *testing.T, max int) []migration {
	t.Helper()
	ms, err := loadMigrations(migrations.SQLite)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	out := make([]migration, 0, len(ms))
	for _, m := range ms {
		if m.version <= max {
			out = append(out, m)
		}
	}
	return out
}

// TestMigration0055_PreservesRowsThroughRebuild seeds live, tombstoned, and
// pinned+canonical rows at the pre-0055 schema, applies 0055, and asserts the
// rebuild preserved every row and column, kept documents_vec_ids joinable, and
// restored the FTS "tombstones excluded" invariant.
func TestMigration0055_PreservesRowsThroughRebuild(t *testing.T) {
	t.Parallel()
	db := newWriterDB(t)
	ctx := t.Context()

	// Stage the schema at 0054 (documents has scope@0027 + canonical@0043 and
	// documents_vec_ids has scope@0054, but path is still globally UNIQUE).
	staged := migrationsUpTo(t, 54)
	if err := runMigrations(ctx, db, staged); err != nil {
		t.Fatalf("runMigrations up to 0054: %v", err)
	}

	// Seed representative rows. Paths differ (0054 still enforces global path
	// uniqueness); the AFTER INSERT trigger indexes live rows into FTS and skips
	// the tombstone (deleted=1).
	seed := []struct {
		uid, path, body, hash, scope string
		pinned, canonical, deleted   int
	}{
		{"DOC_KEEP", "notes/keep.md", "the KEEPTOKEN survives the rebuild", "hkeep", "personal", 0, 0, 0},
		{"DOC_GONE", "notes/gone.md", "the GONETOKEN is tombstoned", "hgone", "personal", 0, 0, 1},
		{"DOC_PIN", "notes/pin.md", "the PINTOKEN is pinned and canonical", "hpin", "team", 1, 1, 0},
	}
	for _, s := range seed {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO documents
				(document_uid, path, slug, title, body, content_hash, revision_uid,
				 pinned, canonical, create_time, update_time, deleted, scope)
			VALUES (?, ?, '', '', ?, ?, ?, ?, ?, '2026-07-01T00:00:00Z', '2026-07-01T00:00:00Z', ?, ?)`,
			s.uid, s.path, s.body, s.hash, s.uid+"REV", s.pinned, s.canonical, s.deleted, s.scope,
		); err != nil {
			t.Fatalf("seed %s: %v", s.uid, err)
		}
	}
	// A vec sidecar row keyed by document_uid must survive the rebuild.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO documents_vec_ids(id, document_uid, chunk_index) VALUES ('DOC_KEEP#0', 'DOC_KEEP', 0)`,
	); err != nil {
		t.Fatalf("seed vec sidecar: %v", err)
	}

	// Apply the remaining migrations (only 0055 is new).
	all, err := loadMigrations(migrations.SQLite)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	// Guard: staging must stop strictly before 0055, otherwise the rows above were
	// seeded after the table rebuild already ran and this test would false-green on
	// unpopulated data instead of exercising the rebuild.
	if len(staged) >= len(all) {
		t.Fatalf("staged %d migrations >= all %d: 0055 not applied over seeded rows", len(staged), len(all))
	}
	if err := runMigrations(ctx, db, all); err != nil {
		t.Fatalf("runMigrations through 0055: %v", err)
	}

	// Row count preserved.
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM documents`).Scan(&count); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if count != len(seed) {
		t.Errorf("row count after rebuild = %d, want %d", count, len(seed))
	}

	// Columns preserved for the pinned+canonical team row.
	var (
		gotScope            string
		gotPinned, gotCanon int
	)
	if err := db.QueryRowContext(ctx,
		`SELECT scope, pinned, canonical FROM documents WHERE document_uid = 'DOC_PIN'`,
	).Scan(&gotScope, &gotPinned, &gotCanon); err != nil {
		t.Fatalf("read DOC_PIN: %v", err)
	}
	if gotScope != "team" || gotPinned != 1 || gotCanon != 1 {
		t.Errorf("DOC_PIN after rebuild = (scope=%q pinned=%d canonical=%d), want (team,1,1)", gotScope, gotPinned, gotCanon)
	}

	// Tombstone deleted flag preserved.
	var gotDeleted int
	if err := db.QueryRowContext(ctx,
		`SELECT deleted FROM documents WHERE document_uid = 'DOC_GONE'`,
	).Scan(&gotDeleted); err != nil {
		t.Fatalf("read DOC_GONE: %v", err)
	}
	if gotDeleted != 1 {
		t.Errorf("DOC_GONE.deleted after rebuild = %d, want 1", gotDeleted)
	}

	// documents_vec_ids still joins by document_uid.
	var joinCount int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM documents d
		JOIN documents_vec_ids v ON v.document_uid = d.document_uid
		WHERE d.document_uid = 'DOC_KEEP'`,
	).Scan(&joinCount); err != nil {
		t.Fatalf("join vec sidecar: %v", err)
	}
	if joinCount != 1 {
		t.Errorf("documents_vec_ids join for DOC_KEEP = %d rows, want 1", joinCount)
	}

	// FTS: live rows indexed, tombstone excluded (step 8 invariant).
	assertFTSCount(t, db, "KEEPTOKEN", 1) // live
	assertFTSCount(t, db, "PINTOKEN", 1)  // live pinned/canonical
	assertFTSCount(t, db, "GONETOKEN", 0) // tombstone must NOT surface
}

// TestMigration0055_CompositeUnique asserts the composite key lets the same path
// coexist across scopes but still rejects a duplicate (path, scope).
func TestMigration0055_CompositeUnique(t *testing.T) {
	t.Parallel()
	db := newWriterDB(t)
	ctx := t.Context()

	all, err := loadMigrations(migrations.SQLite)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if err := runMigrations(ctx, db, all); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	insert := func(uid, path, scope string) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO documents
				(document_uid, path, slug, title, body, content_hash, revision_uid,
				 create_time, update_time, scope)
			VALUES (?, ?, '', '', '', 'h', ?, '2026-07-01T00:00:00Z', '2026-07-01T00:00:00Z', ?)`,
			uid, path, uid+"REV", scope,
		)
		return err
	}

	if err := insert("D1", "notes/foo.md", "personal"); err != nil {
		t.Fatalf("insert personal notes/foo.md: %v", err)
	}
	// Same path, different scope: must be accepted (the whole point of 0055).
	if err := insert("D2", "notes/foo.md", "team"); err != nil {
		t.Errorf("same path in a different scope should be accepted: %v", err)
	}
	// Duplicate (path, scope): must be rejected.
	if err := insert("D3", "notes/foo.md", "personal"); err == nil {
		t.Error("duplicate (path, scope) should violate UNIQUE(path, scope)")
	}
}

// assertFTSCount fails unless a documents_fts MATCH for token returns want rows.
func assertFTSCount(t *testing.T, db *sql.DB, token string, want int) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT rowid FROM documents_fts WHERE documents_fts MATCH ?`, token)
	if err != nil {
		t.Fatalf("FTS match %q: %v", token, err)
	}
	defer func() { _ = rows.Close() }()
	var got int
	for rows.Next() {
		got++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("FTS rows %q: %v", token, err)
	}
	if got != want {
		t.Errorf("FTS match %q = %d rows, want %d", token, got, want)
	}
}
