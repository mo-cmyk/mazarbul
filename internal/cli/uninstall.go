package cli

// uninstall.go — maz uninstall: enumerate-then-consent teardown in three tiers.
//
// Safe: launchd agents, MCP client entries, .maz-backup-* orphans,
// shell-completion lines, and the daemon log. Removed on y/N consent.
// Destructive: data directory (the immutable event log + graph + vault)
// and keychain entries incl. the watchword. Removed only on typed DELETE.
// Manual: the maz binary — we detect the install method and print the
// right removal command; we never guess-and-rm.
//
// --dry-run shows the full inventory without touching anything.
// --yes skips the y/N prompt for the safe tier. --purge adds the destructive
// tier to scope (typed DELETE still required). --keep-data suppresses the
// destructive tier entirely.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/connect"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// uninstallTier classifies an item by its safety profile.
type uninstallTier string

// tierSafe items carry no user data — launchd agents, MCP entries,
// backup orphans, the completion snippet, and the daemon log.
const tierSafe uninstallTier = "safe"

// tierDestructive items are irreversible: the data directory (signed
// event log + graph + vault) and keychain secrets.
const tierDestructive uninstallTier = "destructive"

// tierManual items must be removed by the user; we print the command.
const tierManual uninstallTier = "manual"

// uninstallItem is one thing that can be uninstalled.
type uninstallItem struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Tier        uninstallTier `json:"tier"`
	// Present reports whether this component was found on-disk / in the
	// keychain at inventory time.
	Present bool     `json:"present"`
	Paths   []string `json:"paths,omitempty"`
	// Hint carries the user-facing removal instruction for manual-tier items.
	Hint string `json:"hint,omitempty"`
	// Err carries any inventory-probe error (does not prevent inventory output).
	Err string `json:"error,omitempty"`
	// remove executes the actual removal. Never called in dry-run mode.
	remove func(ctx context.Context) error `json:"-"`
}

// uninstallInventory is the top-level JSON shape for --output json.
type uninstallInventory struct {
	DryRun bool            `json:"dryRun"`
	Items  []uninstallItem `json:"items"`
}

// uninstallFlags holds the parsed flag values for the uninstall command.
type uninstallFlags struct {
	dryRun   bool
	keepData bool
	purge    bool
	yes      bool
}

// uninstallDeps holds injectable dependencies for the uninstall command.
// Nil fields fall back to the real production implementations, so production
// callers can pass nil and tests can override specific deps.
type uninstallDeps struct {
	// dataDir is the resolved data directory. Empty = resolve from flags/config.
	dataDir string
	// secrets is the provider to use for keychain operations.
	// nil = secret.PersistentChain()
	secrets secret.Provider
	// connectSvc is the service to use for MCP uninstall.
	// nil = buildConnectService() from config
	connectSvc *connect.Service
	// promptSafe, if non-nil, overrides the y/N prompt for the safe tier.
	promptSafe func(w io.Writer, label string) (bool, error)
	// promptDeleteToken, if non-nil, overrides the typed-DELETE prompt.
	promptDeleteToken func(w io.Writer) (bool, error)
}

// newUninstallCmd returns the cobra command wired with production dependencies.
// It is wired into root.go; tests use makeUninstallCmd with overrides instead.
func newUninstallCmd() *cobra.Command {
	return makeUninstallCmd(nil)
}

