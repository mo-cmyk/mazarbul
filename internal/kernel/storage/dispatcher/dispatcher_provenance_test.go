package dispatcher_test

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// TestApply_WritesProvenanceForEpisode is the W1.1 ship gate: the dispatcher
// MUST write one provenance row per Episode event with source, confidence,
// and decided_at sourced from the Event itself. Before W1.1 this table was
// created in migration 0001 but never written to; AGENTS.md rule 4
// ("provenance on every fact") was documented but unenforced.
func TestApply_WritesProvenanceForEpisode(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	createTime := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	ev := makeEpisodeEvent(t, "01HZZZZZZZZZZZZZZZZZZZW1P1", "Eowyn works at Erebor Inc.", "maz remember")
	ev.CreateTime = createTime
	ev.Source = "maz remember"
	ev.Confidence = 0.95

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	recs, err := d.Provenance(ctx, ev.UID)
	if err != nil {
		t.Fatalf("Provenance: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("Provenance returned %d rows, want 1", len(recs))
	}
	got := recs[0]
	if got.EventUID != ev.UID {
		t.Errorf("EventUID = %q, want %q", got.EventUID, ev.UID)
	}
	if got.Source != "maz remember" {
		t.Errorf("Source = %q, want %q", got.Source, "maz remember")
	}
	if got.Confidence != 0.95 {
		t.Errorf("Confidence = %v, want 0.95", got.Confidence)
	}
	if !got.DecidedAt.Equal(createTime) {
		t.Errorf("DecidedAt = %v, want %v", got.DecidedAt, createTime)
	}
}

// TestApply_ProvenanceIdempotentOnReplay pins the ON CONFLICT(event_uid)
// DO UPDATE behaviour added by migration 0009. Replaying the same Episode
// event (the rebuild path) must overwrite the row, not fail on a UNIQUE
// violation and not produce a duplicate.
func TestApply_ProvenanceIdempotentOnReplay(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	ev := makeEpisodeEvent(t, "01HZZZZZZZZZZZZZZZZZZZW1P2", "first", "first-source")
	ev.CreateTime = time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	ev.Source = "first-source"
	ev.Confidence = 0.5

	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply first: %v", err)
	}

	// Replay with different source/confidence/createTime: the UPDATE branch
	// of ON CONFLICT must overwrite all three columns.
	ev.CreateTime = time.Date(2026, 5, 27, 11, 0, 0, 0, time.UTC)
	ev.Source = "second-source"
	ev.Confidence = 0.99
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply replay: %v", err)
	}

	recs, err := d.Provenance(ctx, ev.UID)
	if err != nil {
		t.Fatalf("Provenance: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("Provenance returned %d rows after replay, want 1 (UNIQUE constraint should collapse duplicates)", len(recs))
	}
	if recs[0].Source != "second-source" {
		t.Errorf("Source after replay = %q, want %q", recs[0].Source, "second-source")
	}
	if recs[0].Confidence != 0.99 {
		t.Errorf("Confidence after replay = %v, want 0.99", recs[0].Confidence)
	}
}

// TestProvenance_UnknownUIDReturnsEmpty pins the contract documented on
// EventLog.Provenance: a UID with no provenance row returns (nil, nil),
// not an error.
func TestProvenance_UnknownUIDReturnsEmpty(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	recs, err := d.Provenance(ctx, "01HZZZZZZZZZZZZZZZZZZZNONE")
	if err != nil {
		t.Fatalf("Provenance: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("Provenance returned %d rows for unknown uid, want 0", len(recs))
	}
}

// TestProvenance_ClosedDispatcherReturnsError ensures the read path
// honours the same closed-check semantics as HasEvent so a Close race
// surfaces a clean error instead of operating on torn state.
func TestProvenance_ClosedDispatcherReturnsError(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	ev := makeEpisodeEvent(t, "01HZZZZZZZZZZZZZZZZZZZW1P4", "x", "test")
	ev.CreateTime = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := d.Provenance(context.Background(), ev.UID); err == nil {
		t.Error("Provenance on closed dispatcher: expected error, got nil")
	}
}
