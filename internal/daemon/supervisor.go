package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// SignalTerm and SignalKill name the outcomes of TerminateGracefully so
// callers can render a user-facing message without re-deriving which
// signal finally caused the exit.
const (
	SignalTerm = "term"
	SignalKill = "kill"
)

// SupervisorOptions configures RunSupervisor. The caller resolves PID,
// log, HTTP, and socket paths via the Default* helpers; the supervisor
// is the M1 walking-skeleton wiring and does not own configuration
// itself.
type SupervisorOptions struct {
	// PIDPath is the file the child writes its PID into. Required.
	PIDPath string

	// HTTPAddr is the opt-in TCP listen address for the MCP HTTP transport.
	// Empty means TCP is off (the default): the supervisor binds only the
	// Unix socket and skips the TCP listener entirely.
	HTTPAddr string

	// UnixSocket is the filesystem path for the MCP Unix-socket transport.
	// Required.
	UnixSocket string

	// Name and Version are advertised to MCP clients during the handshake.
	// Required.
	Name    string
	Version string

	// Logger is the structured logger every transport and tool uses. The
	// supervisor logs its own lifecycle events here as well. Required.
	Logger *zap.Logger

	// Token is the per-launch ownership secret the supervisor was
	// invoked with (via `internal-daemon-serve --token <hex>`). It is
	// written into the PID file so `daemon stop` and `daemon status`
	// can prove the live process is the daemon they intend to address.
	// Empty token is permitted only for legacy-compat code paths;
	// production launches always supply a 128-bit random.
	Token string

	// DataDir is the directory the kernel store lives under. The
	// supervisor's wiring opens ${DataDir}/index.db for the SQLite +
	// sqlite-vec + FTS5 layers; the value is also forwarded into the
	// Ollama embedder probe (no path use, but logged for diagnostics).
	// Required.
	DataDir string

	// ShutdownTimeout bounds the grace period the supervisor gives the
	// MCP server to drain in-flight requests on SIGTERM/SIGINT. Zero
	// falls back to mcp.DefaultShutdownTimeout.
	ShutdownTimeout time.Duration

	// NewServer builds the mcp.Server for this supervisor run. Production
	// callers leave it nil; RunSupervisor substitutes mcp.New. Tests
	// inject a fake to exercise PID + signal lifecycle without binding
	// real listeners.
	NewServer func(mcp.Options) (mcp.Server, error)

	// NewWiring builds the kernel + tool plumbing the supervisor
	// registers on the MCP server. Production callers leave it nil;
	// RunSupervisor substitutes OpenWiring. Tests inject a fake that
	// returns a pre-built *Wired with no kernel deps so the lifecycle
	// path exercises tool registration without depending on a live
	// Ollama daemon or sqlite-vec extension.
	NewWiring func(context.Context, WiringConfig) (*Wired, error)

	// BootTracer is the fsync'd phase recorder that survives kernel
	// panics. Optional: a nil tracer turns every Mark into a no-op so
	// callers (including tests) can omit the field entirely. Production
	// callers create it via NewBootTracer(DataDir) right after the
	// logger and pass it in here.
	BootTracer *BootTracer
}

// prepareHTTPToken reconciles the on-disk HTTP bearer-token file with the
// daemon's TCP posture for this launch and returns the token to hand the MCP
// server (empty when TCP is off) plus a cleanup func to defer.
//
// When TCP is enabled it mints a fresh token. When TCP is off it removes any
// token left behind by a previously SIGKILL-ed TCP-enabled daemon — whose
// shutdown defer never ran — because daemonHTTPEnabled (in the CLI) treats the
// file's presence as the authoritative TCP-on signal, so a stale file would
// make `maz daemon status` report httpEnabled:true for a listener that is off.
// RemoveHTTPToken is idempotent, so the TCP-off path is a no-op in the common
// case where no stale file exists.
func prepareHTTPToken(dataDir, httpAddr string, logger *zap.Logger) (string, func(), error) {
	if httpAddr == "" {
		if rerr := RemoveHTTPToken(dataDir); rerr != nil {
			logger.Warn("daemon: remove stale HTTP token", zap.Error(rerr))
		}
		return "", func() {}, nil
	}
	tok, err := MintAndWriteHTTPToken(dataDir)
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() {
		if rerr := RemoveHTTPToken(dataDir); rerr != nil {
			logger.Warn("daemon: remove HTTP token", zap.Error(rerr))
		}
	}
	return tok, cleanup, nil
}

