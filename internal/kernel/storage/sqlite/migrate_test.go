package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/migrations"
)

func TestLoadMigrations_OrderAndParsing(t *testing.T) {
	fsys := fstest.MapFS{
		"sqlite/0003_third.up.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE c (id INTEGER);")},
		"sqlite/0001_first.up.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE a (id INTEGER);")},
		"sqlite/0002_second.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE b (id INTEGER);")},
	}

	ms, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) != 3 {
		t.Fatalf("len(ms) = %d, want 3", len(ms))
	}

	wantVersions := []int{1, 2, 3}
	wantNames := []string{"first", "second", "third"}
	for i, m := range ms {
		if m.version != wantVersions[i] {
			t.Errorf("ms[%d].version = %d, want %d", i, m.version, wantVersions[i])
		}
		if m.name != wantNames[i] {
			t.Errorf("ms[%d].name = %q, want %q", i, m.name, wantNames[i])
		}
	}
}

func TestLoadMigrations_RejectsBadFilenames(t *testing.T) {
	cases := []struct {
		label  string
		fsys   fs.FS
		errSub string
	}{
		{
			label: "no numeric prefix",
			fsys: fstest.MapFS{
				"sqlite/initial.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			errSub: "malformed",
		},
		{
			label: "down direction",
			fsys: fstest.MapFS{
				"sqlite/0001_initial.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE foo;")},
			},
			errSub: "malformed",
		},
		{
			label: "dash separator",
			fsys: fstest.MapFS{
				"sqlite/0001-initial.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			errSub: "malformed",
		},
		{
			label: "missing name",
			fsys: fstest.MapFS{
				"sqlite/0001_.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			},
			errSub: "malformed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			_, err := loadMigrations(tc.fsys)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrMigrationBadFilename) {
				t.Errorf("err = %v, want ErrMigrationBadFilename", err)
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("err = %v, want substring %q", err, tc.errSub)
			}
		})
	}
}

func TestLoadMigrations_RejectsDuplicateVersions(t *testing.T) {
	fsys := fstest.MapFS{
		"sqlite/0001_first.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		"sqlite/0001_other.up.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
	}
	_, err := loadMigrations(fsys)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrMigrationDuplicateVersion) {
		t.Errorf("err = %v, want ErrMigrationDuplicateVersion", err)
	}
}

func TestLoadMigrations_SubstitutesVectorDim(t *testing.T) {
	fsys := fstest.MapFS{
		"sqlite/0099_vec_template.up.sql": &fstest.MapFile{
			Data: []byte("CREATE VIRTUAL TABLE t USING vec0(embedding float[{{VECTOR_DIM}}]);"),
		},
	}
	ms, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("len(ms) = %d, want 1", len(ms))
	}
	if strings.Contains(ms[0].sql, "{{VECTOR_DIM}}") {
		t.Errorf("placeholder still present in SQL body: %q", ms[0].sql)
	}
	if !strings.Contains(ms[0].sql, "float[1024]") {
		t.Errorf("VECTOR_DIM substitution missing; got %q", ms[0].sql)
	}
}

func TestLoadMigrations_EmbeddedProductionFS(t *testing.T) {
	// Smoke check that the production embed.FS parses without error and
	// returns at least the 0001 migration.
	ms, err := loadMigrations(migrations.SQLite)
	if err != nil {
		t.Fatalf("loadMigrations(production): %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("production migrations empty; expected at least 0001")
	}
	if ms[0].version != 1 {
		t.Errorf("ms[0].version = %d, want 1", ms[0].version)
	}
	if ms[0].name != "initial_schema" {
		t.Errorf("ms[0].name = %q, want %q", ms[0].name, "initial_schema")
	}
}

// newWriterDB opens a fresh in-memory or file-backed SQLite for runner tests.
// We use a file-backed DB (not :memory:) so that the writer-only constraint
// matches production behavior, and because :memory: drops state between
// connections in some setups.
func newWriterDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner.db")
	// Register the sqlite-vec-aware driver so migrations creating vec0
	// virtual tables (0003, 0005) succeed even when this helper runs
	// before any production sqlite.Open call in the same test process.
	registerVecDriver()
	db, err := sql.Open(driverName, "file:"+path+"?_journal=WAL&_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRunMigrations_AppliesPending(t *testing.T) {
	db := newWriterDB(t)
	ctx := t.Context()

	ms := []migration{
		{version: 1, name: "first", sql: "CREATE TABLE t1 (id INTEGER);"},
		{version: 2, name: "second", sql: "CREATE TABLE t2 (id INTEGER);"},
	}
	if err := runMigrations(ctx, db, ms); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM _migrations").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("_migrations count = %d, want 2", n)
	}

	// Both tables exist.
	for _, table := range []string{"t1", "t2"} {
		var name string
		err := db.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}
}

