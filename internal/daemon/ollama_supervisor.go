//go:build !windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// OllamaSupervisorState is the runtime status of the Ollama child the
// supervisor owns. The four values map directly to maz doctor status
// codes; see newOllamaSupervisorCheck in internal/doctor/checks.go.
type OllamaSupervisorState string

const (
	// StateNotSupervised means no sidecar PID file was found at startup;
	// the supervisor has no child to manage.
	StateNotSupervised OllamaSupervisorState = "notSupervised"
	// StateHealthy means the last liveness probe reported the child alive.
	StateHealthy OllamaSupervisorState = "healthy"
	// StateRestartPending means the child was found dead and a restart is
	// in progress (inside the backoff window before the next attempt).
	StateRestartPending OllamaSupervisorState = "restartPending"
	// StateFailing means the child has crashed failingThreshold or more
	// times within failingWindow; the supervisor has stopped restarting.
	StateFailing OllamaSupervisorState = "failing"
)

// backoffDurations returns the successive wait durations between restart
// attempts. The final entry (30s) is repeated when the attempt index
// exceeds the slice length. Returned as a function rather than a package-
// level var to satisfy gochecknoglobals.
func backoffDurations() []time.Duration {
	return []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
	}
}

const (
	// Failing threshold: if this many crashes occur within failingWindow,
	// transition to StateFailing and stop restarting.
	failingThreshold = 5
	// Failing window is the sliding time window over which crashes are counted.
	failingWindow = 60 * time.Second
	// Ollama probe interval is the period between liveness probes.
	ollamaProbeInterval = 5 * time.Second
	// Ollama term grace is the SIGTERM to SIGKILL escalation window for the
	// managed child on supervisor shutdown.
	ollamaTermGrace = 3 * time.Second
)

// OllamaSupervisorOptions configures an OllamaSupervisor.
type OllamaSupervisorOptions struct {
	// DataDir is the Mazarbul data directory. The supervisor reads
	// <DataDir>/ollama-serve.pid on startup and removes it on clean shutdown.
	DataDir string

	// Logger receives lifecycle events. Nil falls back to zap.NewNop().
	Logger *zap.Logger

	// ProbeAlive is the liveness function. Production value uses
	// processAlive (signal 0); tests inject a fake. Must be safe to call
	// from the supervisor goroutine.
	ProbeAlive func(pid int) bool

	// SpawnOllama starts a new `ollama serve` child and returns its PID.
	// Production value delegates to spawnOllamaServe; tests inject a fake.
	SpawnOllama func(ctx context.Context) (int, error)

	// NumParallel, when > 0, is set on the spawned ollama child as
	// OLLAMA_NUM_PARALLEL=N. Zero leaves the env unset so Ollama
	// applies its built-in default. Threaded from
	// config.UserConfig.Providers["ollama"].NumParallel; see the
	// docstring there for tuning guidance.
	NumParallel int

	// BackoffDurations overrides the default exponential backoff schedule.
	// Nil uses backoffDurations(). Tests inject tiny values so the five-
	// failure threshold is reached in milliseconds rather than 31+ seconds.
	BackoffDurations []time.Duration

	// HTTPProbe returns true when the Ollama HTTP API is responsive.
	// The supervisor uses it to (a) decide whether to spawn a child or
	// adopt an already-running Ollama and (b) detect wedged children
	// that look alive to signal-0 but stop answering requests. Defaults
	// to a 500ms GET against OllamaURL+/api/version.
	HTTPProbe func(ctx context.Context) bool

	// OllamaURL is the base URL used to construct the default HTTPProbe.
	// Defaults to OLLAMA_HOST (with scheme inferred when missing), or
	// http://127.0.0.1:11434 when OLLAMA_HOST is unset. Ignored when a
	// custom HTTPProbe is provided.
	OllamaURL string

	// PidExePath returns the executable path of the given PID so the
	// supervisor can verify a sidecar-recorded PID still belongs to an
	// ollama process before adopting it. Defaults to a platform-specific
	// implementation (proc_pidpath via `ps` on macOS, /proc/<pid>/exe on
	// Linux); returns "" with nil error on platforms with no native
	// support, which isOllamaExe treats as "trust the PID."
	PidExePath func(ctx context.Context, pid int) (string, error)

	// ProbeInterval is the period between liveness probes. Zero falls back
	// to the package-level ollamaProbeInterval (5s). Tests inject sub-second
	// values so delegated-takeover and wedged-detection cases reach the
	// failure threshold inside a unit-test budget.
	ProbeInterval time.Duration

	// WedgedFailureThreshold is the number of consecutive HTTP-probe
	// failures in delegated mode before the supervisor takes over by
	// spawning its own child. Zero falls back to the package-level
	// wedgedFailureThreshold (3). Tests inject 1 or 2 to avoid the
	// production 15s debounce window.
	WedgedFailureThreshold int
}

