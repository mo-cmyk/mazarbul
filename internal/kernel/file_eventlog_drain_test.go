package kernel_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// trackingExtractor records each episode UID it was asked to extract.
// Concurrent-safe: episodeUIDs is guarded by mu.
type trackingExtractor struct {
	mu          sync.Mutex
	episodeUIDs []string
}

func (e *trackingExtractor) ExtractFrom(_ context.Context, ev kernel.Event) ([]kernel.Event, error) {
	if string(ev.Kind) != "episode" {
		return nil, nil
	}
	e.mu.Lock()
	e.episodeUIDs = append(e.episodeUIDs, ev.UID)
	e.mu.Unlock()
	return nil, nil
}

func (e *trackingExtractor) called(uid string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, u := range e.episodeUIDs {
		if u == uid {
			return true
		}
	}
	return false
}

// TestDrainUnextractedEpisodes_RequeuesUnextractedAndSkipsExtracted is the
// failing-test-first anchor for the boot-time backlog drain. The scenario:
//
//  1. Build a log WITHOUT an extractor so append-time extraction is a no-op.
//  2. Append several episode events — they land in the JSONL but are never
//     extracted because no extractor is wired.
//  3. Append one episode event WITH a downstream mention event so
//     hasDownstreamMentions returns true for it (it should be skipped).
//  4. Close that log and open a new one over the SAME dir, this time with a
//     tracking extractor installed.
//  5. Call DrainUnextractedEpisodes; it must enqueue the three un-extracted
//     episodes and skip the one that already has a mention.
//  6. WaitForBackgroundWork to let the queue drain.
//  7. Assert: the three un-extracted episodes were passed to ExtractFrom; the
//     episode with a prior mention was not.
func TestDrainUnextractedEpisodes_RequeuesUnextractedAndSkipsExtracted(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dir := t.TempDir()

	// Phase 1: write episodes into the log with no extractor so they stay
	// un-extracted.
	const (
		episodeUID1 = "01JZDRN000000000000000EPD1"
		episodeUID2 = "01JZDRN000000000000000EPD2"
		episodeUID3 = "01JZDRN000000000000000EPD3"
		// episodeUID4 will have a downstream mention written alongside it.
		episodeUID4 = "01JZDRN000000000000000EPD4"
	)

	// Writer log: no extractor, so nothing is extracted at append time.
	writerLog := kernel.NewFileEventLog(dir, &fakeOntology{})

	episodes := []kernel.Event{
		{
			UID:        episodeUID1,
			Kind:       "episode",
			Source:     "test",
			Confidence: 1.0,
			Payload:    json.RawMessage(`{"text":"Frodo joined Erebor."}`),
		},
		{
			UID:        episodeUID2,
			Kind:       "episode",
			Source:     "test",
			Confidence: 1.0,
			Payload:    json.RawMessage(`{"text":"Sam left the Shire."}`),
		},
		{
			UID:        episodeUID3,
			Kind:       "episode",
			Source:     "test",
			Confidence: 1.0,
			Payload:    json.RawMessage(`{"text":"Gandalf arrived at Rivendell."}`),
		},
	}
	if _, err := writerLog.Append(ctx, episodes); err != nil {
		t.Fatalf("Append un-extracted episodes: %v", err)
	}

	// Write episode4 WITH a downstream mention so DrainUnextractedEpisodes
	// must skip it (hasDownstreamMentions returns true).
	episode4 := kernel.Event{
		UID:        episodeUID4,
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"Bilbo found the ring."}`),
	}
	mention4 := kernel.Event{
		Kind:       "mention",
		Source:     "test",
		Confidence: 1.0,
		Payload: json.RawMessage(
			`{"displayName":"Bilbo","kind":"person","sourceEventUid":"` + episodeUID4 + `",` +
				`"extractionText":"Bilbo","sourceSpan":{"startChar":0,"endChar":5},"groundingScore":1.0}`,
		),
	}
	if _, err := writerLog.Append(ctx, []kernel.Event{episode4, mention4}); err != nil {
		t.Fatalf("Append episode4+mention4: %v", err)
	}
	if err := writerLog.Close(); err != nil {
		t.Fatalf("Close writer log: %v", err)
	}

	// Phase 2: open a fresh log over the same dir with a tracking extractor.
	ex := &trackingExtractor{}
	drainLog := kernel.NewFileEventLog(dir, &fakeOntology{}, kernel.WithExtractor(ex))

	queued, err := drainLog.DrainUnextractedEpisodes(ctx)
	if err != nil {
		t.Fatalf("DrainUnextractedEpisodes: %v", err)
	}
	if err := drainLog.WaitForBackgroundWork(ctx); err != nil {
		t.Fatalf("WaitForBackgroundWork: %v", err)
	}
	if err := drainLog.Close(); err != nil {
		t.Fatalf("Close drain log: %v", err)
	}

	// The three un-extracted episodes must have been enqueued.
	if queued < 3 {
		t.Errorf("DrainUnextractedEpisodes returned queued=%d; want >= 3 (the 3 un-extracted episodes)", queued)
	}

	// The extractor must have been called for each un-extracted episode.
	for _, uid := range []string{episodeUID1, episodeUID2, episodeUID3} {
		if !ex.called(uid) {
			t.Errorf("extractor was NOT called for un-extracted episode %s", uid)
		}
	}

	// The extractor must NOT have been called for the episode with a prior mention.
	if ex.called(episodeUID4) {
		t.Errorf("extractor WAS called for episode4 which already has a downstream mention; want skipped")
	}
}

// TestDrainUnextractedEpisodes_NoOpWhenNoExtractor confirms that
// DrainUnextractedEpisodes returns (0, nil) immediately when no extractor
// is wired, without panicking or iterating the log.
func TestDrainUnextractedEpisodes_NoOpWhenNoExtractor(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dir := t.TempDir()

	log := kernel.NewFileEventLog(dir, &fakeOntology{}) // no extractor
	episode := kernel.Event{
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"A lonely episode."}`),
	}
	if _, err := log.Append(ctx, []kernel.Event{episode}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	queued, err := log.DrainUnextractedEpisodes(ctx)
	if err != nil {
		t.Fatalf("DrainUnextractedEpisodes: %v", err)
	}
	if queued != 0 {
		t.Errorf("DrainUnextractedEpisodes returned queued=%d; want 0 when no extractor wired", queued)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
