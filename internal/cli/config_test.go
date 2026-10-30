package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// makeConfigCmd wires a minimal cobra tree for config subcommand tests.
func makeConfigCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().String("output", "", "output format")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	cfg := newConfigCmd()
	root.AddCommand(cfg)
	if outBuf != nil {
		root.SetOut(outBuf)
		cfg.SetOut(outBuf)
		for _, sub := range cfg.Commands() {
			sub.SetOut(outBuf)
		}
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		cfg.SetErr(errBuf)
		for _, sub := range cfg.Commands() {
			sub.SetErr(errBuf)
		}
	}
	return root
}

// TestConfigGetHelp: --help is non-empty and contains the Examples section.
func TestConfigGetHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeConfigCmd(&buf, nil)
	root.SetArgs([]string{"config", "get", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{"maz config get", "key"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestConfigSetHelp: --help output is non-empty and includes examples.
func TestConfigSetHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeConfigCmd(&buf, nil)
	root.SetArgs([]string{"config", "set", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{"maz config set", "defaults.chat", "recall"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestConfigListHelp: --help is non-empty and mentions --sources.
func TestConfigListHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeConfigCmd(&buf, nil)
	root.SetArgs([]string{"config", "list", "--help"})
	_ = root.Execute()
	out := buf.String()
	if !strings.Contains(out, "--sources") {
		t.Errorf("help missing --sources\nfull:\n%s", out)
	}
}

// TestConfigPathHelp: --help is non-empty.
func TestConfigPathHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeConfigCmd(&buf, nil)
	root.SetArgs([]string{"config", "path", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
}

// TestConfigSet_WalkDotted: setDotted correctly sets nested keys.
func TestConfigSet_WalkDotted(t *testing.T) {
	t.Parallel()
	m := map[string]any{
		"defaults": map[string]any{"chat": "anthropic"},
	}
	if err := setDotted(m, "defaults.chat", "ollama"); err != nil {
		t.Fatalf("setDotted: %v", err)
	}
	sub := m["defaults"].(map[string]any)
	if sub["chat"] != "ollama" {
		t.Errorf("chat = %v, want ollama", sub["chat"])
	}
}

// TestConfigSet_InferType: inferType correctly parses bool, float, string.
func TestConfigSet_InferType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  any
	}{
		{"true", true},
		{"false", false},
		{"0.7", 0.7},
		{"42", float64(42)},
		{"ollama", "ollama"},
	}
	for _, tt := range tests {
		got := inferType(tt.input)
		switch want := tt.want.(type) {
		case float64:
			if f, ok := got.(float64); !ok || f != want {
				t.Errorf("inferType(%q) = %v (%T), want %v", tt.input, got, got, want)
			}
		default:
			if got != tt.want {
				t.Errorf("inferType(%q) = %v (%T), want %v (%T)", tt.input, got, got, tt.want, tt.want)
			}
		}
	}
}

// TestWalkDotted: walkDotted returns correct values for nested paths.
func TestWalkDotted(t *testing.T) {
	t.Parallel()
	m := map[string]any{
		"defaults": map[string]any{
			"chat":  "anthropic",
			"embed": "ollama",
		},
		"recall": map[string]any{
			"mmr_lambda": 0.7,
		},
	}
	got, ok := walkDotted(m, "defaults.chat")
	if !ok {
		t.Fatal("walkDotted(defaults.chat): not found")
	}
	if got != "anthropic" {
		t.Errorf("defaults.chat = %v, want anthropic", got)
	}
	got, ok = walkDotted(m, "recall.mmr_lambda")
	if !ok {
		t.Fatal("walkDotted(recall.mmr_lambda): not found")
	}
	if got != 0.7 {
		t.Errorf("recall.mmr_lambda = %v, want 0.7", got)
	}
	_, ok = walkDotted(m, "nonexistent.key")
	if ok {
		t.Error("expected not-found for nonexistent.key")
	}
}

// TestIsKnownConfigKey pins the documented snake_case key surface — including
// the ,omitempty tables (sync.*, recall.*) that a marshalled-empty-config check
// used to wrongly reject, which is what broke `config set sync.remotes.x.url`.
func TestIsKnownConfigKey(t *testing.T) {
	t.Parallel()
	known := []string{
		"schema_version",
		"defaults.chat", "defaults.embed",
		"providers.ollama.chat_model", "providers.anthropic.model",
		"sync.remotes.personal.url", "sync.remotes.team.auth_method",
		"sync.auto_push", "sync.coalesce_seconds",
		"log.level", "vault.path", "recall.mmr_lambda",
	}
	for _, k := range known {
		if !isKnownConfigKey(k) {
			t.Errorf("isKnownConfigKey(%q) = false, want true (documented key)", k)
		}
	}
	unknown := []string{
		"", "defaults.chatt", "sync.bogus", "providers.ollama.model.deep.typo",
		"sync.remotes.personal.url.extra", "nonexistent",
	}
	for _, k := range unknown {
		if isKnownConfigKey(k) {
			t.Errorf("isKnownConfigKey(%q) = true, want false (typo/too-deep)", k)
		}
	}
}

// TestConfigSetGet_DocumentedKeysRoundTrip drives the user-facing command: the
// lowercase dotted keys the docs and help text teach (defaults.chat, the sync
// remote URL, sync.auto_push) must set and read back. Before json tags landed,
// every one returned "unknown key", forcing users to hand-edit config.toml.
func TestConfigSetGet_DocumentedKeysRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	set := func(key, val string) {
		t.Helper()
		var out, errBuf bytes.Buffer
		root := makeConfigCmd(&out, &errBuf)
		root.SetArgs([]string{"--data-dir", dir, "config", "set", key, val})
		if err := root.Execute(); err != nil {
			t.Fatalf("config set %s %s: %v (stderr: %s)", key, val, err, errBuf.String())
		}
	}
	get := func(key string) string {
		t.Helper()
		var out, errBuf bytes.Buffer
		root := makeConfigCmd(&out, &errBuf)
		root.SetArgs([]string{"--data-dir", dir, "config", "get", key})
		if err := root.Execute(); err != nil {
			t.Fatalf("config get %s: %v (stderr: %s)", key, err, errBuf.String())
		}
		return out.String()
	}

	set("defaults.chat", "ollama")
	set("sync.remotes.personal.url", "file:///tmp/mirror.git")
	set("sync.auto_push", "true")

	if got := get("sync.remotes.personal.url"); !strings.Contains(got, "file:///tmp/mirror.git") {
		t.Errorf("get sync.remotes.personal.url = %q, want the url set above", got)
	}
	if got := get("defaults.chat"); !strings.Contains(got, "ollama") {
		t.Errorf("get defaults.chat = %q, want ollama", got)
	}
	if got := get("sync.auto_push"); !strings.Contains(got, "true") {
		t.Errorf("get sync.auto_push = %q, want true", got)
	}
}

// TestConfigList_JSONOutput: config list --output json produces a bare array
// [{"key","value","source"}], uniform with `maz list entities/relations`.
func TestConfigList_JSONOutput(t *testing.T) {
	t.Parallel()
	// Run against an empty data dir (no config.toml) — should succeed with
	// default values only.
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	root := makeConfigCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "config", "list", "--output", "json"})
	_ = root.Execute()
	// Even with no config.toml the output may be empty or have items from defaults.
	// The important assertion is that if stdout has content, it's a valid JSON array.
	outStr := strings.TrimSpace(out.String())
	if outStr == "" {
		// Acceptable: no config means nothing to list (ErrUserConfigNotFound path).
		return
	}
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(outStr), &parsed); err != nil {
		t.Fatalf("config list JSON is not a parseable array: %v\noutput: %s", err, outStr)
	}
	if len(parsed) > 0 {
		if _, ok := parsed[0]["key"]; !ok {
			t.Errorf("config list item missing 'key' field: %s", outStr)
		}
	}
}

