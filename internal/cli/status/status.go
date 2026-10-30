// Package status renders symbol-first status lines for the maz CLI.
//
// Every method emits a glyph that carries the semantic meaning; colour is
// applied on top as an accelerator. When the caller passes enabled=false all
// ANSI escape sequences are stripped so the output is safe for scripted
// pipelines, --no-tui, and --json contexts.
//
// Example:
//
//	p := status.New(os.Stdout, interactive.ShouldStyle(cmd, os.Stdout))
//	fmt.Println(p.SuccessBullet("migration applied"))
//	fmt.Println(p.SectionRule("Connected sources"))
//	fmt.Println(p.IdentityBullet("daemon", "01ABCDEF"))
package status

import (
	"fmt"
	"io"
	"os"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// Printer renders symbol-first status lines. Obtain one via New.
// All methods use pointer receivers and store styles as pointers to avoid
// copying the heavy lipgloss.Style value (648 bytes each).
type Printer struct {
	enabled bool
	width   int

	// Prebuilt styles stored as pointers. When !enabled, paint() returns its
	// input unchanged so none of these are applied; they are populated
	// regardless so the struct stays consistent.
	accentStyle  *lipgloss.Style
	inkBoldStyle *lipgloss.Style
	successStyle *lipgloss.Style
	errorStyle   *lipgloss.Style
	warnStyle    *lipgloss.Style
	dimStyle     *lipgloss.Style
	mutedStyle   *lipgloss.Style
}

// New returns a *Printer writing to w with colour gated by enabled.
//
// Width detection: if w is a *os.File that is a TTY, term.GetSize is called and
// the detected width is capped at 80. Otherwise the fallback is 60.
func New(w io.Writer, enabled bool) *Printer {
	width := 60
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if detected, _, err := term.GetSize(int(f.Fd())); err == nil && detected > 0 {
			width = min(detected, 80)
		}
	}

	accent := lipgloss.NewStyle().Foreground(style.Accent())
	inkBold := lipgloss.NewStyle().Foreground(style.Ink()).Bold(true)
	success := lipgloss.NewStyle().Foreground(style.ANSISuccess())
	errSt := lipgloss.NewStyle().Foreground(style.ANSIError()).Bold(true)
	warn := lipgloss.NewStyle().Foreground(style.ANSIWarning())
	dim := lipgloss.NewStyle().Foreground(style.ANSIRule())
	muted := lipgloss.NewStyle().Foreground(style.ANSIMuted())

	return &Printer{
		enabled:      enabled,
		width:        width,
		accentStyle:  &accent,
		inkBoldStyle: &inkBold,
		successStyle: &success,
		errorStyle:   &errSt,
		warnStyle:    &warn,
		dimStyle:     &dim,
		mutedStyle:   &muted,
	}
}

// paint applies st.Render to s when colour is enabled; otherwise returns s
// unchanged. This is the single chokepoint that enforces the enabled gate.
func (p *Printer) paint(st *lipgloss.Style, s string) string {
	if !p.enabled {
		return s
	}
	return st.Render(s)
}

// IdentityBullet renders "● label id". The glyph (●) and the id are coloured
// with Accent; the label is coloured with Ink bold.
func (p *Printer) IdentityBullet(label, id string) string {
	return p.paint(p.accentStyle, "●") +
		" " + p.paint(p.inkBoldStyle, label) +
		" " + p.paint(p.accentStyle, id)
}

// SuccessBullet renders "✓ msg". The glyph is coloured with ANSISuccess.
func (p *Printer) SuccessBullet(msg string) string {
	return p.paint(p.successStyle, "✓") + " " + msg
}

// FailureBullet renders "✗ msg". The glyph is coloured with ANSIError bold.
func (p *Printer) FailureBullet(msg string) string {
	return p.paint(p.errorStyle, "✗") + " " + msg
}

// WarnBullet renders "⚠ msg". The glyph is coloured with ANSIWarning.
func (p *Printer) WarnBullet(msg string) string {
	return p.paint(p.warnStyle, "⚠") + " " + msg
}

// MetadataRows renders a two-column table. Each label is left-padded to 7
// characters (%-7s) with a single space before the value. Multi-line values
// receive a hanging indent aligned to the value column.
//
// Colour: labels are rendered with ANSIMuted. Values are unstyled.
func (p *Printer) MetadataRows(rows [][2]string) string {
	const colWidth = 7 // minimum label column width (%-7s)
	indent := strings.Repeat(" ", colWidth+1)

	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		label, value := row[0], row[1]
		paddedLabel := fmt.Sprintf("%-7s", label)
		valueParts := strings.Split(value, "\n")

		line := p.paint(p.mutedStyle, paddedLabel) + " " + valueParts[0]
		parts = append(parts, line)

		for _, cont := range valueParts[1:] {
			parts = append(parts, indent+cont)
		}
	}
	return strings.Join(parts, "\n")
}

// SectionRule renders "── label ──" followed by "─" dashes that pad the line
// to the cached width. The entire rule is coloured with ANSIRule (dim).
func (p *Printer) SectionRule(label string) string {
	const leftPad = "── "
	const rightPad = " ──"

	usedWidth := ansi.StringWidth(leftPad) + ansi.StringWidth(label) + ansi.StringWidth(rightPad)
	fillCount := p.width - usedWidth
	if fillCount < 0 {
		fillCount = 0
	}

	plain := leftPad + label + rightPad + strings.Repeat("─", fillCount)
	return p.paint(p.dimStyle, plain)
}

// HorizontalRule renders a full-width "─" line to the cached width, coloured
// with ANSIRule (dim).
func (p *Printer) HorizontalRule() string {
	plain := strings.Repeat("─", p.width)
	return p.paint(p.dimStyle, plain)
}
