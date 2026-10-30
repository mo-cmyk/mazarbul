// Package browse renders a master-detail TUI for exploring recall results.
//
// The caller supplies a Loader that implements the recall→get→neighbors loop.
// Browse composes internal/tui/picker.Model[Record] for the master list and a
// lipgloss detail card for the selected item.  Both panes live in a single
// Bubble Tea model that switches between browse and detail modes.
//
// Layout (browse mode):
//
//	› <query>
//	[filter strip — full width, optional]
//	[picker — full width]
//	[keymap help · enter: open]
//
// Layout (detail mode):
//
//	› <query>
//	[detail card — 2/3 width] │ [neighbors picker — 1/3 width]
//	esc: back
package browse

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mo-cmyk/mazarbul/internal/tui/picker"
	"github.com/mo-cmyk/mazarbul/internal/tui/style"
	"github.com/mo-cmyk/mazarbul/internal/tui/style/mdrender"
)

// Record is one browsable result row.
type Record struct {
	// ID is the opaque entity identifier passed to Loader.Detail / Loader.Neighbors.
	ID string
	// Title is the entity display name shown in the list.
	Title string
	// Subtitle is a caller-formatted summary line, e.g. "type · org/repo · ⎇ branch · author".
	Subtitle string
	// Kind is the entity kind used for type-coloured node glyphs.
	Kind string
}

// Loader is the recall→get→neighbors loop supplied by the cli layer.
// Browse never imports internal/cli; the cli layer passes a Loader implementation.
type Loader interface {
	// Search returns the top-K records matching query with the given facets.
	Search(query string, f Filters) ([]Record, error)
	// Detail returns the markdown body for the given entity id.
	Detail(id string) (string, error)
	// Neighbors returns records adjacent to the given entity id.
	Neighbors(id string) ([]Record, error)
}

// Editor is optionally implemented by doc-browse loaders.
// Browse type-asserts to Editor and enables e/n keys only when the loader
// implements this interface.  The recall loader does not implement it.
//
// Returned tea.Cmd MUST use tea.ExecProcess to suspend/resume around $EDITOR.
type Editor interface {
	// Edit opens the existing doc identified by id in $EDITOR.
	Edit(id string) tea.Cmd
	// New prompts for a path then opens $EDITOR for a new doc.
	New() tea.Cmd
}

// LinkFollower is optionally implemented by doc-browse loaders.
// Browse type-asserts to LinkFollower and enables the L key only when the
// loader implements this interface.  The recall loader does not implement it.
type LinkFollower interface {
	// Links returns the resolved outgoing wikilinks for the given record id.
	Links(id string) []Record
}

// searchDebounce is the quiet-period before a filter-strip or query change
// fires a new loader.Search call.  Rapid toggles coalesce into one round-trip.
const searchDebounce = 250 * time.Millisecond

// searchTickMsg is the debounce-expiry message.  Seq matches m.searchSeq at
// the time the tick was started; stale ticks (seq < m.searchSeq) are ignored.
type searchTickMsg struct{ seq int }

// browseMode is the internal display state of the model.
type browseMode int

const (
	// ModeBrowse shows the picker master list.
	modeBrowse browseMode = iota
	// ModeDetail shows the detail card and neighbor side list.
	modeDetail
)

// Model is the root Bubble Tea model for the browse screen.
// It composes picker.Model[Record] for both the master list and the neighbor
// side panel in detail mode.
type Model struct {
	loader   Loader
	query    string
	pk       picker.Model[Record] // master list
	mode     browseMode
	detail   string               // markdown body for the current detail
	detailID string               // ID of the currently displayed record
	nb       picker.Model[Record] // neighbor side list (detail mode only)
	width    int
	height   int
	enabled  bool
	dark     bool
	keys     picker.KeyMap
	// filter strip (optional; empty when no banks are set)
	strip     FilterStrip
	searchSeq int // incremented on strip/query change; stale ticks discarded
}

// recordRenderer returns a DefaultRenderer for Record items.
func recordRenderer() picker.RowRenderer[Record] {
	return picker.DefaultRenderer(
		func(r Record) string { return r.Title },
		func(r Record) string { return r.Subtitle },
	)
}