// TestConfigSet_ZeroFloatRoundTrip proves that setting a pointer-typed recall
// knob to 0.0 via `config set` round-trips correctly through `config get`:
// the value must appear as 0 (not "key not found") because a non-nil *float32
// pointing to 0.0 is not dropped by omitempty, unlike the old value-type path.
func TestConfigSet_ZeroFloatRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	set := func(key, val string) {
		t.Helper()
		var out, errBuf bytes.Buffer
		root := makeConfigCmd(&out, &errBuf)
		root.SetArgs([]string{"--data-dir", dir, "config", "set", key, val})
		if err := root.Execute(); err != nil {
			t.Fatalf("config set %s %s: %v (stderr: %s)", key, val, err, errBuf.String())
		}
	}
	get := func(key string) (string, error) {
		t.Helper()
		var out, errBuf bytes.Buffer
		root := makeConfigCmd(&out, &errBuf)
		root.SetArgs([]string{"--data-dir", dir, "config", "get", key})
		err := root.Execute()
		return strings.TrimSpace(out.String()), err
	}

	// rrf_min_score = 0.0: previously dropped by omitempty → "key not found".
	set("recall.rrf_min_score", "0.0")
	got, err := get("recall.rrf_min_score")
	if err != nil {
		t.Errorf("config get recall.rrf_min_score after set 0.0: unexpected error: %v", err)
	}
	if !strings.Contains(got, "0") {
		t.Errorf("recall.rrf_min_score after set 0.0 = %q, want output containing 0", got)
	}

	// mmr_lambda = 0.0: same fix path.
	set("recall.mmr_lambda", "0.0")
	got, err = get("recall.mmr_lambda")
	if err != nil {
		t.Errorf("config get recall.mmr_lambda after set 0.0: unexpected error: %v", err)
	}
	if !strings.Contains(got, "0") {
		t.Errorf("recall.mmr_lambda after set 0.0 = %q, want output containing 0", got)
	}
}

