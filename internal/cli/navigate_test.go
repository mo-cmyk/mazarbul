package cli

// navigate_test.go — regression tests for the read-surface error/exit-code
// honesty and NDJSON streaming findings (the #192 cluster):
//
//   - isExpectedNavError / wrapNavFatal map E_NOT_FOUND + E_VALIDATION to
//     exit 1 (expected failure) and everything else to exit 2 (unexpected),
//     matching the documented 0/1/2/130 taxonomy in topics/exit-codes.md.
//   - neighbors/relations/entities --output ndjson emit exactly one parseable
//     JSON object per array element, not a single wrapper blob.
//   - blame on an entity with no version history honours --output json (the
//     standard flag), not just the legacy --format json, emitting a JSON array.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// ---- exit-code mapping: expected (1) vs unexpected (2) ----

// TestIsExpectedNavError pins the classification that drives the exit code:
// E_NOT_FOUND and E_VALIDATION are expected failures; E_INTERNAL, a nil error,
// and an unrelated error are not.
func TestIsExpectedNavError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not expected", err: nil, want: false},
		{name: "not found is expected", err: &mcp.ToolError{Code: mcp.CodeNotFound, Message: "entity missing"}, want: true},
		{name: "validation is expected", err: &mcp.ToolError{Code: mcp.CodeValidation, Message: "bad arg"}, want: true},
		{name: "internal is not expected", err: &mcp.ToolError{Code: mcp.CodeInternal, Message: "boom"}, want: false},
		{name: "plain error is not expected", err: errors.New("connection refused"), want: false},
		// Daemon transport flattens the ToolError to a plain string carrying
		// the code; the substring fallback must still classify it.
		{name: "transport string with not-found code", err: errors.New("get: E_NOT_FOUND: entity 01HZ... not found"), want: true},
		{name: "transport string with validation code", err: errors.New("neighbors: E_VALIDATION: depth must be between 1 and 6"), want: true},
		{name: "transport string with internal code", err: errors.New("get: E_INTERNAL: dispatcher closed"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isExpectedNavError(tc.err); got != tc.want {
				t.Errorf("isExpectedNavError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestWrapNavFatalExitCodes verifies that wrapNavFatal returns a silentError
// whose code is 1 for expected failures and 2 for unexpected ones, writes the
// tool-prefixed human message to stderr, and — because a buffered (non-TTY)
// stdout resolves to JSON mode — emits the shared {error,code} envelope to
// stdout so a piped consumer never has to parse stderr.
func TestWrapNavFatalExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantJSON string
	}{
		{name: "not found exits 1", err: &mcp.ToolError{Code: mcp.CodeNotFound, Message: "entity missing"}, wantCode: 1, wantJSON: "E_NOT_FOUND"},
		{name: "validation exits 1", err: &mcp.ToolError{Code: mcp.CodeValidation, Message: "bad arg"}, wantCode: 1, wantJSON: "E_VALIDATION"},
		{name: "internal exits 2", err: &mcp.ToolError{Code: mcp.CodeInternal, Message: "boom"}, wantCode: 2, wantJSON: "E_INTERNAL"},
		{name: "plain error exits 2", err: errors.New("connection refused"), wantCode: 2, wantJSON: "E_INTERNAL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var outBuf, errBuf bytes.Buffer
			cmd := &cobra.Command{Use: "maz"}
			cmd.SetOut(&outBuf)
			cmd.SetErr(&errBuf)

			err := wrapNavFatal(cmd, "get", tc.err)
			var se silentError
			if !errors.As(err, &se) {
				t.Fatalf("wrapNavFatal returned %T, want silentError", err)
			}
			if se.code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", se.code, tc.wantCode)
			}
			if !strings.HasPrefix(errBuf.String(), "get: ") {
				t.Errorf("stderr = %q, want a 'get: ' tool prefix", errBuf.String())
			}
			var env errorEnvelope
			if jerr := json.Unmarshal(outBuf.Bytes(), &env); jerr != nil {
				t.Fatalf("stdout %q is not a JSON error envelope: %v", outBuf.String(), jerr)
			}
			if env.Code != tc.wantJSON {
				t.Errorf("envelope code = %q, want %q", env.Code, tc.wantJSON)
			}
			if env.Error == "" {
				t.Errorf("envelope error message is empty; want the failure text")
			}
		})
	}
}

// ---- NDJSON: one parseable JSON object per array element ----

// ndjsonCmd returns a bare command writing to outBuf, for exercising the pure
// render helpers without a full root.
func ndjsonCmd(outBuf *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	cmd.SetOut(outBuf)
	return cmd
}

// assertNDJSONLines asserts the buffer holds exactly wantN lines, each a
// distinct parseable JSON object.
func assertNDJSONLines(t *testing.T, out string, wantN int) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != wantN {
		t.Fatalf("ndjson: expected %d lines, got %d:\n%q", wantN, len(lines), out)
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("ndjson line %d is not a valid JSON object: %v\nline=%q", i, err, line)
		}
	}
}

