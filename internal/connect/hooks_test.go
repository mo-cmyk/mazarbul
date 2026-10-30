package connect

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHookCommand_CommandLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cmd  HookCommand
		args []string
		want string
	}{
		{
			name: "bare binary no env",
			cmd:  HookCommand{Command: "maz"},
			args: []string{"hook", "stop"},
			want: "maz hook stop",
		},
		{
			name: "absolute binary with data dir env",
			cmd:  HookCommand{Command: "/usr/local/bin/maz", Env: map[string]string{"MAZ_DATA_DIR": "/srv/maz"}},
			args: []string{"hook", "session-start", "--warm-start"},
			want: "MAZ_DATA_DIR=/srv/maz /usr/local/bin/maz hook session-start --warm-start",
		},
		{
			name: "data dir with spaces is quoted",
			cmd:  HookCommand{Command: "maz", Env: map[string]string{"MAZ_DATA_DIR": "/Users/Jane Doe/maz"}},
			args: []string{"hook", "stop"},
			want: "MAZ_DATA_DIR='/Users/Jane Doe/maz' maz hook stop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cmd.CommandLine(tc.args...); got != tc.want {
				t.Errorf("CommandLine = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHookCommandFromEntry_PreservesBinaryAndEnv(t *testing.T) {
	t.Parallel()
	entry := ServerEntry{Command: "/abs/maz", Args: []string{"mcp", "serve"}, Env: map[string]string{"MAZ_DATA_DIR": "/d"}}
	hc := HookCommandFromEntry(entry)
	if hc.Command != "/abs/maz" {
		t.Errorf("Command = %q, want /abs/maz", hc.Command)
	}
	if hc.Env["MAZ_DATA_DIR"] != "/d" {
		t.Errorf("Env MAZ_DATA_DIR = %q, want /d", hc.Env["MAZ_DATA_DIR"])
	}
}

func TestUpsertHookEntry_AppendsAndReplaces(t *testing.T) {
	t.Parallel()
	doc := map[string]any{}
	UpsertHookEntry(doc, "Stop", "maz hook stop")
	if !HasMazHookEntry(doc, "Stop") {
		t.Fatal("Stop entry missing after first upsert")
	}
	// Idempotent replace: a second upsert must not duplicate the entry.
	UpsertHookEntry(doc, "Stop", "MAZ_DATA_DIR=/d maz hook stop")
	hooks, _ := doc["hooks"].(map[string]any)
	arr, _ := hooks["Stop"].([]any)
	if len(arr) != 1 {
		t.Errorf("Stop array has %d entries after replace, want 1", len(arr))
	}
}

func TestUpsertHookEntry_PreservesSiblings(t *testing.T) {
	t.Parallel()
	doc := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "user-script.sh"}}},
			},
		},
	}
	UpsertHookEntry(doc, "Stop", "maz hook stop")
	hooks, _ := doc["hooks"].(map[string]any)
	arr, _ := hooks["Stop"].([]any)
	if len(arr) != 2 {
		t.Fatalf("Stop array has %d entries, want 2 (sibling + maz)", len(arr))
	}
	// The user's sibling hook is still present.
	if !strings.Contains(mustJSON(t, doc), "user-script.sh") {
		t.Error("sibling hook was dropped")
	}
}

// TestUpsertHookEntry_DoesNotClobberLookalikeHook pins ownership detection by
// binary basename: a user hook whose command merely resembles ours is NOT
// treated as Mazarbul-owned (preserved on upsert, left alone on uninstall)
// unless its invoked binary basename is actually `maz`/`mazarbul`.
func TestUpsertHookEntry_DoesNotClobberLookalikeHook(t *testing.T) {
	t.Parallel()
	lookalikes := []string{
		"git-hook stop",            // unrelated command containing "hook stop"
		"my-tool hook stop",        // a different binary with a "hook stop" subcommand
		"/opt/bin/mymaz hook stop", // basename "mymaz" — "maz" is only a suffix
		"pre-commit-hook session-start",
	}
	for _, cmd := range lookalikes {
		doc := map[string]any{
			"hooks": map[string]any{
				"Stop": []any{
					map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}},
				},
			},
		}
		UpsertHookEntry(doc, "Stop", "maz hook stop")
		hooks, _ := doc["hooks"].(map[string]any)
		arr, _ := hooks["Stop"].([]any)
		if len(arr) != 2 {
			t.Fatalf("%q: Stop array has %d entries, want 2 (lookalike preserved + maz appended)", cmd, len(arr))
		}
		if !strings.Contains(mustJSON(t, doc), cmd) {
			t.Errorf("a lookalike user hook %q was clobbered on upsert", cmd)
		}
		// And the lookalike is NOT removed on uninstall.
		RemoveHookEntries(doc, "Stop")
		if !strings.Contains(mustJSON(t, doc), cmd) {
			t.Errorf("uninstall removed a lookalike user hook %q", cmd)
		}
	}
}

