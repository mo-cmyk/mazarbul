package cli

// watch_streamer.go bridges daemonclient.StreamEvents into the watch
// package's EventStreamer interface so `maz watch` reflects the real
// daemon state. Events flow over SSE; Jobs, Embedding, and Sync
// snapshots come from polled REST endpoints on the same client.

import (
	"context"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/daemonclient"
	"github.com/mo-cmyk/mazarbul/internal/ingest"
	"github.com/mo-cmyk/mazarbul/internal/tui/watch"
)

// daemonStreamer satisfies watch.EventStreamer over a daemonclient.Client.
// Events are received over /v1/events:stream; the polled snapshots
// (Jobs, Embedding, Sync) hit the matching list endpoints when the
// dashboard requests them.
type daemonStreamer struct {
	ctx    context.Context
	client *daemonclient.Client
}

func newDaemonStreamer(ctx context.Context, client *daemonclient.Client) *daemonStreamer {
	return &daemonStreamer{ctx: ctx, client: client}
}

// Events maps SSE EventRecords into watch.Event values. The pane kind
// is derived from the daemon's event Kind so an `entity` event lands
// in the extraction pane, a `sync.push` event lands in the sync pane,
// etc. An unknown daemon kind routes to EventKindSystem so the user
// still sees activity rather than silent drops.
func (d *daemonStreamer) Events(ctx context.Context) <-chan watch.Event {
	out := make(chan watch.Event, 64)
	src, cancel, err := d.client.StreamEvents(ctx, daemonclient.StreamEventsFilter{})
	if err != nil {
		// Surface the error as a single system-pane event so the user
		// sees why the dashboard is empty instead of staring at a
		// frozen screen.
		go func() {
			defer close(out)
			out <- watch.Event{
				Kind:    watch.EventKindSystem,
				TS:      time.Now(),
				Payload: map[string]any{"error": err.Error(), "hint": "is the daemon running? run `maz daemon start`"},
			}
		}()
		return out
	}
	go func() {
		defer close(out)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-src:
				if !ok {
					return
				}
				out <- watch.Event{
					Kind:    paneKindFor(ev.Kind),
					TS:      ev.CreateTime,
					Payload: map[string]any{"kind": ev.Kind, "uid": ev.UID, "source": ev.Source},
				}
			}
		}
	}()
	return out
}

// paneKindFor maps a daemon Event.Kind onto the watch dashboard's
// four panes. The classification mirrors what the rendered/ tree
// shows so an operator sees the same pipeline structure they see in
// `maz changelog`.
func paneKindFor(daemonKind string) watch.EventKind {
	switch daemonKind {
	case "episode", diffKindEntity, "mention", diffKindRelation,
		"entityIdentifierAdded", "relationCandidatesProposed":
		return watch.EventKindExtraction
	case "embedding", "embedder.queued", "embedder.completed":
		return watch.EventKindEmbedding
	case "sync.push", "sync.pull", "sync.conflict":
		return watch.EventKindSync
	default:
		return watch.EventKindSystem
	}
}

// Jobs maps the daemon's GET /v1/jobs snapshot onto the extraction-jobs
// pane. It surfaces the background-enrichment drain (an aggregate depth
// gauge — see mapJobsSnapshot) plus any running ingest/rebuild runs the
// registry tracks. On a fetch error it returns nil so the pane shows
// "(no active jobs)" instead of stale data.
func (d *daemonStreamer) Jobs(ctx context.Context) []watch.ExtractionJob {
	snap, err := d.client.ListJobs(ctx)
	if err != nil {
		return nil
	}
	return mapJobsSnapshot(snap)
}

// Embedding maps the embedding background-drain depth onto the embedding
// queue pane. Depth is the count of in-flight dense-vector upserts the
// dispatcher is draining; zero values render the "no data" state.
func (d *daemonStreamer) Embedding(ctx context.Context) watch.EmbeddingStats {
	snap, err := d.client.ListJobs(ctx)
	if err != nil {
		return watch.EmbeddingStats{}
	}
	return watch.EmbeddingStats{Depth: snap.Queues.Embedding.InFlight}
}

// Sync stays a zero snapshot: the daemon exposes no sync-status /v1
// endpoint yet, so the pane renders "no data" rather than fabricating
// ahead/behind counts. Use `maz sync status` for live sync state.
func (d *daemonStreamer) Sync(_ context.Context) watch.SyncStatus {
	return watch.SyncStatus{}
}

