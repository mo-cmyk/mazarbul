package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/connect"
)

// connectTestCmd builds a cobra command carrying the --output flag that
// output.From reads, with its stdout wired to buf and a background context so
// runConnectApply's cmd.Context() resolves.
func connectTestCmd(format string, buf *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{Use: "connect"}
	cmd.Flags().String("output", format, "")
	cmd.SetOut(buf)
	cmd.SetContext(context.Background())
	return cmd
}

func TestEmitConnectResults_JSON(t *testing.T) {
	var buf bytes.Buffer
	cmd := connectTestCmd("json", &buf)
	results := []connectResult{
		{report: connect.WriteReport{
			ClientID: "cursor", Scope: connect.ScopeUser, ConfigPath: "/h/.cursor/mcp.json",
			Action: commitActionUpdated, BackupPath: "/h/.cursor/mcp.json.maz-backup-1",
		}},
		{report: connect.WriteReport{
			ClientID: "vscode", Scope: connect.ScopeUser, ConfigPath: "/h/vscode/mcp.json",
		}, err: errors.New("boom")},
	}
	// A tip is supplied but must NOT appear in JSON mode (it would corrupt the document).
	if err := emitConnectResults(cmd, results, "\nTip: do not leak me"); err != nil {
		t.Fatalf("emitConnectResults: %v", err)
	}

	if strings.Contains(buf.String(), "Tip:") {
		t.Fatalf("JSON output leaked the human tip:\n%s", buf.String())
	}
	var rows []connectReportJSON
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].ClientID != "cursor" || rows[0].Action != commitActionUpdated || rows[0].BackupPath == "" {
		t.Fatalf("row 0 = %+v, want cursor/updated with a backup path", rows[0])
	}
	if rows[0].Error != "" {
		t.Fatalf("row 0 should have no error, got %q", rows[0].Error)
	}
	if rows[1].ClientID != "vscode" || rows[1].Error != "boom" {
		t.Fatalf("row 1 = %+v, want vscode with error \"boom\"", rows[1])
	}
}

func TestEmitConnectResults_NDJSON(t *testing.T) {
	var buf bytes.Buffer
	cmd := connectTestCmd("ndjson", &buf)
	results := []connectResult{
		{report: connect.WriteReport{ClientID: "cursor", Scope: connect.ScopeUser, Action: commitActionUpdated}},
		{report: connect.WriteReport{ClientID: "codex", Scope: connect.ScopeUser, Action: "noop"}},
	}
	if err := emitConnectResults(cmd, results, "\nTip: never in ndjson"); err != nil {
		t.Fatalf("emitConnectResults: %v", err)
	}
	out := strings.TrimRight(buf.String(), "\n")
	if strings.Contains(out, "Tip:") {
		t.Fatalf("NDJSON leaked the human tip:\n%s", out)
	}
	// NDJSON contract: one standalone JSON object per line, NOT a JSON array.
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 NDJSON lines, got %d:\n%s", len(lines), out)
	}
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			t.Fatalf("line %d is a JSON array, want a bare object: %s", i, line)
		}
		var row connectReportJSON
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %d is not a valid JSON object: %v\n%s", i, err, line)
		}
	}
}

func TestEmitConnectResults_Text(t *testing.T) {
	var buf bytes.Buffer
	cmd := connectTestCmd("text", &buf)
	results := []connectResult{
		{report: connect.WriteReport{
			ClientID: "cursor", Scope: connect.ScopeUser, ConfigPath: "/h/.cursor/mcp.json", Action: commitActionUpdated,
		}},
	}
	if err := emitConnectResults(cmd, results, "\nTip: install the preset"); err != nil {
		t.Fatalf("emitConnectResults: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "cursor") || !strings.Contains(out, commitActionUpdated) {
		t.Fatalf("text output missing the report row:\n%s", out)
	}
	if !strings.Contains(out, "Tip: install the preset") {
		t.Fatalf("text output missing the onboarding tip:\n%s", out)
	}
	// Text mode must not emit a JSON array.
	if strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Fatalf("text mode emitted JSON:\n%s", out)
	}
}

// stubHookClient is a connect.Client (and HookConfigurer, so the --capture
// guard accepts it) whose Apply returns a fixed Action — letting us drive
// runConnectApply's applied/not-applied aggregation deterministically, without
// real binary resolution or disk writes.
type stubHookClient struct{ action string }

func (stubHookClient) ID() string                       { return "stub" }
func (stubHookClient) DisplayName() string              { return "Stub" }
func (stubHookClient) SupportedScopes() []connect.Scope { return []connect.Scope{connect.ScopeUser} }
func (stubHookClient) RestartHint() string              { return "" }
func (stubHookClient) ConfigPath(connect.Scope, string) (string, error) {
	return "/dev/null", nil
}

func (stubHookClient) Detect(context.Context, string) (connect.Status, error) {
	return connect.StatusInstalledNotRegistered, nil
}

func (s stubHookClient) Apply(context.Context, connect.Scope, string, connect.ServerEntry, connect.ApplyOptions) (connect.WriteReport, error) {
	return connect.WriteReport{ClientID: "stub", Action: s.action}, nil
}

func (stubHookClient) Uninstall(context.Context, connect.Scope, string) (connect.WriteReport, error) {
	return connect.WriteReport{}, nil
}

func (stubHookClient) SupportsHooks() bool { return true }

func (stubHookClient) ApplyHooks(context.Context, connect.Scope, string, connect.HookCommand, connect.HookOptions) (connect.WriteReport, error) {
	return connect.WriteReport{Action: commitActionUpdated}, nil
}

func (stubHookClient) UninstallHooks(context.Context, connect.Scope, string) (connect.WriteReport, error) {
	return connect.WriteReport{}, nil
}

// TestRunConnectApply_ConflictReportsNotApplied pins the capture-posture gate:
// a server-entry conflict writes neither the MCP entry nor the hooks, so
// runConnectApply must report applied=false (so the caller does NOT print the
// "Hooks were written…" posture). Regression for the false-confidence bug where
// printCapturePosture fired unconditionally even on a conflict.
func TestRunConnectApply_ConflictReportsNotApplied(t *testing.T) {
	svc := connect.NewService(t.TempDir(), stubHookClient{action: "conflict"})
	var buf bytes.Buffer
	cmd := connectTestCmd("text", &buf)
	applied, err := runConnectApply(cmd, svc, "stub",
		connect.ScopeUser, "", connect.ApplyOptions{Capture: true, WarmStart: true})
	if err != nil {
		t.Fatalf("runConnectApply: %v", err)
	}
	if applied {
		t.Error("conflict must report applied=false (neither the MCP entry nor hooks were written)")
	}
}

// TestRunConnectApply_SuccessReportsApplied pins the happy path: a write that
// lands (non-conflict) reports applied=true so the caller prints the posture.
func TestRunConnectApply_SuccessReportsApplied(t *testing.T) {
	svc := connect.NewService(t.TempDir(), stubHookClient{action: commitActionUpdated})
	var buf bytes.Buffer
	cmd := connectTestCmd("text", &buf)
	applied, err := runConnectApply(cmd, svc, "stub",
		connect.ScopeUser, "", connect.ApplyOptions{Capture: true})
	if err != nil {
		t.Fatalf("runConnectApply: %v", err)
	}
	if !applied {
		t.Error("a non-conflict apply must report applied=true")
	}
}
