package ladybug

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	lbug "github.com/LadybugDB/go-ladybug"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

//go:embed migrations/*.cypher
var migrationFS embed.FS

// migrationFilenamePattern matches NNNN_<name>.cypher inside the embedded
// migrations directory. The leading digits are the monotonic version; the
// rest is a human-readable slug carried into the _ladybug_migrations node
// for debuggability.
var migrationFilenamePattern = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.cypher$`)

// Conn is a kernel.GraphConn backed by an embedded LadybugDB database.
//
// The lbug Connection is not documented as concurrency-safe across goroutines
// for Query/Execute calls, so Conn serialises every Execute behind a mutex.
// Per-call latency is dominated by graph traversal, not lock contention; if
// concurrency becomes a bottleneck the M2 storage dispatcher will pool
// multiple connections rather than relax this guarantee.
//
// Lifecycle: Close blocks until every result returned by Execute has been
// Closed by its caller, so iterating a result concurrently with a shutdown
// cannot race against freed CGO state. Once Close has been called every
// further Execute returns ErrClosed.
type Conn struct {
	mu       sync.Mutex
	db       *lbug.Database
	conn     *lbug.Connection
	closed   bool
	inflight sync.WaitGroup
}

// ErrClosed is returned by Execute after Close has been called.
var ErrClosed = errors.New("ladybug: connection is closed")

// openMu serializes the go-ladybug CGO lifecycle calls — OpenDatabase /
// OpenConnection (openLocked) and the Database / Connection Close (Conn.Close) —
// across goroutines. The CGO layer is not concurrency-safe for these: concurrent
// opens or closes (even on distinct paths) intermittently panic inside the C
// library, as the parallel test suite, which opens and tears down many isolated
// graphs at once, surfaces. Production opens and closes the graph exactly once
// over a daemon's lifetime, so this guard is contention-free there; it mirrors
// the per-Conn Execute serialization documented on Conn above, extending the
// same "lbug is not concurrency-safe" guarantee to the open/close lifecycle.
//
//nolint:gochecknoglobals // process-lifetime guard for the non-reentrant CGO lifecycle
var openMu sync.Mutex

// OpenLadybug opens an embedded LadybugDB database rooted at path, applies
// any pending migrations from the embedded filesystem, and returns a *Conn
// satisfying kernel.GraphConn.
//
// Path is the filesystem location LadybugDB uses as its database root (e.g.
// ${dataDir}/graph.ladybug). The parent directory is created if missing;
// LadybugDB itself manages the internal file layout at path. Callers must
// not touch those files directly.
func OpenLadybug(ctx context.Context, path string) (*Conn, error) {
	if path == "" {
		return nil, errors.New("ladybug: path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("ladybug: create parent dir: %w", err)
	}

	db, conn, err := openLocked(path)
	if err != nil {
		return nil, err
	}

	c := &Conn{db: db, conn: conn}
	if err := c.migrate(ctx); err != nil {
		if closeErr := c.Close(); closeErr != nil {
			return nil, fmt.Errorf("ladybug: migrate failed: %w (also: close: %w)", err, closeErr)
		}
		return nil, err
	}
	return c, nil
}

// openLocked opens the LadybugDB database and a connection under openMu, so
// concurrent OpenLadybug callers never invoke the non-concurrency-safe CGO open
// path at the same time. The lock is released as soon as both handles exist;
// migration runs unlocked on the per-Conn handle.
func openLocked(path string) (*lbug.Database, *lbug.Connection, error) {
	openMu.Lock()
	defer openMu.Unlock()

	db, err := lbug.OpenDatabase(path, lbug.DefaultSystemConfig())
	if err != nil {
		return nil, nil, fmt.Errorf("ladybug: open database at %s: %w", path, err)
	}
	conn, err := lbug.OpenConnection(db)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("ladybug: open connection: %w", err)
	}
	return db, conn, nil
}

// Execute runs cypher with optional named parameters and returns a forward-
// only result iterator. The caller must Close the returned GraphResult.
//
// Returns ErrClosed if Close has already been called on this Conn. While
// the returned result is alive, Close on this Conn will block until the
// result is Closed.
func (c *Conn) Execute(ctx context.Context, cypher string, args map[string]any) (kernel.GraphResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("ladybug: %w", err)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.inflight.Add(1)
	res, err := c.executeLocked(cypher, args)
	c.mu.Unlock()
	if err != nil {
		c.inflight.Done()
		return nil, err
	}
	res.onClose = c.inflight.Done
	return res, nil
}

func (c *Conn) executeLocked(cypher string, args map[string]any) (*result, error) {
	if len(args) == 0 {
		qr, err := c.conn.Query(cypher)
		if err != nil {
			return nil, fmt.Errorf("ladybug: query: %w", err)
		}
		return &result{qr: qr}, nil
	}
	stmt, err := c.conn.Prepare(cypher)
	if err != nil {
		return nil, fmt.Errorf("ladybug: prepare: %w", err)
	}
	defer stmt.Close()
	qr, err := c.conn.Execute(stmt, args)
	if err != nil {
		return nil, fmt.Errorf("ladybug: execute: %w", err)
	}
	return &result{qr: qr}, nil
}

// Close releases the underlying lbug Connection and Database. Safe to call
// multiple times; subsequent calls are no-ops.
//
// Blocks until every result returned by Execute has been Closed by its
// caller. Closing a Conn while a result iteration is in flight from
// another goroutine would otherwise free the CGO state out from under the
// iterator and risk a panic / segfault.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	c.inflight.Wait()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Serialize the CGO teardown under the same lock that guards opens: go-ladybug
	// is not concurrency-safe across its lifecycle calls, and the parallel test
	// suite closes many graphs at once. openMu is never held while waiting on
	// c.mu (open takes only openMu), so c.mu -> openMu here cannot deadlock.
	openMu.Lock()
	defer openMu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	if c.db != nil {
		c.db.Close()
		c.db = nil
	}
	return nil
}

// migration describes one entry in the embedded migrations/ directory.
type migration struct {
	version int64
	name    string
	body    string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("ladybug: read embedded migrations: %w", err)
	}
	out := make([]migration, 0, len(entries))
	seen := make(map[int64]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilenamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("ladybug: migration filename %q does not match NNNN_<name>.cypher", e.Name())
		}
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ladybug: parse migration version in %q: %w", e.Name(), err)
		}
		if prior, dup := seen[v]; dup {
			return nil, fmt.Errorf("ladybug: duplicate migration version %d (%q and %q)", v, prior, e.Name())
		}
		seen[v] = e.Name()
		body, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("ladybug: read %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: m[2], body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// migrate ensures the schema is at the latest embedded migration version,
// applying any pending migrations in order. Idempotent: a database that is
// already current is a no-op.
//
// Holds the connection mutex for the entire run; concurrent Execute calls
// block until migration completes. Migration files must themselves be
// idempotent (use IF NOT EXISTS) so a crash mid-file does not poison the
// database for the next OpenLadybug.
func (c *Conn) migrate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("ladybug: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	r, err := c.executeLocked(`CREATE NODE TABLE IF NOT EXISTS _ladybug_migrations(version INT64, name STRING, appliedAt TIMESTAMP, PRIMARY KEY(version))`, nil)
	if err != nil {
		return fmt.Errorf("ladybug: ensure migrations table: %w", err)
	}
	if cerr := r.Close(); cerr != nil {
		return fmt.Errorf("ladybug: close ensure-migrations result: %w", cerr)
	}

	current, err := c.currentVersionLocked()
	if err != nil {
		return err
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := c.applyMigrationLocked(m); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conn) currentVersionLocked() (version int64, err error) {
	r, err := c.executeLocked(`MATCH (m:_ladybug_migrations) RETURN max(m.version)`, nil)
	if err != nil {
		return 0, fmt.Errorf("ladybug: query current version: %w", err)
	}
	defer func() {
		if cerr := r.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("ladybug: close current-version result: %w", cerr)
		}
	}()
	if !r.Next() {
		return 0, r.Err()
	}
	// max(...) over an empty node table returns nil, which we treat as
	// version 0 (no migrations applied yet).
	values, err := r.values()
	if err != nil {
		return 0, fmt.Errorf("ladybug: scan current version: %w", err)
	}
	if len(values) == 0 || values[0] == nil {
		return 0, nil
	}
	v, ok := values[0].(int64)
	if !ok {
		return 0, fmt.Errorf("ladybug: unexpected version type %T", values[0])
	}
	return v, nil
}

func (c *Conn) applyMigrationLocked(m migration) error {
	for i, stmt := range splitCypherStatements(m.body) {
		r, err := c.executeLocked(stmt, nil)
		if err != nil {
			return fmt.Errorf("ladybug: migration %04d (%s) statement %d: %w", m.version, m.name, i+1, err)
		}
		if cerr := r.Close(); cerr != nil {
			return fmt.Errorf("ladybug: close migration %04d statement %d result: %w", m.version, i+1, cerr)
		}
	}
	// Lbug accepts ISO-8601-shaped strings via the timestamp(...) function.
	// Bind the version and name as parameters so any future identifiers with
	// special characters cannot break the literal.
	appliedAt := time.Now().UTC().Format(time.RFC3339Nano)
	insert := fmt.Sprintf(
		`CREATE (m:_ladybug_migrations {version: $version, name: $name, appliedAt: timestamp('%s')})`,
		appliedAt,
	)
	r, err := c.executeLocked(insert, map[string]any{
		"version": m.version,
		"name":    m.name,
	})
	if err != nil {
		return fmt.Errorf("ladybug: record migration %04d: %w", m.version, err)
	}
	if cerr := r.Close(); cerr != nil {
		return fmt.Errorf("ladybug: close record-migration result: %w", cerr)
	}
	return nil
}

// splitCypherStatements separates a multi-statement migration file into
// individual statements terminated by `;`. Lbug's Connection.Query expects
// one statement per call.
//
// `// ...` line comments are stripped before splitting. Migration files in
// this repo never embed `;` inside string literals; if one ever needs to,
// rework this helper at that time (YAGNI per AGENTS.md "Simplicity First").
func splitCypherStatements(body string) []string {
	stripped := make([]string, 0, strings.Count(body, "\n"))
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		stripped = append(stripped, line)
	}
	parts := strings.Split(strings.Join(stripped, "\n"), ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if stmt := strings.TrimSpace(p); stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
