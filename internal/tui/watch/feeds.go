package watch

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

// EventKind classifies an incoming event so panes can route it.
type EventKind string

const (
	// EventKindExtraction is emitted when the kernel extracts entities from an episode.
	EventKindExtraction EventKind = "extraction"
	// EventKindEmbedding is emitted when the embedder processes a chunk.
	EventKindEmbedding EventKind = "embedding"
	// EventKindSync is emitted on push/pull operations.
	EventKindSync EventKind = "sync"
	// EventKindSystem is emitted for daemon lifecycle events.
	EventKindSystem EventKind = "system"
)

// Event is one item from the SSE stream.
type Event struct {
	Kind    EventKind
	TS      time.Time
	Payload map[string]any
}

// ExtractionJob is a snapshot of one in-flight extraction pipeline step.
type ExtractionJob struct {
	ID       string
	Pipeline string // e.g. "epi→ent"
	Progress float64
	Done     bool
	// Detail is an optional human note (e.g. "3 in flight · 5 queued").
	// A negative Progress marks an indeterminate gauge row: the renderer
	// shows Detail in place of a fractional progress bar. Background
	// enrichment (the boot/ingest drain) surfaces this way because it
	// reports an aggregate depth, not per-job completion.
	Detail string
}

// EmbeddingStats is a point-in-time snapshot of the embedder queue.
type EmbeddingStats struct {
	Depth   int
	EnqRate float64 // items/s
	Workers int
	Active  int
}

// SyncStatus is a point-in-time snapshot of the sync subsystem.
type SyncStatus struct {
	Remote   string
	Ahead    int
	Behind   int
	LastPush time.Time
}

// SourceStatus classifies a connector source's health.
type SourceStatus int

const (
	// SourceLive means the watermark advanced within the last 24 hours.
	SourceLive SourceStatus = iota
	// SourceStale means the watermark advanced more than 24 hours ago but less than 7 days.
	SourceStale
	// SourceDown means the watermark has never advanced or is more than 7 days stale.
	SourceDown
	// SourceNotConnected means the connector is registered but not yet connected.
	SourceNotConnected
)

// SourceRow is one row in the sources pane: a (connector, stream) watermark entry.
// For not-connected rows the feeder pre-sets Status=SourceNotConnected and leaves Stream "".
// For connected rows the pane derives Status from AdvanceTime against its own tick time.
type SourceRow struct {
	Connector   string
	Stream      string // "" for not-connected rows
	Sequence    uint64
	AdvanceTime time.Time // zero = never advanced
	Status      SourceStatus
}

// EventStreamer is the interface the watch panes read from. Two
// implementations satisfy it: the live daemonStreamer (internal/cli),
// which dials GET /v1/events:stream over SSE for Events and polls the
// /v1/jobs and /v1/sources REST endpoints for the snapshots, and
// FakeEventStreamer below, which generates synthetic data for tests and
// offline demos.
type EventStreamer interface {
	// Events returns a channel of raw events. Close ctx to stop.
	Events(ctx context.Context) <-chan Event
	// Jobs returns the current extraction job list (polled).
	Jobs(ctx context.Context) []ExtractionJob
	// Embedding returns the current embedder queue snapshot (polled).
	Embedding(ctx context.Context) EmbeddingStats
	// Sync returns the current sync status (polled).
	Sync(ctx context.Context) SyncStatus
	// Sources returns the current connector watermark snapshot (polled).
	Sources(ctx context.Context) []SourceRow
}

// FakeEventStreamer generates synthetic events for tests and offline demos.
type FakeEventStreamer struct {
	// Seed controls determinism; zero = random.
	Seed int64
}

// Events emits a synthetic event every ~400 ms.
func (f *FakeEventStreamer) Events(ctx context.Context) <-chan Event {
	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		r := rand.New(rand.NewSource(f.Seed))
		kinds := []EventKind{EventKindExtraction, EventKindEmbedding, EventKindSync, EventKindSystem}
		labels := map[EventKind][]string{
			EventKindExtraction: {"episode→entity", "entity→relation", "entity resolved"},
			EventKindEmbedding:  {"chunk queued", "batch embedded", "model loaded"},
			EventKindSync:       {"push ok", "pull ok", "conflict"},
			EventKindSystem:     {"daemon start", "config reload", "capacity warn"},
		}
		tick := time.NewTicker(400 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-tick.C:
				kind := kinds[r.Intn(len(kinds))]
				msgs := labels[kind]
				ch <- Event{
					Kind: kind,
					TS:   t,
					Payload: map[string]any{
						"msg":  msgs[r.Intn(len(msgs))],
						"size": fmt.Sprintf("%.1fK", r.Float64()*2),
					},
				}
			}
		}
	}()
	return ch
}

// Jobs returns a static set of fake extraction jobs.
func (f *FakeEventStreamer) Jobs(_ context.Context) []ExtractionJob {
	return []ExtractionJob{
		{ID: "ext-01HZ3A", Pipeline: "epi→ent", Progress: 0.85},
		{ID: "ext-01HZ3B", Pipeline: "ent→rel", Progress: 1.0, Done: true},
		{ID: "ext-01HZ3C", Pipeline: "epi→ent", Progress: 0.42},
	}
}

// Embedding returns a static fake embedder queue snapshot.
func (f *FakeEventStreamer) Embedding(_ context.Context) EmbeddingStats {
	return EmbeddingStats{Depth: 412, EnqRate: 18.3, Workers: 4, Active: 1}
}

// Sync returns a static fake sync status.
func (f *FakeEventStreamer) Sync(_ context.Context) SyncStatus {
	return SyncStatus{
		Remote:   "origin/main",
		Ahead:    12,
		Behind:   0,
		LastPush: time.Now().Add(-13 * time.Minute),
	}
}

// Sources returns a deterministic mix of connector watermarks for offline demos
// and tests: one live (< 24h), one stale (> 24h), one never-advanced (down),
// and one not-connected row. Status is set explicitly on every row (the live
// path classifies via ingest.ClassifyWatermark; the pane renders Status and
// never re-derives it), with AdvanceTime kept for the relative-age label.
func (f *FakeEventStreamer) Sources(_ context.Context) []SourceRow {
	now := time.Now()
	return []SourceRow{
		{
			Connector:   "local-git",
			Stream:      "main",
			Sequence:    1842,
			AdvanceTime: now.Add(-47 * time.Minute), // live: last advance < 24h
			Status:      SourceLive,
		},
		{
			Connector:   "github",
			Stream:      "commits",
			Sequence:    304,
			AdvanceTime: now.Add(-30 * time.Hour), // stale: last advance > 24h
			Status:      SourceStale,
		},
		{
			Connector:   "slack",
			Stream:      "general",
			Sequence:    0,
			AdvanceTime: time.Time{}, // never advanced → down
			Status:      SourceDown,
		},
		{
			Connector: "notion",
			Status:    SourceNotConnected, // registered but not yet connected
		},
	}
}
