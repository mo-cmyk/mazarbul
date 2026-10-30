package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/version"
)

// mcpServeUnixSentinel is the placeholder pflag stores for a bare
// `--unix` flag. RunE rewrites it to <data-dir>/mazarbul.sock once the
// root --data-dir flag is parsed, so the runtime data-dir wins over
// the build-time config.Defaults() snapshot. The angle-bracket form
// reads as a placeholder in `--help` output and cannot collide with a
// real socket path (filesystem rejects '<' on macOS in this position,
// and even if a path were equal, the caller's pass-through branch
// would still accept it as explicit input).
const mcpServeUnixSentinel = "<default>"

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run and inspect the Mazarbul MCP server",
		Long: `Foreground commands for the Mazarbul Model Context Protocol server.

The daemon supervisor (see "maz daemon", issue #42) wires the same server
into a long-running background process. The "mcp serve" subcommand is the
foreground sibling: it is the one Claude Desktop, mcp-inspector, and other
parent-process flows spawn as a subprocess.`,
	}
	cmd.AddCommand(newMCPServeCmd())
	return cmd
}

func newMCPServeCmd() *cobra.Command {
	var (
		httpAddr string
		unixPath string
		noStdio  bool
		name     string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server in the foreground",
		Long: `Run the MCP server in the foreground. Defaults to stdio only.

Pass --http (or --http=<addr>) to also expose the streamable HTTP transport.
Pass --unix (or --unix=<path>) to also expose the Unix socket transport.
Pass --no-stdio to disable stdio when running HTTP or Unix in isolation for
debug.

NOTE: --http and --unix take an OPTIONAL value, so a custom path/addr must be
joined with "=" (--unix=/tmp/x.sock). Space-separated (--unix /tmp/x.sock) does
NOT work: the bare flag uses its default and /tmp/x.sock becomes a stray
argument — which is now rejected rather than silently ignored.`,
		// serve takes no positional arguments. Without this, a space-separated
		// optional-value flag (e.g. `--unix /path`, which pflag does NOT bind —
		// it uses the default and leaves /path positional) would be silently
		// dropped, so the server bound the default socket and the user's explicit
		// path vanished. NoArgs surfaces that mistake instead of hiding it.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMCPServe(cmd, mcpServeOpts{
				httpAddr: httpAddr,
				unixPath: resolveUnixSocket(cmd, unixPath),
				stdio:    !noStdio,
				name:     name,
			})
		},
	}
	cmd.Flags().StringVar(&httpAddr, "http", "", "enable streamable HTTP at addr (bare flag uses default 127.0.0.1:7842)")
	cmd.Flag("http").NoOptDefVal = mcp.DefaultHTTPAddr
	cmd.Flags().StringVar(&unixPath, "unix", "", "enable Unix socket at path (bare flag uses <data-dir>/mazarbul.sock)")
	cmd.Flag("unix").NoOptDefVal = mcpServeUnixSentinel
	cmd.Flags().BoolVar(&noStdio, "no-stdio", false, "disable stdio (default: stdio enabled)")
	cmd.Flags().StringVar(&name, "name", "mazarbul", "MCP server name advertised to clients")
	return cmd
}

// resolveUnixSocket maps a raw --unix flag value to the concrete socket
// path passed to mcp.New. Empty means transport disabled. The sentinel
// (bare `--unix` flag) resolves against the persistent --data-dir flag
// on the root command, falling back to config.Defaults().DataDir when
// the flag is unset. Any other value is treated as an explicit
// caller-provided path and passed through verbatim.
func resolveUnixSocket(cmd *cobra.Command, raw string) string {
	switch raw {
	case "":
		return ""
	case mcpServeUnixSentinel:
		dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
		if dataDir == "" {
			dataDir = config.Defaults().DataDir
		}
		return mcp.DefaultUnixSocket(dataDir)
	default:
		return raw
	}
}

type mcpServeOpts struct {
	httpAddr string
	unixPath string
	stdio    bool
	name     string
}

