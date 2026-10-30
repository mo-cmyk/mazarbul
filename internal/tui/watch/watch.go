// Package watch is the maz watch live observability dashboard.
//
// Layout (single alt-screen, no modal forms):
//
//	┌─ maz watch ──────────────────── data-dir ──────┐
//	│ [1] events  35/s    │ [3] embedding queue      │
//	│ …                   │ depth 412  enq 18/s      │
//	├─ [2] extraction ────┼─ [4] sync ───────────────┤
//	│ ext-01HZ… 85%       │ origin/main ↑12          │
//	│ …                   │ last push 16:30:12       │
//	├─ [5] sources ───────┼─ (reserved) ─────────────┤
//	│ local-git  main … ● │                          │
//	│ github     commits▲ │                          │
//	├─ ? help · q quit · 1-5 focus · / search ───────┤
//	└─ providers: anthropic ✓  ollama ✓ ─────────────┘
package watch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// pollInterval controls how often we refresh jobs/embedding/sync panes.
const pollInterval = 2 * time.Second

// ── message types ─────────────────────────────────────────────────────────────

type (
	eventMsg Event
	pollMsg  time.Time
)

// ── Model ─────────────────────────────────────────────────────────────────────

// Model is the root Bubble Tea model for maz watch.
type Model struct {
	// configuration
	dataDir  string
	streamer EventStreamer
	keys     KeyMap

	// layout
	width  int
	height int

	// panes (indices 0-4 map to display numbers 1-5)
	events    *EventsPane
	extract   *ExtractionPane
	embedding *EmbeddingPane
	sync      *SyncPane
	sources   *SourcesPane
	focus     int // 0-4

	// status
	sp     spinner.Model
	paused bool
	help   bool   // help overlay visible
	search string // active search string (empty = none)

	// providers (populated once; purely cosmetic)
	providers string

	// internal
	ctx    context.Context
	cancel context.CancelFunc
	evCh   <-chan Event
}

// New returns an initialised watch Model. Call tea.NewProgram(m) to run it.
func New(ctx context.Context, dataDir string, streamer EventStreamer, providers string) Model {
	child, cancel := context.WithCancel(ctx)
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(style.Accent())
	return Model{
		dataDir:   dataDir,
		streamer:  streamer,
		keys:      DefaultKeyMap(),
		events:    newEventsPane(),
		extract:   newExtractionPane(),
		embedding: newEmbeddingPane(),
		sync:      newSyncPane(),
		sources:   newSourcesPane(),
		focus:     0,
		sp:        sp,
		providers: providers,
		ctx:       child,
		cancel:    cancel,
	}
}

// Init starts the SSE event subscription and the poll tick.
func (m Model) Init() tea.Cmd {
	evCh := m.streamer.Events(m.ctx)
	// We can't store the channel in Init (model is by value), so we use a
	// command that listens and converts to tea messages.
	return tea.Batch(
		listenCmd(evCh),
		pollCmd(),
		m.sp.Tick,
	)
}

// listenCmd returns a tea.Cmd that reads one event from ch and then requeues.
func listenCmd(ch <-chan Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return nil
		}
		return eventMsg(e)
	}
}

// pollCmd returns a tea.Cmd that fires after pollInterval.
func pollCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return pollMsg(t) })
}

// Update is the pure state-transition function.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case eventMsg:
		e := Event(msg)
		m.events.AddEvent(e)
		// Re-subscribe after receiving one event.
		return m, listenCmd(m.evCh)

	case pollMsg:
		m.extract.jobs = m.streamer.Jobs(m.ctx)
		m.embedding.stats = m.streamer.Embedding(m.ctx)
		m.sync.status = m.streamer.Sync(m.ctx)
		m.sources.rows = m.streamer.Sources(m.ctx)
		m.sources.now = time.Time(msg)
		return m, pollCmd()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd
	}

	return m, nil
}

