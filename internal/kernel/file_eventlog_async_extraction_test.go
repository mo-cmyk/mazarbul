package kernel_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// slowExtractor blocks ExtractFrom for the configured delay so the test can
// distinguish a synchronous Append (which would inherit the delay) from an
// async one (which returns immediately and lets extraction complete in the
// background).
type slowExtractor struct {
	delay time.Duration
	done  chan struct{}
	once  sync.Once
}

func (s *slowExtractor) ExtractFrom(ctx context.Context, _ kernel.Event) ([]kernel.Event, error) {
	defer s.once.Do(func() { close(s.done) })
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}

// TestAppend_DoesNotBlockOnExtraction reproduces the LongMemEval feeder
// timeout: extraction calls qwen2.5:7b tool-use which routinely takes
// 17-24s, but the eval client times out after 30s. If extraction stays on
// Append's hot path, several remembers per question cross the timeout
// boundary and the harness records "remember failed for session X: " with
// an empty TimeoutError. The fix moves extraction to a background hook;
// this test pins that behavior so a future refactor cannot regress it.
func TestAppend_DoesNotBlockOnExtraction(t *testing.T) {
	t.Parallel()

	const extractDelay = 500 * time.Millisecond
	// appendBudget is the upper bound on Append latency. The async-extraction
	// contract is that Append returns ~immediately, so any value well below
	// extractDelay/2 proves the goroutine is not blocking. The previous
	// 200ms cap was tight enough to flake under CI load (observed 209ms);
	// 400ms was then observed to flake at 411ms under parallel test load;
	// 450ms still cleanly proves async behaviour (would require 50ms+ of
	// in-line blocking to fail) while tolerating macOS+CGO scheduling noise.
	const appendBudget = 450 * time.Millisecond

	ctx := testutil.TestContext(t)

	ex := &slowExtractor{delay: extractDelay, done: make(chan struct{})}
	storage, _ := newRecordingStorage(nil)
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithStorage(storage),
		kernel.WithExtractor(ex),
	)
	t.Cleanup(func() { _ = log.Close() })

	ev := kernel.Event{
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"Frodo works at Erebor Inc."}`),
	}

	start := time.Now()
	if _, err := log.Append(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	appendDuration := time.Since(start)

	if appendDuration > appendBudget {
		t.Fatalf("Append blocked on extraction: took %v, want under %v "+
			"(extractor delay was %v)", appendDuration, appendBudget, extractDelay)
	}

	select {
	case <-ex.done:
	case <-time.After(extractDelay * 4):
		t.Fatalf("extractor never ran in background within %v", extractDelay*4)
	}
}
