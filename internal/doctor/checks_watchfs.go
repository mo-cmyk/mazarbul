package doctor

import (
	"context"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// newWatchFSCheck surfaces the agent-agnostic filesystem watcher's
// liveness. The watcher is OPTIONAL passive capture, so "not running" is
// a healthy OK, not a failure; only a stale pidfile or a loaded-but-dead
// launchd job warrants a warning. On macOS the launchd agent is
// authoritative (the persistent, managed instance); elsewhere the
// per-data-dir pidfile is the only signal.
func newWatchFSCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "watch_fs.status",
		Sub: subsystemMemory,
		Fn: func(ctx context.Context) (Status, string, error) {
			if status, detail, ok := watchFSLaunchdStatus(ctx); ok {
				return status, detail, nil
			}
			return watchFSPidfileStatus(dataDir)
		},
	}
}

// watchFSLaunchdStatus reports the launchd job state on macOS. The ok
// result is false when no launchd job is loaded (caller falls back to the
// pidfile), including on every non-macOS platform where the stub reports
// not-loaded.
func watchFSLaunchdStatus(ctx context.Context) (status Status, detail string, ok bool) {
	loaded, pid, err := daemon.WatchFSLaunchAgentJobPID(ctx)
	if err != nil || !loaded {
		return StatusOK, "", false
	}
	if pid > 0 {
		return StatusOK, fmt.Sprintf("running under launchd (pid %d)", pid), true
	}
	return StatusWarn, "launchd agent loaded but watcher not currently running", true
}

// watchFSPidfileStatus reports liveness from the watcher's pidfile.
func watchFSPidfileStatus(dataDir string) (Status, string, error) {
	info, err := daemon.ReadPID(daemon.WatchFSPIDPath(dataDir))
	switch {
	case err != nil:
		// A missing/unreadable pidfile means the watcher simply is not
		// running. Passive capture is optional, so this is healthy, not a
		// check failure — the read error is deliberately not propagated.
		//nolint:nilerr // absent pidfile == watcher off == healthy
		return StatusOK, "not running (optional passive capture; start with `maz watch-fs`)", nil
	case info.Running:
		return StatusOK, fmt.Sprintf("running in foreground (pid %d)", info.PID), nil
	default:
		return StatusWarn, fmt.Sprintf("stale pidfile for dead pid %d; clear with `maz watch-fs` or remove watch-fs.pid", info.PID), nil
	}
}
