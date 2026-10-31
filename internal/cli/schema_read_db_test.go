package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// newReadDBTestCmd builds a minimal cobra command with the --data-dir
// PersistentFlag the schema read helpers resolve, plus captured output buffers.
func newReadDBTestCmd() *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	return root
}

// provisionIndexDB creates a migrated index.db under dir so the read-only open
// path has an existing, already-migrated store to read. It opens the full Store
// (writer + migrations) once and closes it, leaving the file on disk.
func provisionIndexDB(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "index.db")
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("provision index.db: %v", err)
	}
	if cerr := store.Close(); cerr != nil {
		t.Fatalf("close provisioned store: %v", cerr)
	}
	return path
}

// TestOpenSchemaReadDB_DaemonHeldTakesReadOnlyPath: when preflightDaemon reports
// a daemon owns the store, openSchemaReadDB must open the lock-tolerant
// read-only SQLite pool against <dataDir>/index.db rather than the in-process
// wiring (which would die on the daemon's graph-DB lock). A successful read of
// the migrated schema proves the read-only branch was taken.
func TestOpenSchemaReadDB_DaemonHeldTakesReadOnlyPath(t *testing.T) {
	// No t.Parallel: stubs the package-level preflightDaemon var.
	dir := t.TempDir()
	provisionIndexDB(t, dir)

	prev := preflightDaemon
	preflightDaemon = func(string, string) error {
		return &daemonLockHeldError{PID: 4242, StopHint: "maz daemon stop"}
	}
	t.Cleanup(func() { preflightDaemon = prev })

	cmd := newReadDBTestCmd()
	db, closeDB, err := openSchemaReadDB(context.Background(), cmd, dir, zap.NewNop())
	if err != nil {
		t.Fatalf("openSchemaReadDB (daemon held): %v", err)
	}
	t.Cleanup(closeDB)

	// The read-only pool reads the daemon's committed schema: the predicates
	// table exists in every migrated store, so a count query must succeed.
	var n int
	if qerr := db.QueryRowContext(context.Background(),
		`SELECT COUNT(1) FROM predicates`).Scan(&n); qerr != nil {
		t.Fatalf("read-only pool query failed (read-only path not taken?): %v", qerr)
	}
}

// TestOpenSchemaReadDB_DaemonHeldMissingStore: when a daemon is reported but no
// index.db exists, the read-only open fails (mode=ro never creates the file).
// The helper must surface that as an error rather than silently falling through
// to the in-process wiring.
func TestOpenSchemaReadDB_DaemonHeldMissingStore(t *testing.T) {
	// No t.Parallel: stubs the package-level preflightDaemon var.
	dir := t.TempDir() // empty: no index.db

	prev := preflightDaemon
	preflightDaemon = func(string, string) error {
		return &daemonLockHeldError{PID: 4242, StopHint: "maz daemon stop"}
	}
	t.Cleanup(func() { preflightDaemon = prev })

	cmd := newReadDBTestCmd()
	_, closeDB, err := openSchemaReadDB(context.Background(), cmd, dir, zap.NewNop())
	if err == nil {
		if closeDB != nil {
			closeDB()
		}
		t.Fatal("openSchemaReadDB with daemon held but no index.db: want error, got nil")
	}
}

// TestOpenSchemaLintDB_DaemonHeldNilEmbedder: the lint variant under a held
// daemon must take the same read-only branch AND return a nil embedder, which
// makes the near-duplicate lint check degrade to its lexical fallback (the same
// path CI/offline runs take).
func TestOpenSchemaLintDB_DaemonHeldNilEmbedder(t *testing.T) {
	// No t.Parallel: stubs the package-level preflightDaemon var.
	dir := t.TempDir()
	provisionIndexDB(t, dir)

	prev := preflightDaemon
	preflightDaemon = func(string, string) error {
		return &daemonLockHeldError{PID: 4242, StopHint: "maz daemon stop"}
	}
	t.Cleanup(func() { preflightDaemon = prev })

	cmd := newReadDBTestCmd()
	db, embedder, closeDB, err := openSchemaLintDB(context.Background(), cmd, dir, zap.NewNop())
	if err != nil {
		t.Fatalf("openSchemaLintDB (daemon held): %v", err)
	}
	t.Cleanup(closeDB)

	if embedder != nil {
		t.Errorf("daemon-held lint path must return a nil embedder (lexical fallback), got %T", embedder)
	}
	var n int
	if qerr := db.QueryRowContext(context.Background(),
		`SELECT COUNT(1) FROM entity_kinds`).Scan(&n); qerr != nil {
		t.Fatalf("read-only pool query failed (read-only path not taken?): %v", qerr)
	}
}

