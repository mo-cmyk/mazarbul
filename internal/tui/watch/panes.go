package watch

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// Pane is the interface every watch sub-pane satisfies.
// Each pane is purely reactive: Tick advances its state from a Msg,
// and Render produces a bounded string at the given dimensions.
type Pane interface {
	Tick(tea.Msg) tea.Cmd
	Render(width, height int, focused bool) string
}

// ── helper ────────────────────────────────────────────────────────────────────

func paneTitle(index int, label string, focused bool) string {
	raw := fmt.Sprintf("[%d] %s", index, label)
	if focused {
		return lipgloss.NewStyle().Foreground(style.Accent()).Bold(true).Render(raw)
	}
	return style.Faint().Render(raw)
}

func paneBox(content string, width, height int, focused bool) string {
	borderColor := style.FaintColor()
	if focused {
		borderColor = style.Accent()
	}
	inner := lipgloss.NewStyle().
		Width(width - 2).
		Height(height - 2).
		Render(content)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Render(inner)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ── EventsPane ───────────────────────────────────────────────────────────────

const maxEventLines = 200

// EventsPane tails incoming events; supports search filter and pause.
type EventsPane struct {
	lines    []string  // formatted lines, newest last
	filter   string    // text filter (empty = all)
	kindMask EventKind // "" = all kinds
	paused   bool
	follow   bool
	offset   int     // scroll offset from bottom (0 = follow)
	rate     float64 // events/sec approximation
	lastSec  time.Time
	countSec int
}

// newEventsPane returns a ready EventsPane with follow enabled.
func newEventsPane() *EventsPane {
	return &EventsPane{follow: true, lastSec: time.Now()}
}

// AddEvent appends one Event to the pane. Called by the root model on
// an eventMsg.
func (p *EventsPane) AddEvent(e Event) {
	if p.paused {
		return
	}
	msg, _ := e.Payload["msg"].(string)   //nolint:errcheck // type assertion; zero value is the correct fallback
	size, _ := e.Payload["size"].(string) //nolint:errcheck // type assertion; zero value is the correct fallback
	ts := e.TS.Format("15:04:05")
	kindTag := lipgloss.NewStyle().Foreground(style.InfoColor()).Render(string(e.Kind))
	line := fmt.Sprintf("%s %s %s %s",
		style.Faint().Render(ts),
		kindTag,
		style.Subtle().Render(msg),
		style.Faint().Render(size),
	)
	p.lines = append(p.lines, line)
	if len(p.lines) > maxEventLines {
		p.lines = p.lines[len(p.lines)-maxEventLines:]
	}

	// rate estimate
	p.countSec++
	if time.Since(p.lastSec) >= time.Second {
		p.rate = float64(p.countSec) / time.Since(p.lastSec).Seconds()
		p.countSec = 0
		p.lastSec = time.Now()
	}
}

// Tick satisfies Pane. EventsPane has no timer of its own; events arrive
// via root model → AddEvent.
func (p *EventsPane) Tick(_ tea.Msg) tea.Cmd { return nil }

// Render produces the events tail bounded to width × height.
func (p *EventsPane) Render(width, height int, focused bool) string {
	header := paneTitle(1, fmt.Sprintf("events  %.0f/s", p.rate), focused)
	inner := height - 4 // border (2) + header (1) + padding (1)
	if inner < 1 {
		inner = 1
	}

	visible := p.visibleLines(inner)
	var b strings.Builder
	b.WriteString(header + "\n")
	for _, l := range visible {
		b.WriteString(l + "\n")
	}
	content := b.String()
	return paneBox(content, width, height, focused)
}

func (p *EventsPane) visibleLines(n int) []string {
	src := p.filtered()
	if len(src) == 0 {
		return []string{style.Faint().Render("(no events)")}
	}
	if p.follow || p.offset == 0 {
		start := len(src) - n
		if start < 0 {
			start = 0
		}
		return src[start:]
	}
	end := len(src) - p.offset
	start := end - n
	if start < 0 {
		start = 0
	}
	if end < 0 {
		end = 0
	}
	return src[start:end]
}

func (p *EventsPane) filtered() []string {
	if p.filter == "" && p.kindMask == "" {
		return p.lines
	}
	out := make([]string, 0, len(p.lines))
	for _, l := range p.lines {
		if p.filter != "" && !strings.Contains(l, p.filter) {
			continue
		}
		if p.kindMask != "" && !strings.Contains(l, string(p.kindMask)) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// ── ExtractionPane ───────────────────────────────────────────────────────────

// ExtractionPane shows in-flight extraction jobs with progress bars.
type ExtractionPane struct {
	jobs   []ExtractionJob
	cursor int
}

func newExtractionPane() *ExtractionPane { return &ExtractionPane{} }

// Tick satisfies Pane; jobs are refreshed by the root model poll tick.
func (p *ExtractionPane) Tick(_ tea.Msg) tea.Cmd { return nil }

// Render produces the extraction jobs list.
func (p *ExtractionPane) Render(width, height int, focused bool) string {
	header := paneTitle(2, "extraction jobs", focused)
	// paneBox lays content into width-2 columns; pass that budget so each row
	// sizes its bar to fill — never exceed — the line and wrap onto the next,
	// borderless line.
	inner := width - 2
	var b strings.Builder
	b.WriteString(header + "\n")
	if len(p.jobs) == 0 {
		b.WriteString(style.Faint().Render("  (no active jobs)") + "\n")
	}
	for i, j := range p.jobs {
		sel := i == p.cursor && focused
		b.WriteString(renderExtractionJob(j, inner, sel) + "\n")
	}
	return paneBox(b.String(), width, height, focused)
}

// renderExtractionJob renders one job row within avail visible columns. The
// progress bar is sized to exactly fill the gap between the fixed left segment
// (prefix + id + pipeline) and the right segment (state + optional detail), so
// the state label can never be pushed past the border and wrap. A final
// ansi.Truncate guards against a left/right segment that alone exceeds avail.
func renderExtractionJob(j ExtractionJob, avail int, selected bool) string {
	prefix := "  "
	if selected {
		prefix = lipgloss.NewStyle().Foreground(style.Accent()).Render("▸ ")
	}
	idShort := j.ID
	if len(idShort) > 10 {
		idShort = idShort[:10] + "…"
	}
	// Indeterminate gauge row (negative Progress): a background drain that
	// reports an aggregate depth rather than per-job completion. Render the
	// detail text in place of a misleading 0% bar.
	if j.Progress < 0 {
		line := fmt.Sprintf("%s%s %-8s %s", prefix, idShort, j.Pipeline,
			lipgloss.NewStyle().Foreground(style.InfoColor()).Render(j.Detail))
		return ansi.Truncate(line, avail, "")
	}
	pct := int(j.Progress * 100)
	state := fmt.Sprintf("%3d%%", pct)
	if j.Done {
		state = lipgloss.NewStyle().Foreground(style.SuccessColor()).Render("done")
	}
	left := fmt.Sprintf("%s%s %-8s ", prefix, idShort, j.Pipeline)
	right := " " + state
	if j.Detail != "" {
		right += "  " + style.Faint().Render(j.Detail)
	}
	barW := avail - ansi.StringWidth(left) - ansi.StringWidth(right)
	if barW < 1 {
		barW = 1 // degenerate width; the truncate below trims any overflow
	}
	line := left + bar(j.Progress, barW) + right
	return ansi.Truncate(line, avail, "")
}

func bar(progress float64, width int) string {
	if width <= 0 {
		return ""
	}
	fill := int(float64(width) * progress)
	fill = clamp(fill, 0, width)
	full := lipgloss.NewStyle().Foreground(style.Accent()).Render(strings.Repeat("█", fill))
	empty := lipgloss.NewStyle().Foreground(style.FaintColor()).Render(strings.Repeat("░", width-fill))
	return full + empty
}

// ── EmbeddingPane ─────────────────────────────────────────────────────────────

// EmbeddingPane shows the embedder queue depth and throughput.
type EmbeddingPane struct {
	stats EmbeddingStats
}

func newEmbeddingPane() *EmbeddingPane { return &EmbeddingPane{} }

// Tick satisfies Pane.
func (p *EmbeddingPane) Tick(_ tea.Msg) tea.Cmd { return nil }

// Render produces the embedding queue snapshot.
func (p *EmbeddingPane) Render(width, height int, focused bool) string {
	header := paneTitle(3, "embedding queue", focused)
	s := p.stats
	depthLine := fmt.Sprintf("  depth %-6d enq %.0f/s", s.Depth, s.EnqRate)
	barW := width - 16
	if barW < 8 {
		barW = 8
	}
	workerFrac := 0.0
	if s.Workers > 0 {
		workerFrac = float64(s.Active) / float64(s.Workers)
	}
	workerBar := bar(workerFrac, barW) + fmt.Sprintf("  %d/%d", s.Active, s.Workers)

	var b strings.Builder
	b.WriteString(header + "\n")
	b.WriteString(style.Subtle().Render(depthLine) + "\n")
	b.WriteString("  " + workerBar + "\n")
	return paneBox(b.String(), width, height, focused)
}

// ── SyncPane ─────────────────────────────────────────────────────────────────

// SyncPane shows the sync subsystem status.
type SyncPane struct {
	status SyncStatus
}

func newSyncPane() *SyncPane { return &SyncPane{} }

// Tick satisfies Pane.
func (p *SyncPane) Tick(_ tea.Msg) tea.Cmd { return nil }

// Render produces the sync status block.
func (p *SyncPane) Render(width, height int, focused bool) string {
	header := paneTitle(4, "sync", focused)
	s := p.status

	remote := style.Subtle().Render(s.Remote)
	arrows := ""
	if s.Ahead > 0 {
		arrows += lipgloss.NewStyle().Foreground(style.SuccessColor()).Render(fmt.Sprintf("↑%d", s.Ahead))
	}
	if s.Behind > 0 {
		if arrows != "" {
			arrows += " "
		}
		arrows += lipgloss.NewStyle().Foreground(style.WarningColor()).Render(fmt.Sprintf("↓%d", s.Behind))
	}
	if arrows == "" {
		arrows = style.Faint().Render("↑0 ↓0")
	}
	lastPush := style.Faint().Render("(never)")
	if !s.LastPush.IsZero() {
		lastPush = style.Faint().Render("last push " + s.LastPush.Format("15:04:05"))
	}

	var b strings.Builder
	b.WriteString(header + "\n")
	b.WriteString("  " + remote + "  " + arrows + "\n")
	b.WriteString("  " + lastPush + "\n")
	_ = width
	return paneBox(b.String(), width, height, focused)
}

// ── SourcesPane ───────────────────────────────────────────────────────────────

// SourcesPane shows connector watermark health in the sources pane.
// Status thresholds are derived by the pane from AdvanceTime against its own
// tick time (now): live ≤24h, stale >24h, down >7d or never advanced.
// Not-connected rows arrive pre-flagged with Status=SourceNotConnected.
type SourcesPane struct {
	rows []SourceRow
	now  time.Time
}

func newSourcesPane() *SourcesPane {
	return &SourcesPane{now: time.Now()}
}

// Tick satisfies Pane; the caller updates now and rows via the pollMsg handler.
func (p *SourcesPane) Tick(_ tea.Msg) tea.Cmd { return nil }

// Render produces the sources watermark table bounded to width × height.
func (p *SourcesPane) Render(width, height int, focused bool) string {
	header := paneTitle(5, "sources", focused)
	// The trailing column is a single status glyph (●/▲/✕) — self-evident, so it
	// carries no header (an abbreviated "st" label just read as broken text).
	colHdr := style.Faint().Render("  connector     stream       seq  last advance")

	// paneBox lays content into width-2 columns. Each row is truncated to that
	// budget so the wide watermark table degrades gracefully in a narrow cell
	// (drops trailing columns) instead of wrapping onto a borderless line.
	inner := width - 2
	var b strings.Builder
	writeLine := func(s string) {
		b.WriteString(ansi.Truncate(s, inner, "") + "\n")
	}
	writeLine(header)
	writeLine(colHdr)

	var notConn []SourceRow
	hasConnected := false
	for _, r := range p.rows {
		if r.Status == SourceNotConnected {
			notConn = append(notConn, r)
			continue
		}
		hasConnected = true
		// Status is set upstream by ingest.ClassifyWatermark (live path) or the
		// fake streamer — the pane renders it, never re-derives it, so all
		// freshness surfaces agree on the boundaries.
		glyph, gs := sourceGlyph(r.Status)
		lastAdv := relativeTime(p.now, r.AdvanceTime)
		// Status glyph leads in the gutter (col 0) so it stays visible when a
		// narrow cell truncates the trailing columns; connector onward still
		// align under the header (which starts at col 2).
		writeLine(fmt.Sprintf("%s %-12s %-10s %5d  %-12s",
			gs.Render(glyph), r.Connector, r.Stream, r.Sequence, lastAdv))
	}
	if !hasConnected && len(notConn) == 0 {
		writeLine(style.Faint().Render("  (no sources connected)"))
	}
	for _, r := range notConn {
		writeLine(style.Faint().Render("  not connected: " + r.Connector))
	}
	return paneBox(b.String(), width, height, focused)
}

// sourceGlyph maps a SourceStatus to its display glyph and style.
// ● live (style.Success), ▲ stale (style.Warning), ✕ down (style.Error).
func sourceGlyph(s SourceStatus) (string, lipgloss.Style) {
	switch s {
	case SourceLive:
		return "●", style.Success()
	case SourceStale:
		return "▲", style.Warning()
	default:
		return "✕", style.Error()
	}
}

// relativeTime renders t relative to now as a human-readable duration string.
// A zero t is rendered as "(never)".
func relativeTime(now, t time.Time) string {
	if t.IsZero() {
		return "(never)"
	}
	since := now.Sub(t)
	if since <= 0 {
		return "just now"
	}
	switch {
	case since < time.Minute:
		return "just now"
	case since < time.Hour:
		return fmt.Sprintf("%dm ago", int(since.Minutes()))
	case since < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(since.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(since.Hours()/24))
	}
}

// ── reservedPane ─────────────────────────────────────────────────────────────

// reservedPane renders an empty faint-bordered placeholder for the sixth grid cell.
// Reserved for a future pane; no content is rendered inside.
func reservedPane(width, height int) string {
	return paneBox("", width, height, false) // reserved
}
