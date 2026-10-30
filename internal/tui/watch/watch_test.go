package watch

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func init() {
	// Force monochrome for deterministic snapshots regardless of host
	// terminal. Individual tests that want colour can unset it locally.
	_ = os.Setenv("NO_COLOR", "1")
}

// testModel returns a Model pre-populated with fake data at 80×24.
func testModel() Model {
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel // cancelled by Quit in tests
	m := New(ctx, "/tmp/test-data", &FakeEventStreamer{Seed: 1}, "anthropic ✓  ollama ✓")
	m.width = 80
	m.height = 24
	// Pre-populate panes with deterministic data so snapshots are stable.
	m.extract.jobs = (&FakeEventStreamer{}).Jobs(ctx)
	m.embedding.stats = (&FakeEventStreamer{}).Embedding(ctx)
	m.sync.status = (&FakeEventStreamer{}).Sync(ctx)
	// Add a few synthetic events.
	for _, e := range []Event{
		{
			Kind: EventKindExtraction, TS: time.Date(2026, 6, 3, 16, 42, 1, 0, time.UTC),
			Payload: map[string]any{"msg": "episode→entity", "size": "0.3K"},
		},
		{
			Kind: EventKindEmbedding, TS: time.Date(2026, 6, 3, 16, 42, 1, 0, time.UTC),
			Payload: map[string]any{"msg": "chunk queued", "size": "0.1K"},
		},
	} {
		m.events.AddEvent(e)
	}
	return m
}

// ── snapshot: default (NO_COLOR) ─────────────────────────────────────────────

func TestSnapshot_NoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()
	view := m.View().Content
	if !strings.Contains(view, "maz watch") {
		t.Errorf("View() missing brand mark\n%s", view)
	}
	if !strings.Contains(view, "[1] events") {
		t.Errorf("View() missing events pane header\n%s", view)
	}
	if !strings.Contains(view, "[2] extraction jobs") {
		t.Errorf("View() missing extraction pane header\n%s", view)
	}
	if !strings.Contains(view, "[3] embedding queue") {
		t.Errorf("View() missing embedding pane header\n%s", view)
	}
	if !strings.Contains(view, "[4] sync") {
		t.Errorf("View() missing sync pane header\n%s", view)
	}
}

// ── snapshot: help overlay ────────────────────────────────────────────────────

func TestSnapshot_HelpOverlay(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()
	m.help = true
	view := m.View().Content
	if !strings.Contains(view, "keyboard reference") {
		t.Errorf("help overlay missing; got:\n%s", view)
	}
	if !strings.Contains(view, "quit") {
		t.Errorf("help overlay missing quit binding; got:\n%s", view)
	}
}

// ── snapshot: events pane rendering ──────────────────────────────────────────

func TestEventsPane_Render(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	ctx := context.Background()
	p := newEventsPane()
	p.AddEvent(Event{
		Kind:    EventKindExtraction,
		TS:      time.Date(2026, 6, 3, 16, 42, 1, 0, time.UTC),
		Payload: map[string]any{"msg": "episode→entity", "size": "0.3K"},
	})
	out := p.Render(40, 10, true)
	if !strings.Contains(out, "[1] events") {
		t.Errorf("missing pane header: %q", out)
	}
	if !strings.Contains(out, "16:42:01") {
		t.Errorf("missing timestamp: %q", out)
	}
	_ = ctx
}

// ── snapshot: extraction pane rendering ──────────────────────────────────────

func TestExtractionPane_Render(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	ctx := context.Background()
	p := newExtractionPane()
	p.jobs = (&FakeEventStreamer{}).Jobs(ctx)
	out := p.Render(40, 10, false)
	if !strings.Contains(out, "[2] extraction jobs") {
		t.Errorf("missing pane header: %q", out)
	}
	if !strings.Contains(out, "epi→ent") {
		t.Errorf("missing pipeline label: %q", out)
	}
}

// ── snapshot: embedding pane rendering ───────────────────────────────────────

func TestEmbeddingPane_Render(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	ctx := context.Background()
	p := newEmbeddingPane()
	p.stats = (&FakeEventStreamer{}).Embedding(ctx)
	out := p.Render(40, 10, false)
	if !strings.Contains(out, "[3] embedding queue") {
		t.Errorf("missing pane header: %q", out)
	}
	if !strings.Contains(out, "412") {
		t.Errorf("missing depth: %q", out)
	}
}

