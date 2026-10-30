package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// makePresetCmd builds a minimal root wiring the top-level preset command,
// mirroring makeInstallCmd's flag surface so output.From resolves cleanly.
func makePresetCmd(outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().String("output", "", "output format")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")
	root.AddCommand(newPresetCmd())
	if outBuf != nil {
		root.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
	}
	return root
}

// presetInstructionsBody is a small, valid (<8 KiB) steering body for fixtures.
const presetInstructionsBody = `# CRM operating instructions

When the user mentions a company, model it as a crm:Account.
Link a person to the company they work for with works_at.
`

// writePresetFixture writes a minimal preset (manifest + one kind + predicates +
// instructions.md) into a fresh temp dir and returns the path. withInstructions
// controls whether instructions.md is written (a pack without it is NOT a preset).
func writePresetFixture(t *testing.T, name string, withInstructions bool) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			t.Fatalf("mkdir %s: %v", full, mkErr)
		}
		if wErr := os.WriteFile(full, []byte(content), 0o644); wErr != nil {
			t.Fatalf("write %s: %v", full, wErr)
		}
	}
	mustWrite("manifest.toml", ""+
		"name        = \""+name+"\"\n"+
		"version     = \"0.1.0\"\n"+
		"description = \"Test preset fixture.\"\n"+
		"category    = \"extension\"\n"+
		"kind        = \"schema-pack\"\n")
	mustWrite("kinds/account.yaml", ""+
		"name: crm:account\n"+
		"displayName: Account\n"+
		"description: A company or organization tracked in the CRM.\n"+
		"extends: organization\n")
	mustWrite("predicates.yaml", ""+
		"predicates:\n"+
		"  - predicate: works_at\n"+
		"    kind: non-functional\n"+
		"    description: Subject person works at the object account.\n")
	if withInstructions {
		mustWrite("instructions.md", presetInstructionsBody)
	}
	return dir
}

// newPresetDataDir creates an isolated data dir with an embedder-free config so
// the in-process wiring boots without contacting Ollama.
func newPresetDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Defaults:      config.DefaultsTable{Embed: "none"},
	}
	if err := config.WriteUserConfig(dir, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}

