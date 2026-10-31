//go:build !darwin

package daemon

import (
	"context"
	"errors"
)

// ErrLaunchdMacOnly is returned by every launchd entry point on a
// non-macOS build. Callers (the `maz ollama install/uninstall/status`
// cobra commands) check for it and produce a friendly "this command
// only runs on macOS" message rather than a panic.
var ErrLaunchdMacOnly = errors.New("daemon: launchd lifecycle is macOS-only")

// OllamaLaunchAgentPlistPath errors on non-Darwin platforms.
func OllamaLaunchAgentPlistPath() (string, error) { return "", ErrLaunchdMacOnly }

// RenderOllamaLaunchAgentPlist errors on non-Darwin platforms.
func RenderOllamaLaunchAgentPlist(_ OllamaLaunchAgentOptions) ([]byte, error) {
	return nil, ErrLaunchdMacOnly
}

// InstallOllamaLaunchAgent errors on non-Darwin platforms.
func InstallOllamaLaunchAgent(_ context.Context, _ string, _ OllamaLaunchAgentOptions) error {
	return ErrLaunchdMacOnly
}

// UninstallOllamaLaunchAgent errors on non-Darwin platforms.
func UninstallOllamaLaunchAgent(_ context.Context) error { return ErrLaunchdMacOnly }

// OllamaLaunchAgentLoaded errors on non-Darwin platforms.
func OllamaLaunchAgentLoaded(_ context.Context) (bool, error) { return false, ErrLaunchdMacOnly }

// LaunchAgentPlistPath errors on non-Darwin platforms.
func LaunchAgentPlistPath() (string, error) { return "", ErrLaunchdMacOnly }

// RenderLaunchAgentPlist errors on non-Darwin platforms.
func RenderLaunchAgentPlist(_ *LaunchAgentOptions) ([]byte, error) { return nil, ErrLaunchdMacOnly }

// InstallLaunchAgent errors on non-Darwin platforms. The daemon falls
// back to a one-shot SpawnChild there; login-persistence is macOS-only.
func InstallLaunchAgent(_ context.Context, _ *LaunchAgentOptions) error { return ErrLaunchdMacOnly }

// UninstallLaunchAgent errors on non-Darwin platforms.
func UninstallLaunchAgent(_ context.Context) error { return ErrLaunchdMacOnly }

// BootoutLaunchAgent errors on non-Darwin platforms. The stop path never
// reaches it there: LaunchAgentLoaded reports false, so `daemon stop`
// stays on the PID-file path.
func BootoutLaunchAgent(_ context.Context) error { return ErrLaunchdMacOnly }

// LaunchAgentLoaded reports not-loaded on non-Darwin platforms (no
// launchd), so callers treat the daemon as PID-file-managed only.
func LaunchAgentLoaded(_ context.Context) (bool, error) { return false, nil }

// LaunchAgentJobPID reports no launchd job on non-Darwin platforms. The
// readiness probe treats (_, loaded=false, _) as "no job to cross-check"
// and falls back to the bare /healthz proof.
func LaunchAgentJobPID(_ context.Context) (int, bool, error) { return 0, false, nil }

// ErrLaunchAgentNotInstalled is returned by ReregisterLaunchAgent when no
// daemon plist exists to re-register. Never returned on non-Darwin
// builds (ReregisterLaunchAgent returns ErrLaunchdMacOnly there).
var ErrLaunchAgentNotInstalled = errors.New("daemon: launchd agent not installed")

// ReadLaunchAgentState reports nothing installed on non-Darwin platforms.
func ReadLaunchAgentState(_ context.Context) (LaunchAgentState, error) {
	return LaunchAgentState{}, nil
}

// ReregisterLaunchAgent errors on non-Darwin platforms.
func ReregisterLaunchAgent(_ context.Context) error { return ErrLaunchdMacOnly }

// WatchFSLaunchAgentPlistPath errors on non-Darwin platforms.
func WatchFSLaunchAgentPlistPath() (string, error) { return "", ErrLaunchdMacOnly }

// RenderWatchFSLaunchAgentPlist errors on non-Darwin platforms.
func RenderWatchFSLaunchAgentPlist(_ *WatchFSLaunchAgentOptions) ([]byte, error) {
	return nil, ErrLaunchdMacOnly
}

// InstallWatchFSLaunchAgent errors on non-Darwin platforms. Run
// `maz watch-fs` in the foreground (e.g. under systemd) there;
// login-persistence is macOS-only.
func InstallWatchFSLaunchAgent(_ context.Context, _ *WatchFSLaunchAgentOptions) error {
	return ErrLaunchdMacOnly
}

// UninstallWatchFSLaunchAgent errors on non-Darwin platforms.
func UninstallWatchFSLaunchAgent(_ context.Context) error { return ErrLaunchdMacOnly }

// WatchFSLaunchAgentLoaded reports not-loaded on non-Darwin platforms (no
// launchd), so callers treat the watcher as foreground/PID-file-only.
func WatchFSLaunchAgentLoaded(_ context.Context) (bool, error) { return false, nil }

// WatchFSLaunchAgentJobPID reports no launchd job on non-Darwin platforms.
func WatchFSLaunchAgentJobPID(_ context.Context) (bool, int, error) { return false, 0, nil }
