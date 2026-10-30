// filterstrip.go — declarative, reusable filter-strip component.
// It renders a pill bar between the browse header and the master list.
// Pure value type: every Update returns a new FilterStrip.

package browse

import (
	"os"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// Filters is the faceted query state emitted by the filter strip and consumed
// by Loader.Search. Each screen uses the subset of fields it needs.
type Filters struct {
	Kinds      []string // recall: entity kinds
	Scopes     []string // recall: personal|project|team
	MinScore   float64  // recall: 0..1
	At         string   // recall: RFC3339 valid-time anchor ("" = now)
	PathPrefix string   // docs
	Tag        string   // docs
	Pinned     bool     // docs
}

// BankKind identifies the interaction model for a filter bank.
type BankKind int

const (
	// BankToggle allows multiple pills to be independently toggled on/off
	// (like checkboxes). Emits a []string of selected option labels.
	BankToggle BankKind = iota
	// BankSingle is a radio-group: selecting one pill deselects the rest.
	// Emits the selected option label (or "" when none selected).
	BankSingle
	// BankSlider is an ordered-stop selector: exactly one stop is active.
	// Options are the stop labels in order ("0","0.05","0.1","0.2"); the
	// active stop index determines the emitted float64 for minScore.
	BankSlider
)

// Bank describes one visual group of filter pills in the strip.
type Bank struct {
	// Label is the human-readable name shown before the pills ("Kind", "Scope").
	Label string
	// Kind controls the multi/single/slider selection semantics.
	Kind BankKind
	// Options are the pill labels in display order.
	// For BankSlider they are ordered stop labels ("0","0.05","0.1","0.2").
	Options []string
	// Key names the Filters field this bank writes.
	// Supported: "kinds","scopes","at","minScore","pathPrefix","tag","pinned".
	Key string
}

// FilterStrip is a declarative, reusable filter-strip component.
//
// It renders a horizontal pill bar.  The caller (browse.Model) stores it as a
// value and replaces it on every Update; there is no shared mutable state.
//
// Focus lifecycle: browse.Model calls WithFocused(true) on Tab; the strip
// then renders the focused pill in Accent() and routes key events to itself
// until WithFocused(false) is called again.
type FilterStrip struct {
	banks   []Bank
	focused bool          // keyboard focus is on this strip
	bankIdx int           // focused bank index (0 … len(banks)-1)
	pillIdx int           // focused pill index within bankIdx
	toggled []toggleState // BankToggle: per-bank set of active indices
	single  []int         // BankSingle: per-bank selected index (-1 = none)
	slider  []int         // BankSlider: per-bank active stop index
	enabled bool
}

// toggleState is a pill-index → on/off map for a BankToggle bank.
// Defined as a named type so copies are independent.
type toggleState map[int]bool

// NewFilterStrip creates a FilterStrip with the given banks.
// Enabled gates ANSI colour; pass false for CI / --no-tui mode.
func NewFilterStrip(banks []Bank, enabled bool) FilterStrip {
	toggled := make([]toggleState, len(banks))
	single := make([]int, len(banks))
	slider := make([]int, len(banks))
	for i := range banks {
		toggled[i] = make(toggleState)
		single[i] = -1 // nothing selected
		// slider[i] = 0 is the zero value (first stop active)
	}
	return FilterStrip{
		banks:   banks,
		enabled: enabled,
		toggled: toggled,
		single:  single,
		slider:  slider,
	}
}

// WithFocused sets keyboard focus on the strip. Called by browse.Model on Tab.
func (s FilterStrip) WithFocused(focused bool) FilterStrip { s.focused = focused; return s }

// Focused reports whether the strip currently has keyboard focus.
func (s FilterStrip) Focused() bool { return s.focused }

// ── Update ────────────────────────────────────────────────────────────────────

// Update processes a key event when the strip is focused.
// It returns a new FilterStrip with updated selection state.
//
//   - space  toggle/select the focused pill
//   - ←/→    move the focused pill within (and across) banks
//   - ↑/↓    move focus to the previous/next bank
//   - r      reset all selections
//
// On a BankSlider, ←/→ also commit the new stop immediately (commit-on-move):
// the bar IS the value, so there is no separate "preview vs committed" state.
func (s FilterStrip) Update(msg tea.KeyPressMsg) FilterStrip {
	if len(s.banks) == 0 {
		return s
	}
	rightK := key.NewBinding(key.WithKeys("right"))
	leftK := key.NewBinding(key.WithKeys("left"))
	upK := key.NewBinding(key.WithKeys("up"))
	downK := key.NewBinding(key.WithKeys("down"))
	spaceK := key.NewBinding(key.WithKeys("space"))
	resetK := key.NewBinding(key.WithKeys("r"))
	switch {
	case key.Matches(msg, rightK):
		return s.moveRight()
	case key.Matches(msg, leftK):
		return s.moveLeft()
	case key.Matches(msg, upK):
		return s.focusBank(s.bankIdx - 1)
	case key.Matches(msg, downK):
		return s.focusBank(s.bankIdx + 1)
	case key.Matches(msg, spaceK):
		return s.activateFocused()
	case key.Matches(msg, resetK):
		return s.reset()
	}
	return s
}

// moveRight advances the focused pill one step to the right, crossing bank
// boundaries. Stops at the last pill of the last bank. On a slider bank the
// new stop is committed immediately.
func (s FilterStrip) moveRight() FilterStrip {
	b := s.banks[s.bankIdx]
	if s.pillIdx < len(b.Options)-1 {
		s.pillIdx++
		return s.commitSlider()
	}
	if s.bankIdx < len(s.banks)-1 {
		s.bankIdx++
		s.pillIdx = s.entryPill(s.bankIdx, 0)
	}
	return s
}

// moveLeft retreats the focused pill one step to the left, crossing bank
// boundaries. Stops at the first pill of the first bank. On a slider bank the
// new stop is committed immediately.
func (s FilterStrip) moveLeft() FilterStrip {
	if s.pillIdx > 0 {
		s.pillIdx--
		return s.commitSlider()
	}
	if s.bankIdx > 0 {
		s.bankIdx--
		s.pillIdx = s.entryPill(s.bankIdx, len(s.banks[s.bankIdx].Options)-1)
	}
	return s
}

// focusBank moves keyboard focus to bank idx (↑/↓), clamped to a valid bank.
// The pill cursor lands on the committed stop of a slider bank, otherwise on
// the same column clamped into the new bank's pill range. No selection state
// changes — moving focus never re-queries.
func (s FilterStrip) focusBank(idx int) FilterStrip {
	if idx < 0 || idx >= len(s.banks) || idx == s.bankIdx {
		return s
	}
	s.bankIdx = idx
	s.pillIdx = s.entryPill(idx, s.pillIdx)
	return s
}

// entryPill returns the pill index the cursor should land on when focus enters
// bank bi. A slider bank lands on its committed stop (so the bar shows the live
// value and ←/→ move relative to it); other banks clamp fallback into range.
func (s FilterStrip) entryPill(bi, fallback int) int {
	if s.banks[bi].Kind == BankSlider {
		return s.slider[bi]
	}
	maxIdx := len(s.banks[bi].Options) - 1
	if fallback < 0 {
		return 0
	}
	if fallback > maxIdx {
		return maxIdx
	}
	return fallback
}

// commitSlider writes the focused pill index into the slider's active stop when
// the focused bank is a BankSlider, so ←/→ change the value with no extra key.
func (s FilterStrip) commitSlider() FilterStrip {
	if s.banks[s.bankIdx].Kind != BankSlider {
		return s
	}
	newSlider := make([]int, len(s.slider))
	copy(newSlider, s.slider)
	newSlider[s.bankIdx] = s.pillIdx
	s.slider = newSlider
	return s
}

// activateFocused toggles/selects/slides the focused pill according to the
// bank's interaction kind.
func (s FilterStrip) activateFocused() FilterStrip {
	if len(s.banks) == 0 {
		return s
	}
	bi := s.bankIdx
	pi := s.pillIdx
	b := s.banks[bi]
	switch b.Kind {
	case BankToggle:
		newToggled := cloneToggled(s.toggled)
		newToggled[bi] = copyBoolMap(s.toggled[bi])
		newToggled[bi][pi] = !newToggled[bi][pi]
		s.toggled = newToggled
	case BankSingle:
		newSingle := make([]int, len(s.single))
		copy(newSingle, s.single)
		if newSingle[bi] == pi {
			newSingle[bi] = -1 // deselect on re-press
		} else {
			newSingle[bi] = pi
		}
		s.single = newSingle
	case BankSlider:
		newSlider := make([]int, len(s.slider))
		copy(newSlider, s.slider)
		newSlider[bi] = pi
		s.slider = newSlider
	}
	return s
}

// reset clears all selections across all banks.
func (s FilterStrip) reset() FilterStrip {
	toggled := make([]toggleState, len(s.banks))
	single := make([]int, len(s.banks))
	slider := make([]int, len(s.banks))
	for i := range s.banks {
		toggled[i] = make(toggleState)
		single[i] = -1
	}
	s.toggled = toggled
	s.single = single
	s.slider = slider
	// Re-sync the focus cursor so a slider bar can't keep showing a stale stop
	// after r (the cursor and the committed stop must agree).
	s.bankIdx = 0
	s.pillIdx = 0
	return s
}

// ── Filters ───────────────────────────────────────────────────────────────────

// Filters returns the current facet values derived from the strip's selection
// state. Fields not addressed by any bank remain at their zero value.
func (s FilterStrip) Filters() Filters {
	f := Filters{}
	for i, b := range s.banks {
		s.applyBankToFilters(&f, i, b)
	}
	return f
}

// applyBankToFilters writes one bank's selection into f, keyed on b.Key.
func (s FilterStrip) applyBankToFilters(f *Filters, i int, b Bank) {
	switch b.Key {
	case "kinds":
		f.Kinds = s.toggledOptions(i, b.Options)
	case "scopes":
		f.Scopes = s.toggledOptions(i, b.Options)
	case "at":
		f.At = s.singleOption(i, b.Options)
	case "minScore":
		f.MinScore = s.sliderFloat(i, b.Options)
	case "pathPrefix":
		f.PathPrefix = s.singleOption(i, b.Options)
	case "tag":
		f.Tag = s.singleOption(i, b.Options)
	case "pinned":
		f.Pinned = s.toggled[i][0] // BankToggle with a single "pinned" option
	}
}

// toggledOptions returns the labels of toggled pills in a BankToggle bank.
func (s FilterStrip) toggledOptions(bankIdx int, opts []string) []string {
	var result []string
	for j, opt := range opts {
		if s.toggled[bankIdx][j] {
			result = append(result, opt)
		}
	}
	return result
}

// singleOption returns the selected option label for a BankSingle bank, or "".
func (s FilterStrip) singleOption(bankIdx int, opts []string) string {
	idx := s.single[bankIdx]
	if idx >= 0 && idx < len(opts) {
		return opts[idx]
	}
	return ""
}

// sliderFloat parses the active stop label as a float64 for a BankSlider bank.
func (s FilterStrip) sliderFloat(bankIdx int, opts []string) float64 {
	idx := s.slider[bankIdx]
	if idx >= 0 && idx < len(opts) {
		if v, err := strconv.ParseFloat(opts[idx], 64); err == nil {
			return v
		}
	}
	return 0
}

// ── View ──────────────────────────────────────────────────────────────────────

// View renders the filter strip as a multi-row bordered box.
//
// Each bank occupies one row: "Label:  pill1  pill2  pill3".
// BankSlider banks render a segmented bar: "Score:  ┤■■■■□□□□□□□□□□□├  >= 0".
// When enabled and NO_COLOR is not set, the focused bank label is rendered in
// style.Accent() and the box border changes from FaintColor() to Accent().
// An empty banks slice returns "".
func (s FilterStrip) View(width int) string {
	if len(s.banks) == 0 {
		return ""
	}
	nc := !s.enabled || os.Getenv("NO_COLOR") != ""

	// innerW: content width inside border (2) and side-padding (2).
	innerW := width - 4
	if innerW < 20 {
		innerW = 20
	}

	// labelW: visible width of the widest "Label:" string plus a two-space gap.
	labelW := 0
	for _, b := range s.banks {
		if lw := ansi.StringWidth(b.Label) + 1; lw > labelW { // +1 for ":"
			labelW = lw
		}
	}
	labelW += 2 // two-space gap between label column and pill content

	rows := make([]string, len(s.banks))
	for bi, b := range s.banks {
		rows[bi] = s.renderBankRow(bi, b, labelW, innerW, nc)
	}

	if nc {
		return s.renderBorderedBoxNC(rows, innerW)
	}
	return s.renderBorderedBoxColor(rows, innerW)
}

// isActive returns true when pill pi in bank bi is currently selected/active.
func (s FilterStrip) isActive(bi, pi int) bool {
	b := s.banks[bi]
	switch b.Kind {
	case BankToggle:
		return s.toggled[bi][pi]
	case BankSingle:
		return s.single[bi] == pi
	case BankSlider:
		return s.slider[bi] == pi
	}
	return false
}

// pillCheck is the prefix marking a SELECTED (active) pill. Selection is shown
// by this glyph in every mode — never by colour alone — so "is this filter on?"
// is unambiguous even in a low-colour capture, and is visually separate from
// the focus cursor (brackets). An earlier design used brackets for BOTH focus
// (colour) and selection (no-colour), which read as the same thing and left
// users unsure whether toggling had taken effect.
const pillCheck = "✓ "

// renderPill renders one pill label. Two orthogonal states are encoded with two
// distinct, mode-independent affordances:
//
//   - SELECTED (active): a leading ✓ (and a filled chip in colour).
//   - FOCUSED (cursor):  surrounding [brackets] (and accent/bold in colour).
//
// No-color mode communicates both through text alone (accessibility):
//
//	active+focused → [✓ label]   focused → [label]
//	active         →  ✓ label    inactive →  label
func (s FilterStrip) renderPill(label string, active, focused, nc bool) string {
	text := label
	if active {
		text = pillCheck + label
	}
	if nc {
		if focused {
			return "[" + text + "]"
		}
		return text
	}
	switch {
	case active && focused:
		return lipgloss.NewStyle().
			Foreground(style.Paper()).
			Background(style.Accent()).
			Bold(true).
			Padding(0, 1).
			Render(text)
	case active:
		return lipgloss.NewStyle().
			Foreground(style.Paper()).
			Background(style.AccentDeep()).
			Padding(0, 1).
			Render(text)
	case focused:
		return lipgloss.NewStyle().
			Foreground(style.Accent()).
			Bold(true).
			Render("[" + label + "]")
	default:
		return lipgloss.NewStyle().
			Foreground(style.ANSIMuted()).
			Render(label)
	}
}

// ── View helpers ──────────────────────────────────────────────────────────────

// sliderStops is the number of fill cells in the segmented slider bar.
const sliderStops = 15

// renderBankRow renders one bank row: "Label:  pill1  pill2  pill3".
// For BankSlider it renders the segmented bar instead of discrete pills.
// The row is truncated to innerW visible columns.
func (s FilterStrip) renderBankRow(bi int, b Bank, labelW, innerW int, nc bool) string {
	bankFocused := s.focused && s.bankIdx == bi

	// Label ("Kind:", "Score:", …) padded to labelW visible columns.
	labelRaw := b.Label + ":"
	var label string
	if nc {
		label = padToWidth(labelRaw, labelW)
	} else if bankFocused {
		label = lipgloss.NewStyle().Foreground(style.Accent()).Bold(true).Render(labelRaw) +
			strings.Repeat(" ", labelW-ansi.StringWidth(labelRaw))
	} else {
		label = lipgloss.NewStyle().Foreground(style.SubtleColor()).Render(labelRaw) +
			strings.Repeat(" ", labelW-ansi.StringWidth(labelRaw))
	}

	// Pill row or slider row.
	var content string
	if b.Kind == BankSlider {
		content = s.renderSliderRow(bi, b, nc)
	} else {
		parts := make([]string, 0, len(b.Options))
		for pi, opt := range b.Options {
			isActive := s.isActive(bi, pi)
			isFocused := s.focused && s.bankIdx == bi && s.pillIdx == pi
			parts = append(parts, s.renderPill(opt, isActive, isFocused, nc))
		}
		content = strings.Join(parts, "  ")
	}

	row := label + content
	if ansi.StringWidth(row) > innerW {
		row = ansi.Truncate(row, innerW, "")
	}
	return row
}

// renderSliderRow renders the segmented-bar visual for a BankSlider bank:
//
//	NC:    |■■■■■□□□□□□□□□□|  >= 0.05
//	Color: ┤■■■■■□□□□□□□□□□├  >= 0.05   (bar in Accent() when focused)
//
// The bar reflects the committed stop (slider[bi]); on a slider ←/→ commit on
// move, so the cursor and the committed stop always agree — there is no
// separate preview state to drift out of sync.
func (s FilterStrip) renderSliderRow(bi int, b Bank, nc bool) string {
	if len(b.Options) == 0 {
		return ""
	}
	maxIdx := len(b.Options) - 1

	displayIdx := s.slider[bi]

	filled := 0
	if maxIdx > 0 {
		filled = (displayIdx * sliderStops) / maxIdx
	}
	if filled > sliderStops {
		filled = sliderStops
	}

	barRunes := strings.Repeat("■", filled) + strings.Repeat("□", sliderStops-filled)

	var valLabel string
	if displayIdx >= 0 && displayIdx < len(b.Options) {
		valLabel = ">= " + b.Options[displayIdx]
	}

	if nc {
		return "|" + barRunes + "|  " + valLabel
	}

	isFocused := s.focused && s.bankIdx == bi
	barColor := style.ANSIMuted()
	if isFocused {
		barColor = style.Accent()
	}
	barStr := lipgloss.NewStyle().Foreground(barColor).Render("┤" + barRunes + "├")
	valStr := lipgloss.NewStyle().Foreground(style.SubtleColor()).Render(valLabel)
	return barStr + "  " + valStr
}

// renderBorderedBoxNC wraps rows in a box-drawing border with a "filters"
// title in the top edge. No ANSI colour sequences are emitted.
func (s FilterStrip) renderBorderedBoxNC(rows []string, innerW int) string {
	// totalInner = innerW + 2: one space of side-padding on each side.
	totalInner := innerW + 2
	const titleText = " filters "
	topDashes := totalInner - ansi.StringWidth(titleText)
	if topDashes < 0 {
		topDashes = 0
	}
	topLine := "┌" + titleText + strings.Repeat("─", topDashes) + "┐"
	botLine := "└" + strings.Repeat("─", totalInner) + "┘"

	lines := make([]string, 0, len(rows)+2)
	lines = append(lines, topLine)
	for _, r := range rows {
		r = padToWidth(r, innerW)
		lines = append(lines, "│ "+r+" │")
	}
	lines = append(lines, botLine)
	return strings.Join(lines, "\n")
}

// renderBorderedBoxColor wraps rows in a rounded border coloured via the style
// package. The border is FaintColor() at rest and Accent() when focused.
// The "filters" title is embedded in the top edge.
func (s FilterStrip) renderBorderedBoxColor(rows []string, innerW int) string {
	// totalInner = innerW + 2: one space of side-padding on each side.
	totalInner := innerW + 2
	const titleText = " filters "
	topDashes := totalInner - ansi.StringWidth(titleText)
	if topDashes < 0 {
		topDashes = 0
	}

	borderColor := style.FaintColor()
	if s.focused {
		borderColor = style.Accent()
	}
	bSty := lipgloss.NewStyle().Foreground(borderColor)

	topLine := bSty.Render("╭" + titleText + strings.Repeat("─", topDashes) + "╮")
	botLine := bSty.Render("╰" + strings.Repeat("─", totalInner) + "╯")

	lines := make([]string, 0, len(rows)+2)
	lines = append(lines, topLine)
	for _, r := range rows {
		r = padToWidth(r, innerW)
		lines = append(lines, bSty.Render("│")+" "+r+" "+bSty.Render("│"))
	}
	lines = append(lines, botLine)
	return strings.Join(lines, "\n")
}

// padToWidth returns s padded with trailing spaces to reach exactly w visible
// columns. If s is already at or beyond w visible columns it is returned as-is.
func padToWidth(s string, w int) string {
	vis := ansi.StringWidth(s)
	if vis >= w {
		return s
	}
	return s + strings.Repeat(" ", w-vis)
}

// ── copy helpers ──────────────────────────────────────────────────────────────

// copyBoolMap returns a shallow copy of m.
func copyBoolMap(m toggleState) toggleState {
	out := make(toggleState, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cloneToggled returns a new []toggleState with each inner map shallow-copied.
func cloneToggled(src []toggleState) []toggleState {
	dst := make([]toggleState, len(src))
	for i, m := range src {
		dst[i] = copyBoolMap(m)
	}
	return dst
}