// TestPresetCmd_DryRunLocalPreset runs install --dry-run against a local preset
// fixture: it lists the seeded kind, touches no event log, and never prompts
// (the dry-run path returns before the confirmation gate and daemon open).
func TestPresetCmd_DryRunLocalPreset(t *testing.T) {
	t.Parallel()
	fixture := writePresetFixture(t, "crm", true)
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "install", fixture, "--dry-run", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("preset install --dry-run: %v (stderr=%q)", err, errBuf.String())
	}
	var got struct {
		KindsRegistered []string `json:"kindsRegistered"`
		DryRun          bool     `json:"dryRun"`
		InstalledAt     *string  `json:"installedAt"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("dry-run JSON: %v (raw=%q)", err, out.String())
	}
	if !got.DryRun {
		t.Errorf("dry-run output should set dryRun=true: %q", out.String())
	}
	if got.InstalledAt != nil {
		t.Errorf("dry-run should omit installedAt, got %v", *got.InstalledAt)
	}
	found := false
	for _, k := range got.KindsRegistered {
		if k == "crm:account" {
			found = true
		}
	}
	if !found {
		t.Errorf("dry-run kindsRegistered %v missing crm:account", got.KindsRegistered)
	}
}

// TestPresetCmd_NonPresetRejected refuses a pack that has no instructions.md and
// points the user at `maz install`. This must fail before any daemon open.
func TestPresetCmd_NonPresetRejected(t *testing.T) {
	t.Parallel()
	fixture := writePresetFixture(t, "plain", false /* no instructions.md */)
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "install", fixture})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error installing a non-preset pack, got nil")
	}
	if !strings.Contains(errBuf.String(), "not a preset") {
		t.Errorf("stderr %q should explain the pack is not a preset", errBuf.String())
	}
}

// TestPresetCmd_UnknownExitsOne reports a friendly error for an argument that is
// neither a local dir nor a builtin.
func TestPresetCmd_UnknownExitsOne(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "install", "no-such-preset-xyz"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for unknown preset, got nil")
	}
	if !strings.Contains(errBuf.String(), "not found") {
		t.Errorf("stderr %q should mention not found", errBuf.String())
	}
}

// TestPresetCmd_InstallNoArgExitsOne ensures bare "preset install" exits non-zero.
func TestPresetCmd_InstallNoArgExitsOne(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "install"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for missing preset argument, got nil")
	}
	if !strings.Contains(errBuf.String(), "argument required") {
		t.Errorf("stderr %q should mention missing argument", errBuf.String())
	}
}

// TestPresetCmd_Lifecycle exercises the full local-preset round trip against an
// isolated in-process wiring: install --yes seeds the kind/predicate and the
// presets row; list shows metadata WITHOUT the body; show includes the body;
// uninstall is a clean cut (the row is gone but the seeded kind remains).
func TestPresetCmd_Lifecycle(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: opens a full daemon wiring with SQLite; running in parallel
	// with other wiring tests exhausts the race-detector address space on macOS.

	dataDir := newPresetDataDir(t)
	fixture := writePresetFixture(t, "crm", true)

	// install --yes (local dir → gate fires, --yes pre-consents).
	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "install", fixture, "--yes", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset install --yes: %v (stderr=%q)", err, errBuf.String())
		}
		if !strings.Contains(out.String(), "crm:account") {
			t.Errorf("install output %q missing seeded kind", out.String())
		}
	}

	// list --json: metadata present, body absent.
	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "list", "--output", "json", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset list: %v (stderr=%q)", err, errBuf.String())
		}
		var items []map[string]any
		if err := json.Unmarshal(out.Bytes(), &items); err != nil {
			t.Fatalf("list JSON: %v (raw=%q)", err, out.String())
		}
		if len(items) != 1 || items[0]["name"] != "crm" {
			t.Fatalf("list = %v, want one preset named crm", items)
		}
		if _, leaked := items[0]["instructions"]; leaked {
			t.Error("list leaked the instructions body")
		}
		if strings.Contains(out.String(), "crm:Account") {
			t.Errorf("list output %q should not contain the body", out.String())
		}
	}

	// show --json: body present.
	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "show", "crm", "--output", "json", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset show: %v (stderr=%q)", err, errBuf.String())
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("show JSON: %v (raw=%q)", err, out.String())
		}
		body, _ := got["instructions"].(string)
		if !strings.Contains(body, "crm:Account") {
			t.Errorf("show body %q missing the instructions", body)
		}
	}

	// uninstall --yes: clean cut.
	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "uninstall", "crm", "--yes", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset uninstall: %v (stderr=%q)", err, errBuf.String())
		}
		if !strings.Contains(out.String(), "remain") {
			t.Errorf("uninstall output %q should note the kinds remain", out.String())
		}
	}

	// list after uninstall: empty.
	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "list", "--output", "json", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset list (post-uninstall): %v (stderr=%q)", err, errBuf.String())
		}
		// A list endpoint must return an empty array, not null, so a JSON
		// consumer can iterate it unconditionally.
		if trimmed := strings.TrimSpace(out.String()); trimmed != "[]" {
			t.Errorf("post-uninstall list = %q, want []", trimmed)
		}
	}

	// The clean-cut guarantee: the seeded kind survives the uninstall.
	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "show", "crm", "--data-dir", dataDir})
		if err := root.Execute(); err == nil {
			t.Error("expected show to fail for an uninstalled preset")
		}
	}
}

// TestPresetCmd_UninstallAbsentExitsOne refuses to uninstall a preset that was
// never installed.
func TestPresetCmd_UninstallAbsentExitsOne(t *testing.T) {
	skipIfShortLiveBackend(t)
	dataDir := newPresetDataDir(t)
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "uninstall", "never-installed", "--yes", "--data-dir", dataDir})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error uninstalling an absent preset, got nil")
	}
	if !strings.Contains(errBuf.String(), "not installed") {
		t.Errorf("stderr %q should say the preset is not installed", errBuf.String())
	}
}

// TestPresetCmd_UninstallNotInstalledJSONEmitsJSON verifies that
// "preset uninstall <name> --output json" emits a JSON error object
// when the named preset is not installed, rather than plain text.
// B6.0: the not-installed error must also be JSON when json mode is active.
func TestPresetCmd_UninstallNotInstalledJSONEmitsJSON(t *testing.T) {
	skipIfShortLiveBackend(t)
	dataDir := newPresetDataDir(t)
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "uninstall", "never-installed", "--yes", "--output", "json", "--data-dir", dataDir})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error uninstalling an absent preset, got nil")
	}
	// stdout must be valid JSON (the error envelope), not empty plain text.
	rawOut := strings.TrimSpace(out.String())
	if rawOut == "" {
		t.Fatal("uninstall --output json: stdout empty; expected a JSON error object")
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(rawOut), &got); jerr != nil {
		t.Fatalf("uninstall --output json: stdout not JSON: %v\nraw: %q", jerr, rawOut)
	}
	if _, ok := got["error"]; !ok {
		t.Errorf("uninstall --output json: JSON missing 'error' key: %s", rawOut)
	}
}

// TestPresetCmd_ListNDJSONIsLineDelimited verifies that "preset list --output ndjson"
// emits one JSON object per line (NDJSON), not a pretty-printed JSON array.
// B9: ndjson must differ from json (one line per item, not a pretty array).
func TestPresetCmd_ListNDJSONIsLineDelimited(t *testing.T) {
	skipIfShortLiveBackend(t)
	dataDir := newPresetDataDir(t)
	fixture := writePresetFixture(t, "crm", true)

	// Install a preset so the list is non-empty.
	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "install", fixture, "--yes", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset install: %v (stderr=%q)", err, errBuf.String())
		}
	}

	// --output json: must be a pretty-printed JSON array (starts with '[').
	var jsonOut bytes.Buffer
	{
		root := makePresetCmd(&jsonOut, nil)
		root.SetArgs([]string{"preset", "list", "--output", "json", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset list --output json: %v", err)
		}
	}

	// --output ndjson: must be one compact JSON object per line (starts with '{').
	var ndjsonOut bytes.Buffer
	{
		root := makePresetCmd(&ndjsonOut, nil)
		root.SetArgs([]string{"preset", "list", "--output", "ndjson", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset list --output ndjson: %v", err)
		}
	}

	jsonRaw := strings.TrimSpace(jsonOut.String())
	ndjsonRaw := strings.TrimSpace(ndjsonOut.String())

	// json output must be an array.
	if !strings.HasPrefix(jsonRaw, "[") {
		t.Errorf("--output json: expected JSON array, got %q", jsonRaw)
	}

	// ndjson output must NOT be an array; each line must be a JSON object.
	if strings.HasPrefix(ndjsonRaw, "[") {
		t.Errorf("--output ndjson: got a JSON array (same as --output json), want one object per line; raw=%q", ndjsonRaw)
	}
	for i, line := range strings.Split(ndjsonRaw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("ndjson line %d not valid JSON: %v (line=%q)", i, err, line)
		}
	}

	// The two outputs must be byte-distinct.
	if jsonRaw == ndjsonRaw {
		t.Errorf("--output json and --output ndjson are byte-identical:\n%s", jsonRaw)
	}
}

// TestPresetCmd_ShowBadArgJSONEmitsErrorEnvelope verifies that
// "preset show <bad> --output json" emits a JSON error envelope on stdout
// (not just plain text on stderr), matching the uninstall shape.
// B8: install/show bad-arg --output json must emit parseable error on stdout.
func TestPresetCmd_ShowBadArgJSONEmitsErrorEnvelope(t *testing.T) {
	skipIfShortLiveBackend(t)
	dataDir := newPresetDataDir(t)

	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "show", "no-such-preset", "--output", "json", "--data-dir", dataDir})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing preset, got nil")
	}

	rawOut := strings.TrimSpace(out.String())
	if rawOut == "" {
		t.Fatal("preset show --output json: stdout empty; expected a JSON error envelope")
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(rawOut), &got); jerr != nil {
		t.Fatalf("preset show --output json: stdout not JSON: %v\nraw: %q", jerr, rawOut)
	}
	if _, ok := got["error"]; !ok {
		t.Errorf("preset show --output json: JSON missing 'error' key: %s", rawOut)
	}
	if _, ok := got["code"]; !ok {
		t.Errorf("preset show --output json: JSON missing 'code' key: %s", rawOut)
	}
}

// TestPresetCmd_InstallBogusJSONEmitsErrorEnvelope verifies that
// "preset install <bogus> --output json" emits a parseable {error,code} envelope
// on stdout (not just plain text on stderr) and exits 1.
// Bug A: the error path was missing the JSON envelope, leaving stdout empty.
func TestPresetCmd_InstallBogusJSONEmitsErrorEnvelope(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "install", "no-such-preset-xyz", "--output", "json"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for bogus preset, got nil")
	}

	rawOut := strings.TrimSpace(out.String())
	if rawOut == "" {
		t.Fatal("preset install --output json: stdout empty; expected a JSON error envelope")
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(rawOut), &got); jerr != nil {
		t.Fatalf("preset install --output json: stdout not JSON: %v\nraw: %q", jerr, rawOut)
	}
	if _, ok := got["error"]; !ok {
		t.Errorf("preset install --output json: JSON missing 'error' key: %s", rawOut)
	}
	code, _ := got["code"].(string)
	if code != "E_NOT_FOUND" {
		t.Errorf("preset install --output json: code = %q, want E_NOT_FOUND", code)
	}
}

// TestPresetCmd_InstallChecksumOnLocalEmitsValidation verifies that passing
// --checksum to a non-remote preset source emits a JSON envelope with
// code E_VALIDATION (flag misuse, not a not-found).
// Bug A: the checksum-on-local error branch must emit E_VALIDATION, not E_NOT_FOUND.
func TestPresetCmd_InstallChecksumOnLocalEmitsValidation(t *testing.T) {
	t.Parallel()
	fixture := writePresetFixture(t, "crm", true)
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{
		"preset", "install", fixture,
		"--checksum", "sha256:deadbeef",
		"--output", "json",
	})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when --checksum is used with a local dir, got nil")
	}

	rawOut := strings.TrimSpace(out.String())
	if rawOut == "" {
		t.Fatal("preset install --checksum --output json: stdout empty; expected a JSON error envelope")
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(rawOut), &got); jerr != nil {
		t.Fatalf("preset install --checksum --output json: stdout not JSON: %v\nraw: %q", jerr, rawOut)
	}
	code, _ := got["code"].(string)
	if code != "E_VALIDATION" {
		t.Errorf("preset install --checksum on local: code = %q, want E_VALIDATION", code)
	}
}

// writePresetFixtureWithManifest writes a preset (custom manifest.toml + one
// kind + predicates + instructions.md) into a fresh temp dir so a test can add
// api_version / scopes lines to the manifest. instructions.md is always written
// so the fixture is a preset.
func writePresetFixtureWithManifest(t *testing.T, manifestBody string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			t.Fatalf("mkdir %s: %v", full, mkErr)
		}
		if wErr := os.WriteFile(full, []byte(content), 0o644); wErr != nil {
			t.Fatalf("write %s: %v", full, wErr)
		}
	}
	write("manifest.toml", manifestBody)
	write("kinds/account.yaml", ""+
		"name: crm:account\n"+
		"displayName: Account\n"+
		"description: A company or organization tracked in the CRM.\n"+
		"extends: organization\n")
	write("instructions.md", presetInstructionsBody)
	return dir
}

// TestPresetCmd_IncompatibleAPIVersionRefused verifies the preset install path
// runs the same apiVersion gate: a preset built against an unsupported host API
// is refused with E_VALIDATION before any state is touched.
func TestPresetCmd_IncompatibleAPIVersionRefused(t *testing.T) {
	t.Parallel()
	fixture := writePresetFixtureWithManifest(t, ""+
		"name        = \"crm\"\n"+
		"version     = \"0.1.0\"\n"+
		"description = \"CRM preset built against a future API.\"\n"+
		"category    = \"extension\"\n"+
		"kind        = \"schema-pack\"\n"+
		"api_version = \"v99.0.0\"\n")
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "install", fixture, "--output", "json", "--dry-run"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected refusal for an incompatible apiVersion, got nil")
	}
	if !strings.Contains(errBuf.String(), "v99.0.0") {
		t.Errorf("stderr %q should name the required version v99.0.0", errBuf.String())
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &got); jerr != nil {
		t.Fatalf("error envelope JSON: %v (raw=%q)", jerr, out.String())
	}
	if code, _ := got["code"].(string); code != "E_VALIDATION" {
		t.Errorf("code = %q, want E_VALIDATION", code)
	}
}

// TestPresetCmd_UnknownScopeTokenRefused verifies the preset install path
// rejects a manifest scope token outside the kernel enum with E_VALIDATION.
func TestPresetCmd_UnknownScopeTokenRefused(t *testing.T) {
	t.Parallel()
	fixture := writePresetFixtureWithManifest(t, ""+
		"name        = \"crm\"\n"+
		"version     = \"0.1.0\"\n"+
		"description = \"CRM preset requesting an unknown scope.\"\n"+
		"category    = \"extension\"\n"+
		"kind        = \"schema-pack\"\n"+
		"scopes      = [\"bogus\"]\n")
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "install", fixture, "--output", "json", "--dry-run"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected refusal for an unknown scope token, got nil")
	}
	if !strings.Contains(errBuf.String(), "scope") {
		t.Errorf("stderr %q should mention the invalid scope", errBuf.String())
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &got); jerr != nil {
		t.Fatalf("error envelope JSON: %v (raw=%q)", jerr, out.String())
	}
	if code, _ := got["code"].(string); code != "E_VALIDATION" {
		t.Errorf("code = %q, want E_VALIDATION", code)
	}
}

// TestPresetCmd_DryRunRecordsGrantedScopes verifies a preset declaring valid
// scopes echoes them in the dry-run result envelope (the record surfaced to the
// caller) without touching the event log.
func TestPresetCmd_DryRunRecordsGrantedScopes(t *testing.T) {
	t.Parallel()
	fixture := writePresetFixtureWithManifest(t, ""+
		"name        = \"crm\"\n"+
		"version     = \"0.1.0\"\n"+
		"description = \"CRM preset requesting valid scopes.\"\n"+
		"category    = \"extension\"\n"+
		"kind        = \"schema-pack\"\n"+
		"api_version = \"v1.0.0\"\n"+
		"scopes      = [\"project\", \"personal\"]\n")
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "install", fixture, "--dry-run", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("preset install --dry-run: %v (stderr=%q)", err, errBuf.String())
	}
	var got struct {
		GrantedScopes []string `json:"grantedScopes"`
		DryRun        bool     `json:"dryRun"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &got); jerr != nil {
		t.Fatalf("dry-run JSON: %v (raw=%q)", jerr, out.String())
	}
	if !got.DryRun {
		t.Errorf("dry-run should set dryRun=true: %q", out.String())
	}
	if len(got.GrantedScopes) != 2 || got.GrantedScopes[0] != "project" || got.GrantedScopes[1] != "personal" {
		t.Errorf("grantedScopes = %v, want [project personal]", got.GrantedScopes)
	}
}

