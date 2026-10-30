// Package style is the single source of truth for Mazarbul TUI visuals.
//
// Mazarbul ships one two-tier palette — a bright orange brand accent with
// git-style semantic type hues, over structural chrome addressed by ANSI
// index — and honours NO_COLOR by collapsing every adaptive colour to
// lipgloss.NoColor at render time. There is no theme registry, no user
// theme files; if alternate palettes are wanted later, they slot in here.
//
// Files outside this package should call the typed Style helpers
// (Brand, Heading, Subtle, …) or the colour accessors (Accent, Ink, …)
// rather than reaching for raw hex literals.
package style

import lipgloss "charm.land/lipgloss/v2"

// Brand returns the primary brand style (accent, bold).
func Brand() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(Accent()).Bold(true)
}

// Heading returns a bold ink style for headings.
func Heading() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(Ink()).Bold(true)
}

// Subtle returns a secondary-text style.
func Subtle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(SubtleColor())
}

// Faint returns a tertiary-text style.
func Faint() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(FaintColor())
}

// Success returns a success-state style.
func Success() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(SuccessColor()).Bold(true)
}

// Warning returns a warning-state style.
func Warning() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(WarningColor())
}

// Error returns an error-state style.
func Error() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(ErrorColor()).Bold(true)
}

// Info returns an info-state style.
func Info() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(InfoColor())
}

// Code returns a styled inline-code presentation.
func Code() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(Accent()).Background(Paper())
}

// Border returns the standard rounded border used by panels.
func Border() lipgloss.Border {
	return lipgloss.RoundedBorder()
}

// PanelStyle returns a bordered panel style using accent for the border.
func PanelStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(Border()).
		BorderForeground(Accent()).
		Padding(1, 2)
}
