package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	_ "github.com/charmbracelet/x/exp/golden" // registers -update flag
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// init replaces the real identity-provisioning seam with a no-op for all
// tests that do not explicitly override it. This prevents tests that call
// runInitCLI / runInitJSON without a MAZ_SECRETS_DIR from touching the OS
// keychain, and keeps snapshot output stable (no random watchword phrase).
// Tests that need real provisioning restore the seam themselves after setting
// MAZ_SECRETS_DIR to an isolated temp dir.
func init() {
	provisionIdentityForInitFn = func(_ context.Context) (string, error) { return "", nil }
}

// initTestCmd wraps ctx + out in a *cobra.Command for driving the runInit*
// paths, which now take a command (so they can reach the materialising-append
// path used by the preset on-ramp) instead of a bare (ctx, writer) pair.
func initTestCmd(ctx context.Context, out io.Writer) *cobra.Command {
	c := &cobra.Command{}
	c.SetContext(ctx)
	c.SetOut(out)
	c.SetErr(io.Discard)
	return c
}

// goldenUpdateRequested returns true when the -update flag is set.
func goldenUpdateRequested() bool {
	f := flag.Lookup("update")
	if f == nil {
		return false
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return false
	}
	b, ok := g.Get().(bool)
	return ok && b
}

// compareGolden either writes or compares bytes against the golden file at path.
func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if goldenUpdateRequested() {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to create)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch %s:\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

// lockRender was a v1 lipgloss helper that pinned the global renderer to a
// specific color profile for snapshot tests. In lipgloss v2 there is no
// global renderer: color stripping is handled at the output layer, and our
// adapt() helper returns lipgloss.NoColor{} when NO_COLOR is set. This
// function is now a no-op; tests rely on t.Setenv("NO_COLOR", "1") +
// ansi.Strip to produce deterministic plain-text output.
func lockRender() func() { return func() {} }

// newBoundFlagSet builds a *pflag.FlagSet whose string flags are bound to
// the given initFlags, mirroring what cobra's StringVar wiring does at
// command registration time.
func newBoundFlagSet(flags *initFlags) *pflag.FlagSet {
	fs := pflag.NewFlagSet("init", pflag.ContinueOnError)
	fs.StringVar(&flags.provider, "provider", ChatProviderNone, "")
	fs.StringVar(&flags.chatModel, "chat-model", "", "")
	fs.StringVar(&flags.embedProvider, "embed", EmbedProviderOllama, "")
	fs.StringVar(&flags.embedModel, "embed-model", ollama.DefaultEmbedModel, "")
	fs.StringVar(&flags.apiKey, "api-key", "", "")
	fs.Bool("yes", false, "")
	return fs
}

// TestRunInitCLI verifies the plain CLI output of `maz init` and that the
// resolved data dir gets a config.toml written.
func TestRunInitCLI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	flags := &initFlags{
		provider:      "anthropic",
		embedProvider: EmbedProviderOllama,
		embedModel:    ollama.DefaultEmbedModel,
	}
	var buf bytes.Buffer
	if err := runInitCLI(initTestCmd(context.Background(), &buf), dir, flags, false, daemonModeOff, ""); err != nil {
		t.Fatalf("runInitCLI: %v", err)
	}
	got := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("Mazarbul initialized at "+dir)) {
		t.Errorf("missing initialization line in output: %q", got)
	}
	if _, err := config.LoadUserConfig(dir); err != nil {
		t.Errorf("config.toml not persisted: %v", err)
	}
}

func TestValidateInitFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		flags   initFlags
		wantErr bool
	}{
		{"none default", initFlags{provider: "none", embedProvider: "ollama"}, false},
		{"none embed none", initFlags{provider: "none", embedProvider: "none"}, false},
		{"anthropic", initFlags{provider: "anthropic", embedProvider: "ollama"}, false},
		{"anthropic with model", initFlags{provider: "anthropic", chatModel: "claude-x", embedProvider: "ollama"}, false},
		{"anthropic embed none", initFlags{provider: "anthropic", embedProvider: "none"}, false},
		{"ollama with model", initFlags{provider: "ollama", chatModel: "llama3:latest", embedProvider: "ollama"}, false},
		{"ollama missing model defaults in", initFlags{provider: "ollama", embedProvider: "ollama"}, false},
		{"unknown provider", initFlags{provider: "openai", embedProvider: "ollama"}, true},
		{"unknown embed provider", initFlags{provider: "anthropic", embedProvider: "voyage"}, true},
		{"empty provider", initFlags{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateInitFlags(&tc.flags)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateInitFlags(%+v) err=%v wantErr=%v", tc.flags, err, tc.wantErr)
			}
		})
	}
}

