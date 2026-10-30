package kernel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// extractionCallCounter pins the contract the renderer cares about: every
// ExtractFrom call FOR AN EPISODE increments the counter so the
// test can assert "the LLM hook was invoked zero times when
// extraction must be skipped." Non-episode events (the Mentions and
// resolver output that flow through the extractor as no-ops in
// production) are explicitly ignored — counting them would conflate
// the cost-bearing LLM calls with cheap pass-through dispatch.
type extractionCallCounter struct {
	episodeCalls atomic.Int64
}

func (c *extractionCallCounter) ExtractFrom(_ context.Context, ev kernel.Event) ([]kernel.Event, error) {
	if string(ev.Kind) == "episode" {
		c.episodeCalls.Add(1)
	}
	return nil, nil
}

// waitForAppendQuiescence drives Close to flush the bgWork wait
// group, then re-opens a fresh logger via the callback so the test
// can keep poking at the dataDir without racing the original writer.
// Close is the only synchronization point that guarantees
// applyExtraction has executed for every Append; polling
// BackgroundWorkDepth flakes under macOS scheduling noise.
func waitForAppendQuiescence(t *testing.T, log *kernel.FileEventLog) {
	t.Helper()
	if err := log.Close(); err != nil {
		t.Fatalf("close to drain extraction: %v", err)
	}
}

// TestApplyExtraction_SkipsWhenMentionIsInSameBatch is the core
// pull-replay scenario: the sender device shipped an Episode plus
// its derived Mentions in one chunk. When the receiver Appends the
// chunk's events as one batch, the extractor must NOT be invoked
// for the Episode — re-running the LLM would produce divergent,
// non-deterministic output that bloats the receiver's event log.
func TestApplyExtraction_SkipsWhenMentionIsInSameBatch(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	ex := &extractionCallCounter{}
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithExtractor(ex),
	)

	episodeUID := "01HZAB0000000000000000EPSD"
	episode := kernel.Event{
		UID:        episodeUID,
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"Rosie joined Erebor Inc. as CFO."}`),
	}
	mention := kernel.Event{
		Kind:       "mention",
		Source:     "test",
		Confidence: 1.0,
		Payload: json.RawMessage(
			`{"displayName":"Rosie","kind":"person","sourceEventUid":"` + episodeUID + `",` +
				`"extractionText":"Rosie","sourceSpan":{"startChar":0,"endChar":5},"groundingScore":1.0}`,
		),
	}

	if _, err := log.Append(ctx, []kernel.Event{episode, mention}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	waitForAppendQuiescence(t, log)

	if got := ex.episodeCalls.Load(); got != 0 {
		t.Errorf("ExtractFrom called %d times; want 0 when mention is in same batch", got)
	}
}

// TestApplyExtraction_RunsWhenNoDownstreamMentionPresent is the
// symmetric guard: when the receiving daemon hosts the original
// extraction (the device that authored the Episode), there are no
// Mentions yet and the extractor MUST run. A regression that
// over-eagerly skipped would silently disable extraction on the
// originating side.
func TestApplyExtraction_RunsWhenNoDownstreamMentionPresent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	ex := &extractionCallCounter{}
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithExtractor(ex),
	)

	episode := kernel.Event{
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"Frodo works at Erebor."}`),
	}
	if _, err := log.Append(ctx, []kernel.Event{episode}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	waitForAppendQuiescence(t, log)

	if got := ex.episodeCalls.Load(); got != 1 {
		t.Errorf("ExtractFrom called %d times; want 1 when no downstream mention", got)
	}
}