// mcpServeMode classifies how `maz mcp serve` should run. The default
// is standalone (the long-standing path: OpenWiring + serve tools
// directly). Proxy mode is taken when a Mazarbul daemon is detected:
// the daemon holds the LadybugDB lock so a second OpenWiring would
// fail with "ladybug status 1" (the bug Claude Desktop hit when the
// user had `maz daemon start` running). In proxy mode we bridge the
// stdio MCP protocol to the daemon's existing HTTP MCP endpoint so a
// Claude-Desktop-spawned `maz mcp serve` Just Works alongside the
// background daemon.
type mcpServeMode string

const (
	mcpServeModeStandalone mcpServeMode = "standalone"
	mcpServeModeProxy      mcpServeMode = "proxy"
)

// pickMCPServeMode chooses the run mode for `maz mcp serve` by
// consulting preflightDaemon. Returns the daemon's Unix-socket path when
// proxy mode is selected — the default transport now that the TCP
// listener is off by default; the socket's 0600 permissions are the
// credential, so the proxy bridges to it without a token. Tests stub
// preflightDaemon to drive both branches.
func pickMCPServeMode(dataDir string) (mcpServeMode, string) {
	socket := mcp.DefaultUnixSocket(dataDir)
	// Proxy when a daemon is detected via its pidfile, OR when the data dir's
	// socket is already being served — the latter catches another `maz mcp serve`
	// (e.g. a second editor) that holds the lock and binds the socket but writes
	// no daemon pidfile. Bridging to it keeps the data dir a singleton instead of
	// the second serve dying on the lock.
	if err := preflightDaemon("", dataDir); err != nil {
		return mcpServeModeProxy, socket
	}
	if probeDaemonListening(dataDir) {
		return mcpServeModeProxy, socket
	}
	return mcpServeModeStandalone, ""
}

// mintServeHTTPToken mints the bearer token the hardened TCP transport
// requires for a standalone `maz mcp serve --http`, returning the token and
// a cleanup func that removes the 0600 token file. When httpAddr is empty
// (stdio/--unix only) it returns an empty token and a no-op cleanup.
func mintServeHTTPToken(dataDir, httpAddr string, logger *zap.Logger) (string, func(), error) {
	if httpAddr == "" {
		return "", func() {}, nil
	}
	tok, err := daemon.MintAndWriteHTTPToken(dataDir)
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() {
		if rerr := daemon.RemoveHTTPToken(dataDir); rerr != nil {
			logger.Warn("mcp serve: remove HTTP token", zap.Error(rerr))
		}
	}
	return tok, cleanup, nil
}

