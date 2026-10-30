package kernel_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// panicOnceExtractor panics on its first ExtractFrom call and succeeds on every
// later one. It lets a test prove background enrichment recovers a panic
// (rather than crashing the daemon — an unrecovered panic in a goroutine is
// fatal to the whole process) AND that the in-flight counters are released so
// WaitForBackgroundWork does not wedge forever on work that has gone away.
type panicOnceExtractor struct {
	calls atomic.Int64
}

func (p *panicOnceExtractor) ExtractFrom(_ context.Context, _ kernel.Event) ([]kernel.Event, error) {
	if p.calls.Add(1) == 1 {
		panic("synthetic extractor panic")
	}
	return nil, nil
}

func episodeEvent(text string) kernel.Event {
	return kernel.Event{
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"` + text + `"}`),
	}
}

// TestBackgroundEnrichmentRecoversPanic pins the daemon-death fix: a panic in
// the background extractor must neither crash the process nor leak the
// bgWork/bgWorkDepth counters (which would make idleStatus report work forever
// and wedge WaitForBackgroundWork). It runs both enrichment paths — the spill
// path (no worker pool) and the bounded worker pool — since the pre-fix
// worker-loop decrement was not deferred and neither path recovered.
//
// Red-proof: without runEnrichmentJob's recover, the first Append's enrichment
// panics on a background goroutine and aborts the entire test binary; without
// the deferred counter release, WaitForBackgroundWork below blocks until the
// context deadline and the test times out.
func TestBackgroundEnrichmentRecoversPanic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		workers int
	}{
		{"spill path no worker pool", 0},
		{"bounded worker pool", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := testutil.TestContext(t)
			ex := &panicOnceExtractor{}
			opts := []kernel.Option{kernel.WithExtractor(ex)}
			if tc.workers > 0 {
				opts = append(opts, kernel.WithExtractionWorkers(tc.workers))
			}
			log := kernel.NewFileEventLog(t.TempDir(), &fakeOntology{}, opts...)
			t.Cleanup(func() { _ = log.Close() })

			// First Append: its background enrichment panics. The daemon must
			// survive and release the in-flight counter.
			if _, err := log.Append(ctx, []kernel.Event{episodeEvent("Frodo works at Erebor Inc.")}); err != nil {
				t.Fatalf("Append (panic case): %v", err)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := log.WaitForBackgroundWork(waitCtx); err != nil {
				t.Fatalf("WaitForBackgroundWork after panic: %v (counter leaked — bgWork.Done skipped on panic)", err)
			}
			if depth := log.BackgroundWorkDepth(); depth != 0 {
				t.Fatalf("BackgroundWorkDepth after panic = %d, want 0 (counter leaked)", depth)
			}

			// Second Append: the extractor now succeeds. It must be processed,
			// proving the worker/daemon stayed alive through the panic.
			if _, err := log.Append(ctx, []kernel.Event{episodeEvent("Sam tends the garden.")}); err != nil {
				t.Fatalf("Append (recovery case): %v", err)
			}
			waitCtx2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
			defer cancel2()
			if err := log.WaitForBackgroundWork(waitCtx2); err != nil {
				t.Fatalf("WaitForBackgroundWork after recovery: %v", err)
			}
			if got := ex.calls.Load(); got < 2 {
				t.Fatalf("extractor ran %d times, want >= 2 (second Append not processed — pool died after panic)", got)
			}
		})
	}
}
