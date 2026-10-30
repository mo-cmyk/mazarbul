package daemon

// rebuild_coordinator.go owns the lifecycle of an ONLINE rebuild/reindex: a
// from-scratch replay of the event log into a fresh projection that swaps in
// without the operator ever stopping the daemon.
//
// Why a coordinator that outlives the serving stack: the swap is performed by
// the supervisor's reload loop, which tears down the current *Wired (closing
// its jobs.Registry) and re-wires a fresh one on the same socket. A rebuild
// job tracked through the per-Wired registry would lose its terminal
// transition across that teardown. The coordinator is created ONCE in
// RunSupervisor and survives every reload, so it is the authoritative source
// of rebuild status the CLI polls — across the socket-drop window the swap
// briefly opens (the reconnecting `mcp serve` proxy masks that window for
// agents; the CLI re-dials).
//
// One rebuild at a time: start() is a mutex-guarded TryLock. A second trigger
// while one is running returns busy=false → the admin endpoint maps that to
// 409 Conflict.

import (
	"context"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// rebuildKind distinguishes a full rebuild from a reindex. Both run the same
// shadow-build + swap path (a reindex IS a from-log rebuild that regenerates
// every vector with the current embedder); the kind is carried for status
// reporting and the source label only.
type rebuildKind string

const (
	rebuildKindRebuild rebuildKind = "rebuild"
	rebuildKindReindex rebuildKind = "reindex"
)

// rebuildState is the coordinator's lifecycle phase, surfaced verbatim on the
// status endpoint so the CLI can drive its progress display and decide when to
// stop polling.
type rebuildState string

const (
	rebuildStateIdle      rebuildState = "idle"
	rebuildStateBuilding  rebuildState = "building"  // shadow build in progress (daemon serving normally)
	rebuildStateSwapping  rebuildState = "swapping"  // quiesce + file swap + reload (socket briefly down)
	rebuildStateSucceeded rebuildState = "succeeded" // swap committed; serving the rebuilt projection
	rebuildStateFailed    rebuildState = "failed"    // aborted; the original projection still serves
)

// rebuildStatusView is the JSON the admin status endpoint returns and the CLI
// polls. It is a value snapshot so the caller never observes a torn read.
type rebuildStatusView struct {
	JobUID   string  `json:"jobUid"`
	Kind     string  `json:"kind"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Detail   string  `json:"detail,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// reloadRequest is sent from a rebuild job goroutine to the supervisor's
// reload loop once the shadow build is complete. The loop performs the swap
// (Shutdown → final delta-replay → close live store → rename → reopen) and
// signals the outcome back on done.
type reloadRequest struct {
	kind       rebuildKind
	jobUID     string
	shadowDir  string
	shadowDisp shadowStore
	done       chan error
}

// shadowStore is the subset of the dispatcher the swap path needs from the
// shadow build: a final delta-replay target and a closer. Kept as an interface
// so the swap logic and its tests do not depend on the concrete dispatcher.
type shadowStore interface {
	kernel.Storage
	WaitForBackgroundVec(ctx context.Context) error
}

// rebuildCoordinator serializes online rebuilds and holds the reload-loop
// channel + the authoritative status. Created once per daemon process.
type rebuildCoordinator struct {
	logger *zap.Logger
	// base parents every rebuild job goroutine; canceled on daemon exit so a
	// rebuild does not outlive the process.
	base     context.Context //nolint:containedctx // process-lifetime parent for detached rebuild jobs
	reloadCh chan reloadRequest

	mu      sync.Mutex
	running bool
	status  rebuildStatusView
}

// newRebuildCoordinator builds a coordinator parented on base. Base should be
// the supervisor's run context so a SIGTERM mid-build cancels the replay.
func newRebuildCoordinator(base context.Context, logger *zap.Logger) *rebuildCoordinator {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &rebuildCoordinator{
		logger:   logger,
		base:     base,
		reloadCh: make(chan reloadRequest),
		status:   rebuildStatusView{State: string(rebuildStateIdle)},
	}
}

// start claims the single rebuild slot and launches the online rebuild job in
// a detached goroutine. It returns the new job's UID and started=true on
// success, or ("", false) when a rebuild is already running (→ 409).
func (c *rebuildCoordinator) start(wired *Wired, dataDir string, kind rebuildKind, strict bool) (jobUID string, started bool) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return "", false
	}
	c.running = true
	jobUID = ulid.Make().String()
	c.status = rebuildStatusView{
		JobUID: jobUID,
		Kind:   string(kind),
		State:  string(rebuildStateBuilding),
		Detail: "building shadow projection",
	}
	c.mu.Unlock()

	go c.run(wired, dataDir, kind, strict, jobUID)
	return jobUID, true
}

