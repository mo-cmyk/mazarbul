package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// queueingExtractor models Ollama's NUM_PARALLEL serialization at the
// call level: each ExtractFrom invocation takes `delay`, and `slots`
// callers can be in-flight concurrently. Calls record their outcome
// (nil on success, ctx.Err on cancellation) so the test can assert the
// loss set.
//
// applyExtraction processes a batch under a single shared ctx — so a
// batch with N events takes N*delay/slots wall-clock with a serialized
// extractor, and late events in the batch see their shared ctx expire.
type queueingExtractor struct {
	slots chan struct{}
	delay time.Duration

	mu         sync.Mutex
	outcomes   []error
	successCnt atomic.Int64
}

func newQueueingExtractor(slots int, delay time.Duration) *queueingExtractor {
	return &queueingExtractor{
		slots: make(chan struct{}, slots),
		delay: delay,
	}
}

func (q *queueingExtractor) ExtractFrom(ctx context.Context, _ kernel.Event) ([]kernel.Event, error) {
	select {
	case q.slots <- struct{}{}:
		defer func() { <-q.slots }()
	case <-ctx.Done():
		q.record(ctx.Err())
		return nil, ctx.Err()
	}
	select {
	case <-time.After(q.delay):
	case <-ctx.Done():
		q.record(ctx.Err())
		return nil, ctx.Err()
	}
	q.successCnt.Add(1)
	q.record(nil)
	return nil, nil
}

func (q *queueingExtractor) record(err error) {
	q.mu.Lock()
	q.outcomes = append(q.outcomes, err)
	q.mu.Unlock()
}

func (q *queueingExtractor) split() (successes, deadlines int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, err := range q.outcomes {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, context.DeadlineExceeded):
			deadlines++
		}
	}
	return
}

// TestExtractionTimeout_TightBudgetLosesQueuedGoroutines pins the loss
// path: applyExtraction wraps the whole batch in ONE ctx with budget
// `extractionTimeout`. When the batch contains N events and the
// extractor is serialized (queue_depth × per_call_delay > timeout),
// late events in the batch see the shared ctx expire and silently
// produce no extraction. This is the bug that caused recall@K
// flakiness in the LongMemEval 15q bench: a different subset of
// episodes lost extraction work each run as the goroutines waiting
// in Ollama's queue saw their 90s ctx expire before being served.
func TestExtractionTimeout_TightBudgetLosesQueuedGoroutines(t *testing.T) {
	t.Parallel()

	const (
		nEvents = 10
		slots   = 1
		delay   = 40 * time.Millisecond
		// nEvents * delay = 400 ms serial cost. With timeout=100 ms,
		// events processed after ~2-3 iterations see ctx expired and fail.
		timeout = 100 * time.Millisecond
	)

	ctx := testutil.TestContext(t)
	ex := newQueueingExtractor(slots, delay)
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithExtractor(ex),
		kernel.WithExtractionTimeout(timeout),
	)
	t.Cleanup(func() { _ = log.Close() })

	batch := make([]kernel.Event, nEvents)
	for i := range batch {
		batch[i] = kernel.Event{
			Kind:       "episode",
			Source:     "test",
			Confidence: 1.0,
			Payload:    json.RawMessage(`{"text":"x"}`),
		}
	}
	if _, err := log.Append(ctx, batch); err != nil {
		t.Fatalf("Append batch: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := log.WaitForBackgroundWork(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundWork: %v", err)
	}

	successes, deadlines := ex.split()
	if successes+deadlines == 0 {
		t.Fatalf("no outcomes recorded; extractor was never called")
	}
	if deadlines == 0 {
		t.Fatalf("expected DeadlineExceeded losses with a tight (%v) timeout + serialized extractor, got %d/%d successes",
			timeout, successes, nEvents)
	}
	if successes >= nEvents {
		t.Fatalf("expected partial loss with tight timeout, got %d/%d successes (all succeeded — semaphore/timeout setup is wrong)",
			successes, nEvents)
	}
	t.Logf("tight-timeout loss path confirmed: %d/%d successful, %d ctx-deadline losses",
		successes, nEvents, deadlines)
}

