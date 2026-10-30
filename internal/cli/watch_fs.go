package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/watchfs"
)

// defaultWatchFSScope is the scope captured file-change episodes land in
// when neither a flag nor config overrides it. Project is the right
// default: file changes are repo-bound, so the memory belongs to the
// project's in-repo, syncable scope rather than the local-only personal
// one.
const defaultWatchFSScope = "project"

// defaultWatchFSConfidence is the writer confidence stamped on captured
// episodes. Below 1.0 because a file change is a weaker signal of a
// durable fact than an explicit remember() call.
const defaultWatchFSConfidence = 0.6

// watchFSFlags holds the parsed flag values for "maz watch-fs".
type watchFSFlags struct {
	scope      string
	sizeCap    int
	debounceMs int
	ignore     []string
	confidence float64
	internal   bool
}

func newWatchFSCmd() *cobra.Command {
	flags := &watchFSFlags{}
	cmd := &cobra.Command{
		Use:   "watch-fs [directories...]",
		Short: "Watch directories and turn file changes into signed memory (agent-agnostic)",
		Long: `Run a long-lived filesystem watcher that captures every text-file change
under the given directories as a signed remember() episode.

This is the agent-agnostic passive-capture path: it needs nothing but the
directories to watch, so hookless agents (cursor, cline, zed, raw API
usage) and plain editor saves all land memory. Each change becomes a
signed episode (source "fs-watch:<project>:<changeKind>") discoverable via
'maz recall' and attributed via the event's EventAuthor signature.

Directories default to the [watch_fs] roots in config.toml when omitted.
Ignore rules honour a built-in denylist (.git, node_modules, build output,
binaries) plus each root's .gitignore; rapid edits are debounced and
identical content is de-duplicated by the write path.

Runs in the foreground until Ctrl-C. Use 'maz watch-fs install' to hand
the watcher to launchd so it persists across logout and reboot.

Examples:
  # Watch a repo in the foreground
  maz watch-fs ~/code/myrepo

  # Watch the roots configured in config.toml under [watch_fs]
  maz watch-fs

  # Persist under launchd (macOS)
  maz watch-fs install ~/code/myrepo`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatchFS(cmd, args, flags)
		},
	}
	bindWatchFSFlags(cmd, flags)
	cmd.AddCommand(newWatchFSInstallCmd())
	cmd.AddCommand(newWatchFSUninstallCmd())
	cmd.AddCommand(newWatchFSStatusCmd())
	return cmd
}

// bindWatchFSFlags registers the shared watcher tuning flags on cmd.
func bindWatchFSFlags(cmd *cobra.Command, flags *watchFSFlags) {
	cmd.Flags().StringVar(&flags.scope, "scope", "",
		`sync/encryption scope for captured episodes: personal, project, or team (default "project")`)
	cmd.Flags().IntVar(&flags.sizeCap, "size-cap", 0,
		"max bytes of file content captured per change (default 4096)")
	cmd.Flags().IntVar(&flags.debounceMs, "debounce", 0,
		"per-path debounce window in milliseconds (default 500)")
	cmd.Flags().StringArrayVar(&flags.ignore, "ignore", nil,
		"extra .gitignore-syntax pattern to skip (repeatable)")
	cmd.Flags().Float64Var(&flags.confidence, "confidence", 0,
		"writer confidence stamped on captured episodes, range 0..1 (default 0.6)")
	cmd.Flags().BoolVar(&flags.internal, "internal", false,
		"internal: run the watch loop (used by the launchd agent)")
	//nolint:errcheck // MarkHidden only fails if the flag doesn't exist; "internal" is registered above
	_ = cmd.Flags().MarkHidden("internal")
}

