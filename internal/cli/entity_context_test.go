package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// TestEntityContextRegisteredUnderEntity asserts that "maz entity context" is
// reachable as a subcommand of "maz entity" and that --help is non-empty.
func TestEntityContextRegisteredUnderEntity(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeEntityRoot(&buf, nil)
	root.SetArgs([]string{"entity", "context", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("entity context --help output is empty")
	}
	for _, want := range []string{
		"Examples:",
		"maz entity context",
		"--budget",
		"--at",
		"dossier",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("entity context --help missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestEntityContextHelpDistinctFromGet asserts the help text explains that
// entity context is different from maz get and maz context.
func TestEntityContextHelpDistinctFromGet(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeEntityRoot(&buf, nil)
	root.SetArgs([]string{"entity", "context", "--help"})
	_ = root.Execute()
	out := buf.String()
	if !strings.Contains(out, "maz get") {
		t.Errorf("help should distinguish entity context from maz get\nfull:\n%s", out)
	}
	if !strings.Contains(out, "maz context") {
		t.Errorf("help should distinguish entity context from maz context\nfull:\n%s", out)
	}
}

// TestEntityContextFlagsParsed asserts that --budget and --at are accepted
// without error and routed into the tool request. Uses the stub wiring so no
// daemon is required.
func TestEntityContextFlagsParsed(t *testing.T) {
	var got json.RawMessage
	tool := fakeTeamToolFunc{
		name: entityContextToolName,
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			got = req
			return json.RawMessage(`{"markdown":"# Merry\n\ntest dossier","tokens":42,"budget":500}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", t.TempDir(),
		"entity", "context", "Merry Brandybuck",
		"--budget", "500",
		"--at", "2025-01-01T00:00:00Z",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity context: %v (stderr=%q)", err, errBuf.String())
	}

	var in map[string]any
	if err := json.Unmarshal(got, &in); err != nil {
		t.Fatalf("tool request is not valid JSON: %v (%s)", err, got)
	}
	if in["entity"] != "Merry Brandybuck" {
		t.Errorf("request entity = %v, want Merry Brandybuck", in["entity"])
	}
	if budget, ok := in["tokenBudget"].(float64); !ok || int(budget) != 500 {
		t.Errorf("request tokenBudget = %v, want 500", in["tokenBudget"])
	}
	if in["at"] != "2025-01-01T00:00:00Z" {
		t.Errorf("request at = %v, want 2025-01-01T00:00:00Z", in["at"])
	}
}

// TestEntityContextDefaultBudgetOmitted asserts that when --budget is not
// passed, the tokenBudget field is absent from the request (the tool applies
// its own default of 4000).
func TestEntityContextDefaultBudgetOmitted(t *testing.T) {
	var got json.RawMessage
	tool := fakeTeamToolFunc{
		name: entityContextToolName,
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			got = req
			return json.RawMessage(`{"markdown":"# Sam\n\n","tokens":10,"budget":4000}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", t.TempDir(),
		"entity", "context", "Sam Gamgee",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity context: %v (stderr=%q)", err, errBuf.String())
	}

	var in map[string]any
	if err := json.Unmarshal(got, &in); err != nil {
		t.Fatalf("tool request is not valid JSON: %v (%s)", err, got)
	}
	if _, present := in["tokenBudget"]; present {
		t.Errorf("tokenBudget should be absent when --budget not passed, got %v", in["tokenBudget"])
	}
}

// TestEntityContextJSONMode asserts that --output json emits the raw tool
// response as parseable JSON containing at minimum the markdown, tokens, and
// budget fields.
func TestEntityContextJSONMode(t *testing.T) {
	tool := fakeTeamToolFunc{
		name: entityContextToolName,
		invoke: func(_ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"markdown":"# Pippin\n\n","tokens":7,"budget":4000,"dropped":0}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "json", "--data-dir", t.TempDir(),
		"entity", "context", "Pippin Took",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity context --output json: %v (stderr=%q)", err, errBuf.String())
	}

	var resp entityContextResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &resp); err != nil {
		t.Fatalf("--output json is not parseable JSON: %v\nout=%q", err, outBuf.String())
	}
	if resp.Markdown == "" {
		t.Error("json response missing markdown field")
	}
	if resp.Budget != 4000 {
		t.Errorf("json budget = %d, want 4000", resp.Budget)
	}
}

// TestEntityContextNotFoundExitsOne asserts that an E_NOT_FOUND from the tool
// is an expected failure that exits 1, not 2.
func TestEntityContextNotFoundExitsOne(t *testing.T) {
	tool := fakeTeamTool{
		name: entityContextToolName,
		err:  &mcp.ToolError{Code: mcp.CodeNotFound, Message: "entity not found"},
	}
	stubTeamWiring(t, tool)

	var errBuf bytes.Buffer
	root := makeEntityRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", t.TempDir(),
		"entity", "context", "Nobody Known",
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-zero exit for E_NOT_FOUND")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Errorf("want silentError code 1 (E_NOT_FOUND), got %v", err)
	}
	if !strings.Contains(errBuf.String(), "E_NOT_FOUND") {
		t.Errorf("stderr should surface E_NOT_FOUND, got %q", errBuf.String())
	}
}

// TestEntityContextAmbiguousExitsOne asserts that an E_VALIDATION from the
// tool (ambiguous display name) is an expected failure that exits 1.
func TestEntityContextAmbiguousExitsOne(t *testing.T) {
	tool := fakeTeamTool{
		name: entityContextToolName,
		err:  &mcp.ToolError{Code: mcp.CodeValidation, Message: "ambiguous name: matches Sam Gamgee, Sam Proudfoot"},
	}
	stubTeamWiring(t, tool)

	var errBuf bytes.Buffer
	root := makeEntityRoot(nil, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", t.TempDir(),
		"entity", "context", "Sam",
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-zero exit for ambiguous name (E_VALIDATION)")
	}
	var se silentError
	if !errors.As(err, &se) || se.code != 1 {
		t.Errorf("want silentError code 1 (E_VALIDATION), got %v", err)
	}
}

// TestEntityContextSelfAccepted asserts the special "self" argument is passed
// verbatim to the tool (resolution happens server-side).
func TestEntityContextSelfAccepted(t *testing.T) {
	var got json.RawMessage
	tool := fakeTeamToolFunc{
		name: entityContextToolName,
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			got = req
			return json.RawMessage(`{"markdown":"# Self\n\n","tokens":5,"budget":4000}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeEntityRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", t.TempDir(),
		"entity", "context", "self",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("entity context self: %v (stderr=%q)", err, errBuf.String())
	}

	var in map[string]any
	if err := json.Unmarshal(got, &in); err != nil {
		t.Fatalf("tool request is not valid JSON: %v (%s)", err, got)
	}
	if in["entity"] != "self" {
		t.Errorf("entity context self: request entity = %v, want self", in["entity"])
	}
}
