package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// memoryAccessSource is the Event.Source stamped on recall-emitted
// memoryAccessed events (issue #231). Follows the <producer>/v<n> convention
// used by other derived-event producers (resolver/v1, extractor/v1).
const memoryAccessSource = "recall/v1"

// memoryAccessCooldownDefault debounces repeated accesses of the same target.
// An agent recalling the same query in a tight loop would otherwise append one
// signed event per surfaced memory per recall; reinforcement saturates
// (1 − 0.9^n), so coalescing rapid duplicates within this window barely moves
// the score while bounding log growth to ~1 access event per target per window.
const memoryAccessCooldownDefault = 5 * time.Minute

// accessAppender is the minimal event-log surface the recorder needs. Satisfied
// by *kernel.FileEventLog (orch.Log).
type accessAppender interface {
	Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error)
}

// memoryAccessRecorder implements kernel.AccessRecorder. It emits memoryAccessed
// events for the memories recall surfaces, best-effort and non-blocking:
// RecordAccess returns immediately and the signed append runs on a detached
// goroutine so a slow append never pins recall. Append failures are warn-logged
// (passive reinforcement is best-effort; rebuild reconstructs strength from
// whatever landed).
type memoryAccessRecorder struct {
	appender accessAppender
	logger   *zap.Logger
	cooldown time.Duration

	mu        sync.Mutex
	lastEmit  map[string]time.Time
	lastPrune time.Time
	wg        sync.WaitGroup
}

// newMemoryAccessRecorder builds a recorder over the event-log appender.
func newMemoryAccessRecorder(a accessAppender, logger *zap.Logger) *memoryAccessRecorder {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &memoryAccessRecorder{
		appender: a,
		logger:   logger,
		cooldown: memoryAccessCooldownDefault,
		lastEmit: map[string]time.Time{},
	}
}

// RecordAccess implements kernel.AccessRecorder. Non-blocking: it debounces the
// targets under the recorder lock, then dispatches the surviving set on a
// detached goroutine.
func (r *memoryAccessRecorder) RecordAccess(ctx context.Context, targetUIDs []string) {
	if r == nil || r.appender == nil || len(targetUIDs) == 0 {
		return
	}
	now := time.Now().UTC()
	fresh := r.debounce(targetUIDs, now)
	if len(fresh) == 0 {
		return
	}
	r.wg.Add(1)
	bgCtx := context.WithoutCancel(ctx)
	go func() {
		defer r.wg.Done()
		r.emit(bgCtx, fresh, now)
	}()
}

// debounce drops targets accessed within the cooldown window and stamps the
// survivors' last-emit time. Returns the targets that should emit an event.
func (r *memoryAccessRecorder) debounce(uids []string, now time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneExpiredLocked(now)
	fresh := make([]string, 0, len(uids))
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		if last, ok := r.lastEmit[uid]; ok && now.Sub(last) < r.cooldown {
			continue
		}
		r.lastEmit[uid] = now
		fresh = append(fresh, uid)
	}
	return fresh
}

// pruneExpiredLocked evicts lastEmit entries older than the cooldown so the map
// stays bounded to recently-accessed targets rather than growing for the
// daemon's lifetime (one entry would otherwise linger forever per memory ever
// recalled). An expired entry suppresses nothing — its next access is already
// past the cooldown and will re-emit — so dropping it changes no behavior. The
// sweep is amortized to at most once per cooldown window; callers hold r.mu.
func (r *memoryAccessRecorder) pruneExpiredLocked(now time.Time) {
	if now.Sub(r.lastPrune) < r.cooldown {
		return
	}
	for uid, last := range r.lastEmit {
		if now.Sub(last) >= r.cooldown {
			delete(r.lastEmit, uid)
		}
	}
	r.lastPrune = now
}

// emit appends one memoryAccessed event per fresh target in a single Append call
// (one lock acquisition, N lines). Scope is left empty so prepare() defaults it
// to personal — the fail-safe direction for the #206 read boundary.
func (r *memoryAccessRecorder) emit(ctx context.Context, uids []string, now time.Time) {
	events := make([]kernel.Event, 0, len(uids))
	for _, uid := range uids {
		payload, err := json.Marshal(ontology.MemoryAccessedEvent{
			TargetUID:  ontology.ULID(uid),
			AccessedAt: now,
		})
		if err != nil {
			r.logger.Warn("daemon: marshal memoryAccessed payload",
				zap.String("targetUid", uid), zap.Error(err))
			continue
		}
		events = append(events, kernel.Event{
			Kind:    ontology.KindMemoryAccessed,
			Source:  memoryAccessSource,
			Payload: payload,
		})
	}
	if len(events) == 0 {
		return
	}
	if _, err := r.appender.Append(ctx, events); err != nil {
		r.logger.Warn("daemon: append memoryAccessed events (recall reinforcement is best-effort)",
			zap.Int("count", len(events)), zap.Error(err))
	}
}

// Close waits for in-flight access appends to drain.
func (r *memoryAccessRecorder) Close() error {
	r.wg.Wait()
	return nil
}
