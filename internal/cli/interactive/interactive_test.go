package interactive

import (
	"bytes"
	"os"
	"testing"

	"github.com/spf13/cobra"
)

// TestShouldStylePrecedence covers the shouldStyle decision matrix in
// priority order: NO_COLOR wins unconditionally, then explicit flags, then
// force-on variables, then terminal type, then CLICOLOR, then TTY state.
func TestShouldStylePrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		when          string
		noColor       string
		clicolor      string
		clicolorForce string
		forceColor    string
		termEnv       string
		isTTY         bool
		want          bool
	}{
		{
			name:    "NO_COLOR beats always",
			when:    "always",
			noColor: "1",
			want:    false,
		},
		{
			name: "never is false",
			when: "never",
			want: false,
		},
		{
			name: "always is true",
			when: "always",
			want: true,
		},
		{
			name:       "FORCE_COLOR forces on in auto",
			when:       "auto",
			forceColor: "1",
			isTTY:      false,
			want:       true,
		},
		{
			name:          "CLICOLOR_FORCE forces on in auto",
			when:          "auto",
			clicolorForce: "1",
			isTTY:         false,
			want:          true,
		},
		{
			name:    "cygwin TERM is off in auto",
			when:    "auto",
			termEnv: "cygwin",
			isTTY:   true,
			want:    false,
		},
		{
			name:     "CLICOLOR=0 is off in auto",
			when:     "auto",
			clicolor: "0",
			isTTY:    true,
			want:     false,
		},
		{
			name:  "auto with TTY is on",
			when:  "auto",
			isTTY: true,
			want:  true,
		},
		{
			name:  "auto without TTY is off",
			when:  "auto",
			isTTY: false,
			want:  false,
		},
		{
			name:    "NO_COLOR beats never",
			when:    "never",
			noColor: "1",
			want:    false,
		},
		{
			name:       "NO_COLOR beats FORCE_COLOR",
			when:       "auto",
			noColor:    "1",
			forceColor: "1",
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := shouldStyle(tc.when, tc.noColor, tc.clicolor, tc.clicolorForce, tc.forceColor, tc.termEnv, tc.isTTY)
			if got != tc.want {
				t.Errorf("shouldStyle(%q, noColor=%q, clicolor=%q, clicolorForce=%q, forceColor=%q, term=%q, isTTY=%v) = %v, want %v",
					tc.when, tc.noColor, tc.clicolor, tc.clicolorForce, tc.forceColor, tc.termEnv, tc.isTTY, got, tc.want)
			}
		})
	}
}

// TestShouldStyleCaseInsensitiveWhen verifies that --color flag values are
// accepted case-insensitively so that ALWAYS, Never, etc. map correctly.
func TestShouldStyleCaseInsensitiveWhen(t *testing.T) {
	t.Parallel()
	if got := shouldStyle("ALWAYS", "", "", "", "", "", false); !got {
		t.Error("shouldStyle(ALWAYS) = false, want true")
	}
	if got := shouldStyle("Never", "", "", "", "", "", true); got {
		t.Error("shouldStyle(Never) = true, want false")
	}
}

