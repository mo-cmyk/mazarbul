package picker

import (
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// RowRenderer is a caller-supplied function that renders one item into one or
// more display lines.
//
//   - selected  multi-select checkbox state (◆ vs ◇)
//   - cursor    item is under the navigation caret (▸ vs space)
//   - width     available terminal columns
type RowRenderer[T any] func(item T, selected, cursor bool, width int) []string

// gutterWidth is the leading column count used by DefaultRenderer's subtitle.
const gutterWidth = 4

// Model is the reusable picker component.
// Cursor and offset are positions within the filtered-index list, not items.
type Model[T any] struct {
	items    []T
	render   RowRenderer[T]
	keys     KeyMap
	cursor   int // position in m.filtered
	offset   int // first visible position in m.filtered
	width    int
	height   int
	multiSel bool
	selected map[int]bool // keyed by original item index in m.items
	// search state
	searching bool
	search    textinput.Model
	filtered  []int          // original indices of m.items matching the query
	titleOf   func(T) string // optional extractor for substring match
	// color gate
	enabled bool
	// showHelp controls whether View() appends the keymap help line. A parent
	// that owns its own footer (e.g. browse) disables this to avoid a doubled
	// help line; the search bar still renders when searching.
	showHelp bool
	// confirm state
	chosen    bool
	chosenIdx int // position in m.filtered at confirm time
	// quit state
	quitting bool
}

// New returns a Model[T] with default settings.
//
// Items is the full data set; render is the row renderer. Use WithTitleOf to
// enable substring search.
func New[T any](items []T, render RowRenderer[T]) Model[T] {
	ti := textinput.New()
	ti.SetVirtualCursor(true)
	ti.Placeholder = "search…"
	ti.Prompt = "/ "
	m := Model[T]{
		items:    items,
		render:   render,
		keys:     DefaultKeyMap(),
		selected: make(map[int]bool),
		search:   ti,
		enabled:  true,
		showHelp: true,
	}
	m.filtered = m.allIndices()
	return m
}

// WithHelp controls whether View() appends the keymap help line. Disable it
// when a parent renders its own footer (the search bar still appears when the
// user is typing a query).
func (m Model[T]) WithHelp(v bool) Model[T] { m.showHelp = v; return m }

// WithMultiSelect enables or disables multi-select mode (space to toggle).
func (m Model[T]) WithMultiSelect(v bool) Model[T] { m.multiSel = v; return m }

// WithSize sets the available terminal area for the component.
func (m Model[T]) WithSize(w, h int) Model[T] { m.width = w; m.height = h; return m }

// WithEnabled controls the color gate. When false, View() emits no ANSI escape
// sequences; glyphs and text remain.
func (m Model[T]) WithEnabled(v bool) Model[T] { m.enabled = v; return m }

// WithTitleOf stores an extractor used for substring search. When nil the
// component falls back to matching against the ANSI-stripped first rendered
// line.
func (m Model[T]) WithTitleOf(fn func(T) string) Model[T] { m.titleOf = fn; return m }

// DefaultRenderer returns a two-line RowRenderer:
//
//   - line 1: node glyph (◆ selected / ◇ unselected) + caret (▸ cursor / space) + title
//   - line 2: subtitle in SubtleColor, indented by gutterWidth
//
// The cursor-row title is highlighted in Accent when color is enabled.
// Subtitle may be nil to produce blank second lines.
func DefaultRenderer[T any](title func(T) string, subtitle func(T) string) RowRenderer[T] {
	return func(item T, selected, cursor bool, _ int) []string {
		noColor := os.Getenv("NO_COLOR") != ""

		nodeGlyph := "◇"
		if selected {
			nodeGlyph = "◆"
		}
		caret := " "
		if cursor {
			caret = "▸"
		}
		// prefix1: node-glyph + caret + space  (3 visible columns)
		// prefix2: gutterWidth spaces for subtitle indent
		prefix1 := nodeGlyph + caret + " "
		prefix2 := strings.Repeat(" ", gutterWidth)

		titleText := title(item)
		var subtitleText string
		if subtitle != nil {
			subtitleText = subtitle(item)
		}

		var line1, line2 string
		if !noColor && cursor {
			line1 = prefix1 + lipgloss.NewStyle().Foreground(style.Accent()).Render(titleText)
		} else {
			line1 = prefix1 + titleText
		}

		if !noColor && subtitleText != "" {
			line2 = prefix2 + lipgloss.NewStyle().Foreground(style.SubtleColor()).Render(subtitleText)
		} else {
			line2 = prefix2 + subtitleText
		}

		return []string{line1, line2}
	}
}

// ── Elm cycle ─────────────────────────────────────────────────────────────────

// Init returns nil; the parent supplies any ticks needed by sub-models.
func (m Model[T]) Init() tea.Cmd { return nil }

// Update is the pure state-transition function for the picker component.
func (m Model[T]) Update(msg tea.Msg) (Model[T], tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyPressMsg:
		if m.searching {
			return m.handleSearchKey(msg)
		}
		return m.handleNavKey(msg)
	}
	return m, nil
}