// deadlineRecordingResolver captures the ctx.Deadline() it sees on its
// first ResolveFrom call so the test can pin which timeout drove that
// ctx (extraction's or resolution's).
type deadlineRecordingResolver struct {
	mu       sync.Mutex
	deadline time.Time
	hasCall  bool
}

func (r *deadlineRecordingResolver) ResolveFrom(ctx context.Context, _ []kernel.Event) ([]kernel.Event, error) {
	dl, _ := ctx.Deadline()
	r.mu.Lock()
	if !r.hasCall {
		r.deadline = dl
		r.hasCall = true
	}
	r.mu.Unlock()
	return nil, nil
}

// recordedDeadline returns the ctx.Deadline() the resolver observed and
// whether ResolveFrom was called at all.
func (r *deadlineRecordingResolver) recordedDeadline() (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deadline, r.hasCall
}

// TestResolutionTimeout_IsIndependentFromExtractionTimeout pins that
// applyResolution's ctx is bounded by resolutionTimeout (set via
// WithResolutionTimeout), NOT by extractionTimeout. Catches a regression
// where the two budgets share a single field — Greptile flagged the
// original implementation for this exact coupling (a wedged resolver
// would have pinned Close / WaitForBackgroundWork for the entire
// extraction window).
func TestResolutionTimeout_IsIndependentFromExtractionTimeout(t *testing.T) {
	t.Parallel()

	const (
		extractionBudget = 10 * time.Second
		resolutionBudget = 100 * time.Millisecond
	)

	ctx := testutil.TestContext(t)
	resolver := &deadlineRecordingResolver{}
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithResolver(resolver),
		kernel.WithExtractionTimeout(extractionBudget),
		kernel.WithResolutionTimeout(resolutionBudget),
	)
	t.Cleanup(func() { _ = log.Close() })

	ev := kernel.Event{
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"x"}`),
	}
	if _, err := log.Append(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := log.WaitForBackgroundWork(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundWork: %v", err)
	}

	deadline, called := resolver.recordedDeadline()
	if !called {
		t.Fatal("resolver was never invoked; applyResolution must be reachable for this contract")
	}
	remaining := time.Until(deadline)
	// The deadline should be near resolutionBudget (100ms) from the
	// resolver's invocation, NOT near extractionBudget (10s). Allow a
	// generous slack window because the goroutine may have spent some
	// time between ctx-create and the resolver entry, but the floor
	// (extractionBudget / 2) clearly distinguishes the two.
	if remaining > extractionBudget/2 {
		t.Fatalf("resolver ctx deadline carried the extraction budget (%v remaining); expected resolution budget %v",
			remaining, resolutionBudget)
	}
}

// TestExtractionTimeout_GenerousBudgetCompletesAllExtractions pins the
// fix: when the extraction timeout is comfortably larger than the
// realistic queue wait time, every goroutine gets a chance to acquire
// the LLM slot and complete. After bumping defaultExtractionTimeout
// the bench's real LongMemEval bursts fit comfortably inside the budget.
func TestExtractionTimeout_GenerousBudgetCompletesAllExtractions(t *testing.T) {
	t.Parallel()

	const (
		nEvents = 10
		slots   = 1
		delay   = 40 * time.Millisecond
		// 2 s budget for the same 10 × 40 ms = 400 ms worst-case queue wait.
		timeout = 2 * time.Second
	)

	ctx := testutil.TestContext(t)
	ex := newQueueingExtractor(slots, delay)
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithExtractor(ex),
		kernel.WithExtractionTimeout(timeout),
	)
	t.Cleanup(func() { _ = log.Close() })

	batch := make([]kernel.Event, nEvents)
	for i := range batch {
		batch[i] = kernel.Event{
			Kind:       "episode",
			Source:     "test",
			Confidence: 1.0,
			Payload:    json.RawMessage(`{"text":"x"}`),
		}
	}
	if _, err := log.Append(ctx, batch); err != nil {
		t.Fatalf("Append batch: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := log.WaitForBackgroundWork(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundWork: %v", err)
	}

	successes, deadlines := ex.split()
	if deadlines > 0 {
		t.Fatalf("expected zero ctx-deadline losses with generous (%v) timeout, got %d losses (%d successes)",
			timeout, deadlines, successes)
	}
	if successes != nEvents {
		t.Fatalf("expected %d successful extractions, got %d", nEvents, successes)
	}
}
