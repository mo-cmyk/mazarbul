package watch

import "charm.land/bubbles/v2/key"

// KeyMap is the locked keymap for the watch dashboard.
type KeyMap struct {
	Quit        key.Binding
	FocusPane1  key.Binding
	FocusPane2  key.Binding
	FocusPane3  key.Binding
	FocusPane4  key.Binding
	FocusPane5  key.Binding
	ScrollUp    key.Binding
	ScrollDown  key.Binding
	Reload      key.Binding
	Help        key.Binding
	Search      key.Binding
	EventFilter key.Binding
	Pause       key.Binding
	Follow      key.Binding
}

// DefaultKeyMap returns the standard watch dashboard bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		FocusPane1: key.NewBinding(
			key.WithKeys("1"),
			key.WithHelp("1", "events"),
		),
		FocusPane2: key.NewBinding(
			key.WithKeys("2"),
			key.WithHelp("2", "jobs"),
		),
		FocusPane3: key.NewBinding(
			key.WithKeys("3"),
			key.WithHelp("3", "embedding"),
		),
		FocusPane4: key.NewBinding(
			key.WithKeys("4"),
			key.WithHelp("4", "sync"),
		),
		FocusPane5: key.NewBinding(
			key.WithKeys("5"),
			key.WithHelp("5", "sources"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "scroll up"),
		),
		ScrollDown: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "scroll down"),
		),
		Reload: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "reload"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Search: key.NewBinding(
			key.WithKeys("/"),
			key.WithHelp("/", "search"),
		),
		EventFilter: key.NewBinding(
			key.WithKeys("e"),
			key.WithHelp("e", "event filter"),
		),
		Pause: key.NewBinding(
			key.WithKeys("p"),
			key.WithHelp("p", "pause/resume"),
		),
		Follow: key.NewBinding(
			key.WithKeys("f"),
			key.WithHelp("f", "follow latest"),
		),
	}
}

// ShortHelp returns the minimal keymap shown in the status bar.
func (k KeyMap) ShortHelp() string {
	return "? help · q quit · 1-5 focus · r reload · / search"
}

// FullHelp returns all bindings grouped for the overlay.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Help, k.Quit},
		{k.FocusPane1, k.FocusPane2, k.FocusPane3, k.FocusPane4, k.FocusPane5},
		{k.ScrollUp, k.ScrollDown},
		{k.Reload, k.Pause, k.Follow},
		{k.Search, k.EventFilter},
	}
}