// New returns a Model pre-seeded with records.
//
// Enabled gates colour and ANSI styling; pass false for CI / screen-reader
// mode.  The caller must set query on the returned model before Run.
func New(loader Loader, records []Record, enabled bool) Model {
	rend := recordRenderer()
	// Browse owns the header/footer chrome, so both pickers render rows only
	// (WithHelp(false)) — otherwise the picker's own help line would double the
	// browse footer and leak into the neighbor column in detail mode.
	pk := picker.New(records, rend).
		WithEnabled(enabled).
		WithHelp(false).
		WithTitleOf(func(r Record) string { return r.Title })
	nb := picker.New([]Record{}, rend).
		WithEnabled(enabled).
		WithHelp(false).
		WithTitleOf(func(r Record) string { return r.Title })
	return Model{
		loader:  loader,
		pk:      pk,
		nb:      nb,
		enabled: enabled,
		dark:    true, // default dark; captured once by the program
		keys:    picker.DefaultKeyMap(),
	}
}

// WithDark sets the dark-theme flag used when rendering detail markdown.
func (m Model) WithDark(dark bool) Model { m.dark = dark; return m }

// WithFilterStrip attaches a filter strip rendered between the header and the
// master list.  Banks describe which facets are shown; an empty slice is a
// no-op (same as not calling this method).  Callers that want no strip (e.g.
// recall --tui without facets) simply omit this call.
func (m Model) WithFilterStrip(banks []Bank) Model {
	m.strip = NewFilterStrip(banks, m.enabled)
	return m
}

// Init satisfies tea.Model; browse needs no background ticks at start.
func (m Model) Init() tea.Cmd { return nil }

// Update is the pure Elm-architecture state-transition function.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		contentH := m.contentHeight()
		m.pk = m.pk.WithSize(msg.Width, contentH)
		nbW := m.nbWidth()
		m.nb = m.nb.WithSize(nbW, contentH)
		return m, nil

	case tea.KeyPressMsg:
		if m.mode == modeDetail {
			return m.handleDetailKey(msg)
		}
		return m.handleBrowseKey(msg)

	case searchTickMsg:
		return m.handleSearchTick(msg)
	}
	return m, nil
}

// handleBrowseKey dispatches keystrokes in browse mode.
//
//nolint:cyclop // key dispatch table; splitting would obscure intent without reducing real complexity
func (m Model) handleBrowseKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// While the picker is in incremental-search mode, every keystroke belongs to
	// the search box — forward it unconditionally. This is what makes esc CLEAR
	// the query (and return to the full list) instead of quitting the program,
	// and stops typed letters like "q" from being swallowed as the global Quit
	// key while the user is typing a query.
	if m.pk.Searching() {
		p, cmd := m.pk.Update(msg)
		m.pk = p
		return m, cmd
	}
	// Quit (q / ctrl+c).
	if key.Matches(msg, m.keys.Quit) {
		return m, tea.Quit
	}
	// Back (esc) in browse mode quits.
	if key.Matches(msg, m.keys.Back) {
		return m, tea.Quit
	}

	// Tab: cycle focus between picker and filter strip.
	tabK := key.NewBinding(key.WithKeys("tab"))
	if len(m.strip.banks) > 0 && key.Matches(msg, tabK) {
		m.strip = m.strip.WithFocused(!m.strip.Focused())
		return m, nil
	}

	// When the strip is focused, route filter keys to it; debounce on change.
	if m.strip.Focused() {
		prev := m.strip.Filters()
		m.strip = m.strip.Update(msg)
		if !filtersEqual(prev, m.strip.Filters()) {
			return m.startSearchDebounce()
		}
		return m, nil
	}

	// Capability keys: Editor (e/n) and LinkFollower (L).
	if cmd := m.handleCapabilityKey(msg); cmd != nil {
		return m, cmd
	}

	// Confirm (enter) → open detail for the current cursor item.
	if key.Matches(msg, m.keys.Confirm) {
		p, _ := m.pk.Update(msg)
		m.pk = p
		if rec, ok := m.pk.Chosen(); ok {
			return m.openDetail(rec.ID)
		}
		return m, nil
	}
	// All other keys are forwarded to the picker (navigation, search, etc.).
	p, cmd := m.pk.Update(msg)
	m.pk = p
	return m, cmd
}