// OllamaSupervisor monitors and restarts an `ollama serve` child that
// Mazarbul's init wizard started on behalf of the user. It runs as a
// goroutine inside the daemon process. Start is non-blocking; the
// goroutine exits when the provided context is cancelled.
type OllamaSupervisor struct {
	opts OllamaSupervisorOptions

	mu           sync.RWMutex
	state        OllamaSupervisorState
	restartCount int
	failingSince time.Time     // non-zero only in StateFailing
	pid          int           // current child PID; 0 when notSupervised or delegated
	stopped      chan struct{} // closed when the goroutine exits

	// delegated reports that an external Ollama is providing the API
	// and the supervisor is not managing a child. Set by attemptRestart
	// when the pre-spawn HTTP probe finds the endpoint already responding;
	// cleared when the supervisor decides to spawn its own child. The
	// public State() always reports StateHealthy in delegated mode so
	// maz doctor sees "ollama is up" regardless of who owns the process.
	delegated bool

	// wedgedCount counts consecutive HTTP probe failures in delegated
	// mode. When it reaches wedgedFailureThreshold the supervisor stops
	// trusting the external endpoint and takes over by spawning its own
	// child. Bounded debouncing prevents a single dropped packet or
	// momentary GC pause in the external Ollama from causing an
	// unnecessary takeover.
	wedgedCount int
}

// NewOllamaSupervisor constructs a supervisor. Idle until Start is called.
//
//nolint:gocritic // OllamaSupervisorOptions is heavy (96B) but this constructor runs once per daemon startup
func NewOllamaSupervisor(opts OllamaSupervisorOptions) *OllamaSupervisor {
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	if opts.ProbeAlive == nil {
		opts.ProbeAlive = processAlive
	}
	// Capture the user-configured base URL BEFORE the OLLAMA_HOST
	// fallback below stamps a default onto opts.OllamaURL. The spawned
	// child should only carry an OLLAMA_HOST binding when the user
	// explicitly set [providers.ollama].base_url — otherwise it inherits
	// the user's shell value (if any) or Ollama's compiled-in default.
	userBindHost := opts.OllamaURL
	if opts.SpawnOllama == nil {
		opts.SpawnOllama = func(ctx context.Context) (int, error) {
			return spawnOllamaServe(ctx, opts.DataDir, opts.NumParallel, userBindHost)
		}
	}
	if opts.OllamaURL == "" {
		opts.OllamaURL = resolveOllamaURL()
	}
	if opts.HTTPProbe == nil {
		opts.HTTPProbe = defaultHTTPProbe(opts.OllamaURL)
	}
	if opts.PidExePath == nil {
		opts.PidExePath = defaultPidExePath
	}
	if opts.ProbeInterval == 0 {
		opts.ProbeInterval = ollamaProbeInterval
	}
	if opts.WedgedFailureThreshold == 0 {
		opts.WedgedFailureThreshold = wedgedFailureThreshold
	}
	return &OllamaSupervisor{
		opts:    opts,
		state:   StateNotSupervised,
		stopped: make(chan struct{}),
	}
}

// Wait blocks until the supervisor goroutine has fully exited (including
// child shutdown and PID file cleanup). Call after cancelling the context
// passed to Start. Safe to call multiple times.
func (s *OllamaSupervisor) Wait() {
	<-s.stopped
}

// State returns the current supervisor state. Safe to call from any goroutine.
func (s *OllamaSupervisor) State() OllamaSupervisorState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// RestartCount returns the total number of times the supervisor has
// restarted the child since Start was called.
func (s *OllamaSupervisor) RestartCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.restartCount
}

// FailingSince returns the time at which the supervisor entered
// StateFailing. Returns the zero time when not in that state.
func (s *OllamaSupervisor) FailingSince() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.failingSince
}

