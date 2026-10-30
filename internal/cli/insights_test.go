package cli

// insights_test.go — tests for `maz insights …`, the Insight Inbox surface.
// They pin the cli-developer two-mode contract:
//
//   1. --help is non-empty and teaches (Examples + the dismiss-is-an-event note).
//   2. CLI (text) mode produces stable, numbered output.
//   3. --output json / ndjson produce parseable, stable JSON.
//   4. Exit codes match the documented taxonomy (0 ok, 1 expected failure).
//
// The render + parse logic is exercised hermetically against synthetic tool
// JSON (no backend, always runs). The exit-code contract is exercised end-to-end
// against a live in-process backend through the already-wired get tool (CI only,
// skipped under -short like the other e2e tests). The dismiss write path's
// event-append guarantee is proven at the MCP-tool layer in
// internal/mcp/tools/insights_test.go (TestDismissInsight_AppendsEvent).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ---- help: non-empty and teaching ----

// TestInsightsHelp verifies the parent --help is non-empty, teaches the
// subcommands + examples, and is explicit that dismiss records an EVENT.
func TestInsightsHelp(t *testing.T) {
	t.Parallel()
	cmd := newInsightsCmd()
	long := cmd.Long
	if strings.TrimSpace(long) == "" {
		t.Fatal("insights Long help is empty")
	}
	for _, want := range []string{
		"Examples:", "maz insights", "show <id>", "dismiss <id>", "reflect",
		"insightDismissed", "--state", "Exit codes:",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("help missing %q", want)
		}
	}
	if strings.TrimSpace(cmd.Short) == "" {
		t.Error("insights Short help is empty")
	}
}

// TestInsightsSubcommandHelp verifies each subcommand carries non-empty,
// example-bearing help.
func TestInsightsSubcommandHelp(t *testing.T) {
	t.Parallel()
	subs := map[string]*cobra.Command{
		"list":    newInsightsListCmd(),
		"show":    newInsightsShowCmd(),
		"dismiss": newInsightsDismissCmd(),
		"reflect": newInsightsReflectCmd(),
	}
	for name, sub := range subs {
		if strings.TrimSpace(sub.Long) == "" {
			t.Errorf("%s: Long help is empty", name)
		}
		if !strings.Contains(sub.Long, "Examples:") {
			t.Errorf("%s: help missing Examples:", name)
		}
		if !strings.Contains(sub.Long, "maz insights "+name) {
			t.Errorf("%s: help missing a 'maz insights %s' example", name, name)
		}
		if strings.TrimSpace(sub.Short) == "" {
			t.Errorf("%s: Short help is empty", name)
		}
	}
	// The dismiss help must state the event-sourced, non-in-place behaviour.
	if !strings.Contains(subs["dismiss"].Long, "insightDismissed") {
		t.Error("dismiss help must mention the insightDismissed event")
	}
}

// ---- hermetic rendering: text / ndjson ----

// insightsCmd returns a bare command writing to outBuf for exercising the pure
// render helpers without a full root (mirrors timeline_test's timelineCmd).
func insightsCmd(outBuf *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	cmd.SetOut(outBuf)
	return cmd
}

// listInsightsJSON returns a listInsights-tool response with the given rows.
func listInsightsJSON(rows ...string) json.RawMessage {
	return json.RawMessage(`{"insights":[` + strings.Join(rows, ",") + `]}`)
}

// TestRenderInsightsCLINumbered verifies text mode numbers the rows and shows
// the statement, citation count, confidence, state, and resource name.
func TestRenderInsightsCLINumbered(t *testing.T) {
	t.Parallel()
	raw := listInsightsJSON(
		`{"name":"insights/01A","statement":"Frodo trusts Sam.","state":"hot","currentConfidence":0.95,"sourceCount":2}`,
		`{"name":"insights/01B","statement":"Merry likes the Shire.","state":"hot","currentConfidence":0.60,"sourceCount":1}`,
	)
	var outBuf bytes.Buffer
	if err := renderInsightsCLI(insightsCmd(&outBuf), raw, 50); err != nil {
		t.Fatalf("renderInsightsCLI: %v", err)
	}
	out := outBuf.String()
	for _, want := range []string{"1", "2", "Frodo trusts Sam.", "Merry likes the Shire.", "insights/01A", "0.95", "hot"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q\nout=%q", want, out)
		}
	}
}

// TestRenderInsightsCLIEmpty verifies the empty-listing line.
func TestRenderInsightsCLIEmpty(t *testing.T) {
	t.Parallel()
	var outBuf bytes.Buffer
	if err := renderInsightsCLI(insightsCmd(&outBuf), listInsightsJSON(), 50); err != nil {
		t.Fatalf("renderInsightsCLI empty: %v", err)
	}
	if !strings.Contains(outBuf.String(), "no insights") {
		t.Errorf("empty output = %q, want 'no insights'", outBuf.String())
	}
}

