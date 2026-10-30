package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/connect"
)

// These tests pin Claude Code's lifecycle-hook behavior (issue #228) against the
// generic adapter built from the claude-code descriptor — the descriptor's
// HookSpec is what makes the adapter satisfy connect.HookConfigurer and honor
// --capture/--warm-start. They exercise the real descriptor, not a stub.

// claudeCodeHooks returns the claude-code adapter as a connect.HookConfigurer,
// failing the test if the descriptor no longer declares a hook spec.
func claudeCodeHooks(t *testing.T) connect.HookConfigurer { //nolint:ireturn // test helper returns the interface.
	t.Helper()
	hc, ok := find(t, "claude-code").(connect.HookConfigurer)
	if !ok || !hc.SupportsHooks() {
		t.Fatal("claude-code adapter does not support lifecycle hooks")
	}
	return hc
}

// withTempHome points $HOME at a temp dir so ConfigPath (~/.claude.json) and the
// hook config path (~/.claude/settings.json) resolve under the test's control.
// Not parallel-safe (mutates process env).
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func readSettings(t *testing.T, home string) map[string]any {
	t.Helper()
	path := filepath.Join(home, ".claude", "settings.json")
	doc, err := connect.ReadJSONFile(path)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	return doc
}

func TestClaudeCodeApplyHooks_WritesBothBlocks(t *testing.T) {
	home := withTempHome(t)
	cc := claudeCodeHooks(t)
	hookCmd := connect.HookCommand{Command: "maz"}

	rep, err := cc.ApplyHooks(context.Background(), connect.ScopeUser, "", hookCmd,
		connect.HookOptions{Capture: true, WarmStart: true})
	if err != nil {
		t.Fatalf("ApplyHooks: %v", err)
	}
	if rep.Action != "updated" {
		t.Errorf("action = %q, want updated", rep.Action)
	}
	doc := readSettings(t, home)
	if !connect.HasMazHookEntry(doc, "Stop") {
		t.Error("Stop hook missing")
	}
	if !connect.HasMazHookEntry(doc, "SessionStart") {
		t.Error("SessionStart hook missing")
	}
	js := mustJSONStr(t, doc)
	if !strings.Contains(js, "maz hook stop") || !strings.Contains(js, "maz hook session-start --warm-start") {
		t.Errorf("hook commands missing from settings.json:\n%s", js)
	}
}

func TestClaudeCodeApplyHooks_NonDestructiveMergeAndBackup(t *testing.T) {
	home := withTempHome(t)
	// Seed a user-authored Stop hook + an unrelated top-level key.
	settingsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seed := map[string]any{
		"model": "claude-sonnet-4-6",
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "user-script.sh"}}},
			},
		},
	}
	seedBytes, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), seedBytes, 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	cc := claudeCodeHooks(t)
	rep, err := cc.ApplyHooks(context.Background(), connect.ScopeUser, "",
		connect.HookCommand{Command: "maz"}, connect.HookOptions{Capture: true})
	if err != nil {
		t.Fatalf("ApplyHooks: %v", err)
	}
	if rep.BackupPath == "" {
		t.Error("expected a backup of the pre-existing settings.json")
	}
	doc := readSettings(t, home)
	if doc["model"] != "claude-sonnet-4-6" {
		t.Error("unrelated top-level key was dropped")
	}
	js := mustJSONStr(t, doc)
	if !strings.Contains(js, "user-script.sh") {
		t.Error("user-authored sibling hook was dropped")
	}
	if !strings.Contains(js, "maz hook stop") {
		t.Error("Mazarbul Stop hook was not appended")
	}
}

// TestClaudeCodeApply_NoFlagsWritesNoHooks pins issue #228 acceptance #5: a
// plain apply (no --capture/--warm-start) touches only the MCP config and never
// writes a hooks file.
func TestClaudeCodeApply_NoFlagsWritesNoHooks(t *testing.T) {
	home := withTempHome(t)
	cc := find(t, "claude-code")
	entry := connect.ServerEntry{Command: "maz", Args: []string{"mcp", "serve"}}

	if _, err := cc.Apply(context.Background(), connect.ScopeUser, "", entry, connect.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("plain apply must not write settings.json; stat err = %v", err)
	}
}

