package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/connect"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// makeUninstallRoot wires a cobra command tree that mirrors production enough
// for PersistentFlags (--data-dir, --output, --json, --no-tui) to be resolved
// by output.From(cmd) and resolveDataDir(cmd).
func makeUninstallRoot(deps *uninstallDeps, outBuf, errBuf *bytes.Buffer) *cobra.Command {
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("data-dir", "", "data directory")
	root.PersistentFlags().String("output", "", "output format: text|json|ndjson")
	root.PersistentFlags().Bool("json", false, "JSON output (legacy)")
	root.PersistentFlags().Bool("no-tui", false, "CLI mode")

	cmd := makeUninstallCmd(deps)
	root.AddCommand(cmd)

	if outBuf != nil {
		root.SetOut(outBuf)
		cmd.SetOut(outBuf)
	}
	if errBuf != nil {
		root.SetErr(errBuf)
		cmd.SetErr(errBuf)
	}
	return root
}

// fakeSecretProvider is an in-memory secret.Provider for testing.
// Keys must be populated before use; Delete records deletions.
type fakeSecretProvider struct {
	data    map[string]string
	deleted []string
}

func newFakeSecrets(keys ...string) *fakeSecretProvider {
	f := &fakeSecretProvider{data: make(map[string]string)}
	for _, k := range keys {
		f.data[k] = "fake-value"
	}
	return f
}

func (f *fakeSecretProvider) Get(_ context.Context, name string) (string, error) {
	v, ok := f.data[name]
	if !ok {
		return "", secret.ErrNotFound
	}
	return v, nil
}

func (f *fakeSecretProvider) Set(_ context.Context, name, value string) error {
	f.data[name] = value
	return nil
}

func (f *fakeSecretProvider) Delete(_ context.Context, name string) error {
	if _, ok := f.data[name]; !ok {
		return secret.ErrNotFound
	}
	delete(f.data, name)
	f.deleted = append(f.deleted, name)
	return nil
}

// emptyConnectSvc returns a connect.Service with no clients (portable: avoids
// any per-client config file path that may not exist on CI).
func emptyConnectSvc(dataDir string) *connect.Service {
	return connect.NewService(dataDir)
}

// TestUninstallHelp: --help output is non-empty and includes examples.
func TestUninstallHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := makeUninstallRoot(nil, &buf, nil)
	root.SetArgs([]string{"uninstall", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("help output is empty")
	}
	for _, want := range []string{"Examples:", "maz uninstall", "--yes", "--purge", "--dry-run", "--keep-data"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestUninstallDryRunJSON: --dry-run --output json produces parseable JSON with
// all three tiers present and dryRun:true.
func TestUninstallDryRunJSON(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    newFakeSecrets("mazarbul:watchword", "mazarbul:identity:ed25519"),
		connectSvc: emptyConnectSvc(tmp),
	}

	var buf, errBuf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, &errBuf)
	root.SetArgs([]string{"uninstall", "--dry-run", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr: %s", err, errBuf.String())
	}

	var inv uninstallInventory
	if err := json.Unmarshal(buf.Bytes(), &inv); err != nil {
		t.Fatalf("JSON parse: %v\noutput: %s", err, buf.String())
	}

	if !inv.DryRun {
		t.Error("expected dryRun:true in JSON output")
	}
	if len(inv.Items) == 0 {
		t.Fatal("inventory is empty")
	}

	// All three tiers must appear.
	tiers := map[uninstallTier]bool{}
	for _, it := range inv.Items {
		tiers[it.Tier] = true
	}
	for _, want := range []uninstallTier{tierSafe, tierDestructive, tierManual} {
		if !tiers[want] {
			t.Errorf("tier %q missing from inventory", want)
		}
	}
}

// TestUninstallJSONInventoryWithoutDryRun: --output json in non-interactive
// mode still outputs a full inventory (non-interactive forces dry-run).
func TestUninstallJSONInventoryWithoutDryRun(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    newFakeSecrets(),
		connectSvc: emptyConnectSvc(tmp),
	}

	var buf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, nil)
	root.SetArgs([]string{"uninstall", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var inv uninstallInventory
	if err := json.Unmarshal(buf.Bytes(), &inv); err != nil {
		t.Fatalf("JSON parse: %v\noutput: %s", err, buf.String())
	}
	// Non-interactive (no TTY, no --yes) → dry-run in JSON mode.
	if !inv.DryRun {
		t.Error("expected dryRun:true when non-interactive and no --yes given")
	}
}

// TestUninstallBinaryAlwaysManual: the maz binary is always in the manual tier.
func TestUninstallBinaryAlwaysManual(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    newFakeSecrets(),
		connectSvc: emptyConnectSvc(tmp),
	}

	var buf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, nil)
	root.SetArgs([]string{"uninstall", "--dry-run", "--output", "json"})
	_ = root.Execute()

	var inv uninstallInventory
	if err := json.Unmarshal(buf.Bytes(), &inv); err != nil {
		t.Fatalf("JSON parse: %v", err)
	}

	for _, it := range inv.Items {
		if it.Name == "binary" {
			if it.Tier != tierManual {
				t.Errorf("binary tier = %q, want %q", it.Tier, tierManual)
			}
			if it.Hint == "" {
				t.Error("binary item has no removal hint")
			}
			return
		}
	}
	t.Error("binary item not found in inventory")
}