func runMCPServe(cmd *cobra.Command, opts mcpServeOpts) error {
	if !opts.stdio && opts.httpAddr == "" && opts.unixPath == "" {
		return errors.New("--no-stdio requires at least one of --http or --unix")
	}

	logger := logging.FromContext(cmd.Context())

	// Route around the LadybugDB single-writer lock when a Mazarbul
	// daemon is already running. Without this, Claude Desktop's
	// `maz mcp serve` child process races the background daemon for
	// the on-disk lock and dies with "ladybug status 1".
	//
	// Exception: when the caller explicitly passes --data-dir they want
	// an isolated kernel against that directory, NOT a proxy to whatever
	// daemon happens to hold the default data dir. The LongMemEval eval
	// harness depends on this: each question spawns
	// `maz mcp serve --data-dir /tmp/maz-eval-XYZ` and would otherwise
	// route through the user's running daemon (wrong data, contention,
	// timeouts). Detection via Changed() distinguishes "user passed
	// --data-dir XYZ" from "left it empty / unset".
	dataDirOverride := cmd.Root().PersistentFlags().Changed("data-dir")
	if !dataDirOverride {
		dataDir := config.Defaults().DataDir
		if mode, socketPath := pickMCPServeMode(dataDir); mode == mcpServeModeProxy {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runMCPServeProxy(ctx, opts, socketPath, logger)
		}
		// pickMCPServeMode returned standalone: no running daemon was detected.
		// A launchd-managed daemon may still be completing its boot sequence —
		// the common race on macOS where Claude Desktop spawns `maz mcp serve`
		// before launchd's child has bound the Unix socket. Grabbing the graph
		// lock in-process during that window causes the daemon to crash-loop
		// with "ladybug status 1" until this process exits.
		//
		// When a daemon agent is configured (plist on disk), wait up to
		// mcpServeSocketWaitTimeout for the socket to appear. If it does,
		// proxy to the daemon instead of opening the DB ourselves. If the
		// deadline passes (daemon genuinely absent), fall through to standalone
		// with a clear warning so the log explains any subsequent lock conflict.
		if daemonConfiguredFn(dataDir) {
			socket := mcp.DefaultUnixSocket(dataDir)
			logger.Info("mcp serve: daemon agent is configured; waiting for socket before going standalone",
				zap.String("socket", socket),
				zap.Duration("timeout", mcpServeSocketWaitTimeout))
			if waitForDaemonSocketFn(dataDir, mcpServeSocketWaitTimeout) {
				ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
				defer stop()
				return runMCPServeProxy(ctx, opts, socket, logger)
			}
			logger.Warn("mcp serve: daemon socket did not appear within timeout; starting standalone (holds graph lock until this process exits)",
				zap.String("socket", socket),
				zap.Duration("timeout", mcpServeSocketWaitTimeout),
				zap.String("hint", "if a daemon is also starting it will fail with 'status 1'; quit the MCP host (e.g. Claude Desktop) that spawned this serve to release the lock"))
		}
	}

	return serveStandalone(cmd, opts, logger)
}

// serveStandalone opens an in-process kernel and serves the MCP tool
// surface directly, the path taken when no daemon holds the data dir.
// Split from runMCPServe to keep each function within the complexity
// budget.
func serveStandalone(cmd *cobra.Command, opts mcpServeOpts, logger *zap.Logger) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	// Bind the data dir's unix socket by default so this serve is a singleton
	// that other `maz` commands cooperate with instead of fighting for the lock.
	// An editor/agent-spawned `maz mcp serve` (stdio only) used to hold the
	// LadybugDB lock while binding nothing, so `maz remember`/`maz init` from a
	// terminal had no transport to reach it and died with "status 1". With the
	// socket bound, runToolViaDaemonOrInProcess routes those calls here. Explicit
	// --unix still wins; the server clears any stale socket file before binding.
	if opts.unixPath == "" {
		opts.unixPath = mcp.DefaultUnixSocket(dataDir)
		logger.Info("mcp serve: binding data-dir socket so CLI commands can reach this server",
			zap.String("socket", opts.unixPath))
	}
	wired, err := daemon.OpenWiring(cmd.Context(), daemon.WiringConfig{DataDir: dataDir, Logger: logger})
	if err != nil {
		// Surface an actionable message when the data dir is locked by a
		// running daemon or another `mcp serve`. The LadybugDB C library
		// reports contention as the opaque "failed to open database with
		// status 1", which is unhelpful on its own.
		if isDataDirLocked(err) {
			return fmt.Errorf("mcp serve: data dir %s is locked, likely by a running daemon or another `mcp serve`; stop it first with `maz daemon stop`: %w", dataDir, err)
		}
		return fmt.Errorf("mcp serve: %w", err)
	}
	defer func() {
		if cerr := wired.Close(cmd.Context()); cerr != nil {
			logger.Warn("mcp serve: close wiring", zap.Error(cerr))
		}
	}()

	// The standalone --http transport is an explicit TCP opt-in, so it is
	// hardened exactly like the daemon's: mint a 0600 bearer token and
	// remove it on exit. Stdio/--unix need no token.
	httpToken, cleanupToken, terr := mintServeHTTPToken(dataDir, opts.httpAddr, logger)
	if terr != nil {
		return fmt.Errorf("mcp serve: %w", terr)
	}
	defer cleanupToken()

	srv, err := mcp.New(mcp.Options{
		Name:       opts.name,
		Version:    version.Version,
		Logger:     logger,
		Stdio:      opts.stdio,
		HTTPAddr:   opts.httpAddr,
		Token:      httpToken,
		UnixSocket: opts.unixPath,
		Secrets:    wired.Secrets,
		// Channel A (#214): same per-session instructions composition as the
		// supervisor daemon path. This is the standalone path Claude Code uses
		// over stdio, so it must carry the active-preset section too. Nil-safe.
		InstructionComposer: wired.InstructionComposer,
	})
	if err != nil {
		return fmt.Errorf("mcp new: %w", err)
	}

	for _, t := range wired.Tools {
		if rerr := srv.Register(t); rerr != nil {
			return fmt.Errorf("mcp serve: register %q: %w", t.Name(), rerr)
		}
	}

	// Register MCP resources (hot brief, vault index, pinned docs, documents
	// template) on the standalone serve path too, mirroring the supervisor
	// daemon path (wireAndBuildServer). Without this, `maz mcp serve` advertises
	// the resources capability but exposes an empty resource set.
	registerServeResources(srv, wired.ResourceAssembler, wired.ResourceDocReader)

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mcp serve: %w", err)
	}
	return nil
}