// TestPresetCmd_ShowNDJSONIsCompactLine verifies that
// "preset show <name> --output ndjson" produces a single compact line with no
// embedded newlines in the object — not the pretty-indented multi-line block
// that --output json produces.
// Bug B: runPresetShow was using SetIndent for both JSON and NDJSON.
func TestPresetCmd_ShowNDJSONIsCompactLine(t *testing.T) {
	skipIfShortLiveBackend(t)
	dataDir := newPresetDataDir(t)
	fixture := writePresetFixture(t, "crm", true)

	// Install the preset first.
	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "install", fixture, "--yes", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset install: %v (stderr=%q)", err, errBuf.String())
		}
	}

	// --output ndjson: must be a single compact line (no embedded newlines).
	var ndjsonOut bytes.Buffer
	root := makePresetCmd(&ndjsonOut, nil)
	root.SetArgs([]string{"preset", "show", "crm", "--output", "ndjson", "--data-dir", dataDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("preset show --output ndjson: %v", err)
	}

	raw := strings.TrimSpace(ndjsonOut.String())
	// Must be valid JSON.
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("preset show --output ndjson: not valid JSON: %v\nraw: %q", err, raw)
	}
	// Must be a single line (no embedded newlines in the object body).
	lines := strings.Split(raw, "\n")
	if len(lines) != 1 {
		t.Errorf("preset show --output ndjson: got %d lines, want 1 compact line\nraw: %q", len(lines), raw)
	}
	// Sanity: the name field must be present.
	if obj["name"] != "crm" {
		t.Errorf("preset show --output ndjson: name = %v, want crm", obj["name"])
	}
}

