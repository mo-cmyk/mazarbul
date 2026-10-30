package daemon

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/cognitive"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
)

// Cognitive-job cadence defaults (issue #231). The loop ticks at the
// consolidation cadence; reflection runs on the (longer) reflect cadence.
const (
	cognitiveConsolidateIntervalDefault = time.Hour
	cognitiveReflectIntervalDefault     = 24 * time.Hour
)

// cognitiveSupervisor runs the autonomous reflection + consolidation jobs and
// the insight decay/demote sweep on a daily/hourly tick, idle-gated so they
// never compete with active ingestion. It mirrors syncSupervisor.sealLoop: one
// goroutine, run-on-tick, select on ctx/stop. An overlap guard (runMu, TryLock
// in the loop) ensures a slow LLM tick does not stack a second pass, and the
// same lock serializes the manual-trigger MCP tools against the ticker.
type cognitiveSupervisor struct {
	reflector    *cognitive.Reflector
	consolidator *cognitive.Consolidator
	sweepDB      *sql.DB
	idle         func() bool
	jobs         *jobs.Registry
	logger       *zap.Logger

	reflectInterval     time.Duration
	consolidateInterval time.Duration
	sweepOpts           cognitive.SweepOptions

	stop      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	runMu     sync.Mutex
}

// newCognitiveSupervisor builds the supervisor. SweepDB is the SQLite writer
// pool (the decay sweep updates insights in place). Idle reports whether the
// kernel is quiescent; registry may be nil (jobs are simply not surfaced).
func newCognitiveSupervisor(
	reflector *cognitive.Reflector,
	consolidator *cognitive.Consolidator,
	sweepDB *sql.DB,
	idle func() bool,
	registry *jobs.Registry,
	logger *zap.Logger,
) *cognitiveSupervisor {
	if logger == nil {
		logger = zap.NewNop()
	}
	if idle == nil {
		idle = func() bool { return true }
	}
	return &cognitiveSupervisor{
		reflector:           reflector,
		consolidator:        consolidator,
		sweepDB:             sweepDB,
		idle:                idle,
		jobs:                registry,
		logger:              logger,
		reflectInterval:     cognitiveReflectIntervalDefault,
		consolidateInterval: cognitiveConsolidateIntervalDefault,
		stop:                make(chan struct{}),
	}
}

// Start launches the tick loop on a background goroutine.
func (s *cognitiveSupervisor) Start(ctx context.Context) {
	s.wg.Add(1)
	go s.loop(ctx)
}

// loop ticks at the consolidation cadence; reflection runs once per reflect
// cadence. Each tick is idle-gated and overlap-guarded.
func (s *cognitiveSupervisor) loop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.consolidateInterval)
	defer t.Stop()
	lastReflect := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			if !s.idle() {
				continue
			}
			// Overlap guard: skip this tick if a manual trigger (or a prior
			// slow tick) still holds the run lock.
			if !s.runMu.TryLock() {
				continue
			}
			s.tick(ctx, &lastReflect)
			s.runMu.Unlock()
		}
	}
}

// tick runs consolidation + the decay sweep every time, and reflection when the
// reflect cadence has elapsed. Caller holds runMu.
func (s *cognitiveSupervisor) tick(ctx context.Context, lastReflect *time.Time) {
	s.runConsolidationLocked(ctx)
	if _, err := cognitive.SweepInsightDecay(ctx, s.sweepDB, s.sweepOpts); err != nil {
		s.logger.Warn("daemon: cognitive decay sweep failed", zap.Error(err))
	}
	if time.Since(*lastReflect) >= s.reflectInterval {
		s.runReflectionLocked(ctx)
		*lastReflect = time.Now()
	}
}

// RunReflection runs one reflection pass plus a decay sweep, serialized against
// the ticker. Used by the manual-trigger MCP tool; not idle-gated (the caller
// asked explicitly).
func (s *cognitiveSupervisor) RunReflection(ctx context.Context) (cognitive.Result, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	res := s.runReflectionLocked(ctx)
	if _, err := cognitive.SweepInsightDecay(ctx, s.sweepDB, s.sweepOpts); err != nil {
		s.logger.Warn("daemon: cognitive decay sweep failed", zap.Error(err))
	}
	return res.result, res.err
}

// RunConsolidation runs one consolidation pass, serialized against the ticker.
func (s *cognitiveSupervisor) RunConsolidation(ctx context.Context) (cognitive.Result, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	r := s.runConsolidationLocked(ctx)
	return r.result, r.err
}

// passOutcome bundles a job result with its error for the locked helpers.
type passOutcome struct {
	result cognitive.Result
	err    error
}

func (s *cognitiveSupervisor) runReflectionLocked(ctx context.Context) passOutcome {
	if s.reflector == nil {
		return passOutcome{result: cognitive.Result{Skipped: "not-wired"}}
	}
	jobCtx, done := s.beginJob(ctx, "reflection", "Synthesize insights")
	res, err := s.reflector.Run(jobCtx)
	done(err)
	if err != nil {
		s.logger.Warn("daemon: reflection pass failed", zap.Error(err))
	}
	return passOutcome{result: res, err: err}
}

func (s *cognitiveSupervisor) runConsolidationLocked(ctx context.Context) passOutcome {
	if s.consolidator == nil {
		return passOutcome{result: cognitive.Result{Skipped: "not-wired"}}
	}
	jobCtx, done := s.beginJob(ctx, "consolidation", "Consolidate recent memories")
	res, err := s.consolidator.Run(jobCtx)
	done(err)
	if err != nil {
		s.logger.Warn("daemon: consolidation pass failed", zap.Error(err))
	}
	return passOutcome{result: res, err: err}
}

// beginJob registers a job with the registry (when present) and returns the
// job context plus a completion callback. With no registry it is a no-op
// wrapper over ctx.
func (s *cognitiveSupervisor) beginJob(ctx context.Context, kind, title string) (context.Context, func(error)) {
	if s.jobs == nil {
		return ctx, func(error) {}
	}
	tracker, jobCtx := s.jobs.Begin(ctx, kind, title)
	return jobCtx, func(err error) {
		if err != nil {
			tracker.Fail(err)
			return
		}
		tracker.Done()
	}
}

// Close stops the loop and waits for the in-flight tick to drain. It is
// idempotent: a second call (defensive teardown, a closer registry that fires
// twice) is a no-op rather than a panic on the already-closed stop channel.
func (s *cognitiveSupervisor) Close() error {
	s.closeOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
	return nil
}