// Sources maps the daemon's GET /v1/sources snapshot onto the sources
// pane. Each persisted watermark becomes a row carrying its last-advance
// time and a freshness Status classified by ingest.ClassifyWatermark (the
// single source of truth shared with `maz source list` and `maz doctor`);
// registered connectors with no watermark are appended as not-connected
// rows. On a fetch error it returns nil so the pane shows "(no sources)"
// rather than stale data.
func (d *daemonStreamer) Sources(ctx context.Context) []watch.SourceRow {
	snap, err := d.client.ListSources(ctx)
	if err != nil {
		return nil
	}
	return mapSourcesSnapshot(snap, time.Now())
}

// mapSourcesSnapshot is the pure mapping from a daemon /v1/sources snapshot
// to sources-pane rows. Each watermark's freshness is classified by
// ingest.ClassifyWatermark(advanceTime, now) — the same function `maz source
// list` and `maz doctor` use — so all three surfaces agree on the live/stale/
// down boundaries (no re-derivation in the pane). An unparseable or empty
// AdvanceTime yields the zero instant (rendered as never-advanced) and
// classifies as down. Registered connectors absent from the watermark set are
// appended as SourceNotConnected rows. Kept side-effect free (now is passed
// in) so the mapping is unit-tested without a live daemon or wall clock.
func mapSourcesSnapshot(snap daemonclient.SourcesResponse, now time.Time) []watch.SourceRow {
	seen := make(map[string]bool, len(snap.Watermarks))
	out := make([]watch.SourceRow, 0, len(snap.Watermarks)+len(snap.Connectors))
	for _, wm := range snap.Watermarks {
		seen[wm.Connector] = true
		var adv time.Time
		if wm.AdvanceTime != "" {
			if t, err := time.Parse(time.RFC3339Nano, wm.AdvanceTime); err == nil {
				adv = t
			}
		}
		status, _ := ingest.ClassifyWatermark(wm.AdvanceTime, now)
		out = append(out, watch.SourceRow{
			Connector:   wm.Connector,
			Stream:      wm.Stream,
			Sequence:    wm.Sequence,
			AdvanceTime: adv,
			Status:      watchSourceStatus(status),
		})
	}
	for _, name := range snap.Connectors {
		if !seen[name] {
			out = append(out, watch.SourceRow{Connector: name, Status: watch.SourceNotConnected})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// watchSourceStatus maps an ingest watermark-status string (the
// ClassifyWatermark vocabulary) onto the watch pane's SourceStatus enum.
// An unrecognized value falls back to SourceDown — the safe "needs attention"
// classification — rather than masquerading as live.
func watchSourceStatus(s string) watch.SourceStatus {
	switch s {
	case ingest.WatermarkLive:
		return watch.SourceLive
	case ingest.WatermarkStale:
		return watch.SourceStale
	default:
		return watch.SourceDown
	}
}

// mapJobsSnapshot is the pure mapping from a daemon jobs snapshot to the
// extraction-jobs pane rows. It is kept side-effect free so the mapping
// is unit-tested without a live daemon. The background-enrichment drain
// is prepended as an indeterminate gauge row (Progress < 0) whenever any
// extraction work is in flight or queued; running registry jobs follow
// with real progress. Completed/failed history is omitted — the pane
// shows live in-flight work only.
func mapJobsSnapshot(snap daemonclient.JobsResponse) []watch.ExtractionJob {
	out := make([]watch.ExtractionJob, 0, len(snap.Jobs)+1)
	if ex := snap.Queues.Extraction; ex.InFlight > 0 || ex.Queued > 0 {
		detail := fmt.Sprintf("%d in flight", ex.InFlight)
		if ex.Queued > 0 {
			detail += fmt.Sprintf(" · %d queued", ex.Queued)
		}
		out = append(out, watch.ExtractionJob{
			ID:       "enrichment",
			Pipeline: "epi→ent",
			Progress: -1,
			Detail:   detail,
		})
	}
	for i := range snap.Jobs {
		j := &snap.Jobs[i]
		if j.State != daemonclient.JobStateRunning {
			continue
		}
		out = append(out, watch.ExtractionJob{
			ID:       j.UID,
			Pipeline: pipelineForJobKind(j.Kind),
			Progress: j.Progress,
			Detail:   j.Detail,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pipelineForJobKind maps a registry job Kind onto a short pane label.
func pipelineForJobKind(kind string) string {
	switch kind {
	case daemonclient.JobKindExtraction:
		return "epi→ent"
	case daemonclient.JobKindIngest:
		return daemonclient.JobKindIngest
	case daemonclient.JobKindRebuild:
		return daemonclient.JobKindRebuild
	case "":
		return "job"
	default:
		return kind
	}
}