// makeUninstallCmd is the testable harness — accepts overridable deps.
func makeUninstallCmd(deps *uninstallDeps) *cobra.Command {
	var flags uninstallFlags
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove Mazarbul components from this machine",
		Long: `Enumerate and optionally remove every Mazarbul component in three tiers.

SAFE (no data loss): launchd agents (daemon + watch-fs), MCP entries for all
connected AI clients, .maz-backup-* orphan files, the shell-completion line in
your rc file, and the daemon log. Removed on y/N confirmation or --yes.

DESTRUCTIVE (irreversible): the data directory (signed immutable event log,
graph, vault) and keychain secrets including the watchword — deleting the
watchword makes encrypted sync scopes unrecoverable. Also the io.mazarbul.ollama
launchd agent (stops your Ollama). Removed only after typing DELETE. Opt-in
with --purge.

MANUAL: the maz binary. The command detects the install method and prints the
right removal command (e.g. "brew uninstall mazarbul"); it never guesses a path
and deletes it.

Without --yes or --purge the command prints the inventory and exits (dry-run
posture). Always safe to run; nothing is touched without explicit consent.

Exit codes:
  0   completed (or dry-run showed inventory)
  1   user cancelled or consent refused
  2   unexpected error during removal

Examples:
  # Show what would be removed (safe to run, nothing is touched)
  maz uninstall

  # Remove safe components after y/N confirmation
  maz uninstall --yes

  # Remove everything including data directory + keychain (requires typing DELETE)
  maz uninstall --yes --purge

  # Machine-readable inventory for automation
  maz uninstall --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUninstall(cmd, flags, deps)
		},
	}
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false,
		"show what would be removed without removing anything")
	cmd.Flags().BoolVar(&flags.keepData, "keep-data", false,
		"exclude data directory and keychain entries from scope")
	cmd.Flags().BoolVar(&flags.purge, "purge", false,
		"include data directory and keychain entries (requires typing DELETE)")
	cmd.Flags().BoolVar(&flags.yes, "yes", false,
		"remove safe items without interactive y/N confirmation")
	return cmd
}

// uninstallRuntime holds the resolved runtime dependencies for a single run.
type uninstallRuntime struct {
	dataDir      string
	secrets      secret.Provider
	connectSvc   *connect.Service
	promptSafe   func(io.Writer, string) (bool, error)
	promptDelete func(io.Writer) (bool, error)
}

// resolveRuntime resolves all injectable deps, falling back to prod implementations.
func resolveRuntime(cmd *cobra.Command, deps *uninstallDeps) uninstallRuntime {
	r := uninstallRuntime{secrets: secret.PersistentChain()}

	if deps != nil && deps.dataDir != "" {
		r.dataDir = deps.dataDir
	} else {
		var err error
		r.dataDir, err = resolveDataDir(cmd)
		if err != nil {
			r.dataDir = config.Defaults().DataDir
		}
	}
	if deps != nil && deps.secrets != nil {
		r.secrets = deps.secrets
	}
	if deps != nil && deps.connectSvc != nil {
		r.connectSvc = deps.connectSvc
	} else {
		svc, err := buildConnectService()
		if err == nil {
			r.connectSvc = svc
		}
	}
	if deps != nil {
		r.promptSafe = deps.promptSafe
		r.promptDelete = deps.promptDeleteToken
	}
	return r
}

// runUninstall is the command body: resolve deps, build inventory, consent, act.
func runUninstall(cmd *cobra.Command, flags uninstallFlags, deps *uninstallDeps) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	r := resolveRuntime(cmd, deps)

	items := enumerateItems(ctx, r.dataDir, r.secrets, r.connectSvc, flags)

	wr := output.From(cmd)
	w := cmd.OutOrStdout()
	clz := colorizer.New(interactive.ShouldStyle(cmd, w), w)

	actionRequested := flags.yes || flags.purge
	isDryRun := flags.dryRun || (!actionRequested && !interactive.CanPromptInteractively())

	if wr.Format() != output.FormatText {
		inv := uninstallInventory{DryRun: isDryRun, Items: items}
		return json.NewEncoder(w).Encode(inv)
	}

	renderInventoryText(w, clz, items, isDryRun, flags)
	if isDryRun {
		return nil
	}

	cancelled, err := executeSafeTier(ctx, w, clz, items, flags, r.promptSafe)
	if err != nil || cancelled {
		return err
	}
	cancelled, err = executeDestructiveTier(ctx, w, clz, items, flags, r.promptDelete)
	if err != nil || cancelled {
		return err
	}

	printManualTier(w, clz, items)
	fmt.Fprintln(w, clz.Success("✓")+" uninstall complete.") //nolint:errcheck // best-effort TTY write
	return nil
}

// executeSafeTier handles consent + removal for the safe tier.
// Returns (cancelled=true, nil) when the user declines the prompt.
func executeSafeTier(
	ctx context.Context,
	w io.Writer,
	clz *colorizer.Colorizer,
	items []uninstallItem,
	flags uninstallFlags,
	promptOverride func(io.Writer, string) (bool, error),
) (cancelled bool, err error) {
	safeItems := itemsOfTier(items, tierSafe)
	if len(safeItems) == 0 {
		return false, nil
	}
	if !flags.yes {
		confirmed, pErr := promptSafe(w, promptOverride, "Remove safe components listed above? (no data loss)")
		if pErr != nil {
			return false, pErr
		}
		if !confirmed {
			fmt.Fprintln(w, "uninstall cancelled.") //nolint:errcheck // best-effort TTY write
			return true, nil
		}
	}
	executeItems(ctx, w, clz, safeItems)
	return false, nil
}

// executeDestructiveTier handles the typed-DELETE gate + removal for destructive items.
// Returns (cancelled=true, nil) when the user declines or the token is wrong.
func executeDestructiveTier(
	ctx context.Context,
	w io.Writer,
	clz *colorizer.Colorizer,
	items []uninstallItem,
	flags uninstallFlags,
	promptOverride func(io.Writer) (bool, error),
) (cancelled bool, err error) {
	if !flags.purge || flags.keepData {
		return false, nil
	}
	present := presentItems(itemsOfTier(items, tierDestructive))
	if len(present) == 0 {
		return false, nil
	}

	fmt.Fprintln(w)                                                                 //nolint:errcheck // best-effort TTY write
	fmt.Fprintln(w, clz.Warn("! WARNING: The following actions are irreversible:")) //nolint:errcheck // best-effort TTY write
	for _, it := range present {
		fmt.Fprintf(w, "  %s  %s\n", clz.Warn("!"), it.Description) //nolint:errcheck // best-effort TTY write
	}
	fmt.Fprintln(w)                                                                                  //nolint:errcheck // best-effort TTY write
	fmt.Fprintln(w, clz.Warn("  Deleting the watchword makes encrypted sync scopes unrecoverable.")) //nolint:errcheck // best-effort TTY write
	fmt.Fprintln(w)                                                                                  //nolint:errcheck // best-effort TTY write

	ok, pErr := promptDeleteToken(w, promptOverride)
	if pErr != nil {
		return false, pErr
	}
	if !ok {
		fmt.Fprintln(w, "destructive removal cancelled (wrong token or cancelled).") //nolint:errcheck // best-effort TTY write
		return true, nil
	}
	executeItems(ctx, w, clz, present)
	return false, nil
}

// executeItems runs the remove closure for each present item, printing status.
func executeItems(ctx context.Context, w io.Writer, clz *colorizer.Colorizer, items []uninstallItem) {
	for i := range items {
		it := items[i]
		if !it.Present || it.remove == nil {
			continue
		}
		if err := it.remove(ctx); err != nil {
			fmt.Fprintf(w, "%s  %s: %v\n", clz.Warn("!"), it.Description, err) //nolint:errcheck // best-effort TTY write
		} else {
			fmt.Fprintf(w, "%s  %s\n", clz.Success("✓"), it.Description) //nolint:errcheck // best-effort TTY write
		}
	}
}

// printManualTier prints the removal hint for any present manual-tier items.
func printManualTier(w io.Writer, clz *colorizer.Colorizer, items []uninstallItem) {
	for _, it := range itemsOfTier(items, tierManual) {
		if it.Present {
			fmt.Fprintf(w, "%s  %s: %s\n", clz.Accent("→"), it.Description, it.Hint) //nolint:errcheck // best-effort TTY write
		}
	}
}

// enumerateItems builds the full inventory without performing any removal.
// It probes what is present on the current system.
func enumerateItems(
	ctx context.Context,
	dataDir string,
	secrets secret.Provider,
	connectSvc *connect.Service,
	flags uninstallFlags,
) []uninstallItem {
	items := make([]uninstallItem, 0, 16)

	// ── SAFE tier ──────────────────────────────────────────────────────────

	// 1+2. Daemon and watch-FS launchd agents (macOS-only; stubs return false on other OSes).
	for _, la := range []struct {
		name, description string
		plistPath         func() (string, error)
		loadedFn          func(context.Context) (bool, error)
		removeFn          func(context.Context) error
	}{
		{
			"daemon-launchd-agent",
			"Mazarbul daemon launchd agent (" + daemon.LaunchAgentLabel + ")",
			daemon.LaunchAgentPlistPath,
			daemon.LaunchAgentLoaded,
			daemon.UninstallLaunchAgent,
		},
		{
			"watchfs-launchd-agent",
			"Mazarbul watch-fs launchd agent (" + daemon.WatchFSLaunchAgentLabel + ")",
			daemon.WatchFSLaunchAgentPlistPath,
			daemon.WatchFSLaunchAgentLoaded,
			daemon.UninstallWatchFSLaunchAgent,
		},
	} {
		items = append(items, probeLaunchdItem(ctx, la.name, la.description, la.plistPath, la.loadedFn, la.removeFn))
	}

	// 3. MCP entries in AI client config files.
	if connectSvc != nil {
		for _, c := range connectSvc.Clients() {
			// Only global/user scope; we do not enumerate per-project entries
			// (those belong to the project, not to the maz install).
			it := probeMCPClient(ctx, connectSvc, c, dataDir)
			items = append(items, it)
		}
	}

	// 4. .maz-backup-* orphan files.
	backupItems := probeBackupFiles(ctx, connectSvc)
	items = append(items, backupItems...)

	// 5. Shell-completion line in shell RC files.
	items = append(items, probeCompletionLine())

	// 6. Daemon log file.
	logPath := config.DaemonLogFile()
	items = append(items, probePathItem(
		"daemon-log",
		"Daemon log file",
		logPath,
		tierSafe,
		func(ctx context.Context) error { return os.Remove(logPath) },
	))

	// ── DESTRUCTIVE tier ───────────────────────────────────────────────────

	if !flags.keepData {
		// 7. Ollama launchd agent (macOS-only; destructive because it stops Ollama).
		items = append(items, probeLaunchdItemTier(
			ctx,
			"ollama-launchd-agent",
			"Mazarbul-managed Ollama launchd agent ("+daemon.OllamaLaunchAgentLabel+"); stops Ollama",
			tierDestructive,
			daemon.OllamaLaunchAgentPlistPath,
			daemon.OllamaLaunchAgentLoaded,
			daemon.UninstallOllamaLaunchAgent,
		))

		// 8. Data directory.
		dataDirPresent := false
		if _, err := os.Stat(dataDir); err == nil {
			dataDirPresent = true
		}
		items = append(items, uninstallItem{
			Name:        "data-dir",
			Description: "Data directory (event log, graph, vault): " + dataDir,
			Tier:        tierDestructive,
			Present:     dataDirPresent,
			Paths:       []string{dataDir},
			remove: func(ctx context.Context) error {
				return os.RemoveAll(dataDir)
			},
		})

		// 9. Keychain entries.
		keychainKeys := keychainEntries()
		for _, kk := range keychainKeys {
			k := kk // capture
			present := false
			var probeErr string
			if _, err := secrets.Get(ctx, k.name); err == nil {
				present = true
			} else if !errors.Is(err, secret.ErrNotFound) {
				probeErr = err.Error()
			}
			items = append(items, uninstallItem{
				Name:        "keychain-" + strings.ReplaceAll(k.name, ":", "-"),
				Description: k.description,
				Tier:        tierDestructive,
				Present:     present,
				Err:         probeErr,
				remove: func(ctx context.Context) error {
					err := secrets.Delete(ctx, k.name)
					if errors.Is(err, secret.ErrNotFound) {
						return nil // already gone, not an error
					}
					return err
				},
			})
		}
	}

	// ── MANUAL tier ────────────────────────────────────────────────────────

	items = append(items, probeBinary())

	return items
}

// keychainEntry is a secret name + human description pair.
type keychainEntry struct {
	name        string
	description string
}

// keychainEntries returns the complete list of keychain entries maz owns.
// The watchword entry is listed first and with a prominent warning in the
// description because losing it makes encrypted sync scopes unrecoverable.
func keychainEntries() []keychainEntry {
	return []keychainEntry{
		{
			watchword.KeychainKey,
			"Keychain: watchword (mazarbul:watchword) — DELETING MAKES ENCRYPTED SYNC UNRECOVERABLE",
		},
		{identity.KeychainEd25519Seed, "Keychain: signing key (mazarbul:identity:ed25519)"},
		{identity.KeychainX25519Seed, "Keychain: wrap key (mazarbul:identity:x25519)"},
		{identity.KeychainDeviceID, "Keychain: device ID (mazarbul:identity:device)"},
		{secret.NameAnthropicAPIKey, "Keychain: Anthropic API key"},
		{secret.NameOpenAIAPIKey, "Keychain: OpenAI API key"},
		{secret.NameGranolaAPIKey, "Keychain: Granola API key"},
	}
}

// probeLaunchdItem builds a safe-tier launchd inventory item.
func probeLaunchdItem(
	ctx context.Context,
	name, description string,
	plistPathFn func() (string, error),
	loadedFn func(context.Context) (bool, error),
	removeFn func(context.Context) error,
) uninstallItem {
	return probeLaunchdItemTier(ctx, name, description, tierSafe, plistPathFn, loadedFn, removeFn)
}

// probeLaunchdItemTier builds a launchd inventory item with an explicit tier.
func probeLaunchdItemTier(
	ctx context.Context,
	name, description string,
	tier uninstallTier,
	plistPathFn func() (string, error),
	loadedFn func(context.Context) (bool, error),
	removeFn func(context.Context) error,
) uninstallItem {
	plistPath, plistErr := plistPathFn()
	if plistErr != nil {
		// Non-Darwin platform: launchd not available — skip gracefully.
		return uninstallItem{
			Name:        name,
			Description: description + " (not applicable on " + runtime.GOOS + ")",
			Tier:        tier,
			Present:     false,
		}
	}

	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
	defer probeCancel()

	loaded, _ := loadedFn(probeCtx) //nolint:errcheck // probe; error treated as not-loaded
	_, statErr := os.Stat(plistPath)
	present := loaded || statErr == nil

	return uninstallItem{
		Name:        name,
		Description: description,
		Tier:        tier,
		Present:     present,
		Paths:       []string{plistPath},
		remove: func(ctx context.Context) error {
			// removeFn (e.g. daemon.UninstallLaunchAgent) handles launchctl bootout
			// and plist removal atomically. Treat not-exist as already-clean.
			if err := removeFn(ctx); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		},
	}
}

// probeMCPClient builds a safe-tier MCP inventory item for one client.
func probeMCPClient(ctx context.Context, svc *connect.Service, c connect.Client, dataDir string) uninstallItem {
	// Probe: check whether the config file exists at the global scope.
	configPath, pathErr := c.ConfigPath(connect.ScopeUser, "")
	var paths []string
	present := false
	var probErr string

	if pathErr == nil {
		paths = []string{configPath}
		if _, err := os.Stat(configPath); err == nil {
			present = true
		}
	} else {
		probErr = pathErr.Error()
	}

	cID := c.ID()
	return uninstallItem{
		Name:        "mcp-" + cID,
		Description: "MCP entry in " + c.DisplayName() + " config",
		Tier:        tierSafe,
		Present:     present,
		Paths:       paths,
		Err:         probErr,
		remove: func(ctx context.Context) error {
			_, err := svc.Uninstall(ctx, cID, connect.ScopeUser, "")
			if err != nil && errors.Is(err, connect.ErrUnknownClient) {
				return nil // client removed from registry; skip silently
			}
			return err
		},
	}
}

// probeBackupFiles enumerates .maz-backup-* files in each client's config
// directory and returns one safe-tier item per directory that has orphans.
func probeBackupFiles(_ context.Context, connectSvc *connect.Service) []uninstallItem {
	dirSet := backupDirSet(connectSvc)
	items := make([]uninstallItem, 0, len(dirSet))
	for dir := range dirSet {
		matches, err := filepath.Glob(filepath.Join(dir, "*.maz-backup-*"))
		if err != nil || len(matches) == 0 {
			continue
		}
		items = append(items, dirBackupItem(dir, matches))
	}
	return items
}

// backupDirSet returns the set of config directories to search for orphaned backups.
func backupDirSet(connectSvc *connect.Service) map[string]struct{} {
	dirs := map[string]struct{}{}
	if connectSvc == nil {
		return dirs
	}
	for _, c := range connectSvc.Clients() {
		if p, err := c.ConfigPath(connect.ScopeUser, ""); err == nil {
			dirs[filepath.Dir(p)] = struct{}{}
		}
	}
	return dirs
}

// dirBackupItem builds a safe-tier item for orphaned backup files in dir.
func dirBackupItem(dir string, matches []string) uninstallItem {
	return uninstallItem{
		Name:        "backups-" + filepath.Base(dir),
		Description: fmt.Sprintf(".maz-backup-* orphan files in %s (%d file(s))", dir, len(matches)),
		Tier:        tierSafe,
		Present:     true,
		Paths:       matches,
		remove: func(_ context.Context) error {
			return removeMatchedFiles(matches)
		},
	}
}

// removeMatchedFiles removes each path, ignoring not-exist errors.
func removeMatchedFiles(paths []string) error {
	var errs []error
	for _, f := range paths {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("remove backups: %v", errs)
	}
	return nil
}

// completionMarker is the sentinel string written by `maz completion install`.
const completionMarker = "# maz shell completion"

// probeCompletionLine checks each known shell RC file for the maz completion
// block and returns a safe-tier inventory item.
func probeCompletionLine() uninstallItem {
	rcFiles := shellRCCandidates()
	var present bool
	var foundPaths []string

	for _, rc := range rcFiles {
		if containsCompletionMarker(rc) {
			present = true
			foundPaths = append(foundPaths, rc)
		}
	}

	captured := foundPaths // close over for remove func
	return uninstallItem{
		Name:        "completion-line",
		Description: "Shell completion snippet in RC file(s)",
		Tier:        tierSafe,
		Present:     present,
		Paths:       foundPaths,
		remove: func(ctx context.Context) error {
			var errs []error
			for _, rc := range captured {
				if err := removeCompletionBlock(rc); err != nil {
					errs = append(errs, err)
				}
			}
			if len(errs) > 0 {
				return fmt.Errorf("remove completion: %v", errs)
			}
			return nil
		},
	}
}

// shellRCCandidates returns all RC files maz may have written to.
func shellRCCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".config", "fish", "config.fish"),
	}
}

// containsCompletionMarker reports whether path exists and contains the
// maz completion sentinel.
func containsCompletionMarker(path string) bool {
	f, err := os.Open(filepath.Clean(path)) // #nosec G304 -- path comes from fixed home-relative list
	if err != nil {
		return false
	}
	defer f.Close() // read-only probe; close error is inconsequential
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.Contains(sc.Text(), completionMarker) {
			return true
		}
	}
	return false
}

// removeCompletionBlock rewrites path omitting the two-line block written by
// `maz completion install`:
//
//	# maz shell completion
//	source <(maz completion <shell>)   // bash / zsh
//	  — or —
//	maz completion fish | source       // fish
//
// Matching is on the comment marker, so the second line is removed whatever its
// shell-specific form. If the file does not exist or the block is absent this is
// a no-op.
func removeCompletionBlock(path string) error {
	data, err := os.ReadFile(filepath.Clean(path)) // #nosec G304
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	skip := false
	for _, line := range lines {
		if strings.Contains(line, completionMarker) {
			skip = true // skip this line and the next (the source line)
			continue
		}
		if skip {
			skip = false // source line was just skipped; resume
			continue
		}
		out = append(out, line)
	}

	result := strings.Join(out, "\n")
	// Trim a trailing blank line that removal may leave.
	result = strings.TrimRight(result, "\n") + "\n"

	return os.WriteFile(filepath.Clean(path), []byte(result), 0o644) // #nosec G306
}

// probePathItem returns an inventory item that probes path presence and removes
// it with os.Remove on execution.
func probePathItem(name, description, path string, tier uninstallTier, removeFn func(context.Context) error) uninstallItem {
	_, err := os.Stat(path)
	present := err == nil
	return uninstallItem{
		Name:        name,
		Description: description + ": " + path,
		Tier:        tier,
		Present:     present,
		Paths:       []string{path},
		remove:      removeFn,
	}
}

// probeBinary detects the running binary's install method and returns a
// manual-tier item with the appropriate removal hint.
func probeBinary() uninstallItem {
	exe, err := daemon.ResolveExecutable()
	if err != nil {
		return uninstallItem{
			Name:        "binary",
			Description: "maz binary (could not locate)",
			Tier:        tierManual,
			Present:     false,
			Err:         err.Error(),
		}
	}

	// Reuse upgrade.go's install-method detection (which handles mise/scoop in
	// addition to Homebrew) so a package-managed binary gets the matching
	// uninstall command instead of a bare `rm` that leaves a dangling registry
	// entry.
	var hint string
	switch detectInstallInfo(exe).Method {
	case installMethodHomebrew:
		hint = "brew uninstall mazarbul"
	case installMethodMise:
		hint = "mise uninstall mazarbul"
	case installMethodScoop:
		hint = "scoop uninstall mazarbul"
	default: // tarball / unknown
		hint = "rm " + exe
	}

	return uninstallItem{
		Name:        "binary",
		Description: "maz binary: " + exe,
		Tier:        tierManual,
		Present:     true,
		Paths:       []string{exe},
		Hint:        hint,
		// remove is intentionally nil — binary removal is always manual.
	}
}

// itemsOfTier filters items to those matching tier.
func itemsOfTier(items []uninstallItem, tier uninstallTier) []uninstallItem {
	var out []uninstallItem
	for _, it := range items {
		if it.Tier == tier {
			out = append(out, it)
		}
	}
	return out
}

// presentItems filters to items where Present == true.
func presentItems(items []uninstallItem) []uninstallItem {
	var out []uninstallItem
	for _, it := range items {
		if it.Present {
			out = append(out, it)
		}
	}
	return out
}

// renderInventoryText writes the three-tier inventory to w in human format.
func renderInventoryText(w io.Writer, clz *colorizer.Colorizer, items []uninstallItem, dryRun bool, flags uninstallFlags) {
	if dryRun {
		fmt.Fprintln(w, clz.Dim("dry run — nothing will be removed")) //nolint:errcheck // best-effort TTY write
		fmt.Fprintln(w)                                               //nolint:errcheck // best-effort TTY write
	}
	for _, td := range []struct {
		tier  uninstallTier
		label string
	}{
		{tierSafe, "SAFE (no data loss)"},
		{tierDestructive, "DESTRUCTIVE (irreversible)"},
		{tierManual, "MANUAL (print removal command only)"},
	} {
		renderTierSection(w, clz, itemsOfTier(items, td.tier), td.label)
	}
	if dryRun {
		renderDryRunHints(w, clz, flags)
	}
}

// renderTierSection prints the label and each item row for one tier.
func renderTierSection(w io.Writer, clz *colorizer.Colorizer, tierItems []uninstallItem, label string) {
	fmt.Fprintf(w, "%s\n", clz.Meta(label)) //nolint:errcheck // best-effort TTY write
	for i := range tierItems {
		it := &tierItems[i]
		sym := itemSymbol(clz, it)
		line := fmt.Sprintf("  %s  %s", sym, it.Description)
		if it.Hint != "" {
			line += clz.Dim("  (" + it.Hint + ")")
		}
		fmt.Fprintln(w, line) //nolint:errcheck // best-effort TTY write
		if it.Err != "" {
			fmt.Fprintf(w, "     %s probe error: %s\n", clz.Warn("!"), it.Err) //nolint:errcheck // best-effort TTY write
		}
	}
	fmt.Fprintln(w) //nolint:errcheck // best-effort TTY write
}

// itemSymbol returns the status symbol for an inventory item row.
func itemSymbol(clz *colorizer.Colorizer, it *uninstallItem) string {
	if !it.Present {
		return clz.Dim("○")
	}
	switch it.Tier {
	case tierDestructive:
		return clz.Warn("!")
	case tierManual:
		return clz.Accent("→")
	default:
		return clz.Success("●")
	}
}

// renderDryRunHints prints the "--yes / --purge" action hint footer.
func renderDryRunHints(w io.Writer, clz *colorizer.Colorizer, flags uninstallFlags) {
	var hints []string
	if !flags.yes {
		hints = append(hints, "--yes to remove safe items")
	}
	if !flags.purge && !flags.keepData {
		hints = append(hints, "--purge to include data directory + keychain")
	}
	if len(hints) > 0 {
		fmt.Fprintf(w, "%s  %s\n", clz.Dim("→"), //nolint:errcheck // best-effort TTY write
			clz.Dim("run with "+strings.Join(hints, ", ")))
	}
}

// promptSafe calls uiform.PromptYN for the safe tier or uses the override.
func promptSafe(w io.Writer, override func(io.Writer, string) (bool, error), label string) (bool, error) {
	if override != nil {
		return override(w, label)
	}
	return uiform.PromptYN(w, label, false)
}

// promptDeleteToken presents the typed-DELETE confirmation flow.
// Returns (true, nil) when the user typed "DELETE" exactly.
// Returns (false, nil) when non-interactive (can't prompt), or user cancelled.
func promptDeleteToken(w io.Writer, override func(io.Writer) (bool, error)) (bool, error) {
	if override != nil {
		return override(w)
	}
	if !interactive.CanPromptInteractively() {
		fmt.Fprintln(w, clzDim("data/keychain removal requires interactive typed DELETE; skipped in non-interactive mode")) //nolint:errcheck // best-effort TTY write
		return false, nil
	}

	var token string
	f := uiform.New(huh.NewGroup(
		huh.NewInput().
			Title(`Type "DELETE" to permanently remove data directory and keychain entries`).
			Description("This cannot be undone. The watchword makes encrypted sync scopes unrecoverable.").
			Value(&token),
	))
	if err := f.Run(); err != nil {
		return false, uiform.HandleFormCancellation(w, "delete confirmation", err)
	}
	return token == "DELETE", nil
}

// clzDim is a fallback dim helper used in non-colorizer contexts.
// When color is unavailable we just return the string as-is.
func clzDim(s string) string { return s }