// handleKey dispatches key events.
//
//nolint:gocognit,gocyclo // key dispatch table; splitting would obscure the intent without reducing real complexity
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Quit always wins regardless of overlay state.
	if key.Matches(msg, m.keys.Quit) {
		m.cancel()
		return m, tea.Quit
	}

	// Help toggle.
	if key.Matches(msg, m.keys.Help) {
		m.help = !m.help
		return m, nil
	}

	// Dismiss overlays with Esc.
	if msg.String() == "esc" {
		m.help = false
		m.search = ""
		return m, nil
	}

	// Pane focus.
	switch {
	case key.Matches(msg, m.keys.FocusPane1):
		m.focus = 0
		return m, nil
	case key.Matches(msg, m.keys.FocusPane2):
		m.focus = 1
		return m, nil
	case key.Matches(msg, m.keys.FocusPane3):
		m.focus = 2
		return m, nil
	case key.Matches(msg, m.keys.FocusPane4):
		m.focus = 3
		return m, nil
	case key.Matches(msg, m.keys.FocusPane5):
		m.focus = 4
		return m, nil
	}

	// Pause / resume.
	if key.Matches(msg, m.keys.Pause) {
		m.paused = !m.paused
		m.events.paused = m.paused
		return m, nil
	}

	// Follow latest.
	if key.Matches(msg, m.keys.Follow) {
		m.events.follow = !m.events.follow
		if m.events.follow {
			m.events.offset = 0
		}
		return m, nil
	}

	// Reload.
	if key.Matches(msg, m.keys.Reload) {
		m.extract.jobs = m.streamer.Jobs(m.ctx)
		m.embedding.stats = m.streamer.Embedding(m.ctx)
		m.sync.status = m.streamer.Sync(m.ctx)
		m.sources.rows = m.streamer.Sources(m.ctx)
		return m, nil
	}

	// Scroll (events pane when focused).
	if m.focus == 0 {
		switch {
		case key.Matches(msg, m.keys.ScrollUp):
			m.events.follow = false
			m.events.offset++
			return m, nil
		case key.Matches(msg, m.keys.ScrollDown):
			if m.events.offset > 0 {
				m.events.offset--
			}
			return m, nil
		}
	}

	// Cursor on extraction pane.
	if m.focus == 1 {
		switch {
		case key.Matches(msg, m.keys.ScrollUp):
			if m.extract.cursor > 0 {
				m.extract.cursor--
			}
		case key.Matches(msg, m.keys.ScrollDown):
			if m.extract.cursor < len(m.extract.jobs)-1 {
				m.extract.cursor++
			}
		}
		return m, nil
	}

	// Event-kind filter chip.
	if key.Matches(msg, m.keys.EventFilter) {
		m.events.kindMask = nextKindMask(m.events.kindMask)
		return m, nil
	}

	// Search (primitive: set filter on events pane).
	if key.Matches(msg, m.keys.Search) {
		// Toggle search mode: for now we toggle with the next keystroke.
		// A full huh.Input embed is deferred — the search filter simply
		// cycles the text filter off/on with a static label.
		m.events.filter = cycleSearch(m.events.filter)
		return m, nil
	}

	return m, nil
}

func nextKindMask(cur EventKind) EventKind {
	kinds := []EventKind{"", EventKindExtraction, EventKindEmbedding, EventKindSync, EventKindSystem}
	for i, k := range kinds {
		if k == cur {
			return kinds[(i+1)%len(kinds)]
		}
	}
	return ""
}

func cycleSearch(cur string) string {
	// Stub: real implementation would open an inline textinput.
	// For now, toggling applies/clears a demo filter string.
	if cur == "" {
		return "episode"
	}
	return ""
}

