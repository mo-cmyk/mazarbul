package kernel_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// extractorThatAppends is an Extractor that calls back into a FileEventLog
// via Append. This mirrors production: the real extractor reads an Episode
// event and Appends the derived Mention + RelationCandidatesProposed
// events, which themselves go through scheduleBackgroundEnrichment.
//
// The recursion is what triggers the producer-after-close race: by the
// time the worker's inner Append runs, Close() has already started and
// extractionQueue may be in the process of being closed.
type extractorThatAppends struct {
	log     *kernel.FileEventLog
	calls   atomic.Int32
	emitted atomic.Int32
}

func (e *extractorThatAppends) ExtractFrom(ctx context.Context, ev kernel.Event) ([]kernel.Event, error) {
	e.calls.Add(1)
	// Emit one derived event so applyExtraction's inner Append fires.
	if ev.Kind == "episode" {
		derived := kernel.Event{
			Kind:       "mention",
			Source:     "test-extractor",
			Confidence: 1.0,
			Payload:    json.RawMessage(`{"text":"derived"}`),
		}
		e.emitted.Add(1)
		return []kernel.Event{derived}, nil
	}
	return nil, nil
}

// TestClose_DoesNotPanicOnConcurrentExtractionAppend pins the
// "panic: send on closed channel" race observed running `maz remember`:
//
//  1. Caller appends an Episode.
//  2. The bounded worker pool picks up the extraction job.
//  3. Worker's applyExtraction calls Append on the derived events.
//  4. That inner Append hits scheduleBackgroundEnrichment, which
//     selects on extractionQueue.
//  5. Meanwhile the caller's deferred Close() closes
//     extractionStop AND extractionQueue.
//  6. The producer's select races: both arms (closed-send,
//     closed-receive) are ready; Go picks uniformly at random.
//     50% chance per inner Append of "send on closed channel".
//
// The test sets extraction workers > 0 so the bounded-pool path runs
// (not the spill goroutine), appends multiple events, then calls Close
// while extraction is mid-flight. Before the fix this panics on
// roughly every run. After the fix it must return cleanly.
//
// The test relies on Go's test runner failing the whole binary when a
// goroutine panics, so a single observed panic in any of the 20
// iterations is a failure.
func TestClose_DoesNotPanicOnConcurrentExtractionAppend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for iter := range 20 {
		ex := &extractorThatAppends{}
		log := kernel.NewFileEventLog(
			t.TempDir(),
			&fakeOntology{},
			kernel.WithExtractionWorkers(4),
			kernel.WithExtractor(ex),
		)
		ex.log = log

		// Append a few episodes so the worker pool has live jobs when
		// Close races in. With 4 workers and 6 episodes the inner
		// Append on derived events is virtually guaranteed to be
		// in-flight when Close starts.
		for i := range 6 {
			ev := kernel.Event{
				Kind:       "episode",
				Source:     "test",
				Confidence: 1.0,
				Payload: json.RawMessage(
					`{"text":"episode ` + string(rune('A'+i)) + `"}`,
				),
			}
			if _, err := log.Append(ctx, []kernel.Event{ev}); err != nil {
				t.Fatalf("iter %d: Append[%d]: %v", iter, i, err)
			}
		}

		// Close races with the in-flight workers. Pre-fix this panics
		// in a goroutine and kills the test binary. Post-fix it must
		// drain cleanly.
		if err := log.Close(); err != nil {
			t.Fatalf("iter %d: Close: %v", iter, err)
		}
	}
}
