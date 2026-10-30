package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/migrations"
)

// driverName is the name we register for our customised sqlite3 driver that
// loads the sqlite-vec extension into every new connection. It is distinct
// from the default "sqlite3" driver name registered by mattn/go-sqlite3 in
// its init so that consumers of the kernel storage cannot accidentally open
// the database without the extension wired in.
const driverName = "sqlite3_vec"

// registerVecDriver registers the sqlite-vec-aware driver exactly once per
// process. Sqlite_vec.Auto wires the extension as a SQLite auto-extension
// at the C level via sqlite3_auto_extension; every connection opened by
// mattn's driver afterwards loads the extension automatically. The
// ConnectHook then runs a tiny smoke query (vec_version()) on each new
// connection so a missing or broken extension surfaces immediately at
// PingContext time with a clear error.
//
// The Go sql package panics on a duplicate sql.Register call, so the
// registration is fenced behind sync.OnceFunc to allow multiple Store
// opens in the same process (e.g. the unit-test suite).
//
//nolint:gochecknoglobals // process-lifetime driver registration, pay-once via sync.OnceFunc
var registerVecDriver = sync.OnceFunc(func() {
	sqlite_vec.Auto()
	sql.Register(driverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			rows, err := conn.Query("SELECT vec_version()", nil)
			if err != nil {
				return fmt.Errorf("sqlite-vec: vec_version probe failed (extension not loaded): %w", err)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("sqlite-vec: close vec_version probe rows: %w", err)
			}
			return nil
		},
	})
})

// Store owns the writer and reader pools for a single SQLite database file.
// Open returns a *Store with all pending migrations applied. The writer pool
// serializes writes (MaxOpenConns=1, matching SQLite's single-writer reality);
// the reader pool opens the same file in mode=ro and allows concurrent reads
// via the shared WAL.
type Store struct {
	writer *sql.DB
	reader *sql.DB
	path   string
}

// Open creates the parent directory if it does not exist, opens the writer
// pool with the production pragmas, runs every pending migration, then opens
// the read-only reader pool. The returned *Store is ready to use.
//
// Open is idempotent: a second call against the same path observes the
// applied migrations in _migrations and skips them.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlite: empty path")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("sqlite: create parent dir: %w", err)
	}

	writer, err := openWriter(ctx, path)
	if err != nil {
		return nil, err
	}

	if err := applyPragmas(ctx, writer); err != nil {
		_ = writer.Close()
		return nil, err
	}

	ms, err := loadMigrations(migrations.SQLite)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := runMigrations(ctx, writer, ms); err != nil {
		_ = writer.Close()
		return nil, err
	}

	reader, err := openReader(ctx, path)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}

	return &Store{writer: writer, reader: reader, path: path}, nil
}

// Writer returns the serializing writer pool. Callers must not call Close on
// the returned handle; the Store owns its lifecycle.
func (s *Store) Writer() *sql.DB { return s.writer }

// Reader returns the read-only reader pool. Callers must not call Close on
// the returned handle; the Store owns its lifecycle.
func (s *Store) Reader() *sql.DB { return s.reader }

// Path returns the absolute path of the SQLite database file backing this
// Store. Useful for tests and observability.
func (s *Store) Path() string { return s.path }

// Close shuts down both pools. Safe to call multiple times; subsequent
// calls return nil. Callers should not use Writer or Reader after Close.
func (s *Store) Close() error {
	var errs []error
	if s.writer != nil {
		if err := s.writer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close writer: %w", err))
		}
		s.writer = nil
	}
	if s.reader != nil {
		if err := s.reader.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close reader: %w", err))
		}
		s.reader = nil
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("sqlite: close: %w", errors.Join(errs...))
}

// openWriter opens the writer pool with the production DSN flags and
// pins MaxOpenConns to 1. Pragmas are also applied via PRAGMA after open
// for defense in depth (some flags are version-sensitive).
func openWriter(ctx context.Context, path string) (*sql.DB, error) {
	return openPool(ctx, "writer", writerDSN(path), 1, 1)
}

// openReader opens the reader pool in read-only mode. Allows multiple
// concurrent readers backed by the shared WAL file.
func openReader(ctx context.Context, path string) (*sql.DB, error) {
	return openPool(ctx, "reader", readerDSN(path), runtime.NumCPU(), 2)
}

// OpenReadOnly opens ONLY a read-only connection pool for an EXISTING, already
// migrated database — no writer pool and no migrations. It is the lock-tolerant
// read path: because the writer set WAL, a daemon may hold the writer while this
// pool serves concurrent cross-process reads of the daemon's committed state.
// A CLI read command uses it to answer `schema predicates`/`entity-kinds` while
// a daemon owns the store (the full in-process OpenWiring would block on the
// daemon's graph-DB lock). The caller owns the returned *sql.DB and must Close
// it. Fails if path does not exist (mode=ro never creates the file) — the
// signal that no store has been provisioned, so the caller should fall back to
// the in-process wiring path that migrates a fresh store.
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("sqlite: empty path")
	}
	return openReader(ctx, path)
}

// openPool is the shared body of openWriter and openReader. Role is
// embedded in error messages ("open writer", "ping reader") to keep
// failure surfaces distinguishable without duplicating the open/ping
// scaffolding.
func openPool(ctx context.Context, role, dsn string, maxOpen, maxIdle int) (*sql.DB, error) {
	registerVecDriver()
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", role, err)
	}

	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: ping %s: %w", role, err)
	}
	return db, nil
}

// writerDSN builds the writer DSN with WAL, foreign keys, busy timeout,
// NORMAL synchronous, and an immediate-lock transaction policy.
func writerDSN(path string) string {
	q := url.Values{}
	q.Set("_journal", "WAL")
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	q.Set("_synchronous", "NORMAL")
	q.Set("_txlock", "immediate")
	return fmt.Sprintf("file:%s?%s", escapeURIPath(path), q.Encode())
}

// readerDSN builds the read-only DSN. The writer set WAL on the shared
// file; readers inherit it.
func readerDSN(path string) string {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	return fmt.Sprintf("file:%s?%s", escapeURIPath(path), q.Encode())
}

// escapeURIPath percent-encodes the characters that would be misread as
// URI syntax when a raw filesystem path is spliced into a SQLite URI of
// the form "file:<path>?<query>". The encoded set is the minimum that
// breaks the parse: `%` first (to avoid double-encoding subsequent
// escapes), then `?` and `#`. All other path bytes (slashes, spaces,
// dots) pass through unchanged; SQLite tolerates them in URI paths.
func escapeURIPath(p string) string {
	r := strings.NewReplacer(
		"%", "%25",
		"?", "%3F",
		"#", "%23",
	)
	return r.Replace(p)
}

// applyPragmas issues PRAGMA statements after open. The DSN flags already
// set these for new connections, but re-issuing is cheap and removes any
// driver-version drift. We assert the WAL pragma's return value because
// SQLite quietly downgrades to delete-mode journals on filesystems that
// cannot support WAL.
func applyPragmas(ctx context.Context, db *sql.DB) error {
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journal); err != nil {
		return fmt.Errorf("sqlite: set journal_mode=WAL: %w", err)
	}
	if journal != "wal" {
		return fmt.Errorf("sqlite: journal_mode is %q after WAL request (filesystem may not support WAL)", journal)
	}

	stmts := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = NORMAL",
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("sqlite: exec %q: %w", s, err)
		}
	}
	return nil
}
