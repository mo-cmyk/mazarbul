package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/selfupdate"
	"github.com/mo-cmyk/mazarbul/internal/version"
)

// upgradeAction is the user's choice in the interactive upgrade prompt.
type upgradeAction int

// upgradeActionNow means the user chose to install the update immediately.
const upgradeActionNow upgradeAction = 0

// upgradeActionSkip means skip this one invocation only.
const upgradeActionSkip upgradeAction = 1

// upgradeActionSkipUntilNext means suppress the notification for this specific
// release tag; the user will be notified again once a newer tag appears.
const upgradeActionSkipUntilNext upgradeAction = 2

// upgradeActionCancelled is a sentinel for a form abort (Ctrl-C / timeout);
// the cancel message is already printed by HandleFormCancellation.
const upgradeActionCancelled upgradeAction = 3

// upgradeSkipFile is the file name under the owned cache dir that records the
// version the user chose to suppress notifications for.
const upgradeSkipFile = "upgrade-skip.json"

func newUpgradeCmd() *cobra.Command {
	var (
		channel   string
		pinned    string
		checkOnly bool
	)
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update maz to the latest release",
		Long: `Update maz to the latest release for the active channel.

The matching release archive is downloaded, verified against its checksums.txt,
and the binary and engine library are replaced atomically. A binary installed
via Homebrew defers to ` + "`brew upgrade mazarbul`" + `. A binary managed via
mise defers to ` + "`mise upgrade mazarbul`" + `. A binary managed via Scoop
defers to ` + "`scoop update mazarbul`" + `. A private-repo (beta) release
needs GITHUB_TOKEN or MAZ_INSTALL_TOKEN set.

Examples:
  # Check whether a newer release is available.
  maz upgrade --check

  # Install the latest release (shows an interactive prompt on a TTY).
  maz upgrade

  # Install a specific release.
  maz upgrade --version v0.4.0

  # Query update status as JSON (for scripting; never installs).
  maz upgrade --output json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgrade(cmd, channel, pinned, checkOnly)
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "", "release channel: stable (default) or nightly (env: MAZ_CHANNEL)")
	cmd.Flags().StringVar(&pinned, "version", "", "install a specific release tag instead of the latest")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "report whether a newer release exists without installing")
	return cmd
}

// Install-method identifiers returned in upgradeInstallInfo.Method. Shared with
// uninstall.go's binary-removal hint so both derive the package manager the
// same way.
const (
	installMethodHomebrew = "homebrew"
	installMethodMise     = "mise"
	installMethodScoop    = "scoop"
	installMethodTarball  = "tarball"
	installMethodUnknown  = "unknown"
)

// upgradeInstallInfo describes how the running binary was installed and how
// to upgrade it.
type upgradeInstallInfo struct {
	// Method is the human-readable install method: "tarball", "homebrew",
	// "mise", "scoop", or "unknown".
	Method string
	// UpgradeCommand is the command the user should run to upgrade. For
	// tarball installs it is "maz upgrade"; for managed installs it is the
	// package-manager command.
	UpgradeCommand string
	// SelfUpdatable is true when maz can replace itself (tarball installs
	// only). For package-manager installs it is always false.
	SelfUpdatable bool
	// loc is the detected selfupdate.Location, used for the self-replace path.
	loc selfupdate.Location
}

// detectInstallInfo resolves the running binary's install location and method,
// augmenting selfupdate.DetectLocation with mise and Scoop detection.
func detectInstallInfo(exe string) upgradeInstallInfo {
	loc := selfupdate.DetectLocation(exe)
	switch loc.Method {
	case selfupdate.MethodHomebrew:
		return upgradeInstallInfo{Method: installMethodHomebrew, UpgradeCommand: "brew upgrade mazarbul", loc: loc}
	case selfupdate.MethodPrefix:
		if isMiseInstall(loc.Exe) {
			return upgradeInstallInfo{Method: installMethodMise, UpgradeCommand: "mise upgrade mazarbul", loc: loc}
		}
		if isScoopInstall(loc.Exe) {
			return upgradeInstallInfo{Method: installMethodScoop, UpgradeCommand: "scoop update mazarbul", loc: loc}
		}
		return upgradeInstallInfo{Method: installMethodTarball, UpgradeCommand: "maz upgrade", SelfUpdatable: true, loc: loc}
	default:
		// MethodUnknown: still check for mise/scoop before giving up.
		if isMiseInstall(loc.Exe) {
			return upgradeInstallInfo{Method: installMethodMise, UpgradeCommand: "mise upgrade mazarbul", loc: loc}
		}
		if isScoopInstall(loc.Exe) {
			return upgradeInstallInfo{Method: installMethodScoop, UpgradeCommand: "scoop update mazarbul", loc: loc}
		}
		return upgradeInstallInfo{Method: installMethodUnknown, loc: loc}
	}
}

// miseInstallPrefixes returns the directory prefixes that identify a
// mise-managed install. Mise installs binaries under ~/.local/share/mise or
// ~/.mise (or %LOCALAPPDATA%\mise on Windows).
func miseInstallPrefixes() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil // can't resolve home dir; treat as no mise prefix
	}
	sep := string(filepath.Separator)
	return []string{
		filepath.Join(home, ".local", "share", "mise") + sep,
		filepath.Join(home, ".mise") + sep,
		filepath.Join(home, "AppData", "Local", "mise") + sep,
	}
}

// isMiseInstall reports whether exe was installed under a mise prefix.
func isMiseInstall(exe string) bool {
	for _, prefix := range miseInstallPrefixes() {
		if strings.HasPrefix(exe, prefix) {
			return true
		}
	}
	return false
}

// isScoopInstall reports whether exe was installed via Scoop (Windows).
func isScoopInstall(exe string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false // can't resolve home dir; treat as not a Scoop install
	}
	scoopApps := filepath.Join(home, "scoop", "apps") + string(filepath.Separator)
	return strings.HasPrefix(exe, scoopApps)
}

// upgradeSkipFilePath returns the path to the upgrade-skip cache file, or ""
// when no cache directory can be resolved (e.g. no home directory).
// MAZ_SKIP_FILE, when set, overrides the path; this is the test-isolation
// seam (same pattern as MAZ_API_BASE / MAZ_BASE_URL for other cache files).
func upgradeSkipFilePath() string {
	if p := os.Getenv("MAZ_SKIP_FILE"); p != "" {
		return p
	}
	dir := config.OwnedDaemonLogDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, upgradeSkipFile)
}

// writeSkipVersion persists tag as the version to suppress passive
// notifications for until a newer release appears.
func writeSkipVersion(tag string) error {
	p := upgradeSkipFilePath()
	if p == "" {
		return nil // no cache dir available; skip silently
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("upgrade: create skip-version dir: %w", err)
	}
	data, _ := json.Marshal(map[string]string{"skip_until": tag}) //nolint:errcheck // marshalling map[string]string never errors
	tmp, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".tmp.*")
	if err != nil {
		return fmt.Errorf("upgrade: create skip-version tmp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("upgrade: write skip-version: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("upgrade: close skip-version: %w", err)
	}
	if err := os.Rename(tmpPath, p); err != nil {
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("upgrade: save skip-version: %w", err)
	}
	return nil
}

// readSkipVersion returns the release tag the user asked to suppress passive
// notifications for, or "" when none is recorded.
func readSkipVersion() string {
	p := upgradeSkipFilePath()
	if p == "" {
		return ""
	}
	data, err := os.ReadFile(p) // #nosec G304 -- p is the owned cache file
	if err != nil {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	return m["skip_until"]
}

func runUpgrade(cmd *cobra.Command, channelFlag, pinned string, checkOnly bool) error {
	channel := channelFlag
	if channel == "" {
		channel = os.Getenv("MAZ_CHANNEL")
	}
	ch, err := selfupdate.ParseChannel(channel)
	if err != nil {
		return err
	}
	opts := &selfupdate.Options{
		Channel: ch,
		Version: pinned,
		Token:   upgradeToken(),
		APIBase: os.Getenv("MAZ_API_BASE"),
		BaseURL: os.Getenv("MAZ_BASE_URL"),
	}

	exe, err := daemon.ResolveExecutable()
	if err != nil {
		return fmt.Errorf("upgrade: locate running binary: %w", err)
	}
	info := detectInstallInfo(exe)
	wr := output.From(cmd)

	// JSON/NDJSON mode is purely informational: resolve the latest release and
	// emit the canonical contract without installing or modifying anything.
	isJSON := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	if isJSON {
		return runUpgradeJSON(cmd, opts, &info)
	}

	// Managed installs (Homebrew, mise, Scoop) cannot self-replace; delegate
	// to the package manager and return.
	if !info.SelfUpdatable {
		if info.Method == installMethodUnknown {
			return fmt.Errorf("upgrade: maz at %s is not in a bin/ + lib/ install layout; reinstall via the install script to enable self-update", info.loc.Exe)
		}
		return renderManagedInstall(cmd, &info)
	}

	return runUpgradeSelfUpdate(cmd, opts, pinned, checkOnly, &info)
}

// runUpgradeSelfUpdate handles the resolve-check-prompt-download path for
// tarball installs where maz can replace itself. It is only called when
// info.SelfUpdatable is true.
func runUpgradeSelfUpdate(cmd *cobra.Command, opts *selfupdate.Options, pinned string, checkOnly bool, info *upgradeInstallInfo) error {
	if err := info.loc.ValidatePrefix(); err != nil {
		return err
	}

	rel, err := selfupdate.ResolveRelease(cmd.Context(), opts)
	if err != nil {
		return tokenHint(err, opts.Token)
	}
	if checkOnly {
		return renderCheck(cmd, rel.Tag)
	}
	if upgradeUpToDate(pinned, version.Version, rel.Tag) {
		return renderUpToDate(cmd, rel.Tag)
	}

	// Interactive path: show the 3-option prompt when running on a TTY;
	// scripted path (piped/CI) proceeds directly without blocking.
	if interactive.CanPromptInteractively() {
		proceed, perr := applyUserUpgradeChoice(cmd, rel)
		if perr != nil {
			return perr
		}
		if !proceed {
			return nil
		}
	}

	// Probe writability only when we are actually going to write, so --check
	// works from a read-only inspection too.
	if err := info.loc.EnsureWritable(); err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	if err := downloadAndSwap(cmd, opts, rel, info.loc); err != nil {
		return err
	}
	if err := renderUpgraded(cmd, version.Version, rel.Tag); err != nil {
		return err
	}
	restartDaemonAfterUpgrade(cmd)
	return nil
}

// applyUserUpgradeChoice presents the 3-option prompt and returns whether to
// proceed with the download. It must only be called when
// interactive.CanPromptInteractively() is true.
func applyUserUpgradeChoice(cmd *cobra.Command, rel selfupdate.Release) (proceed bool, err error) {
	action, perr := promptUpgradeAction(cmd, rel.Tag)
	if perr != nil {
		return false, perr
	}
	switch action {
	case upgradeActionCancelled:
		// Cancel message already printed by HandleFormCancellation.
		return false, nil
	case upgradeActionSkip:
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "skipped; run `maz upgrade` to update later.") //nolint:errcheck // best-effort
		return false, nil
	case upgradeActionSkipUntilNext:
		if werr := writeSkipVersion(rel.Tag); werr != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "upgrade: could not save skip preference: %v\n", werr) //nolint:errcheck // advisory
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "skipping %s; you will be notified when a newer release is available.\n", rel.Tag) //nolint:errcheck // best-effort
		return false, nil
	case upgradeActionNow:
		return true, nil
	default:
		return true, nil
	}
}

// runUpgradeJSON resolves the latest release and emits the canonical upgrade
// JSON contract without modifying the installed binary. Errors resolving the
// latest release are swallowed and represented as an empty latestVersion: the
// JSON output must always be parseable.
func runUpgradeJSON(cmd *cobra.Command, opts *selfupdate.Options, info *upgradeInstallInfo) error {
	// Cobra v1.10.x returns the raw ctx field (nil for a command that was
	// never executed via Execute/ExecuteContext). Guard so the HTTP client
	// gets a valid context rather than a nil one.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	latestVersion := ""
	if rel, err := selfupdate.ResolveRelease(ctx, opts); err == nil {
		latestVersion = rel.Tag
	}
	// An unknown install layout has no UpgradeCommand; emit an actionable
	// fallback rather than "" so a consumer reading upgradeCommand without first
	// checking selfUpdatable never receives a meaningless empty string.
	upgradeCommand := info.UpgradeCommand
	if upgradeCommand == "" {
		upgradeCommand = "reinstall via the install script"
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
		"currentVersion": version.Version,
		"latestVersion":  latestVersion,
		"installMethod":  info.Method,
		"upgradeCommand": upgradeCommand,
		"selfUpdatable":  info.SelfUpdatable,
	})
}

// promptUpgradeAction presents the 3-option upgrade prompt and returns the
// user's choice. It must be called only when interactive.CanPromptInteractively
// is true.
func promptUpgradeAction(cmd *cobra.Command, latest string) (upgradeAction, error) {
	var action upgradeAction
	opts := []huh.Option[upgradeAction]{
		huh.NewOption(fmt.Sprintf("Update now  (%s → %s)", version.Version, latest), upgradeActionNow),
		huh.NewOption("Skip this time", upgradeActionSkip),
		huh.NewOption(fmt.Sprintf("Skip %s (notify again on a newer release)", latest), upgradeActionSkipUntilNext),
	}
	f := uiform.New(huh.NewGroup(
		huh.NewSelect[upgradeAction]().
			Title(fmt.Sprintf("maz %s is available", latest)).
			Options(opts...).
			Value(&action),
	))
	if err := f.Run(); err != nil {
		// HandleFormCancellation prints "upgrade cancelled." for user aborts
		// and returns nil; other errors are wrapped and returned.
		cancelErr := uiform.HandleFormCancellation(cmd.OutOrStdout(), "upgrade", err)
		if cancelErr == nil {
			return upgradeActionCancelled, nil
		}
		return upgradeActionSkip, cancelErr
	}
	return action, nil
}

// renderManagedInstall prints the delegate command for a package-manager
// install (Homebrew, mise, Scoop) where maz cannot self-replace. The caller
// is responsible for routing JSON requests through runUpgradeJSON instead.
func renderManagedInstall(cmd *cobra.Command, info *upgradeInstallInfo) error {
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "maz was installed via %s; run `%s` to update\n",
		info.Method, info.UpgradeCommand)
	return err
}

// upgradeUpToDate reports whether the resolved release means no install is
// needed. A pinned --version is "up to date" only when it equals the running
// build, so any other explicit pin (even an older tag) still installs. The
// latest path is up to date unless the release is strictly newer, which avoids
// a surprise downgrade if "latest" ever resolves behind the running build.
func upgradeUpToDate(pinned, current, latest string) bool {
	if pinned != "" {
		return latest == current
	}
	return !selfupdate.Newer(current, latest)
}

// downloadAndSwap runs the verified-install pipeline into temp dirs and swaps
// the live install. Everything is staged and checksum-verified before any live
// file is touched, and the staged binary is smoke-tested before the swap.
func downloadAndSwap(cmd *cobra.Command, opts *selfupdate.Options, rel selfupdate.Release, loc selfupdate.Location) error {
	tmp, err := os.MkdirTemp("", "maz-upgrade-*")
	if err != nil {
		return fmt.Errorf("upgrade: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }() //nolint:errcheck // best-effort temp cleanup
	dlDir := filepath.Join(tmp, "dl")
	stageDir := filepath.Join(tmp, "stage")
	for _, d := range []string{dlDir, stageDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("upgrade: create temp dir: %w", err)
		}
	}

	archivePath, sumsPath, err := selfupdate.Download(cmd.Context(), opts, rel, dlDir)
	if err != nil {
		return tokenHint(err, opts.Token)
	}
	if err := selfupdate.VerifyChecksum(archivePath, sumsPath, rel.ArchiveName); err != nil {
		return err
	}
	if err := selfupdate.Extract(archivePath, stageDir); err != nil {
		return err
	}
	if err := selfupdate.Smoke(cmd.Context(), stageDir); err != nil {
		return err
	}
	if err := selfupdate.Swap(stageDir, loc); err != nil {
		return err
	}
	// Man pages are supplementary: install them best-effort so a write hiccup
	// under share/man never fails an otherwise-good binary/lib upgrade.
	if err := selfupdate.InstallManPages(stageDir, selfupdate.ManDir(loc.Prefix)); err != nil {
		//nolint:errcheck // advisory: the upgrade itself succeeded
		fmt.Fprintf(cmd.ErrOrStderr(), "upgraded; man pages not installed: %v\n", err)
	}
	return nil
}

// tokenHint augments a release-access error with an actionable token hint when
// no token was set: a private (beta) repo answers 404 to an anonymous request,
// which is otherwise a confusing "not found".
func tokenHint(err error, token string) error {
	if token == "" {
		return fmt.Errorf("%w: if this is a private (beta) release, set GITHUB_TOKEN (or MAZ_INSTALL_TOKEN) and retry", err)
	}
	return err
}

// restartDaemonAfterUpgrade restarts a running daemon so it picks up the new
// binary (a live daemon keeps serving the old in-memory code until restarted).
// It is best-effort: any failure prints an advisory and leaves the upgrade
// itself successful.
func restartDaemonAfterUpgrade(cmd *cobra.Command) {
	errw := cmd.ErrOrStderr()
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	info, err := daemon.ReadPID(daemon.DataDirPIDPath(dataDir))
	if err != nil || !info.Running || !daemon.IsMazDaemon(info.PID, info.Token) {
		return // no live daemon to restart
	}

	// A launchd-managed daemon is booted out by stop and relaunched by its
	// KeepAlive onto the new binary; a manual daemon must be respawned.
	loaded, _ := launchAgentLoadedFn(cmd.Context()) //nolint:errcheck // best-effort ownership hint
	if serr := runDaemonStop(cmd, io.Discard); serr != nil {
		_, _ = fmt.Fprintf(errw, "upgraded; could not stop the daemon to restart it: %v\n  run `maz daemon stop` then `maz daemon start`\n", serr) //nolint:errcheck // advisory
		return
	}
	if loaded {
		_, _ = fmt.Fprintln(errw, "restarted the launchd-managed daemon onto the new version") //nolint:errcheck // advisory
		return
	}
	if _, serr := spawnDaemonOnce(cmd.Context(), dataDir, ""); serr != nil {
		_, _ = fmt.Fprintf(errw, "upgraded; could not restart the daemon: %v\n  run `maz daemon start` to bring it back\n", serr) //nolint:errcheck // advisory
		return
	}
	_, _ = fmt.Fprintln(errw, "restarted the daemon onto the new version") //nolint:errcheck // advisory
}

func renderCheck(cmd *cobra.Command, latest string) error {
	w := cmd.OutOrStdout()
	upToDate := !selfupdate.Newer(version.Version, latest)
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"status":   "checked",
			"current":  version.Version,
			"latest":   latest,
			"upToDate": upToDate,
		})
	}
	if upToDate {
		_, err := fmt.Fprintf(w, "up to date (%s)\n", version.Version)
		return err
	}
	_, err := fmt.Fprintf(w, "%s is available (you have %s); run `maz upgrade`\n", latest, version.Version)
	return err
}

func renderUpToDate(cmd *cobra.Command, tag string) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"status":  "up_to_date",
			"version": tag,
		})
	}
	_, err := fmt.Fprintf(w, "already up to date (%s)\n", tag)
	return err
}

func renderUpgraded(cmd *cobra.Command, from, to string) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"status": "upgraded",
			"from":   from,
			"to":     to,
		})
	}
	_, err := fmt.Fprintf(w, "upgraded maz %s -> %s\n", from, to)
	return err
}
