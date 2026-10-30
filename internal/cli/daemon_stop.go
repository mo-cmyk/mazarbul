package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// stopSoftGrace and stopKillGrace bound the parent-side wait windows.
// The soft grace exceeds mcp.DefaultShutdownTimeout (10 s) by 5 s so a
// clean child drain finishes before SIGKILL. The kill grace is a 1 s
// safety pad for the OS to deliver the uncatchable signal.
const (
	stopSoftGrace = 15 * time.Second
	stopKillGrace = 1 * time.Second
)

// launchd lifecycle seams. Tests stub these to exercise the
// launchd-managed stop path without a real `launchctl`; production code
// must not reassign them.
//
//nolint:gochecknoglobals // test seams for the launchd-managed stop/status branch
var (
	launchAgentLoadedFn  = daemon.LaunchAgentLoaded
	bootoutLaunchAgentFn = daemon.BootoutLaunchAgent
)

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running Mazarbul background daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemonStop(cmd, cmd.OutOrStdout())
		},
	}
}

func runDaemonStop(cmd *cobra.Command, w io.Writer) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	pidPath := daemon.DataDirPIDPath(dataDir)

	// A launchd-managed daemon must be booted out, not SIGTERM'd: its
	// KeepAlive would relaunch the process the signal just killed. Boot
	// it out of the user domain (keeping the plist so it returns at the
	// next login); `maz daemon uninstall` removes persistence entirely.
	// A launchctl probe error must NOT fall through to the SIGTERM path:
	// if the daemon is in fact launchd-managed, KeepAlive would undo the
	// signal, so surface the error and let the user retry.
	loaded, lerr := launchAgentLoadedFn(cmd.Context())
	if lerr != nil {
		return fmt.Errorf("daemon: check launchd ownership (daemon may still be running): %w", lerr)
	}
	if loaded {
		if berr := bootoutLaunchAgentFn(cmd.Context()); berr != nil {
			return berr
		}
		// The daemon removes its own PID file on graceful exit; clean up
		// best-effort in case the bootout SIGTERM did not drain in time,
		// so a later `status` does not report a stale entry.
		_ = daemon.DeletePID(pidPath) //nolint:errcheck // best-effort; bootout already stopped the daemon
		return renderStopLaunchd(cmd, w)
	}

	info, err := daemon.ReadPID(pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return renderStopNotRunning(cmd, w)
		}
		return fmt.Errorf("daemon: read pid: %w", err)
	}
	if !info.Running {
		if derr := daemon.DeletePID(pidPath); derr != nil {
			return derr
		}
		return renderStopStale(cmd, w, info.PID)
	}
	// Stop must NEVER signal a PID that is alive but not our daemon.
	// A stale PID file whose PID has been recycled by an unrelated
	// user process would otherwise let `maz daemon stop` SIGTERM (and
	// then SIGKILL) someone's editor or shell. Confirm identity via
	// IsMazDaemon before any signal goes out; if the live PID is not
	// ours, treat the file as stale and clean it up.
	if !daemon.IsMazDaemon(info.PID, info.Token) {
		if derr := daemon.DeletePID(pidPath); derr != nil {
			return derr
		}
		return renderStopStale(cmd, w, info.PID)
	}

	sig, err := daemon.TerminateGracefully(info.PID, stopSoftGrace, stopKillGrace)
	if err != nil {
		return err
	}
	if derr := daemon.DeletePID(pidPath); derr != nil {
		return derr
	}
	return renderStopOK(cmd, w, info.PID, sig)
}

func renderStopOK(cmd *cobra.Command, w io.Writer, pid int, sig string) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"status": "stopped",
			"pid":    pid,
			"signal": sig,
		})
	}
	_, err := fmt.Fprintf(w, "ok daemon stopped (pid %d)\n", pid)
	return err
}

func renderStopLaunchd(cmd *cobra.Command, w io.Writer) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"status":    "stopped",
			"managedBy": "launchd",
			"hint":      "restarts at next login; run `maz daemon uninstall` to disable",
		})
	}
	_, err := fmt.Fprintln(w, "ok daemon stopped (launchd agent booted out; returns at next login — `maz daemon uninstall` to disable)")
	return err
}

func renderStopStale(cmd *cobra.Command, w io.Writer, pid int) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{
			"status": "stale",
			"pid":    pid,
		})
	}
	_, err := fmt.Fprintf(w, "removed stale pid file (pid %d)\n", pid)
	return err
}

func renderStopNotRunning(cmd *cobra.Command, w io.Writer) error {
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return json.NewEncoder(w).Encode(map[string]any{"status": "not_running"})
	}
	_, err := fmt.Fprintln(w, "not running")
	return err
}