// TestRefuseSchemaWriteUnderDaemon covers both branches of the clean-error
// fallback used by schema alias / import / review when no equivalent MCP tool
// exists: a held daemon yields a refusal (silentError 1 + actionable stderr),
// and no daemon yields (false, nil) so the caller proceeds in-process.
func TestRefuseSchemaWriteUnderDaemon(t *testing.T) {
	// No t.Parallel: stubs the package-level preflightDaemon var.
	t.Run("daemon held refuses cleanly", func(t *testing.T) {
		prev := preflightDaemon
		preflightDaemon = func(string, string) error {
			return &daemonLockHeldError{PID: 4242, StopHint: "maz daemon stop"}
		}
		t.Cleanup(func() { preflightDaemon = prev })

		var errBuf bytes.Buffer
		cmd := &cobra.Command{Use: "maz"}
		cmd.SetErr(&errBuf)
		refused, rerr := refuseSchemaWriteUnderDaemon(cmd, t.TempDir(), "schema alias")
		if !refused {
			t.Fatal("daemon held: want refused=true")
		}
		var se silentError
		if !asSilentError(rerr, &se) || se.code != 1 {
			t.Fatalf("want silentError code 1, got %v", rerr)
		}
		if !bytes.Contains(errBuf.Bytes(), []byte("schema alias needs exclusive store access")) {
			t.Errorf("missing actionable refusal message; stderr=%q", errBuf.String())
		}
	})

	t.Run("no daemon falls through", func(t *testing.T) {
		prev := preflightDaemon
		preflightDaemon = func(string, string) error { return nil }
		t.Cleanup(func() { preflightDaemon = prev })

		cmd := &cobra.Command{Use: "maz"}
		cmd.SetErr(&bytes.Buffer{})
		refused, rerr := refuseSchemaWriteUnderDaemon(cmd, t.TempDir(), "schema import")
		if refused || rerr != nil {
			t.Fatalf("no daemon: want (false, nil), got (%v, %v)", refused, rerr)
		}
	})
}

// asSilentError reports whether err is a silentError, copying it into target.
func asSilentError(err error, target *silentError) bool {
	se, ok := err.(silentError) //nolint:errorlint // silentError is a sentinel value type, not wrapped
	if ok {
		*target = se
	}
	return ok
}

// TestOpenPresetReadDB_DaemonHeldTakesReadOnlyPath: `maz preset list` and
// `maz preset show` read the presets table through openPresetReadDB. With a
// daemon holding the graph-DB lock they must take the read-only SQLite path
// instead of the in-process wiring, which used to fail with "ladybug ... status 1".
func TestOpenPresetReadDB_DaemonHeldTakesReadOnlyPath(t *testing.T) {
	// No t.Parallel: stubs the package-level preflightDaemon var.
	dir := t.TempDir()
	provisionIndexDB(t, dir)

	prev := preflightDaemon
	preflightDaemon = func(string, string) error {
		return &daemonLockHeldError{PID: 4242, StopHint: "maz daemon stop"}
	}
	t.Cleanup(func() { preflightDaemon = prev })

	cmd := newReadDBTestCmd()
	if err := cmd.PersistentFlags().Set("data-dir", dir); err != nil {
		t.Fatalf("set data-dir: %v", err)
	}
	db, closeDB, err := openPresetReadDB(context.Background(), cmd, zap.NewNop())
	if err != nil {
		t.Fatalf("openPresetReadDB (daemon held): %v", err)
	}
	t.Cleanup(closeDB)

	var n int
	if qerr := db.QueryRowContext(context.Background(),
		`SELECT COUNT(1) FROM presets`).Scan(&n); qerr != nil {
		t.Fatalf("read-only pool query on presets failed: %v", qerr)
	}
}