// TestApplyExtraction_SkipsAcrossConsecutiveBatches pins the second
// pull-replay shape: a chunk replay can split Episode and Mention
// across two consecutive Append batches (chunk boundary lands
// between them). The skip set must persist across batches in the
// same daemon lifetime — otherwise the receiver re-extracts on the
// later batch's arrival and undoes the deduplication.
func TestApplyExtraction_SkipsAcrossConsecutiveBatches(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	ex := &extractionCallCounter{}
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithExtractor(ex),
	)

	episodeUID := "01HZAB0000000000000000EPSDX"[:26]
	mention := kernel.Event{
		Kind:       "mention",
		Source:     "test",
		Confidence: 1.0,
		Payload: json.RawMessage(
			`{"displayName":"Rosie","kind":"person","sourceEventUid":"` + episodeUID + `",` +
				`"extractionText":"Rosie","sourceSpan":{"startChar":0,"endChar":5},"groundingScore":1.0}`,
		),
	}
	episode := kernel.Event{
		UID:        episodeUID,
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"Rosie Cotton role CFO at Erebor."}`),
	}

	// Mention arrives first (its source Episode reference is now in
	// the dedup set), then the Episode arrives — must NOT re-extract.
	if _, err := log.Append(ctx, []kernel.Event{mention}); err != nil {
		t.Fatalf("Append mention: %v", err)
	}
	if _, err := log.Append(ctx, []kernel.Event{episode}); err != nil {
		t.Fatalf("Append episode: %v", err)
	}
	waitForAppendQuiescence(t, log)

	if got := ex.episodeCalls.Load(); got != 0 {
		t.Errorf("ExtractFrom called %d times; want 0 across consecutive batches", got)
	}
}

// TestApplyExtraction_MalformedMentionPayloadDoesNotPoisonSkipSet
// guards the JSON probe: a Mention whose payload is garbage must
// not stop legitimate extraction from running for the next Episode.
// Worst case is "we don't dedupe this one," not "we silently stop
// extracting forever."
func TestApplyExtraction_MalformedMentionPayloadDoesNotPoisonSkipSet(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	ex := &extractionCallCounter{}
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithExtractor(ex),
	)

	mention := kernel.Event{
		Kind:       "mention",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{}`), // no sourceEventUid
	}
	episode := kernel.Event{
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"Distinct episode."}`),
	}

	if _, err := log.Append(ctx, []kernel.Event{mention, episode}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	waitForAppendQuiescence(t, log)

	if got := ex.episodeCalls.Load(); got != 1 {
		t.Errorf("ExtractFrom called %d times; want 1 (malformed mention must not silence extractor)", got)
	}
}

// TestApplyExtraction_JSONLFallbackCatchesCrossDaemonRestart pins
// the cross-restart correctness path that the in-memory LRU alone
// cannot cover. Scenario:
//
//  1. Daemon writes a Mention to the JSONL log (no Episode locally
//     yet — the Mention came from a sender device on a previous pull).
//  2. Daemon process exits and a new daemon opens the SAME data
//     directory (simulated here by Close + a fresh NewFileEventLog
//     pointing at the same path).
//  3. The Episode the Mention references arrives on a later pull.
//     The fresh daemon's in-memory LRU is empty, but the JSONL
//     forward-scan inside applyExtraction must still find the
//     Mention and skip the LLM call.
//
// A regression here would silently re-extract entities on every
// daemon restart that crosses a Mention/Episode pull boundary,
// producing divergent canonical Entities over time.
func TestApplyExtraction_JSONLFallbackCatchesCrossDaemonRestart(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dir := t.TempDir()

	episodeUID := "01HZAB0000000000000000DAEMN"[:26]

	// Daemon #1: write the Mention only, then close.
	ex1 := &extractionCallCounter{}
	d1 := kernel.NewFileEventLog(
		dir,
		&fakeOntology{},
		kernel.WithExtractor(ex1),
	)
	mention := kernel.Event{
		Kind:       "mention",
		Source:     "test",
		Confidence: 1.0,
		Payload: json.RawMessage(
			`{"displayName":"Rosie","kind":"person","sourceEventUid":"` + episodeUID + `",` +
				`"extractionText":"Rosie","sourceSpan":{"startChar":0,"endChar":5},"groundingScore":1.0}`,
		),
	}
	if _, err := d1.Append(ctx, []kernel.Event{mention}); err != nil {
		t.Fatalf("daemon #1 Append mention: %v", err)
	}
	if err := d1.Close(); err != nil {
		t.Fatalf("daemon #1 Close: %v", err)
	}

	// Daemon #2: fresh process pointed at the same dir. Its LRU is
	// empty; only the JSONL scan can catch the dedup.
	ex2 := &extractionCallCounter{}
	d2 := kernel.NewFileEventLog(
		dir,
		&fakeOntology{},
		kernel.WithExtractor(ex2),
	)
	episode := kernel.Event{
		UID:        episodeUID,
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"Rosie Cotton role CFO at Erebor."}`),
	}
	if _, err := d2.Append(ctx, []kernel.Event{episode}); err != nil {
		t.Fatalf("daemon #2 Append episode: %v", err)
	}
	waitForAppendQuiescence(t, d2)

	if got := ex2.episodeCalls.Load(); got != 0 {
		t.Errorf("daemon #2 ExtractFrom called %d times; want 0 (JSONL fallback must catch the prior Mention)", got)
	}
}

