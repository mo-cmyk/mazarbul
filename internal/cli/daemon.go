package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// tailPollInterval matches GNU tail's default polling cadence. Short enough
// to feel live; long enough to keep CPU near zero when idle.
const tailPollInterval = 200 * time.Millisecond

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the Mazarbul background daemon",
		Long: `Manage the Mazarbul background daemon (MCP server over HTTP + Unix socket).

Subcommands:
  start      detach a one-shot daemon process, write the PID, begin serving
  install    hand the daemon to launchd: start now + relaunch on login (macOS)
  uninstall  unload the launchd agent and remove its plist (macOS)
  stop       halt the running daemon (boots out the launchd agent if managed)
  status     report running/stale/absent with PID, uptime, and listeners
  logs       print the daemon log path; tail with --follow`,
	}
	cmd.AddCommand(newDaemonLogsCmd())
	cmd.AddCommand(newDaemonStartCmd())
	cmd.AddCommand(newDaemonInstallCmd())
	cmd.AddCommand(newDaemonUninstallCmd())
	cmd.AddCommand(newDaemonStopCmd())
	cmd.AddCommand(newDaemonStatusCmd())
	cmd.AddCommand(newDaemonBootTraceCmd())
	return cmd
}

// newDaemonBootTraceCmd prints the boot phase trace from the most
// recent (or in-flight) supervisor start. The trace's last line names
// the phase that began but did not complete — the only signal that
// survives a host freeze or hard kill where zap's buffered file logger
// drops everything.
func newDaemonBootTraceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "boot-trace",
		Short: "Print the daemon's fsync'd boot-phase trace",
		Long: `Print the boot-trace.log written by the daemon child process.

Each line records the phase the child was about to enter, fsync'd before
that phase ran. After a crash or host freeze, the final line names the
phase that hung — diagnostics zap's buffered daemon.log cannot give.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			if dataDir == "" {
				dataDir = config.Defaults().DataDir
			}
			return runDaemonBootTrace(cmd, cmd.OutOrStdout(), dataDir)
		},
	}
}

func runDaemonBootTrace(cmd *cobra.Command, w io.Writer, dataDir string) error {
	body, err := daemon.ReadBootTrace(dataDir)
	if err != nil {
		return fmt.Errorf("daemon: read boot trace: %w", err)
	}
	wr := output.From(cmd)
	if len(body) == 0 {
		if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
			return json.NewEncoder(w).Encode(map[string]any{"status": "absent"})
		}
		_, werr := fmt.Fprintln(w, "no boot trace recorded yet")
		return werr
	}
	if _, werr := w.Write(body); werr != nil {
		return fmt.Errorf("daemon: write boot trace: %w", werr)
	}
	return nil
}

func newDaemonLogsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print the daemon log file path; tail it with --follow",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := config.DaemonLogFile()
			if follow {
				return runDaemonLogsFollow(cmd.Context(), cmd.OutOrStdout(), path)
			}
			return runDaemonLogsShow(cmd, cmd.OutOrStdout(), path)
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "tail the log file (like tail -f)")
	return cmd
}

// runDaemonLogsShow prints the resolved log file path in the active output mode.
func runDaemonLogsShow(cmd *cobra.Command, w io.Writer, path string) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]string{"logFile": path})
	}
	_, err := fmt.Fprintln(w, path)
	return err
}

// runDaemonLogsFollow tails path, writing appended bytes to w until ctx is
// cancelled or the process receives SIGINT/SIGTERM. Detects log rotation by
// comparing the open file with the file currently at path (os.SameFile,
// portable across Unix and Windows).
func runDaemonLogsFollow(ctx context.Context, w io.Writer, path string) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	f, err := openLogTail(path)
	if err != nil {
		return err
	}
	// Closure form so the deferred close sees the current f after rotation
	// reassigns it, not the original file pointer.
	defer func() { f.Close() }()
	reader := bufio.NewReader(f)

	for {
		if ctx.Err() != nil {
			return nil
		}
		if derr := drainAvailable(reader, w); derr != nil {
			return derr
		}
		if !waitForChange(ctx) {
			return nil
		}
		nf, rotated, rerr := reopenIfRotated(f, path)
		if rerr != nil {
			continue
		}
		if rotated {
			f.Close()
			f = nf
			reader = bufio.NewReader(f)
		}
	}
}

// openLogTail opens path and seeks to end, ready for tail-follow reading.
func openLogTail(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("daemon log not found at %s; start the daemon first", path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, fmt.Errorf("seek: %w", err)
	}
	return f, nil
}

// drainAvailable reads all currently-buffered lines from r and writes them
// to w. Returns nil at EOF (more may be appended later), error otherwise.
func drainAvailable(r *bufio.Reader, w io.Writer) error {
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return werr
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

// waitForChange sleeps the poll interval. Returns false if ctx is cancelled
// during the wait.
func waitForChange(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(tailPollInterval):
		return true
	}
}

// reopenIfRotated returns a freshly opened file when path now refers to a
// different inode than f (lumberjack rotation). Returns (nil, false, nil)
// when no rotation has happened.
func reopenIfRotated(f *os.File, path string) (*os.File, bool, error) {
	openInfo, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	if os.SameFile(openInfo, pathInfo) {
		return nil, false, nil
	}
	nf, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("reopen %s: %w", path, err)
	}
	return nf, true, nil
}