// TestConfigSet_CoalesceSecondsZero proves that `config set sync.coalesce_seconds 0`
// round-trips through `config get` as 0, not as the compiled default (30).
// Previously the merge guard `if src.Sync.CoalesceSeconds != 0` silently treated
// the stored 0 as "unset" and the compiled default won.
func TestConfigSet_CoalesceSecondsZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var out, errBuf bytes.Buffer
	root := makeConfigCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "config", "set", "sync.coalesce_seconds", "0"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config set sync.coalesce_seconds 0: %v (stderr: %s)", err, errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	root = makeConfigCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "config", "get", "sync.coalesce_seconds"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config get sync.coalesce_seconds: %v (stderr: %s)", err, errBuf.String())
	}
	got := strings.TrimSpace(out.String())
	// Must contain "0" and must NOT contain "30" (the compiled default).
	if !strings.Contains(got, "0") {
		t.Errorf("sync.coalesce_seconds after set 0 = %q, want 0", got)
	}
	if strings.Contains(got, "30") {
		t.Errorf("sync.coalesce_seconds after set 0 = %q, got the compiled default 30 instead of the stored 0", got)
	}
}

// TestConfigCmds_RegisteredInRoot: config subcommands are reachable from root.
func TestConfigCmds_RegisteredInRoot(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	for _, sub := range []string{"get", "set", "list", "path", "edit"} {
		cmd, _, err := root.Find([]string{"config", sub})
		if err != nil {
			t.Errorf("root.Find(config %s): %v", sub, err)
			continue
		}
		if cmd == nil || cmd.Name() != sub {
			t.Errorf("config %s not found in root (got %v)", sub, cmd)
		}
	}
}

// TestConfigGet_PrintsLoadErrorToStderr pins the Greptile P1 finding
// on PR #145: when LoadEffectiveConfig fails (corrupt config.toml,
// future schema_version), `maz config get` must surface the error
// text on stderr instead of returning silentError silently. The
// operator otherwise sees only exit code 8 and has nothing to act on
// for the same broken file the daemon refuses at boot.
func TestConfigGet_PrintsLoadErrorToStderr(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	// Seed a corrupt config.toml so LoadEffectiveConfig fails the parse step.
	if err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("[providers.ollama\nbroken = "), 0o600); err != nil {
		t.Fatalf("seed corrupt config: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeConfigCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dataDir, "config", "get"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-nil error for corrupt config; got nil")
	}
	if !strings.Contains(errBuf.String(), "config") {
		t.Errorf("stderr does not mention 'config' — operator has no actionable message.\nstderr:\n%s", errBuf.String())
	}
}

