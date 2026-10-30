package kernel_test

// file_eventlog_queue_depth_test.go pins the operator-visibility surface for
// the bounded extraction worker pool. Without a queue-depth reading,
// operators cannot tell whether the daemon is keeping up with ingestion
// (queue near zero) or building a backlog (queue near capacity). The metric
// is read-only and snapshots the live channel length plus the worker count;
// callers can call it as often as they want without affecting the pool.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// blockingExtractor releases via releaseCh so the test can build up a known
// queue depth before any worker drains. Mirrors countingExtractor in the
// worker-pool test but exposes the in-flight count for cross-checking the
// reported depth.
type blockingExtractor struct {
	inflight  atomic.Int32
	releaseCh chan struct{}
	startedCh chan struct{}
}

func (b *blockingExtractor) ExtractFrom(ctx context.Context, _ kernel.Event) ([]kernel.Event, error) {
	b.inflight.Add(1)
	defer b.inflight.Add(-1)
	select {
	case b.startedCh <- struct{}{}:
	default:
	}
	select {
	case <-b.releaseCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}

// TestBackgroundQueueDepth_ReflectsBacklog appends more episodes than the
// worker pool can drain (workers stuck on the blockingExtractor), and asserts
// BackgroundQueueDepth reports the expected backlog. After releasing the
// workers and draining, depth must return to zero.
func TestBackgroundQueueDepth_ReflectsBacklog(t *testing.T) {
	t.Parallel()

	const workers = 2
	const total = 7

	ctx := testutil.TestContext(t)
	ex := &blockingExtractor{
		releaseCh: make(chan struct{}),
		startedCh: make(chan struct{}, total),
	}
	storage, _ := newRecordingStorage(nil)
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithStorage(storage),
		kernel.WithExtractor(ex),
		kernel.WithExtractionWorkers(workers),
	)
	t.Cleanup(func() { _ = log.Close() })

	for i := range total {
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

	// Wait until every worker is stuck on the blockingExtractor so the queue
	// has settled. After 2 workers each grabbed one job, the remaining
	// total-workers jobs must be sitting on the channel.
	for received := range workers {
		select {
		case <-ex.startedCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d extractions started; pool may be stalled", received)
		}
	}

	// Allow the workers a small grace period to receive their jobs from the
	// channel; len(channel) measures buffered-but-not-yet-received entries.
	deadline := time.Now().Add(time.Second)
	wantDepth := total - workers
	var depth int
	for time.Now().Before(deadline) {
		depth = log.BackgroundQueueDepth()
		if depth == wantDepth {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if depth != wantDepth {
		t.Errorf("BackgroundQueueDepth = %d, want %d (%d total - %d workers actively running)", depth, wantDepth, total, workers)
	}

	close(ex.releaseCh)

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := log.WaitForBackgroundWork(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundWork: %v", err)
	}

	if depth := log.BackgroundQueueDepth(); depth != 0 {
		t.Errorf("post-drain BackgroundQueueDepth = %d, want 0", depth)
	}
}

// TestBackgroundQueueDepth_ZeroWithoutPool returns 0 when the worker pool is
// disabled (legacy per-Append spawn path). There is no queue in that mode.
func TestBackgroundQueueDepth_ZeroWithoutPool(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	storage, _ := newRecordingStorage(nil)
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithStorage(storage),
		// no WithExtractionWorkers — pool disabled.
	)
	t.Cleanup(func() { _ = log.Close() })

	if depth := log.BackgroundQueueDepth(); depth != 0 {
		t.Errorf("BackgroundQueueDepth without worker pool = %d, want 0", depth)
	}
	_ = ctx
}
