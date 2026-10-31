package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/selfupdate"
	"github.com/mo-cmyk/mazarbul/internal/version"
)

// upgradeRenderCmd builds a command whose output format is fixed, plus a buffer
// the render helpers write to. The output flags are registered directly on the
// command (not a parent) so output.From resolves them without a full Execute,
// which is what merges inherited persistent flags.
func upgradeRenderCmd(t *testing.T, format string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "upgrade"}
	cmd.Flags().String("output", "", "output format")
	cmd.Flags().Bool("json", false, "JSON output")
	if err := cmd.Flags().Set("output", format); err != nil {
		t.Fatalf("set output: %v", err)
	}
	cmd.SetOut(&buf)
	return cmd, &buf
}

// withVersion temporarily overrides version.Version for a subtest.
func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = prev })
}

func TestUpgradeRender(t *testing.T) {
	t.Run("managed install prints delegate command in text", func(t *testing.T) {
		cmd, buf := upgradeRenderCmd(t, "text")
		info := upgradeInstallInfo{Method: "homebrew", UpgradeCommand: "brew upgrade mazarbul"}
		if err := renderManagedInstall(cmd, &info); err != nil {
			t.Fatalf("renderManagedInstall: %v", err)
		}
		if !strings.Contains(buf.String(), "brew upgrade mazarbul") {
			t.Errorf("output %q missing brew hint", buf.String())
		}
		if !strings.Contains(buf.String(), "homebrew") {
			t.Errorf("output %q missing method name", buf.String())
		}
	})

	t.Run("mise install prints delegate command", func(t *testing.T) {
		cmd, buf := upgradeRenderCmd(t, "text")
		info := upgradeInstallInfo{Method: "mise", UpgradeCommand: "mise upgrade mazarbul"}
		if err := renderManagedInstall(cmd, &info); err != nil {
			t.Fatalf("renderManagedInstall (mise): %v", err)
		}
		if !strings.Contains(buf.String(), "mise upgrade mazarbul") {
			t.Errorf("output %q missing mise hint", buf.String())
		}
	})

	t.Run("scoop install prints delegate command", func(t *testing.T) {
		cmd, buf := upgradeRenderCmd(t, "text")
		info := upgradeInstallInfo{Method: "scoop", UpgradeCommand: "scoop update mazarbul"}
		if err := renderManagedInstall(cmd, &info); err != nil {
			t.Fatalf("renderManagedInstall (scoop): %v", err)
		}
		if !strings.Contains(buf.String(), "scoop update mazarbul") {
			t.Errorf("output %q missing scoop hint", buf.String())
		}
	})

	t.Run("check reports up to date", func(t *testing.T) {
		withVersion(t, "v1.0.0")
		cmd, buf := upgradeRenderCmd(t, "text")
		if err := renderCheck(cmd, "v1.0.0"); err != nil {
			t.Fatalf("renderCheck: %v", err)
		}
		if !strings.Contains(buf.String(), "up to date") {
			t.Errorf("output %q, want up to date", buf.String())
		}
	})

	t.Run("check reports a newer release", func(t *testing.T) {
		withVersion(t, "v1.0.0")
		cmd, buf := upgradeRenderCmd(t, "text")
		if err := renderCheck(cmd, "v2.0.0"); err != nil {
			t.Fatalf("renderCheck: %v", err)
		}
		if !strings.Contains(buf.String(), "v2.0.0 is available") {
			t.Errorf("output %q, want availability hint", buf.String())
		}
	})

	t.Run("upgraded reports the transition", func(t *testing.T) {
		cmd, buf := upgradeRenderCmd(t, "text")
		if err := renderUpgraded(cmd, "v1.0.0", "v2.0.0"); err != nil {
			t.Fatalf("renderUpgraded: %v", err)
		}
		if !strings.Contains(buf.String(), "upgraded maz v1.0.0 -> v2.0.0") {
			t.Errorf("output %q", buf.String())
		}
	})
}

