package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
)

// vectorDimPlaceholder is the textual sentinel inside a migration file that
// loadMigrations swaps out for storage.VectorDim at load time. The vec0
// virtual-table DDL takes the dim as a literal token (CREATE VIRTUAL TABLE
// ... USING vec0(embedding float[N])) rather than as a bound parameter, so
// we template it here instead of letting it drift across the codebase.
const vectorDimPlaceholder = "{{VECTOR_DIM}}"

// migrationsDir is the path within the embedded FS where the SQLite
// migration files live. The migrations package roots its embed at
// internal/kernel/storage/migrations/, so files are at sqlite/NNNN_*.up.sql.
const migrationsDir = "sqlite"

// Migration runner sentinel errors. Returned via fmt.Errorf wrapping with
// the offending filename or version.
var (
	// ErrMigrationBadFilename signals a migration file whose name does not
	// match NNNN_<name>.up.sql.
	ErrMigrationBadFilename = errors.New("sqlite: malformed migration filename")

	// ErrMigrationDuplicateVersion signals two migration files that parse to
	// the same numeric version.
	ErrMigrationDuplicateVersion = errors.New("sqlite: duplicate migration version")

	// ErrMigrationDrift signals that a migration version already recorded
	// in _migrations was applied under a different name than the current
	// in-tree migration file uses. The drift implies a shipped migration
	// has been renamed or edited in place; forward-only migrations forbid
	// this, so the runner surfaces the inconsistency instead of silently
	// skipping the new file.
	ErrMigrationDrift = errors.New("sqlite: migration drift detected")
)

// migration is one forward-only migration loaded from the embed FS.
type migration struct {
	version int
	name    string
	sql     string
}

// migrationFilenameRE matches NNNN_<name>.up.sql where NNNN is one or more
// digits and <name> is one or more characters that are not slashes or dots.
// The runner never executes .down.sql files; forward-only by design.
var migrationFilenameRE = regexp.MustCompile(`^([0-9]+)_([^/.]+)\.up\.sql$`)

// loadMigrations walks fsys under migrationsDir, parses each filename, and
// returns the resulting migrations sorted by version ascending. Any
// malformed name or duplicate version aborts the load.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read %s: %w", migrationsDir, err)
	}

	ms := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		match := migrationFilenameRE.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("%w: %q", ErrMigrationBadFilename, name)
		}

		// The regex guarantees match[1] is one or more decimal digits; the
		// belt-and-braces error check satisfies errcheck without adding
		// reachable code paths.
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %w", ErrMigrationBadFilename, name, err)
		}
		if prior, ok := seen[version]; ok {
			return nil, fmt.Errorf("%w: %d (files %q and %q)", ErrMigrationDuplicateVersion, version, prior, name)
		}
		seen[version] = name

		body, err := fs.ReadFile(fsys, migrationsDir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("sqlite: read %s: %w", name, err)
		}

		sqlBody := strings.ReplaceAll(string(body), vectorDimPlaceholder, strconv.Itoa(storage.VectorDim))

		ms = append(ms, migration{
			version: version,
			name:    match[2],
			sql:     strings.TrimSpace(sqlBody),
		})
	}

	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// runMigrations creates the _migrations bookkeeping table if absent, then
// for each migration in version order: skips if already applied, otherwise
// runs the SQL plus the _migrations insert inside a single BEGIN IMMEDIATE
// transaction. A failure during the migration rolls back the partial
// schema and the _migrations row together.
//
// Db must be the writer pool (SetMaxOpenConns=1). Running migrations
// against the reader pool will fail because the reader is read-only.
func runMigrations(ctx context.Context, db *sql.DB, ms []migration) error {
	const createBookkeeping = `
CREATE TABLE IF NOT EXISTS _migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TEXT NOT NULL
);`
	if _, err := db.ExecContext(ctx, createBookkeeping); err != nil {
		return fmt.Errorf("sqlite: create _migrations: %w", err)
	}

	applied, err := loadAppliedVersions(ctx, db)
	if err != nil {
		return err
	}

	for _, m := range ms {
		if appliedName, ok := applied[m.version]; ok {
			if appliedName != m.name {
				return fmt.Errorf("%w: version %d applied as %q, in-tree file is %q",
					ErrMigrationDrift, m.version, appliedName, m.name)
			}
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("sqlite: apply migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

// loadAppliedVersions reads the (version, name) pairs from _migrations.
// Returning the name lets the caller surface migration drift (a shipped
// migration was renamed in place) instead of silently skipping the new
// file. See ErrMigrationDrift.
func loadAppliedVersions(ctx context.Context, db *sql.DB) (map[int]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT version, name FROM _migrations")
	if err != nil {
		return nil, fmt.Errorf("sqlite: query _migrations: %w", err)
	}
	defer rows.Close()

	out := make(map[int]string)
	for rows.Next() {
		var (
			v int
			n string
		)
		if err := rows.Scan(&v, &n); err != nil {
			return nil, fmt.Errorf("sqlite: scan _migrations: %w", err)
		}
		out[v] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate _migrations: %w", err)
	}
	return out, nil
}

// applyMigration executes one migration inside a single transaction with
// the matching _migrations row, then commits. If any step fails, the
// transaction is rolled back and the original error is preserved; any
// rollback failure is joined onto the returned error for diagnostics.
//
// The in-transaction recheck against _migrations handles the concurrent-
// fresh-open race: if two processes call Open against the same path
// simultaneously, both can read an empty bookkeeping table before either
// acquires the writer lock. Without the recheck, the loser would replay
// the same non-idempotent CREATE TABLE statements and fail with
// "table already exists". With it, the loser observes the winner's row
// inside the lock and commits an empty transaction instead.
func applyMigration(ctx context.Context, db *sql.DB, m migration) (retErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if retErr == nil {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = fmt.Errorf("%w (rollback: %w)", retErr, rbErr)
		}
	}()

	var existingName string
	err = tx.QueryRowContext(ctx,
		"SELECT name FROM _migrations WHERE version = ?", m.version,
	).Scan(&existingName)
	switch {
	case err == nil:
		// Another opener applied this version while we were waiting for
		// the writer lock. Honour drift detection inside the lock too.
		if existingName != m.name {
			return fmt.Errorf("%w: version %d applied as %q, in-tree file is %q",
				ErrMigrationDrift, m.version, existingName, m.name)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit recheck tx: %w", err)
		}
		return nil
	case errors.Is(err, sql.ErrNoRows):
		// Not applied yet; fall through to run the migration.
	default:
		return fmt.Errorf("recheck _migrations: %w", err)
	}

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("exec migration sql: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO _migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("record _migrations row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