// ── snapshot: sync pane rendering ────────────────────────────────────────────

func TestSyncPane_Render(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	ctx := context.Background()
	p := newSyncPane()
	p.status = (&FakeEventStreamer{}).Sync(ctx)
	out := p.Render(40, 10, false)
	if !strings.Contains(out, "[4] sync") {
		t.Errorf("missing pane header: %q", out)
	}
	if !strings.Contains(out, "origin/main") {
		t.Errorf("missing remote: %q", out)
	}
}

// ── keyboard: q exits ────────────────────────────────────────────────────────

func TestKey_QExits(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("expected a quit cmd, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected QuitMsg, got %T", msg)
	}
}

// ── keyboard: Ctrl+C exits ────────────────────────────────────────────────────

func TestKey_CtrlCExits(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("expected a quit cmd, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected QuitMsg, got %T", msg)
	}
}

// ── keyboard: ? toggles help ──────────────────────────────────────────────────

func TestKey_HelpToggle(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()

	// Help starts hidden.
	if m.help {
		t.Fatal("help should start hidden")
	}

	// Press ?.
	updated, _ := m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	m2 := updated.(Model)
	if !m2.help {
		t.Fatal("help should be visible after ?")
	}

	// Press ? again.
	updated2, _ := m2.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	m3 := updated2.(Model)
	if m3.help {
		t.Fatal("help should be hidden after second ?")
	}
}

// ── keyboard: 1→2 changes focus ──────────────────────────────────────────────

func TestKey_PaneFocus(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()

	// Default focus is pane 1 (index 0).
	if m.focus != 0 {
		t.Fatalf("initial focus = %d, want 0", m.focus)
	}

	// Press 2.
	updated, _ := m.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	m2 := updated.(Model)
	if m2.focus != 1 {
		t.Fatalf("focus after '2' = %d, want 1", m2.focus)
	}

	// Press 4.
	updated2, _ := m2.Update(tea.KeyPressMsg{Code: '4', Text: "4"})
	m3 := updated2.(Model)
	if m3.focus != 3 {
		t.Fatalf("focus after '4' = %d, want 3", m3.focus)
	}
}

// ── keyboard: p pauses/resumes events ────────────────────────────────────────

func TestKey_PauseResume(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	m2 := updated.(Model)
	if !m2.paused {
		t.Fatal("should be paused after p")
	}

	updated2, _ := m2.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	m3 := updated2.(Model)
	if m3.paused {
		t.Fatal("should be resumed after second p")
	}
}

// ── keyboard: r reloads panes ─────────────────────────────────────────────────

func TestKey_Reload(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()
	m.extract.jobs = nil // clear
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	m2 := updated.(Model)
	if len(m2.extract.jobs) == 0 {
		t.Fatal("reload should repopulate jobs from streamer")
	}
}

// ── model: full render contains all pane headers ──────────────────────────────

func TestProgram_RendersTitleAndPanes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()
	view := m.View().Content

	for _, want := range []string{
		"maz watch",
		"[1] events",
		"[2] extraction",
		"[3] embedding",
		"[4] sync",
		"[5] sources",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View() missing %q", want)
		}
	}
}

// ── sources pane: row rendering ───────────────────────────────────────────────

