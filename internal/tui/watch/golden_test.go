package watch

// golden_test.go — golden snapshot tests and tuitest-driven interaction tests
// for the watch TUI.  All tests use deterministic state (fixed times, seeded
// fakes) so the golden files are stable across runs.
//
// Regenerate goldens:
//
//	go test -tags=sqlite_fts5 -run TestGolden ./internal/tui/watch/ -update

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/mo-cmyk/mazarbul/internal/tui/tuitest"
)

// goldenTestModel returns a watch.Model with all time-sensitive fields pinned
// to baseTime so the golden snapshots are stable across test runs.
func goldenTestModel() Model {
	const baseISO = "2026-06-03T16:42:00Z"
	base, _ := time.Parse(time.RFC3339, baseISO)

	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel

	m := New(ctx, "/tmp/test-data", &FakeEventStreamer{Seed: 1}, "anthropic ✓  ollama ✓")
	m.width = 80
	m.height = 24

	m.extract.jobs = (&FakeEventStreamer{}).Jobs(ctx)
	m.embedding.stats = (&FakeEventStreamer{}).Embedding(ctx)

	// Pin sync status so "last push HH:MM:SS" is stable.
	m.sync.status = SyncStatus{
		Remote:   "origin/main",
		Ahead:    12,
		Behind:   0,
		LastPush: base.Add(-13 * time.Minute),
	}

	// Pin sources pane so relative-age labels are stable.
	m.sources.now = base
	m.sources.rows = []SourceRow{
		{
			Connector: "local-git", Stream: "main", Sequence: 1842,
			AdvanceTime: base.Add(-47 * time.Minute), Status: SourceLive,
		},
		{
			Connector: "github", Stream: "commits", Sequence: 304,
			AdvanceTime: base.Add(-30 * time.Hour), Status: SourceStale,
		},
		{
			Connector: "slack", Stream: "general", Sequence: 0,
			AdvanceTime: time.Time{}, Status: SourceDown,
		}, // never advanced
		{Connector: "notion", Status: SourceNotConnected},
	}

	// Seed two deterministic events with fixed timestamps.
	for _, e := range []Event{
		{
			Kind: EventKindExtraction, TS: base.Add(time.Second),
			Payload: map[string]any{"msg": "episode→entity", "size": "0.3K"},
		},
		{
			Kind: EventKindEmbedding, TS: base.Add(time.Second),
			Payload: map[string]any{"msg": "chunk queued", "size": "0.1K"},
		},
	} {
		m.events.AddEvent(e)
	}
	return m
}

// ── golden snapshots ──────────────────────────────────────────────────────────

// TestGolden_WatchDefaultLayout captures the 2×3 grid with all five panes and
// focus on pane 1 (events).  Run with -update to regenerate.
func TestGolden_WatchDefaultLayout(t *testing.T) {
	restore := tuitest.LockRender(t)
	defer restore()

	view := tuitest.StripANSI(goldenTestModel().View().Content)
	tuitest.CompareGolden(t, "testdata/watch_default.golden.txt", view)
}

// TestGolden_WatchSourcesFocused captures the 2×3 grid with the sources pane
// (pane [5]) focused — its rounded border and header are visually highlighted.
func TestGolden_WatchSourcesFocused(t *testing.T) {
	restore := tuitest.LockRender(t)
	defer restore()

	m := goldenTestModel()
	m.focus = 4 // 0-indexed: 4 → display [5] sources

	view := tuitest.StripANSI(m.View().Content)
	tuitest.CompareGolden(t, "testdata/watch_sources_focused.golden.txt", view)
}

// TestGolden_WatchLegacyPanes proves that each of the four legacy panes (1–4)
// renders correctly after the 2×3 grid refactor.  One file per pane so diffs
// are isolated.
func TestGolden_WatchLegacyPanes(t *testing.T) {
	cases := []struct {
		focus int
		slug  string
	}{
		{0, "pane1_events"},
		{1, "pane2_extraction"},
		{2, "pane3_embedding"},
		{3, "pane4_sync"},
	}
	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			restore := tuitest.LockRender(t)
			defer restore()

			m := goldenTestModel()
			m.focus = tc.focus
			view := tuitest.StripANSI(m.View().Content)
			tuitest.CompareGolden(t, "testdata/watch_"+tc.slug+".golden.txt", view)
		})
	}
}

// ── interaction tests ─────────────────────────────────────────────────────────

// TestInteraction_WatchFocusSources drives key "5" and asserts that the
// sources pane becomes focused and connector rows are visible in the view.
func TestInteraction_WatchFocusSources(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	m2, view := tuitest.DriveKeys(t, goldenTestModel(),
		tea.KeyPressMsg{Code: '5', Text: "5"},
	)

	if m2.focus != 4 {
		t.Errorf("focus = %d after pressing 5, want 4 (sources)", m2.focus)
	}
	plain := tuitest.StripANSI(view)
	if !strings.Contains(plain, "[5] sources") {
		t.Errorf("view missing [5] sources header after key 5:\n%s", plain)
	}
	for _, want := range []string{"local-git", "github", "notion"} {
		if !strings.Contains(plain, want) {
			t.Errorf("view missing connector %q in sources pane:\n%s", want, plain)
		}
	}
}

// TestInteraction_WatchHelpThenEsc drives ? to open the help overlay and Esc
// to close it, asserting on the view at each step.
func TestInteraction_WatchHelpThenEsc(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := goldenTestModel()

	// Press ? → overlay opens.
	m2, helpView := tuitest.DriveKeys(t, m,
		tea.KeyPressMsg{Code: '?', Text: "?"},
	)
	if !m2.help {
		t.Fatal("help should be visible after ?")
	}
	if !strings.Contains(helpView, "keyboard reference") {
		t.Errorf("help view missing 'keyboard reference':\n%s", helpView)
	}

	// Press Esc → overlay dismissed.
	_, postView := tuitest.DriveKeys(t, m2,
		tea.KeyPressMsg{Code: tea.KeyEscape},
	)
	if strings.Contains(postView, "keyboard reference") {
		t.Errorf("keyboard reference should not appear after Esc:\n%s", postView)
	}
}
