package kernel_test

import (
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestPassiveCaptureEpisode_SkipsExtraction pins the ambient-capture
// contract: an Episode produced by a passive client lifecycle hook
// (source "hook:claude-code/stop", written by `maz hook stop` on every
// conversation turn) must NOT trigger background entity/relation
// extraction. The Stop hook fires every turn, so auto-extracting each
// capture floods the knowledge graph with mis-typed, low-signal entities
// (the bug that forced a full database wipe). The raw Episode stays
// durable and searchable (asserted below); only the LLM extraction is
// suppressed.
//
// Extraction is the single chokepoint: the resolver only mints
// Entity/Relation events from the Mention + RelationCandidatesProposed
// events the extractor emits, so skipping ExtractFrom skips the whole
// graph-minting pipeline.
func TestPassiveCaptureEpisode_SkipsExtraction(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dir := t.TempDir()

	ex := &extractionCallCounter{}
	log := kernel.NewFileEventLog(
		dir,
		&fakeOntology{},
		kernel.WithExtractor(ex),
	)

	episode := kernel.Event{
		Kind:       "episode",
		Source:     "hook:claude-code/stop", // passive ambient capture
		Confidence: 0.6,
		Payload:    json.RawMessage(`{"text":"user: fix the tick handler\nassistant: done, the integer phase now increments"}`),
	}
	receipts, err := log.Append(ctx, []kernel.Event{episode})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	waitForAppendQuiescence(t, log)

	if got := ex.episodeCalls.Load(); got != 0 {
		t.Errorf("ExtractFrom called %d times for a passive capture; want 0 (passive captures must not auto-extract)", got)
	}

	// The raw Episode must remain durable so recall/FTS/vector can still
	// surface it — suppressing extraction must not drop the capture.
	if len(receipts) != 1 {
		t.Fatalf("Append returned %d receipts; want 1", len(receipts))
	}
	episodeUID := receipts[0].UID

	reopened := kernel.NewFileEventLog(dir, &fakeOntology{}, kernel.WithExtractor(&extractionCallCounter{}))
	t.Cleanup(func() { _ = reopened.Close() })
	found := false
	for ev, readErr := range reopened.Read(ctx, kernel.ReadOpts{Kinds: []kernel.EventKind{"episode"}}) {
		if readErr != nil {
			t.Fatalf("Read: %v", readErr)
		}
		if ev.UID == episodeUID {
			found = true
		}
	}
	if !found {
		t.Errorf("passive-capture Episode %s not found on the durable log; the raw capture must be kept", episodeUID)
	}
}

// TestDeliberateRememberEpisode_StillExtracts is the symmetric guard:
// a deliberate, agent-initiated remember (source "mcp:remember") must
// keep full extraction. This proves the passive-capture gate is narrow —
// it suppresses only ambient hook captures, never the primary write
// surface. A regression that over-broadly silenced extraction would fail
// here.
func TestDeliberateRememberEpisode_StillExtracts(t *testing.T) {
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
		Source:     "mcp:remember", // deliberate agent-initiated write
		Confidence: 1.0,
		Payload:    json.RawMessage(`{"text":"Frodo works at Erebor."}`),
	}
	if _, err := log.Append(ctx, []kernel.Event{episode}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	waitForAppendQuiescence(t, log)

	if got := ex.episodeCalls.Load(); got != 1 {
		t.Errorf("ExtractFrom called %d times for a deliberate remember; want 1 (deliberate writes keep full extraction)", got)
	}
}

// TestPassiveCaptureEpisode_SkippedByDrainAfterRestart pins the design
// property the gate depends on: keying on the persisted Source means the
// boot-time DrainUnextractedEpisodes catch-up pass also skips passive
// captures, so a daemon restart never re-extracts them. A passive episode
// has no downstream Mention by construction, so a naive drain would
// re-enqueue (and extract) it on every boot — reintroducing exactly the
// graph pollution this fix removes. The drain must skip it entirely while
// still draining a genuinely un-extracted deliberate episode.
//
// Mirrors TestDrainUnextractedEpisodes_RequeuesUnextractedAndSkipsExtracted:
// write WITHOUT an extractor → close → reopen WITH a tracking extractor →
// drain → assert.
func TestPassiveCaptureEpisode_SkippedByDrainAfterRestart(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dir := t.TempDir()

	const (
		passiveUID    = "01JZPASV00000000000000PASV"
		deliberateUID = "01JZPASV0000000000000DELIB"
	)

	// Phase 1: write both episodes with NO extractor wired, so neither is
	// extracted at append time and both land un-extracted on the log.
	writer := kernel.NewFileEventLog(dir, &fakeOntology{})
	episodes := []kernel.Event{
		{
			UID:        passiveUID,
			Kind:       "episode",
			Source:     "hook:claude-code/stop", // passive ambient capture
			Confidence: 0.6,
			Payload:    json.RawMessage(`{"text":"user: tweak the phase counter\nassistant: done"}`),
		},
		{
			UID:        deliberateUID,
			Kind:       "episode",
			Source:     "mcp:remember", // deliberate agent-initiated write
			Confidence: 1.0,
			Payload:    json.RawMessage(`{"text":"Frodo works at Erebor."}`),
		},
	}
	if _, err := writer.Append(ctx, episodes); err != nil {
		t.Fatalf("Append un-extracted episodes: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close writer log: %v", err)
	}

	// Phase 2: reopen over the same dir with a tracking extractor (the
	// "fresh daemon after restart" — its in-memory state is empty).
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

	if ex.called(passiveUID) {
		t.Errorf("extractor WAS called for the passive capture %s during drain; want skipped (the gate must survive restart)", passiveUID)
	}
	if !ex.called(deliberateUID) {
		t.Errorf("extractor was NOT called for the deliberate episode %s during drain; want extracted", deliberateUID)
	}
	if queued != 1 {
		t.Errorf("DrainUnextractedEpisodes returned queued=%d; want 1 (only the deliberate episode; the passive capture must not be counted)", queued)
	}
}
