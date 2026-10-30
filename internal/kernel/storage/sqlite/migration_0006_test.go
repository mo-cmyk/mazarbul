package sqlite

// migration_0006_test.go: integration tests for the 0006_predicates migration.
// Applies the production embedded FS through migration 0006 and asserts the
// predicates table exists with the correct schema and seed data.
//
// Acceptance criteria:
//   - predicates table exists with the expected columns.
//   - The functional seed predicates from 0006/0012/0013 are present with
//     kind = 'functional', and the document-relation predicates from 0019 are
//     present with kind = 'non-functional'.
//   - The CHECK constraint on kind rejects invalid values.
//   - The CHECK constraint on added_by rejects invalid values.

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/migrations"
)

// TestMigration0006_TableExists verifies the predicates table is created and
// has the expected schema columns.
func TestMigration0006_TableExists(t *testing.T) {
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

	// Table must be present in sqlite_master.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='predicates'`,
	).Scan(&n); err != nil {
		t.Fatalf("probe predicates table: %v", err)
	}
	if n == 0 {
		t.Fatal("predicates table missing after migration 0006")
	}

	// All expected columns must be present.
	wantCols := []string{"predicate", "kind", "description", "added_at", "added_by"}
	for _, col := range wantCols {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info('predicates') WHERE name = ?`, col,
		).Scan(&count); err != nil {
			t.Fatalf("pragma_table_info predicates col %s: %v", col, err)
		}
		if count == 0 {
			t.Errorf("predicates.%s missing after migration 0006", col)
		}
	}
}

// TestMigration0006_SeedRows verifies that the canonical seed predicates are
// present with the expected kind. The row count includes additive seeds from
// later migrations (e.g. 0012 added has_name, 0019 added the document-relation
// predicates); we assert the full expected lists explicitly rather than a magic
// number so future additive migrations only need to extend a slice. Functional
// and non-functional seeds are tracked separately because each carries a
// different kind invariant.
func TestMigration0006_SeedRows(t *testing.T) {
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

	functionalSeeds := []string{
		"works_at", "lives_in", "born_in", "child_of", "parent_of",
		"married_to", "member_of", "located_in", "has_role", "owns",
		"has_name",   // added by 0012_self_entity_seed
		"studies_at", // added by 0013_studies_at_predicate
	}
	// Document-relation predicates seeded by 0019_document_predicates plus
	// `describes` from 0041_doc_describes_predicate. All are non-functional: a
	// document may simultaneously mention/support/contradict/describe many
	// targets, so there is no "at most one current value" semantics.
	documentSeeds := []string{
		"mentions", "links_to", "relates_to", "contradicts",
		"supports", "blocks", "depends_on", "part_of",
		"describes", // added by 0041_doc_describes_predicate
	}

	var total int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM predicates`,
	).Scan(&total); err != nil {
		t.Fatalf("count predicates: %v", err)
	}
	if want := len(functionalSeeds) + len(documentSeeds); total != want {
		t.Errorf("predicates row count = %d, want %d", total, want)
	}

	assertSeed := func(name, wantKind string) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var kind, addedBy string
			if err := db.QueryRowContext(ctx,
				`SELECT kind, added_by FROM predicates WHERE predicate = ?`, name,
			).Scan(&kind, &addedBy); err != nil {
				t.Fatalf("select %s: %v", name, err)
			}
			if kind != wantKind {
				t.Errorf("%s: kind = %q, want %q", name, kind, wantKind)
			}
			if addedBy != "seed" {
				t.Errorf("%s: added_by = %q, want %q", name, addedBy, "seed")
			}
		})
	}
	for _, name := range functionalSeeds {
		assertSeed(name, "functional")
	}
	for _, name := range documentSeeds {
		assertSeed(name, "non-functional")
	}
}

// TestMigration0006_CheckConstraints verifies that the CHECK constraints on
// kind and added_by reject invalid values at the SQLite layer.
func TestMigration0006_CheckConstraints(t *testing.T) {
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

	// Invalid kind must be rejected.
	_, err = db.ExecContext(ctx,
		`INSERT INTO predicates (predicate, kind, added_at, added_by)
		 VALUES ('bad_kind', 'invalid_kind', '2026-05-25T00:00:00Z', 'seed')`,
	)
	if err == nil {
		t.Error("INSERT with invalid kind should have been rejected by CHECK constraint")
	}

	// Invalid added_by must be rejected.
	_, err = db.ExecContext(ctx,
		`INSERT INTO predicates (predicate, kind, added_at, added_by)
		 VALUES ('bad_by', 'functional', '2026-05-25T00:00:00Z', 'robot')`,
	)
	if err == nil {
		t.Error("INSERT with invalid added_by should have been rejected by CHECK constraint")
	}

	// Valid non-functional kind must be accepted.
	_, err = db.ExecContext(ctx,
		`INSERT INTO predicates (predicate, kind, added_at, added_by)
		 VALUES ('collaborates_with', 'non-functional', '2026-05-25T00:00:00Z', 'user')`,
	)
	if err != nil {
		t.Errorf("INSERT with non-functional kind failed: %v", err)
	}
}
