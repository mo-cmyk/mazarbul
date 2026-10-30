// Package panel renders bordered content panels using the active theme.
package panel

import (
	"image/color"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// Kind selects the panel's color treatment.
type Kind int

const (
	// Neutral uses the accent color for the border.
	Neutral Kind = iota
	// Success uses the success color for the border.
	Success
	// Warning uses the warning color for the border.
	Warning
	// Error uses the error color for the border.
	Error
	// Info uses the info color for the border.
	Info
)

// Render returns content wrapped in a bordered panel.
func Render(kind Kind, content string) string {
	var borderColor color.Color
	switch kind {
	case Success:
		borderColor = style.SuccessColor()
	case Warning:
		borderColor = style.WarningColor()
	case Error:
		borderColor = style.ErrorColor()
	case Info:
		borderColor = style.InfoColor()
	default:
		borderColor = style.Accent()
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(1, 2).
		Render(content)
}
