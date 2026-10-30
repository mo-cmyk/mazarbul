package cli

import (
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/daemonclient"
	"github.com/mo-cmyk/mazarbul/internal/tui/watch"
)

func TestMapJobsSnapshot(t *testing.T) {
	t.Run("background extraction drain surfaces as an indeterminate gauge row", func(t *testing.T) {
		snap := daemonclient.JobsResponse{
			Queues: daemonclient.QueueDepths{
				Extraction: daemonclient.QueueDepth{InFlight: 3},
			},
		}
		got := mapJobsSnapshot(snap)
		if len(got) != 1 {
			t.Fatalf("want 1 row, got %d: %+v", len(got), got)
		}
		row := got[0]
		if row.Progress >= 0 {
			t.Errorf("gauge row must have negative Progress, got %v", row.Progress)
		}
		if row.ID != "enrichment" || row.Pipeline != "epi→ent" {
			t.Errorf("unexpected gauge row identity: %+v", row)
		}
		if row.Detail != "3 in flight" {
			t.Errorf("Detail = %q, want %q", row.Detail, "3 in flight")
		}
	})

	t.Run("queued depth is appended to the gauge detail", func(t *testing.T) {
		snap := daemonclient.JobsResponse{
			Queues: daemonclient.QueueDepths{
				Extraction: daemonclient.QueueDepth{InFlight: 3, Queued: 5},
			},
		}
		got := mapJobsSnapshot(snap)
		if len(got) != 1 || got[0].Detail != "3 in flight · 5 queued" {
			t.Fatalf("Detail = %q, want %q", got[0].Detail, "3 in flight · 5 queued")
		}
	})

	t.Run("only running registry jobs are surfaced", func(t *testing.T) {
		snap := daemonclient.JobsResponse{
			Jobs: []daemonclient.Job{
				{UID: "j-run", Kind: "ingest", State: "running", Progress: 0.4},
				{UID: "j-done", Kind: "ingest", State: "succeeded", Progress: 1},
				{UID: "j-fail", Kind: "rebuild", State: "failed"},
			},
		}
		got := mapJobsSnapshot(snap)
		if len(got) != 1 {
			t.Fatalf("want 1 running row, got %d: %+v", len(got), got)
		}
		if got[0].ID != "j-run" || got[0].Pipeline != "ingest" || got[0].Progress != 0.4 {
			t.Errorf("unexpected running row: %+v", got[0])
		}
	})

	t.Run("gauge precedes registry rows when both present", func(t *testing.T) {
		snap := daemonclient.JobsResponse{
			Jobs: []daemonclient.Job{
				{UID: "j-run", Kind: "ingest", State: "running", Progress: 0.2},
			},
			Queues: daemonclient.QueueDepths{
				Extraction: daemonclient.QueueDepth{InFlight: 1},
			},
		}
		got := mapJobsSnapshot(snap)
		if len(got) != 2 {
			t.Fatalf("want 2 rows, got %d: %+v", len(got), got)
		}
		if got[0].ID != "enrichment" || got[1].ID != "j-run" {
			t.Errorf("ordering wrong: %+v", got)
		}
	})

	t.Run("quiescent snapshot maps to nil so the pane shows no active jobs", func(t *testing.T) {
		got := mapJobsSnapshot(daemonclient.JobsResponse{})
		if got != nil {
			t.Errorf("want nil, got %+v", got)
		}
	})
}