// runWatchFS resolves configuration, builds the watcher, and runs it in
// the foreground until the process receives SIGINT/SIGTERM.
func runWatchFS(cmd *cobra.Command, args []string, flags *watchFSFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	dataDir, err := resolveDataDir(cmd)
	if err != nil {
		return err
	}

	opts, scope, confidence, err := resolveWatchFSOptions(cmd, args, flags, dataDir, logger)
	if err != nil {
		return err
	}
	watcher, err := watchfs.New(opts)
	if err != nil {
		return err
	}

	signalCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Refuse to start a second watcher against the same data dir: a live
	// watch-fs pidfile (foreground or launchd-managed) means another
	// process is already capturing into this store, and running a second
	// would double every episode. WritePID itself only refuses a live
	// daemon (IsMazDaemon), not a watcher, so the guard is explicit here.
	pidPath := daemon.WatchFSPIDPath(dataDir)
	if info, rerr := daemon.ReadPID(pidPath); rerr == nil && info.Running && info.Token == daemon.WatchFSPIDToken {
		return fmt.Errorf("watch-fs: already running for %s (pid %d); stop it first", dataDir, info.PID)
	}

	// Record liveness so `maz watch-fs status` and `maz doctor` can see a
	// foreground (non-launchd) watcher. Best-effort: a write failure must
	// not stop capture, and the launchd path tracks liveness via launchctl.
	if perr := daemon.WritePID(pidPath, daemon.WatchFSPIDToken, dataDir); perr != nil {
		logger.Warn("watch-fs: record pidfile", zap.Error(perr))
	} else {
		defer func() {
			if derr := daemon.DeletePID(pidPath); derr != nil {
				logger.Warn("watch-fs: clear pidfile", zap.Error(derr))
			}
		}()
	}

	// Under launchd (--internal) stdout is redirected to a log file, so a
	// "Ctrl-C to stop" hint is misleading; announce the startup through the
	// structured logger instead. Foreground runs get the human header.
	if flags.internal {
		logger.Info("watch-fs: started", zap.Int("roots", len(watcher.Roots())), zap.String("scope", scope))
	} else {
		output.From(cmd).Header(fmt.Sprintf(
			"watch-fs: watching %d root(s) [scope=%s]; Ctrl-C to stop", len(watcher.Roots()), scope))
	}

	emit := watchFSEmitter(cmd, dataDir, logger, scope, confidence)
	if rerr := watcher.Run(signalCtx, emit); rerr != nil {
		return fmt.Errorf("watch-fs: %w", rerr)
	}
	return nil
}

// watchFSEmitter returns the EmitFunc that turns each Change into a signed
// remember() episode, routed through the same daemon-or-in-process path as
// `maz remember`.
func watchFSEmitter(cmd *cobra.Command, dataDir string, logger *zap.Logger, scope string, confidence float64) watchfs.EmitFunc {
	return func(ctx context.Context, c *watchfs.Change) error {
		in := map[string]any{
			"text":       watchfs.RenderText(c),
			"source":     c.Source(),
			"confidence": confidence,
		}
		if scope != "" {
			in["scope"] = scope
		}
		body, merr := json.Marshal(in)
		if merr != nil {
			return fmt.Errorf("marshal remember request: %w", merr)
		}
		raw, rerr := runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger, "remember", body, invokeRemember)
		if rerr != nil {
			return rerr
		}
		logger.Info("watch-fs: captured change",
			zap.String("relPath", c.RelPath), zap.String("kind", string(c.Kind)),
			zap.String("episode", episodeNameFromRemember(raw)))
		return nil
	}
}

// episodeNameFromRemember extracts the episode resource name from a
// remember response for logging; an unparseable body yields "".
func episodeNameFromRemember(raw json.RawMessage) string {
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ""
	}
	return out.Name
}

// resolveWatchFSOptions merges positional roots + flags + [watch_fs]
// config (flags win over config, positional roots win over config roots)
// into a watchfs.Options plus the resolved scope and confidence.
func resolveWatchFSOptions(cmd *cobra.Command, args []string, flags *watchFSFlags, dataDir string, logger *zap.Logger) (watchfs.Options, string, float64, error) {
	cfg := loadWatchFSConfig(dataDir, logger)

	roots := args
	if len(roots) == 0 {
		roots = cfg.Roots
	}
	if len(roots) == 0 {
		return watchfs.Options{}, "", 0, errors.New(
			"watch-fs: no directories to watch; pass one or more paths or set [watch_fs] roots in config.toml")
	}
	if err := validateWatchFSRoots(roots); err != nil {
		return watchfs.Options{}, "", 0, err
	}

	// Validate scope up front (mirroring the remember tool) so an invalid
	// --scope fails immediately rather than silently dropping every capture
	// when each emit's remember() call rejects it deep in the write path.
	scope := firstNonEmpty(flags.scope, cfg.Scope, defaultWatchFSScope)
	if err := validateWatchFSScope(scope); err != nil {
		return watchfs.Options{}, "", 0, err
	}

	sizeCap := cfg.SizeCapBytes
	if cmd.Flags().Changed("size-cap") {
		sizeCap = flags.sizeCap
	}

	debounce := time.Duration(cfg.DebounceMs) * time.Millisecond
	if cmd.Flags().Changed("debounce") {
		debounce = time.Duration(flags.debounceMs) * time.Millisecond
	}

	confidence := defaultWatchFSConfidence
	switch {
	case cmd.Flags().Changed("confidence"):
		confidence = flags.confidence
	case cfg.Confidence > 0:
		confidence = cfg.Confidence
	}

	return watchfs.Options{
		Roots:        roots,
		SizeCap:      sizeCap,
		Debounce:     debounce,
		ExtraIgnores: slices.Concat(cfg.Ignore, flags.ignore),
		Logger:       logger,
	}, scope, confidence, nil
}