// TestClaudeCodeApply_CaptureWritesHooks pins that the full Apply path writes
// the MCP entry AND the hook block when --capture is set.
func TestClaudeCodeApply_CaptureWritesHooks(t *testing.T) {
	home := withTempHome(t)
	cc := find(t, "claude-code")
	entry := connect.ServerEntry{Command: "maz", Args: []string{"mcp", "serve"}}

	if _, err := cc.Apply(context.Background(), connect.ScopeUser, "", entry,
		connect.ApplyOptions{Capture: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !connect.HasMazHookEntry(readSettings(t, home), "Stop") {
		t.Error("--capture apply did not write the Stop hook")
	}
	// The MCP server entry still landed in the separate ~/.claude.json file.
	mcpDoc, err := connect.ReadJSONFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	servers, _ := mcpDoc["mcpServers"].(map[string]any)
	if _, ok := servers[connect.MazServerKey]; !ok {
		t.Error("MCP server entry missing from ~/.claude.json")
	}
}

func TestClaudeCodeUninstall_RemovesHooksAndServer(t *testing.T) {
	home := withTempHome(t)
	cc := find(t, "claude-code")
	entry := connect.ServerEntry{Command: "maz", Args: []string{"mcp", "serve"}}
	if _, err := cc.Apply(context.Background(), connect.ScopeUser, "", entry,
		connect.ApplyOptions{Capture: true, WarmStart: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if _, err := cc.Uninstall(context.Background(), connect.ScopeUser, ""); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	doc := readSettings(t, home)
	if connect.HasMazHookEntry(doc, "Stop") || connect.HasMazHookEntry(doc, "SessionStart") {
		t.Error("uninstall left Mazarbul hooks behind")
	}
	mcpDoc, _ := connect.ReadJSONFile(filepath.Join(home, ".claude.json"))
	servers, _ := mcpDoc["mcpServers"].(map[string]any)
	if _, ok := servers[connect.MazServerKey]; ok {
		t.Error("uninstall left the MCP server entry behind")
	}
}

func TestClaudeCodeApplyHooks_DryRunNoWrite(t *testing.T) {
	home := withTempHome(t)
	cc := claudeCodeHooks(t)
	if _, err := cc.ApplyHooks(context.Background(), connect.ScopeUser, "",
		connect.HookCommand{Command: "maz"}, connect.HookOptions{Capture: true, DryRun: true}); err != nil {
		t.Fatalf("ApplyHooks dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("dry-run must not write settings.json; stat err = %v", err)
	}
}

// TestClaudeCodeApplyHooks_ProjectScope pins that project scope targets
// <projectRoot>/.claude/settings.json, not the user file.
func TestClaudeCodeApplyHooks_ProjectScope(t *testing.T) {
	home := withTempHome(t)
	projectRoot := t.TempDir()
	cc := claudeCodeHooks(t)
	if _, err := cc.ApplyHooks(context.Background(), connect.ScopeProject, projectRoot,
		connect.HookCommand{Command: "maz"}, connect.HookOptions{Capture: true}); err != nil {
		t.Fatalf("ApplyHooks project: %v", err)
	}
	projDoc, err := connect.ReadJSONFile(filepath.Join(projectRoot, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read project settings: %v", err)
	}
	if !connect.HasMazHookEntry(projDoc, "Stop") {
		t.Error("project-scope Stop hook missing from <projectRoot>/.claude/settings.json")
	}
	// The user-scope settings.json must be untouched.
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("project-scope apply wrote the user settings.json; stat err = %v", err)
	}
}

// TestClaudeCodeApplyHooks_FlagSelectsEvent pins that each flag writes ONLY its
// own event: --capture → Stop (no SessionStart), --warm-start → SessionStart
// (no Stop).
func TestClaudeCodeApplyHooks_FlagSelectsEvent(t *testing.T) {
	t.Run("capture only writes Stop", func(t *testing.T) {
		home := withTempHome(t)
		cc := claudeCodeHooks(t)
		if _, err := cc.ApplyHooks(context.Background(), connect.ScopeUser, "",
			connect.HookCommand{Command: "maz"}, connect.HookOptions{Capture: true}); err != nil {
			t.Fatalf("ApplyHooks: %v", err)
		}
		doc := readSettings(t, home)
		if !connect.HasMazHookEntry(doc, "Stop") {
			t.Error("--capture should write the Stop hook")
		}
		if connect.HasMazHookEntry(doc, "SessionStart") {
			t.Error("--capture must not write a SessionStart hook")
		}
	})
	t.Run("warm-start only writes SessionStart", func(t *testing.T) {
		home := withTempHome(t)
		cc := claudeCodeHooks(t)
		if _, err := cc.ApplyHooks(context.Background(), connect.ScopeUser, "",
			connect.HookCommand{Command: "maz"}, connect.HookOptions{WarmStart: true}); err != nil {
			t.Fatalf("ApplyHooks: %v", err)
		}
		doc := readSettings(t, home)
		if !connect.HasMazHookEntry(doc, "SessionStart") {
			t.Error("--warm-start should write the SessionStart hook")
		}
		if connect.HasMazHookEntry(doc, "Stop") {
			t.Error("--warm-start must not write a Stop hook")
		}
	})
}

// TestClaudeCodeApply_NoFlagsLeavesExistingSettingsUntouched pins the harder
// half of issue #228 acceptance #5: when settings.json ALREADY exists, a plain
// apply (no --capture/--warm-start) must not rewrite or touch it.
func TestClaudeCodeApply_NoFlagsLeavesExistingSettingsUntouched(t *testing.T) {
	home := withTempHome(t)
	settingsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	settingsPath := filepath.Join(settingsDir, "settings.json")
	original := []byte(`{"model":"claude-sonnet-4-6","hooks":{"Stop":[{"hooks":[{"type":"command","command":"user-script.sh"}]}]}}`)
	if err := os.WriteFile(settingsPath, original, 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	before, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	cc := find(t, "claude-code")
	entry := connect.ServerEntry{Command: "maz", Args: []string{"mcp", "serve"}}
	if _, err := cc.Apply(context.Background(), connect.ScopeUser, "", entry, connect.ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("plain apply rewrote an existing settings.json\nbefore: %s\nafter:  %s", before, after)
	}
}

func mustJSONStr(t *testing.T, doc map[string]any) string {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