func TestRunMigrations_SkipsApplied(t *testing.T) {
	db := newWriterDB(t)
	ctx := t.Context()

	ms := []migration{
		{version: 1, name: "first", sql: "CREATE TABLE t1 (id INTEGER);"},
	}
	if err := runMigrations(ctx, db, ms); err != nil {
		t.Fatalf("first runMigrations: %v", err)
	}

	var appliedAt1 string
	if err := db.QueryRowContext(ctx, "SELECT applied_at FROM _migrations WHERE version=1").Scan(&appliedAt1); err != nil {
		t.Fatalf("read applied_at: %v", err)
	}

	// Second run with the same migrations must be a no-op. The applied_at
	// timestamp must not change because the runner skips already-applied
	// versions; no wall-clock sleep needed.
	if err := runMigrations(ctx, db, ms); err != nil {
		t.Fatalf("second runMigrations: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM _migrations").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("_migrations count after re-run = %d, want 1", n)
	}

	var appliedAt2 string
	if err := db.QueryRowContext(ctx, "SELECT applied_at FROM _migrations WHERE version=1").Scan(&appliedAt2); err != nil {
		t.Fatalf("read applied_at after re-run: %v", err)
	}
	if appliedAt1 != appliedAt2 {
		t.Errorf("applied_at changed across re-run: %q -> %q", appliedAt1, appliedAt2)
	}
}

// TestRunMigrations_DriftDetection guards against the silent-skip
// surfaced by Greptile on PR #71: a shipped migration's content can be
// edited or renamed while keeping the same numeric prefix, and the
// version-only comparison would skip the new file on databases that
// already applied the old one. The runner must surface the inconsistency.
func TestRunMigrations_DriftDetection(t *testing.T) {
	db := newWriterDB(t)
	ctx := t.Context()

	original := []migration{
		{version: 1, name: "initial_schema", sql: "CREATE TABLE t1 (id INTEGER);"},
	}
	if err := runMigrations(ctx, db, original); err != nil {
		t.Fatalf("first runMigrations: %v", err)
	}

	// Same version, different name: the contributor renamed the file in
	// place. Forward-only migrations forbid this; the runner must error.
	renamed := []migration{
		{version: 1, name: "renamed_schema", sql: "CREATE TABLE t1 (id INTEGER);"},
	}
	err := runMigrations(ctx, db, renamed)
	if err == nil {
		t.Fatal("runMigrations: expected ErrMigrationDrift, got nil")
	}
	if !errors.Is(err, ErrMigrationDrift) {
		t.Errorf("err = %v, want ErrMigrationDrift", err)
	}
}

func TestRunMigrations_AtomicOnError(t *testing.T) {
	db := newWriterDB(t)
	ctx := t.Context()

	ms := []migration{
		{version: 1, name: "good", sql: "CREATE TABLE t1 (id INTEGER);"},
		{version: 2, name: "bad", sql: "THIS IS NOT VALID SQL;"},
	}
	err := runMigrations(ctx, db, ms)
	if err == nil {
		t.Fatal("runMigrations: expected error, got nil")
	}

	// _migrations contains row for v1 but not v2.
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM _migrations").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("_migrations count after failed run = %d, want 1", n)
	}

	var v int
	if err := db.QueryRowContext(ctx, "SELECT version FROM _migrations").Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != 1 {
		t.Errorf("_migrations[0].version = %d, want 1", v)
	}

	// t1 exists (committed by v1), t2 does not exist (rolled back).
	var tname string
	if err := db.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE name='t1'").Scan(&tname); err != nil {
		t.Errorf("t1 missing: %v", err)
	}
	err = db.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE name='t2'").Scan(&tname)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("t2 should not exist after rollback; query err = %v", err)
	}
}

func TestRunMigrations_ContextCancelled(t *testing.T) {
	db := newWriterDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancel

	ms := []migration{
		{version: 1, name: "first", sql: "CREATE TABLE t1 (id INTEGER);"},
	}
	err := runMigrations(ctx, db, ms)
	if err == nil {
		t.Fatal("runMigrations with cancelled ctx: expected error, got nil")
	}
}

func TestRunMigrations_NoOpOnEmpty(t *testing.T) {
	db := newWriterDB(t)
	if err := runMigrations(t.Context(), db, nil); err != nil {
		t.Fatalf("runMigrations with nil slice: %v", err)
	}

	// _migrations table is created even with no pending migrations.
	var tname string
	if err := db.QueryRowContext(t.Context(), "SELECT name FROM sqlite_master WHERE name='_migrations'").Scan(&tname); err != nil {
		t.Errorf("_migrations table not created: %v", err)
	}
}