// Start reads the sidecar PID file, takes ownership of the existing child
// (or transitions to notSupervised if the file is absent), and launches the
// supervisor goroutine. The goroutine exits when ctx is cancelled; on exit
// the child is terminated and the sidecar PID file is removed. Start returns
// only after the goroutine is running.
func (s *OllamaSupervisor) Start(ctx context.Context) error {
	pid, err := readSidecarPID(s.opts.DataDir)
	if err != nil {
		s.mu.Lock()
		s.state = StateNotSupervised
		s.mu.Unlock()
		s.opts.Logger.Info("ollama supervisor: no sidecar PID file; entering notSupervised",
			zap.String("dataDir", s.opts.DataDir),
			zap.Error(err),
		)
	} else {
		// The sidecar file records only a numeric PID. If the OS has
		// reused that number for an unrelated process (a real risk
		// across login sessions on macOS), adopting it would cause a
		// spurious SIGTERM at shutdown. Verify the running binary is
		// actually ollama before claiming ownership.
		adoptHealthy := false
		if s.opts.ProbeAlive(pid) {
			exe, perr := s.opts.PidExePath(ctx, pid)
			switch {
			case perr != nil:
				s.opts.Logger.Warn("ollama supervisor: identity probe failed; treating sidecar PID as stale",
					zap.Int("pid", pid),
					zap.Error(perr),
				)
			case !isOllamaExe(exe):
				s.opts.Logger.Warn("ollama supervisor: sidecar PID is not an ollama process; treating as stale",
					zap.Int("pid", pid),
					zap.String("exe", exe),
				)
			default:
				adoptHealthy = true
			}
		}
		s.mu.Lock()
		if adoptHealthy {
			s.pid = pid
			s.state = StateHealthy
		} else {
			// Forget the unrelated/stale PID. attemptRestart will
			// either spawn a fresh child or, if an external Ollama
			// answers the pre-spawn probe, enter delegated mode.
			s.pid = 0
			s.state = StateRestartPending
		}
		state := s.state
		s.mu.Unlock()
		// Clear any stale failing marker from a previous crash cycle. If a prior
		// daemon wrote the marker and then crashed before cleanup, the marker
		// would persist across restarts. When the new daemon adopts a live child
		// (StateHealthy), maz doctor must not keep reporting StateFailing. Clear
		// it here rather than relying solely on the attemptRestart path which is
		// not reached when the child is already alive.
		if state == StateHealthy {
			if werr := deleteFailingMarker(s.opts.DataDir); werr != nil {
				s.opts.Logger.Warn("ollama supervisor: clear stale failing marker on adopt", zap.Error(werr))
			}
		}
		s.opts.Logger.Info("ollama supervisor: took ownership of child",
			zap.Int("pid", pid),
			zap.String("state", string(state)),
		)
	}

	ready := make(chan struct{})
	go func() {
		close(ready)
		s.run(ctx)
	}()
	<-ready
	return nil
}

// run is the supervisor goroutine. It probes the child every
// ollamaProbeInterval and restarts it when dead, with bounded backoff.
// When ctx is cancelled it terminates the child and removes the sidecar
// PID file.
//
// If the initial state is StateRestartPending (i.e. the child was already
// dead when Start read the sidecar PID file), one tick is dispatched
// immediately so the restart begins without waiting a full probe interval.
func (s *OllamaSupervisor) run(ctx context.Context) {
	defer close(s.stopped)

	ticker := time.NewTicker(s.opts.ProbeInterval)
	defer ticker.Stop()

	// crashTimes records the wall-clock time of each crash within the
	// sliding failingWindow so the failing-state gate can be evaluated.
	crashTimes := make([]time.Time, 0, failingThreshold)
	retryN := 0 // consecutive spawn failures since last healthy period

	// If we took ownership of an already-dead child, begin restarting now
	// rather than waiting ollamaProbeInterval for the first ticker fire.
	s.mu.RLock()
	initialState := s.state
	s.mu.RUnlock()
	if initialState == StateRestartPending {
		s.tick(ctx, &crashTimes, &retryN)
	}

	for {
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case <-ticker.C:
			s.tick(ctx, &crashTimes, &retryN)
		}
	}
}