func TestPipelineForJobKind(t *testing.T) {
	cases := map[string]string{
		"extraction": "epi→ent",
		"ingest":     "ingest",
		"rebuild":    "rebuild",
		"":           "job",
		"weird":      "weird",
	}
	for kind, want := range cases {
		if got := pipelineForJobKind(kind); got != want {
			t.Errorf("pipelineForJobKind(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestMapSourcesSnapshot(t *testing.T) {
	t.Run("watermark maps to a row with parsed advance time and classified status", func(t *testing.T) {
		adv := "2026-06-24T12:30:00.123456789Z"
		now := time.Date(2026, 6, 24, 13, 30, 0, 0, time.UTC) // ~1h after adv → live
		snap := daemonclient.SourcesResponse{
			Watermarks: []daemonclient.SourceWatermark{
				{Connector: "local-git", Stream: "main", Sequence: 1842, AdvanceTime: adv},
			},
		}
		got := mapSourcesSnapshot(snap, now)
		if len(got) != 1 {
			t.Fatalf("want 1 row, got %d: %+v", len(got), got)
		}
		row := got[0]
		if row.Connector != "local-git" || row.Stream != "main" || row.Sequence != 1842 {
			t.Errorf("unexpected row identity: %+v", row)
		}
		want, _ := time.Parse(time.RFC3339Nano, adv)
		if !row.AdvanceTime.Equal(want) {
			t.Errorf("AdvanceTime = %v, want %v", row.AdvanceTime, want)
		}
		// Status is classified by ingest.ClassifyWatermark (shared with source
		// list / doctor), not re-derived by the pane.
		if row.Status != watch.SourceLive {
			t.Errorf("Status = %v, want SourceLive (advanced ~1h ago)", row.Status)
		}
	})

	t.Run("seven-day-old watermark classifies as stale not down", func(t *testing.T) {
		// Pins the boundary that previously diverged: ClassifyWatermark treats
		// exactly 7 days as stale (age <= 7d), so the pane must agree rather
		// than re-deriving "down" at >= 7d.
		adv := "2026-06-01T00:00:00Z"
		now := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC) // exactly 7 days later
		snap := daemonclient.SourcesResponse{
			Watermarks: []daemonclient.SourceWatermark{
				{Connector: "github", Stream: "commits", Sequence: 7, AdvanceTime: adv},
			},
		}
		got := mapSourcesSnapshot(snap, now)
		if len(got) != 1 || got[0].Status != watch.SourceStale {
			t.Fatalf("want 1 stale row, got %+v", got)
		}
	})

	t.Run("empty advance time yields the zero instant", func(t *testing.T) {
		snap := daemonclient.SourcesResponse{
			Watermarks: []daemonclient.SourceWatermark{
				{Connector: "slack", Stream: "general", Sequence: 0},
			},
		}
		got := mapSourcesSnapshot(snap, time.Now())
		if len(got) != 1 || !got[0].AdvanceTime.IsZero() {
			t.Fatalf("want one row with zero AdvanceTime, got %+v", got)
		}
	})

	t.Run("unparseable advance time degrades to the zero instant not an error", func(t *testing.T) {
		snap := daemonclient.SourcesResponse{
			Watermarks: []daemonclient.SourceWatermark{
				{Connector: "github", Stream: "commits", Sequence: 5, AdvanceTime: "not-a-time"},
			},
		}
		got := mapSourcesSnapshot(snap, time.Now())
		if len(got) != 1 || !got[0].AdvanceTime.IsZero() {
			t.Fatalf("want one row with zero AdvanceTime, got %+v", got)
		}
	})

	t.Run("registered connectors without a watermark become not-connected rows", func(t *testing.T) {
		snap := daemonclient.SourcesResponse{
			Connectors: []string{"local-git", "notion", "slack"},
			Watermarks: []daemonclient.SourceWatermark{
				{Connector: "local-git", Stream: "main", Sequence: 1, AdvanceTime: "2026-06-24T12:00:00Z"},
			},
		}
		now := time.Date(2026, 6, 24, 13, 0, 0, 0, time.UTC) // ~1h after adv → live
		got := mapSourcesSnapshot(snap, now)
		// 1 watermark row + 2 not-connected (notion, slack); local-git is not duplicated.
		if len(got) != 3 {
			t.Fatalf("want 3 rows, got %d: %+v", len(got), got)
		}
		if got[0].Connector != "local-git" || got[0].Status != watch.SourceLive {
			t.Errorf("watermark row should precede not-connected rows: %+v", got[0])
		}
		for _, row := range got[1:] {
			if row.Status != watch.SourceNotConnected {
				t.Errorf("row %q: Status = %v, want SourceNotConnected", row.Connector, row.Status)
			}
			if row.Connector == "local-git" {
				t.Error("local-git has a watermark and must not also appear as not-connected")
			}
		}
	})

	t.Run("empty snapshot maps to nil so the pane shows no sources", func(t *testing.T) {
		if got := mapSourcesSnapshot(daemonclient.SourcesResponse{}, time.Now()); got != nil {
			t.Errorf("want nil, got %+v", got)
		}
	})
}
