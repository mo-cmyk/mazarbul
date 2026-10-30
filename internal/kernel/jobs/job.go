// Package jobs holds Mazarbul's in-process background-job registry.
// The Jobs hub panel (M6) and the listJobs/cancelJob MCP tools both
// read through Registry; producers (rebuild, ingest connectors,
// extractor) report progress through Tracker.
package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// maxHistoryAge is the default window LoadHistory returns on registry init.
const maxHistoryAge = 24 * time.Hour

// State is the lifecycle phase of a Job.
type State string

const (
	// StateRunning is in-flight; the producer is updating progress.
	StateRunning State = "running"
	// StateSucceeded completed normally.
	StateSucceeded State = "succeeded"
	// StateFailed completed with an error.
	StateFailed State = "failed"
	// StateCanceled was canceled by an external caller (UI / MCP).
	StateCanceled State = "canceled"
)

// Job is a single tracked unit of background work.
type Job struct {
	UID       string    `json:"uid"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	State     State     `json:"state"`
	Progress  float64   `json:"progress"`
	Detail    string    `json:"detail,omitempty"`
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime,omitempty"`
	ErrMsg    string    `json:"errMsg,omitempty"`
}

// persistOp is the unit enqueued to the writer goroutine.
type persistOp struct {
	job Job
}

// Registry is the in-memory job catalog. Safe for concurrent access.
type Registry struct {
	mu      sync.RWMutex
	jobs    map[string]*Job
	cancels map[string]context.CancelFunc

	// dataDir is set when persistence is enabled (NewRegistryWithStore).
	dataDir string
	// persist is non-nil when persistence is enabled. The writer goroutine
	// drains it; callers enqueue via enqueuePersist.
	persist chan persistOp
	// persistMu guards persistClosed and the close of persist against
	// concurrent enqueuePersist sends. Background enrichment goroutines call
	// enqueuePersist (via rep.Update / rep.Done / rep.Fail) and can still be
	// in flight when Close() runs at shutdown; without this guard the send
	// races the close and panics with "send on closed channel", which —
	// before the eventlog's panic recovery — took down the whole daemon.
	persistMu     sync.RWMutex
	persistClosed bool
	// wg tracks the writer goroutine lifecycle for Close.
	wg sync.WaitGroup
}

// NewRegistry returns an empty in-memory Registry with no persistence.
func NewRegistry() *Registry {
	return &Registry{
		jobs:    map[string]*Job{},
		cancels: map[string]context.CancelFunc{},
	}
}

// NewRegistryWithStore returns a Registry that persists job state
// transitions to $dataDir/jobs.jsonl via a background writer goroutine.
// It replays the last 24 h of records into in-memory state on startup.
// Call Close to flush and stop the goroutine.
func NewRegistryWithStore(dataDir string) *Registry {
	r := &Registry{
		jobs:    map[string]*Job{},
		cancels: map[string]context.CancelFunc{},
		dataDir: dataDir,
		persist: make(chan persistOp, 64),
	}
	// Replay recent history so the panel is populated after a restart.
	if hist, err := LoadHistory(dataDir, maxHistoryAge); err == nil {
		for i := range hist {
			j := hist[i]
			r.jobs[j.UID] = &j
		}
	}
	r.wg.Add(1)
	go r.writeLoop()
	return r
}

// Close flushes any pending persist operations and stops the writer goroutine.
// Safe to call on a registry created with NewRegistry (no-op).
func (r *Registry) Close() {
	if r.persist == nil {
		return
	}
	// Take the exclusive lock so no enqueuePersist send is mid-flight when we
	// close the channel; persistClosed makes Close idempotent and tells late
	// senders to drop their snapshot instead of panicking on the closed channel.
	r.persistMu.Lock()
	if r.persistClosed {
		r.persistMu.Unlock()
		return
	}
	r.persistClosed = true
	close(r.persist)
	r.persistMu.Unlock()
	r.wg.Wait()
}

// writeLoop drains persistOp entries and appends them to jobs.jsonl.
func (r *Registry) writeLoop() {
	defer r.wg.Done()
	path := filepath.Join(r.dataDir, "jobs.jsonl")
	for op := range r.persist {
		appendJobRecord(path, op.job)
	}
}

// appendJobRecord appends a single JSON line to path. Write errors are
// silently ignored: persistence is best-effort observability.
func appendJobRecord(path string, j Job) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path is constructed from controlled dataDir + constant filename
	if err != nil {
		return
	}
	defer f.Close()
	line, err := json.Marshal(j)
	if err != nil {
		return
	}
	line = append(line, '\n')
	_, _ = f.Write(line) //nolint:errcheck // best-effort persistence sink
}

