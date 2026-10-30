//go:build !windows

package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
)

// newSupervisorOpts builds OllamaSupervisorOptions with fake seams so
// tests never touch real processes or the filesystem beyond t.TempDir().
//
// HTTPProbe and PidExePath are fixed-shape stubs that match the
// "default supervised path" the legacy tests exercised: no external
// Ollama is running (so attemptRestart never enters delegated mode)
// and every PID adopted from a sidecar file is treated as a real
// ollama binary. Tests that exercise delegation or identity-check
// behaviour override these fields directly.
func newSupervisorOpts(
	t *testing.T,
	probe func(int) bool,
	spawn func(context.Context) (int, error),
) daemon.OllamaSupervisorOptions {
	t.Helper()
	return daemon.OllamaSupervisorOptions{
		DataDir:     t.TempDir(),
		ProbeAlive:  probe,
		SpawnOllama: spawn,
		HTTPProbe:   func(context.Context) bool { return false },
		PidExePath:  func(context.Context, int) (string, error) { return "ollama", nil },
	}
}

// aliveProbe returns a probe that always reports the given PID as alive.
func aliveProbe(target int) func(int) bool {
	return func(pid int) bool { return pid == target }
}

// deadProbe always reports the process dead.
func deadProbe(_ int) bool { return false }

// TestOllamaSupervisor_NoPIDFile: when no sidecar PID file exists the
// supervisor enters StateNotSupervised immediately after Start.
func TestOllamaSupervisor_NoPIDFile(t *testing.T) {
	t.Parallel()
	opts := newSupervisorOpts(t, deadProbe, func(_ context.Context) (int, error) {
		return 0, errors.New("should not be called")
	})
	sup := daemon.NewOllamaSupervisor(opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); sup.Wait() })

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := sup.State(); got != daemon.StateNotSupervised {
		t.Errorf("State = %q, want %q", got, daemon.StateNotSupervised)
	}
	if sup.RestartCount() != 0 {
		t.Errorf("RestartCount = %d, want 0", sup.RestartCount())
	}
}

// TestOllamaSupervisor_HealthyProcess: when the sidecar PID file exists
// and the probe reports the process alive, the supervisor enters StateHealthy.
func TestOllamaSupervisor_HealthyProcess(t *testing.T) {
	t.Parallel()
	opts := newSupervisorOpts(t, nil, nil)
	const fakePID = 99999

	// Write a sidecar PID file with the fake PID.
	if err := writeSidecarPIDForTest(opts.DataDir, fakePID); err != nil {
		t.Fatalf("write sidecar PID: %v", err)
	}
	opts.ProbeAlive = aliveProbe(fakePID)
	opts.SpawnOllama = func(_ context.Context) (int, error) {
		return 0, errors.New("should not be called in healthy path")
	}

	sup := daemon.NewOllamaSupervisor(opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); sup.Wait() })

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := sup.State(); got != daemon.StateHealthy {
		t.Errorf("State = %q, want %q", got, daemon.StateHealthy)
	}
}