// handleSearchKey routes keystrokes to the embedded textinput and recomputes
// the filtered list after every change.
func (m Model[T]) handleSearchKey(msg tea.KeyPressMsg) (Model[T], tea.Cmd) {
	// Esc — exit search and reset filter.
	if key.Matches(msg, m.keys.Back) {
		m.searching = false
		m.search.SetValue("")
		m.search.Blur()
		m.filtered = m.allIndices()
		m.cursor = 0
		m.offset = 0
		return m, nil
	}
	// Enter — exit search, keep filter.
	if key.Matches(msg, m.keys.Confirm) {
		m.searching = false
		m.search.Blur()
		return m, nil
	}
	// Route to textinput.
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	q := m.search.Value()
	m.filtered = m.computeFiltered(q)
	// Keep cursor in bounds.
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
	m.offset = 0
	return m, cmd
}

// handleNavKey processes navigation keystrokes outside search mode.
//
//nolint:gocyclo // key dispatch table; splitting would obscure the intent without reducing real complexity
func (m Model[T]) handleNavKey(msg tea.KeyPressMsg) (Model[T], tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.quitting = true
		return m, nil

	case key.Matches(msg, m.keys.Back):
		m.quitting = true
		return m, nil

	case key.Matches(msg, m.keys.Search):
		m.searching = true
		cmd := m.search.Focus()
		return m, cmd

	case key.Matches(msg, m.keys.Up):
		if m.cursor > 0 {
			m.cursor--
			m.ensureCursorVisible()
		}
		return m, nil

	case key.Matches(msg, m.keys.Down):
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
			m.ensureCursorVisible()
		}
		return m, nil

	case key.Matches(msg, m.keys.NextPage):
		rows := m.visibleRows()
		m.cursor = min(max(0, len(m.filtered)-1), m.cursor+rows)
		m.ensureCursorVisible()
		return m, nil

	case key.Matches(msg, m.keys.PrevPage):
		rows := m.visibleRows()
		m.cursor = max(0, m.cursor-rows)
		m.ensureCursorVisible()
		return m, nil

	case key.Matches(msg, m.keys.Home):
		m.cursor = 0
		m.offset = 0
		return m, nil

	case key.Matches(msg, m.keys.End):
		if len(m.filtered) > 0 {
			m.cursor = len(m.filtered) - 1
			m.ensureCursorVisible()
		}
		return m, nil

	case key.Matches(msg, m.keys.Confirm):
		if len(m.filtered) > 0 {
			m.chosen = true
			m.chosenIdx = m.cursor
		}
		return m, nil

	case key.Matches(msg, m.keys.Toggle):
		if m.multiSel && len(m.filtered) > 0 {
			origIdx := m.filtered[m.cursor]
			m.selected[origIdx] = !m.selected[origIdx]
		}
		return m, nil
	}
	return m, nil
}

// ── View ──────────────────────────────────────────────────────────────────────

// View renders the component as a plain string. The parent integrates this into
// its own layout via lipgloss.JoinVertical or similar.
//
// When the model is disabled OR NO_COLOR is set, no ANSI escape sequences are
// emitted (glyphs stay — symbol-first accessibility). When ACCESSIBLE is set,
// a simple numbered list is rendered instead.
func (m Model[T]) View() string {
	noColor := !m.enabled || os.Getenv("NO_COLOR") != ""
	if os.Getenv("ACCESSIBLE") != "" {
		return m.accessibleView()
	}
	sep := m.separatorLine(noColor)
	out := m.renderItemRows(noColor, sep) + m.renderFooter(noColor)
	return m.applyWidthCap(out)
}

// renderItemRows renders the visible slice of filtered items separated by sep.
func (m Model[T]) renderItemRows(noColor bool, sep string) string {
	var b strings.Builder
	rows := m.visibleRows()
	end := m.offset + rows
	if end > len(m.filtered) {
		end = len(m.filtered)
	}
	for i := m.offset; i < end; i++ {
		origIdx := m.filtered[i]
		lines := m.render(m.items[origIdx], m.selected[origIdx], i == m.cursor, m.width)
		if i > m.offset {
			b.WriteString(sep + "\n")
		}
		for _, line := range lines {
			b.WriteString(maybeStrip(line, noColor) + "\n")
		}
	}
	return b.String()
}