func TestRenderNeighborsNDJSONOneObjectPerLine(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"neighbors":[
		{"entity":{"name":"entities/01A","kind":"person","displayName":"Merry"},"via":{"predicate":"works_at"},"direction":"out","hops":1},
		{"entity":{"name":"entities/01B","kind":"person","displayName":"Sam"},"via":{"predicate":"reports_to"},"direction":"in","hops":2}
	]}`)
	var outBuf bytes.Buffer
	if err := renderNeighborsNDJSON(ndjsonCmd(&outBuf), raw); err != nil {
		t.Fatalf("renderNeighborsNDJSON: %v", err)
	}
	assertNDJSONLines(t, outBuf.String(), 2)
}

func TestRenderRelationsNDJSONOneObjectPerLine(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"relations":[
		{"predicate":"works_at","subjectName":"Merry","objectName":"Mithril","direction":"out","validUntil":""},
		{"predicate":"reports_to","subjectName":"Sam","objectName":"Merry","direction":"in","validUntil":"2025-01-01T00:00:00Z"}
	]}`)
	var outBuf bytes.Buffer
	if err := renderRelationsNDJSON(ndjsonCmd(&outBuf), raw); err != nil {
		t.Fatalf("renderRelationsNDJSON: %v", err)
	}
	assertNDJSONLines(t, outBuf.String(), 2)
}

func TestRenderEntitiesNDJSONOneObjectPerLine(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"entities":[
		{"name":"entities/01A","kind":"person","displayName":"Merry","confidence":0.9},
		{"name":"entities/01B","kind":"organization","displayName":"Mithril","confidence":0.8},
		{"name":"entities/01C","kind":"place","displayName":"Berlin","confidence":0.7}
	]}`)
	var outBuf bytes.Buffer
	if err := renderEntitiesNDJSON(ndjsonCmd(&outBuf), raw); err != nil {
		t.Fatalf("renderEntitiesNDJSON: %v", err)
	}
	assertNDJSONLines(t, outBuf.String(), 3)
}

// TestRenderEntitiesNDJSONEmptyEmitsNothing verifies that an empty array emits
// no lines (not a `{"entities":[]}` wrapper), matching the ndjson contract.
func TestRenderEntitiesNDJSONEmptyEmitsNothing(t *testing.T) {
	t.Parallel()
	var outBuf bytes.Buffer
	if err := renderEntitiesNDJSON(ndjsonCmd(&outBuf), json.RawMessage(`{"entities":[]}`)); err != nil {
		t.Fatalf("renderEntitiesNDJSON empty: %v", err)
	}
	if outBuf.Len() != 0 {
		t.Errorf("empty entities ndjson: expected no output, got %q", outBuf.String())
	}
}

// ---- blame honours --output json, not just legacy --format json ----

// TestBlameNoHistoryOutputJSONIsArray verifies that `maz blame <uid> --output
// json` on a freshly-created entity (present in the live store, no git history)
// emits a JSON array (the empty-history shape) rather than the prose
// "no version history" line. The bug: blame only honoured the legacy
// --format json and ignored the standard --output json.
func TestBlameNoHistoryOutputJSONIsArray(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	entityName := runEntityCreateCmd(t, dir, "person", "Blame JSON Tester")
	uid := strings.TrimPrefix(entityName, "entities/")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "blame", uid})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("blame --output json: %v (stderr=%q)", err, errBuf.String())
	}
	out := strings.TrimSpace(outBuf.String())
	if out == "" {
		t.Fatal("blame --output json produced empty output")
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("blame --output json is not a JSON array: %v\nout=%q", err, out)
	}
	if strings.Contains(out, "no version history") {
		t.Errorf("blame --output json leaked prose 'no version history' into JSON mode: %q", out)
	}
}

// TestBlameNoHistoryLegacyFormatJSONStillWorks guards the legacy --format json
// flag so the new --output wiring did not regress it.
func TestBlameNoHistoryLegacyFormatJSONStillWorks(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)

	entityName := runEntityCreateCmd(t, dir, "person", "Blame Legacy Tester")
	uid := strings.TrimPrefix(entityName, "entities/")

	var outBuf, errBuf bytes.Buffer
	root := makeShowBlameRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "blame", uid, "--format", "json"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("blame --format json: %v (stderr=%q)", err, errBuf.String())
	}
	out := strings.TrimSpace(outBuf.String())
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("blame --format json is not a JSON array: %v\nout=%q", err, out)
	}
}