// TestCanPromptPrecedence covers the canPrompt decision matrix via the
// injectable helper. Real /dev/tty is never opened.
func TestCanPromptPrecedence(t *testing.T) {
	t.Parallel()

	// devOpens is a /dev/tty stub that always succeeds so the final fallback
	// can be tested without a real terminal.
	devOpens := func() bool { return true }
	devFails := func() bool { return false }

	tests := []struct {
		name      string
		env       map[string]string
		isTesting bool
		devTTY    func() bool
		want      bool
	}{
		{
			name: "MAZ_TEST_TTY=1 is true",
			env:  map[string]string{"MAZ_TEST_TTY": "1"},
			want: true,
		},
		{
			name: "MAZ_TEST_TTY=0 is false",
			env:  map[string]string{"MAZ_TEST_TTY": "0"},
			want: false,
		},
		{
			name: "MAZ_TEST_TTY=yes is false",
			env:  map[string]string{"MAZ_TEST_TTY": "yes"},
			want: false,
		},
		{
			name:      "isTesting=true is false",
			env:       map[string]string{},
			isTesting: true,
			want:      false,
		},
		{
			name: "GEMINI_CLI set is false",
			env:  map[string]string{"GEMINI_CLI": "1"},
			want: false,
		},
		{
			name: "COPILOT_CLI set is false",
			env:  map[string]string{"COPILOT_CLI": "1"},
			want: false,
		},
		{
			name: "PI_CODING_AGENT set is false",
			env:  map[string]string{"PI_CODING_AGENT": "1"},
			want: false,
		},
		{
			name: "CLAUDECODE set is false",
			env:  map[string]string{"CLAUDECODE": "1"},
			want: false,
		},
		{
			name: "GIT_TERMINAL_PROMPT=0 is false",
			env:  map[string]string{"GIT_TERMINAL_PROMPT": "0"},
			want: false,
		},
		{
			name: "GIT_TERMINAL_PROMPT=1 does not block",
			env:  map[string]string{"GIT_TERMINAL_PROMPT": "1"},
			want: true, // falls through to devTTY = devOpens
		},
		{
			name: "CI=true is false",
			env:  map[string]string{"CI": "true"},
			want: false,
		},
		{
			name: "CI=1 is false",
			env:  map[string]string{"CI": "1"},
			want: false,
		},
		{
			name: "CI=false is ignored",
			env:  map[string]string{"CI": "false"},
			want: true, // falls through to devTTY = devOpens
		},
		{
			name: "dev/tty opens is true",
			env:  map[string]string{},
			want: true,
		},
		{
			name:   "dev/tty fails is false",
			env:    map[string]string{},
			devTTY: devFails,
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(key string) string { return tc.env[key] }
			devFn := devOpens
			if tc.devTTY != nil {
				devFn = tc.devTTY
			}
			got := canPrompt(getenv, tc.isTesting, devFn)
			if got != tc.want {
				t.Errorf("canPrompt(env=%v, isTesting=%v) = %v, want %v",
					tc.env, tc.isTesting, got, tc.want)
			}
		})
	}
}

// TestIsTTYNonFile verifies that IsTTY returns false for non-file writers
// such as bytes.Buffer.
func TestIsTTYNonFile(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if IsTTY(&buf) {
		t.Error("IsTTY(bytes.Buffer) = true, want false")
	}
}

// TestShouldStyleWithCmd verifies that ShouldStyle reads the --color flag
// from a cobra.Command and delegates to the pure shouldStyle helper.
// Not parallel: uses t.Setenv to control the process environment.
func TestShouldStyleWithCmd(t *testing.T) {
	// Build a minimal command with the --color flag registered, matching the
	// root.go registration that will be added by the C2 workstream.
	newCmd := func(colorVal string) *cobra.Command {
		cmd := &cobra.Command{Use: "test"}
		cmd.Flags().String("color", "auto", "colour mode")
		if colorVal != "" {
			if err := cmd.Flags().Set("color", colorVal); err != nil {
				t.Fatalf("set --color=%q: %v", colorVal, err)
			}
		}
		return cmd
	}

	// With --color=always and a non-TTY writer, styling must be on even
	// if NO_COLOR is not set.
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	if !ShouldStyle(newCmd("always"), &buf) {
		t.Error("ShouldStyle(always, buf) = false, want true")
	}

	// With --color=never and any writer, styling must be off.
	if ShouldStyle(newCmd("never"), os.Stdout) {
		t.Error("ShouldStyle(never, stdout) = true, want false")
	}
}

// TestShouldStyleNoColorEnv verifies that NO_COLOR in the environment beats
// --color=always as seen through the full ShouldStyle wrapper.
// Not parallel: uses t.Setenv to control the process environment.
func TestShouldStyleNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("color", "auto", "colour mode")
	if err := cmd.Flags().Set("color", "always"); err != nil {
		t.Fatalf("set --color=always: %v", err)
	}
	var buf bytes.Buffer
	if ShouldStyle(cmd, &buf) {
		t.Error("ShouldStyle with NO_COLOR=1 and --color=always = true, want false")
	}
}