// RunSupervisor is the child-process entrypoint. It writes the PID file,
// constructs the MCP server with the three configured transports, and
// blocks on signals until SIGTERM/SIGINT triggers a graceful shutdown.
// The PID file is removed on every exit path (success, server error,
// signal) so a crashed run never leaks state.
//
// Ctx is the inherited process context; callers typically pass
// context.Background() and let RunSupervisor install its own
// signal-driven cancellation. Cancelling ctx externally also triggers
// shutdown.
//
// Opts is taken by pointer because SupervisorOptions carries several
// pointer/string fields and a closure; copying on every call is wasted
// work, and validate normalises the value in place.
func RunSupervisor(ctx context.Context, opts *SupervisorOptions) error {
	if opts == nil {
		return errors.New("daemon: RunSupervisor opts must not be nil")
	}
	if err := opts.validate(); err != nil {
		return err
	}

	// PhaseEntry and PhaseLoggerInit are emitted by the CLI entrypoint
	// (runInternalDaemonServe) before this function is called — that is
	// the only place where "child process started but logger not yet
	// available" is observable. RunSupervisor picks up from PhasePIDWrite.
	opts.BootTracer.Mark(PhasePIDWrite)
	if err := WritePID(opts.PIDPath, opts.Token, opts.DataDir); err != nil {
		return fmt.Errorf("daemon: write pid: %w", err)
	}
	// Register cleanup AFTER successful WritePID. A defer above the
	// WritePID check would, on the ErrAlreadyRunning path, race the
	// live owner: the second invocation would unlink the live
	// daemon's PID file even though it never owned it.
	defer func() {
		if err := DeletePID(opts.PIDPath); err != nil {
			opts.Logger.Warn("daemon: delete pid", zap.Error(err))
		}
	}()

	// Reconcile the HTTP bearer-token file with this launch's TCP posture.
	httpToken, cleanupToken, terr := prepareHTTPToken(opts.DataDir, opts.HTTPAddr, opts.Logger)
	if terr != nil {
		return terr
	}
	defer cleanupToken()

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the Ollama lifecycle supervisor when the user config says the
	// wizard started and owns an `ollama serve` child. The supervisor goroutine
	// runs for the lifetime of runCtx; it terminates the child on context cancel.
	opts.BootTracer.Mark(PhaseOllamaSupervisor)
	maybeStartOllamaSupervisor(runCtx, opts)

	// The rebuild coordinator outlives every serving-stack reload: it owns the
	// online-rebuild status the CLI polls across the brief socket-drop window a
	// swap opens, and the channel the reload loop selects on. Parented on runCtx
	// so a SIGTERM mid-rebuild cancels the replay.
	coord := newRebuildCoordinator(runCtx, opts.Logger)

	return runServeLoop(ctx, runCtx, opts, httpToken, coord)
}