// TestCommandIsMazHook_BasenameBoundary pins that ownership requires the
// maz/mazarbul binary on a basename boundary, across env prefixes and absolute
// paths, and rejects suffix-only lookalikes.
func TestCommandIsMazHook_BasenameBoundary(t *testing.T) {
	t.Parallel()
	owned := []string{
		"maz hook stop",
		"mazarbul hook stop",
		"/usr/local/bin/maz hook stop",
		"MAZ_DATA_DIR=/srv/maz /usr/local/bin/maz hook session-start --warm-start",
		"MAZ_DATA_DIR='/Users/Jane Doe/maz' maz hook stop",
	}
	notOwned := []string{
		"git-hook stop",
		"my-tool hook stop",
		"/opt/mymaz hook stop",
		"maz remember",
		"maz hook",
		"echo maz hooks stop",
		// `maz` as an ARGUMENT to another binary, not the invoked program —
		// must not be misclassified as ours (would be removed on uninstall).
		"echo maz hook stop",
		"logger maz hook stop",
		"sh maz hook stop",
		"/usr/bin/strace maz hook stop",
	}
	for _, c := range owned {
		if !commandIsMazHook(c) {
			t.Errorf("commandIsMazHook(%q) = false, want true", c)
		}
	}
	for _, c := range notOwned {
		if commandIsMazHook(c) {
			t.Errorf("commandIsMazHook(%q) = true, want false", c)
		}
	}
}

func TestRemoveHookEntries_OnlyMazAndCleansUp(t *testing.T) {
	t.Parallel()
	doc := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "user-script.sh"}}},
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "maz hook stop"}}},
			},
			"SessionStart": []any{
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "maz hook session-start --warm-start"}}},
			},
		},
	}
	if !RemoveHookEntries(doc, "Stop") {
		t.Error("RemoveHookEntries(Stop) reported nothing removed")
	}
	if !RemoveHookEntries(doc, "SessionStart") {
		t.Error("RemoveHookEntries(SessionStart) reported nothing removed")
	}
	js := mustJSON(t, doc)
	if strings.Contains(js, "maz hook") {
		t.Errorf("maz hook entries survived removal:\n%s", js)
	}
	if !strings.Contains(js, "user-script.sh") {
		t.Errorf("sibling hook was dropped:\n%s", js)
	}
	// SessionStart had only the maz entry, so the empty event key is dropped.
	if strings.Contains(js, "SessionStart") {
		t.Errorf("emptied SessionStart key should be removed:\n%s", js)
	}
}

func TestRemoveHookEntries_NoopWhenAbsent(t *testing.T) {
	t.Parallel()
	if RemoveHookEntries(map[string]any{}, "Stop") {
		t.Error("RemoveHookEntries on empty doc should report nothing removed")
	}
}

// stubClient is a minimal Client that does NOT implement HookConfigurer, used
// to pin the Service guard.
type stubClient struct{}

func (stubClient) ID() string                                     { return "stub" }
func (stubClient) DisplayName() string                            { return "Stub" }
func (stubClient) SupportedScopes() []Scope                       { return []Scope{ScopeUser} }
func (stubClient) Detect(context.Context, string) (Status, error) { return StatusNotInstalled, nil }
func (stubClient) ConfigPath(Scope, string) (string, error)       { return "/dev/null", nil }
func (stubClient) Uninstall(context.Context, Scope, string) (WriteReport, error) {
	return WriteReport{}, nil
}
func (stubClient) RestartHint() string { return "" }
func (stubClient) Apply(context.Context, Scope, string, ServerEntry, ApplyOptions) (WriteReport, error) {
	return WriteReport{Action: "updated"}, nil
}

func TestServiceApply_RejectsHooksForNonHookClient(t *testing.T) {
	t.Parallel()
	svc := NewService("", stubClient{})
	_, err := svc.Apply(context.Background(), "stub", ScopeUser, "", ApplyOptions{Capture: true})
	if err == nil || !strings.Contains(err.Error(), "does not support lifecycle hooks") {
		t.Fatalf("want lifecycle-hooks rejection, got %v", err)
	}
}

func mustJSON(t *testing.T, doc map[string]any) string {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	return string(b)
}
