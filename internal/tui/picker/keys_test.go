package picker

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// TestKeyMatches verifies that key.Matches fires for the documented bindings.
func TestKeyMatches(t *testing.T) {
	km := DefaultKeyMap()

	cases := []struct {
		name    string
		msg     tea.KeyPressMsg
		binding key.Binding
		want    bool
	}{
		// Down
		{"j matches Down", tea.KeyPressMsg{Code: 'j'}, km.Down, true},
		{"down arrow matches Down", tea.KeyPressMsg{Code: tea.KeyDown}, km.Down, true},

		// Up
		{"k matches Up", tea.KeyPressMsg{Code: 'k'}, km.Up, true},
		{"up arrow matches Up", tea.KeyPressMsg{Code: tea.KeyUp}, km.Up, true},

		// Quit — two bindings on the same key.Binding
		{"q matches Quit", tea.KeyPressMsg{Code: 'q', Text: "q"}, km.Quit, true},
		{"ctrl+c matches Quit", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, km.Quit, true},

		// Toggle — space key (decoder always produces "space", never " ")
		{"space matches Toggle", tea.KeyPressMsg{Code: tea.KeySpace}, km.Toggle, true},

		// Navigation
		{"enter matches Confirm", tea.KeyPressMsg{Code: tea.KeyEnter}, km.Confirm, true},
		{"esc matches Back", tea.KeyPressMsg{Code: tea.KeyEscape}, km.Back, true},
		{"slash matches Search", tea.KeyPressMsg{Code: '/', Text: "/"}, km.Search, true},
		{"n matches NextPage", tea.KeyPressMsg{Code: 'n', Text: "n"}, km.NextPage, true},
		{"right arrow matches NextPage", tea.KeyPressMsg{Code: tea.KeyRight}, km.NextPage, true},
		{"p matches PrevPage", tea.KeyPressMsg{Code: 'p', Text: "p"}, km.PrevPage, true},
		{"left arrow matches PrevPage", tea.KeyPressMsg{Code: tea.KeyLeft}, km.PrevPage, true},
		{"g matches Home", tea.KeyPressMsg{Code: 'g', Text: "g"}, km.Home, true},
		{"home key matches Home", tea.KeyPressMsg{Code: tea.KeyHome}, km.Home, true},
		{"G matches End", tea.KeyPressMsg{Code: 'G', Text: "G"}, km.End, true},
		{"end key matches End", tea.KeyPressMsg{Code: tea.KeyEnd}, km.End, true},

		// Negative — wrong binding
		{"j does not match Up", tea.KeyPressMsg{Code: 'j'}, km.Up, false},
		{"q does not match Down", tea.KeyPressMsg{Code: 'q', Text: "q"}, km.Down, false},
		{"space does not match Quit", tea.KeyPressMsg{Code: tea.KeySpace}, km.Quit, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := key.Matches(tc.msg, tc.binding)
			if got != tc.want {
				t.Errorf("key.Matches(%q, binding %q) = %v, want %v",
					tc.msg.String(), tc.binding.Help().Key, got, tc.want)
			}
		})
	}
}

// TestHelp_NoColor confirms that Help() contains expected key-desc fragments
// and the " · " separator when NO_COLOR is set (plain-text mode).
func TestHelp_NoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	km := DefaultKeyMap()
	h := km.Help()

	if !strings.Contains(h, " · ") {
		t.Errorf("Help() missing separator ' · '; got: %q", h)
	}

	fragments := []string{
		"q quit",
		"esc back",
		"/ search",
		"enter select",
		"↑/k up",
		"↓/j down",
		"n/→ next page",
		"p/← prev page",
		"g/home top",
		"G/end bottom",
		"space toggle",
	}
	for _, want := range fragments {
		if !strings.Contains(h, want) {
			t.Errorf("Help() missing fragment %q; got: %q", want, h)
		}
	}
}

// TestHelp_DisabledBinding confirms that a disabled binding is omitted from Help() output.
func TestHelp_DisabledBinding(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	km := DefaultKeyMap()
	km.Search.SetEnabled(false)

	h := km.Help()
	if strings.Contains(h, "search") {
		t.Errorf("Help() should omit disabled Search binding; got: %q", h)
	}
	// All other bindings should still appear.
	if !strings.Contains(h, "quit") {
		t.Errorf("Help() should still contain Quit after disabling Search; got: %q", h)
	}
}