// runServeLoop builds the serving stack, starts it, and re-wires it in place on
// the SAME socket whenever an online rebuild swaps the projection. The loop
// returns only on a terminal stop (signal or transport error); an online
// rebuild swap re-enters the loop without ever exiting the process.
//
// On each iteration srv.Start runs in a goroutine so the loop can select
// between the server stopping (terminal) and a rebuild swap request. A swap
// shuts the server down, replays the final delta, closes the live store,
// renames the rebuilt files into place, and falls through to re-wire — the
// listener is down only for that window, which the reconnecting `mcp serve`
// proxy masks for agents.
func runServeLoop(ctx, runCtx context.Context, opts *SupervisorOptions, httpToken string, coord *rebuildCoordinator) error {
	first := true
	for {
		wired, srv, err := wireAndBuildServer(ctx, opts, httpToken, coord)
		if err != nil {
			return err
		}

		if first {
			opts.BootTracer.Mark(PhaseServerStart)
			opts.Logger.Info("daemon: supervisor starting",
				zap.String("httpAddr", opts.HTTPAddr),
				zap.String("socket", opts.UnixSocket),
				zap.String("pidFile", opts.PIDPath),
			)
			opts.BootTracer.Mark(PhaseReady)
			first = false
		} else {
			opts.Logger.Info("daemon: serving stack reloaded after rebuild swap",
				zap.String("socket", opts.UnixSocket))
		}

		startErrCh := make(chan error, 1)
		go func() { startErrCh <- srv.Start(runCtx) }()

		select {
		case startErr := <-startErrCh:
			// The server stopped on its own: a clean shutdown (runCtx cancel),
			// stdio close, or a transport error. Drain on the error path (mirrors
			// the historical explicit-Shutdown), close the wiring, and exit.
			if startErr != nil && !errors.Is(startErr, context.Canceled) {
				shutdownServer(ctx, opts, srv)
			}
			closeWiredGrace(ctx, opts, wired)
			return classifyStartErr(opts, startErr)
		case req := <-coord.reloadCh:
			// Online rebuild: perform the quiesce + swap, then loop to re-wire.
			performSwap(ctx, opts, srv, wired, req)
			if rerr := runCtx.Err(); rerr != nil {
				// A signal landed during the swap window; do not re-bind. Route
				// through classifyStartErr so context.Canceled is a clean exit.
				return classifyStartErr(opts, rerr)
			}
		}
	}
}

// shutdownServer drains and stops srv, logging any error. Used on the terminal
// transport-error path.
func shutdownServer(ctx context.Context, opts *SupervisorOptions, srv mcp.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace(opts.ShutdownTimeout))
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		opts.Logger.Warn("daemon: shutdown", zap.Error(err))
	}
}

// closeWiredGrace closes the wiring under a grace-bounded context, logging any
// error. Wired.Close is idempotent, so the swap path can call it defensively.
func closeWiredGrace(ctx context.Context, opts *SupervisorOptions, wired *Wired) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace(opts.ShutdownTimeout))
	defer cancel()
	if cerr := wired.Close(closeCtx); cerr != nil {
		opts.Logger.Warn("daemon: close wiring", zap.Error(cerr))
	}
}

// classifyStartErr maps a server Start outcome to the supervisor's return value
// and emits the matching lifecycle log line.
func classifyStartErr(opts *SupervisorOptions, startErr error) error {
	switch {
	case startErr == nil:
		opts.Logger.Info("daemon: supervisor stopped cleanly")
		return nil
	case errors.Is(startErr, context.Canceled):
		opts.Logger.Info("daemon: supervisor stopped on signal")
		return nil
	default:
		opts.Logger.Error("daemon: supervisor exited with error", zap.Error(startErr))
		return startErr
	}
}

func (o *SupervisorOptions) validate() error {
	if o.PIDPath == "" {
		return errors.New("daemon: SupervisorOptions.PIDPath required")
	}
	if o.HTTPAddr == "" && o.UnixSocket == "" {
		return errors.New("daemon: SupervisorOptions must enable HTTPAddr or UnixSocket")
	}
	if o.Name == "" {
		return errors.New("daemon: SupervisorOptions.Name required")
	}
	if o.Version == "" {
		return errors.New("daemon: SupervisorOptions.Version required")
	}
	if o.Logger == nil {
		return errors.New("daemon: SupervisorOptions.Logger required")
	}
	if o.DataDir == "" {
		return errors.New("daemon: SupervisorOptions.DataDir required")
	}
	return nil
}