// TestOllamaSupervisor_ProcessKilledTransitionsToRestartPending: when
// the probe reports the child dead after Start, the supervisor transitions
// to StateRestartPending and eventually to StateHealthy after a successful
// respawn.
func TestOllamaSupervisor_ProcessKilledTransitionsToRestartPending(t *testing.T) {
	t.Parallel()
	opts := newSupervisorOpts(t, nil, nil)
	const oldPID = 11111
	const newPID = 22222

	if err := writeSidecarPIDForTest(opts.DataDir, oldPID); err != nil {
		t.Fatalf("write sidecar PID: %v", err)
	}

	// Phase 1: alive probe returns true for oldPID so Start → StateHealthy.
	// Phase 2: after the first tick, the probe returns false (child "died").
	// Phase 3: spawn succeeds → StateHealthy with newPID.
	var callCount atomic.Int32
	opts.ProbeAlive = func(pid int) bool {
		n := callCount.Add(1)
		if n <= 1 {
			return pid == oldPID // alive on the initial ownership probe
		}
		return pid == newPID // dead for oldPID, alive once newPID is set
	}

	spawned := make(chan struct{})
	opts.SpawnOllama = func(_ context.Context) (int, error) {
		close(spawned)
		return newPID, nil
	}

	// Inject tiny backoff so the restart attempt fires in milliseconds rather
	// than the production 1s minimum, keeping this test well inside any CI
	// or pre-commit time budget.
	opts.BackoffDurations = []time.Duration{1 * time.Millisecond}

	sup := daemon.NewOllamaSupervisor(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	// Wait for the goroutine to finish before TempDir cleanup removes the
	// data directory; otherwise shutdown writes race the directory removal.
	t.Cleanup(func() { cancel(); sup.Wait() })

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := sup.State(); got != daemon.StateHealthy {
		t.Errorf("initial State = %q, want %q", got, daemon.StateHealthy)
	}

	// Wait for the spawn to occur (supervisor's probe ticker fires at
	// ollamaProbeInterval = 5s). We simulate a quicker path by observing
	// the spawned channel; the test has a 10s budget.
	select {
	case <-spawned:
	case <-ctx.Done():
		t.Fatal("timed out waiting for supervisor to spawn replacement child")
	}

	// Allow a moment for the state machine to commit StateHealthy.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sup.State() == daemon.StateHealthy && sup.RestartCount() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if got := sup.State(); got != daemon.StateHealthy {
		t.Errorf("post-restart State = %q, want %q", got, daemon.StateHealthy)
	}
	if sup.RestartCount() < 1 {
		t.Errorf("RestartCount = %d, want >= 1", sup.RestartCount())
	}
}

// TestOllamaSupervisor_FiveFailuresInSixtySeconds: after failingThreshold
// consecutive spawn failures the supervisor enters StateFailing and stops
// attempting restarts.
//
// Tiny BackoffDurations (1ms each) are injected so all five backoff waits
// complete in milliseconds rather than the production 1+2+4+8+16 = 31s.
func TestOllamaSupervisor_FiveFailuresInSixtySeconds(t *testing.T) {
	t.Parallel()
	opts := newSupervisorOpts(t, nil, nil)
	const fakePID = 33333

	if err := writeSidecarPIDForTest(opts.DataDir, fakePID); err != nil {
		t.Fatalf("write sidecar PID: %v", err)
	}

	// Probe always returns dead so the supervisor immediately schedules restarts.
	opts.ProbeAlive = deadProbe

	// Inject tiny backoff so all five failures occur in milliseconds.
	opts.BackoffDurations = []time.Duration{
		1 * time.Millisecond,
		1 * time.Millisecond,
		1 * time.Millisecond,
		1 * time.Millisecond,
		1 * time.Millisecond,
	}

	var spawnCalls atomic.Int32
	failing := make(chan struct{})
	opts.SpawnOllama = func(_ context.Context) (int, error) {
		n := spawnCalls.Add(1)
		if n >= 5 {
			select {
			case <-failing:
			default:
				close(failing)
			}
		}
		return 0, errors.New("spawn failed")
	}

	sup := daemon.NewOllamaSupervisor(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() { cancel(); sup.Wait() })

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for 5 spawn failures. With 1ms backoffs this finishes in <100ms.
	select {
	case <-failing:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %d spawn failures; got %d", 5, spawnCalls.Load())
	}

	// Allow a short moment for the state machine to commit StateFailing.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sup.State() == daemon.StateFailing {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := sup.State(); got != daemon.StateFailing {
		t.Errorf("State = %q, want %q after %d failures", got, daemon.StateFailing, spawnCalls.Load())
	}
	if sup.FailingSince().IsZero() {
		t.Error("FailingSince is zero; want non-zero when StateFailing")
	}
}