func TestUpgradeUpToDate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pinned  string
		current string
		latest  string
		want    bool
	}{
		{"latest equals current", "", "v1.0.0", "v1.0.0", true},
		{"latest is newer", "", "v1.0.0", "v1.1.0", false},
		{"latest resolves behind current is up to date", "", "v1.2.0", "v1.1.0", true},
		{"pinned to current is up to date", "v1.0.0", "v1.0.0", "v1.0.0", true},
		{"pinned to an older tag still installs", "v0.9.0", "v1.0.0", "v0.9.0", false},
		{"pinned to a newer tag still installs", "v2.0.0", "v1.0.0", "v2.0.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := upgradeUpToDate(tc.pinned, tc.current, tc.latest); got != tc.want {
				t.Errorf("upgradeUpToDate(%q, %q, %q) = %v, want %v", tc.pinned, tc.current, tc.latest, got, tc.want)
			}
		})
	}
}

// TestUpgradeJSONContract verifies that the --output json path emits the
// canonical contract fields: currentVersion, latestVersion, installMethod,
// upgradeCommand, selfUpdatable.
// Not parallel: subtests call t.Setenv (which alters os.Getenv globally) and
// withVersion (which mutates the version.Version package variable).
func TestUpgradeJSONContract(t *testing.T) {
	// Spin up a minimal fake GitHub-releases endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer srv.Close()

	t.Run("tarball install emits full contract", func(t *testing.T) {
		t.Setenv("MAZ_API_BASE", srv.URL)
		withVersion(t, "v1.0.0")
		cmd, buf := upgradeRenderCmd(t, "json")
		info := upgradeInstallInfo{
			Method:         "tarball",
			UpgradeCommand: "maz upgrade",
			SelfUpdatable:  true,
		}
		if err := runUpgradeJSON(cmd, upgradeTestOpts(t), &info); err != nil {
			t.Fatalf("runUpgradeJSON: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("decode JSON: %v — raw: %s", err, buf.String())
		}
		if got["currentVersion"] != "v1.0.0" {
			t.Errorf("currentVersion = %v, want v1.0.0", got["currentVersion"])
		}
		if got["latestVersion"] != "v9.9.9" {
			t.Errorf("latestVersion = %v, want v9.9.9", got["latestVersion"])
		}
		if got["installMethod"] != "tarball" {
			t.Errorf("installMethod = %v, want tarball", got["installMethod"])
		}
		if got["upgradeCommand"] != "maz upgrade" {
			t.Errorf("upgradeCommand = %v, want maz upgrade", got["upgradeCommand"])
		}
		if got["selfUpdatable"] != true {
			t.Errorf("selfUpdatable = %v, want true", got["selfUpdatable"])
		}
	})

	t.Run("homebrew install reports not self-updatable and correct command", func(t *testing.T) {
		t.Setenv("MAZ_API_BASE", srv.URL)
		withVersion(t, "v1.0.0")
		cmd, buf := upgradeRenderCmd(t, "json")
		info := upgradeInstallInfo{
			Method:         "homebrew",
			UpgradeCommand: "brew upgrade mazarbul",
			SelfUpdatable:  false,
		}
		if err := runUpgradeJSON(cmd, upgradeTestOpts(t), &info); err != nil {
			t.Fatalf("runUpgradeJSON: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("decode JSON: %v — raw: %s", err, buf.String())
		}
		if got["installMethod"] != "homebrew" {
			t.Errorf("installMethod = %v, want homebrew", got["installMethod"])
		}
		if got["upgradeCommand"] != "brew upgrade mazarbul" {
			t.Errorf("upgradeCommand = %v, want brew upgrade mazarbul", got["upgradeCommand"])
		}
		if got["selfUpdatable"] != false {
			t.Errorf("selfUpdatable = %v, want false", got["selfUpdatable"])
		}
	})

	t.Run("mise install reports correct method and command", func(t *testing.T) {
		t.Setenv("MAZ_API_BASE", srv.URL)
		withVersion(t, "v1.0.0")
		cmd, buf := upgradeRenderCmd(t, "json")
		info := upgradeInstallInfo{
			Method:         "mise",
			UpgradeCommand: "mise upgrade mazarbul",
			SelfUpdatable:  false,
		}
		if err := runUpgradeJSON(cmd, upgradeTestOpts(t), &info); err != nil {
			t.Fatalf("runUpgradeJSON: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		if got["installMethod"] != "mise" {
			t.Errorf("installMethod = %v, want mise", got["installMethod"])
		}
		if got["upgradeCommand"] != "mise upgrade mazarbul" {
			t.Errorf("upgradeCommand = %v, want mise upgrade mazarbul", got["upgradeCommand"])
		}
		if got["selfUpdatable"] != false {
			t.Errorf("selfUpdatable = %v, want false", got["selfUpdatable"])
		}
	})

	t.Run("scoop install reports correct method and command", func(t *testing.T) {
		t.Setenv("MAZ_API_BASE", srv.URL)
		withVersion(t, "v1.0.0")
		cmd, buf := upgradeRenderCmd(t, "json")
		info := upgradeInstallInfo{
			Method:         "scoop",
			UpgradeCommand: "scoop update mazarbul",
			SelfUpdatable:  false,
		}
		if err := runUpgradeJSON(cmd, upgradeTestOpts(t), &info); err != nil {
			t.Fatalf("runUpgradeJSON: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		if got["installMethod"] != "scoop" {
			t.Errorf("installMethod = %v, want scoop", got["installMethod"])
		}
		if got["upgradeCommand"] != "scoop update mazarbul" {
			t.Errorf("upgradeCommand = %v, want scoop update mazarbul", got["upgradeCommand"])
		}
	})

	t.Run("unknown install emits non-empty upgradeCommand fallback", func(t *testing.T) {
		t.Setenv("MAZ_API_BASE", srv.URL)
		withVersion(t, "v1.0.0")
		cmd, buf := upgradeRenderCmd(t, "json")
		// Unknown layout: detectInstallInfo leaves UpgradeCommand == "".
		info := upgradeInstallInfo{Method: "unknown", SelfUpdatable: false}
		if err := runUpgradeJSON(cmd, upgradeTestOpts(t), &info); err != nil {
			t.Fatalf("runUpgradeJSON: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("decode JSON: %v", err)
		}
		if got["installMethod"] != "unknown" {
			t.Errorf("installMethod = %v, want unknown", got["installMethod"])
		}
		// The contract field must never be an empty string — consumers reading
		// upgradeCommand without checking selfUpdatable get an actionable value.
		if got["upgradeCommand"] == "" || got["upgradeCommand"] == nil {
			t.Errorf("upgradeCommand = %q, want non-empty fallback", got["upgradeCommand"])
		}
		if got["selfUpdatable"] != false {
			t.Errorf("selfUpdatable = %v, want false", got["selfUpdatable"])
		}
	})

	t.Run("json output contains no banner or ANSI", func(t *testing.T) {
		t.Setenv("MAZ_API_BASE", srv.URL)
		withVersion(t, "v1.0.0")
		cmd, buf := upgradeRenderCmd(t, "json")
		info := upgradeInstallInfo{Method: "tarball", UpgradeCommand: "maz upgrade", SelfUpdatable: true}
		if err := runUpgradeJSON(cmd, upgradeTestOpts(t), &info); err != nil {
			t.Fatalf("runUpgradeJSON: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "\x1b[") {
			t.Errorf("JSON output contains ANSI escape sequences: %q", out)
		}
		// Must be parseable JSON.
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("not valid JSON: %v", err)
		}
	})

	t.Run("offline resolve yields empty latestVersion but still valid JSON", func(t *testing.T) {
		t.Setenv("MAZ_API_BASE", "http://127.0.0.1:1") // connection refused
		withVersion(t, "v1.0.0")
		cmd, buf := upgradeRenderCmd(t, "json")
		info := upgradeInstallInfo{Method: "tarball", UpgradeCommand: "maz upgrade", SelfUpdatable: true}
		if err := runUpgradeJSON(cmd, upgradeTestOpts(t), &info); err != nil {
			t.Fatalf("runUpgradeJSON on offline resolve: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("not valid JSON: %v", err)
		}
		// latestVersion present (may be "" on error — that is fine).
		if _, ok := got["latestVersion"]; !ok {
			t.Error("latestVersion key missing from offline JSON")
		}
	})
}

// TestDetectInstallMethod verifies that isMiseInstall and isScoopInstall
// correctly identify managed-install paths.
func TestDetectInstallMethod(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve home dir: %v", err)
	}
	sep := string(filepath.Separator)

	cases := []struct {
		name           string
		exe            string
		wantMethod     string
		wantCommand    string
		wantSelfUpdate bool
	}{
		{
			name:           "mise in .local/share/mise",
			exe:            filepath.Join(home, ".local", "share", "mise", "installs", "mazarbul", "v1.0.0", "bin", "maz"),
			wantMethod:     "mise",
			wantCommand:    "mise upgrade mazarbul",
			wantSelfUpdate: false,
		},
		{
			name:           "mise in .mise",
			exe:            filepath.Join(home, ".mise", "installs", "mazarbul", "v1.0.0", "bin", "maz"),
			wantMethod:     "mise",
			wantCommand:    "mise upgrade mazarbul",
			wantSelfUpdate: false,
		},
		{
			name:           "scoop install",
			exe:            filepath.Join(home, "scoop", "apps", "mazarbul", "current", "bin", "maz"),
			wantMethod:     "scoop",
			wantCommand:    "scoop update mazarbul",
			wantSelfUpdate: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// isMiseInstall / isScoopInstall do not require the path to exist;
			// test the helpers directly.
			if tc.wantMethod == "mise" {
				if !isMiseInstall(tc.exe) {
					t.Errorf("isMiseInstall(%q) = false, want true", tc.exe)
				}
			}
			if tc.wantMethod == "scoop" {
				if !isScoopInstall(tc.exe) {
					t.Errorf("isScoopInstall(%q) = false, want true", tc.exe)
				}
			}
			// Non-mise path must not be detected as mise.
			nonMise := sep + "usr" + sep + "local" + sep + "bin" + sep + "maz"
			if isMiseInstall(nonMise) {
				t.Errorf("isMiseInstall(%q) = true, want false", nonMise)
			}
			// Non-scoop path must not be detected as scoop.
			nonScoop := sep + "usr" + sep + "local" + sep + "bin" + sep + "maz"
			if isScoopInstall(nonScoop) {
				t.Errorf("isScoopInstall(%q) = true, want false", nonScoop)
			}
		})
	}
}

// TestUpgradeSkipVersion verifies that writeSkipVersion + readSkipVersion
// persist and return the tag correctly. It uses the MAZ_SKIP_FILE seam for
// test isolation so the real system cache directory is never touched.
func TestUpgradeSkipVersion(t *testing.T) {
	t.Run("persists and returns the skipped tag", func(t *testing.T) {
		// Point MAZ_SKIP_FILE at a temp path so write/read are isolated.
		skipFile := filepath.Join(t.TempDir(), "upgrade-skip.json")
		t.Setenv("MAZ_SKIP_FILE", skipFile)

		tag := "v2.0.0"
		if err := writeSkipVersion(tag); err != nil {
			t.Fatalf("writeSkipVersion: %v", err)
		}
		got := readSkipVersion()
		if got != tag {
			t.Errorf("readSkipVersion() = %q, want %q", got, tag)
		}
	})

	t.Run("returns empty string when no skip file exists", func(t *testing.T) {
		// Point MAZ_SKIP_FILE at a path that does not exist.
		t.Setenv("MAZ_SKIP_FILE", filepath.Join(t.TempDir(), "no-such-skip.json"))
		if got := readSkipVersion(); got != "" {
			t.Errorf("readSkipVersion() = %q, want empty string", got)
		}
	})
}

// TestUpgradeHelpNonEmpty verifies that --help output is non-empty and
// includes the Examples section.
func TestUpgradeHelpNonEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	root := &cobra.Command{Use: "maz", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("output", "", "output format")
	root.PersistentFlags().Bool("json", false, "JSON output")
	root.PersistentFlags().Bool("no-tui", false, "disable TUI")
	upgrade := newUpgradeCmd()
	root.AddCommand(upgrade)
	root.SetOut(&buf)
	upgrade.SetOut(&buf)
	root.SetArgs([]string{"upgrade", "--help"})
	_ = root.Execute()
	out := buf.String()
	if len(out) == 0 {
		t.Fatal("upgrade --help output is empty")
	}
	for _, want := range []string{"Examples:", "maz upgrade", "--check", "--output json"} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade --help missing %q in:\n%s", want, out)
		}
	}
}

// upgradeTestOpts returns selfupdate.Options for tests. Callers must set
// MAZ_API_BASE (via t.Setenv) before calling to point at a test server or a
// refused port. The helper reads MAZ_API_BASE so it mirrors what runUpgrade
// would build in production.
func upgradeTestOpts(t *testing.T) *selfupdate.Options {
	t.Helper()
	ch, err := selfupdate.ParseChannel("")
	if err != nil {
		t.Fatalf("ParseChannel: %v", err)
	}
	return &selfupdate.Options{
		Channel: ch,
		APIBase: os.Getenv("MAZ_API_BASE"),
	}
}

// TestUpgradeLaunchdReregister covers the launchd follow-up after a binary
// replacement: a registered agent is re-registered on self-update (so
// launchd pins the new binary's signature), and a managed install prints
// the `maz daemon install` follow-up. An unloaded or absent agent is left
// alone.
func TestUpgradeLaunchdReregister(t *testing.T) {
	registered := daemon.LaunchAgentState{Installed: true, Loaded: true, PID: 1}
	tests := []struct {
		name        string
		st          daemon.LaunchAgentState
		reregErr    error
		wantHandled bool
		wantCalls   int
		wantStderr  string
	}{
		{name: "registered agent is re-registered", st: registered, wantHandled: true, wantCalls: 1, wantStderr: "re-registered"},
		{name: "re-register failure prints manual command", st: registered, reregErr: errors.New("boom"), wantHandled: true, wantCalls: 1, wantStderr: "maz daemon install"},
		{name: "stopped agent falls back", st: daemon.LaunchAgentState{Installed: true}},
		{name: "no agent falls back", st: daemon.LaunchAgentState{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubLaunchAgentState(t, tc.st)
			prev := reregisterLaunchAgentFn
			t.Cleanup(func() { reregisterLaunchAgentFn = prev })
			calls := 0
			reregisterLaunchAgentFn = func(context.Context) error { calls++; return tc.reregErr }

			cmd, _ := upgradeRenderCmd(t, "text")
			var errBuf bytes.Buffer
			cmd.SetErr(&errBuf)
			if got := reregisterLaunchAgentAfterUpgrade(cmd); got != tc.wantHandled {
				t.Fatalf("handled = %v, want %v", got, tc.wantHandled)
			}
			if calls != tc.wantCalls {
				t.Fatalf("re-register calls = %d, want %d", calls, tc.wantCalls)
			}
			if !strings.Contains(errBuf.String(), tc.wantStderr) {
				t.Fatalf("stderr %q missing %q", errBuf.String(), tc.wantStderr)
			}

			cmd, buf := upgradeRenderCmd(t, "text")
			info := upgradeInstallInfo{Method: "homebrew", UpgradeCommand: "brew upgrade mazarbul"}
			if err := renderManagedInstall(cmd, &info); err != nil {
				t.Fatalf("renderManagedInstall: %v", err)
			}
			if hinted := strings.Contains(buf.String(), "maz daemon install"); hinted != tc.st.Loaded {
				t.Fatalf("managed hint present = %v, want %v: %q", hinted, tc.st.Loaded, buf.String())
			}
		})
	}
}
