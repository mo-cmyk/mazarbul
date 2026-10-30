package kernel_test

// file_eventlog_worker_pool_test.go pins the bounded-worker-pool contract
// for background extraction. The previous shape spawned one goroutine per
// Append, so a haystack of 500 episodes spawned 500 goroutines all racing
// to hit Ollama. Ollama serialises at the model (num_parallel default = 4
// in current Ollama), so the surplus goroutines just held memory + HTTP
// connections + writer-mutex contention without doing useful work.
//
// The fix: a fixed-size worker pool reads from a bounded channel. New
// Appends enqueue rather than spawn. WithExtractionWorkers(n) controls
// the pool size; the queue exposes its depth so operators can see backlog.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// countingExtractor counts concurrent in-flight ExtractFrom calls and
// records the peak. block until releaseCh fires so the test can drive how
// long extraction takes; the peak reading captures the maximum concurrency
// the pool ever allowed.
type countingExtractor struct {
	inflight  atomic.Int32
	peak      atomic.Int32
	releaseCh chan struct{}
	startedCh chan struct{}
}

func (c *countingExtractor) ExtractFrom(ctx context.Context, _ kernel.Event) ([]kernel.Event, error) {
	now := c.inflight.Add(1)
	defer c.inflight.Add(-1)
	for {
		peak := c.peak.Load()
		if now <= peak {
			break
		}
		if c.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	select {
	case c.startedCh <- struct{}{}:
	default:
	}
	select {
	case <-c.releaseCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}

// TestAppend_ExtractionWorkerPool_CapsConcurrency proves the bounded pool
// enforces its size cap. With workers=2 and 10 episodes appended back to
// back, never more than 2 extractions should be in flight simultaneously.
// Before the worker pool landed, 10 goroutines would race and peak would
// equal 10.
func TestAppend_ExtractionWorkerPool_CapsConcurrency(t *testing.T) {
	t.Parallel()

	const workerCount = 2
	const episodeCount = 10

	ctx := testutil.TestContext(t)

	ex := &countingExtractor{
		releaseCh: make(chan struct{}),
		startedCh: make(chan struct{}, episodeCount),
	}
	storage, _ := newRecordingStorage(nil)
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithStorage(storage),
		kernel.WithExtractor(ex),
		kernel.WithExtractionWorkers(workerCount),
	)
	t.Cleanup(func() { _ = log.Close() })

	for i := range episodeCount {
		ev := kernel.Event{
			Kind:       "episode",
			Source:     "test",
			Confidence: 1.0,
			Payload:    json.RawMessage(`{"text":"Frodo works at Erebor."}`),
		}
		if _, err := log.Append(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	// Wait until at least workerCount extractions have started so the
	// peak reading is stable before we release them.
	for received := range workerCount {
		select {
		case <-ex.startedCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d extractions started; pool may be unbounded or stalled", received)
		}
	}

	if peak := ex.peak.Load(); peak > int32(workerCount) {
		t.Errorf("peak concurrency %d > workers %d (pool is unbounded)", peak, workerCount)
	}

	close(ex.releaseCh)

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := log.WaitForBackgroundWork(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundWork: %v", err)
	}

	if peak := ex.peak.Load(); peak > int32(workerCount) {
		t.Errorf("post-drain peak concurrency %d > workers %d", peak, workerCount)
	}
	if peak := ex.peak.Load(); peak == 0 {
		t.Error("peak concurrency 0 means no extraction ever ran; worker pool may be deadlocked")
	}
}

// TestAppend_ExtractionWorkerPool_SpillsWhenQueueFull pins the fix for a
// real regression observed during the LongMemEval probe: with workers=4 and
// capPerWorker=32 the bounded queue holds 128 jobs; sustained ingestion fills
// it and the next Append blocks for a worker drain (~20s for an LLM
// extraction). The harness's 30s per-call timeout fired on every remember
// past slot 128.
//
// The fix changed scheduleBackgroundEnrichment's select to be non-blocking:
// when the queue is full it spills into a one-shot goroutine. This test
// reproduces the saturation pattern with a small workerCount + many appends
// + blocked workers, and asserts every Append returns within hotPathBudget.
func TestAppend_ExtractionWorkerPool_SpillsWhenQueueFull(t *testing.T) {
	t.Parallel()

	const workerCount = 1
	// capPerWorker is 32; with 1 worker the queue holds 32 jobs. Append
	// 64 events so the second half MUST spill rather than block.
	const episodeCount = 64
	// 500ms budget tolerates GC pauses + parallel-test load on slower runners.
	// The non-blocking property is what matters; 250ms was empirically too
	// tight (occasional 259ms / 270ms hits under -race + full suite parallelism).
	const hotPathBudget = 500 * time.Millisecond

	ctx := testutil.TestContext(t)

	ex := &countingExtractor{
		// releaseCh stays open so workers block on every job, guaranteeing
		// the queue fills and stays full.
		releaseCh: make(chan struct{}),
		startedCh: make(chan struct{}, episodeCount),
	}
	storage, _ := newRecordingStorage(nil)
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithStorage(storage),
		kernel.WithExtractor(ex),
		kernel.WithExtractionWorkers(workerCount),
	)
	t.Cleanup(func() {
		close(ex.releaseCh)
		_ = log.Close()
	})

	for i := range episodeCount {
		ev := kernel.Event{
			Kind:       "episode",
			Source:     "test",
			Confidence: 1.0,
			Payload:    json.RawMessage(`{"text":"Frodo works at Erebor."}`),
		}
		start := time.Now()
		if _, err := log.Append(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if dt := time.Since(start); dt > hotPathBudget {
			t.Errorf("Append %d took %v > budget %v (queue saturation blocked the writer)", i, dt, hotPathBudget)
		}
	}
}