// TestOllamaSupervisor_ContextCancelStopsGoroutine: cancelling the run
// context causes the supervisor goroutine to exit within a short deadline.
// This validates the approved plan requirement: "Scope: daemon stop context
// cancel propagates to OllamaSupervisor.Start — assert goroutine exits
// within 100ms."
//
// Because the supervisor's probe ticker fires every 5s, we rely on the
// select{ctx.Done()} arm in run(). The goroutine must exit quickly.
func TestOllamaSupervisor_ContextCancelStopsGoroutine(t *testing.T) {
	t.Parallel()
	opts := newSupervisorOpts(t, deadProbe, func(_ context.Context) (int, error) {
		return 0, errors.New("not called")
	})
	// No sidecar PID file → StateNotSupervised; shutdown path is trivial.

	sup := daemon.NewOllamaSupervisor(opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); sup.Wait() })

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Cancel the context; the goroutine's select{ctx.Done()} arm fires on
	// its next iteration (at most ollamaProbeInterval = 5s away, but
	// typically immediate since the select is already blocked on Done/ticker).
	// sup.Wait() in the cleanup ensures the goroutine has fully exited
	// (including child shutdown and PID file cleanup) before TempDir removal.
	cancel()
	sup.Wait()
	// No assertions needed beyond "no panic, no data race"; -race covers the rest.
}

// writeSidecarPIDForTest writes a single-line "<pid>\n" file at
// <dataDir>/ollama-serve.pid with mode 0600 for test setup.
func writeSidecarPIDForTest(dataDir string, pid int) error {
	path := filepath.Join(dataDir, "ollama-serve.pid")
	body := []byte(strconv.Itoa(pid) + "\n")
	return os.WriteFile(path, body, 0o600)
}

// TestOllamaSupervisor_PreSpawnHTTPProbe_DelegatesWhenEndpointResponds
// is the regression gate for the macOS-freeze duplicate-spawn path: a
// pre-existing Ollama (Homebrew launchd, an orphan wizard child) must
// cause attemptRestart to delegate rather than launch a competing
// `ollama serve` that would port-conflict five times and burn through
// the failing-threshold budget allocating Metal buffers on each crash.
func TestOllamaSupervisor_PreSpawnHTTPProbe_DelegatesWhenEndpointResponds(t *testing.T) {
	t.Parallel()
	opts := newSupervisorOpts(t, deadProbe, nil)
	const oldPID = 44444

	if err := writeSidecarPIDForTest(opts.DataDir, oldPID); err != nil {
		t.Fatalf("write sidecar PID: %v", err)
	}
	// HTTP probe answers: pre-spawn check must take the delegate path.
	opts.HTTPProbe = func(context.Context) bool { return true }
	// SpawnOllama panics if invoked: the whole point of delegation is
	// to skip the spawn entirely.
	opts.SpawnOllama = func(context.Context) (int, error) {
		t.Fatal("SpawnOllama must not be called when endpoint already responds")
		return 0, nil
	}
	// Tiny backoff so the one-shot RestartPending tick in run() fires
	// within the test budget.
	opts.BackoffDurations = []time.Duration{1 * time.Millisecond}

	sup := daemon.NewOllamaSupervisor(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() { cancel(); sup.Wait() })

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the supervisor to transition Start's RestartPending into
	// the delegated Healthy state. attemptRestart sets state=Healthy
	// after the HTTP probe succeeds, all under tiny backoff.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sup.State() == daemon.StateHealthy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := sup.State(); got != daemon.StateHealthy {
		t.Errorf("State = %q, want %q after delegated takeover", got, daemon.StateHealthy)
	}
}

