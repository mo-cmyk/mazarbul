//go:build !windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// sigKill is the signal-delivery seam SpawnChild uses for its
// cleanup path. Tests override this so the broadcast-prevention
// invariant ("never call kill with a non-positive PID") can be
// asserted without a process actually delivering signals.
var sigKill = syscall.Kill //nolint:gochecknoglobals // test seam for the cleanup-SIGTERM path; see TestSpawnChild_DoesNotBroadcastSIGTERMOnReadyTimeout

// SpawnConfig is what the parent-side spawnChild needs to fork the
// detached daemon process. Fields beyond Args, Env, and Probe are
// resolved by the caller (the daemon start CLI command) using the
// Default* helpers so parent and child agree on every path.
type SpawnConfig struct {
	// Executable is the absolute path of the binary to re-exec. Typically
	// derived from os.Executable(); the caller may override for tests.
	Executable string

	// Args are the arguments handed to the child, including the
	// subcommand. Example: []string{"internal-daemon-serve"}. The binary
	// path is prepended automatically.
	Args []string

	// Env is the environment for the child process. Pass os.Environ() to
	// inherit; tests override.
	Env []string

	// PIDPath is polled until it exists or ReadyTimeout elapses. Required.
	PIDPath string

	// ProbeAddr is dialed with a short TCP timeout once the PID file
	// appears. Empty disables the TCP probe (the default, TCP-off posture).
	ProbeAddr string

	// ProbeSocket is the Unix socket dialed once the PID file appears when
	// ProbeAddr is empty — the default readiness signal now that the TCP
	// transport is off by default. Empty falls back to PID-file-only
	// readiness.
	ProbeSocket string

	// ReadyTimeout bounds the total time spawnChild waits for the PID
	// file to appear AND the TCP probe to succeed. Zero defaults to 10 s.
	ReadyTimeout time.Duration

	// LogPath is the resolved daemon log path. spawnChild does not read
	// it; it is plumbed back to the caller so the CLI start command can
	// print "logs at <path>".
	LogPath string
}

// SpawnResult is what spawnChild returns to the parent CLI command on
// success.
type SpawnResult struct {
	PID     int
	LogPath string
}

// SpawnChild re-execs the binary at cfg.Executable in a detached
// session, waits for the child to write its PID file, and (when
// ProbeAddr is non-empty) for the HTTP listener to accept TCP
// connections. Returns the child PID on success.
//
// On any failure path the spawned child is signalled with SIGTERM so a
// half-started daemon does not leak.
//
// SpawnChild is Unix-only (Setsid is required); the package is gated
// with //go:build !windows. AGENTS.md excludes Windows for M1.
func SpawnChild(ctx context.Context, cfg *SpawnConfig) (*SpawnResult, error) {
	if err := validateSpawnConfig(cfg); err != nil {
		return nil, err
	}

	devnullR, devnullW, err := openDevNullPair()
	if err != nil {
		return nil, err
	}
	defer closeWithStderr(devnullR, "daemon: close /dev/null read")
	defer closeWithStderr(devnullW, "daemon: close /dev/null write")

	cmd := buildChildCommand(ctx, cfg, devnullR, devnullW)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("daemon: spawn %s: %w", cfg.Executable, err)
	}
	// Capture the child PID BEFORE Release. As of Go 1.20+ on every
	// non-Windows platform, os.Process.Release() sets Process.Pid = -1.
	// Reading Pid AFTER Release and passing that -1 to syscall.Kill
	// broadcasts SIGTERM to every process the calling user can signal —
	// in practice, collapsing the entire macOS GUI session (Spotlight,
	// Siri, iCloud agents, Control Center, lsd, the menubar) and looking
	// exactly like a kernel crash to the operator. The capture below
	// keeps the cleanup signal targeted at the real child.
	childPID := cmd.Process.Pid
	// Detach: the child is Setsid'd into its own session, so init (PID
	// 1) reaps it, not the parent. We must still call Release so Go's
	// runtime does not hold the os.Process finalizer; dropping the
	// reaper goroutine avoids "wait: no child processes" stderr noise
	// every successful daemon start would otherwise emit.
	if rerr := cmd.Process.Release(); rerr != nil {
		fmt.Fprintf(os.Stderr, "daemon: release child handle: %v\n", rerr)
	}

	if err := waitForReady(ctx, cfg, childPID); err != nil {
		// Belt-and-suspenders: even with the capture above, an unforeseen
		// future regression that pipes -1 (or 0) into this code path
		// would still nuke the user session. Refuse to signal non-
		// positive PIDs explicitly so the broadcast door is welded shut.
		if childPID > 0 {
			if kerr := sigKill(childPID, syscall.SIGTERM); kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
				fmt.Fprintf(os.Stderr, "daemon: cleanup SIGTERM pid %d: %v\n", childPID, kerr)
			}
		}
		return nil, err
	}

	info, err := ReadPID(cfg.PIDPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: read pid after spawn: %w", err)
	}
	return &SpawnResult{PID: info.PID, LogPath: cfg.LogPath}, nil
}

func validateSpawnConfig(cfg *SpawnConfig) error {
	if cfg == nil {
		return errors.New("daemon: SpawnChild cfg must not be nil")
	}
	if cfg.Executable == "" {
		return errors.New("daemon: SpawnConfig.Executable required")
	}
	if cfg.PIDPath == "" {
		return errors.New("daemon: SpawnConfig.PIDPath required")
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 10 * time.Second
	}
	// Surface the live-owner case at the parent layer so the user sees
	// "already running (pid N)" without a 10 s spawn timeout. Require
	// both liveness AND maz-daemon identity: a PID that's alive but
	// belongs to an unrelated process (post-recycle) is a stale file,
	// not a live owner. Other ReadPID failures (missing file, parse
	// error, symlink) are benign here: WritePID inside the child
	// handles those via its remove + O_EXCL self-heal at pidfile.go:62.
	if info, err := ReadPID(cfg.PIDPath); err == nil && info.Running && IsMazDaemon(info.PID, info.Token) {
		return fmt.Errorf("%w: pid %d holds %s", ErrAlreadyRunning, info.PID, cfg.PIDPath)
	}
	return nil
}