// enqueuePersist sends a snapshot of j to the persist channel without
// blocking. Called under r.mu, so j is copied before the send.
func (r *Registry) enqueuePersist(j Job) {
	if r.persist == nil {
		return
	}
	// RLock pairs with Close's exclusive Lock so a send can never overlap the
	// channel close. persistClosed drops snapshots that arrive after shutdown:
	// persistence is best-effort observability, so losing a final transition is
	// preferable to a "send on closed channel" panic on a background goroutine.
	r.persistMu.RLock()
	defer r.persistMu.RUnlock()
	if r.persistClosed {
		return
	}
	select {
	case r.persist <- persistOp{job: j}:
	default:
		// Channel full: drop the transition rather than blocking the hot path.
	}
}

// Begin starts tracking a new job with the given kind and title.
// Returns a Tracker the caller uses to report progress + completion.
// The returned context is canceled when Cancel(uid) is called.
func (r *Registry) Begin(ctx context.Context, kind, title string) (*Tracker, context.Context) {
	uid := ulid.Make().String()
	jobCtx, cancel := context.WithCancel(ctx)
	job := &Job{
		UID:       uid,
		Kind:      kind,
		Title:     title,
		State:     StateRunning,
		StartTime: time.Now().UTC(),
	}
	r.mu.Lock()
	r.jobs[uid] = job
	r.cancels[uid] = cancel
	r.enqueuePersist(*job)
	r.mu.Unlock()
	return &Tracker{reg: r, uid: uid}, jobCtx
}

// AsStarter returns a kernel.JobStarter adapter backed by this registry.
// Pass the result to kernel.WithJobReporter so the rebuild loop can
// report progress through the jobs panel without importing this package
// from the kernel package.
func (r *Registry) AsStarter() kernel.JobStarter {
	return &registryStarter{r: r}
}

// registryStarter adapts *Registry to kernel.JobStarter.
type registryStarter struct{ r *Registry }

// Begin implements kernel.JobStarter. *Tracker satisfies kernel.JobReporter
// because it has Update, Done, and Fail with matching signatures.
func (s *registryStarter) Begin(ctx context.Context, kind, title string) (kernel.JobReporter, context.Context) {
	t, jobCtx := s.r.Begin(ctx, kind, title)
	return t, jobCtx
}

// List returns a snapshot of every tracked job, newest first.
func (r *Registry) List() []Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, *j)
	}
	// Newest StartTime first — keeps the running ones at the top.
	for i := range len(out) - 1 {
		for j := i + 1; j < len(out); j++ {
			if out[j].StartTime.After(out[i].StartTime) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Get returns a single job snapshot by UID.
func (r *Registry) Get(uid string) (Job, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	j, ok := r.jobs[uid]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Cancel triggers cancellation of the named job. The job's context
// receives the cancel signal; the tracker observes it and marks the
// job as Canceled.
func (r *Registry) Cancel(uid string) bool {
	r.mu.Lock()
	cancel, ok := r.cancels[uid]
	if ok {
		delete(r.cancels, uid)
	}
	r.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Tracker is the producer-side handle: Update / Done / Fail.
type Tracker struct {
	reg *Registry
	uid string
}

// UID returns the job's identifier.
func (t *Tracker) UID() string { return t.uid }

// Update records progress (0..1) and an optional detail message.
// Also enqueues a persist snapshot when persistence is enabled.
func (t *Tracker) Update(progress float64, detail string) {
	t.reg.mu.Lock()
	defer t.reg.mu.Unlock()
	j, ok := t.reg.jobs[t.uid]
	if !ok {
		return
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	j.Progress = progress
	if detail != "" {
		j.Detail = detail
	}
	t.reg.enqueuePersist(*j)
}

// Done marks the job succeeded.
func (t *Tracker) Done() {
	t.reg.complete(t.uid, StateSucceeded, "")
}

// Fail marks the job failed with the given error message.
func (t *Tracker) Fail(err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	t.reg.complete(t.uid, StateFailed, msg)
}

// CancelDone marks the job canceled. Called by the producer when its
// loop notices the context was canceled.
func (t *Tracker) CancelDone() {
	t.reg.complete(t.uid, StateCanceled, "")
}

func (r *Registry) complete(uid string, state State, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[uid]
	if !ok {
		return
	}
	j.State = state
	j.EndTime = time.Now().UTC()
	if errMsg != "" {
		j.ErrMsg = errMsg
	}
	if state == StateSucceeded {
		j.Progress = 1
	}
	if cancel, ok := r.cancels[uid]; ok {
		cancel()
		delete(r.cancels, uid)
	}
	r.enqueuePersist(*j)
}
