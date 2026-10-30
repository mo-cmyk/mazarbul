//go:build !windows

package daemon

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestProcessRelease_SetsPidToNegativeOne pins the stdlib contract that
// motivated the SpawnChild fix: on every non-Windows GOOS,
// `(*os.Process).Release()` sets `Pid = -1`. Reading Pid after Release
// and passing it to `syscall.Kill` would broadcast the signal to every
// process the calling user can signal — the macOS session-collapse bug.
// If a future Go release ever changes this, the SpawnChild belt
// (capture-before-Release) is still correct, but the suspenders
// (`if childPID > 0`) becomes unnecessary; revisit then.
func TestProcessRelease_SetsPidToNegativeOne(t *testing.T) {
	t.Parallel()
	// `/usr/bin/true` exists on every macOS + Linux runner and exits
	// immediately. We never Wait on the result; the child is reaped by
	// init/launchd because we Setsid below. The test only cares about
	// the stdlib's mutation of cmd.Process.Pid.
	cmd := exec.CommandContext(t.Context(), "/usr/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("/usr/bin/true Start: %v", err)
	}
	if cmd.Process.Pid <= 0 {
		t.Fatalf("post-Start Pid = %d, want positive", cmd.Process.Pid)
	}
	if rerr := cmd.Process.Release(); rerr != nil {
		t.Fatalf("Release: %v", rerr)
	}
	if got := cmd.Process.Pid; got != -1 {
		t.Errorf("post-Release Pid = %d, want -1 (stdlib contract; "+
			"if Go has changed this, audit SpawnChild's cleanup belt)", got)
	}
}

// TestSpawnChild_DoesNotBroadcastSIGTERMOnReadyTimeout is the regression
// gate for the macOS session-collapse bug. Before the fix, SpawnChild
// did `cmd.Process.Release()` and THEN passed `cmd.Process.Pid` (now -1)
// to `syscall.Kill` on the ready-timeout cleanup path. The kernel
// sandbox logged "maz(3769) deny(1) signal others [systemstatusd] signum:15"
// for every process the call attempted to touch — equivalently a
// `kill(-1, SIGTERM)` that SIGTERM'd every user process (Spotlight,
// Siri, iCloud, Control Center, lsd, ...) and looked like the Mac
// had crashed.
//
// The test gates SpawnChild's behaviour: when the child exits before
// writing its PID file (we use `/usr/bin/true`), the cleanup path must
// pass a POSITIVE pid into the kill seam, never -1 or 0.
func TestSpawnChild_DoesNotBroadcastSIGTERMOnReadyTimeout(t *testing.T) {
	// No t.Parallel: sigKill is a package-level seam shared across the
	// process; parallel tests would race on the swap.
	var (
		mu        sync.Mutex
		killCalls []int
	)
	prevKill := sigKill
	sigKill = func(pid int, sig syscall.Signal) error {
		mu.Lock()
		killCalls = append(killCalls, pid)
		mu.Unlock()
		// Pretend success so the cleanup path completes without trying
		// to escalate. Returning ESRCH would also work; nil keeps the
		// "we signalled cleanly" branch covered.
		_ = sig
		return nil
	}
	t.Cleanup(func() { sigKill = prevKill })

	dir := t.TempDir()
	cfg := &SpawnConfig{
		// `/usr/bin/true` exits 0 immediately. waitForReady will see no
		// PID file appear and time out, driving the cleanup branch.
		Executable: "/usr/bin/true",
		Args:       []string{"true"},
		PIDPath:    filepath.Join(dir, "fake.pid"),
		// Keep the timeout tight so the test runs in well under a
		// second; the bug reproduces regardless of timeout length.
		ReadyTimeout: 200 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := SpawnChild(ctx, cfg)
	if err == nil {
		t.Fatalf("SpawnChild: err = nil, want a ready-timeout error")
	}

	mu.Lock()
	calls := append([]int(nil), killCalls...)
	mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("sigKill called %d times, want exactly 1; calls=%v", len(calls), calls)
	}
	if calls[0] <= 0 {
		t.Fatalf("sigKill called with non-positive PID %d — broadcast SIGTERM bug; "+
			"would collapse the macOS user session in production", calls[0])
	}
}

// TestWaitForTCP_RejectsDeadChildAfterTCPSuccess is the regression gate for
// Finding 1: `maz daemon start` used to report "running" when the child died
// immediately after writing its PID file (e.g. because the port was already in
// use). waitForTCP would succeed on the existing listener, never noticing the
// spawned child had already exited.
//
// This test simulates the scenario: a listener holds the target port, the
// child PID is a just-exited process (we use /usr/bin/true), and the parent
// waits. waitForTCP must return a non-nil error naming the dead child rather
// than returning nil (success) just because TCP connected.
func TestWaitForTCP_RejectsDeadChildAfterTCPSuccess(t *testing.T) {
	t.Parallel()

	// Reserve a free port and bind it ourselves to simulate "another daemon
	// is already listening". Keep the listener alive for the test duration.
	lc := net.ListenConfig{}
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()

	// Use a definitely-dead PID: spawn /usr/bin/true, let it exit, keep the
	// PID value. On Unix a reaped PID is ESRCH (not alive).
	cmd := exec.CommandContext(t.Context(), "/usr/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if serr := cmd.Start(); serr != nil {
		t.Fatalf("start /usr/bin/true: %v", serr)
	}
	deadPID := cmd.Process.Pid
	// Wait for the process to actually exit before we use the PID.
	_ = cmd.Wait()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	deadline := time.Now().Add(1 * time.Second)

	err = waitForTCP(ctx, addr, deadPID, deadline)
	if err == nil {
		t.Fatal("waitForTCP: err = nil, want an error (child is dead)")
	}
	if !strings.Contains(err.Error(), "exited after") {
		t.Errorf("error %q does not mention 'exited after'; expected message about dead child", err.Error())
	}
}

// TestDataDirPIDPath_UsesDataDir verifies that DataDirPIDPath embeds the data
// directory, keeping isolated daemon instances from colliding on a shared
// global PID file.
func TestDataDirPIDPath_UsesDataDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := DataDirPIDPath(dir)
	if !strings.HasPrefix(got, dir) {
		t.Errorf("DataDirPIDPath(%q) = %q; want path under dataDir", dir, got)
	}
	if !strings.HasSuffix(got, "mazarbul.pid") {
		t.Errorf("DataDirPIDPath(%q) = %q; want to end in mazarbul.pid", dir, got)
	}
}
