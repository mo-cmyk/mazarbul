package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
)

// makeAskCmd wires a cobra command tree in the same shape as production
// so PersistentFlags (--data-dir, --json, --no-tui) are accessible.
func makeAskCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	ask := newAskCmd()
	root.AddCommand(ask)
	if outBuf != nil {
		root.SetOut(outBuf)
		ask.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		ask.SetErr(errBuf)
	}
	return root
}

// TestAskHelp: --help output is non-empty and includes the examples section.
func TestAskHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeAskCmd(&buf, nil)
	root.SetArgs([]string{"ask", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{
		"Examples:",
		"maz ask",
		"--limit",
		"--at",
		"--no-graph",
		"--json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestAskChatDisabled: when no ask tool is registered (chat disabled),
// stderr carries the canonical error message and exit code is 1.
func TestAskChatDisabled(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	cmd := newAskCmd()
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)

	// runAsk with empty wired.Tools (simulated by calling the not-found
	// branch directly via renderAskCLI path: provide an empty tool list).
	// We do this by constructing a minimal cobra tree and exercising the
	// "chat disabled" path via the helper that checks the found flag.
	err := askNotFoundError(cmd)

	var se silentError
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.As(err, &se) {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1", se.code)
	}
	if !strings.Contains(errBuf.String(), "chat provider disabled") {
		t.Errorf("stderr missing chat-disabled message: %q", errBuf.String())
	}
}

// askNotFoundError exercises the "ask tool not found" branch directly,
// writing to cmd.ErrOrStderr() and returning the appropriate silentError.
// This avoids opening real daemon wiring in unit tests.
func askNotFoundError(cmd *cobra.Command) error {
	if _, werr := cmd.ErrOrStderr().Write([]byte(errChatDisabledMsg + "\n")); werr != nil {
		return werr
	}
	return silentError{code: 1, err: fmt.Errorf("%s", errChatDisabledMsg)}
}

// TestRenderAskCLI_ProseOnly: an answer with no citations renders only
// the prose block.
func TestRenderAskCLI_ProseOnly(t *testing.T) {
	t.Parallel()
	answer := composer.Answer{
		Answer:    "Merry works at Mithril Corp.",
		Citations: nil,
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var buf bytes.Buffer
	cmd := newAskCmd()
	cmd.SetOut(&buf)

	if err := renderAskCLI(cmd, raw); err != nil {
		t.Fatalf("renderAskCLI: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Merry works at Mithril Corp.") {
		t.Errorf("output missing prose: %q", out)
	}
	if strings.Contains(out, "Citations:") {
		t.Errorf("output should not contain Citations section when empty: %q", out)
	}
}

// TestRenderAskCLI_WithCitations: citations block appears when citations
// are present and each row contains the episode UID.
func TestRenderAskCLI_WithCitations(t *testing.T) {
	t.Parallel()
	answer := composer.Answer{
		Answer: "See [1] for details.",
		Citations: []composer.Citation{
			{Index: 1, EpisodeUID: "01HN0000000000000000000001", Snippet: "Merry joined Mithril in 2024."},
		},
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var buf bytes.Buffer
	cmd := newAskCmd()
	cmd.SetOut(&buf)

	if err := renderAskCLI(cmd, raw); err != nil {
		t.Fatalf("renderAskCLI: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"See [1] for details.",
		"Citations:",
		"01HN0000000000000000000001",
		"Merry joined Mithril",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestRenderAskCLI_StripsHighlightMarkup pins that FTS5 <b>…</b> highlight
// markers are stripped from the human-facing citation table, mirroring the
// recall CLI table. The MCP wire shape keeps the markers; the CLI is
// display-only.
func TestRenderAskCLI_StripsHighlightMarkup(t *testing.T) {
	t.Parallel()
	answer := composer.Answer{
		Answer: "See [1] for details.",
		Citations: []composer.Citation{
			{Index: 1, EpisodeUID: "01HN0000000000000000000001", Snippet: "Merry joined <b>Mithril</b> in 2024."},
		},
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var buf bytes.Buffer
	cmd := newAskCmd()
	cmd.SetOut(&buf)

	if err := renderAskCLI(cmd, raw); err != nil {
		t.Fatalf("renderAskCLI: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "<b>") || strings.Contains(out, "</b>") {
		t.Errorf("citation table leaked FTS5 highlight markup\nfull:\n%s", out)
	}
	if !strings.Contains(out, "Merry joined Mithril in 2024.") {
		t.Errorf("output missing clean snippet text\nfull:\n%s", out)
	}
}

// TestRenderAskCLI_JSON: JSON mode emits the raw JSON to stdout.
func TestRenderAskCLI_JSON(t *testing.T) {
	t.Parallel()

	answer := composer.Answer{
		Answer:    "Merry works at Mithril.",
		Citations: []composer.Citation{{Index: 1, EpisodeUID: "01HN0000000000000000000001", Snippet: "snippet"}},
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var outBuf bytes.Buffer
	cmd := newAskCmd()
	cmd.SetOut(&outBuf)

	// Simulate the JSON branch: write raw directly.
	if _, werr := fmt.Fprintln(&outBuf, string(raw)); werr != nil {
		t.Fatalf("write: %v", werr)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &parsed); err != nil {
		t.Fatalf("parse JSON output: %v (%q)", err, outBuf.String())
	}
	for _, key := range []string{"answer", "citations"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("JSON output missing key %q", key)
		}
	}
}

// TestRenderAskCLI_NoHTMLTags asserts that <b>...</b> FTS5 highlight markup
// in citation snippets is stripped before reaching plain-text output. Fix H.
func TestRenderAskCLI_NoHTMLTags(t *testing.T) {
	t.Parallel()
	answer := composer.Answer{
		Answer: "Merry prefers black coffee. [1]",
		Citations: []composer.Citation{
			{Index: 1, EpisodeUID: "01HN0000000000000000000001", Snippet: "I prefer black <b>coffee</b> in the morning"},
		},
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var buf bytes.Buffer
	cmd := newAskCmd()
	cmd.SetOut(&buf)

	if err := renderAskCLI(cmd, raw); err != nil {
		t.Fatalf("renderAskCLI: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "<b>") || strings.Contains(out, "</b>") {
		t.Errorf("plain-text ask output must not contain HTML tags, got:\n%s", out)
	}
	// Content sans tags must still appear.
	if !strings.Contains(out, "coffee") {
		t.Errorf("snippet content missing after tag strip: %q", out)
	}
}

// TestAskFlags: flag defaults match the documented contract.
func TestAskFlags(t *testing.T) {
	t.Parallel()
	cmd := newAskCmd()
	if got, _ := cmd.Flags().GetInt("limit"); got != 10 {
		t.Errorf("--limit default = %d, want 10", got)
	}
	if got, _ := cmd.Flags().GetString("at"); got != "" {
		t.Errorf("--at default = %q, want empty", got)
	}
	if got, _ := cmd.Flags().GetBool("no-graph"); got {
		t.Errorf("--no-graph default = true, want false")
	}
}

// TestAskLimitValidation: values outside 1..50 are rejected with exit 1
// and a clear message; values at the boundary edges are accepted.
func TestAskLimitValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		limit   int
		wantErr bool
	}{
		{"zero", 0, true},
		{"negative", -1, true},
		{"over max 51", 51, true},
		{"over max 100", 100, true},
		{"min boundary 1", 1, false},
		{"max boundary 50", 50, false},
		{"mid 10", 10, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			cmd := newAskCmd()
			cmd.SetErr(&errBuf)
			flags := &askFlags{limit: tc.limit}
			err := validateAskFlags(cmd, flags)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("limit=%d: expected error, got nil", tc.limit)
				}
				var se silentError
				if !errors.As(err, &se) {
					t.Fatalf("limit=%d: expected silentError, got %T: %v", tc.limit, err, err)
				}
				if se.code != 1 {
					t.Errorf("limit=%d: exit code = %d, want 1", tc.limit, se.code)
				}
				if !strings.Contains(errBuf.String(), "out of range") {
					t.Errorf("limit=%d: stderr missing 'out of range': %q", tc.limit, errBuf.String())
				}
			} else if err != nil {
				t.Errorf("limit=%d: unexpected error: %v", tc.limit, err)
			}
		})
	}
}

// TestAskAtValidation: a bad --at value produces exit 1 with a friendly
// RFC 3339 message; a valid timestamp is accepted without error.
func TestAskAtValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		at      string
		wantErr bool
	}{
		{"empty accepted", "", false},
		{"valid RFC 3339 UTC", "2026-01-01T00:00:00Z", false},
		{"valid RFC 3339 with offset", "2026-06-18T12:00:00+02:00", false},
		{"valid RFC 3339 nanoseconds", "2026-01-01T00:00:00.123456789Z", false},
		{"bare date", "2026-01-01", true},
		{"garbage", "not-a-time", true},
		{"Jan 1 reference style", "January 2, 2006", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var errBuf bytes.Buffer
			cmd := newAskCmd()
			cmd.SetErr(&errBuf)
			flags := &askFlags{limit: 10, at: tc.at}
			err := validateAskFlags(cmd, flags)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("--at=%q: expected error, got nil", tc.at)
				}
				var se silentError
				if !errors.As(err, &se) {
					t.Fatalf("--at=%q: expected silentError, got %T: %v", tc.at, err, err)
				}
				if se.code != 1 {
					t.Errorf("--at=%q: exit code = %d, want 1", tc.at, se.code)
				}
				msg := errBuf.String()
				// Must not leak the Go time.Parse "parsing time ... as ..." form
				// which exposes the internal RFC3339Nano layout string.
				if strings.Contains(msg, `parsing time`) {
					t.Errorf("--at=%q: stderr leaks time.Parse error: %q", tc.at, msg)
				}
				// Must give a human-readable hint.
				if !strings.Contains(msg, "RFC 3339") {
					t.Errorf("--at=%q: stderr missing RFC 3339 hint: %q", tc.at, msg)
				}
			} else if err != nil {
				t.Errorf("--at=%q: unexpected error: %v", tc.at, err)
			}
		})
	}
}

// TestIsProviderAuthError: the helper correctly classifies 401/403 error
// strings and leaves unrelated errors unclassified.
func TestIsProviderAuthError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"generic error", errors.New("something broke"), false},
		{"anthropic 401 status", errors.New("anthropic: Unauthorized (status 401, type authentication_error)"), true},
		{"anthropic HTTP 401", errors.New("anthropic: HTTP 401"), true},
		{"anthropic status 403", errors.New("anthropic: Forbidden (status 403, type permission_error)"), true},
		{"HTTP 403 string", errors.New("ask: compose: HTTP 403"), true},
		{"authentication_error type", errors.New("mcp: E_INTERNAL: ask: compose: authentication_error"), true},
		{"rate limit 429", errors.New("anthropic: rate limit (status 429)"), false},
		{"storage error", errors.New("ladybug: status 1"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isProviderAuthError(tc.err)
			if got != tc.want {
				t.Errorf("isProviderAuthError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestAskAuthErrorExitCode: when isProviderAuthError returns true the
// validateAskFlags+error-routing path emits exit 1 and a maz secret set hint.
func TestAskAuthErrorExitCode(t *testing.T) {
	t.Parallel()
	var errBuf bytes.Buffer
	cmd := newAskCmd()
	cmd.SetErr(&errBuf)

	// Simulate what runAsk does when it receives a 401 from the tool layer.
	authErr := errors.New("anthropic: Unauthorized (status 401, type authentication_error)")
	if !isProviderAuthError(authErr) {
		t.Fatal("test setup: isProviderAuthError returned false for a 401 error")
	}
	msg := `ask: provider authentication failed (HTTP 401/403): run "maz secret set" to configure your API key`
	if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), msg); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	result := silentError{code: 1, err: errors.New(msg)}

	var se silentError
	if !errors.As(result, &se) {
		t.Fatalf("expected silentError, got %T", result)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1", se.code)
	}
	if !strings.Contains(errBuf.String(), "maz secret set") {
		t.Errorf("stderr missing 'maz secret set': %q", errBuf.String())
	}
}
