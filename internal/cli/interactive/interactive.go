// Package interactive provides TTY-detection and ANSI-style-gating
// primitives for the maz CLI.
//
// Two gated primitives are offered:
//
//   - CanPromptInteractively: safe to block on a user prompt?
//   - ShouldStyle: safe to emit ANSI escape sequences?
//
// Both are backed by pure injectable helpers so they are unit-testable
// without a real TTY or cobra.Command.
package interactive

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// IsTTY reports whether w is a terminal by type-asserting to *os.File and
// calling term.IsTerminal.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// canPrompt is the injectable core of CanPromptInteractively.
// All decisions are driven from parameters so the logic is unit-testable
// without a real process environment or /dev/tty.
func canPrompt(getenv func(string) string, isTesting bool, devTTYOpens func() bool) bool {
	// 1. MAZ_TEST_TTY: "1" => true, any other non-empty value => false.
	if v := getenv("MAZ_TEST_TTY"); v != "" {
		return v == "1"
	}
	// 2. Running under go test.
	if isTesting {
		return false
	}
	// 3. Agent sentinels: any non-empty value => false.
	for _, sentinel := range []string{"GEMINI_CLI", "COPILOT_CLI", "PI_CODING_AGENT", "CLAUDECODE"} {
		if getenv(sentinel) != "" {
			return false
		}
	}
	if getenv("GIT_TERMINAL_PROMPT") == "0" {
		return false
	}
	// 4. CI set to a truthy value => false; "false" is explicitly ignored.
	if ci := getenv("CI"); ci != "" && ci != "false" {
		return false
	}
	// 5. Fall back to whether /dev/tty is openable.
	return devTTYOpens()
}

// CanPromptInteractively reports whether it is safe to show an interactive
// prompt. Precedence, first match wins:
//
//  1. MAZ_TEST_TTY: "1" => true, any other non-empty value => false.
//  2. testing.Testing() => false.
//  3. Agent sentinels => false: any of GEMINI_CLI / COPILOT_CLI /
//     PI_CODING_AGENT / CLAUDECODE non-empty, or GIT_TERMINAL_PROMPT == "0".
//  4. CI != "" && CI != "false" => false.
//  5. /dev/tty opens => true, else false.
func CanPromptInteractively() bool {
	return canPrompt(os.Getenv, testing.Testing(), func() bool {
		f, err := os.Open("/dev/tty")
		if err != nil {
			return false
		}
		_ = f.Close()
		return true
	})
}

// shouldStyle is the injectable core of ShouldStyle.
// Precedence (first match wins):
//
//  1. noColor != "" => false (NO_COLOR wins unconditionally).
//  2. when == "never" => false.
//  3. when == "always" => true.
//  4. clicolorForce != "" || forceColor != "" => true (force-on in auto).
//  5. termEnv == "cygwin" => false (legacy console renders ESC as left-arrow).
//  6. clicolor == "0" => false.
//  7. otherwise => isTTY.
func shouldStyle(when, noColor, clicolor, clicolorForce, forceColor, termEnv string, isTTY bool) bool {
	if noColor != "" {
		return false
	}
	switch strings.ToLower(when) {
	case "never":
		return false
	case "always":
		return true
	}
	if clicolorForce != "" || forceColor != "" {
		return true
	}
	if termEnv == "cygwin" {
		return false
	}
	if clicolor == "0" {
		return false
	}
	return isTTY
}

// ShouldStyle reports whether ANSI styling should be emitted to w for cmd.
// It reads the --color flag off cmd (auto|always|never), the NO_COLOR /
// CLICOLOR / CLICOLOR_FORCE / FORCE_COLOR env vars, TERM, and IsTTY(w).
func ShouldStyle(cmd *cobra.Command, w io.Writer) bool {
	when, err := cmd.Flags().GetString("color")
	if err != nil || when == "" {
		when = "auto"
	}
	return shouldStyle(
		when,
		os.Getenv("NO_COLOR"),
		os.Getenv("CLICOLOR"),
		os.Getenv("CLICOLOR_FORCE"),
		os.Getenv("FORCE_COLOR"),
		os.Getenv("TERM"),
		IsTTY(w),
	)
}
