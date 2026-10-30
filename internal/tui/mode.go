// Package tui contains shared terminal-UI primitives and mode detection.
//
// Mazarbul commands run in one of three modes:
//
//   - ModeTUI:  full Bubble Tea program with colors, animations, forms.
//     Default when stdout is a terminal.
//   - ModeCLI:  plain text output, no escape sequences.
//     Default when stdout is piped, or --no-tui is set, or NO_COLOR
//     disables color (in practice we still suppress the TUI then).
//   - ModeJSON: machine-readable output (JSON object per line).
//     Selected by --json.
//
// Both ModeCLI and ModeJSON convey the same information as ModeTUI; the
// difference is presentation only.
package tui

import (
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Mode identifies the active output mode.
type Mode int

const (
	// ModeTUI is the rich Bubble Tea output.
	ModeTUI Mode = iota
	// ModeCLI is plain text.
	ModeCLI
	// ModeJSON is machine-readable JSON.
	ModeJSON
)

var current = ModeCLI

// DetectMode resolves the mode from command flags, environment, and TTY
// state. Call from PersistentPreRun before running the command body.
//
// Resolution order:
//  1. --json flag => ModeJSON
//  2. --no-tui flag => ModeCLI
//  3. NO_COLOR env => ModeCLI
//  4. stdout is not a terminal => ModeCLI
//  5. otherwise => ModeTUI
func DetectMode(cmd *cobra.Command) Mode {
	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		current = ModeJSON
		return current
	}
	if noTUI, _ := cmd.Flags().GetBool("no-tui"); noTUI {
		current = ModeCLI
		return current
	}
	if os.Getenv("NO_COLOR") != "" {
		current = ModeCLI
		return current
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		current = ModeCLI
		return current
	}
	current = ModeTUI
	return current
}

// Current returns the active mode.
func Current() Mode { return current }

// IsTUI reports whether the active mode is TUI.
func IsTUI() bool { return current == ModeTUI }

// IsCLI reports whether the active mode is plain CLI.
func IsCLI() bool { return current == ModeCLI }

// IsJSON reports whether the active mode is JSON.
func IsJSON() bool { return current == ModeJSON }

// IsNoColor reports whether colors should be suppressed (NO_COLOR env).
func IsNoColor() bool { return os.Getenv("NO_COLOR") != "" }

// SetMode overrides the active mode. Use in tests only.
func SetMode(m Mode) { current = m }

// DetectHubMode reports whether the interactive hub should launch for a
// bare `maz` invocation (i.e. no subcommand). Returns false when:
//
//   - stdout is not a terminal
//   - --headless or --no-hub is set
//   - MAZ_HEADLESS env is set to a truthy value
//   - the supplied interactive flag is false (`tui.interactive=false` in
//     ~/.config/mazarbul/config.yaml)
//
// Callers should still respect a returned false by printing the help
// text or routing to the cold-start splash.
func DetectHubMode(cmd *cobra.Command, interactive bool) bool {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return false
	}
	if headless, _ := cmd.Flags().GetBool("headless"); headless {
		return false
	}
	if noHub, _ := cmd.Flags().GetBool("no-hub"); noHub {
		return false
	}
	if v := os.Getenv("MAZ_HEADLESS"); v == "1" || v == "true" || v == "TRUE" {
		return false
	}
	return interactive
}