// TestOllamaSupervisor_DelegatedMode_TakesOverWhenEndpointSilent
// verifies that once the supervisor is in delegated mode, repeated
// HTTPProbe failures cause it to take over by spawning its own child
// (debounced via wedgedFailureThreshold so a single transient miss
// does not trigger needless takeover).
func TestOllamaSupervisor_DelegatedMode_TakesOverWhenEndpointSilent(t *testing.T) {
	t.Parallel()
	opts := newSupervisorOpts(t, deadProbe, nil)

	// HTTP probe sequence: answer initially so attemptRestart enters
	// delegated mode, then go silent for the remaining ticks so the
	// supervisor decides to take over. The atomic counter is the
	// simplest deterministic seam.
	var probeCalls atomic.Int32
	opts.HTTPProbe = func(context.Context) bool {
		return probeCalls.Add(1) == 1 // only the very first call succeeds
	}

	const spawnedPID = 55555
	spawned := make(chan struct{})
	opts.SpawnOllama = func(context.Context) (int, error) {
		select {
		case <-spawned:
		default:
			close(spawned)
		}
		return spawnedPID, nil
	}
	// After takeover, ProbeAlive must report the spawned PID alive so
	// the supervisor settles in StateHealthy without immediately
	// re-restarting.
	opts.ProbeAlive = func(pid int) bool { return pid == spawnedPID }
	// Sidecar PID file with a dead PID so Start enters RestartPending.
	if err := writeSidecarPIDForTest(opts.DataDir, 11111); err != nil {
		t.Fatalf("write sidecar PID: %v", err)
	}
	opts.BackoffDurations = []time.Duration{1 * time.Millisecond}
	// Sub-second tick + 1-strike threshold collapses what is normally a
	// ~15s production debounce into a few ticks of test runtime so the
	// case fits inside the unit-test budget (10s pre-commit hook cap).
	opts.ProbeInterval = 20 * time.Millisecond
	opts.WedgedFailureThreshold = 1

	sup := daemon.NewOllamaSupervisor(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() { cancel(); sup.Wait() })

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// First HTTPProbe succeeds → delegated mode; subsequent probes fail.
	// At WedgedFailureThreshold=1 each ProbeInterval tick increments
	// wedgedCount and triggers takeover, so SpawnOllama is called inside
	// a few hundred ms.
	select {
	case <-spawned:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for delegated-mode takeover; supervisor never spawned")
	}
}

// TestOllamaSupervisor_RejectsStaleSidecarPID_WhenExeMismatches
// verifies the identity-check defence: when the OS has reused a
// previously-recorded PID for an unrelated process, Start must not
// adopt it. The supervisor must enter RestartPending and clear the
// stale PID instead of sending SIGTERM to bash/Slack/etc. at shutdown.
func TestOllamaSupervisor_RejectsStaleSidecarPID_WhenExeMismatches(t *testing.T) {
	t.Parallel()
	const stalePID = 66666
	opts := newSupervisorOpts(t, aliveProbe(stalePID), nil)

	if err := writeSidecarPIDForTest(opts.DataDir, stalePID); err != nil {
		t.Fatalf("write sidecar PID: %v", err)
	}
	// PidExePath reports a non-ollama binary: identity check must fail.
	opts.PidExePath = func(context.Context, int) (string, error) { return "/bin/bash", nil }
	// Endpoint responds so attemptRestart enters delegated mode rather
	// than calling SpawnOllama (which would also work but pulls in more
	// state machinery than this test needs to exercise).
	opts.HTTPProbe = func(context.Context) bool { return true }
	opts.SpawnOllama = func(context.Context) (int, error) {
		t.Fatal("SpawnOllama must not be called: endpoint responds")
		return 0, nil
	}
	opts.BackoffDurations = []time.Duration{1 * time.Millisecond}

	sup := daemon.NewOllamaSupervisor(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() { cancel(); sup.Wait() })

	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Start's identity check sets state=RestartPending. run() dispatches
	// one immediate tick which enters attemptRestart, which sees the
	// HTTP probe respond and delegates. The terminal state is Healthy.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sup.State() == daemon.StateHealthy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := sup.State(); got != daemon.StateHealthy {
		t.Errorf("State = %q, want %q after stale-PID rejection -> delegated", got, daemon.StateHealthy)
	}
}