// TestConfigGetKey_PrintsLoadErrorToStderr is the per-key sibling of
// the test above: `maz config get defaults.chat` must also surface
// the parse / schema error before returning silentError, because the
// key-lookup branch hits the same LoadEffectiveConfig call.
func TestConfigGetKey_PrintsLoadErrorToStderr(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("[providers.ollama\nbroken = "), 0o600); err != nil {
		t.Fatalf("seed corrupt config: %v", err)
	}

	var outBuf, errBuf bytes.Buffer
	root := makeConfigCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dataDir, "config", "get", "defaults.chat"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-nil error for corrupt config on key lookup; got nil")
	}
	if !strings.Contains(errBuf.String(), "config") {
		t.Errorf("stderr does not mention 'config' on key path — operator has no actionable message.\nstderr:\n%s", errBuf.String())
	}
}

// TestConfigSet_ReservedKeyRejected covers B1.0: writing schema_version via
// `maz config set` must be rejected with exit code 1 and a clear error message.
// schema_version is managed by `maz migrate` and cannot be set directly by users.
func TestConfigSet_ReservedKeyRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	root := makeConfigCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "config", "set", "schema_version", "2"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when setting reserved key schema_version; got nil")
	}
	var se silentError
	ok := errors.As(err, &se)
	if !ok {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("exit code = %d, want 1 (validation error)", se.code)
	}
	if !strings.Contains(errBuf.String(), "schema_version") {
		t.Errorf("stderr does not mention 'schema_version'; got: %s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "maz migrate") {
		t.Errorf("stderr does not mention 'maz migrate'; got: %s", errBuf.String())
	}
}

// TestConfigSet_TrailingZeroFloat covers B1.1: values like "0.0", "1.0", and
// "0.50" must be accepted as floats, not rejected with a JSON-unmarshal error.
func TestConfigSet_TrailingZeroFloat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  float64
	}{
		{"0.0", 0.0},
		{"1.0", 1.0},
		{"0.50", 0.5},
		{"0.5", 0.5},
		{"1.5", 1.5},
	}
	for _, tc := range cases {
		got := inferType(tc.input)
		f, ok := got.(float64)
		if !ok {
			t.Errorf("inferType(%q) = %v (%T), want float64", tc.input, got, got)
			continue
		}
		if f != tc.want {
			t.Errorf("inferType(%q) = %v, want %v", tc.input, f, tc.want)
		}
	}
}

// TestConfigSet_UnknownKeyExitCode covers B1.2: an unknown key must exit with
// code 1 (validation error), not code 8 (unexpected failure).
func TestConfigSet_UnknownKeyExitCode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	root := makeConfigCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "config", "set", "defaults.chatt", "ollama"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unknown key; got nil")
	}
	var se silentError
	ok := errors.As(err, &se)
	if !ok {
		t.Fatalf("expected silentError, got %T: %v", err, err)
	}
	if se.code != 1 {
		t.Errorf("unknown key exit code = %d, want 1 (documented contract for validation errors)", se.code)
	}
}

// TestConfigSet_BadValueExitCode pins the documented contract: supplying a
// value that does not match the target field type (e.g. "abc" for a float
// key) is a VALIDATION error and must exit with code 1, not code 2.
// It also asserts the error message is user-actionable and does not leak Go
// internal type names (RecallTable, float32, "json: cannot unmarshal").
func TestConfigSet_BadValueExitCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key   string
		value string
	}{
		{"recall.rrf_min_score", "abc"},
		{"sync.coalesce_seconds", "abc"},
		{"sync.coalesce_seconds", "0.5"}, // non-integer for an int field
		{"recall.rrf_min_score", "NaN"},  // NaN is not a meaningful config value
		{"recall.rrf_min_score", "Inf"},  // nor is ±Inf
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var outBuf, errBuf bytes.Buffer
			root := makeConfigCmd(&outBuf, &errBuf)
			root.SetArgs([]string{"--data-dir", dir, "config", "set", tc.key, tc.value})
			err := root.Execute()
			if err == nil {
				t.Fatal("expected error for bad value type; got nil")
			}
			var se silentError
			if !errors.As(err, &se) {
				t.Fatalf("expected silentError, got %T: %v", err, err)
			}
			if se.code != 1 {
				t.Errorf("exit code = %d, want 1 (validation error per documented contract)", se.code)
			}
			stderr := errBuf.String()
			for _, forbidden := range []string{"RecallTable", "float32", "json: cannot unmarshal"} {
				if strings.Contains(stderr, forbidden) {
					t.Errorf("stderr leaks Go internals %q — must be user-actionable\nstderr: %s", forbidden, stderr)
				}
			}
			if !strings.Contains(stderr, tc.key) {
				t.Errorf("stderr does not name the offending key %q\nstderr: %s", tc.key, stderr)
			}
		})
	}
}

