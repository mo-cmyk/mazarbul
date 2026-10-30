// Package uiform is the single surface for every interactive prompt in the
// maz CLI. It owns theming (mapped to Mazarbul's orange palette), form
// construction, and gate-checked prompt helpers that silently short-circuit
// when running non-interactively (CI, piped, agent invocations).
package uiform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"charm.land/huh/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// Theme returns a huh.Theme built from Mazarbul's orange palette.
//
// It starts from huh.ThemeBase and overrides the accent-bearing elements:
// selection caret, focused title, multi-select "[•]" prefix, error indicators,
// and descriptions — all resolved through the style package so NO_COLOR is
// honoured automatically.
func Theme() huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		t := huh.ThemeBase(isDark)

		accent := style.Accent()
		subtle := style.SubtleColor()
		errC := style.ErrorColor()
		ink := style.Ink()
		faint := style.FaintColor()

		// Focused: accent-coloured titles, carets, and selection marks.
		t.Focused.Title = lipgloss.NewStyle().Foreground(accent)
		t.Focused.Description = lipgloss.NewStyle().Foreground(subtle)
		t.Focused.ErrorIndicator = lipgloss.NewStyle().Foreground(errC).SetString(" *")
		t.Focused.ErrorMessage = lipgloss.NewStyle().Foreground(errC)

		t.Focused.SelectSelector = lipgloss.NewStyle().Foreground(accent).SetString("> ")
		t.Focused.Option = lipgloss.NewStyle().Foreground(ink)

		t.Focused.MultiSelectSelector = lipgloss.NewStyle().Foreground(accent).SetString("> ")
		t.Focused.SelectedPrefix = lipgloss.NewStyle().Foreground(accent).SetString("[•] ")
		t.Focused.SelectedOption = lipgloss.NewStyle().Foreground(accent)
		t.Focused.UnselectedPrefix = lipgloss.NewStyle().Foreground(ink).SetString("[ ] ")
		t.Focused.UnselectedOption = lipgloss.NewStyle().Foreground(ink)

		t.Focused.FocusedButton = lipgloss.NewStyle().Foreground(accent).Bold(true).Padding(0, 2)
		t.Focused.BlurredButton = lipgloss.NewStyle().Foreground(faint).Padding(0, 2)
		t.Focused.Next = t.Focused.FocusedButton

		t.Focused.TextInput.Cursor = lipgloss.NewStyle().Foreground(accent)
		// Colour only — the bubbles textinput supplies its own "> " prompt, which
		// huh renders as promptStyle.Render(textinput.Prompt). Adding SetString
		// here would inject a second sigil and render "> > " (doubled prompt).
		t.Focused.TextInput.Prompt = lipgloss.NewStyle().Foreground(accent)
		t.Focused.TextInput.Placeholder = lipgloss.NewStyle().Foreground(faint)

		// Blurred: same structure, hidden border and no directional cursor.
		t.Blurred = t.Focused
		t.Blurred.Description = lipgloss.NewStyle().Foreground(subtle)
		t.Blurred.SelectSelector = lipgloss.NewStyle().SetString("  ")
		t.Blurred.MultiSelectSelector = lipgloss.NewStyle().SetString("  ")

		t.Group.Title = t.Focused.Title
		t.Group.Description = t.Focused.Description

		t.FieldSeparator = lipgloss.NewStyle().SetString("\n")

		return t
	})
}

// New builds a huh.Form with Mazarbul's theme applied to all groups.
// When the ACCESSIBLE or MAZ_ACCESSIBLE environment variable is non-empty,
// accessible (plain-text) mode is activated so screen-reader users get clean
// line-by-line output instead of the Bubble Tea renderer.
func New(groups ...*huh.Group) *huh.Form {
	f := huh.NewForm(groups...).WithTheme(Theme())
	if os.Getenv("ACCESSIBLE") != "" || os.Getenv("MAZ_ACCESSIBLE") != "" {
		f = f.WithAccessible(true)
	}
	return f
}

// HandleFormCancellation maps recognised exit conditions (user abort, timeout,
// context cancellation) to a clean "… cancelled." message on w and a nil
// error. Any other non-nil error is wrapped with the action name and returned.
func HandleFormCancellation(w io.Writer, action string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, huh.ErrUserAborted) ||
		errors.Is(err, huh.ErrTimeout) ||
		errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintf(w, "%s cancelled.\n", action) //nolint:errcheck // best-effort cancellation message
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

// PromptYN presents a yes/no confirmation. When the process is not running
// interactively (CI, piped, agent), def is returned immediately without
// blocking.
func PromptYN(w io.Writer, prompt string, def bool) (bool, error) {
	if !interactive.CanPromptInteractively() {
		return def, nil
	}
	var v bool
	f := New(huh.NewGroup(huh.NewConfirm().Title(prompt).Value(&v)))
	if err := f.Run(); err != nil {
		return v, HandleFormCancellation(w, "prompt", err)
	}
	return v, nil
}

// PromptSelect presents a single-choice selection. Non-interactive invocations
// return a descriptive error rather than blocking; callers should supply a
// flag-based default path for scripted use.
//
//nolint:ireturn // returns the type parameter T (constraint comparable); ireturn treats a generic return as an interface
func PromptSelect[T comparable](w io.Writer, prompt string, opts []huh.Option[T]) (T, error) {
	var zero T
	if !interactive.CanPromptInteractively() {
		return zero, fmt.Errorf("selection required: non-interactive invocation cannot prompt for %q", prompt)
	}
	var v T
	f := New(huh.NewGroup(huh.NewSelect[T]().Title(prompt).Options(opts...).Value(&v)))
	if err := f.Run(); err != nil {
		return zero, HandleFormCancellation(w, "select", err)
	}
	return v, nil
}

// PromptMultiSelect presents a multi-choice selection. Non-interactive
// invocations return a descriptive error; callers must provide flag-based
// alternatives for scripted use.
func PromptMultiSelect[T comparable](w io.Writer, prompt string, opts []huh.Option[T]) ([]T, error) {
	if !interactive.CanPromptInteractively() {
		return nil, fmt.Errorf("selection required: non-interactive invocation cannot prompt for %q", prompt)
	}
	var v []T
	f := New(huh.NewGroup(huh.NewMultiSelect[T]().Title(prompt).Options(opts...).Value(&v)))
	if err := f.Run(); err != nil {
		return nil, HandleFormCancellation(w, "multiselect", err)
	}
	return v, nil
}
