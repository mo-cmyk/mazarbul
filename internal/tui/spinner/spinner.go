// Package spinner wraps bubbles/spinner with Mazarbul defaults.
package spinner

import (
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// Kind selects the spinner animation.
type Kind int

const (
	// Moon is a phased lunar spinner. Use for thinking or long waits;
	// fits the project's Khazad-dum tone.
	Moon Kind = iota
	// Dot is a compact spinner. Use for short waits under one second.
	Dot
	// Globe is a wide rotating globe. Use for sync or remote work.
	Globe
	// Pulse is a single character pulsing. Use for ambient activity.
	Pulse
)

// Model wraps a bubbles spinner with a labeled view.
type Model struct {
	sp    spinner.Model
	label string
}

// New returns a spinner model with kind and label.
func New(kind Kind, label string) Model {
	sp := spinner.New()
	sp.Spinner = mapKind(kind)
	sp.Style = style.Brand()
	return Model{sp: sp, label: label}
}

// Init starts the spinner ticker.
func (m Model) Init() tea.Cmd { return m.sp.Tick }

// Update advances the spinner.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	var cmd tea.Cmd
	m.sp, cmd = m.sp.Update(msg)
	return m, cmd
}

// View renders the spinner glyph followed by the label.
func (m Model) View() string {
	if m.label == "" {
		return m.sp.View()
	}
	return m.sp.View() + " " + style.Subtle().Render(m.label)
}

// SetLabel updates the label text.
func (m *Model) SetLabel(s string) { m.label = s }

func mapKind(k Kind) spinner.Spinner {
	switch k {
	case Dot:
		return spinner.Dot
	case Globe:
		return spinner.Globe
	case Pulse:
		return spinner.Pulse
	default:
		return spinner.Moon
	}
}