// TestNeighborsRejectsBadDepth pins the client-side --depth bound on maz
// neighbors so it stays consistent with maz traverse / maz doc canvas (reject
// out-of-range, exit 1) rather than silently clamping. The check runs before any
// tool round-trip, so no live backend is needed.
func TestNeighborsRejectsBadDepth(t *testing.T) {
	t.Parallel()
	for _, depth := range []string{"0", "7", "99"} {
		var outBuf, errBuf bytes.Buffer
		root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
		root.PersistentFlags().String("data-dir", "", "data directory")
		root.PersistentFlags().String("output", "", "output format")
		root.AddCommand(newNeighborsCmd())
		root.SetOut(&outBuf)
		root.SetErr(&errBuf)
		root.SetArgs([]string{"--data-dir", t.TempDir(), "neighbors", "whatever", "--depth", depth})
		root.SetContext(context.Background())
		if err := root.Execute(); err == nil {
			t.Fatalf("--depth %s: expected a non-zero exit, got nil", depth)
		}
		if !strings.Contains(errBuf.String(), "must be between 1 and") {
			t.Errorf("--depth %s: stderr = %q, want a range error", depth, errBuf.String())
		}
	}
}

// ---- get --output ndjson emits parseable JSON, not text ----

// TestRenderGetNDJSONEmitsSingleParsableObject asserts that renderGetNDJSON
// emits exactly one line containing a valid JSON object, not the key/value text
// that renderGetCLI would produce. This is the regression guard for B3.0: get
// --output ndjson was silently falling back to text output.
func TestRenderGetNDJSONEmitsSingleParsableObject(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"entity","entity":{"name":"entities/01A","kind":"person","displayName":"Frodo"},"provenance":[]}`)
	var outBuf bytes.Buffer
	if err := renderGetNDJSON(ndjsonCmd(&outBuf), raw); err != nil {
		t.Fatalf("renderGetNDJSON: %v", err)
	}
	out := strings.TrimRight(outBuf.String(), "\n")
	// Must be exactly one line.
	if strings.Contains(out, "\n") {
		t.Fatalf("renderGetNDJSON emitted multiple lines: %q", outBuf.String())
	}
	// That line must be parseable JSON.
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("renderGetNDJSON output is not valid JSON: %v\nout=%q", err, out)
	}
	// Must NOT look like tab-separated text (a sign of the CLI renderer firing).
	if strings.Contains(out, "\t") {
		t.Errorf("renderGetNDJSON output contains tabs — CLI text renderer fired instead of JSON: %q", out)
	}
}

// TestRenderGetCLISurfacesShadowedDocuments verifies the text renderer prints a
// `shadowedDocument` line per entry in get's top-level shadowedDocuments envelope
// field — the signal must not be JSON-only (a CLI user picking the entity over a
// same-named note has to learn the note exists).
func TestRenderGetCLISurfacesShadowedDocuments(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"entity","entity":{"name":"entities/01PROD","kind":"product","displayName":"Mithrilmail"},` +
		`"provenance":[],"shadowedDocuments":[{"name":"entities/01DOC","uid":"01DOC"}]}`)
	var outBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.SetOut(&outBuf)
	if err := renderGetCLI(cmd, raw); err != nil {
		t.Fatalf("renderGetCLI: %v", err)
	}
	out := outBuf.String()
	if !strings.Contains(out, "shadowedDocument") {
		t.Errorf("renderGetCLI dropped the shadowedDocuments signal in text mode: %q", out)
	}
	if !strings.Contains(out, "entities/01DOC") {
		t.Errorf("renderGetCLI did not surface the shadowed doc's resource name: %q", out)
	}
}

// TestRenderGetCLINoShadowedDocumentsEmitsNoLine verifies a record without any
// shadowedDocuments envelope field emits no shadow line (omitempty absence path).
func TestRenderGetCLINoShadowedDocumentsEmitsNoLine(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"entity","entity":{"name":"entities/01A","kind":"person","displayName":"Frodo"},"provenance":[]}`)
	var outBuf bytes.Buffer
	cmd := &cobra.Command{Use: "maz"}
	cmd.SetOut(&outBuf)
	if err := renderGetCLI(cmd, raw); err != nil {
		t.Fatalf("renderGetCLI: %v", err)
	}
	if strings.Contains(outBuf.String(), "shadowedDocument") {
		t.Errorf("renderGetCLI emitted a shadow line with no shadowedDocuments present: %q", outBuf.String())
	}
}

// TestRenderGetNDJSONNilIsError verifies that renderGetNDJSON treats a nil raw
// payload (unavailable tool) as an error written to stderr, matching the
// nil-guard in emitNavResult.
func TestRenderGetNDJSONNilIsError(t *testing.T) {
	t.Parallel()
	var outBuf, errBuf bytes.Buffer
	cmd := ndjsonCmd(&outBuf)
	cmd.SetErr(&errBuf)
	err := renderGetNDJSON(cmd, nil)
	if err == nil {
		t.Fatal("renderGetNDJSON(nil) returned nil error, want a silentError")
	}
	if outBuf.Len() != 0 {
		t.Errorf("renderGetNDJSON(nil) wrote to stdout: %q", outBuf.String())
	}
	if !strings.Contains(errBuf.String(), errNavUnavailableMsg) {
		t.Errorf("renderGetNDJSON(nil) stderr = %q, want %q", errBuf.String(), errNavUnavailableMsg)
	}
}

// compile-time guard: fmt is used by the table-driven helpers above when they
// format diagnostics; keep the import meaningful if those calls change.
var _ = fmt.Sprintf
