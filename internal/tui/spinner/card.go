package spinner

import (
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// Card is a small Bubble Tea component that shows a spinning MiniDot beside a
// label and a dimmed subtitle line. It is suitable for embedding inside a
// larger model to indicate ongoing background work (indexing, syncing, etc.).
//
// Card is a value type; embed it in your model struct and forward tea messages
// via Update.
type Card struct {
	sp       spinner.Model
	label    string
	subtitle string
	width    int
}

// NewCard returns a Card with the given label, subtitle, and container width.
// The spinner is pre-styled with the brand accent color.
func NewCard(label, subtitle string, width int) Card {
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = lipgloss.NewStyle().Foreground(style.Accent()).Bold(true)
	return Card{
		sp:       sp,
		label:    label,
		subtitle: subtitle,
		width:    width,
	}
}

// Init returns the spinner tick command. Wire this into your model's Init.
func (c Card) Init() tea.Cmd { return c.sp.Tick }

// Update advances the spinner on every message. Wire this into your model's
// Update alongside your own message handling.
func (c Card) Update(msg tea.Msg) (Card, tea.Cmd) {
	var cmd tea.Cmd
	c.sp, cmd = c.sp.Update(msg)
	return c, cmd
}

// View renders the card as a two-line borderless block:
//
//	⠋ label
//	  subtitle (dimmed)
//
// Width is clamped to [44, 76] after subtracting 8 for interior padding so
// the card never overflows a narrow terminal or becomes unwieldy in a wide one.
func (c Card) View() string {
	w := min(max(c.width-8, 44), 76)

	line1 := c.sp.View() + " " + c.label
	line2 := lipgloss.NewStyle().Foreground(style.ANSIMuted()).Render(c.subtitle)

	return lipgloss.NewStyle().
		Width(w).
		Render(line1 + "\n" + line2)
}
