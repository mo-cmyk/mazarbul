package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// makeReferenceRoot wires a minimal cobra root carrying the reference command
// plus the persistent flags it reads via cmd.Root().PersistentFlags().
func makeReferenceRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newReferenceCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// TestReferenceRoutesRequestToTool pins the CLI contract: `maz reference <uri>`
// builds the {uri, label, scope} request, routes it through the reference tool,
// and prints the returned resource name.
func TestReferenceRoutesRequestToTool(t *testing.T) {
	var got json.RawMessage
	tool := fakeTeamToolFunc{
		name: "reference",
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			got = req
			return json.RawMessage(`{"name":"entities/01ARZ3NDEKTSV4RRFFQ69G5FAV","created":true}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeReferenceRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--output", "text", "--data-dir", t.TempDir(), "reference",
		"https://github.com/org/repo/issues/42", "--scope", "project", "--label", "issue #42",
	})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("reference: %v (stderr=%q)", err, errBuf.String())
	}

	var in map[string]any
	if err := json.Unmarshal(got, &in); err != nil {
		t.Fatalf("request not valid JSON: %v (%s)", err, got)
	}
	if in["uri"] != "https://github.com/org/repo/issues/42" {
		t.Errorf("request uri = %v", in["uri"])
	}
	if in["label"] != "issue #42" {
		t.Errorf("request label = %v, want \"issue #42\"", in["label"])
	}
	if in["scope"] != "project" {
		t.Errorf("request scope = %v, want project", in["scope"])
	}
	if printed := strings.TrimSpace(outBuf.String()); printed != "entities/01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("printed name = %q, want the tool's entities/{uid}", printed)
	}
}

// TestReferenceOmitsUnsetFlags pins that omitted --label/--scope stay off the
// request body so the tool applies its own defaults (personal scope, URI label).
func TestReferenceOmitsUnsetFlags(t *testing.T) {
	var got json.RawMessage
	tool := fakeTeamToolFunc{
		name: "reference",
		invoke: func(req json.RawMessage) (json.RawMessage, error) {
			got = req
			return json.RawMessage(`{"name":"entities/01ARZ3NDEKTSV4RRFFQ69G5FAV","created":true}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeReferenceRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--output", "text", "--data-dir", t.TempDir(), "reference", "mailto:alice@example.com"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("reference: %v (stderr=%q)", err, errBuf.String())
	}
	var in map[string]any
	if err := json.Unmarshal(got, &in); err != nil {
		t.Fatalf("request not valid JSON: %v (%s)", err, got)
	}
	if _, ok := in["label"]; ok {
		t.Errorf("label should be absent when --label is unset, got %v", in["label"])
	}
	if _, ok := in["scope"]; ok {
		t.Errorf("scope should be absent when --scope is unset, got %v", in["scope"])
	}
}

// TestReferenceJSON: --output json passes the tool response through verbatim.
func TestReferenceJSON(t *testing.T) {
	tool := fakeTeamToolFunc{
		name: "reference",
		invoke: func(_ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"name":"entities/01ARZ3NDEKTSV4RRFFQ69G5FAV","uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","uri":"https://x.example","scope":"personal","created":true}`), nil
		},
	}
	stubTeamWiring(t, tool)

	var outBuf, errBuf bytes.Buffer
	root := makeReferenceRoot(&outBuf, &errBuf)
	root.SetArgs([]string{"--output", "json", "--data-dir", t.TempDir(), "reference", "https://x.example"})
	root.SetContext(context.Background())
	if err := root.Execute(); err != nil {
		t.Fatalf("reference --output json: %v (stderr=%q)", err, errBuf.String())
	}
	var resp struct {
		Name    string `json:"name"`
		Created bool   `json:"created"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &resp); err != nil {
		t.Fatalf("parse json: %v (out=%q)", err, outBuf.String())
	}
	if !strings.HasPrefix(resp.Name, "entities/") || !resp.Created {
		t.Errorf("json resp = %+v", resp)
	}
}

// TestReferenceHelp: --help is non-empty and advertises the relate pairing.
func TestReferenceHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeReferenceRoot(&buf, nil)
	root.SetArgs([]string{"reference", "--help"})
	_ = root.Execute()
	out := buf.String()
	for _, want := range []string{"Examples:", "maz reference", "has_primary_source"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}