// TestRunInitJSON verifies the JSON output of `maz init`.
func TestRunInitJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	flags := &initFlags{
		provider:      "anthropic",
		embedProvider: EmbedProviderOllama,
		embedModel:    ollama.DefaultEmbedModel,
	}
	var buf bytes.Buffer
	if err := runInitJSON(initTestCmd(context.Background(), &buf), dir, flags, daemonModeOff, "", connectSpecNone); err != nil {
		t.Fatalf("runInitJSON: %v", err)
	}
	out := buf.String()
	for _, want := range []string{`"ok":true`, `"chat":"anthropic"`, `"embed":"ollama"`} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("missing %q in JSON output: %s", want, out)
		}
	}
	if _, err := config.LoadUserConfig(dir); err != nil {
		t.Errorf("config.toml not persisted: %v", err)
	}
}

// TestRunInitJSON_YesFlag verifies that --yes + --json still produces
// parseable JSON with ok:true and does not add extra fields.
func TestRunInitJSON_YesFlag(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	// --yes defaults: anthropic + ollama embeddings.
	flags := &initFlags{
		provider:      ChatProviderAnthropic,
		embedProvider: EmbedProviderOllama,
		embedModel:    ollama.DefaultEmbedModel,
	}
	var buf bytes.Buffer
	if err := runInitJSON(initTestCmd(context.Background(), &buf), dir, flags, daemonModeOff, "", connectSpecNone); err != nil {
		t.Fatalf("runInitJSON with yes defaults: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &out); err != nil {
		t.Fatalf("JSON parse: %v (%s)", err, buf.String())
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Errorf("ok field = %v, want true", out["ok"])
	}
	if chat, _ := out["chat"].(string); chat != "anthropic" {
		t.Errorf("chat = %q, want anthropic", chat)
	}
}

// TestRunInitJSON_EmbedNone verifies that --embed=none writes a config
// without a [providers.ollama] section and reports embed=none in the
// summary JSON. This is the headline issue-#105 contract on the
// non-interactive path.
func TestRunInitJSON_EmbedNone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	flags := &initFlags{
		provider:      "anthropic",
		embedProvider: EmbedProviderNone,
	}
	var buf bytes.Buffer
	if err := runInitJSON(initTestCmd(context.Background(), &buf), dir, flags, daemonModeOff, "", connectSpecNone); err != nil {
		t.Fatalf("runInitJSON: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"embed":"none"`)) {
		t.Errorf("missing \"embed\":\"none\" in JSON output: %s", buf.String())
	}
	cfg, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("config.toml not persisted: %v", err)
	}
	if _, ok := cfg.Providers["ollama"]; ok {
		t.Errorf("config.toml unexpectedly contains [providers.ollama] section when --embed=none")
	}
	if got, want := cfg.Defaults.Embed, EmbedProviderNone; got != want {
		t.Errorf("Defaults.Embed = %q, want %q", got, want)
	}
}

