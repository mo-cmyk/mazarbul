// Package picker provides the shared keymap for the maz picker TUI screen.
//
// Every binding carries both match keys (used by key.Matches) and help text
// (rendered in footers) so the two never drift apart. Import this package to
// get a consistent keyset across any picker-flavoured screen.
package picker

import (
	"strings"

	"charm.land/bubbles/v2/key"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// KeyMap holds all key bindings for the picker TUI screen.
// Every field carries both match keys and help text so they stay in sync.
type KeyMap struct {
	// Quit exits the picker unconditionally.
	Quit key.Binding
	// Back dismisses the current overlay or returns to the parent screen.
	Back key.Binding
	// Search opens the inline search filter.
	Search key.Binding
	// Confirm accepts the current selection.
	Confirm key.Binding
	// Up moves the cursor one row up.
	Up key.Binding
	// Down moves the cursor one row down.
	Down key.Binding
	// NextPage advances to the next result page.
	NextPage key.Binding
	// PrevPage returns to the previous result page.
	PrevPage key.Binding
	// Home jumps to the first item.
	Home key.Binding
	// End jumps to the last item.
	End key.Binding
	// Toggle marks or unmarks the item under the cursor.
	Toggle key.Binding
}

// DefaultKeyMap returns the standard key bindings for the picker screen.
//
// Keys and help text are co-located in each binding so they can never drift.
// The space key uses the canonical "space" string produced by the Kitty /
// ultraviolet key decoder — terminals always emit "space", never " ".
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "back"),
		),
		Search: key.NewBinding(
			key.WithKeys("/"),
			key.WithHelp("/", "search"),
		),
		Confirm: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "select"),
		),
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		NextPage: key.NewBinding(
			key.WithKeys("n", "right"),
			key.WithHelp("n/→", "next page"),
		),
		PrevPage: key.NewBinding(
			key.WithKeys("p", "left"),
			key.WithHelp("p/←", "prev page"),
		),
		Home: key.NewBinding(
			key.WithKeys("g", "home"),
			key.WithHelp("g/home", "top"),
		),
		End: key.NewBinding(
			key.WithKeys("G", "end"),
			key.WithHelp("G/end", "bottom"),
		),
		Toggle: key.NewBinding(
			key.WithKeys("space"),
			key.WithHelp("space", "toggle"),
		),
	}
}

// Help renders all enabled bindings as a single footer line of the form
// "key desc · key desc · …". The separator " · " and the key labels are
// both styled with style.ANSIMuted(), which collapses to plain text when
// NO_COLOR is set so assertions remain stable in tests.
func (k KeyMap) Help() string {
	sep := lipgloss.NewStyle().Foreground(style.ANSIMuted()).Render(" · ")
	label := lipgloss.NewStyle().Foreground(style.ANSIMuted())

	bindings := []key.Binding{
		k.Quit, k.Back, k.Search, k.Confirm,
		k.Up, k.Down, k.NextPage, k.PrevPage,
		k.Home, k.End, k.Toggle,
	}

	parts := make([]string, 0, len(bindings))
	for _, b := range bindings {
		if !b.Enabled() {
			continue
		}
		h := b.Help()
		parts = append(parts, label.Render(h.Key)+" "+h.Desc)
	}
	return strings.Join(parts, sep)
}