func openDevNullPair() (read, write *os.File, err error) {
	read, err = os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: open /dev/null read: %w", err)
	}
	write, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		closeWithStderr(read, "daemon: close /dev/null read")
		return nil, nil, fmt.Errorf("daemon: open /dev/null write: %w", err)
	}
	return read, write, nil
}

func closeWithStderr(c interface{ Close() error }, label string) {
	if err := c.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", label, err)
	}
}

func buildChildCommand(ctx context.Context, cfg *SpawnConfig, stdin, stdout *os.File) *exec.Cmd {
	cmd := exec.CommandContext(ctx, cfg.Executable, cfg.Args...) // #nosec G204 -- executable resolved via os.Executable in the caller, args are package-internal literals
	cmd.Env = cfg.Env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = 0
	// Detach: ctx cancel must not propagate SIGKILL to the session; the
	// child outlives the parent process.
	cmd.Cancel = func() error { return nil }
	return cmd
}

func waitForReady(ctx context.Context, cfg *SpawnConfig, childPID int) error {
	deadline := time.Now().Add(cfg.ReadyTimeout)
	if err := waitForPIDFile(ctx, cfg.PIDPath, childPID, deadline); err != nil {
		return err
	}
	if cfg.ProbeAddr != "" {
		return waitForTCP(ctx, cfg.ProbeAddr, childPID, deadline)
	}
	if cfg.ProbeSocket != "" {
		return waitForUnixSocket(ctx, cfg.ProbeSocket, childPID, deadline)
	}
	return nil
}

// waitForUnixSocket blocks until the daemon accepts a connection on the
// Unix socket at path, the child dies, or the deadline elapses. It is the
// socket-transport analogue of waitForTCP and the default readiness probe
// under the TCP-off posture.
func waitForUnixSocket(ctx context.Context, path string, childPID int, deadline time.Time) error {
	const poll = 50 * time.Millisecond
	// probeCtx below is the single knob controlling per-attempt duration; a
	// net.Dialer.Timeout would just duplicate that 200ms deadline (the
	// shorter wins, and they are identical). Matches waitForTCP's pattern.
	var dialer net.Dialer
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		conn, derr := dialer.DialContext(probeCtx, "unix", path)
		cancel()
		if derr == nil {
			if cerr := conn.Close(); cerr != nil {
				return fmt.Errorf("daemon: close probe conn: %w", cerr)
			}
			if !processAlive(childPID) {
				return fmt.Errorf("daemon: child %d exited after %s accepted", childPID, path)
			}
			return nil
		}
		if !processAlive(childPID) {
			return fmt.Errorf("daemon: child %d exited before %s accepted connections", childPID, path)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon: timed out probing %s", path)
		}
		if err := sleepCtx(ctx, poll); err != nil {
			return err
		}
	}
}

func waitForPIDFile(ctx context.Context, path string, childPID int, deadline time.Time) error {
	const poll = 50 * time.Millisecond
	for {
		// File-present alone is not enough: a stale PID file could
		// satisfy os.Stat before WritePID's remove + O_EXCL replaces
		// it. Require ReadPID to return the child's own PID so the
		// parent never moves on with a stale identity.
		if info, err := ReadPID(path); err == nil && info.PID == childPID {
			return nil
		}
		if !processAlive(childPID) {
			return fmt.Errorf("daemon: child %d exited before writing PID", childPID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon: timed out waiting for PID file %s", path)
		}
		if err := sleepCtx(ctx, poll); err != nil {
			return err
		}
	}
}

func waitForTCP(ctx context.Context, addr string, childPID int, deadline time.Time) error {
	const poll = 50 * time.Millisecond
	dialer := net.Dialer{Timeout: 200 * time.Millisecond}
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		conn, derr := dialer.DialContext(probeCtx, "tcp", addr)
		cancel()
		if derr == nil {
			if cerr := conn.Close(); cerr != nil {
				return fmt.Errorf("daemon: close probe conn: %w", cerr)
			}
			// TCP accepted, but the child may have died immediately after
			// writing the PID file — most commonly because the address was
			// already in use: the existing listener satisfied the probe
			// while the child exited with "bind: address already in use".
			// Confirm the child is still alive before reporting success.
			if !processAlive(childPID) {
				return fmt.Errorf("daemon: child %d exited after %s accepted (address may already be in use by another daemon)", childPID, addr)
			}
			return nil
		}
		if !processAlive(childPID) {
			return fmt.Errorf("daemon: child %d exited before %s accepted connections", childPID, addr)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon: timed out probing %s", addr)
		}
		if err := sleepCtx(ctx, poll); err != nil {
			return err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return fmt.Errorf("daemon: sleepCtx: %w", ctx.Err())
	}
}

// ResolveExecutable returns the absolute path of the running binary
// suitable for re-exec. Os.Executable is the canonical source; LookPath
// is the fallback for the rare case where the kernel-reported path has
// been removed since startup.
func ResolveExecutable() (string, error) {
	if p, err := os.Executable(); err == nil {
		abs, aerr := filepath.Abs(p)
		if aerr == nil {
			return abs, nil
		}
		return p, nil
	}
	if p, err := exec.LookPath("maz"); err == nil {
		return p, nil
	}
	return "", errors.New("daemon: cannot resolve maz executable for re-exec")
}