// TestRunInitCLI_Yes_DefaultsAnthropicKeyPresent verifies that --yes with an
// available Anthropic key prints the full summary block without a warning and
// writes a config.toml with defaults.chat = "anthropic".
func TestRunInitCLI_Yes_DefaultsAnthropicKeyPresent(t *testing.T) {
	// Use the test seam so this test is not dependent on env or keychain.
	prev := anthropicKeyAvailableFn
	t.Cleanup(func() { anthropicKeyAvailableFn = prev })
	anthropicKeyAvailableFn = func() bool { return true }

	dir := filepath.Join(t.TempDir(), "data")
	flags := &initFlags{
		provider:      ChatProviderAnthropic,
		embedProvider: EmbedProviderOllama,
		embedModel:    ollama.DefaultEmbedModel,
	}
	var buf bytes.Buffer
	if err := runInitCLI(initTestCmd(context.Background(), &buf), dir, flags, true, daemonModeOff, ""); err != nil {
		t.Fatalf("runInitCLI --yes: %v", err)
	}
	out := buf.String()

	// Summary block lines must be present.
	for _, want := range []string{
		"data directory",
		"chat",
		"anthropic",
		"embeddings",
		"ollama",
		"Ready.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
	// No warning when key is present.
	if strings.Contains(out, "warning:") {
		t.Errorf("unexpected warning in output:\n%s", out)
	}

	cfg, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("config.toml not persisted: %v", err)
	}
	if got := cfg.Defaults.Chat; got != "anthropic" {
		t.Errorf("Defaults.Chat = %q, want anthropic", got)
	}
}