// TestUninstallWatchwordWarning: the watchword keychain entry carries the
// irreversibility warning in its description.
func TestUninstallWatchwordWarning(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    newFakeSecrets("mazarbul:watchword"),
		connectSvc: emptyConnectSvc(tmp),
	}

	var buf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, nil)
	root.SetArgs([]string{"uninstall", "--dry-run", "--output", "json"})
	_ = root.Execute()

	var inv uninstallInventory
	if err := json.Unmarshal(buf.Bytes(), &inv); err != nil {
		t.Fatalf("JSON parse: %v", err)
	}

	for _, it := range inv.Items {
		if strings.Contains(it.Name, "watchword") {
			if !strings.Contains(strings.ToUpper(it.Description), "UNRECOVERABLE") {
				t.Errorf("watchword description lacks unrecoverability warning: %q", it.Description)
			}
			if it.Tier != tierDestructive {
				t.Errorf("watchword tier = %q, want %q", it.Tier, tierDestructive)
			}
			return
		}
	}
	t.Error("watchword item not found in inventory")
}

// TestUninstallDataDirInDestructiveTier: data directory appears in the
// destructive tier and never in the safe tier.
func TestUninstallDataDirInDestructiveTier(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    newFakeSecrets(),
		connectSvc: emptyConnectSvc(tmp),
	}

	var buf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, nil)
	root.SetArgs([]string{"uninstall", "--dry-run", "--output", "json"})
	_ = root.Execute()

	var inv uninstallInventory
	if err := json.Unmarshal(buf.Bytes(), &inv); err != nil {
		t.Fatalf("JSON parse: %v", err)
	}

	for _, it := range inv.Items {
		if it.Name == "data-dir" {
			if it.Tier != tierDestructive {
				t.Errorf("data-dir tier = %q, want %q", it.Tier, tierDestructive)
			}
			if !it.Present {
				t.Error("data-dir should be present (tmp dir exists)")
			}
			return
		}
	}
	t.Error("data-dir item not found in inventory")
}

// TestUninstallKeepDataExcludesDestructiveTier: --keep-data removes data-dir
// and keychain entries from the inventory.
func TestUninstallKeepDataExcludesDestructiveTier(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    newFakeSecrets("mazarbul:watchword"),
		connectSvc: emptyConnectSvc(tmp),
	}

	var buf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, nil)
	root.SetArgs([]string{"uninstall", "--dry-run", "--keep-data", "--output", "json"})
	_ = root.Execute()

	var inv uninstallInventory
	if err := json.Unmarshal(buf.Bytes(), &inv); err != nil {
		t.Fatalf("JSON parse: %v", err)
	}

	for _, it := range inv.Items {
		if it.Tier == tierDestructive {
			t.Errorf("--keep-data: unexpected destructive item %q in inventory", it.Name)
		}
	}
}

