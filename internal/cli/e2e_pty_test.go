//go:build e2e

// This file's pty end-to-end suite drives the REAL maz binary through a
// pseudo-terminal so the dual-mode rendering contract (#269) is verified the
// way a user actually experiences it: colour on an interactive TTY, plain text
// under NO_COLOR, and clean machine output under --output json. It is gated
// behind the `e2e` build tag so the default `go test` gate stays fast; run it
// with:
//
//	go test -tags='e2e sqlite_fts5' ./internal/cli/ -run E2E
//
// The store is seeded empty (init --embed none --daemon off) — these tests
// assert the rendering CONTRACT (chrome, colour gating, JSON cleanliness,
// arg/flag-error UX, completion), not data-dependent output, which the
// per-command unit tests already cover against synthetic fixtures. No daemon is
// started and no network/Ollama is touched, so the suite is hermetic and never
// risks leaking into a real store.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// sgrPattern matches an SGR (Select Graphic Rendition) sequence — i.e. actual
// text STYLING like \x1b[38;5;245m or \x1b[m. It deliberately does NOT match the
// terminal capability-detection probes lipgloss emits for light/dark palette
// selection (OSC-11 background query \x1b]11;?\x07 and the DA1 query \x1b[c),
// which are sent regardless of NO_COLOR and are not styling.
var sgrPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

func hasStyling(s string) bool { return sgrPattern.MatchString(s) }

// e2eEnv holds the one-time-built binary path and the seeded data dir shared by
// every test in this suite.
type e2eEnv struct {
	bin     string
	dataDir string
}

var e2e e2eEnv

func TestMain(m *testing.M) {
	os.Exit(runE2EMain(m))
}

// runE2EMain builds the binary, seeds an isolated data dir, runs the suite, and
// returns the exit code (kept separate so deferred cleanup runs before exit).
func runE2EMain(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "maz-e2e-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)

	bin := filepath.Join(tmp, "maz")
	build := exec.CommandContext(context.Background(), "go", "build", "-tags=sqlite_fts5", "-o", bin, "../../cmd/maz")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic("build maz: " + err.Error())
	}

	dataDir := filepath.Join(tmp, "data")
	initCmd := exec.CommandContext(context.Background(), bin, "--data-dir", dataDir, "init", "--yes", "--embed", "none", "--daemon", "off")
	if out, err := initCmd.CombinedOutput(); err != nil {
		panic("init data dir: " + err.Error() + "\n" + string(out))
	}

	e2e = e2eEnv{bin: bin, dataDir: dataDir}
	return m.Run()
}

// runPTY runs the binary attached to a pseudo-terminal (so IsTTY() is true) and
// returns the combined output. extraEnv entries (e.g. "NO_COLOR=1") are appended
// to a baseline that forces a colour-capable TERM.
func runPTY(t *testing.T, extraEnv []string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), e2e.bin, append([]string{"--data-dir", e2e.dataDir}, args...)...)
	// A pty makes IsTTY() true, so colour is on by default — no force needed.
	// Forcing it would mask the NO_COLOR precedence we want to assert.
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Env = append(cmd.Env, extraEnv...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty.Start(%v): %v", args, err)
	}
	defer func() { _ = ptmx.Close() }()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, ptmx); close(done) }()
	_ = cmd.Wait()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return buf.String()
}

// runPipe runs the binary with piped (non-TTY) stdout/stderr and returns them
// separately, matching how an agent or `| jq` consumes maz.
func runPipe(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), e2e.bin, append([]string{"--data-dir", e2e.dataDir}, args...)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var o, e bytes.Buffer
	cmd.Stdout = &o
	cmd.Stderr = &e
	_ = cmd.Run()
	return o.String(), e.String()
}

func TestE2EStatusColorOnTTY(t *testing.T) {
	out := runPTY(t, nil, "status")
	if !hasStyling(out) {
		t.Errorf("status on a colour TTY should emit SGR styling, got plain:\n%s", out)
	}
}

func TestE2EStatusPlainUnderNoColor(t *testing.T) {
	out := runPTY(t, []string{"NO_COLOR=1"}, "status")
	if hasStyling(out) {
		t.Errorf("NO_COLOR=1 must emit no SGR styling even on a TTY, got:\n%q", out)
	}
	// Symbol-first contract: meaning still legible without colour.
	if !strings.Contains(out, "store") && !strings.Contains(out, "daemon") {
		t.Errorf("status should still render labels under NO_COLOR, got:\n%s", out)
	}
}

func TestE2EStatusJSONIsCleanAndCarriesNewFields(t *testing.T) {
	stdout, _ := runPipe(t, nil, "status", "--output", "json")
	if hasStyling(stdout) {
		t.Errorf("--output json must never contain SGR styling, got:\n%q", stdout)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("status --output json is not valid JSON: %v\n%s", err, stdout)
	}
	for _, k := range []string{"daemon", "log"} {
		if _, ok := rep[k]; !ok {
			t.Errorf("status JSON missing new field %q: %v", k, rep)
		}
	}
}

func TestE2EDigestAndActivityJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"digest json", []string{"digest", "--since", "7d", "--output", "json"}},
		{"activity json", []string{"activity", "--span", "30d", "--output", "json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _ := runPipe(t, nil, tc.args...)
			if hasStyling(stdout) {
				t.Errorf("%s: --output json must not contain SGR styling:\n%q", tc.name, stdout)
			}
			var v any
			if err := json.Unmarshal([]byte(stdout), &v); err != nil {
				t.Errorf("%s: not valid JSON: %v\n%s", tc.name, err, stdout)
			}
		})
	}
}

func TestE2EArgCountErrorShowsUsage(t *testing.T) {
	_, stderr := runPipe(t, nil, "get")
	if !strings.Contains(stderr, "accepts 1 arg") {
		t.Errorf("expected arg-count error, got: %q", stderr)
	}
	if !strings.Contains(stderr, "Usage") && !strings.Contains(stderr, "usage") {
		t.Errorf("arg-count error should print usage, got: %q", stderr)
	}
}

func TestE2EMisspelledFlagSuggests(t *testing.T) {
	_, stderr := runPipe(t, nil, "recall", "x", "--lmit", "5")
	if !strings.Contains(stderr, "did you mean --limit") {
		t.Errorf("expected did-you-mean for --lmit, got: %q", stderr)
	}
}

func TestE2EScopeCompletionReturnsThreeSet(t *testing.T) {
	// Cobra's hidden __complete command drives shell completion: the candidate
	// values + the numeric directive code land on stdout, while the
	// human-readable directive name ("ShellCompDirectiveNoFileComp") goes to
	// stderr. Assert the values on stdout and the directive across both streams.
	stdout, stderr := runPipe(t, nil, "__complete", "recall", "--scope", "")
	for _, want := range []string{"project", "personal", "team"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("scope completion missing %q, got:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout+stderr, "NoFileComp") {
		t.Errorf("scope completion must set ShellCompDirectiveNoFileComp, got stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestE2EVersionBannerNeverInJSON(t *testing.T) {
	stdout, _ := runPipe(t, nil, "status", "--output", "json")
	if strings.Contains(strings.ToLower(stdout), "available") && strings.Contains(stdout, "run:") {
		t.Errorf("version banner must never appear in --output json, got:\n%s", stdout)
	}
}