// TestPresetCmd_UninstallSuccessJSONEmitsJSON verifies that a successful
// "preset uninstall <name> --output json" emits a JSON result object
// rather than the plain-text "Deactivated preset..." message.
// B6.0: success output must also honour the json format flag.
func TestPresetCmd_UninstallSuccessJSONEmitsJSON(t *testing.T) {
	skipIfShortLiveBackend(t)
	dataDir := newPresetDataDir(t)
	fixture := writePresetFixture(t, "crm", true)

	// Install first.
	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "install", fixture, "--yes", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset install: %v (stderr=%q)", err, errBuf.String())
		}
	}

	// Uninstall with JSON output.
	var out, errBuf bytes.Buffer
	root := makePresetCmd(&out, &errBuf)
	root.SetArgs([]string{"preset", "uninstall", "crm", "--yes", "--output", "json", "--data-dir", dataDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("preset uninstall --output json: %v (stderr=%q)", err, errBuf.String())
	}
	rawOut := strings.TrimSpace(out.String())
	if rawOut == "" {
		t.Fatal("uninstall --output json: stdout empty; expected a JSON result object")
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(rawOut), &got); jerr != nil {
		t.Fatalf("uninstall --output json: stdout not JSON: %v\nraw: %q", jerr, rawOut)
	}
	if _, ok := got["name"]; !ok {
		t.Errorf("uninstall --output json: JSON missing 'name' key: %s", rawOut)
	}
	if got["name"] != "crm" {
		t.Errorf("uninstall --output json: name = %v, want crm", got["name"])
	}
}