// wireAndBuildServer opens the kernel-side plumbing, builds the MCP server
// with the resolved secret provider wired into mcp.Options.Secrets, and
// registers every produced tool. Extracted from RunSupervisor to keep its
// cyclomatic complexity in check.
//
// On any failure after wiring succeeds, wired is Closed before returning so
// the caller never has to clean up a partially-initialised state. The
// partially-registered tools on srv are unreachable because srv.Start has
// not yet been called.
func wireAndBuildServer(ctx context.Context, opts *SupervisorOptions, httpToken string, coord *rebuildCoordinator) (*Wired, mcp.Server, error) {
	wireFn := opts.NewWiring
	if wireFn == nil {
		wireFn = OpenWiring
	}
	wired, err := wireFn(ctx, WiringConfig{
		DataDir:    opts.DataDir,
		Logger:     opts.Logger,
		BootTracer: opts.BootTracer,
	})
	if err != nil {
		// The LadybugDB C library surfaces the exclusive graph lock as the
		// opaque "failed to open database with status 1". The most common
		// cause is a `maz mcp serve` process that an MCP host (e.g. Claude
		// Desktop) auto-spawned while the daemon was not yet running: that
		// process grabbed the lock in standalone mode and will hold it for
		// its entire lifetime. The daemon then crash-loops under launchd
		// until the lock is released. Emit a clear message naming the likely
		// holder and the remedy before returning the raw error.
		if strings.Contains(err.Error(), "status 1") || strings.Contains(err.Error(), "failed to open database") {
			opts.Logger.Error(
				"daemon: graph database lock held by another process — "+
					"likely a `maz mcp serve` your editor or Claude Desktop auto-started "+
					"while the daemon was not yet running; "+
					"quit that MCP host to release the lock, then launchd will restart the daemon cleanly",
				zap.String("dataDir", opts.DataDir),
				zap.Error(err),
			)
		}
		return nil, nil, fmt.Errorf("daemon: open wiring: %w", err)
	}

	v1, v1Err := NewV1Handler(wired, opts.Logger)
	if v1Err != nil {
		// Non-fatal: the MCP surface still works without the /v1 REST layer.
		// Log the failure and continue without ExtraRoutes.
		opts.Logger.Warn("daemon: v1 handler unavailable; /v1 routes disabled",
			zap.Error(v1Err),
		)
	}

	// Build the socket-only admin write surface (POST /admin/v1/events:append
	// and, in Part B, /admin/v1/rebuild + /admin/v1/reindex). Non-fatal: the
	// daemon still serves MCP and /v1 if the admin handler fails to wire.
	adminH, adminErr := NewAdminHandler(wired, coord, opts.DataDir, opts.Logger)
	if adminErr != nil {
		opts.Logger.Warn("daemon: admin handler unavailable; /admin routes disabled",
			zap.Error(adminErr),
		)
	}

	build := opts.NewServer
	if build == nil {
		build = mcp.New
	}
	srv, err := build(mcp.Options{
		Name:                opts.Name,
		Version:             opts.Version,
		Logger:              opts.Logger,
		HTTPAddr:            opts.HTTPAddr,
		Token:               httpToken,
		UnixSocket:          opts.UnixSocket,
		ShutdownTimeout:     opts.ShutdownTimeout,
		Secrets:             wired.Secrets,
		ExtraRoutes:         v1,
		AdminRoutes:         adminH,
		InstructionComposer: wired.InstructionComposer, // #214; nil-safe (static base when unwired)
	})
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace(opts.ShutdownTimeout))
		if cerr := wired.Close(closeCtx); cerr != nil {
			opts.Logger.Warn("daemon: close wiring after server build failure", zap.Error(cerr))
		}
		cancel()
		return nil, nil, fmt.Errorf("daemon: build mcp server: %w", err)
	}

	for _, t := range wired.Tools {
		if rerr := srv.Register(t); rerr != nil {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace(opts.ShutdownTimeout))
			if cerr := wired.Close(closeCtx); cerr != nil {
				opts.Logger.Warn("daemon: close wiring after register failure", zap.Error(cerr))
			}
			cancel()
			return nil, nil, fmt.Errorf("daemon: register tool %q: %w", t.Name(), rerr)
		}
	}

	// Register MCP resources (hot brief, vault index, pinned docs, documents
	// template). The server type-asserts to ResourceRegistrar; implementations
	// that do not support resources (e.g. test NoopServer) simply do not satisfy
	// it and resource registration is skipped silently.
	if rr, ok := srv.(mcp.ResourceRegistrar); ok {
		rr.RegisterContextResources(wired.ResourceAssembler, wired.ResourceDocReader)
	}

	// Boot-time backlog drain (daemon-only). Re-enqueue episodes appended while
	// no extractor was active — a one-shot in-process ingest that exited before
	// its async worker ran, a crash mid-extraction, or the daemon being down at
	// ingest time. This lives here, NOT in wireMemoryLayer, because the latter
	// runs on every OpenWiring — including the ~10 one-shot in-process CLI
	// commands (get/recall/source list/...) whose deferred drainAllBackground
	// would otherwise block the command until the ENTIRE backlog extracted.
	// context.WithoutCancel so the drain survives the boot context.
	drainExtractionBacklog(context.WithoutCancel(ctx), wired, opts.Logger)

	// Pre-warm the local Ollama model(s) off the request path so the first
	// recall embed / extraction does not pay a cold model-load (feedback #3).
	// context.WithoutCancel so the load survives the boot context.
	prewarmModels(context.WithoutCancel(ctx), wired, opts.Logger)

	return wired, srv, nil
}