// tick performs one probe cycle. Called on every ticker fire.
func (s *OllamaSupervisor) tick(ctx context.Context, crashTimes *[]time.Time, retryN *int) {
	s.mu.RLock()
	state := s.state
	pid := s.pid
	delegated := s.delegated
	s.mu.RUnlock()

	switch state {
	case StateNotSupervised, StateFailing:
		return // nothing to do
	case StateHealthy:
		if delegated {
			// In delegated mode the supervisor does not own a PID;
			// HTTPProbe is the only signal that tells us whether the
			// external Ollama is still up. Debounce transient failures
			// with wedgedFailureThreshold so a single dropped packet or
			// a brief GC pause does not trigger an unnecessary takeover.
			if s.opts.HTTPProbe(ctx) {
				s.mu.Lock()
				s.wedgedCount = 0
				s.mu.Unlock()
				return
			}
			s.mu.Lock()
			s.wedgedCount++
			count := s.wedgedCount
			s.mu.Unlock()
			if count < s.opts.WedgedFailureThreshold {
				s.opts.Logger.Warn("ollama supervisor: external endpoint probe failed",
					zap.String("url", s.opts.OllamaURL),
					zap.Int("consecutiveFailures", count),
				)
				return
			}
			s.opts.Logger.Warn("ollama supervisor: external endpoint went silent; taking over",
				zap.String("url", s.opts.OllamaURL),
				zap.Int("consecutiveFailures", count),
			)
			s.mu.Lock()
			s.delegated = false
			s.wedgedCount = 0
			s.state = StateRestartPending
			s.mu.Unlock()
			s.attemptRestart(ctx, crashTimes, retryN)
			return
		}
		if s.opts.ProbeAlive(pid) {
			return // still alive
		}
		s.opts.Logger.Warn("ollama supervisor: child died",
			zap.Int("pid", pid),
		)
		*crashTimes = recordCrash(*crashTimes, time.Now())
		if failingNow(*crashTimes) {
			s.enterFailing()
			return
		}
		s.mu.Lock()
		s.state = StateRestartPending
		s.mu.Unlock()
		s.attemptRestart(ctx, crashTimes, retryN)
	case StateRestartPending:
		s.attemptRestart(ctx, crashTimes, retryN)
	}
}

// attemptRestart loops: wait backoff, try spawn, repeat until spawn succeeds,
// StateFailing is entered, or ctx is cancelled. Returning from this function
// always leaves the supervisor in a terminal-for-now state (Healthy, Failing,
// or context-cancelled).
//
// Before each spawn attempt the loop runs HTTPProbe: if something already
// answers on the Ollama port (Homebrew launchd, a prior wizard child whose
// PID file we missed, another `maz` instance) the supervisor enters
// delegated mode and skips the spawn. This is the load-bearing fix for
// the macOS-freeze bug — a competing spawn would burn through the
// failing-threshold budget allocating Metal buffers on each crash.
func (s *OllamaSupervisor) attemptRestart(ctx context.Context, crashTimes *[]time.Time, retryN *int) {
	for {
		// Check cancellation BEFORE the next backoff + probe + spawn
		// cycle. Without this, a context cancelled mid-loop would
		// reach HTTPProbe/SpawnOllama with a dead context, which most
		// HTTP/exec implementations return immediately on with an
		// error — leading recordCrash to log a spurious crash entry
		// just before the outer ctx.Done arm of the select kicks in.
		if ctx.Err() != nil {
			return
		}
		wait := s.backoffFor(*retryN)
		s.opts.Logger.Info("ollama supervisor: waiting before restart",
			zap.Duration("backoff", wait),
			zap.Int("attempt", *retryN+1),
		)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		if s.opts.HTTPProbe(ctx) {
			s.mu.Lock()
			s.delegated = true
			s.pid = 0
			s.state = StateHealthy
			s.wedgedCount = 0
			s.mu.Unlock()
			*retryN = 0
			s.opts.Logger.Info("ollama supervisor: endpoint already responding; entering delegated mode",
				zap.String("url", s.opts.OllamaURL),
			)
			// Stale failing marker would otherwise persist and confuse maz doctor.
			if werr := deleteFailingMarker(s.opts.DataDir); werr != nil {
				s.opts.Logger.Warn("ollama supervisor: clear stale failing marker on delegate", zap.Error(werr))
			}
			return
		}

		newPID, err := s.opts.SpawnOllama(ctx)
		if err != nil {
			s.opts.Logger.Error("ollama supervisor: spawn failed",
				zap.Error(err),
				zap.Int("attempt", *retryN+1),
			)
			*retryN++
			*crashTimes = recordCrash(*crashTimes, time.Now())
			if failingNow(*crashTimes) {
				s.enterFailing()
				return
			}
			// Continue loop: apply next backoff and retry.
			continue
		}

		s.mu.Lock()
		s.pid = newPID
		s.delegated = false
		s.restartCount++
		s.state = StateHealthy
		s.wedgedCount = 0
		s.mu.Unlock()
		*retryN = 0

		if werr := writeSidecarPID(s.opts.DataDir, newPID); werr != nil {
			s.opts.Logger.Warn("ollama supervisor: update sidecar PID file", zap.Error(werr))
		}
		// Remove any stale failing marker from a previous crash cycle so
		// maz doctor stops reporting StateFailing now that a child is healthy.
		if werr := deleteFailingMarker(s.opts.DataDir); werr != nil {
			s.opts.Logger.Warn("ollama supervisor: clear stale failing marker", zap.Error(werr))
		}
		s.opts.Logger.Info("ollama supervisor: child restarted",
			zap.Int("newPID", newPID),
			zap.Int("totalRestarts", s.restartCount),
		)
		return
	}
}