// handleCapabilityKey routes e/n/L when the loader implements Editor or
// LinkFollower.  Returns a non-nil tea.Cmd when a capability was invoked,
// nil otherwise (caller falls through to picker routing).
func (m *Model) handleCapabilityKey(msg tea.KeyPressMsg) tea.Cmd {
	edEditK := key.NewBinding(key.WithKeys("e"))
	edNewK := key.NewBinding(key.WithKeys("n"))
	linkK := key.NewBinding(key.WithKeys("L"))

	if ed, ok := m.loader.(Editor); ok {
		if key.Matches(msg, edEditK) {
			tmp, _ := m.pk.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if rec, ok := tmp.Chosen(); ok {
				return ed.Edit(rec.ID)
			}
			return nil
		}
		if key.Matches(msg, edNewK) {
			return ed.New()
		}
	}
	if lf, ok := m.loader.(LinkFollower); ok {
		if key.Matches(msg, linkK) {
			tmp, _ := m.pk.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if rec, ok := tmp.Chosen(); ok {
				m.pk = m.makePickerFromRecords(lf.Links(rec.ID))
			}
			// Return a non-nil but no-op cmd so caller knows it was handled.
			return func() tea.Msg { return nil }
		}
	}
	return nil
}

// handleDetailKey dispatches keystrokes in detail mode.
func (m Model) handleDetailKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Esc → back to browse.
	if key.Matches(msg, m.keys.Back) {
		m.mode = modeBrowse
		return m, nil
	}
	// Quit (q / ctrl+c).
	if key.Matches(msg, m.keys.Quit) {
		return m, tea.Quit
	}
	// Confirm (enter) on a neighbor → re-seed the detail with that neighbor.
	if key.Matches(msg, m.keys.Confirm) {
		p, _ := m.nb.Update(msg)
		m.nb = p
		if rec, ok := m.nb.Chosen(); ok {
			return m.openDetail(rec.ID)
		}
		return m, nil
	}
	// All other keys go to the neighbor picker (navigation).
	p, cmd := m.nb.Update(msg)
	m.nb = p
	return m, cmd
}

// openDetail loads the detail body and neighbors for id, then switches to
// detail mode.  Loader calls are made synchronously; top-K recall is bounded
// so the latency is acceptable within an Update call.
func (m Model) openDetail(id string) (tea.Model, tea.Cmd) {
	m.detailID = id

	body, err := m.loader.Detail(id)
	if err != nil {
		body = "*Error loading detail.*"
	}
	m.detail = body

	nbRecords, err := m.loader.Neighbors(id)
	if err != nil {
		nbRecords = nil
	}
	rend := recordRenderer()
	nb := picker.New(nbRecords, rend).
		WithEnabled(m.enabled).
		WithHelp(false).
		WithTitleOf(func(r Record) string { return r.Title })
	if m.width > 0 {
		nbH := m.contentHeight()
		nb = nb.WithSize(m.nbWidth(), nbH)
	}
	m.nb = nb
	m.mode = modeDetail
	return m, nil
}

// startSearchDebounce increments searchSeq and schedules a 250ms tick.  When
// the tick fires with the same seq, handleSearchTick re-queries the loader.
func (m Model) startSearchDebounce() (tea.Model, tea.Cmd) {
	m.searchSeq++
	seq := m.searchSeq
	return m, tea.Tick(searchDebounce, func(time.Time) tea.Msg {
		return searchTickMsg{seq: seq}
	})
}

// handleSearchTick fires the actual loader.Search when the debounce expires.
// Stale ticks (seq mismatch) are silently ignored.
func (m Model) handleSearchTick(msg searchTickMsg) (tea.Model, tea.Cmd) {
	if msg.seq != m.searchSeq {
		return m, nil
	}
	records, err := m.loader.Search(m.query, m.strip.Filters())
	if err != nil {
		return m, nil
	}
	m.pk = m.makePickerFromRecords(records)
	return m, nil
}

