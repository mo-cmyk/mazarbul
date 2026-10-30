// Package mdrender renders Markdown to styled terminal output using glamour/v2.
//
// Render applies the Mazarbul colour palette (orange accent, cyan/indigo type
// hues, amber literals) as a delta on top of glamour's built-in dark/light
// configs.  RenderForWriter is the higher-level entry point that short-circuits
// to raw text for non-TTY writers and when NO_COLOR is set.
//
// # Palette source
//
// All hex values mirror internal/tui/style/colors.go exactly.  Dark-theme
// values: accent #fb923c, cyan #22d3ee, indigo #818cf8, amber #fbbf24.
// Light-theme values: accent #f97316, cyan #06b6d4, indigo #6366f1, amber #f59e0b.
package mdrender

import (
	"io"
	"os"
	"sync"

	glamour "charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	glamstyles "charm.land/glamour/v2/styles"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	chromastyles "github.com/alecthomas/chroma/v2/styles"
	"golang.org/x/term"
)

const maxWidth = 80

// mazChromaStyleName is the name of our pre-registered code-block syntax theme.
//
// We register this style ourselves (see registerChromaStyle) and point
// StyleCodeBlock.Theme at it while leaving StyleCodeBlock.Chroma nil. That keeps
// glamour's CodeBlockElement.Render out of its style-registration branch — that
// branch (codeblock.go) calls chroma.MustNewStyle and then Unlocks a
// package-level mutex WITHOUT a defer, so any panic there (which a custom Chroma
// StyleConfig with a single bad entry will trigger) leaks the mutex and
// DEADLOCKS every subsequent code-block render in the process. Registering via
// chroma.NewStyle — which returns an error rather than panicking — under our own
// name sidesteps the bug entirely while preserving the palette.
const mazChromaStyleName = "mazarbul"

// registerChromaOnce guards one-time registration of the code-block syntax theme.
var registerChromaOnce sync.Once

// registerChromaStyle registers the Mazarbul code-block syntax theme exactly
// once. On any registration error it returns silently; glamour's quick.Highlight
// then falls back to a built-in chroma style — never a panic, never a deadlock.
//
// Hex values mirror colors.go (indigo keywords, cyan functions, amber literals,
// green/red diff). One dark-leaning style serves both backgrounds, matching
// glamour's own single-name code-block model.
func registerChromaStyle() {
	registerChromaOnce.Do(func() {
		st, err := chroma.NewStyle(mazChromaStyleName, chroma.StyleEntries{
			chroma.Text:                "#C4C4C4",
			chroma.Error:               "#F1F1F1 bg:#F05B5B",
			chroma.Comment:             "#676767",
			chroma.CommentPreproc:      "#FF875F",
			chroma.Keyword:             "#818cf8",
			chroma.KeywordReserved:     "#818cf8",
			chroma.KeywordNamespace:    "#818cf8",
			chroma.KeywordType:         "#818cf8",
			chroma.NameFunction:        "#22d3ee",
			chroma.NameBuiltin:         "#22d3ee",
			chroma.LiteralNumber:       "#fbbf24",
			chroma.LiteralString:       "#fbbf24",
			chroma.LiteralStringEscape: "#fbbf24",
			chroma.GenericInserted:     "#22C55E",
			chroma.GenericDeleted:      "#EF4444",
			chroma.GenericEmph:         "italic",
			chroma.GenericStrong:       "bold",
			chroma.GenericSubheading:   "#8a8a8a",
			chroma.Background:          "bg:#1e1e2e",
		})
		if err != nil {
			return
		}
		chromastyles.Register(st)
	})
}

// boolPtr / strPtr are local pointer helpers; glamour's StyleConfig fields are
// mostly pointer-typed so that zero values mean "unset / inherit".
func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// mazDarkTheme returns the Mazarbul dark StyleConfig.
//
// It starts from a value copy of glamour's DarkStyleConfig (replacing pointer
// fields does NOT mutate the package-level original) and overrides the Mazarbul
// palette. Code-block highlighting is delegated to the pre-registered
// "mazarbul" chroma style (Chroma nil, Theme set) — see registerChromaStyle.
func mazDarkTheme() ansi.StyleConfig {
	cfg := glamstyles.DarkStyleConfig // value copy; pointer fields are replaced below

	cfg.Document.Color = strPtr("252")

	// H1: accent orange (strip the default background block).
	cfg.H1.Color = strPtr("#fb923c")
	cfg.H1.BackgroundColor = nil
	cfg.H1.Bold = boolPtr(true)

	// H2: cyan.
	cfg.H2.Color = strPtr("#22d3ee")
	cfg.H2.Bold = boolPtr(true)

	// H3: indigo.
	cfg.H3.Color = strPtr("#818cf8")
	cfg.H3.Bold = boolPtr(true)

	// Links: cyan, underlined.
	cfg.Link.Color = strPtr("#22d3ee")
	cfg.LinkText.Color = strPtr("#22d3ee")
	cfg.Link.Underline = boolPtr(true)

	// Enumerations: indigo. List bullets / inline code: accent.
	cfg.Enumeration.Color = strPtr("#818cf8")
	cfg.Item.Color = strPtr("#fb923c")
	cfg.Code.Color = strPtr("#fb923c")
	cfg.Code.BackgroundColor = strPtr("236")

	cfg.HorizontalRule.Color = strPtr("240")

	// Code blocks: use the pre-registered "mazarbul" style; keep Chroma nil so
	// glamour never enters its mutex-guarded MustNewStyle branch.
	cfg.CodeBlock.Chroma = nil
	cfg.CodeBlock.Theme = mazChromaStyleName

	return cfg
}

