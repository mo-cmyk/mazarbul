package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
)

// syncConflictDetector implements conflictDetector by running the functional-slot
// scan against a live SQLite reader and emitting new conflict events into the
// event log. It is constructed once per daemon and shared across all consume
// loops (DetectAndEmit is safe for concurrent calls; each call opens its own
// read transaction and the emitter's dedup guard is protected by the
// append-only log's row-insertion guarantee).
type syncConflictDetector struct {
	reader     *sql.DB
	log        kernel.EventLog
	predicates resolver.FunctionalPredicate
	logger     *zap.Logger
}

// newSyncConflictDetector builds a conflictDetector over the dispatcher's read
// pool, the event log, and the predicate registry. Returns nil when any
// dependency is absent (disabled path for test wiring with no kernel).
func newSyncConflictDetector(
	reader *sql.DB,
	log kernel.EventLog,
	predicates resolver.FunctionalPredicate,
	logger *zap.Logger,
) *syncConflictDetector {
	if reader == nil || log == nil || predicates == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &syncConflictDetector{
		reader:     reader,
		log:        log,
		predicates: predicates,
		logger:     logger,
	}
}

// HasOpenConflicts reports whether entityUID is the subject of any functional
// slot that has more than one open (valid_until IS NULL) relation. Satisfies
// the tools.entityConflictChecker interface via structural typing. Best-effort:
// a query error returns (false, err) so the caller degrades gracefully.
func (d *syncConflictDetector) HasOpenConflicts(ctx context.Context, entityUID string) (bool, error) {
	var count int
	err := d.reader.QueryRowContext(ctx,
		`SELECT count(*)
		   FROM (
		     SELECT predicate
		       FROM relations
		      WHERE subject_uid = ?
		        AND valid_until IS NULL
		      GROUP BY predicate
		     HAVING count(*) > 1
		   ) t`,
		entityUID,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("has open conflicts for entity: %w", err)
	}
	return count > 0, nil
}

// DetectAndEmit scans for concurrent-supersede conflicts and emits one
// relationSupersedeConflict event per newly-detected pair. Idempotent: pairs
// already recorded in the event log are skipped. Non-fatal: the caller logs any
// returned error at WARN and continues.
func (d *syncConflictDetector) DetectAndEmit(ctx context.Context) error {
	conflicts, err := resolver.DetectConcurrentSupersedeOn(ctx, d.reader, d.predicates)
	if err != nil {
		return fmt.Errorf("detect concurrent supersede: %w", err)
	}
	if len(conflicts) == 0 {
		return nil
	}
	emitted, err := resolver.EmitConcurrentSupersedeConflictsTo(
		ctx, d.log, d.logger, conflicts, "sync:pull", time.Now,
	)
	if err != nil {
		return fmt.Errorf("emit supersede conflicts: %w", err)
	}
	if emitted > 0 {
		d.logger.Warn("daemon: sync consume detected and recorded new supersede conflicts",
			zap.Int("emitted", emitted))
	}
	return nil
}
