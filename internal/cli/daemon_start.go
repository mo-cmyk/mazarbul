package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/version"
)

// internalDaemonServeCmd is the hidden subcommand the parent re-execs
// to enter the child-process body. It must remain hidden from --help so
// users do not invoke it directly; the wiring lives on the root maz
// command (not under daemon) so the cobra invocation is `maz
// internal-daemon-serve` regardless of which daemon subcommand spawned it.
//
// The --token flag carries the per-launch ownership secret the parent
// minted (16 bytes of crypto/rand, hex-encoded). The child writes the
// token into the PID file and exposes it via argv; `daemon stop` and
// `daemon status` read the PID file and verify the live process's
// `ps -o args=` output ends with " --token <hex>" before signalling.
// Greptile P1 #107 round 5: an unforgeable per-launch token is the only
// way to defeat false-positive ownership matches when an unrelated `maz`
// process's argv happens to contain the daemon subcommand pair.
func newInternalDaemonServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "internal-daemon-serve",
		Hidden: true,
		Short:  "(internal) child entrypoint for the daemon supervisor",
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, _ := cmd.Flags().GetString("token")
			httpAddr, _ := cmd.Flags().GetString("http-addr")
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			if dataDir == "" {
				dataDir = config.Defaults().DataDir
			}
			return runInternalDaemonServe(cmd.Context(), token, dataDir, httpAddr)
		},
	}
	cmd.Flags().String("token", "", "per-launch ownership token minted by the parent supervisor (required)")
	cmd.Flags().String("http-addr", "", "opt-in TCP address for the MCP HTTP transport (default: disabled — daemon binds only the Unix socket; falls back to MAZ_HTTP_ADDR, e.g. 127.0.0.1:7842)")
	return cmd
}

func runInternalDaemonServe(ctx context.Context, token, dataDir, httpAddr string) error {
	// The boot tracer is the FIRST thing the child does. Every later
	// step — including logger init, which the daemon.log emptiness
	// suggests has failed silently in past crashes — gets a phase line
	// on disk before it runs. NewBootTracer failures must NOT block
	// boot: they leave tracer=nil and BootTracer.Mark becomes a no-op.
	tracer, terr := daemon.NewBootTracer(dataDir)
	if terr != nil {
		fmt.Fprintf(os.Stderr, "daemon: boot tracer unavailable: %v\n", terr)
	}
	defer func() {
		if cerr := tracer.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "daemon: close boot tracer: %v\n", cerr)
		}
	}()

	tracer.Mark(daemon.PhaseEntry)
	tracer.Mark(daemon.PhaseLoggerInit)
	logger, err := logging.NewLogger(logging.Config{
		Level:    logging.LevelInfo,
		Format:   logging.FormatJSON,
		FilePath: config.DaemonLogFile(),
	})
	if err != nil {
		return fmt.Errorf("daemon: build logger: %w", err)
	}
	defer func() {
		// zap's Sync errors on /dev/stderr with "bad file descriptor"
		// on Darwin (zap issue #328); the daemon process writes to a
		// file sink anyway, so a Sync error here means a real flush
		// failure and we surface it via stderr.
		if err := logger.Sync(); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: logger sync: %v\n", err)
		}
	}()

	if httpAddr == "" {
		httpAddr = daemon.DefaultHTTPAddr()
	}
	opts := &daemon.SupervisorOptions{
		PIDPath:    daemon.DataDirPIDPath(dataDir),
		HTTPAddr:   httpAddr,
		UnixSocket: daemonSocketPath(dataDir),
		Name:       "mazarbul",
		Version:    version.Version,
		Logger:     logger,
		Token:      token,
		DataDir:    dataDir,
		BootTracer: tracer,
	}
	return daemon.RunSupervisor(ctx, opts)
}

// daemonSocketPath is the canonical Unix-socket path the daemon binds
// for a given dataDir. It is intentionally a thin wrapper over
// mcp.DefaultUnixSocket so the daemon writer and the doctor/CLI readers
// share one source of truth. Pre-fix, the writer used
// daemon.DefaultSocketPath() ($TMPDIR/mazarbul.sock) while doctor read
// mcp.DefaultUnixSocket (<dataDir>/mazarbul.sock); the resulting
// disagreement made a healthy daemon look broken to the doctor probe.
func daemonSocketPath(dataDir string) string {
	return mcp.DefaultUnixSocket(dataDir)
}

// mintDaemonToken returns 16 bytes of crypto/rand hex-encoded. Used by
// the parent to mint a per-launch ownership secret that the child
// carries in argv and records in the PID file.
func mintDaemonToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("daemon: mint token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func newDaemonStartCmd() *cobra.Command {
	var httpAddr string
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the Mazarbul background daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, err := resolveDataDir(cmd)
			if err != nil {
				return err
			}
			return runDaemonStart(cmd, cmd.OutOrStdout(), dataDir, httpAddr)
		},
	}
	cmd.Flags().StringVar(&httpAddr, "http-addr", "", "opt-in TCP address for the MCP HTTP transport (default: disabled — daemon binds only the Unix socket; falls back to MAZ_HTTP_ADDR, e.g. 127.0.0.1:7842)")
	return cmd
}