// renderFooter renders the optional search bar and the help line. The help
// line is omitted when showHelp is false (the parent owns its own footer); the
// search bar is always shown while searching.
func (m Model[T]) renderFooter(noColor bool) string {
	var b strings.Builder
	if m.searching {
		b.WriteString(maybeStrip(m.search.View(), noColor) + "\n")
	}
	if m.showHelp {
		b.WriteString(maybeStrip(m.keys.Help(), noColor))
	}
	return b.String()
}

// applyWidthCap hard-truncates every line in s to m.width columns.
func (m Model[T]) applyWidthCap(s string) string {
	if m.width <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if ansi.StringWidth(line) > m.width {
			lines[i] = ansi.Truncate(line, m.width, "")
		}
	}
	return strings.Join(lines, "\n")
}

// maybeStrip returns ansi.Strip(s) when noColor is true, otherwise s unchanged.
func maybeStrip(s string, noColor bool) string {
	if noColor {
		return ansi.Strip(s)
	}
	return s
}

// accessibleView renders a simple numbered list for screen-reader / CI use.
func (m Model[T]) accessibleView() string {
	var b strings.Builder
	for i, origIdx := range m.filtered {
		item := m.items[origIdx]
		var titleText string
		if m.titleOf != nil {
			titleText = m.titleOf(item)
		} else {
			lines := m.render(item, false, false, m.width)
			if len(lines) > 0 {
				titleText = ansi.Strip(lines[0])
			}
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, titleText)
	}
	return b.String()
}

// separatorLine returns a horizontal rule styled as dim chrome when color is
// enabled.
func (m Model[T]) separatorLine(noColor bool) string {
	w := m.width
	if w <= 0 {
		w = 40
	}
	s := strings.Repeat("─", w)
	if noColor {
		return s
	}
	return lipgloss.NewStyle().Foreground(style.ANSIMuted()).Render(s)
}

// ── Accessors ─────────────────────────────────────────────────────────────────

// Chosen returns the confirmed item and true when the Confirm binding was used.
func (m Model[T]) Chosen() (T, bool) {
	if !m.chosen || len(m.filtered) == 0 {
		var zero T
		return zero, false
	}
	origIdx := m.filtered[m.chosenIdx]
	return m.items[origIdx], true
}

// Selected returns all items marked with Toggle in multi-select mode, in
// original item order.
func (m Model[T]) Selected() []T {
	var result []T
	for i, item := range m.items {
		if m.selected[i] {
			result = append(result, item)
		}
	}
	return result
}

// Quitting returns true once the Quit or Back binding was pressed.
func (m Model[T]) Quitting() bool { return m.quitting }

// Searching reports whether the picker is in incremental-search mode (the
// search bar is focused and capturing keystrokes). A host model uses this to
// route every keystroke to the picker while a query is being typed — so esc
// clears the query instead of being swallowed as a global "back/quit" key.
func (m Model[T]) Searching() bool { return m.searching }

// ── Internal helpers ──────────────────────────────────────────────────────────

// visibleRows returns how many items fit in the current height.
// When height is 0 (uninitialized), all filtered items are visible.
func (m Model[T]) visibleRows() int {
	if m.height == 0 {
		return len(m.filtered)
	}
	reserved := 0
	if m.showHelp {
		reserved++ // help footer
	}
	if m.searching {
		reserved++ // search input line
	}
	available := m.height - reserved
	if available < 1 {
		return 1
	}
	// Each item takes 2 content lines + 1 separator = 3 lines, except the last
	// item (no trailing separator), which takes 2.  N items → 3N-1 lines.
	// Solve: 3N-1 ≤ available → N ≤ (available+1)/3.
	rows := (available + 1) / 3
	if rows < 1 {
		return 1
	}
	return rows
}

// ensureCursorVisible adjusts offset so the cursor row is within the viewport.
func (m *Model[T]) ensureCursorVisible() {
	rows := m.visibleRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	} else if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// allIndices returns [0, 1, … len(m.items)-1].
func (m Model[T]) allIndices() []int {
	idx := make([]int, len(m.items))
	for i := range idx {
		idx[i] = i
	}
	return idx
}

// computeFiltered returns original indices whose titles contain query
// (case-insensitive). An empty query returns all indices.
func (m Model[T]) computeFiltered(query string) []int {
	if query == "" {
		return m.allIndices()
	}
	q := strings.ToLower(query)
	var result []int
	for i, item := range m.items {
		if m.matchItem(item, q) {
			result = append(result, i)
		}
	}
	return result
}

// matchItem returns true when item contains query (lower-cased).
func (m Model[T]) matchItem(item T, query string) bool {
	if m.titleOf != nil {
		return strings.Contains(strings.ToLower(m.titleOf(item)), query)
	}
	// Fallback: render and match against the first ANSI-stripped line.
	lines := m.render(item, false, false, m.width)
	if len(lines) == 0 {
		return false
	}
	return strings.Contains(strings.ToLower(ansi.Strip(lines[0])), query)
}