// enterFailing transitions to StateFailing, writes a failing-marker file
// so maz doctor can detect the state out-of-process, and logs the event.
func (s *OllamaSupervisor) enterFailing() {
	s.mu.Lock()
	s.state = StateFailing
	s.failingSince = time.Now()
	s.mu.Unlock()
	// Write a marker file so inferOllamaSupervisorState (run by maz doctor
	// out-of-process) can distinguish StateFailing from StateRestartPending.
	if werr := writeFailingMarker(s.opts.DataDir); werr != nil {
		s.opts.Logger.Warn("ollama supervisor: write failing marker", zap.Error(werr))
	}
	s.opts.Logger.Error("ollama supervisor: too many crashes; no longer restarting",
		zap.Int("threshold", failingThreshold),
		zap.Duration("window", failingWindow),
	)
}

// shutdown terminates the child and removes the sidecar PID file. Called
// when the supervisor goroutine's context is cancelled. In delegated
// mode the supervisor does not own a child, so the SIGTERM block is
// skipped entirely; the external Ollama keeps running and is the
// responsibility of whoever spawned it.
func (s *OllamaSupervisor) shutdown() {
	s.mu.RLock()
	pid := s.pid
	state := s.state
	delegated := s.delegated
	s.mu.RUnlock()

	childExited := true
	if !delegated && state != StateNotSupervised && pid > 0 {
		s.opts.Logger.Info("ollama supervisor: terminating child on shutdown",
			zap.Int("pid", pid),
		)
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			s.opts.Logger.Warn("ollama supervisor: SIGTERM child",
				zap.Int("pid", pid),
				zap.Error(err),
			)
		}
		if !WaitForExit(pid, ollamaTermGrace, 100*time.Millisecond) {
			// Child survived SIGTERM grace window; escalate to SIGKILL.
			s.opts.Logger.Warn("ollama supervisor: child did not exit after SIGTERM; sending SIGKILL",
				zap.Int("pid", pid),
			)
			if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr != nil && !errors.Is(kerr, syscall.ESRCH) {
				s.opts.Logger.Warn("ollama supervisor: SIGKILL child", zap.Int("pid", pid), zap.Error(kerr))
			}
			if !WaitForExit(pid, 2*time.Second, 100*time.Millisecond) {
				// Child survived SIGKILL; leave the PID file so maz doctor
				// and the next daemon start can find and report the orphan.
				s.opts.Logger.Error("ollama supervisor: child survived SIGKILL; PID file left on disk",
					zap.Int("pid", pid),
				)
				childExited = false
			}
		}
	}
	if childExited {
		if err := deleteSidecarPID(s.opts.DataDir); err != nil {
			s.opts.Logger.Warn("ollama supervisor: remove sidecar PID file", zap.Error(err))
		}
		// Remove the failing marker so a fresh daemon start begins in a
		// clean state rather than inheriting a stale failing flag.
		if err := deleteFailingMarker(s.opts.DataDir); err != nil {
			s.opts.Logger.Warn("ollama supervisor: remove failing marker", zap.Error(err))
		}
	}
	s.opts.Logger.Info("ollama supervisor: stopped")
}

