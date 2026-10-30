package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/logging"
)

// TestParseLevel verifies all recognized level strings map to the correct
// logging.Level constant. The default case (unknown input) maps to warn so
// one-shot commands are quiet by default.
func TestParseLevel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  logging.Level
	}{
		{"debug", logging.LevelDebug},
		{"info", logging.LevelInfo},
		{"warn", logging.LevelWarn},
		{"error", logging.LevelError},
		{"", logging.LevelWarn},      // unknown input → warn default
		{"bogus", logging.LevelWarn}, // unrecognised value → warn default
		// MAZ_LOG_LEVEL is read verbatim, so case + surrounding space must
		// normalise to the intended (non-default) level, not fall through.
		{"DEBUG", logging.LevelDebug},
		{"ERROR", logging.LevelError},
		{"Info", logging.LevelInfo},
		{"  debug  ", logging.LevelDebug},
	} {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := parseLevel(tc.input)
			if got != tc.want {
				t.Errorf("parseLevel(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestEffectiveLogLevel_LoggerSinksToStderr verifies that effectiveLogLevel
// produces a level that NewLogger accepts without a FilePath, meaning the
// logger writes to stderr rather than a file. This is the regression guard
// for the TUI hub removal.
func TestEffectiveLogLevel_LoggerSinksToStderr(t *testing.T) {
	// t.Setenv requires no t.Parallel at this level.
	t.Setenv("MAZ_LOG_LEVEL", "")
	for _, level := range []string{"info", "debug", "warn"} {
		t.Run(level, func(t *testing.T) {
			// Subtests inherit the Setenv so must also not call t.Parallel.
			got := effectiveLogLevel(true, level, "maz recall")
			logger, err := logging.NewLogger(logging.Config{Level: got, Format: logging.FormatJSON})
			if err != nil {
				t.Fatalf("NewLogger: %v", err)
			}
			if logger == nil {
				t.Fatal("logger is nil")
			}
		})
	}
}

// TestHelpCmd_UnknownTopic_ExitsOne is the regression guard for B4.0:
// `maz help <unknown-topic>` must exit 1 and hint at `maz help-topics`.
// Previously cobra's built-in help command used Run (not RunE) so it always
// exited 0.
func TestHelpCmd_UnknownTopic_ExitsOne(t *testing.T) {
	t.Parallel()

	root := newRootCmd()

	// Capture output so the test is not noisy.
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)

	root.SetArgs([]string{"help", "no-such-topic-xyz"})
	err := root.Execute()

	// Execute must return an error for an unknown topic.
	if err == nil {
		t.Fatal("Execute returned nil; want a non-nil error for unknown help topic")
	}

	// The error must be a silentError carrying exit code 1.
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T (%v); want silentError", err, err)
	}
	if se.code != 1 {
		t.Errorf("silentError.code = %d; want 1", se.code)
	}

	// The output must hint at help-topics.
	out := buf.String()
	if !strings.Contains(out, "help-topics") {
		t.Errorf("output %q does not mention \"help-topics\"", out)
	}
}

// TestHelpCmd_KnownTopic_ExitsZero verifies that a valid subcommand name still
// produces help output and exits 0.
func TestHelpCmd_KnownTopic_ExitsZero(t *testing.T) {
	t.Parallel()

	root := newRootCmd()

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)

	root.SetArgs([]string{"help", "version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute returned %v; want nil for known topic", err)
	}

	if buf.Len() == 0 {
		t.Error("no output for known topic; want non-empty help text")
	}
}

// ── WS9: arg/flag error tests ─────────────────────────────────────────────────

// TestEditDistance verifies the Levenshtein implementation for boundary cases
// and typical flag-typo distances.
func TestEditDistance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"output", "outpu", 1},
		{"output", "ouput", 1}, //nolint:misspell // intentional typo to test Levenshtein suggestion
		{"output", "input", 3},
		{"scope", "scop", 1},
		{"scope", "scpe", 1},
		{"data-dir", "data-dri", 2},
	} {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			t.Parallel()
			got := editDistance(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestIsArgCountError verifies the patterns cobra uses for argument-count errors.
func TestIsArgCountError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"accepts 1 arg(s), received 0", true},
		{"accepts between 1 and 3 arg(s), received 4", true},
		{"requires at least 1 arg(s)", true},
		{"accepts at most 2 arg(s)", true},
		{"unknown command argument, received 0", true},
		{"unknown flag: --foo", false},
		{"", false},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			t.Parallel()
			got := isArgCountError(tc.msg)
			if got != tc.want {
				t.Errorf("isArgCountError(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// TestExtractUnknownFlagName checks the flag-name extractor for both long and
// short flag error formats produced by pflag.
func TestExtractUnknownFlagName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		msg  string
		want string
	}{
		{"unknown flag: --output", "output"},
		{"unknown flag: --data-dir", "data-dir"},
		{"unknown shorthand flag: 'x' in -x", "x"},
		{"unknown shorthand flag: 'v' in -v", "v"},
		{"some other error", ""},
		{"", ""},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			t.Parallel()
			got := extractUnknownFlagName(tc.msg)
			if got != tc.want {
				t.Errorf("extractUnknownFlagName(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}

// TestFlagError_DidYouMeanSuggestion verifies that an unknown flag triggers a
// "did you mean --X?" suggestion when a close match exists, and that the error
// message appears on stderr.
func TestFlagError_DidYouMeanSuggestion(t *testing.T) {
	t.Parallel()

	root := newRootCmd()
	var errBuf bytes.Buffer
	root.SetErr(&errBuf)
	root.SetOut(&errBuf)

	// "--outpu" is one edit away from "--output".
	root.SetArgs([]string{"recall", "--outpu", "foo"})
	err := root.Execute()
	if err == nil {
		t.Fatal("Execute returned nil; want error for unknown flag")
	}

	out := errBuf.String()
	if !strings.Contains(out, "did you mean --output") {
		t.Errorf("stderr %q does not contain did-you-mean suggestion for --output", out)
	}
}

// TestRootNoPagerFlag verifies that --no-pager is a registered persistent flag
// on the root command (and therefore inherited by all subcommands).
func TestRootNoPagerFlag(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	f := root.PersistentFlags().Lookup("no-pager")
	if f == nil {
		t.Fatal("--no-pager persistent flag not registered on root")
	}
	if f.DefValue != "false" {
		t.Errorf("--no-pager default = %q; want \"false\"", f.DefValue)
	}
}