// mazLightTheme returns the Mazarbul light StyleConfig.
//
// Uses the one-step-darker siblings of the dark palette so the brand reads
// equally well on light backgrounds; hex values match colors.go light entries.
// Code-block highlighting shares the single "mazarbul" chroma style.
func mazLightTheme() ansi.StyleConfig {
	cfg := glamstyles.LightStyleConfig // value copy

	cfg.Document.Color = strPtr("234")

	cfg.H1.Color = strPtr("#f97316")
	cfg.H1.BackgroundColor = nil
	cfg.H1.Bold = boolPtr(true)

	cfg.H2.Color = strPtr("#06b6d4")
	cfg.H2.Bold = boolPtr(true)

	cfg.H3.Color = strPtr("#6366f1")
	cfg.H3.Bold = boolPtr(true)

	cfg.Link.Color = strPtr("#06b6d4")
	cfg.LinkText.Color = strPtr("#06b6d4")
	cfg.Link.Underline = boolPtr(true)

	cfg.Enumeration.Color = strPtr("#6366f1")
	cfg.Item.Color = strPtr("#f97316")
	cfg.Code.Color = strPtr("#f97316")
	cfg.Code.BackgroundColor = strPtr("254")

	cfg.HorizontalRule.Color = strPtr("240")

	cfg.CodeBlock.Chroma = nil
	cfg.CodeBlock.Theme = mazChromaStyleName

	return cfg
}

// Render renders md to an ANSI-styled string using the Mazarbul theme.
//
// Dark selects the dark-palette variant (true) or the light variant (false).
// Width is clamped: values <=0 or >80 are silently treated as 80.
//
// Any panic inside glamour is caught by the deferred recover; the raw md is
// returned instead so callers always receive a non-panic result.
func Render(md string, width int, dark bool) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = md
		}
	}()

	registerChromaStyle()

	if width <= 0 || width > maxWidth {
		width = maxWidth
	}

	cfg := mazDarkTheme()
	if !dark {
		cfg = mazLightTheme()
	}

	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(cfg),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return md
	}

	rendered, err := r.Render(md)
	if err != nil {
		return md
	}
	return rendered
}

// darkOnce captures the terminal background polarity a single time before any
// Bubble Tea program starts.  Defaults to dark (true) when the terminal cannot
// be queried (CI, piped output, non-TTY).
//
// NOTE: termenv is not a direct dependency; lipgloss.HasDarkBackground is
// semantically identical and already imported.
var (
	darkOnce sync.Once
	isDark   bool
)

func detectDark() bool {
	darkOnce.Do(func() {
		isDark = lipgloss.HasDarkBackground(os.Stdin, os.Stdout)
	})
	return isDark
}

// isTTY returns true when w is an *os.File attached to a live terminal.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// RenderForWriter returns styled markdown when w is a colour TTY; otherwise it
// returns the raw md string unchanged.
//
// Two conditions force the raw path:
//   - NO_COLOR is set in the environment.
//   - w is not an *os.File connected to a real terminal.
//
// When styling is active, width is fixed at 80 and the dark/light palette is
// resolved once via a package-level sync.Once before any program starts.
func RenderForWriter(w io.Writer, md string) string {
	if os.Getenv("NO_COLOR") != "" {
		return md
	}
	if !isTTY(w) {
		return md
	}
	return RenderStyled(md)
}

// RenderStyled renders markdown with the Mazarbul glamour theme at the default
// width and detected background polarity, performing NO gating of its own. It
// is for callers that have already decided styling is wanted via the shared
// interactive.ShouldStyle gate (which honours --color/CLICOLOR, not just
// NO_COLOR), so forcing colour with --color=always renders even into a pipe.
func RenderStyled(md string) string {
	return Render(md, maxWidth, detectDark())
}