func TestSourcesPane_RendersAllRowKinds(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	p := newSourcesPane()
	base := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	p.now = base
	p.rows = []SourceRow{
		{
			Connector:   "local-git",
			Stream:      "main",
			Sequence:    1842,
			AdvanceTime: base.Add(-47 * time.Minute), // live: < 24h
			Status:      SourceLive,
		},
		{
			Connector:   "github",
			Stream:      "commits",
			Sequence:    304,
			AdvanceTime: base.Add(-30 * time.Hour), // stale: > 24h
			Status:      SourceStale,
		},
		{
			Connector:   "slack",
			Stream:      "general",
			Sequence:    0,
			AdvanceTime: time.Time{}, // down: never advanced
			Status:      SourceDown,
		},
		{
			Connector: "notion",
			Status:    SourceNotConnected,
		},
	}
	out := p.Render(40, 12, false)

	if !strings.Contains(out, "[5] sources") {
		t.Errorf("missing pane header: %q", out)
	}
	// Live row: local-git present with live glyph ●
	if !strings.Contains(out, "local-git") {
		t.Errorf("missing live connector name: %q", out)
	}
	if !strings.Contains(out, "●") {
		t.Errorf("missing live glyph ●: %q", out)
	}
	// Stale row: github present with stale glyph ▲
	if !strings.Contains(out, "github") {
		t.Errorf("missing stale connector name: %q", out)
	}
	if !strings.Contains(out, "▲") {
		t.Errorf("missing stale glyph ▲: %q", out)
	}
	// Down row: slack present with down glyph ✕
	if !strings.Contains(out, "slack") {
		t.Errorf("missing down connector name: %q", out)
	}
	if !strings.Contains(out, "✕") {
		t.Errorf("missing down glyph ✕: %q", out)
	}
	// Not-connected row: notion shown with "not connected" label
	if !strings.Contains(out, "not connected") {
		t.Errorf("missing not-connected label: %q", out)
	}
	if !strings.Contains(out, "notion") {
		t.Errorf("missing not-connected connector name: %q", out)
	}
}

func TestSourcesPane_EmptyRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	p := newSourcesPane()
	out := p.Render(40, 10, false)
	if !strings.Contains(out, "[5] sources") {
		t.Errorf("missing pane header with no rows: %q", out)
	}
	if !strings.Contains(out, "no sources connected") {
		t.Errorf("missing empty-state label: %q", out)
	}
}

// Freshness classification (live/stale/down boundaries) is owned by
// ingest.ClassifyWatermark (see ingest.TestClassifyWatermark) and applied in
// cli.mapSourcesSnapshot (see cli.TestMapSourcesSnapshot); the pane renders
// the resulting SourceRow.Status without re-deriving it.

// ── keyboard: 5 focuses sources pane ─────────────────────────────────────────

func TestKey_Focus5SourcesPane(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()

	// Default focus is 0.
	if m.focus != 0 {
		t.Fatalf("initial focus = %d, want 0", m.focus)
	}

	// Press 5.
	updated, _ := m.Update(tea.KeyPressMsg{Code: '5', Text: "5"})
	m2 := updated.(Model)
	if m2.focus != 4 {
		t.Fatalf("focus after '5' = %d, want 4", m2.focus)
	}

	// View must highlight the sources pane header.
	view := m2.View().Content
	if !strings.Contains(view, "[5] sources") {
		t.Errorf("view missing [5] sources after focusing: %q", view)
	}
}

// ── keyboard: legacy focus keys unaffected by 2×3 refactor ───────────────────

func TestKey_LegacyPaneFocusUnchanged(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := testModel()

	steps := []struct {
		key       rune
		wantFocus int
		wantPane  string
	}{
		{'1', 0, "[1] events"},
		{'2', 1, "[2] extraction"},
		{'3', 2, "[3] embedding"},
		{'4', 3, "[4] sync"},
		{'5', 4, "[5] sources"},
	}
	for _, s := range steps {
		t.Run("press "+string(s.key), func(t *testing.T) {
			updated, _ := m.Update(tea.KeyPressMsg{Code: s.key, Text: string(s.key)})
			m2 := updated.(Model)
			if m2.focus != s.wantFocus {
				t.Errorf("focus = %d, want %d", m2.focus, s.wantFocus)
			}
			view := m2.View().Content
			if !strings.Contains(view, s.wantPane) {
				t.Errorf("view missing %q: %s", s.wantPane, view)
			}
		})
	}
}

// ── sources pane: FakeEventStreamer coverage ──────────────────────────────────

func TestFakeEventStreamer_Sources(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	ctx := context.Background()
	rows := (&FakeEventStreamer{}).Sources(ctx)
	if len(rows) != 4 {
		t.Fatalf("want 4 rows, got %d", len(rows))
	}
	// Last row must be not-connected.
	if rows[3].Status != SourceNotConnected {
		t.Errorf("last row status = %d, want SourceNotConnected", rows[3].Status)
	}
	// First three rows are connected (Status == SourceLive default).
	for i, r := range rows[:3] {
		if r.Status == SourceNotConnected {
			t.Errorf("row %d unexpectedly pre-flagged as not-connected", i)
		}
		if r.Connector == "" {
			t.Errorf("row %d has empty connector", i)
		}
	}
}
