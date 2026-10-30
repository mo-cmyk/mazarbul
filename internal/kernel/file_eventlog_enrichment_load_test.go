package kernel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// gateExtractor blocks every ExtractFrom on a release channel so a test can
// pin a burst of enrichment in flight (saturating the worker pool's queue and
// spill slots), then release them all at once and assert a clean drain.
type gateExtractor struct {
	release chan struct{}
}

func (g *gateExtractor) ExtractFrom(ctx context.Context, _ kernel.Event) ([]kernel.Event, error) {
	select {
	case <-g.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestBackgroundEnrichmentSurvivesBurst pins the "daemon dies after ~20 ops"
// fix at the kernel layer: a burst of writes far exceeding the bounded worker
// pool's absorb capacity (queue + spill) must (a) never block Append, (b) cap
// the spilled goroutines and drop the overflow with a warn rather than spawn an
// unbounded herd, and (c) leave the in-flight counters perfectly balanced so
// the kernel returns to idle once the backlog drains — no leaked bgWork that
// would wedge idleStatus / WaitForBackgroundWork forever.
func TestBackgroundEnrichmentSurvivesBurst(t *testing.T) {
	t.Parallel()

	const workers = 1 // absorb capacity = queue(1*32) + spill(1*64) = 96
	const burst = 130 // > 96, so the tail must hit the drop path

	gate := &gateExtractor{release: make(chan struct{})}
	core, logs := observer.New(zap.WarnLevel)
	ctx := logging.WithLogger(testutil.TestContext(t), zap.New(core))

	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithExtractor(gate),
		kernel.WithExtractionWorkers(workers),
	)
	t.Cleanup(func() { _ = log.Close() })

	// Fire the burst. Each Append must return promptly even though every
	// extraction is gated — the enrichment is off the hot path.
	for i := range burst {
		ev := kernel.Event{
			Kind:       "episode",
			Source:     "test",
			Confidence: 1.0,
			Payload:    json.RawMessage(fmt.Sprintf(`{"text":"Episode number %d about Frodo and the Ring."}`, i)),
		}
		appendCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if _, err := log.Append(appendCtx, []kernel.Event{ev}); err != nil {
			cancel()
			t.Fatalf("Append %d blocked or failed: %v", i, err)
		}
		cancel()
	}

	// The overflow beyond the absorb capacity must be dropped (logged), not
	// spawned as unbounded goroutines.
	drops := logs.FilterMessageSnippet("enrichment backlog saturated").All()
	if len(drops) == 0 {
		t.Fatalf("expected some enrichment batches to be dropped under a %d burst with absorb capacity ~96, got none", burst)
	}

	// Release the gated extractions and assert the kernel drains to idle with
	// the in-flight counter back at zero — proving the dropped batches released
	// their counter and the spilled ones completed.
	close(gate.release)
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := log.WaitForBackgroundWork(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundWork after burst: %v (counter leaked under spill/drop)", err)
	}
	if depth := log.BackgroundWorkDepth(); depth != 0 {
		t.Fatalf("BackgroundWorkDepth after drain = %d, want 0", depth)
	}
}