// TestConfigSet_FusionModeEnum covers B4: recall.fusion_mode must reject
// unknown values with exit 1 and name the legal options; valid values must
// succeed.
func TestConfigSet_FusionModeEnum(t *testing.T) {
	t.Parallel()

	t.Run("invalid value rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		var outBuf, errBuf bytes.Buffer
		root := makeConfigCmd(&outBuf, &errBuf)
		root.SetArgs([]string{"--data-dir", dir, "config", "set", "recall.fusion_mode", "garbage"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected error for invalid fusion_mode value; got nil")
		}
		var se silentError
		if !errors.As(err, &se) {
			t.Fatalf("expected silentError, got %T: %v", err, err)
		}
		if se.code != 1 {
			t.Errorf("exit code = %d, want 1 (validation error)", se.code)
		}
		stderr := errBuf.String()
		if !strings.Contains(stderr, "garbage") {
			t.Errorf("stderr does not echo the invalid value %q\nstderr: %s", "garbage", stderr)
		}
		for _, legal := range []string{"rrf", "weighted"} {
			if !strings.Contains(stderr, legal) {
				t.Errorf("stderr does not name legal value %q\nstderr: %s", legal, stderr)
			}
		}
	})

	for _, valid := range []string{"rrf", "weighted"} {
		t.Run("valid value "+valid, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var outBuf, errBuf bytes.Buffer
			root := makeConfigCmd(&outBuf, &errBuf)
			root.SetArgs([]string{"--data-dir", dir, "config", "set", "recall.fusion_mode", valid})
			if err := root.Execute(); err != nil {
				t.Fatalf("config set recall.fusion_mode %s: unexpected error: %v (stderr: %s)", valid, err, errBuf.String())
			}
		})
	}
}

// TestConfigSet_UnknownKeyErrorText covers B2: the error text for an unknown
// key must not point at `maz schema show config` (which emits nothing useful)
// and must instead direct the agent to `maz config list`.
func TestConfigSet_UnknownKeyErrorText(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	root := makeConfigCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "config", "set", "recall.nonexistent_key", "val"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unknown key; got nil")
	}
	stderr := errBuf.String()
	if strings.Contains(stderr, "schema show config") {
		t.Errorf("error text still references dead-end `maz schema show config`\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "maz config list") {
		t.Errorf("error text does not direct to `maz config list`\nstderr: %s", stderr)
	}
}

// TestConfigSet_FloatRoundTrip exercises that trailing-zero floats written via
// config set are accepted (no JSON-unmarshal error) and non-zero values
// round-trip correctly. The B1.1 regression was: "0.0", "1.0", "0.50" were
// passed as the string literal to json.Unmarshal into a float32 field, which
// failed with "cannot unmarshal string into … float32". Zero-value floats with
// omitempty are not stored and therefore not readable back via config get —
// that is correct behaviour, not a bug.
func TestConfigSet_FloatRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	set := func(key, val string) error {
		t.Helper()
		var out, errBuf bytes.Buffer
		root := makeConfigCmd(&out, &errBuf)
		root.SetArgs([]string{"--data-dir", dir, "config", "set", key, val})
		return root.Execute()
	}
	get := func(key string) string {
		t.Helper()
		var out, errBuf bytes.Buffer
		root := makeConfigCmd(&out, &errBuf)
		root.SetArgs([]string{"--data-dir", dir, "config", "get", key})
		if err := root.Execute(); err != nil {
			t.Fatalf("config get %s: %v (stderr: %s)", key, err, errBuf.String())
		}
		return strings.TrimSpace(out.String())
	}

	// "0.0" must not produce a JSON-unmarshal error (B1.1 regression).
	if err := set("recall.rrf_min_score", "0.0"); err != nil {
		t.Errorf("config set recall.rrf_min_score 0.0: unexpected error: %v", err)
	}

	// "0.50" is non-zero so it survives omitempty and must read back as a number.
	if err := set("recall.rrf_min_score", "0.50"); err != nil {
		t.Fatalf("config set recall.rrf_min_score 0.50: %v", err)
	}
	got := get("recall.rrf_min_score")
	// Output is JSON when stdout is not a TTY (bytes.Buffer in tests).
	// Accept both plain "0.5" and the JSON envelope {"key":…,"value":0.5}.
	if !strings.Contains(got, "0.5") {
		t.Errorf("recall.rrf_min_score after set 0.50 = %q, want output containing 0.5", got)
	}
}