// TestRunInitCLI_Yes_DefaultsAnthropicKeyAbsent verifies that --yes without
// an Anthropic API key emits a warning line and still exits 0 (no error).
func TestRunInitCLI_Yes_DefaultsAnthropicKeyAbsent(t *testing.T) {
	// Use the test seam so this test is not dependent on env or keychain.
	prev := anthropicKeyAvailableFn
	t.Cleanup(func() { anthropicKeyAvailableFn = prev })
	anthropicKeyAvailableFn = func() bool { return false }

	dir := filepath.Join(t.TempDir(), "data")
	flags := &initFlags{
		provider:      ChatProviderAnthropic,
		embedProvider: EmbedProviderOllama,
		embedModel:    ollama.DefaultEmbedModel,
	}
	var buf bytes.Buffer
	// runInitCLI must not return an error (exit 0 contract).
	if err := runInitCLI(initTestCmd(context.Background(), &buf), dir, flags, true, daemonModeOff, ""); err != nil {
		t.Fatalf("runInitCLI --yes (no key): %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "warning:") {
		t.Errorf("expected warning line in output when key absent:\n%s", out)
	}
	if !strings.Contains(out, "maz secret set") {
		t.Errorf("warning should mention `maz secret set`:\n%s", out)
	}
}

// TestRunInitCLI_Yes_FlagOverrideChatModel verifies that --yes --provider=ollama
// --chat-model=llama3 persists the user-supplied chat model, not a default.
func TestRunInitCLI_Yes_FlagOverrideChatModel(t *testing.T) {
	flags := &initFlags{}
	fs := newBoundFlagSet(flags)
	// Simulate: maz init --yes --provider=ollama --chat-model=llama3
	if err := fs.Parse([]string{"--provider=ollama", "--chat-model=llama3"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	// After Parse, flags.provider == "ollama" and flags.chatModel == "llama3"
	// (bound via StringVar). detectYesDefaults must not overwrite them.
	detectYesDefaults(fs, flags)

	if flags.provider != "ollama" {
		t.Errorf("provider = %q, want ollama (Changed flag overridden)", flags.provider)
	}
	if flags.chatModel != "llama3" {
		t.Errorf("chatModel = %q, want llama3", flags.chatModel)
	}
	if err := validateInitFlags(flags); err != nil {
		t.Fatalf("validateInitFlags: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "data")
	var buf bytes.Buffer
	if err := runInitCLI(initTestCmd(context.Background(), &buf), dir, flags, true, daemonModeOff, ""); err != nil {
		t.Fatalf("runInitCLI: %v", err)
	}
	cfg, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("config.toml not persisted: %v", err)
	}
	if got := cfg.Providers["ollama"].ChatModel; got != "llama3" {
		t.Errorf("[providers.ollama].chat_model = %q, want llama3", got)
	}
}

// TestRunInitCLI_Yes_OllamaWithoutModelDefaultsIn verifies that
// --yes --provider=ollama without --chat-model now succeeds and persists
// ollama.DefaultChatModel — symmetric with --embed, which already defaults via
// ollama.DefaultEmbedModel. (Previously this combination was rejected, which
// broke `maz init --provider ollama` out of the box.)
func TestRunInitCLI_Yes_OllamaWithoutModelDefaultsIn(t *testing.T) {
	flags := &initFlags{}
	fs := newBoundFlagSet(flags)
	if err := fs.Parse([]string{"--provider=ollama"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	// After Parse: flags.provider == "ollama", flags.chatModel == "" (not Changed).
	detectYesDefaults(fs, flags)

	if err := validateInitFlags(flags); err != nil {
		t.Fatalf("validateInitFlags should accept ollama without --chat-model now: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "data")
	var buf bytes.Buffer
	if err := runInitCLI(initTestCmd(context.Background(), &buf), dir, flags, true, daemonModeOff, ""); err != nil {
		t.Fatalf("runInitCLI: %v", err)
	}
	cfg, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("config.toml not persisted: %v", err)
	}
	if got := cfg.Providers["ollama"].ChatModel; got != ollama.DefaultChatModel {
		t.Errorf("[providers.ollama].chat_model = %q, want default %q", got, ollama.DefaultChatModel)
	}
}

// TestRunInitCLI_Yes_Snapshot is a golden-file test for the nocolor output of
// `maz init --yes`. Run `go test -update ./internal/cli/...` to regenerate.
func TestRunInitCLI_Yes_Snapshot(t *testing.T) {
	// Pin the key-availability seam so the golden output is hermetic
	// regardless of env or keychain state on the test machine.
	prev := anthropicKeyAvailableFn
	t.Cleanup(func() { anthropicKeyAvailableFn = prev })
	anthropicKeyAvailableFn = func() bool { return true }

	restore := lockRender()
	defer restore()
	t.Setenv("NO_COLOR", "1")

	// Use a fixed data dir path so the snapshot is stable across runs.
	dir := "/tmp/maz-test-snapshot"

	flags := &initFlags{
		provider:      ChatProviderAnthropic,
		embedProvider: EmbedProviderOllama,
		embedModel:    ollama.DefaultEmbedModel,
	}

	// Intercept filesystem writes so the test is hermetic.
	tmpDir := t.TempDir()
	flags2 := *flags
	var buf bytes.Buffer

	// We only snapshot the output; write to tmpDir for the config.
	if err := runInitCLI(initTestCmd(context.Background(), &buf), tmpDir, &flags2, true, daemonModeOff, ""); err != nil {
		t.Fatalf("runInitCLI: %v", err)
	}

	// Replace the tmpDir path with the canonical snapshot path so the golden
	// file is stable across machines and runs. Strip any residual ANSI escape
	// codes so the golden file stays plain-text regardless of lipgloss version.
	snapshotOut := ansi.Strip(strings.ReplaceAll(buf.String(), tmpDir, dir))

	compareGolden(t, "testdata/init_yes_nocolor.txt", []byte(snapshotOut))
}

// TestRunInitJSON_ConnectField asserts that runInitJSON always includes a
// "connect" field in its JSON output, regardless of whether --connect was
// given. With --connect=none the field holds a single skipped entry; with a
// real client name the field holds a per-client result (error is acceptable
// since no AI client is installed in the test environment).
func TestRunInitJSON_ConnectField(t *testing.T) {
	t.Run("connect none produces skipped entry", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		flags := &initFlags{
			provider:      ChatProviderAnthropic,
			embedProvider: EmbedProviderOllama,
			embedModel:    "mxbai-embed-large",
		}
		var buf bytes.Buffer
		if err := runInitJSON(initTestCmd(context.Background(), &buf), dir, flags, daemonModeOff, "", connectSpecNone); err != nil {
			t.Fatalf("runInitJSON: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &out); err != nil {
			t.Fatalf("JSON parse: %v\n%s", err, buf.String())
		}
		rows, ok := out["connect"].([]any)
		if !ok || len(rows) == 0 {
			t.Fatalf("connect field missing or empty in JSON output:\n%s", buf.String())
		}
		row, ok := rows[0].(map[string]any)
		if !ok {
			t.Fatalf("connect[0] is not an object: %v", rows[0])
		}
		if skipped, _ := row["skipped"].(string); skipped == "" {
			t.Errorf("connect[0].skipped should be non-empty for --connect=none; got %v", row)
		}
	})

	t.Run("connect with named client produces result entry", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		flags := &initFlags{
			provider:      ChatProviderAnthropic,
			embedProvider: EmbedProviderOllama,
			embedModel:    "mxbai-embed-large",
		}
		var buf bytes.Buffer
		// claude-code is a known client; it may or may not be installed on the
		// test host. Either way the JSON output must include a "connect" array
		// with an entry for it (action or error, never silent omission).
		if err := runInitJSON(initTestCmd(context.Background(), &buf), dir, flags, daemonModeOff, "", "claude-code"); err != nil {
			t.Fatalf("runInitJSON: %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &out); err != nil {
			t.Fatalf("JSON parse: %v\n%s", err, buf.String())
		}
		rows, ok := out["connect"].([]any)
		if !ok || len(rows) == 0 {
			t.Fatalf("connect field missing or empty in JSON output:\n%s", buf.String())
		}
		row, ok := rows[0].(map[string]any)
		if !ok {
			t.Fatalf("connect[0] is not an object: %v", rows[0])
		}
		clientID, _ := row["clientId"].(string)
		if clientID != "claude-code" {
			t.Errorf("connect[0].clientId = %q, want claude-code", clientID)
		}
		// Either action or error must be present — never silent nothing.
		action, _ := row["action"].(string)
		errStr, _ := row["error"].(string)
		if action == "" && errStr == "" {
			t.Errorf("connect[0] has neither action nor error; output was:\n%s", buf.String())
		}
	})
}

// TestRunInitCLI_Yes_Connect asserts that the --yes (non-interactive) path
// calls runInitConnectStep when --connect is not "none". The connect tip line
// must appear for --connect=none; a per-client line must appear for a real name.
func TestRunInitCLI_Yes_Connect(t *testing.T) {
	prev := anthropicKeyAvailableFn
	t.Cleanup(func() { anthropicKeyAvailableFn = prev })
	anthropicKeyAvailableFn = func() bool { return true }

	t.Run("connect none emits tip", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		flags := &initFlags{
			provider:      ChatProviderAnthropic,
			embedProvider: EmbedProviderOllama,
			embedModel:    "mxbai-embed-large",
		}
		var buf bytes.Buffer
		cmd := initTestCmd(context.Background(), &buf)
		if err := runInitCLI(cmd, dir, flags, true, daemonModeOff, ""); err != nil {
			t.Fatalf("runInitCLI: %v", err)
		}
		// runInit calls runInitConnectStep after runInitCLI; simulate it here.
		if err := runInitConnectStep(context.Background(), &buf, dir, connectSpecNone); err != nil {
			t.Fatalf("runInitConnectStep: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "maz connect all") {
			t.Errorf("expected connect tip in output for --connect=none:\n%s", out)
		}
	})

	t.Run("connect with named client produces output line", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		flags := &initFlags{
			provider:      ChatProviderAnthropic,
			embedProvider: EmbedProviderOllama,
			embedModel:    "mxbai-embed-large",
		}
		var buf bytes.Buffer
		cmd := initTestCmd(context.Background(), &buf)
		if err := runInitCLI(cmd, dir, flags, true, daemonModeOff, ""); err != nil {
			t.Fatalf("runInitCLI: %v", err)
		}
		// Simulate the --connect=claude-code step that runInit calls.
		if err := runInitConnectStep(context.Background(), &buf, dir, "claude-code"); err != nil {
			t.Fatalf("runInitConnectStep: %v", err)
		}
		out := buf.String()
		// The connect step must emit a line mentioning the client — either the
		// action report or an error. Never silent.
		if !strings.Contains(out, "claude-code") {
			t.Errorf("expected claude-code in connect output:\n%s", out)
		}
	})
}

// TestInit_Yes_ProvidesWatchword asserts the core identity-provisioning
// contract: `maz init --yes` (both CLI and JSON paths) must generate and
// persist a Watchword so the device has a bound principal after init.
// Each subtest isolates secrets in a temp dir via MAZ_SECRETS_DIR so no
// real OS keychain is touched.
func TestInit_Yes_ProvidesWatchword(t *testing.T) {
	cases := []struct {
		name string
		run  func(cmd *cobra.Command, dir string, flags *initFlags) error
	}{
		{
			name: "CLI path provisions watchword",
			run: func(cmd *cobra.Command, dir string, flags *initFlags) error {
				return runInitCLI(cmd, dir, flags, true, daemonModeOff, "")
			},
		},
		{
			name: "JSON path provisions watchword",
			run: func(cmd *cobra.Command, dir string, flags *initFlags) error {
				return runInitJSON(cmd, dir, flags, daemonModeOff, "", connectSpecNone)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Isolate secrets in a temp dir: PersistentChain() and DefaultChain()
			// both honour MAZ_SECRETS_DIR, so no real OS keychain is touched.
			secretsDir := t.TempDir()
			t.Setenv(secret.EnvSecretsDir, secretsDir)

			// Restore the real provisioning function for this test; the
			// package-level init() set it to a no-op to protect other tests.
			prevFn := provisionIdentityForInitFn
			t.Cleanup(func() { provisionIdentityForInitFn = prevFn })
			provisionIdentityForInitFn = provisionIdentityIfNeeded

			// Suppress the Anthropic-key warning so only watchword-related
			// output (or its absence) drives the assertions.
			prev := anthropicKeyAvailableFn
			t.Cleanup(func() { anthropicKeyAvailableFn = prev })
			anthropicKeyAvailableFn = func() bool { return true }

			dataDir := filepath.Join(t.TempDir(), "data")
			flags := &initFlags{
				provider:      ChatProviderAnthropic,
				embedProvider: EmbedProviderOllama,
				embedModel:    ollama.DefaultEmbedModel,
			}
			var buf bytes.Buffer
			if err := tc.run(initTestCmd(context.Background(), &buf), dataDir, flags); err != nil {
				t.Fatalf("init: %v", err)
			}

			ctx := context.Background()
			// PersistentChain uses MAZ_SECRETS_DIR (set above), so it reads
			// from the same isolated file store that init wrote to.
			chain := secret.PersistentChain()

			ww, err := chain.Get(ctx, watchword.KeychainKey)
			if err != nil {
				t.Fatalf("watchword not found in secret chain after --yes init: %v", err)
			}
			if ww == "" {
				t.Fatal("watchword is empty after --yes init")
			}

			// With the Watchword in place, identity keypairs must be derivable.
			if _, err := identity.New(chain).Initialize(ctx); err != nil {
				t.Fatalf("identity.Initialize failed after --yes init: %v", err)
			}
		})
	}
}

// TestUserConfigFromFlags_TableOmissions documents the contract that the
// [providers.ollama] table only lands in the persisted config when Ollama
// fills at least one slot.
func TestUserConfigFromFlags_TableOmissions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		flags       initFlags
		wantOllama  bool
		wantEmbedKV string
	}{
		{
			name:        "anthropic+ollama keeps ollama table",
			flags:       initFlags{provider: "anthropic", embedProvider: "ollama", embedModel: ollama.DefaultEmbedModel},
			wantOllama:  true,
			wantEmbedKV: "ollama",
		},
		{
			name:        "anthropic+none omits ollama table",
			flags:       initFlags{provider: "anthropic", embedProvider: "none"},
			wantOllama:  false,
			wantEmbedKV: "none",
		},
		{
			name:        "none+ollama keeps ollama table for embeddings only",
			flags:       initFlags{provider: "none", embedProvider: "ollama", embedModel: ollama.DefaultEmbedModel},
			wantOllama:  true,
			wantEmbedKV: "ollama",
		},
		{
			name:        "none+none omits ollama table",
			flags:       initFlags{provider: "none", embedProvider: "none"},
			wantOllama:  false,
			wantEmbedKV: "none",
		},
		{
			name:        "ollama+none keeps ollama table (chat slot uses it)",
			flags:       initFlags{provider: "ollama", chatModel: "llama3:latest", embedProvider: "none"},
			wantOllama:  true,
			wantEmbedKV: "none",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := userConfigFromFlags(&tc.flags)
			_, hasOllama := cfg.Providers["ollama"]
			if hasOllama != tc.wantOllama {
				t.Errorf("has [providers.ollama] = %v, want %v", hasOllama, tc.wantOllama)
			}
			if got := cfg.Defaults.Embed; got != tc.wantEmbedKV {
				t.Errorf("Defaults.Embed = %q, want %q", got, tc.wantEmbedKV)
			}
			if tc.wantOllama && tc.flags.embedProvider == "none" {
				// When ollama serves only the chat slot, its embed_model
				// field must be empty so the daemon does not later try
				// to load a phantom model.
				if got := cfg.Providers["ollama"].EmbedModel; got != "" {
					t.Errorf("[providers.ollama].embed_model = %q, want empty (embed=none)", got)
				}
			}
		})
	}
}

// TestInit_ProviderNone pins the agent-side-reasoning default: `maz init
// --yes` with no --provider writes defaults.chat = "none" and records no chat
// model anywhere (no [providers.anthropic] table, no ollama chat_model), while
// an explicit --provider or a bare --api-key still selects a chat model.
func TestInit_ProviderNone(t *testing.T) {
	cases := []struct {
		name          string
		args          []string
		wantChat      string
		wantAnthropic bool
		wantOllamaCM  string
		wantSummary   string
	}{
		{name: "default is none", args: nil, wantChat: "none", wantSummary: "your agent does the reasoning"},
		{name: "explicit none", args: []string{"--provider=none"}, wantChat: "none", wantSummary: "your agent does the reasoning"},
		{name: "explicit ollama", args: []string{"--provider=ollama"}, wantChat: "ollama", wantAnthropic: true, wantOllamaCM: ollama.DefaultChatModel, wantSummary: "ollama (" + ollama.DefaultChatModel + ")"},
		{name: "explicit anthropic", args: []string{"--provider=anthropic"}, wantChat: "anthropic", wantAnthropic: true, wantSummary: "anthropic (claude-opus-4-7)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := anthropicKeyAvailableFn
			t.Cleanup(func() { anthropicKeyAvailableFn = prev })
			anthropicKeyAvailableFn = func() bool { return true }

			flags := &initFlags{}
			fs := newBoundFlagSet(flags)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse flags: %v", err)
			}
			detectYesDefaults(fs, flags)
			impliedProviderFromAPIKey(fs, flags)
			if err := validateInitFlags(flags); err != nil {
				t.Fatalf("validateInitFlags: %v", err)
			}

			dir := filepath.Join(t.TempDir(), "data")
			var buf bytes.Buffer
			if err := runInitCLI(initTestCmd(context.Background(), &buf), dir, flags, true, daemonModeOff, ""); err != nil {
				t.Fatalf("runInitCLI: %v", err)
			}
			cfg, err := config.LoadUserConfig(dir)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if got := cfg.Defaults.Chat; got != tc.wantChat {
				t.Errorf("Defaults.Chat = %q, want %q", got, tc.wantChat)
			}
			if _, ok := cfg.Providers["anthropic"]; ok != tc.wantAnthropic {
				t.Errorf("has [providers.anthropic] = %v, want %v", ok, tc.wantAnthropic)
			}
			if got := cfg.Providers["ollama"].ChatModel; got != tc.wantOllamaCM {
				t.Errorf("[providers.ollama].chat_model = %q, want %q", got, tc.wantOllamaCM)
			}
			if got := cfg.Providers["ollama"].EmbedModel; got != ollama.DefaultEmbedModel {
				t.Errorf("[providers.ollama].embed_model = %q, want %q", got, ollama.DefaultEmbedModel)
			}
			if !strings.Contains(buf.String(), tc.wantSummary) {
				t.Errorf("summary missing %q:\n%s", tc.wantSummary, buf.String())
			}
		})
	}
}

// TestImpliedProviderFromAPIKey: a bare --api-key selects Anthropic now that
// the default is none; an explicit --provider always wins.
func TestImpliedProviderFromAPIKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no key keeps none", args: nil, want: "none"},
		{name: "bare key implies anthropic", args: []string{"--api-key=sk-test"}, want: "anthropic"},
		{name: "explicit provider wins", args: []string{"--api-key=sk-test", "--provider=ollama"}, want: "ollama"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			flags := &initFlags{}
			fs := newBoundFlagSet(flags)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse flags: %v", err)
			}
			impliedProviderFromAPIKey(fs, flags)
			if flags.provider != tc.want {
				t.Errorf("provider = %q, want %q", flags.provider, tc.want)
			}
		})
	}
}
