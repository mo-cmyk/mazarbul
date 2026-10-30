//go:build e2e

// TestDaemonLifecycle_E2E spawns the maz daemon via Setsid; on a local
// developer macOS host a hung pre-push or test harness can leak the
// detached children and starve kernel resources. The build tag keeps
// this test out of pre-push and the default `go test ./...` run; CI
// opts in explicitly via `go test -tags=e2e ./internal/cli/...`.

package cli_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// TestDaemonLifecycle_E2E compiles maz, starts the daemon, asserts
// status reports running, stops it, and asserts status reports
// not-running. The test binds an ephemeral HTTP port and a
// per-tempdir Unix socket so parallel runs do not collide.
//
// Skipped under -short because it shells out to go build.
func TestDaemonLifecycle_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E daemon lifecycle in -short mode")
	}

	mazBin := buildMaz(t)
	tmp := t.TempDir()
	pidPath := filepath.Join(tmp, "maz.pid")
	logPath := filepath.Join(tmp, "daemon.log")
	socketPath := filepath.Join(tmp, "mazarbul.sock")
	httpAddr := ephemeralLoopback(t)

	env := append(os.Environ(),
		"MAZ_DATA_DIR="+tmp, // isolate the store so the daemon never touches the repo's default .mazarbul-data
		"MAZ_PIDFILE="+pidPath,
		"MAZ_LOG_FILE="+logPath,
		"MAZ_SOCKET="+socketPath,
		"MAZ_HTTP_ADDR="+httpAddr,
	)

	// Cleanup must run even on test failure: kill the daemon and remove
	// the socket so the next run is clean.
	t.Cleanup(func() {
		if info, err := readPIDFile(pidPath); err == nil && info > 0 {
			_ = syscall.Kill(info, syscall.SIGKILL)
		}
		_ = os.Remove(socketPath)
	})

	// 1. start
	runMaz(t, mazBin, env, 0, "daemon", "start")

	// 2. status running
	out := runMaz(t, mazBin, env, 0, "daemon", "status")
	if !strings.Contains(out, "running") {
		t.Fatalf("status after start did not say 'running':\n%s", out)
	}
	pid, err := readPIDFile(pidPath)
	if err != nil {
		t.Fatalf("read pid after start: %v", err)
	}
	if !strings.Contains(out, fmt.Sprintf("pid %d", pid)) {
		t.Fatalf("status missing PID %d:\n%s", pid, out)
	}

	// HTTP probe: confirm the listener really is up on the bound port.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	dialer := net.Dialer{}
	conn, derr := dialer.DialContext(probeCtx, "tcp", httpAddr)
	probeCancel()
	if derr != nil {
		t.Fatalf("HTTP probe %s after start: %v", httpAddr, derr)
	}
	if cerr := conn.Close(); cerr != nil {
		t.Fatalf("close probe: %v", cerr)
	}

	// 3. stop
	runMaz(t, mazBin, env, 0, "daemon", "stop")

	// 4. status absent (PID file removed).
	out = runMaz(t, mazBin, env, 0, "daemon", "status")
	if !strings.Contains(out, "not running") {
		t.Fatalf("status after stop did not say 'not running':\n%s", out)
	}

	if _, err := os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PID file still present after stop: stat err = %v", err)
	}
}

func TestDaemonStop_NoPIDFile(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	mazBin := buildMaz(t)
	tmp := t.TempDir()
	env := append(os.Environ(),
		"MAZ_DATA_DIR="+tmp, // isolate the store from the repo's default .mazarbul-data
		"MAZ_PIDFILE="+filepath.Join(tmp, "maz.pid"),
		"MAZ_LOG_FILE="+filepath.Join(tmp, "daemon.log"),
	)
	out := runMaz(t, mazBin, env, 0, "daemon", "stop")
	if !strings.Contains(out, "not running") {
		t.Errorf("stop without PID did not say 'not running':\n%s", out)
	}
}

// buildMaz compiles the maz binary into a temp dir and returns its
// path. The build is cached per test invocation; subsequent calls in
// the same test reuse the artifact via t.TempDir.
func buildMaz(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "maz")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "../../cmd/maz")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build maz: %v", err)
	}
	return bin
}

// runMaz invokes maz with the given args and env, asserts the exit
// code, and returns combined stdout+stderr. wantExit=-1 disables the
// exit-code assertion. Forces --output text so the human-readable daemon
// status lines ("running pid N", "not running") are present for the
// substring assertions — a piped, non-TTY child otherwise defaults to JSON
// (which emits "not_running"/`"pid":N`, breaking those checks). The old
// --no-tui flag this used to pass was removed in the #268 output rework.
func runMaz(t *testing.T, bin string, env []string, wantExit int, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	full := append([]string{"--output", "text"}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("run %s %v: %v\n%s", bin, args, err, out)
		}
	}
	if wantExit >= 0 && exit != wantExit {
		t.Fatalf("maz %v exit = %d, want %d\n%s", args, exit, wantExit, out)
	}
	return string(out)
}

// readPIDFile returns the daemon PID, delegating to the production
// daemon.ReadPID parser. The pidfile is multi-line (pid, token, data-dir);
// reusing the canonical reader keeps the test in lockstep with the format
// rather than re-parsing it (an earlier hand-rolled Atoi over the whole
// file broke when the token/data-dir lines were added).
func readPIDFile(path string) (int, error) {
	info, err := daemon.ReadPID(path)
	if err != nil {
		return 0, fmt.Errorf("read pid file: %w", err)
	}
	return info.PID, nil
}

// ephemeralLoopback reserves a free TCP port on 127.0.0.1, then closes
// the listener so the daemon can bind it. The brief window between
// close and re-bind is small enough that parallel tests do not realistically
// collide.
func ephemeralLoopback(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	addr := ln.Addr().String()
	if cerr := ln.Close(); cerr != nil {
		t.Fatalf("close reservation: %v", cerr)
	}
	return addr
}
