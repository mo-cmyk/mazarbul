package sqlite

// migration_0016_test.go: integration tests for the 0016_documents migration.
// Applies the production embedded FS through migration 0016 and asserts all
// expected schema objects exist and behave correctly.
//
// Acceptance criteria:
//   - documents table exists with all expected columns.
//   - documents_fts FTS5 virtual table exists and the AFTER INSERT / AFTER
//     UPDATE / AFTER DELETE triggers are present.
//   - documents_vec virtual table exists.
//   - documents_vec_ids sidecar table exists with all expected columns and
//     the document_uid index.
//   - FTS5 indexing round-trips correctly (insert → FTS search returns the row).
//   - Running the full migration set twice is idempotent (runner skips).

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/migrations"
)

// TestMigration0016_Up_ObjectsExist applies all production migrations and
// asserts every schema object introduced by 0016 is present and queryable.
func TestMigration0016_Up_ObjectsExist(t *testing.T) {
	t.Parallel()
	db := newWriterDB(t)
	ctx := t.Context()

	ms, err := loadMigrations(migrations.SQLite)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if err := runMigrations(ctx, db, ms); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	// documents table must exist.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='documents'`,
	).Scan(&n); err != nil {
		t.Fatalf("probe documents table: %v", err)
	}
	if n == 0 {
		t.Fatal("documents table missing after migration 0016")
	}

	// Expected columns on documents.
	for _, col := range []string{
		"document_uid", "path", "slug", "title", "body",
		"frontmatter_json", "frontmatter_raw", "content_hash", "revision_uid",
		"pinned", "create_time", "update_time", "unresolved_links_json", "deleted",
	} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info('documents') WHERE name = ?`, col,
		).Scan(&count); err != nil {
			t.Fatalf("pragma_table_info documents col %s: %v", col, err)
		}
		if count == 0 {
			t.Errorf("documents.%s missing after migration 0016", col)
		}
	}

	// FTS5 virtual table and sync triggers must exist.
	for _, obj := range []struct {
		typ  string
		name string
	}{
		{"table", "documents_fts"},
		{"trigger", "documents_ai"},
		{"trigger", "documents_ad"},
		{"trigger", "documents_au"},
	} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type=? AND name=?`, obj.typ, obj.name,
		).Scan(&count); err != nil {
			t.Fatalf("probe %s %s: %v", obj.typ, obj.name, err)
		}
		if count == 0 {
			t.Errorf("%s %s missing after migration 0016", obj.typ, obj.name)
		}
	}

	// documents_vec and documents_vec_ids must exist.
	for _, tbl := range []string{"documents_vec", "documents_vec_ids"} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE name = ?`, tbl,
		).Scan(&count); err != nil {
			t.Fatalf("probe %s: %v", tbl, err)
		}
		if count == 0 {
			t.Errorf("table %s missing after migration 0016", tbl)
		}
	}

	// documents_vec_ids must have document_uid, chunk_index, heading, byte_offset.
	for _, col := range []string{"id", "document_uid", "chunk_index", "heading", "byte_offset"} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info('documents_vec_ids') WHERE name = ?`, col,
		).Scan(&count); err != nil {
			t.Fatalf("pragma_table_info documents_vec_ids col %s: %v", col, err)
		}
		if count == 0 {
			t.Errorf("documents_vec_ids.%s missing after migration 0016", col)
		}
	}
}

// TestMigration0016_FTS_Triggers verifies that the AFTER INSERT trigger
// populates documents_fts and that the FTS search returns the inserted row.
func TestMigration0016_FTS_Triggers(t *testing.T) {
	t.Parallel()
	db := newWriterDB(t)
	ctx := t.Context()

	ms, err := loadMigrations(migrations.SQLite)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if err := runMigrations(ctx, db, ms); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	// Insert a document row — the AFTER INSERT trigger should index it.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO documents
			(document_uid, path, slug, title, body, content_hash, revision_uid,
			 create_time, update_time)
		VALUES
			('DOCUID0001', 'notes/alpha.md', 'alpha', 'Alpha Project',
			 'This document is about the alpha initiative unique token UNIQALPHA',
			 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
			 'REVUID0001', '2026-06-05T10:00:00Z', '2026-06-05T10:00:00Z')
	`); err != nil {
		t.Fatalf("insert document: %v", err)
	}

	// FTS search by title should find the row.
	rows, err := db.QueryContext(ctx,
		`SELECT rowid FROM documents_fts WHERE documents_fts MATCH 'alpha'`)
	if err != nil {
		t.Fatalf("FTS search: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var count int
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("FTS rows: %v", err)
	}
	if count == 0 {
		t.Error("FTS search for 'alpha' returned 0 rows; AFTER INSERT trigger may not have fired")
	}

	// FTS search by unique body token must find the row.
	rows2, err := db.QueryContext(ctx,
		`SELECT rowid FROM documents_fts WHERE documents_fts MATCH 'UNIQALPHA'`)
	if err != nil {
		t.Fatalf("FTS body search: %v", err)
	}
	defer func() { _ = rows2.Close() }()

	var bodyCount int
	for rows2.Next() {
		bodyCount++
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("FTS body rows: %v", err)
	}
	if bodyCount == 0 {
		t.Error("FTS body search for 'UNIQALPHA' returned 0 rows; body column not indexed")
	}
}

// TestMigration0016_VecIds_UniqueConstraint verifies that documents_vec_ids
// enforces uniqueness on the id column and allows multiple chunks per document.
func TestMigration0016_VecIds_UniqueConstraint(t *testing.T) {
	t.Parallel()
	db := newWriterDB(t)
	ctx := t.Context()

	ms, err := loadMigrations(migrations.SQLite)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if err := runMigrations(ctx, db, ms); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	// Two chunks for the same document (different IDs) must be accepted.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO documents_vec_ids(id, document_uid, chunk_index)
		 VALUES ('DOCUID0001#0', 'DOCUID0001', 0)`,
	); err != nil {
		t.Errorf("insert chunk 0: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO documents_vec_ids(id, document_uid, chunk_index)
		 VALUES ('DOCUID0001#1', 'DOCUID0001', 1)`,
	); err != nil {
		t.Errorf("insert chunk 1: %v", err)
	}

	// Duplicate id must be rejected.
	if _, dupErr := db.ExecContext(ctx,
		`INSERT INTO documents_vec_ids(id, document_uid, chunk_index)
		 VALUES ('DOCUID0001#0', 'DOCUID0001', 0)`,
	); dupErr == nil {
		t.Error("duplicate id in documents_vec_ids should be rejected (UNIQUE constraint)")
	}
}

// TestMigration0016_Idempotent verifies that running the full migration set a
// second time does not error. The runner skips already-applied versions via the
// _migrations bookkeeping table; this test guards against accidental
// non-idempotent DDL in future edits to the migration file.
func TestMigration0016_Idempotent(t *testing.T) {
	t.Parallel()
	db := newWriterDB(t)
	ctx := t.Context()

	ms, err := loadMigrations(migrations.SQLite)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if err := runMigrations(ctx, db, ms); err != nil {
		t.Fatalf("first runMigrations: %v", err)
	}
	if err := runMigrations(ctx, db, ms); err != nil {
		t.Fatalf("second runMigrations (idempotency check): %v", err)
	}
}
