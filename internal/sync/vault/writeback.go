package vault

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Writeback is the materialize-on-new-revision half of the bidirectional
// vault loop. It subscribes to path notifications (via Notify) and, for each,
// calls Projector.SyncPath which enforces:
//
//   - I1 (content-hash idempotency): SyncPath writes the file only when the
//     stored hash differs from the on-disk hash. If the watcher just ingested
//     the file, the hashes will be equal and the write is skipped — loop broken.
//   - I2 (ingest-before-render): SyncPath's writeDocLocked reads the on-disk
//     bytes first; if they do not match the projector's last-written hash, the
//     file is classified as an un-ingested edit and skipped.
//   - I3 (single serialized writer): all SyncPath calls go through Projector.mu.
//   - I4 (atomic writes): SyncPath uses writeFileAtomic internally.
//
// Notify implements RevisionNotifier so the watcher can call it after each
// successful ingest, and so MCP tools (writeDocument, importDocuments) can
// hook in when they append a revision through a non-watcher path.
//
// Coalescing & backpressure: notified paths are accumulated in a set keyed by
// vault-relative path (pending), so a burst of N notifications for the same
// path collapses to a single SyncPath, and — critically — a burst across many
// DISTINCT paths never drops any of them (the set grows by unique key, not by
// notification count). A buffered wakeup channel signals the worker without the
// caller ever blocking. Because the queue is a map guarded by a mutex rather
// than a channel, Notify can never panic by sending on a closed channel during
// shutdown.
type Writeback struct {
	projector *Projector
	log       *zap.Logger

	// mu guards pending and stopped.
	mu sync.Mutex
	// pending is the keyed set of (scope, vault-relative path) pairs awaiting
	// reconciliation. Keying by the pair is what makes Notify lossless under a
	// multi-path burst and keeps the same relative path in two scopes distinct.
	pending map[pendingRef]struct{}
	// stopped is set by Stop; once true, Notify becomes a no-op.
	stopped bool

	// wake is a 1-buffered signal channel: Notify does a non-blocking send so it
	// never blocks, and the worker drains the entire pending set on each wakeup.
	wake chan struct{}
	// done is closed by Stop to unblock the worker.
	done chan struct{}
	// stopOnce guards the single close of done.
	stopOnce sync.Once
}

// NewWriteback builds a Writeback backed by projector. Log may be nil.
func NewWriteback(projector *Projector, log *zap.Logger) *Writeback {
	if log == nil {
		log = zap.NewNop()
	}
	return &Writeback{
		projector: projector,
		log:       log,
		pending:   map[pendingRef]struct{}{},
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// Notify enqueues (scope, rel) for writeback reconciliation. Safe for
// concurrent calls and never blocks. After Stop, Notify is a no-op. Implements
// RevisionNotifier so both the watcher and MCP tools can call it.
func (wb *Writeback) Notify(scope, rel string) {
	wb.mu.Lock()
	if wb.stopped {
		wb.mu.Unlock()
		return
	}
	wb.pending[pendingRef{scope: scope, rel: rel}] = struct{}{}
	wb.mu.Unlock()

	// Non-blocking wakeup: the buffer holds at most one pending signal, which is
	// all the worker needs — it drains the whole set on each wakeup.
	select {
	case wb.wake <- struct{}{}:
	default:
	}
}

// Run drives the writeback loop until ctx is cancelled or Stop is called. On
// each wakeup it snapshots and drains the pending set, calling SyncPath once
// per distinct path. A short coalescing window lets a burst of writes (e.g. an
// importDocuments batch) accumulate into the set before the flush, so a path
// touched several times in quick succession yields a single SyncPath. Blocks;
// callers should run it in a goroutine.
func (wb *Writeback) Run(ctx context.Context) {
	flush := time.NewTicker(DefaultWatchDebounce)
	defer flush.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-wb.done:
			return
		case <-wb.wake:
			// A notification arrived; wait for the next flush tick to coalesce a
			// burst, then drain.
		case <-flush.C:
			wb.drain(ctx)
		}
	}
}

// drain snapshots the pending set under the lock, clears it, and reconciles
// each path. Reconciliation runs outside the lock so Notify never blocks behind
// a slow SyncPath.
func (wb *Writeback) drain(ctx context.Context) {
	wb.mu.Lock()
	if len(wb.pending) == 0 {
		wb.mu.Unlock()
		return
	}
	batch := make([]pendingRef, 0, len(wb.pending))
	for ref := range wb.pending {
		batch = append(batch, ref)
		delete(wb.pending, ref)
	}
	wb.mu.Unlock()

	for _, ref := range batch {
		wb.reconcilePath(ctx, ref.scope, ref.rel)
	}
}

// Stop signals the Run loop to exit and makes Notify a no-op. Idempotent.
func (wb *Writeback) Stop() {
	wb.mu.Lock()
	wb.stopped = true
	wb.mu.Unlock()
	wb.stopOnce.Do(func() {
		close(wb.done)
	})
}

// reconcilePath calls Projector.SyncPath for one (scope, vault-relative path).
// All invariants (I1-I4) are enforced inside SyncPath; this wrapper only adds
// logging.
func (wb *Writeback) reconcilePath(ctx context.Context, scope, rel string) {
	_, err := wb.projector.SyncPath(ctx, rel, scope)
	if err != nil {
		// ErrNotFound is expected when a document is tombstoned or not yet
		// committed to the projection (race between Append and SyncPath): log
		// at debug, not warn, to avoid noise in normal operation.
		wb.log.Debug("Vault writeback: sync path skipped or not found",
			zap.String("path", rel),
			zap.String("scope", scope),
			zap.Error(err))
		return
	}
}