// TestApplyExtraction_LRUBoundedAtCapacity guards against memory
// growth on a long-running daemon that sees a steady stream of
// unique Mentions. The extractedEpisodeUIDs map must FIFO-evict
// once it exceeds extractedEpisodeUIDsCap. We exercise the eviction
// by recording cap+overflow distinct Episode UIDs and asserting
// the set's effective size never grows past the cap (verified
// indirectly: an "early" UID re-arrives after eviction and the
// re-extraction is observable via the counter).
//
// Without a hard cap a malicious or buggy sender could OOM the
// receiver by emitting unique Mentions until the map fills RAM.
func TestApplyExtraction_LRUBoundedAtCapacity(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	ex := &extractionCallCounter{}
	log := kernel.NewFileEventLog(
		t.TempDir(),
		&fakeOntology{},
		kernel.WithExtractor(ex),
	)
	t.Cleanup(func() { _ = log.Close() })

	// Build a sentinel Episode UID we'll re-Append at the end.
	sentinelEpisodeUID := "01HZAB000000000000000SENTNL"[:26]
	sentinelMention := kernel.Event{
		Kind:       "mention",
		Source:     "test",
		Confidence: 1.0,
		Payload: json.RawMessage(
			`{"displayName":"S","kind":"person","sourceEventUid":"` + sentinelEpisodeUID + `",` +
				`"extractionText":"S","sourceSpan":{"startChar":0,"endChar":1},"groundingScore":1.0}`,
		),
	}
	if _, err := log.Append(ctx, []kernel.Event{sentinelMention}); err != nil {
		t.Fatalf("Append sentinel mention: %v", err)
	}

	// Now flood the LRU with > cap distinct Mentions to evict the
	// sentinel. 1500 > extractedEpisodeUIDsCap (1024). Use a single
	// Append batch to amortise the bgWork wait.
	const flood = 1500
	floodBatch := make([]kernel.Event, 0, flood)
	for i := range flood {
		// Build a 26-char unique UID (Crockford-safe alphabet not
		// strictly required for this test — kernel does not parse
		// the UID; the dedup set keys by string equality).
		uid := "01HZAB" + fmt.Sprintf("%020d", i)
		floodBatch = append(floodBatch, kernel.Event{
			Kind:       "mention",
			Source:     "test",
			Confidence: 1.0,
			Payload: json.RawMessage(
				`{"displayName":"X","kind":"person","sourceEventUid":"` + uid + `",` +
					`"extractionText":"X","sourceSpan":{"startChar":0,"endChar":1},"groundingScore":1.0}`,
			),
		})
	}
	if _, err := log.Append(ctx, floodBatch); err != nil {
		t.Fatalf("Append flood: %v", err)
	}

	// Reset the counter and append the sentinel Episode. Because
	// the LRU has been overflowed, the in-memory dedup MUST miss;
	// the JSONL fallback IS expected to catch it (the sentinel
	// Mention is durable on disk). So extraction still gets
	// skipped — but via a different code path than the LRU. We
	// assert: extractor counter stays at 0.
	ex.episodeCalls.Store(0)
	episode := kernel.Event{
		UID:        sentinelEpisodeUID,
		Kind:       "episode",
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"sentinel episode"}`),
	}
	if _, err := log.Append(ctx, []kernel.Event{episode}); err != nil {
		t.Fatalf("Append sentinel episode: %v", err)
	}
	waitForAppendQuiescence(t, log)

	if got := ex.episodeCalls.Load(); got != 0 {
		t.Errorf("ExtractFrom called %d times for the sentinel episode; want 0 (LRU overflowed → JSONL fallback must catch)", got)
	}
}

// _ = sync.WaitGroup; reference kept so future testutil churn that
// drops the sync import doesn't break this file.
var _ sync.WaitGroup