// TestConfigSet_DocumentedSnakeCaseKeys pins that the exact dotted keys the
// `config set` help advertises (and config.toml uses) actually take effect:
// they round-trip through the writer and read back via `config get`. Before the
// fix the setter resolved keys against the JSON (PascalCase Go-field) namespace,
// so every documented snake_case key — defaults.chat, providers.ollama.chat_model
// — was rejected as "unknown key".
func TestConfigSet_DocumentedSnakeCaseKeys(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	// Seed a config with the providers.ollama block so defaults.chat=ollama is
	// coherent and the provider sub-keys have a table to land in.
	seed := "schema_version = 1\n\n[providers]\n  [providers.ollama]\n    base_url = \"http://localhost:11434\"\n\n[defaults]\n  chat = \"anthropic\"\n  embed = \"ollama\"\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	sets := [][2]string{
		{"defaults.chat", "ollama"},
		{"providers.ollama.chat_model", "qwen2.5:7b"},
		{"providers.ollama.embed_model", "snowflake-arctic-embed2"},
		{"recall.mmr_lambda", "0.7"},
		{"sync.auto_push", "true"},
	}
	for _, kv := range sets {
		var outBuf, errBuf bytes.Buffer
		root := makeConfigCmd(&outBuf, &errBuf)
		root.SetArgs([]string{"--data-dir", dataDir, "config", "set", kv[0], kv[1]})
		if err := root.Execute(); err != nil {
			t.Fatalf("config set %s %s: %v\nstderr: %s", kv[0], kv[1], err, errBuf.String())
		}
		if strings.Contains(errBuf.String(), "unknown key") {
			t.Fatalf("config set %s rejected the documented key as unknown:\n%s", kv[0], errBuf.String())
		}
	}

	// Read each back via `config get <key>` — proves the snake_case key resolves
	// on the read side too, and the value actually landed.
	want := map[string]string{
		"defaults.chat":                "ollama",
		"providers.ollama.chat_model":  "qwen2.5:7b",
		"providers.ollama.embed_model": "snowflake-arctic-embed2",
	}
	for key, exp := range want {
		var outBuf, errBuf bytes.Buffer
		root := makeConfigCmd(&outBuf, &errBuf)
		root.SetArgs([]string{"--data-dir", dataDir, "config", "get", key})
		if err := root.Execute(); err != nil {
			t.Fatalf("config get %s: %v\nstderr: %s", key, err, errBuf.String())
		}
		// The test harness has no TTY, so `config get` may render either plain
		// text or the JSON envelope; either way the resolved value must appear.
		if got := strings.TrimSpace(outBuf.String()); !strings.Contains(got, exp) {
			t.Errorf("config get %s = %q, want it to contain %q", key, got, exp)
		}
	}
}

