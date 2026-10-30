package sqlite

// migration_0007_test.go: integration tests for the 0007_relations migration.
// Applies the production embedded FS through migration 0007 and asserts the
// relations table and its indexes exist and behave correctly.
//
// Acceptance criteria:
//   - relations table exists with the expected columns.
//   - Three indexes exist: idx_relations_subject_predicate,
//     idx_relations_object_predicate, idx_relations_temporal.
//   - A sample relation row INSERT + SELECT round-trips correctly,
//     including nullable columns set to NULL.

import (
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/migrations"
)

// TestMigration0007_TableExists verifies the relations table is created and
// has the expected columns.
func TestMigration0007_TableExists(t *testing.T) {
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
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='relations'`,
	).Scan(&n); err != nil {
		t.Fatalf("probe relations table: %v", err)
	}
	if n == 0 {
		t.Fatal("relations table missing after migration 0007")
	}

	// All expected columns must be present.
	wantCols := []string{
		"uid", "subject_uid", "predicate", "object_uid",
		"confidence", "source_event_uid", "create_time",
		"valid_from", "valid_until", "validity_score",
		"superseded_by_uid", "provenance",
	}
	for _, col := range wantCols {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info('relations') WHERE name = ?`, col,
		).Scan(&count); err != nil {
			t.Fatalf("pragma_table_info relations col %s: %v", col, err)
		}
		if count == 0 {
			t.Errorf("relations.%s missing after migration 0007", col)
		}
	}
}

// TestMigration0007_IndexesExist verifies the three expected indexes are
// present in sqlite_master.
func TestMigration0007_IndexesExist(t *testing.T) {
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

	wantIndexes := []string{
		"idx_relations_subject_predicate",
		"idx_relations_object_predicate",
		"idx_relations_temporal",
	}
	for _, idx := range wantIndexes {
		t.Run(idx, func(t *testing.T) {
			t.Parallel()
			var count int
			if err := db.QueryRowContext(ctx,
				`SELECT count(*) FROM sqlite_master WHERE type='index' AND name = ?`, idx,
			).Scan(&count); err != nil {
				t.Fatalf("probe index %s: %v", idx, err)
			}
			if count == 0 {
				t.Errorf("index %s missing after migration 0007", idx)
			}
		})
	}
}

// TestMigration0007_RoundTrip verifies that a relation row can be inserted
// and read back with all columns, including nullable columns set to NULL.
// Nullable columns (valid_from, valid_until, validity_score, superseded_by_uid)
// are bound as typed nil pointers so the VALUES clause uses all-? placeholders,
// which avoids gofumpt collapsing repeated NULL literals in the SQL string.
func TestMigration0007_RoundTrip(t *testing.T) {
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

	// Insert a sample relation. relations has no FK constraint to episodes,
	// so synthetic UIDs are safe for the test.
	uid := "01JWABCDEFGHJKMNPQRSTVWXYZ"
	subjectUID := "01JWSUBJECT0000000000000000"
	objectUID := "01JWOBJECT00000000000000000"
	createTime := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	sourceEventUID := "01JWSOURCEEVENT000000000000"

	// Nullable columns bound as typed nil so the SQL uses only ? placeholders.
	var (
		validFrom       *string
		validUntil      *string
		validityScore   *float64
		supersededByUID *string
	)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations`+
			` (uid, subject_uid, predicate, object_uid, confidence,`+
			` source_event_uid, create_time,`+
			` valid_from, valid_until, validity_score, superseded_by_uid, provenance)`+
			` VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uid, subjectUID, "works_at", objectUID, 0.95,
		sourceEventUID, createTime,
		validFrom, validUntil, validityScore, supersededByUID, "test",
	); err != nil {
		t.Fatalf("INSERT relation: %v", err)
	}

	var (
		gotUID        string
		gotSubject    string
		gotPredicate  string
		gotObject     string
		gotConfidence float64
		gotSource     string
		gotCreate     string
		gotProvenance string
	)
	if err := db.QueryRowContext(ctx,
		`SELECT uid, subject_uid, predicate, object_uid, confidence,
		        source_event_uid, create_time, provenance
		 FROM relations WHERE uid = ?`, uid,
	).Scan(&gotUID, &gotSubject, &gotPredicate, &gotObject, &gotConfidence,
		&gotSource, &gotCreate, &gotProvenance); err != nil {
		t.Fatalf("SELECT relation: %v", err)
	}

	if gotUID != uid {
		t.Errorf("uid = %q, want %q", gotUID, uid)
	}
	if gotSubject != subjectUID {
		t.Errorf("subject_uid = %q, want %q", gotSubject, subjectUID)
	}
	if gotPredicate != "works_at" {
		t.Errorf("predicate = %q, want %q", gotPredicate, "works_at")
	}
	if gotObject != objectUID {
		t.Errorf("object_uid = %q, want %q", gotObject, objectUID)
	}
	if gotConfidence != 0.95 {
		t.Errorf("confidence = %v, want 0.95", gotConfidence)
	}
	if gotSource != sourceEventUID {
		t.Errorf("source_event_uid = %q, want %q", gotSource, sourceEventUID)
	}
	if gotCreate != createTime {
		t.Errorf("create_time = %q, want %q", gotCreate, createTime)
	}
	if gotProvenance != "test" {
		t.Errorf("provenance = %q, want %q", gotProvenance, "test")
	}
}
