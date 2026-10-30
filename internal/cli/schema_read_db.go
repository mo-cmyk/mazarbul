package cli

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// openSchemaReadDB returns a *sql.DB for a SQLite-only schema READ. When a
// daemon serves dataDir it opens a lock-tolerant read-only pool (the daemon
// holds the graph-DB write lock, so the full in-process wiring would fail);
// otherwise it opens the in-process wiring (which migrates a fresh store) and
// returns its SQLite handle. The returned close func releases whatever was
// opened. Mirrors the daemon-or-in-process split the write commands use.
func openSchemaReadDB(ctx context.Context, cmd *cobra.Command, dataDir string, logger *zap.Logger) (*sql.DB, func(), error) {
	// A live daemon owns the store's graph-DB write lock, so the full in-process
	// OpenWiring would die with "ladybug ... status 1". Read the daemon's
	// committed SQLite state through a lock-tolerant read-only pool instead; WAL
	// lets it read concurrently while the daemon holds the writer.
	if perr := preflightDaemon("", dataDir); perr != nil {
		db, err := sqlite.OpenReadOnly(ctx, filepath.Join(dataDir, "index.db"))
		if err != nil {
			return nil, nil, fmt.Errorf("open read-only sqlite: %w", err)
		}
		return db, func() { _ = db.Close() }, nil
	}

	// No daemon: open the in-process wiring. This preserves today's behaviour,
	// including migrating a fresh store, when the read runs standalone.
	wired, err := daemon.OpenWiring(ctx, daemon.WiringConfig{DataDir: dataDir, Logger: logger})
	if err != nil {
		return nil, nil, fmt.Errorf("open wiring: %w", err)
	}
	db, err := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if err != nil {
		if cerr := wired.Close(ctx); cerr != nil {
			logger.Warn("schema read: close wiring after sqlite open failure", zap.Error(cerr))
		}
		return nil, nil, fmt.Errorf("open sqlite: %w", err)
	}
	return db, func() {
		if cerr := wired.Close(ctx); cerr != nil {
			logger.Warn("schema read: close wiring", zap.Error(cerr))
		}
	}, nil
}

// openSchemaLintDB is the lint variant of openSchemaReadDB: it also surfaces the
// dense-vector Embedder so the near-duplicate check can run its embedding lane.
// When a daemon serves dataDir it opens the lock-tolerant read-only SQLite pool
// and returns a nil embedder, which makes lintNearDuplicateKinds degrade to its
// lexical fallback (the same path CI and offline runs take). When no daemon
// runs it opens the in-process wiring and returns wired.Embedder, preserving
// the embedding-based near-duplicate detection. The returned close func releases
// whatever was opened.
//
//nolint:ireturn // returns the kernel.Embedder lint consumes; nil is the documented daemon/offline degraded sentinel
func openSchemaLintDB(ctx context.Context, cmd *cobra.Command, dataDir string, logger *zap.Logger) (*sql.DB, kernel.Embedder, func(), error) {
	if perr := preflightDaemon("", dataDir); perr != nil {
		db, err := sqlite.OpenReadOnly(ctx, filepath.Join(dataDir, "index.db"))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open read-only sqlite: %w", err)
		}
		return db, nil, func() { _ = db.Close() }, nil
	}

	wired, err := daemon.OpenWiring(ctx, daemon.WiringConfig{DataDir: dataDir, Logger: logger})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open wiring: %w", err)
	}
	db, err := wired.Orchestrator.Storage.OpenSQLite(ctx)
	if err != nil {
		if cerr := wired.Close(ctx); cerr != nil {
			logger.Warn("schema lint: close wiring after sqlite open failure", zap.Error(cerr))
		}
		return nil, nil, nil, fmt.Errorf("open sqlite: %w", err)
	}
	return db, wired.Embedder, func() {
		if cerr := wired.Close(ctx); cerr != nil {
			logger.Warn("schema lint: close wiring", zap.Error(cerr))
		}
	}, nil
}

// refuseSchemaWriteUnderDaemon reports whether a daemon serves dataDir and, when
// it does, prints a clean actionable refusal to stderr and returns a
// silentError(1). It is the fallback for schema WRITE commands that have no
// equivalent MCP authoring tool to route to (schema alias / import / review):
// the in-process write would otherwise race the daemon's exclusive store lock
// and surface a cryptic "ladybug ... Status 1". The opName prefixes the message
// (e.g. "schema alias"). Returns (false, nil) when no daemon runs, so the caller
// falls through to its in-process path unchanged.
func refuseSchemaWriteUnderDaemon(cmd *cobra.Command, dataDir, opName string) (bool, error) {
	if preflightDaemon("", dataDir) == nil {
		return false, nil
	}
	//nolint:errcheck // best-effort stderr
	fmt.Fprintf(cmd.ErrOrStderr(),
		"%s needs exclusive store access; stop the daemon (maz daemon stop) and retry, "+
			"or use the equivalent MCP tool\n", opName)
	return true, silentError{code: 1, err: fmt.Errorf("%s: daemon holds the store lock", opName)}
}
