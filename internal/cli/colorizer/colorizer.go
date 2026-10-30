// Package colorizer renders git-style semantic colour for the maz CLI.
//
// Colour is symbol-first: it is an accelerator layered on glyphs, never the
// sole carrier of meaning. Every colour is sourced from internal/tui/style
// (no raw escapes), so the palette stays themeable and NO_COLOR-aware. The
// caller decides whether colour is on (via internal/cli/interactive.ShouldStyle)
// and passes that in, keeping this package decoupled from flag/TTY plumbing.
package colorizer

import (
	"image/color"
	"io"
	"os"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/mo-cmyk/mazarbul/internal/tui/style"
)

// Colorizer renders semantic colour when enabled. When disabled, every method
// returns its input unchanged so output is byte-identical to the plain form.
type Colorizer struct {
	enabled bool
	profile colorprofile.Profile
}

// New returns a Colorizer whose colours are down-sampled to the profile detected
// for w (the stream the rendered output is destined for). When colour is forced
// on (e.g. --color=always) while w is not a colour TTY, full truecolour is
// emitted and the consumer down-samples. Callers that render into an in-memory
// buffer before writing to the real stream should pass that destination stream
// (or os.Stdout) so the profile matches where the bytes ultimately land.
func New(enabled bool, w io.Writer) *Colorizer {
	p := colorprofile.Detect(w, os.Environ())
	if enabled && p < colorprofile.ANSI {
		p = colorprofile.TrueColor
	}
	return &Colorizer{enabled: enabled, profile: p}
}

// paint wraps s in col (optionally bold) when enabled, down-sampling col to the
// resolved profile; otherwise returns s unchanged.
func (c *Colorizer) paint(s string, col color.Color, bold bool) string {
	if !c.enabled {
		return s
	}
	st := lipgloss.NewStyle().Foreground(c.profile.Convert(col))
	if bold {
		st = st.Bold(true)
	}
	return st.Render(s)
}

// Added marks inserted/created content (green).
func (c *Colorizer) Added(s string) string { return c.paint(s, style.DiffAddedColor(), false) }

// Removed marks removed/forgotten content (red).
func (c *Colorizer) Removed(s string) string { return c.paint(s, style.DiffRemovedColor(), false) }

// Changed marks modified content (amber).
func (c *Colorizer) Changed(s string) string { return c.paint(s, style.AmberColor(), false) }

// Superseded marks superseded content (purple).
func (c *Colorizer) Superseded(s string) string { return c.paint(s, style.PurpleColor(), false) }

// Conflict marks a conflict (red, bold).
func (c *Colorizer) Conflict(s string) string { return c.paint(s, style.ANSIError(), true) }

// Ident marks an identifier such as a ULID or UID (amber).
func (c *Colorizer) Ident(s string) string { return c.paint(s, style.AmberColor(), false) }

// Meta marks metadata and structure (cyan).
func (c *Colorizer) Meta(s string) string { return c.paint(s, style.CyanColor(), false) }

// Dim marks dim/secondary text (ANSI 8).
func (c *Colorizer) Dim(s string) string { return c.paint(s, style.ANSIDim(), false) }

// Accent marks the active/current element (orange; reserved, never "success").
func (c *Colorizer) Accent(s string) string { return c.paint(s, style.Accent(), false) }

// Success marks a success state (ANSI green).
func (c *Colorizer) Success(s string) string { return c.paint(s, style.ANSISuccess(), false) }

// Warn marks a warning state (ANSI yellow).
func (c *Colorizer) Warn(s string) string { return c.paint(s, style.ANSIWarning(), false) }

// Err marks an error state (ANSI red).
func (c *Colorizer) Err(s string) string { return c.paint(s, style.ANSIError(), false) }