// loadWatchFSConfig returns the [watch_fs] table for dataDir, or a zero
// table when config.toml is absent or unreadable (the watcher is then
// driven entirely by flags + positional args).
func loadWatchFSConfig(dataDir string, logger *zap.Logger) config.WatchFSTable {
	cfg, err := config.LoadUserConfig(dataDir)
	if err != nil {
		logger.Debug("watch-fs: load config", zap.Error(err))
		return config.WatchFSTable{}
	}
	return cfg.WatchFS
}

// validateWatchFSScope rejects a scope that is not one of the three
// recognised kernel scopes, matching the remember tool's validation so
// `maz watch-fs --scope <bad>` fails at startup with a clear message.
func validateWatchFSScope(scope string) error {
	switch scope {
	case string(kernel.ScopePersonal), string(kernel.ScopeProject), string(kernel.ScopeTeam):
		return nil
	default:
		return fmt.Errorf("watch-fs: invalid scope %q (allowed: personal, project, team)", scope)
	}
}

// validateWatchFSRoots reports the first root that is not an existing
// directory. Both the foreground watcher and the launchd installer call it
// so a missing path fails fast at the CLI: foreground would otherwise watch
// nothing (reads as a hang), and the installer would write a plist whose
// watcher stat-fails the root on every launch, making launchd KeepAlive
// restart it in a tight loop.
func validateWatchFSRoots(roots []string) error {
	for _, r := range roots {
		switch fi, err := os.Stat(r); {
		case err != nil:
			return fmt.Errorf("watch-fs: cannot watch %q: %w", r, err)
		case !fi.IsDir():
			return fmt.Errorf("watch-fs: %q is not a directory", r)
		}
	}
	return nil
}

// firstNonEmpty returns the first non-empty string in vals, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func newWatchFSInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install [directories...]",
		Short: "Install the watch-fs launchd agent (start now + relaunch on every login)",
		Long: `Hand the filesystem watcher's lifecycle to macOS launchd.

Writes ~/Library/LaunchAgents/io.mazarbul.watch-fs.plist and loads it,
which starts the watcher now (RunAtLoad) and relaunches it on every login
and after a crash (KeepAlive). Directories default to the [watch_fs] roots
in config.toml when omitted. Use 'maz watch-fs uninstall' to remove it.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if runtime.GOOS != darwinGOOS {
				return fmt.Errorf("maz watch-fs install is macOS-only (current: %s); run `maz watch-fs` in the foreground instead", runtime.GOOS)
			}
			dataDir, err := resolveDataDir(cmd)
			if err != nil {
				return err
			}
			roots, rerr := resolveInstallRoots(args, dataDir)
			if rerr != nil {
				return rerr
			}
			if ierr := daemon.InstallWatchFSLaunchAgent(cmd.Context(),
				&daemon.WatchFSLaunchAgentOptions{DataDir: dataDir, Roots: roots}); ierr != nil {
				return ierr
			}
			plistPath, perr := daemon.WatchFSLaunchAgentPlistPath()
			if perr != nil {
				return perr
			}
			return renderWatchFSInstall(cmd, plistPath, roots)
		},
	}
}

// resolveInstallRoots picks the roots for the launchd agent: positional
// args win, else the [watch_fs] config roots. At least one is required.
func resolveInstallRoots(args []string, dataDir string) ([]string, error) {
	roots := args
	if len(roots) == 0 {
		if cfg, err := config.LoadUserConfig(dataDir); err == nil {
			roots = cfg.WatchFS.Roots
		}
	}
	if len(roots) == 0 {
		return nil, errors.New("watch-fs install: no directories; pass paths or set [watch_fs] roots in config.toml")
	}
	// Validate before the plist is written: a missing root would make the
	// launchd-supervised watcher exit on every launch and KeepAlive-loop.
	if err := validateWatchFSRoots(roots); err != nil {
		return nil, err
	}
	return roots, nil
}

func newWatchFSUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the watch-fs launchd agent (stop + disable login relaunch)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != darwinGOOS {
				return fmt.Errorf("maz watch-fs uninstall is macOS-only (current: %s)", runtime.GOOS)
			}
			if err := daemon.UninstallWatchFSLaunchAgent(cmd.Context()); err != nil {
				return err
			}
			return renderWatchFSUninstall(cmd)
		},
	}
}

func newWatchFSStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the filesystem watcher is running",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, err := resolveDataDir(cmd)
			if err != nil {
				return err
			}
			return renderWatchFSStatus(cmd, dataDir)
		},
	}
}
