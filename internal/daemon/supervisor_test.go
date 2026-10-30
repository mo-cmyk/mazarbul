package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// fakeServer is a minimal mcp.Server stand-in that blocks Start until
// Shutdown is called or its context is cancelled. It lets the
// supervisor exercise its PID + signal lifecycle without binding real
// HTTP or Unix listeners.
type fakeServer struct {
	startCalled    chan struct{}
	shutdownCalled chan struct{}
	startErr       error
	shutdownErr    error
	mu             sync.Mutex
	done           bool
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		startCalled:    make(chan struct{}),
		shutdownCalled: make(chan struct{}),
	}
}

func (f *fakeServer) Register(_ mcp.Tool) error { return nil }

func (f *fakeServer) Start(ctx context.Context) error {
	close(f.startCalled)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.shutdownCalled:
		return f.startErr
	}
}

func (f *fakeServer) Shutdown(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done {
		return nil
	}
	f.done = true
	close(f.shutdownCalled)
	return f.shutdownErr
}

func newTestSupervisorOpts(t *testing.T, pidPath string, fake *fakeServer) *daemon.SupervisorOptions {
	t.Helper()
	return &daemon.SupervisorOptions{
		PIDPath:    pidPath,
		HTTPAddr:   "127.0.0.1:0",
		UnixSocket: filepath.Join(t.TempDir(), "mazarbul.sock"),
		Name:       "mazarbul-test",
		Version:    "test",
		Logger:     zap.NewNop(),
		DataDir:    t.TempDir(),
		NewServer:  func(_ mcp.Options) (mcp.Server, error) { return fake, nil },
		// NewWiring fakes the kernel plumbing so the lifecycle test does
		// not need a live Ollama daemon or sqlite-vec extension.
		// supervisor_test.go is about PID + signal lifecycle; the
		// kernel-coupled paths are exercised by the integration suite.
		NewWiring: func(_ context.Context, _ daemon.WiringConfig) (*daemon.Wired, error) {
			return &daemon.Wired{}, nil
		},
	}
}

func TestRunSupervisor_LifecycleOnContextCancel(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "maz.pid")
	fake := newFakeServer()
	opts := newTestSupervisorOpts(t, pidPath, fake)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- daemon.RunSupervisor(ctx, opts) }()

	// Wait for Start so we know the PID file is in place.
	select {
	case <-fake.startCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fake server Start")
	}

	info, err := daemon.ReadPID(pidPath)
	if err != nil {
		t.Fatalf("ReadPID while supervisor running: %v", err)
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID file contents = %d, want self %d", info.PID, os.Getpid())
	}

	cancel()

	select {
	case rerr := <-errCh:
		if rerr != nil {
			t.Fatalf("RunSupervisor returned %v, want nil on ctx cancel", rerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RunSupervisor to return")
	}

	if _, err := os.Stat(pidPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PID file still present after shutdown: stat err = %v", err)
	}
}

func TestRunSupervisor_AlreadyRunning(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "maz.pid")
	if err := os.WriteFile(pidPath, []byte(currentPIDStr()), 0o600); err != nil {
		t.Fatalf("seed live pid file: %v", err)
	}
	// WritePID's already-running gate requires both liveness AND
	// maz-daemon identity. The Go test runner is alive but not a maz
	// daemon, so stub the identity probe to claim ownership.
	restore := daemon.SetIsMazDaemonForTest(func(int, string) bool { return true })
	defer daemon.SetIsMazDaemonForTest(restore)

	fake := newFakeServer()
	opts := newTestSupervisorOpts(t, pidPath, fake)

	err := daemon.RunSupervisor(context.Background(), opts)
	if !errors.Is(err, daemon.ErrAlreadyRunning) {
		t.Fatalf("RunSupervisor over live PID: err = %v, want ErrAlreadyRunning", err)
	}

	// The seeded PID file must survive because the supervisor never
	// wrote (and therefore never owns) it. Greptile-style invariant.
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("seeded PID file removed by ErrAlreadyRunning path: %v", err)
	}
}

// TestWritePID_LiveButNotMazDaemonReplacesFile verifies the
// PID-reuse safety property added in this PR: a PID file whose PID
// has been recycled by an unrelated live process is treated as
// stale and replaced, not honored as a live owner.
func TestWritePID_LiveButNotMazDaemonReplacesFile(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "maz.pid")
	if err := os.WriteFile(pidPath, []byte(currentPIDStr()), 0o600); err != nil {
		t.Fatalf("seed live pid file: %v", err)
	}
	// Identity probe says "not a maz daemon" — simulates a recycled PID.
	restore := daemon.SetIsMazDaemonForTest(func(int, string) bool { return false })
	defer daemon.SetIsMazDaemonForTest(restore)

	if err := daemon.WritePID(pidPath, "", ""); err != nil {
		t.Fatalf("WritePID over recycled-PID file: %v, want nil (file should be replaced)", err)
	}

	info, err := daemon.ReadPID(pidPath)
	if err != nil {
		t.Fatalf("ReadPID after replace: %v", err)
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID file PID = %d, want %d (current process, replacement)", info.PID, os.Getpid())
	}
}

