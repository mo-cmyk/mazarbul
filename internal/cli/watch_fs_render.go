package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// watchFSStatus is the resolved liveness view rendered by `maz watch-fs
// status` and consumed (in spirit) by the doctor check.
type watchFSStatus struct {
	// Running reports whether a watcher process is currently alive.
	Running bool `json:"running"`
	// PID is the watcher process id when known (0 otherwise).
	PID int `json:"pid,omitempty"`
	// LaunchdLoaded reports whether the launchd agent is loaded (macOS).
	LaunchdLoaded bool `json:"launchdLoaded"`
	// Detail is a short human-readable summary.
	Detail string `json:"detail"`
}

// resolveWatchFSStatus determines watcher liveness for dataDir. On macOS
// the launchd agent is authoritative (it is the persistent, managed
// instance); the per-data-dir pidfile is the fallback for a foreground
// watcher and the only signal on non-macOS.
func resolveWatchFSStatus(cmd *cobra.Command, dataDir string) watchFSStatus {
	if runtime.GOOS == darwinGOOS {
		if loaded, pid, err := daemon.WatchFSLaunchAgentJobPID(cmd.Context()); err == nil && loaded {
			if pid > 0 {
				return watchFSStatus{
					Running: true, PID: pid, LaunchdLoaded: true,
					Detail: fmt.Sprintf("running under launchd (pid %d)", pid),
				}
			}
			return watchFSStatus{
				Running: false, LaunchdLoaded: true,
				Detail: "launchd agent loaded; child not currently running",
			}
		}
	}
	return watchFSStatusFromPID(dataDir)
}

// watchFSStatusFromPID reads the watcher pidfile and reports liveness.
func watchFSStatusFromPID(dataDir string) watchFSStatus {
	info, err := daemon.ReadPID(daemon.WatchFSPIDPath(dataDir))
	switch {
	case err != nil:
		return watchFSStatus{Detail: "not running (no pidfile)"}
	case info.Running:
		return watchFSStatus{
			Running: true, PID: info.PID,
			Detail: fmt.Sprintf("running in foreground (pid %d)", info.PID),
		}
	default:
		return watchFSStatus{Detail: fmt.Sprintf("not running (stale pidfile for pid %d)", info.PID)}
	}
}

func renderWatchFSStatus(cmd *cobra.Command, dataDir string) error {
	st := resolveWatchFSStatus(cmd, dataDir)
	wr := output.From(cmd)
	w := cmd.OutOrStdout()
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return wr.Emit(st)
	}
	prefix := "stopped"
	if st.Running {
		prefix = "running"
	}
	_, err := fmt.Fprintf(w, "watch-fs: %s — %s\n", prefix, st.Detail)
	return err
}

func renderWatchFSInstall(cmd *cobra.Command, plistPath string, roots []string) error {
	wr := output.From(cmd)
	w := cmd.OutOrStdout()
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return wr.Emit(map[string]any{
			"action":    "installed",
			"plistPath": plistPath,
			"roots":     roots,
		})
	}
	_, err := fmt.Fprintf(w, "ok watch-fs launchd agent installed; watching %d root(s)\n", len(roots))
	return err
}

func renderWatchFSUninstall(cmd *cobra.Command) error {
	wr := output.From(cmd)
	w := cmd.OutOrStdout()
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		return wr.Emit(map[string]any{"action": "uninstalled"})
	}
	_, err := fmt.Fprintln(w, "ok watch-fs launchd agent uninstalled")
	return err
}