// drainExtractionBacklog asynchronously re-enqueues any un-extracted episodes in
// the event log for extraction. Background goroutine so a large backlog does not
// delay the server coming up. It is a no-op when the wiring carries no
// orchestrator/event-log (degraded or test wirings); DrainUnextractedEpisodes
// itself self-gates (returns immediately without an extractor) and skips
// episodes that already have downstream mentions.
func drainExtractionBacklog(ctx context.Context, wired *Wired, logger *zap.Logger) {
	if wired == nil || wired.Orchestrator == nil || wired.Orchestrator.Log == nil {
		return
	}
	log := wired.Orchestrator.Log
	go func() {
		n, err := log.DrainUnextractedEpisodes(ctx)
		if err != nil {
			logger.Warn("daemon: extraction backlog drain failed", zap.Error(err))
			return
		}
		if n > 0 {
			logger.Info("daemon: extraction backlog drain enqueued un-extracted episode(s)",
				zap.Int("queued", n),
			)
		}
	}()
}

func shutdownGrace(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return mcp.DefaultShutdownTimeout
}

// WaitForExit waits up to grace for pid to exit, polling at the given
// cadence. Returns true if the process is still alive at the deadline.
// Exported for the CLI stop command and the lifecycle test.
func WaitForExit(pid int, grace time.Duration, poll time.Duration) bool {
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	deadline := time.Now().Add(grace)
	for {
		if !processAlive(pid) {
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(poll)
	}
}

// TerminateGracefully sends SIGTERM, waits up to softGrace for the
// process to exit. If still alive it escalates to SIGKILL and waits
// killGrace longer. Returns the signal that finally caused termination
// (SignalTerm or SignalKill) and any error from the kill syscalls. The
// caller is responsible for removing the PID file.
func TerminateGracefully(pid int, softGrace, killGrace time.Duration) (string, error) {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return SignalTerm, nil
		}
		return "", fmt.Errorf("daemon: SIGTERM pid %d: %w", pid, err)
	}
	if !WaitForExit(pid, softGrace, 100*time.Millisecond) {
		return SignalTerm, nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// The SIGTERM grace caught it just before escalation; from
			// the caller's view this was a clean SIGTERM exit, not a
			// SIGKILL forcing-the-issue.
			return SignalTerm, nil
		}
		return "", fmt.Errorf("daemon: SIGKILL pid %d: %w", pid, err)
	}
	if WaitForExit(pid, killGrace, 50*time.Millisecond) {
		return SignalKill, fmt.Errorf("daemon: pid %d survived SIGKILL", pid)
	}
	return SignalKill, nil
}
