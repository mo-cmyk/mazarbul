package style

import (
	"image/color"
	"os"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"
)

// adapt returns an adaptive colour that picks light or dark depending on the
// terminal, collapsing to lipgloss.NoColor when NO_COLOR is set.
//
// NO_COLOR is read on every call rather than cached: the cost is a map
// lookup, and re-reading lets tests flip it via t.Setenv without juggling a
// process-level sync.Once.
func adapt(light, dark string) color.Color {
	if os.Getenv("NO_COLOR") != "" {
		return lipgloss.NoColor{}
	}
	return compat.AdaptiveColor{Light: lipgloss.Color(light), Dark: lipgloss.Color(dark)}
}

// ansi returns a structural-chrome colour by ANSI index (0–255), collapsing to
// lipgloss.NoColor when NO_COLOR is set.
//
// Tier-2 chrome is addressed by index rather than hex so it inherits the
// user's terminal theme instead of pinning a truecolour value.
func ansi(index string) color.Color {
	if os.Getenv("NO_COLOR") != "" {
		return lipgloss.NoColor{}
	}
	return lipgloss.Color(index)
}

// Mazarbul's palette is two-tier. Tier-1 carries brand and type meaning as
// truecolour hex (lipgloss down-samples to the terminal's profile); tier-2 is
// structural chrome addressed by ANSI index so it inherits the user's theme.
// The brand accent is a bright orange; light-mode values are the matching
// one-step-darker siblings so the brand reads the same in both backgrounds.

// Accent is the primary brand colour (orange). Used for the brand mark, active
// tab, selection caret, identity bullet, section titles, and spinner glyphs.
func Accent() color.Color { return adapt("#f97316", "#fb923c") }

// AccentDeep is a deeper accent variant for hover or focused states.
func AccentDeep() color.Color { return adapt("#ea580c", "#fdba74") }

// Ink is the foreground body-text colour.
func Ink() color.Color { return adapt("#1A1816", "#F5F1EB") }

// Paper is the background colour.
func Paper() color.Color { return adapt("#FAF7F2", "#1A1816") }

// SubtleColor is the secondary-text colour (labels, hints).
//
// The dark hex is deliberately #A8A39C (B=156), one step above the 144↔145
// ANSI256 quantization midpoint at B=155. Termenv versions disagree on
// round-half tie-breaking for B=155, so any platform that rounds the other
// way emits 144 instead of 145 and breaks golden snapshots. Stepping B to
// 156 keeps it deterministically on the 175 side → ANSI 145 everywhere.
func SubtleColor() color.Color { return adapt("#6B6660", "#A8A39C") }

// FaintColor is the tertiary-text colour (borders, separators, disabled).
func FaintColor() color.Color { return adapt("#B8B2A8", "#4A4641") }

// SuccessColor is the semantic success colour.
func SuccessColor() color.Color { return adapt("#5A7A50", "#8FAB7E") }

// WarningColor is the semantic warning colour.
func WarningColor() color.Color { return adapt("#A8742A", "#D4A574") }

// ErrorColor is the semantic error colour.
func ErrorColor() color.Color { return adapt("#A04848", "#C87878") }

// InfoColor is the semantic info colour.
func InfoColor() color.Color { return adapt("#3A5D7A", "#8AA8C0") }

// Tier-1 semantic type hues (truecolour, adaptive). These carry node/type
// meaning across the read surface and the colorizer; lipgloss down-samples
// them to the terminal's colour profile.

// PurpleColor marks the session type, supersede edges, and detail-card chrome.
func PurpleColor() color.Color { return adapt("#a855f7", "#c084fc") }

// BlueColor marks the commit/relation type and markdown links.
func BlueColor() color.Color { return adapt("#3b82f6", "#60a5fa") }

// CyanColor marks metadata and structure (the colorizer's Meta).
func CyanColor() color.Color { return adapt("#06b6d4", "#22d3ee") }

// IndigoColor marks keywords and enumerations.
func IndigoColor() color.Color { return adapt("#6366f1", "#818cf8") }

// AmberColor marks identifiers and ULIDs (the colorizer's Ident).
func AmberColor() color.Color { return adapt("#f59e0b", "#fbbf24") }

// DiffAddedColor is the inserted-content colour for diffs and chroma blocks.
func DiffAddedColor() color.Color { return adapt("#16a34a", "#22C55E") }

// DiffRemovedColor is the deleted-content colour for diffs and chroma blocks.
func DiffRemovedColor() color.Color { return adapt("#dc2626", "#EF4444") }

// Tier-2 structural chrome (ANSI indices). These inherit the user's terminal
// theme rather than pinning a truecolour value.

// ANSISuccess is ANSI green (index 2) for the ✓ success bullet and +N stats.
func ANSISuccess() color.Color { return ansi("2") }

// ANSIError is ANSI red (index 1) for the ✗ failure bullet and conflicts.
func ANSIError() color.Color { return ansi("1") }

// ANSIWarning is ANSI yellow (index 3) for the ⚠ warning bullet and stale state.
func ANSIWarning() color.Color { return ansi("3") }

// ANSIDim is ANSI bright-black (index 8) for hashes, units, and table headers.
func ANSIDim() color.Color { return ansi("8") }

// ANSIMuted is ANSI 245 for help-key labels and placeholders.
func ANSIMuted() color.Color { return ansi("245") }

// ANSIRule is ANSI 241 for faint separators and section rules.
func ANSIRule() color.Color { return ansi("241") }

// ANSIIdentValue is ANSI white (index 7) for the primary identifier value.
func ANSIIdentValue() color.Color { return ansi("7") }

// agentHues maps a writer identity to a stable brand hue, mirroring the
// signed EventAuthor/principal Mazarbul stamps on every event.
var agentHues = map[string]string{
	"claude":   "#fb923c",
	"gemini":   "#60a5fa",
	"amp":      "#f87171",
	"codex":    "#818cf8",
	"opencode": "#22d3ee",
	"copilot":  "#a78bfa",
	"pi":       "#fbbf24",
	"cursor":   "#38bdf8",
	"droid":    "#f472b6",
	"kiro":     "#c084fc",
}

// AgentColor maps a writer identity to its stable hue; an unknown identity
// resolves to ANSI 245. Collapses to lipgloss.NoColor when NO_COLOR is set.
func AgentColor(agent string) color.Color {
	if os.Getenv("NO_COLOR") != "" {
		return lipgloss.NoColor{}
	}
	if hex, ok := agentHues[strings.ToLower(strings.TrimSpace(agent))]; ok {
		return lipgloss.Color(hex)
	}
	return lipgloss.Color("245")
}