// makePickerFromRecords builds a new master-list picker sized to the current
// viewport.  Used by initial load, debounced re-queries, and LinkFollower.
func (m Model) makePickerFromRecords(records []Record) picker.Model[Record] {
	rend := recordRenderer()
	pk := picker.New(records, rend).
		WithEnabled(m.enabled).
		WithHelp(false).
		WithTitleOf(func(r Record) string { return r.Title })
	if m.width > 0 {
		pk = pk.WithSize(m.width, m.contentHeight())
	}
	return pk
}

// View satisfies tea.Model; it declares the alt-screen and delegates to buildView.
func (m Model) View() tea.View {
	v := tea.NewView(m.buildView())
	v.AltScreen = true
	return v
}

// buildView computes the full screen string for the current model state.
func (m Model) buildView() string {
	if m.width == 0 {
		return "loading…"
	}

	header := m.renderHeader()
	footer := m.renderFooter()

	var body string
	if m.mode == modeDetail {
		body = m.renderDetailPane()
	} else {
		body = m.pk.View()
	}

	parts := []string{header}
	if sv := m.strip.View(m.width); sv != "" {
		parts = append(parts, sv)
	}
	parts = append(parts, body, footer)
	full := lipgloss.JoinVertical(lipgloss.Left, parts...)
	full = m.applyWidthCap(full)

	// Strip all ANSI when color is disabled regardless of NO_COLOR.
	if !m.enabled {
		return ansi.Strip(full)
	}
	return full
}

// noColor returns true when color should be suppressed (enabled=false or NO_COLOR set).
func (m Model) noColor() bool {
	return !m.enabled || os.Getenv("NO_COLOR") != ""
}

// renderHeader renders "› <query>" with the accent color when color is active.
func (m Model) renderHeader() string {
	q := m.query
	if q == "" {
		q = "(all)"
	}
	if m.noColor() {
		return "› " + q
	}
	prefix := lipgloss.NewStyle().Foreground(style.Accent()).Render("›")
	return prefix + " " + q
}

// renderFooter renders the keymap help line, the strip hint, or "esc: back".
func (m Model) renderFooter() string {
	nc := m.noColor()
	if m.mode == modeDetail {
		if nc {
			return "esc: back"
		}
		return lipgloss.NewStyle().Foreground(style.ANSIMuted()).Render("esc") + ": back"
	}
	// Strip-focus hint replaces the normal keymap when the strip is active.
	if m.strip.Focused() {
		hint := "↑/↓: bank  ←/→: move  space: toggle  r: reset  tab: to list"
		if nc {
			return hint
		}
		return lipgloss.NewStyle().Foreground(style.ANSIMuted()).Render(hint)
	}
	hint := m.browseKeyHints()
	if nc {
		return hint
	}
	return lipgloss.NewStyle().Foreground(style.ANSIMuted()).Render(hint)
}

// browseKeyHints builds the browse-mode footer legend. It is composed (rather
// than delegating to picker.Help) so it can surface the keys that actually
// apply: `tab` to reach the filter strip when one is present, and the loader's
// capability keys (e/n edit/new for an Editor, L for a LinkFollower). When the
// loader is an Editor the picker's `n`=next-page binding is shadowed by `n`=new
// note, so the legend advertises e/n/L instead of a misleading paging hint.
func (m Model) browseKeyHints() string {
	parts := []string{"↑/↓ move", "/ search"}
	_, isEditor := m.loader.(Editor)
	_, isLinker := m.loader.(LinkFollower)
	if isEditor {
		parts = append(parts, "e edit", "n new")
	} else {
		parts = append(parts, "n/p page")
	}
	if isLinker {
		parts = append(parts, "L follow link")
	}
	if len(m.strip.banks) > 0 {
		parts = append(parts, "tab filters")
	}
	parts = append(parts, "enter open", "q quit")
	return strings.Join(parts, " · ")
}

// detailGutter is the blank column count between the detail card and the
// neighbor list so the two panes read as distinct surfaces, not glued together.
const detailGutter = 2

