package sqlite

// migration_0005_test.go: integration tests for the 0005_multivec_spatial
// migration. Applies the production embedded FS through migration 0005 and
// asserts all expected schema objects exist and behave correctly.
//
// Acceptance criteria (P4):
//   - entities_vec_ids gains a composite (id, vector_type) unique key; two
//     rows for the same entity with different vector_type values are accepted
//     and a duplicate (id, vector_type) is rejected.
//   - mentions_vec and mentions_vec_ids tables exist.
//   - entities gains latitude, longitude, coordinates_provenance, create_time.
//   - mentions gains extraction_text.
//   - The both-or-neither trigger fires when only one coordinate is set.
//   - Running the full migration set twice is idempotent (runner skips).

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/migrations"
)

// TestMigration0005_Up_ObjectsExist applies all production migrations and
// asserts every schema object introduced by 0005 is present and queryable.
func TestMigration0005_Up_ObjectsExist(t *testing.T) {
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

	// entities_vec_ids must accept two rows for the same entity with distinct
	// vector_type values and reject a duplicate (id, vector_type) pair.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO entities_vec_ids(id, vector_type) VALUES ('UID001', 'name')`,
	); err != nil {
		t.Errorf("entities_vec_ids insert name: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO entities_vec_ids(id, vector_type) VALUES ('UID001', 'summary')`,
	); err != nil {
		t.Errorf("entities_vec_ids insert summary: %v", err)
	}
	if _, dupErr := db.ExecContext(ctx,
		`INSERT INTO entities_vec_ids(id, vector_type) VALUES ('UID001', 'name')`,
	); dupErr == nil {
		t.Error("duplicate (id, vector_type) in entities_vec_ids should be rejected")
	}

	// mentions_vec and mentions_vec_ids must exist.
	for _, tbl := range []string{"mentions_vec", "mentions_vec_ids"} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE name = ?`, tbl,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", tbl, err)
		}
		if n == 0 {
			t.Errorf("table %s missing after migration 0005", tbl)
		}
	}

	// entities must have the new spatial + create_time columns.
	for _, col := range []string{"latitude", "longitude", "coordinates_provenance", "create_time"} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info('entities') WHERE name = ?`, col,
		).Scan(&count); err != nil {
			t.Fatalf("pragma_table_info entities col %s: %v", col, err)
		}
		if count == 0 {
			t.Errorf("entities.%s missing after migration 0005", col)
		}
	}

	// mentions must have the extraction_text column.
	var etCount int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('mentions') WHERE name = 'extraction_text'`,
	).Scan(&etCount); err != nil {
		t.Fatalf("pragma_table_info mentions extraction_text: %v", err)
	}
	if etCount == 0 {
		t.Error("mentions.extraction_text missing after migration 0005")
	}
}

// TestMigration0005_CoordBothOrNeither asserts that the BEFORE INSERT trigger
// aborts when exactly one of (latitude, longitude) is NULL.
func TestMigration0005_CoordBothOrNeither(t *testing.T) {
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

	cases := []struct {
		label   string
		uid     string
		lat     any
		lon     any
		wantErr bool
	}{
		{label: "both set", uid: "UID00A", lat: 52.52, lon: 13.40, wantErr: false},
		{label: "both NULL", uid: "UID00B", lat: nil, lon: nil, wantErr: false},
		{label: "lat only", uid: "UID00C", lat: 52.52, lon: nil, wantErr: true},
		{label: "lon only", uid: "UID00D", lat: nil, lon: 13.40, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			_, err := db.ExecContext(ctx,
				`INSERT INTO entities
					(uid, kind, display_name, confidence, resolution_status, latitude, longitude)
				 VALUES (?, 'place', 'TestCity', 1.0, 'confirmed', ?, ?)`,
				tc.uid, tc.lat, tc.lon,
			)
			if tc.wantErr && err == nil {
				t.Errorf("[%s] expected trigger error, got nil", tc.label)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("[%s] unexpected error: %v", tc.label, err)
			}
		})
	}
}

// TestMigration0005_Idempotent verifies that running the full migration set a
// second time does not error. The runner skips already-applied versions via the
// _migrations bookkeeping table; this test guards against accidental non-
// idempotent DDL (e.g. CREATE TABLE without IF NOT EXISTS) in future edits.
func TestMigration0005_Idempotent(t *testing.T) {
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
