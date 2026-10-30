package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newDocLintTestCmd returns a bare command wired to in-memory stdout/stderr so
// decodeAndRenderDocLint can be exercised without a daemon or wiring. The
// --output flag is pinned to text so the text renderer runs deterministically
// regardless of the test process's TTY state (off-TTY defaults to JSON).
func newDocLintTestCmd() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "lint"}
	cmd.Flags().String("output", "text", "")
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	return cmd, &out, &errBuf
}

// TestDecodeAndRenderDocLint_NilRawSurfacesUnavailable proves a nil tool result
// (older or minimal daemon that never registered lintDocuments) yields the
// shared "navigation tools not available" handling and exit code 1, not a
// cryptic JSON parse error.
func TestDecodeAndRenderDocLint_NilRawSurfacesUnavailable(t *testing.T) {
	cmd, _, errBuf := newDocLintTestCmd()

	err := decodeAndRenderDocLint(cmd, nil)

	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want silentError", err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1", se.code)
	}
	if !strings.Contains(errBuf.String(), errNavUnavailableMsg) {
		t.Errorf("stderr = %q, want it to contain %q", errBuf.String(), errNavUnavailableMsg)
	}
	if strings.Contains(errBuf.String(), "parse response") {
		t.Errorf("stderr = %q, should not report a JSON parse error", errBuf.String())
	}
}

// TestDecodeAndRenderDocLint_CleanReportExitsZero proves an all-empty report
// renders the all-clear line and returns nil (exit 0).
func TestDecodeAndRenderDocLint_CleanReportExitsZero(t *testing.T) {
	cmd, out, _ := newDocLintTestCmd()
	raw := json.RawMessage(`{"orphans":[],"deadLinks":[],"contradictions":[]}`)

	if err := decodeAndRenderDocLint(cmd, raw); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "no document issues found") {
		t.Errorf("stdout = %q, want the all-clear line", out.String())
	}
}

// TestDecodeAndRenderDocLint_IssuesExitOne proves a report carrying any issue
// (here a dead link) renders the section and exits 1 with errDocLintIssues.
func TestDecodeAndRenderDocLint_IssuesExitOne(t *testing.T) {
	cmd, out, _ := newDocLintTestCmd()
	raw := json.RawMessage(`{"orphans":[],"deadLinks":[{"documentUid":"d1","path":"notes/dangling.md","title":"Dangling"}],"contradictions":[]}`)

	err := decodeAndRenderDocLint(cmd, raw)

	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want silentError", err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1", se.code)
	}
	if !errors.Is(err, errDocLintIssues) {
		t.Errorf("err = %v, want it to wrap errDocLintIssues", err)
	}
	if !strings.Contains(out.String(), "notes/dangling.md") {
		t.Errorf("stdout = %q, want the dead link path", out.String())
	}
}

// TestDecodeAndRenderDocLint_JSONNeverEmitsNull proves the array contract holds
// even against an OLDER daemon payload that omits a category this CLI knows
// (here danglingEdges): the re-emitted JSON carries every category as an array,
// never null, so an agent can iterate without a nil check.
func TestDecodeAndRenderDocLint_JSONNeverEmitsNull(t *testing.T) {
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "lint"}
	cmd.Flags().String("output", "json", "")
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)

	// An older daemon's payload: no danglingEdges key at all.
	raw := json.RawMessage(`{"orphans":[],"deadLinks":[],"contradictions":[]}`)
	if err := decodeAndRenderDocLint(cmd, raw); err != nil {
		t.Fatalf("err = %v, want nil (clean report)", err)
	}
	for _, key := range []string{`"orphans":[]`, `"deadLinks":[]`, `"contradictions":[]`, `"danglingEdges":[]`} {
		if !strings.Contains(out.String(), key) {
			t.Errorf("JSON output %s missing %s (array contract violated)", out.String(), key)
		}
	}
	if strings.Contains(out.String(), "null") {
		t.Errorf("JSON output %s must not contain null", out.String())
	}
}

// TestDecodeAndRenderDocLint_MalformedRawIsFatal proves a non-empty but
// malformed payload still reports a parse error and exits 2.
func TestDecodeAndRenderDocLint_MalformedRawIsFatal(t *testing.T) {
	cmd, _, errBuf := newDocLintTestCmd()

	err := decodeAndRenderDocLint(cmd, json.RawMessage(`{not json`))

	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want silentError", err)
	}
	if se.code != 2 {
		t.Errorf("exit code = %d, want 2", se.code)
	}
	if !strings.Contains(errBuf.String(), "parse response") {
		t.Errorf("stderr = %q, want a parse-response error", errBuf.String())
	}
}
