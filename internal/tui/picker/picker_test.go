package picker

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// ── helpers ────────────────────────────────────────────────────────────────────

// titleOnly returns a DefaultRenderer that uses only titles (no subtitle).
func titleOnly() RowRenderer[string] {
	return DefaultRenderer(
		func(s string) string { return s },
		nil,
	)
}

// newStrings builds a Model[string] wired for title-matching, NO_COLOR on.
func newStrings(t *testing.T, items ...string) Model[string] {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	return New(items, titleOnly()).
		WithTitleOf(func(s string) string { return s })
}

// pressKey drives one Update and returns the updated model.
func pressKey(t *testing.T, m Model[string], msg tea.KeyPressMsg) Model[string] {
	t.Helper()
	updated, _ := m.Update(msg)
	return updated
}

// ── tests ──────────────────────────────────────────────────────────────────────

func TestRenders_AllItems(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "alpha", "beta")
	v := m.View()
	if !strings.Contains(v, "alpha") {
		t.Errorf("View() missing 'alpha'; got:\n%s", v)
	}
	if !strings.Contains(v, "beta") {
		t.Errorf("View() missing 'beta'; got:\n%s", v)
	}
}

func TestDown_MovesCursor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "alpha", "beta")
	// Initial cursor is on alpha (▸ alpha).
	if !strings.Contains(m.View(), "▸") {
		t.Fatal("initial view missing caret ▸")
	}

	m = pressKey(t, m, tea.KeyPressMsg{Code: 'j'})

	v := m.View()
	// After moving down, caret should precede "beta" on the same line.
	for _, line := range strings.Split(v, "\n") {
		if strings.Contains(line, "▸") && strings.Contains(line, "beta") {
			return // found
		}
	}
	t.Errorf("after j, ▸ should be on 'beta' line; got:\n%s", v)
}

func TestEnter_ChoosesCursorItem(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "alpha", "beta")

	// Move down to beta.
	m = pressKey(t, m, tea.KeyPressMsg{Code: 'j'})
	// Confirm.
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	item, ok := m.Chosen()
	if !ok {
		t.Fatal("Chosen() returned false after Enter")
	}
	if item != "beta" {
		t.Errorf("Chosen() = %q, want %q", item, "beta")
	}
}

func TestSpace_TogglesMultiSelect(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "alpha", "beta").WithMultiSelect(true)

	// First space marks alpha.
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeySpace})
	sel := m.Selected()
	if len(sel) != 1 || sel[0] != "alpha" {
		t.Errorf("after first space, Selected() = %v, want [alpha]", sel)
	}

	// Second space unmarks alpha.
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeySpace})
	sel = m.Selected()
	if len(sel) != 0 {
		t.Errorf("after second space, Selected() = %v, want []", sel)
	}
}

func TestNoEscapes_WhenDisabled(t *testing.T) {
	// Do NOT set NO_COLOR — we want to verify that WithEnabled(false) alone
	// strips all escape sequences from View().
	os.Unsetenv("NO_COLOR")
	t.Cleanup(func() { os.Unsetenv("NO_COLOR") })

	m := New([]string{"alpha", "beta"}, titleOnly()).WithEnabled(false)
	v := m.View()
	if strings.Contains(v, "\x1b[") {
		t.Errorf("View() with enabled=false must not contain ANSI escapes; got:\n%s", v)
	}
}

func TestLines_RespectWidthCap(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "alpha", "beta").WithSize(80, 20)
	v := m.View()
	for _, line := range strings.Split(v, "\n") {
		w := ansi.StringWidth(line)
		if w > 80 {
			t.Errorf("line width %d > 80: %q", w, line)
		}
	}
}

func TestHome_JumpsToFirst(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "alpha", "beta", "gamma")

	// Move to last item.
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	// Home returns to first.
	m = pressKey(t, m, tea.KeyPressMsg{Code: 'g'})

	for _, line := range strings.Split(m.View(), "\n") {
		if strings.Contains(line, "▸") && strings.Contains(line, "alpha") {
			return
		}
	}
	t.Errorf("after g, ▸ should be on 'alpha'; got:\n%s", m.View())
}

func TestEnd_JumpsToLast(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "alpha", "beta", "gamma")

	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})

	for _, line := range strings.Split(m.View(), "\n") {
		if strings.Contains(line, "▸") && strings.Contains(line, "gamma") {
			return
		}
	}
	t.Errorf("after End, ▸ should be on 'gamma'; got:\n%s", m.View())
}

func TestQuit_SetsQuittingFlag(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "alpha")
	m = pressKey(t, m, tea.KeyPressMsg{Code: 'q', Text: "q"})
	if !m.Quitting() {
		t.Error("Quitting() should be true after q")
	}
}

func TestSearch_FiltersItems(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "apple", "banana", "apricot")

	// Enter search mode with /.
	m = pressKey(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	if !m.searching {
		t.Fatal("should be in search mode after /")
	}

	// Type 'a', 'p' — should match apple and apricot but not banana.
	m = pressKey(t, m, tea.KeyPressMsg{Code: 'a', Text: "a"})
	m = pressKey(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})

	v := m.View()
	if !strings.Contains(v, "apple") {
		t.Errorf("filtered view should contain 'apple'; got:\n%s", v)
	}
	if !strings.Contains(v, "apricot") {
		t.Errorf("filtered view should contain 'apricot'; got:\n%s", v)
	}
	if strings.Contains(v, "banana") {
		t.Errorf("filtered view should NOT contain 'banana'; got:\n%s", v)
	}
}

func TestSearch_EscResetsFilter(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "apple", "banana")

	m = pressKey(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m = pressKey(t, m, tea.KeyPressMsg{Code: 'z', Text: "z"}) // no match
	// Esc clears filter.
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})

	if m.searching {
		t.Error("should have exited search mode after Esc")
	}
	v := m.View()
	if !strings.Contains(v, "apple") || !strings.Contains(v, "banana") {
		t.Errorf("after Esc all items should be visible; got:\n%s", v)
	}
}

func TestAccessible_NumberedList(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("ACCESSIBLE", "1")
	m := newStrings(t, "alpha", "beta")
	v := m.View()
	if !strings.Contains(v, "1. ") {
		t.Errorf("accessible view should start with '1. '; got:\n%s", v)
	}
	if !strings.Contains(v, "2. ") {
		t.Errorf("accessible view should have '2. '; got:\n%s", v)
	}
	if strings.Contains(v, "\x1b[") {
		t.Errorf("accessible view must not contain ANSI escapes; got:\n%s", v)
	}
}

func TestSelected_MultiItemToggle(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "alpha", "beta", "gamma").WithMultiSelect(true)

	// Toggle alpha.
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeySpace})
	// Move to beta, toggle.
	m = pressKey(t, m, tea.KeyPressMsg{Code: 'j'})
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeySpace})

	sel := m.Selected()
	if len(sel) != 2 {
		t.Fatalf("Selected() len = %d, want 2; got %v", len(sel), sel)
	}
	if sel[0] != "alpha" || sel[1] != "beta" {
		t.Errorf("Selected() = %v, want [alpha beta]", sel)
	}
}

func TestInit_ReturnsNil(t *testing.T) {
	m := newStrings(t, "alpha")
	if m.Init() != nil {
		t.Error("Init() should return nil for a component model")
	}
}

func TestWindowSize_UpdatesDimensions(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newStrings(t, "alpha")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if updated.width != 120 || updated.height != 40 {
		t.Errorf("after WindowSizeMsg, width=%d height=%d, want 120 40", updated.width, updated.height)
	}
}