// buildDaemonChildArgs assembles the argv the parent process re-execs into
// the child. The child re-parses these via cobra: `--token` is required for
// the PID ownership probe, `--data-dir` (a persistent root flag) is forwarded
// so the wiring opens ${dataDir}/index.db rather than the build-time default,
// and `--http-addr` carries the resolved TCP listen address so an isolated
// instance can bind a non-default port. Empty dataDir or httpAddr omits the
// respective flag so the child falls through to its own defaults.
func buildDaemonChildArgs(token, dataDir, httpAddr string) []string {
	args := []string{"internal-daemon-serve", "--token", token}
	if httpAddr != "" {
		args = append(args, "--http-addr", httpAddr)
	}
	if dataDir != "" {
		// --data-dir is a persistent root flag, so the child must see
		// it BEFORE the subcommand name for cobra's flag parser to
		// attach it to the root command.
		args = append([]string{"--data-dir", dataDir}, args...)
	}
	return args
}

// spawnDaemonOnce forks a single detached daemon process for dataDir and
// waits for it to become ready (PID file + TCP probe). It is the
// one-shot, non-persistent path: no launchd, no relaunch-on-login. The
// raw error is returned unwrapped so callers can branch on
// daemon.ErrAlreadyRunning. Shared by `maz daemon start` and the turnkey
// `maz init` daemon step.
//
// The httpAddrOverride selects the TCP listen address for the daemon's MCP
// HTTP transport; empty resolves to daemon.DefaultHTTPAddr() (which reads
// MAZ_HTTP_ADDR).
func spawnDaemonOnce(ctx context.Context, dataDir, httpAddrOverride string) (*daemon.SpawnResult, error) {
	rawAddr := httpAddrOverride
	if rawAddr == "" {
		rawAddr = daemon.DefaultHTTPAddr()
	}
	httpAddr, err := daemon.ValidateHTTPAddr(rawAddr)
	if err != nil {
		return nil, err
	}
	exe, err := daemon.ResolveExecutable()
	if err != nil {
		return nil, err
	}
	token, err := mintDaemonToken()
	if err != nil {
		return nil, err
	}
	pidPath := daemon.DataDirPIDPath(dataDir)
	cfg := &daemon.SpawnConfig{
		Executable: exe,
		// Token travels on argv so `ps -o args=` exposes it for the
		// ownership probe in `daemon stop` / `daemon status`. The
		// child re-parses it via cobra flags and writes it into the
		// PID file; both surfaces must agree, so the parent is the
		// single source of truth. --data-dir is forwarded when the
		// user passed it on the parent shell so the daemon opens the
		// same kernel store the user named.
		Args:      buildDaemonChildArgs(token, dataDir, httpAddr),
		Env:       os.Environ(),
		PIDPath:   pidPath,
		ProbeAddr: httpAddr,
		// Readiness falls back to the Unix socket when TCP is off (the
		// default posture); SpawnChild prefers ProbeAddr when it is set.
		ProbeSocket: daemonSocketPath(dataDir),
		LogPath:     config.DaemonLogFile(),
	}
	return daemon.SpawnChild(ctx, cfg)
}

func runDaemonStart(cmd *cobra.Command, w io.Writer, dataDir, httpAddrOverride string) error {
	res, err := spawnDaemonOnce(cmd.Context(), dataDir, httpAddrOverride)
	if err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			if rerr := renderAlreadyRunning(cmd, w, daemon.DataDirPIDPath(dataDir)); rerr != nil {
				return rerr
			}
			return silentError{code: 1, err: err}
		}
		return err
	}

	httpAddr := httpAddrOverride
	if httpAddr == "" {
		httpAddr = daemon.DefaultHTTPAddr()
	}
	return renderStartOK(cmd, w, res, httpAddr, daemonSocketPath(dataDir))
}

func renderStartOK(cmd *cobra.Command, w io.Writer, res *daemon.SpawnResult, httpAddr, socket string) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"status":   "running",
			"pid":      res.PID,
			"logFile":  res.LogPath,
			"httpAddr": httpAddr,
			"socket":   socket,
		})
	}
	_, err := fmt.Fprintf(w, "ok daemon running (pid %d); logs at %s\n", res.PID, res.LogPath)
	return err
}

func renderAlreadyRunning(cmd *cobra.Command, w io.Writer, pidPath string) error {
	info, err := daemon.ReadPID(pidPath)
	if err != nil {
		return fmt.Errorf("daemon: read pid: %w", err)
	}
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"status": "already_running",
			"pid":    info.PID,
		}); err != nil {
			return fmt.Errorf("daemon: encode already-running: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(w, "daemon already running (pid %d)\n", info.PID); err != nil {
		return fmt.Errorf("daemon: write already-running: %w", err)
	}
	return nil
}