// TestUninstallYesDoesNotRemoveDataWithoutDELETE: --yes + --purge removes safe
// items on confirmation but does NOT remove data/keychain without typed DELETE.
func TestUninstallYesDoesNotRemoveDataWithoutDELETE(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	fakeSec := newFakeSecrets("mazarbul:watchword")
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    fakeSec,
		connectSvc: emptyConnectSvc(tmp),
		// Safe consent: always confirm.
		promptSafe: func(_ io.Writer, _ string) (bool, error) { return true, nil },
		// DELETE token: always deny.
		promptDeleteToken: func(_ io.Writer) (bool, error) { return false, nil },
	}

	var buf, errBuf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, &errBuf)
	root.SetArgs([]string{"uninstall", "--yes", "--purge"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr: %s", err, errBuf.String())
	}

	// Data directory must NOT have been removed.
	if _, err := os.Stat(tmp); os.IsNotExist(err) {
		t.Error("data dir was removed despite DELETE token being denied")
	}
	// Watchword must NOT have been deleted from keychain.
	for _, del := range fakeSec.deleted {
		if strings.Contains(del, "watchword") {
			t.Error("watchword was deleted from keychain despite DELETE token being denied")
		}
	}
}

// TestUninstallNothingRemovedWithoutConsent: running without --yes in
// non-interactive mode prints inventory and exits, touching nothing.
func TestUninstallNothingRemovedWithoutConsent(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeSec := &fakeSecretProvider{data: map[string]string{"mazarbul:watchword": "secret"}}
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    fakeSec,
		connectSvc: emptyConnectSvc(tmp),
		// No promptSafe / promptDeleteToken overrides → defaults auto-decline in
		// non-interactive mode (CanPromptInteractively returns false under go test).
	}

	var buf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, nil)
	// No --yes, no --purge → non-interactive dry-run posture.
	root.SetArgs([]string{"uninstall", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Sentinel must still exist (nothing was touched).
	if _, err := os.Stat(sentinel); os.IsNotExist(err) {
		t.Error("sentinel file was removed without consent")
	}
	if len(fakeSec.deleted) > 0 {
		t.Errorf("secrets were deleted without consent: %v", fakeSec.deleted)
	}
}

// TestUninstallCompletionBlockRemoval: removeCompletionBlock strips the maz
// completion lines from an RC file, preserving other content.
func TestUninstallCompletionBlockRemoval(t *testing.T) {
	t.Parallel()

	rcContent := "# existing line\n" +
		"export FOO=bar\n" +
		"\n" +
		"# maz shell completion\n" +
		"source <(maz completion zsh)\n" +
		"# trailing line\n"

	tmp := t.TempDir()
	rc := filepath.Join(tmp, ".zshrc")
	if err := os.WriteFile(rc, []byte(rcContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := removeCompletionBlock(rc); err != nil {
		t.Fatalf("removeCompletionBlock: %v", err)
	}

	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}

	result := string(got)
	if strings.Contains(result, "# maz shell completion") {
		t.Error("completion marker still present after removal")
	}
	if strings.Contains(result, "source <(maz completion zsh)") {
		t.Error("source line still present after removal")
	}
	if !strings.Contains(result, "# existing line") {
		t.Error("existing line was removed (should be kept)")
	}
	if !strings.Contains(result, "# trailing line") {
		t.Error("trailing line was removed (should be kept)")
	}
}

// TestUninstallCompletionBlockRemovalMissingFile: removeCompletionBlock is a
// no-op when the RC file does not exist.
func TestUninstallCompletionBlockRemovalMissingFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	err := removeCompletionBlock(filepath.Join(tmp, "nonexistent"))
	if err != nil {
		t.Errorf("expected nil for missing file, got %v", err)
	}
}

// TestUninstallContainsCompletionMarker: containsCompletionMarker finds the
// sentinel line when present and returns false when absent.
func TestUninstallContainsCompletionMarker(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"present", "export X=1\n# maz shell completion\nsource <(maz completion zsh)\n", true},
		{"absent", "export X=1\n# other stuff\n", false},
		{"empty file", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			path := filepath.Join(tmp, "rc")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := containsCompletionMarker(path)
			if got != tc.want {
				t.Errorf("containsCompletionMarker = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUninstallKeychainEntriesEnumerated: all canonical keychain keys appear
// in the destructive tier when the fake provider has them present.
func TestUninstallKeychainEntriesEnumerated(t *testing.T) {
	t.Parallel()
	keys := keychainEntries()
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.name
	}
	fakeSec := newFakeSecrets(names...)

	tmp := t.TempDir()
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    fakeSec,
		connectSvc: emptyConnectSvc(tmp),
	}

	var buf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, nil)
	root.SetArgs([]string{"uninstall", "--dry-run", "--output", "json"})
	_ = root.Execute()

	var inv uninstallInventory
	if err := json.Unmarshal(buf.Bytes(), &inv); err != nil {
		t.Fatalf("JSON parse: %v", err)
	}

	// Watchword and identity keys must appear in the destructive tier as present.
	found := map[string]bool{}
	for _, it := range inv.Items {
		if it.Tier == tierDestructive && it.Present {
			found[it.Name] = true
		}
	}
	if !found["keychain-mazarbul-watchword"] {
		t.Error("watchword not present in destructive tier")
	}
	if !found["keychain-mazarbul-identity-ed25519"] {
		t.Error("ed25519 key not present in destructive tier")
	}
}

// TestUninstallDryRunNeverTouchesFilesystem: dry-run + --purge must not
// remove anything from the data directory or keychain.
func TestUninstallDryRunNeverTouchesFilesystem(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	safeFile := filepath.Join(tmp, "keep-me")
	if err := os.WriteFile(safeFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeSec := newFakeSecrets("mazarbul:watchword")
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    fakeSec,
		connectSvc: emptyConnectSvc(tmp),
	}

	var buf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, nil)
	root.SetArgs([]string{"uninstall", "--dry-run", "--purge", "--output", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Files must still exist.
	if _, err := os.Stat(safeFile); errors.Is(err, os.ErrNotExist) {
		t.Error("file was removed during dry-run")
	}
	if _, err := os.Stat(tmp); errors.Is(err, os.ErrNotExist) {
		t.Error("data dir was removed during dry-run")
	}
	// Keychain untouched.
	if len(fakeSec.deleted) > 0 {
		t.Errorf("secrets deleted during dry-run: %v", fakeSec.deleted)
	}
}

// TestUninstallTextDryRunGolden: --dry-run --output text output matches the
// golden file. Run with -update to regenerate. Uses t.Setenv → no t.Parallel.
func TestUninstallTextDryRunGolden(t *testing.T) {
	// The dry-run inventory in enumerateItems probes the REAL machine —
	// launchctl-loaded agents (daemon.LaunchAgentLoaded) — which is not
	// injectable through uninstallDeps. Home-relative paths are pinned to a
	// fake HOME below, but the golden still captures the local launchd state
	// (● = daemon actually loaded, macOS launchd wording) and cannot match a
	// fresh CI runner on any OS. Skip under CI; the
	// golden stays a useful local-dev regression aid. Making it CI-portable
	// needs enumerateItems to accept injectable probes — tracked as follow-up.
	if os.Getenv("CI") != "" {
		t.Skip("uninstall golden captures local machine state (loaded launchd agents, daemon.log, home paths); not portable to CI runners")
	}
	t.Setenv("NO_COLOR", "1")

	// Point every home-relative probe (daemon.log, shell RC files) at a fake
	// HOME so the golden never captures the developer's real home path. The
	// daemon.log is created so the "present" (●) branch is exercised
	// deterministically.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("MAZ_LOG_FILE", "")
	logDir := filepath.Join(fakeHome, ".cache", "mazarbul")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatalf("mkdir fake log dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "daemon.log"), nil, 0o600); err != nil {
		t.Fatalf("write fake daemon.log: %v", err)
	}

	tmp := t.TempDir()
	deps := &uninstallDeps{
		dataDir:    tmp,
		secrets:    newFakeSecrets("mazarbul:watchword"),
		connectSvc: emptyConnectSvc(tmp),
	}

	var buf bytes.Buffer
	root := makeUninstallRoot(deps, &buf, nil)
	// Force text format (buffer is not a TTY so default would be JSON).
	root.SetArgs([]string{"uninstall", "--dry-run", "--output", "text"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := buf.Bytes()
	// Normalise paths that change between runs.
	exe, _ := os.Executable()
	if exe != "" {
		got = bytes.ReplaceAll(got, []byte(exe), []byte("/TEST_BINARY"))
	}
	got = bytes.ReplaceAll(got, []byte(tmp), []byte("/tmp/TEST_DATA_DIR"))
	got = bytes.ReplaceAll(got, []byte(fakeHome), []byte("/HOME"))

	compareGolden(t, filepath.Join("testdata", "uninstall_confirm_nocolor.txt"), got)
}
