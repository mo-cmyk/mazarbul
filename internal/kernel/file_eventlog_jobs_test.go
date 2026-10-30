package kernel_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// fakeJobStarter records every Begin call so the extraction-worker
// instrumentation can be asserted without standing up a real
// jobs.Registry. It returns a no-op kernel.JobReporter.
type fakeJobStarter struct {
	begins atomic.Int64
	mu     sync.Mutex
	kinds  []string
}

func newFakeJobStarter() *fakeJobStarter { return &fakeJobStarter{} }

func (f *fakeJobStarter) Begin(ctx context.Context, kind, _ string) (kernel.JobReporter, context.Context) {
	f.begins.Add(1)
	f.mu.Lock()
	f.kinds = append(f.kinds, kind)
	f.mu.Unlock()
	return noopJobReporter{}, ctx
}

type noopJobReporter struct{}

func (noopJobReporter) Update(float64, string) {}
func (noopJobReporter) Done()                  {}
func (noopJobReporter) Fail(error)             {}

// recordingExtractor counts ExtractFrom calls so the test can wait
// deterministically for the background worker to drain the episode.
type recordingExtractor struct{ calls atomic.Int64 }

func (r *recordingExtractor) ExtractFrom(_ context.Context, _ kernel.Event) ([]kernel.Event, error) {
	r.calls.Add(1)
	return nil, nil
}

// TestExtractionWorker_StartsJob is the regression guard for the
// user-reported gap "Jobs panel daemon doesn't emit updates outside
// rebuild." The FileEventLog's background extraction worker loop is
// the most natural producer (every remember triggers it) but never
// reported its work through the jobs.Registry. This test fails before
// the SetJobStarter + per-batch Begin wiring lands and passes after.
func TestExtractionWorker_StartsJob(t *testing.T) {
	ctx := testutil.TestContext(t)
	fake := newFakeJobStarter()
	rec := &recordingExtractor{}

	w := newTestWriter(t,
		kernel.WithExtractor(rec),
		kernel.WithExtractionWorkers(1),
	)
	w.SetJobStarter(fake)

	episode := kernel.Event{
		Kind:       "remember",
		Source:     "test",
		Confidence: 1,
		Payload:    json.RawMessage(`{"text":"trigger extraction"}`),
	}
	if _, err := w.Append(ctx, []kernel.Event{episode}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Wait for the worker to drain the extraction batch.
	bgCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := w.WaitForBackgroundWork(bgCtx); err != nil {
		t.Fatalf("WaitForBackgroundWork: %v", err)
	}

	if rec.calls.Load() == 0 {
		t.Fatalf("recordingExtractor.ExtractFrom never called — extractor not wired")
	}
	if got := fake.begins.Load(); got == 0 {
		t.Errorf("fakeJobStarter.Begin call count = %d, want >= 1 (extraction worker never started a job)", got)
	}
}