// TestConfigSet_NestedMapRoundTrip pins the riskiest path of the TOML
// reflective round-trip: setting a key inside a map-of-struct field
// (providers.<name>, sync.remotes.<scope>) must land the new value AND preserve
// the sibling map entries (a naive marshal/unmarshal could drop them). It also
// exercises the deeper sync.remotes.<scope>.url path past two map levels.
func TestConfigSet_NestedMapRoundTrip(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	// Two providers + a sync remote: the sibling (anthropic) and the deeper
	// nested map (sync.remotes.personal) must survive a write to ollama.
	seed := "schema_version = 1\n\n" +
		"[providers]\n" +
		"  [providers.ollama]\n    base_url = \"http://localhost:11434\"\n" +
		"  [providers.anthropic]\n    model = \"claude-opus-4-7\"\n\n" +
		"[sync]\n  [sync.remotes]\n    [sync.remotes.personal]\n      url = \"file:///tmp/old.git\"\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	sets := [][2]string{
		{"providers.ollama.base_url", "http://localhost:9999"},
		{"sync.remotes.personal.url", "file:///tmp/new.git"},
	}
	for _, kv := range sets {
		var outBuf, errBuf bytes.Buffer
		root := makeConfigCmd(&outBuf, &errBuf)
		root.SetArgs([]string{"--data-dir", dataDir, "config", "set", kv[0], kv[1]})
		if err := root.Execute(); err != nil {
			t.Fatalf("config set %s %s: %v\nstderr: %s", kv[0], kv[1], err, errBuf.String())
		}
	}

	// The new values landed AND the untouched anthropic sibling survived.
	want := map[string]string{
		"providers.ollama.base_url": "http://localhost:9999",
		"providers.anthropic.model": "claude-opus-4-7",
		"sync.remotes.personal.url": "file:///tmp/new.git",
	}
	for key, exp := range want {
		var outBuf, errBuf bytes.Buffer
		root := makeConfigCmd(&outBuf, &errBuf)
		root.SetArgs([]string{"--data-dir", dataDir, "config", "get", key})
		if err := root.Execute(); err != nil {
			t.Fatalf("config get %s: %v\nstderr: %s", key, err, errBuf.String())
		}
		if got := strings.TrimSpace(outBuf.String()); !strings.Contains(got, exp) {
			t.Errorf("config get %s = %q, want it to contain %q (sibling/nested map lost?)", key, got, exp)
		}
	}
}

// TestConfigSet_RejectsTypos pins that a genuine typo in a documented key is
// still rejected (the fix must not turn the validator into a no-op).
func TestConfigSet_RejectsTypos(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	var outBuf, errBuf bytes.Buffer
	root := makeConfigCmd(&outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dataDir, "config", "set", "defaults.chatt", "ollama"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected 'unknown key' error for defaults.chatt typo; got nil")
	}
	if !strings.Contains(errBuf.String(), "unknown key") {
		t.Errorf("stderr does not flag the typo as unknown key:\n%s", errBuf.String())
	}
}

// TestConfigList_CSVOutput verifies that --output csv emits a parseable CSV
// header row plus data rows (not the text "key = value" format). It exercises
// Fix 6 of the --output rendering parity wave.
func TestConfigList_CSVOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Seed a minimal config so buildConfigItems has at least one item to emit.
	seed := "schema_version = 1\n\n[defaults]\n  chat = \"ollama\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}

	var out, errBuf bytes.Buffer
	root := makeConfigCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "config", "list", "--output", "csv"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config list --output csv: %v\nstderr: %s", err, errBuf.String())
	}
	outStr := out.String()
	// Must not contain the text-format "key = value" layout.
	if strings.Contains(outStr, " = ") {
		t.Errorf("csv output must not contain text 'key = value' format, got:\n%s", outStr)
	}
	// Must be parseable CSV.
	r := csv.NewReader(strings.NewReader(outStr))
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v\nraw:\n%s", err, outStr)
	}
	// Must have a header + at least one data row.
	if len(records) < 2 {
		t.Fatalf("expected header + data rows, got %d rows:\n%s", len(records), outStr)
	}
	// Header must contain "key" and "value" columns.
	header := records[0]
	hasKey, hasValue := false, false
	for _, col := range header {
		if col == "key" {
			hasKey = true
		}
		if col == "value" {
			hasValue = true
		}
	}
	if !hasKey || !hasValue {
		t.Errorf("csv header %v must contain 'key' and 'value' columns", header)
	}
}

// TestConfigList_KVOutput verifies that --output kv emits "key: value" lines
// (not the text-format "key = value" layout). It exercises Fix 6 of the
// --output rendering parity wave.
func TestConfigList_KVOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed := "schema_version = 1\n\n[defaults]\n  chat = \"ollama\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}

	var out, errBuf bytes.Buffer
	root := makeConfigCmd(&out, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "config", "list", "--output", "kv"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config list --output kv: %v\nstderr: %s", err, errBuf.String())
	}
	outStr := out.String()
	// kv format uses "key: value" — must not contain "key = value" text-layout.
	if strings.Contains(outStr, " = ") {
		t.Errorf("kv output must not contain text 'key = value' format, got:\n%s", outStr)
	}
	// Must contain at least one "key: value" line.
	if !strings.Contains(outStr, ": ") {
		t.Errorf("kv output must contain 'key: value' lines, got:\n%s", outStr)
	}
}