// run executes one rebuild end to end and releases the slot. It is the
// goroutine body launched by start; the heavy lifting lives in
// runOnlineRebuild so the orchestration is unit-testable apart from the
// status bookkeeping here.
func (c *rebuildCoordinator) run(wired *Wired, dataDir string, kind rebuildKind, strict bool, jobUID string) {
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	err := runOnlineRebuild(c.base, c, wired, dataDir, kind, strict, jobUID)
	if err != nil {
		c.setFailed(jobUID, err)
		c.logger.Error("online rebuild failed", zap.String("kind", string(kind)), zap.Error(err))
		return
	}
	c.setSucceeded(jobUID)
	c.logger.Info("online rebuild succeeded", zap.String("kind", string(kind)), zap.String("jobUid", jobUID))
}

// statusView returns a snapshot of the current/last rebuild status.
func (c *rebuildCoordinator) statusView() rebuildStatusView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// setState updates the lifecycle phase + detail without touching progress.
func (c *rebuildCoordinator) setState(jobUID string, state rebuildState, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.JobUID != jobUID {
		return
	}
	c.status.State = string(state)
	c.status.Detail = detail
}

func (c *rebuildCoordinator) setSucceeded(jobUID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.JobUID != jobUID {
		return
	}
	c.status.State = string(rebuildStateSucceeded)
	c.status.Progress = 1
	c.status.Detail = "rebuild complete"
	c.status.Error = ""
}

func (c *rebuildCoordinator) setFailed(jobUID string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.JobUID != jobUID {
		return
	}
	c.status.State = string(rebuildStateFailed)
	c.status.Error = err.Error()
}

// Begin implements kernel.JobStarter so the shadow build's replay reports
// fractional progress straight into the coordinator status. The returned
// reporter's Done/Fail are intentionally inert: the coordinator owns terminal
// state (the swap has yet to run when the replay's defer fires).
func (c *rebuildCoordinator) Begin(ctx context.Context, _, _ string) (kernel.JobReporter, context.Context) {
	return &coordReporter{c: c, jobUID: c.statusView().JobUID}, ctx
}

// coordReporter forwards replay progress into the coordinator status.
type coordReporter struct {
	c      *rebuildCoordinator
	jobUID string
}

func (r *coordReporter) Update(progress float64, detail string) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if r.c.status.JobUID != r.jobUID {
		return
	}
	// Cap shadow-build progress below 1 so the terminal 1.0 is reserved for a
	// committed swap; a stalled or fast replay never reads as "done".
	if progress > 0.95 {
		progress = 0.95
	}
	r.c.status.Progress = progress
	if detail != "" {
		r.c.status.Detail = detail
	}
}

// Done is inert: see Begin. The coordinator marks success only after the swap.
func (r *coordReporter) Done() {}

// Fail is inert: the orchestration surfaces replay errors through its return
// value, which run() maps to setFailed.
func (r *coordReporter) Fail(_ error) {}

// reloadDeadline bounds how long the swap may block the serving stack (final
// delta-replay + embed drain + rename + reopen). Generous because a busy
// daemon may have accrued a multi-event delta whose episodes re-embed during
// the window; the JSONL is durable throughout, so a timeout aborts the swap
// and leaves the original projection serving.
const reloadDeadline = 5 * time.Minute