// buildView renders the complete screen as a string.
// The layout is a 3-row × 2-col grid:
//
//	row 0: [1] events       | [3] embedding
//	row 1: [2] extraction   | [4] sync
//	row 2: [5] sources      | (reserved)
func (m Model) buildView() string {
	if m.width == 0 {
		return "loading…"
	}

	if m.help {
		return m.helpOverlay()
	}

	// Divide the available height into three equal rows.
	// Subtract 3 to leave room for the titleBar border, statusBar border, and footer.
	totalH := m.height
	content := totalH - 3
	rowH := content / 3
	if rowH < 4 {
		rowH = 4
	}
	halfW := m.width / 2
	rightW := m.width - halfW

	// Row 0: events | embedding
	evPane := m.events.Render(halfW, rowH, m.focus == 0)
	embPane := m.embedding.Render(rightW, rowH, m.focus == 2)
	// Row 1: extraction | sync
	extPane := m.extract.Render(halfW, rowH, m.focus == 1)
	syncPane := m.sync.Render(rightW, rowH, m.focus == 3)
	// Row 2: sources | reserved placeholder
	srcPane := m.sources.Render(halfW, rowH, m.focus == 4)
	rsvPane := reservedPane(rightW, rowH) // reserved

	top := lipgloss.JoinHorizontal(lipgloss.Top, evPane, embPane)
	mid := lipgloss.JoinHorizontal(lipgloss.Top, extPane, syncPane)
	bot := lipgloss.JoinHorizontal(lipgloss.Top, srcPane, rsvPane)

	return lipgloss.JoinVertical(lipgloss.Left,
		m.titleBar(),
		top,
		mid,
		bot,
		m.statusBar(),
		m.footer(),
	)
}

// View satisfies the tea.Model interface and declares the alt-screen.
func (m Model) View() tea.View {
	v := tea.NewView(m.buildView())
	v.AltScreen = true
	return v
}

func (m Model) titleBar() string {
	left := lipgloss.NewStyle().Foreground(style.Accent()).Bold(true).Render("maz watch")
	right := style.Faint().Render(m.dataDir)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 4
	if gap < 1 {
		gap = 1
	}
	bar := left + strings.Repeat(" ", gap) + right
	return lipgloss.NewStyle().
		Width(m.width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(style.FaintColor()).
		Render(bar)
}

func (m Model) statusBar() string {
	hint := style.Faint().Render(m.keys.ShortHelp())
	if m.paused {
		hint = lipgloss.NewStyle().Foreground(style.WarningColor()).Render("paused · ") + hint
	}
	return lipgloss.NewStyle().
		Width(m.width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderTop(true).
		BorderForeground(style.FaintColor()).
		Render(hint)
}

func (m Model) footer() string {
	prefix := style.Faint().Render("providers: ")
	provStr := m.providers
	if provStr == "" {
		provStr = style.Faint().Render("(none)")
	}
	return lipgloss.NewStyle().
		Width(m.width).
		Render(prefix + provStr)
}

func (m Model) helpOverlay() string {
	var b strings.Builder
	b.WriteString(style.Brand().Render("maz watch — keyboard reference") + "\n\n")
	for _, group := range m.keys.FullHelp() {
		for _, binding := range group {
			keys := strings.Join(binding.Keys(), ", ")
			b.WriteString(fmt.Sprintf("  %-20s %s\n",
				lipgloss.NewStyle().Foreground(style.Accent()).Render(keys),
				style.Subtle().Render(binding.Help().Desc),
			))
		}
		b.WriteString("\n")
	}
	// Sources-pane glyph legend: the [5] sources markers are otherwise unlabelled.
	b.WriteString(style.Subtle().Render("[5] sources freshness") + "\n")
	for _, lg := range []struct {
		st    SourceStatus
		label string
	}{
		{SourceLive, "live (advanced within a day)"},
		{SourceStale, "stale (over a day)"},
		{SourceDown, "down (over a week, or never)"},
	} {
		glyph, gs := sourceGlyph(lg.st)
		b.WriteString(fmt.Sprintf("  %-20s %s\n", gs.Render(glyph), style.Subtle().Render(lg.label)))
	}
	b.WriteString("\n")
	b.WriteString(style.Faint().Render("press ? or esc to close"))
	return lipgloss.NewStyle().
		Width(m.width).
		Height(m.height).
		Render(b.String())
}

// Run launches the watch TUI against the given streamer and blocks until quit.
func Run(ctx context.Context, dataDir string, streamer EventStreamer, providers string) error {
	m := New(ctx, dataDir, streamer, providers)
	p := tea.NewProgram(m,
		tea.WithContext(ctx),
	)
	_, err := p.Run()
	return err
}