// isDataDirLocked reports whether err indicates that the data directory's
// exclusive on-disk lock is held by another process. The LadybugDB C library
// reports lock contention as the opaque "failed to open database with status 1"
// buried inside a wiring error chain; we surface this as an actionable hint
// rather than the raw status code.
func isDataDirLocked(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status 1") || strings.Contains(msg, "failed to open database")
}

// mcpServeSocketWaitTimeout caps how long runMCPServe waits for a configured
// launchd daemon to bind its Unix socket before falling back to standalone
// mode. Chosen to outlast a cold-boot (sqlite-vec open + Ollama probe) while
// keeping the Claude Desktop startup stall bounded.
const mcpServeSocketWaitTimeout = 15 * time.Second

// daemonConfiguredFn reports whether a Mazarbul launchd agent is installed
// for dataDir. A true result means a daemon is expected to manage this data
// dir and will bind the Unix socket shortly after login; runMCPServe uses it
// to detect the boot-race window and wait rather than going standalone.
//
//nolint:gochecknoglobals // test seam: tests stub this to avoid touching real launchd state
var daemonConfiguredFn = func(dataDir string) bool {
	// Only the default data dir gets a launchd agent; an explicit --data-dir
	// (eval harness, isolated test) is never daemon-managed.
	if !isDefaultDataDir(dataDir) {
		return false
	}
	path, err := daemon.LaunchAgentPlistPath()
	if err != nil {
		// Non-Darwin platform or $HOME unavailable — no launchd agent.
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// waitForDaemonSocketFn polls until the daemon's Unix socket for dataDir
// accepts connections or the timeout elapses. Returns true on the first
// successful dial. Production code calls probeDaemonListening so that tests
// can stub both vars independently.
//
//nolint:gochecknoglobals // test seam: tests stub this to avoid real waits
var waitForDaemonSocketFn = func(dataDir string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if probeDaemonListening(dataDir) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// registerServeResources installs the MCP context + document resources on a
// server that supports them, mirroring the supervisor daemon path. It is a
// no-op (returning false) when srv does not implement ResourceRegistrar — e.g.
// a transport build that omits resources or the test NoopServer — so callers
// never need to type-assert at the call site. Returns whether registration
// happened, which the test uses to lock the standalone serve wiring in place.
func registerServeResources(srv mcp.Server, assembler mcp.ContextAssembler, nav mcp.DocumentReader) bool {
	rr, ok := srv.(mcp.ResourceRegistrar)
	if !ok {
		return false
	}
	rr.RegisterContextResources(assembler, nav)
	return true
}