// TestRunSupervisor_WiringFailureSurfacesCleanly pins that a
// NewWiring failure surfaces to the caller without leaving a PID file
// behind. A live deployment hitting this path (e.g. Ollama unreachable
// at boot) must fail fast and let `maz daemon start`'s parent log a
// readable error rather than wedge half-running.
func TestRunSupervisor_WiringFailureSurfacesCleanly(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "maz.pid")
	fake := newFakeServer()
	opts := newTestSupervisorOpts(t, pidPath, fake)
	wireErr := errors.New("ollama unreachable")
	opts.NewWiring = func(_ context.Context, _ daemon.WiringConfig) (*daemon.Wired, error) {
		return nil, wireErr
	}

	err := daemon.RunSupervisor(context.Background(), opts)
	if !errors.Is(err, wireErr) {
		t.Fatalf("RunSupervisor: err = %v, want wrap of %v", err, wireErr)
	}
	if _, sErr := os.Stat(pidPath); !errors.Is(sErr, os.ErrNotExist) {
		t.Errorf("PID file still present after wiring failure: stat err = %v", sErr)
	}
}

func TestRunSupervisor_RejectsMissingOptions(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*daemon.SupervisorOptions)
	}{
		{"no pid", func(o *daemon.SupervisorOptions) { o.PIDPath = "" }},
		{"no transport", func(o *daemon.SupervisorOptions) { o.HTTPAddr, o.UnixSocket = "", "" }},
		{"no name", func(o *daemon.SupervisorOptions) { o.Name = "" }},
		{"no version", func(o *daemon.SupervisorOptions) { o.Version = "" }},
		{"no logger", func(o *daemon.SupervisorOptions) { o.Logger = nil }},
		{"no data dir", func(o *daemon.SupervisorOptions) { o.DataDir = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := newTestSupervisorOpts(t, filepath.Join(t.TempDir(), "maz.pid"), newFakeServer())
			tc.mut(opts)
			err := daemon.RunSupervisor(context.Background(), opts)
			if err == nil {
				t.Fatalf("RunSupervisor with %s: err = nil, want non-nil", tc.name)
			}
		})
	}
}

func TestWaitForExit_ProcessGone(t *testing.T) {
	// A PID we know is dead. Use staleDeadPID from pidfile_test.go (same
	// package _test).
	pid := staleDeadPID(t)
	if alive := daemon.WaitForExit(pid, 100*time.Millisecond, 10*time.Millisecond); alive {
		t.Errorf("WaitForExit returned alive=true for dead pid %d", pid)
	}
}

func TestWaitForExit_ProcessAlive(t *testing.T) {
	// Spawn a sleeper, observe its alive-ness, then terminate.
	pid, reaped := startSleeper(t)
	defer func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		<-reaped
	}()
	alive := daemon.WaitForExit(pid, 100*time.Millisecond, 25*time.Millisecond)
	if !alive {
		t.Errorf("WaitForExit returned alive=false for live pid %d", pid)
	}
}

func TestTerminateGracefully_SIGTERMSucceeds(t *testing.T) {
	pid, reaped := startSleeper(t)
	defer func() { <-reaped }()
	signal, err := daemon.TerminateGracefully(pid, 2*time.Second, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("TerminateGracefully: %v", err)
	}
	if signal != daemon.SignalTerm {
		t.Errorf("signal = %q, want %q", signal, daemon.SignalTerm)
	}
}

func TestTerminateGracefully_AlreadyDead(t *testing.T) {
	pid := staleDeadPID(t)
	signal, err := daemon.TerminateGracefully(pid, 100*time.Millisecond, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("TerminateGracefully(dead pid %d): %v", pid, err)
	}
	if signal != daemon.SignalTerm {
		t.Errorf("signal for already-dead pid = %q, want %q", signal, daemon.SignalTerm)
	}
}

// startSleeper launches /bin/sh -c 'sleep 5' and returns the child PID
// plus a channel that closes once the OS has reaped the process. The
// reaper goroutine prevents the child from lingering as a zombie after
// SIGTERM/SIGKILL, which would otherwise keep processAlive truthy via
// kill(-0) and break TerminateGracefully's polling loop.
func startSleeper(t *testing.T) (int, <-chan struct{}) {
	t.Helper()
	proc, err := os.StartProcess("/bin/sh", []string{"sh", "-c", "sleep 5"}, &os.ProcAttr{})
	if err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		if _, werr := proc.Wait(); werr != nil {
			t.Logf("sleeper wait: %v", werr)
		}
	}()
	return proc.Pid, reaped
}

func currentPIDStr() string {
	return strconv.Itoa(os.Getpid()) + "\n"
}