// TestRenderInsightsNDJSON verifies --output ndjson emits one parseable JSON
// object per insight, matching the entities/relations/neighbors contract.
func TestRenderInsightsNDJSON(t *testing.T) {
	t.Parallel()
	raw := listInsightsJSON(
		`{"name":"insights/01A","statement":"A","state":"hot","currentConfidence":0.9,"sourceCount":1}`,
		`{"name":"insights/01B","statement":"B","state":"demoted","currentConfidence":0.3,"sourceCount":0}`,
	)
	var outBuf bytes.Buffer
	if err := renderInsightsNDJSON(insightsCmd(&outBuf), raw); err != nil {
		t.Fatalf("renderInsightsNDJSON: %v", err)
	}
	lines := strings.Split(strings.TrimRight(outBuf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("ndjson: want 2 lines, got %d:\n%q", len(lines), outBuf.String())
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("ndjson line %d not a JSON object: %v\nline=%q", i, err, line)
		}
	}
}

// TestRenderInsightShowCLI verifies the single-insight card renders the
// statement, resource name, meta, and typed entity citations.
func TestRenderInsightShowCLI(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"insight","insight":{
		"uid":"01A","name":"insights/01A","statement":"Frodo trusts Sam.",
		"insightType":"pattern","state":"hot","currentConfidence":0.9,
		"synthesizedAt":"2026-06-17T10:00:00Z",
		"sourceUids":["ent1","ent2"],"sourceCitations":["entities/ent1","entities/ent2"]}}`)
	var outBuf bytes.Buffer
	if err := renderInsightShowCLI(insightsCmd(&outBuf), raw); err != nil {
		t.Fatalf("renderInsightShowCLI: %v", err)
	}
	out := outBuf.String()
	for _, want := range []string{"Frodo trusts Sam.", "insights/01A", "pattern", "evidence", "entities/ent1", "entities/ent2"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q\nout=%q", want, out)
		}
	}
}

// TestRenderInsightShowCLINonInsight verifies a non-insight record is echoed
// rather than mis-rendered (defensive: get may resolve another kind).
func TestRenderInsightShowCLINonInsight(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"entity","entity":{"uid":"x","displayName":"Sam"}}`)
	var outBuf bytes.Buffer
	if err := renderInsightShowCLI(insightsCmd(&outBuf), raw); err != nil {
		t.Fatalf("renderInsightShowCLI(non-insight): %v", err)
	}
	if !strings.Contains(outBuf.String(), "Sam") {
		t.Errorf("non-insight output should echo the payload, got %q", outBuf.String())
	}
}

// TestRenderInsightsReflectCLI covers the three reflection outcomes.
func TestRenderInsightsReflectCLI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"synthesized", `{"appendedEventUids":["e1","e2"],"appendedCount":2}`, "synthesized 2"},
		{"skipped", `{"appendedEventUids":[],"appendedCount":0,"skipped":"breaker-open"}`, "skipped: breaker-open"},
		{"none", `{"appendedEventUids":[],"appendedCount":0}`, "no new insights"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var outBuf bytes.Buffer
			if err := renderInsightsReflectCLI(insightsCmd(&outBuf), json.RawMessage(c.raw)); err != nil {
				t.Fatalf("renderInsightsReflectCLI: %v", err)
			}
			if !strings.Contains(outBuf.String(), c.want) {
				t.Errorf("reflect output = %q, want substring %q", outBuf.String(), c.want)
			}
		})
	}
}

// ---- end-to-end: live in-process backend (CI only; skipped under -short) ----

// makeInsightsRoot wires a minimal root carrying the persistent flags
// invokeNavTool + output.From read, plus the insights command tree.
func makeInsightsRoot(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().String("output", "", "output format (text|json|ndjson)")
	root.AddCommand(newInsightsCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// TestInsightsE2EShowUnknownExitsOne pins the exit-code contract: showing a
// well-formed but nonexistent insight is an expected failure (E_NOT_FOUND) and
// exits 1 (not 2), emitting the shared JSON error envelope. It routes through
// the already-wired get tool, so it holds regardless of the listInsights /
// dismissInsight registration.
func TestInsightsE2EShowUnknownExitsOne(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir) // skips under -short; boots the live in-process backend

	var outBuf, errBuf bytes.Buffer
	root := makeInsightsRoot(&outBuf, &errBuf)
	root.SetArgs([]string{
		"--data-dir", dir, "--output", "json",
		"insights", "show", "insights/01ARZ3NDEKTSV4RRFFQ69G5FAV",
	})
	root.SetContext(context.Background())
	err := root.Execute()
	if err == nil {
		t.Fatal("insights show on an unknown insight exited 0; want a non-zero exit")
	}
	var se silentError
	if !errors.As(err, &se) {
		t.Fatalf("error is %T, want silentError", err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1 (expected failure)", se.code)
	}
	if !strings.Contains(outBuf.String(), "E_NOT_FOUND") {
		t.Errorf("json error envelope = %q, want an E_NOT_FOUND code", outBuf.String())
	}
}