// backoffFor returns the wait duration for the n-th restart attempt
// (0-indexed). When opts.BackoffDurations is set it is used instead of
// the default backoffDurations() schedule. Caps at the last entry.
func (s *OllamaSupervisor) backoffFor(n int) time.Duration {
	steps := s.opts.BackoffDurations
	if len(steps) == 0 {
		steps = backoffDurations()
	}
	if n >= len(steps) {
		return steps[len(steps)-1]
	}
	return steps[n]
}

// recordCrash appends t and trims entries older than failingWindow,
// capping the slice at failingThreshold entries.
func recordCrash(times []time.Time, t time.Time) []time.Time {
	cutoff := t.Add(-failingWindow)
	out := times[:0]
	for _, ts := range times {
		if ts.After(cutoff) {
			out = append(out, ts)
		}
	}
	out = append(out, t)
	if len(out) > failingThreshold {
		out = out[len(out)-failingThreshold:]
	}
	return out
}

// failingNow returns true when times contains at least failingThreshold
// entries (all within failingWindow by construction from recordCrash).
func failingNow(times []time.Time) bool {
	return len(times) >= failingThreshold
}

// sidecarPIDPath returns <dataDir>/ollama-serve.pid.
func sidecarPIDPath(dataDir string) string {
	return filepath.Join(dataDir, "ollama-serve.pid")
}

// readSidecarPID reads <dataDir>/ollama-serve.pid and returns the PID.
// Uses the existing ReadPID helper which handles single-line files.
func readSidecarPID(dataDir string) (int, error) {
	info, err := ReadPID(sidecarPIDPath(dataDir))
	if err != nil {
		return 0, err
	}
	return info.PID, nil
}

// writeSidecarPID writes a single-line "<pid>\n" PID file at
// <dataDir>/ollama-serve.pid with mode 0600. Replaces any existing file.
func writeSidecarPID(dataDir string, pid int) error {
	path := sidecarPIDPath(dataDir)
	// Remove first so os.WriteFile creates a fresh inode (avoiding mode
	// inheritance on an existing file, per the WritePID pattern).
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: clear sidecar PID file: %w", err)
	}
	body := strconv.Itoa(pid) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("daemon: write sidecar PID file: %w", err)
	}
	return nil
}

// deleteSidecarPID removes <dataDir>/ollama-serve.pid. A missing file is
// not an error.
func deleteSidecarPID(dataDir string) error {
	return DeletePID(sidecarPIDPath(dataDir))
}

// failingMarkerPath returns the path of the marker file written when the
// supervisor enters StateFailing. The marker lets maz doctor read this state
// out-of-process to distinguish StateFailing from StateRestartPending.
func failingMarkerPath(dataDir string) string {
	return filepath.Join(dataDir, "ollama-supervisor-failing")
}

// writeFailingMarker creates <dataDir>/ollama-supervisor-failing with mode
// 0600. Its presence signals StateFailing to out-of-process observers.
func writeFailingMarker(dataDir string) error {
	if err := os.WriteFile(failingMarkerPath(dataDir), []byte("failing\n"), 0o600); err != nil {
		return fmt.Errorf("daemon: write failing marker: %w", err)
	}
	return nil
}