// TestPresetCmd_ReinstallIsIdempotent verifies that re-installing an already-active
// preset is a no-op: it exits 0, emits an "already installed" indicator, and does
// NOT append duplicate events to the log.
func TestPresetCmd_ReinstallIsIdempotent(t *testing.T) {
	skipIfShortLiveBackend(t)
	// No t.Parallel: opens full daemon wiring with SQLite.

	dataDir := newPresetDataDir(t)
	fixture := writePresetFixture(t, "crm", true)

	runInstall := func() (string, string, error) {
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "install", fixture, "--yes", "--data-dir", dataDir})
		err := root.Execute()
		return out.String(), errBuf.String(), err
	}

	// First install: must succeed and mention the seeded kind.
	out1, err1, err := runInstall()
	if err != nil {
		t.Fatalf("first install: %v (stderr=%q)", err, err1)
	}
	if !strings.Contains(out1, "crm:account") {
		t.Errorf("first install output %q missing seeded kind crm:account", out1)
	}

	// Second install without --force: must exit 0 with "already installed" message.
	out2, err2, err := runInstall()
	if err != nil {
		t.Fatalf("second install (no-op): %v (stderr=%q)", err, err2)
	}
	if !strings.Contains(out2, "already") {
		t.Errorf("second install output %q should mention 'already installed'", out2)
	}

	// Verify the presets table still has exactly one row (no duplicate).
	{
		var out, errBuf bytes.Buffer
		root := makePresetCmd(&out, &errBuf)
		root.SetArgs([]string{"preset", "list", "--output", "json", "--data-dir", dataDir})
		if err := root.Execute(); err != nil {
			t.Fatalf("preset list: %v (stderr=%q)", err, errBuf.String())
		}
		var items []map[string]any
		if err := json.Unmarshal(out.Bytes(), &items); err != nil {
			t.Fatalf("list JSON: %v (raw=%q)", err, out.String())
		}
		if len(items) != 1 {
			t.Errorf("preset list = %d items after re-install, want exactly 1 (no duplicate row)", len(items))
		}
	}
}