// renderDetailPane renders the split-view: detail card (left) + neighbor picker (right).
func (m Model) renderDetailPane() string {
	// Reserve the gutter out of the card's two-thirds so card + gutter + neighbor
	// column sum to the screen width without tripping the width cap.
	detailW := m.width*2/3 - detailGutter
	if detailW < 30 {
		detailW = 30
	}
	innerWidth := detailW - 6 // border(2) + padding(4) = 6
	if innerWidth < 10 {
		innerWidth = 10
	}

	rendered := mdrender.Render(m.detail, innerWidth, m.dark)

	cardStyle := lipgloss.NewStyle().
		Border(style.Border()).
		BorderForeground(style.PurpleColor()).
		Padding(0, 2).
		MarginRight(detailGutter).
		Width(innerWidth)

	detailCard := cardStyle.Render(rendered)

	right := m.renderNeighborColumn()
	return lipgloss.JoinHorizontal(lipgloss.Top, detailCard, right)
}

// renderNeighborColumn renders the neighbor list under a dim "neighbors"
// heading so the right pane is self-labelling instead of an unframed list.
func (m Model) renderNeighborColumn() string {
	nbView := m.nb.View()
	heading := "neighbors"
	if !m.noColor() {
		heading = lipgloss.NewStyle().Foreground(style.ANSIMuted()).Render(heading)
	}
	return lipgloss.JoinVertical(lipgloss.Left, heading, "", nbView)
}

// applyWidthCap hard-truncates every line in s to m.width visible columns.
func (m Model) applyWidthCap(s string) string {
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

// nbWidth returns the column budget for the neighbor picker panel.
func (m Model) nbWidth() int {
	w := m.width / 3
	if w < 20 {
		return 20
	}
	return w
}

// stripLines returns the number of terminal lines occupied by the filter strip.
// Zero when no banks are configured; otherwise len(banks) + 2 (top and bottom
// border rows of the bordered box).
func (m Model) stripLines() int {
	if len(m.strip.banks) == 0 {
		return 0
	}
	return len(m.strip.banks) + 2
}

// contentHeight returns the available lines for the picker (total height minus
// header, footer, and the optional filter-strip line).
func (m Model) contentHeight() int {
	h := m.height - 4 - m.stripLines()
	if h < 4 {
		return 4
	}
	return h
}

// ── filter helpers (used by handleBrowseKey via startSearchDebounce) ──────────

// filtersEqual reports whether two Filters are structurally identical.
// Used to detect strip changes before starting the debounce timer.
func filtersEqual(a, b Filters) bool {
	if a.MinScore != b.MinScore || a.At != b.At || a.PathPrefix != b.PathPrefix ||
		a.Tag != b.Tag || a.Pinned != b.Pinned {
		return false
	}
	return stringSlicesEqual(a.Kinds, b.Kinds) && stringSlicesEqual(a.Scopes, b.Scopes)
}

// stringSlicesEqual reports whether a and b have equal length and values in order.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── Run ───────────────────────────────────────────────────────────────────────

// Run seeds the model with Search(initialQuery, Filters{}) and runs the
// alt-screen TUI.
//
// Enabled gates all colour output; pass false for CI or --no-tui mode.
func Run(loader Loader, initialQuery string, enabled bool) error {
	records, err := loader.Search(initialQuery, Filters{})
	if err != nil {
		return fmt.Errorf("browse: initial search: %w", err)
	}
	m := New(loader, records, enabled)
	m.query = initialQuery
	if _, err = tea.NewProgram(m).Run(); err != nil {
		return fmt.Errorf("browse: %w", err)
	}
	return nil
}

// RunWithStrip seeds the model with Search(initialQuery, Filters{}), attaches
// the given facet banks as a filter strip rendered between the header and the
// master list, and runs the alt-screen TUI. An empty banks slice behaves like
// Run (no strip).
//
// Enabled gates all colour output; pass false for CI or --no-tui mode.
func RunWithStrip(loader Loader, initialQuery string, enabled bool, banks []Bank) error {
	records, err := loader.Search(initialQuery, Filters{})
	if err != nil {
		return fmt.Errorf("browse: initial search: %w", err)
	}
	m := New(loader, records, enabled).WithFilterStrip(banks)
	m.query = initialQuery
	if _, err = tea.NewProgram(m).Run(); err != nil {
		return fmt.Errorf("browse: %w", err)
	}
	return nil
}