// deleteFailingMarker removes <dataDir>/ollama-supervisor-failing. A missing
// file is not an error.
func deleteFailingMarker(dataDir string) error {
	err := os.Remove(failingMarkerPath(dataDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: remove failing marker: %w", err)
	}
	return nil
}

// FailingMarkerExists reports whether the failing marker file is present under
// dataDir. Used by maz doctor to infer StateFailing out-of-process.
func FailingMarkerExists(dataDir string) bool {
	_, err := os.Stat(failingMarkerPath(dataDir))
	return err == nil
}

// ollamaServeEnv builds the env slice for the spawned `ollama serve`
// child. The resource caps are always applied (two resident models so the
// chat + embed models co-reside without eviction thrash, one parallel slot,
// 30s keep-alive, flash attention) so the supervised sidecar stays bounded.
// The two-model floor matches the default stack (qwen2.5:7b chat ~4.7 GB +
// snowflake-arctic-embed2 ~1.2 GB ≈ 6 GB resident, safe on >= 16 GB hosts).
// When the user has explicitly configured `[providers.ollama] num_parallel = N`
// with N > 1, the user value overrides the slot cap so multi-request workloads
// can opt in. Caps are appended AFTER parent so exec honours the last
// occurrence of each key. Pure function for direct unit testing.
func ollamaServeEnv(parent []string, numParallel int, bindHost string) []string {
	out := make([]string, 0, len(parent)+5)
	out = append(out, parent...)
	out = append(out,
		"OLLAMA_MAX_LOADED_MODELS=2",
		"OLLAMA_KEEP_ALIVE=30s",
		"OLLAMA_FLASH_ATTENTION=1",
	)
	if numParallel > 1 {
		out = append(out, fmt.Sprintf("OLLAMA_NUM_PARALLEL=%d", numParallel))
	} else {
		out = append(out, "OLLAMA_NUM_PARALLEL=1")
	}
	// Bind the spawned child to the same endpoint the provider client
	// talks to. ollama serve accepts OLLAMA_HOST as a bare host:port or
	// scheme://host:port; stripping the scheme keeps the value
	// canonical so a probe URL with a leading "http://" still resolves
	// to the correct bind address.
	if host := ollamaHostFromURL(bindHost); host != "" {
		out = append(out, "OLLAMA_HOST="+host)
	}
	return out
}

// ollamaHostFromURL extracts the bare host:port from a URL of the form
// "scheme://host:port" or "host:port". Empty input returns empty (caller
// uses that as the "no override" signal). The function deliberately
// avoids net/url for the common case so a path or query string in the
// input does not silently leak into OLLAMA_HOST.
func ollamaHostFromURL(u string) string {
	s := strings.TrimSpace(u)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

// spawnOllamaServe starts `ollama serve` detached (Setpgid:true), with
// stdout/stderr appended to <dataDir>/ollama-serve.log. Returns the child
// PID. The supervisor does not reap the child; on shutdown it sends SIGTERM
// and WaitForExit polls until the kernel reclaims it.
//
// NumParallel > 0 sets OLLAMA_NUM_PARALLEL=N in the child's env so a
// single model runner can service multiple chat/embed requests
// concurrently. Zero falls through to the F1 cap (1 parallel slot) baked
// into ollamaServeEnv.
func spawnOllamaServe(ctx context.Context, dataDir string, numParallel int, bindHost string) (int, error) {
	cmd, logFile, err := buildOllamaServeCmd(ctx, dataDir, numParallel, bindHost)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logFile.Close() }()

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("ollama supervisor: spawn: %w", err)
	}
	pid := cmd.Process.Pid
	// Release so the runtime does not wait for this process; the
	// supervisor tracks it via PID and signal-0 probes.
	if rerr := cmd.Process.Release(); rerr != nil {
		// Release failed: kill the child before returning so the restart
		// loop does not accumulate unmanaged orphan processes.
		if kerr := cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
			return 0, fmt.Errorf("ollama supervisor: kill after release failure: %w", kerr)
		}
		return 0, fmt.Errorf("ollama supervisor: release: %w", rerr)
	}
	return pid, nil
}

// buildOllamaServeCmd configures the *exec.Cmd that will run `ollama
// serve`. Extracted from spawnOllamaServe so tests can verify command
// shape (resource caps, stdout/stderr, process-group attributes)
// without spawning a real process. The returned *os.File is the open
// log handle; callers own closing it after cmd.Start returns (the
// child inherits the FD on Start).
func buildOllamaServeCmd(ctx context.Context, dataDir string, numParallel int, bindHost string) (*exec.Cmd, *os.File, error) {
	logPath := filepath.Join(dataDir, "ollama-serve.log")
	//nolint:gosec // logPath is dataDir + constant filename
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("ollama supervisor: open log %s: %w", logPath, err)
	}
	cmd := exec.CommandContext(ctx, "ollama", "serve")
	cmd.Cancel = func() error { return nil } // supervisor manages lifecycle manually
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = ollamaServeEnv(os.Environ(), numParallel, bindHost)
	return cmd, logFile, nil
}
