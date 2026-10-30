package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// EnrichmentReporter reports the kernel's background enrichment (extraction
// + resolution) depth. Implemented by *kernel.FileEventLog in production.
// The interface keeps this tool free of an import cycle (kernel pulls in
// mcp via the daemon wiring, not the other way round).
type EnrichmentReporter interface {
	BackgroundWorkDepth() int64
	ExtractionQueueDepth() int
	WaitForBackgroundWork(ctx context.Context) error
}

// VecReporter reports the dispatcher's background embedding depth.
// Implemented by *dispatcher.Dispatcher in production.
type VecReporter interface {
	BackgroundVecDepth() int64
	WaitForBackgroundVec(ctx context.Context) error
}

// ---------- idleStatus ----------

const idleStatusInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const idleStatusOutputSchema = `{
  "type": "object",
  "properties": {
    "enrichmentQueueDepth": {"type": "integer", "minimum": 0, "description": "Jobs sitting in the bounded extraction worker pool, not yet picked up by a worker."},
    "enrichmentInflight":   {"type": "integer", "minimum": 0, "description": "Total background enrichment work in flight (queued + running). When 0 the extraction/resolution side is quiescent."},
    "vecInflight":          {"type": "integer", "minimum": 0, "description": "Background embedding+vec0-upsert goroutines currently running. When 0 the vector side is quiescent."},
    "isIdle":               {"type": "boolean", "description": "True when every background lane reports 0 in-flight work."}
  },
  "required": ["enrichmentQueueDepth", "enrichmentInflight", "vecInflight", "isIdle"],
  "additionalProperties": false
}`

type idleStatusOutput struct {
	EnrichmentQueueDepth int   `json:"enrichmentQueueDepth"`
	EnrichmentInflight   int64 `json:"enrichmentInflight"`
	VecInflight          int64 `json:"vecInflight"`
	IsIdle               bool  `json:"isIdle"`
}

// IdleStatus reports background-work counters so callers can poll
// progress between an ingestion burst and a recall. The eval harness
// uses it to separate ingestion wall-clock from recall wall-clock
// without conflating LLM extraction latency into the recall number.
type IdleStatus struct {
	enrich EnrichmentReporter
	vec    VecReporter
}

// NewIdleStatus constructs the tool. Either reporter may be nil; a nil
// reporter contributes zero to its lane (the tool reports "idle" for
// that lane, which matches the daemon's behavior when extraction or
// vec is disabled at construction time).
func NewIdleStatus(enrich EnrichmentReporter, vec VecReporter) *IdleStatus {
	return &IdleStatus{enrich: enrich, vec: vec}
}

// Name implements mcp.Tool.
func (*IdleStatus) Name() string { return "idleStatus" }

// Description implements mcp.Tool.
func (*IdleStatus) Description() string {
	return "Return current background-work counters: extraction queue depth, in-flight enrichment, in-flight embeddings. Use to poll whether the kernel has caught up after a burst of remember() calls, before calling recall() for a clean retrieval measurement."
}

// InputSchema implements mcp.Tool.
func (*IdleStatus) InputSchema() json.RawMessage { return json.RawMessage(idleStatusInputSchema) }

// OutputSchema implements mcp.Tool.
func (*IdleStatus) OutputSchema() json.RawMessage { return json.RawMessage(idleStatusOutputSchema) }

// Invoke implements mcp.Tool.
func (s *IdleStatus) Invoke(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	out := idleStatusOutput{}
	if s.enrich != nil {
		out.EnrichmentQueueDepth = s.enrich.ExtractionQueueDepth()
		out.EnrichmentInflight = s.enrich.BackgroundWorkDepth()
	}
	if s.vec != nil {
		out.VecInflight = s.vec.BackgroundVecDepth()
	}
	out.IsIdle = out.EnrichmentInflight == 0 && out.VecInflight == 0
	body, err := json.Marshal(out)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", err))
	}
	return body, nil
}

// ---------- waitForIdle ----------

const waitForIdleInputSchema = `{
  "type": "object",
  "properties": {
    "timeoutSeconds": {"type": "integer", "minimum": 1, "maximum": 3600, "description": "Maximum seconds to wait. Defaults to 300. The tool returns timedOut:true rather than erroring when the budget expires."}
  },
  "additionalProperties": false
}`

const waitForIdleOutputSchema = `{
  "type": "object",
  "properties": {
    "isIdle":      {"type": "boolean", "description": "True when both enrichment and vec drained within the budget; false if timedOut."},
    "timedOut":    {"type": "boolean", "description": "True when the timeoutSeconds budget expired before the kernel went idle."},
    "elapsedMs":   {"type": "integer", "minimum": 0, "description": "Wall-clock spent waiting, in milliseconds."},
    "enrichmentInflight": {"type": "integer", "minimum": 0, "description": "Enrichment depth observed at return time (0 unless timedOut)."},
    "vecInflight":  {"type": "integer", "minimum": 0, "description": "Vec depth observed at return time (0 unless timedOut)."}
  },
  "required": ["isIdle", "timedOut", "elapsedMs", "enrichmentInflight", "vecInflight"],
  "additionalProperties": false
}`

type waitForIdleInput struct {
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

type waitForIdleOutput struct {
	IsIdle             bool  `json:"isIdle"`
	TimedOut           bool  `json:"timedOut"`
	ElapsedMs          int64 `json:"elapsedMs"`
	EnrichmentInflight int64 `json:"enrichmentInflight"`
	VecInflight        int64 `json:"vecInflight"`
}

// defaultWaitForIdleTimeout matches the recall path's drain budget so a
// caller who calls waitForIdle then recall observes consistent timeouts.
// 300s absorbs the LongMemEval haystack drain (~50 sessions * ~3s
// extraction each, fanned across 4 workers).
const defaultWaitForIdleTimeout = 300 * time.Second

// WaitForIdle blocks until both the kernel's enrichment lane and the
// dispatcher's vec lane drain, or timeoutSeconds elapses. Composes the
// two existing internal drain primitives (FileEventLog.WaitForBackgroundWork
// and Dispatcher.WaitForBackgroundVec) so a caller doesn't need separate
// round-trips per lane.
//
// Drain order matters: enrichment first because extraction may schedule
// fresh vec embeddings inside its applyExtraction path; if we waited on
// vec first, we could observe "vec idle" only to have enrichment add
// more vec work a moment later. Drain enrichment first, then vec — once
// enrichment is idle the vec queue is monotonically draining.
type WaitForIdle struct {
	enrich EnrichmentReporter
	vec    VecReporter
}

// NewWaitForIdle constructs the tool. Either reporter may be nil; a
// nil reporter is treated as "already idle" for its lane.
func NewWaitForIdle(enrich EnrichmentReporter, vec VecReporter) *WaitForIdle {
	return &WaitForIdle{enrich: enrich, vec: vec}
}

// Name implements mcp.Tool.
func (*WaitForIdle) Name() string { return "waitForIdle" }

// Description implements mcp.Tool.
func (*WaitForIdle) Description() string {
	return "Block until the kernel finishes all background extraction, resolution, and embedding work (or the timeout expires). Use between a burst of remember() calls and a benchmark recall() so the recall measures retrieval latency only, not the tail of ingestion. Cheap when the kernel is already idle."
}

// InputSchema implements mcp.Tool.
func (*WaitForIdle) InputSchema() json.RawMessage { return json.RawMessage(waitForIdleInputSchema) }

// OutputSchema implements mcp.Tool.
func (*WaitForIdle) OutputSchema() json.RawMessage { return json.RawMessage(waitForIdleOutputSchema) }

// SelfTiming implements mcp.SelfTimingTool: waitForIdle owns its own budget
// (the caller-supplied timeoutSeconds, up to an hour) and reports timedOut
// rather than erroring, so it must not be truncated by the adapter's default
// per-invocation deadline.
func (*WaitForIdle) SelfTiming() bool { return true }

// drainLane wraps a single drain primitive: nil return = drained, true
// = timeout/cancel observed (caller treats as soft signal), error = a
// non-deadline failure to surface as E_INTERNAL.
func drainLane(ctx context.Context, waitFn func(context.Context) error, label string) (timedOut bool, err error) {
	if waitFn == nil {
		return false, nil
	}
	if e := waitFn(ctx); e != nil {
		if errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled) {
			return true, nil
		}
		return false, mcp.NewInternalError(fmt.Errorf("wait %s: %w", label, e))
	}
	return false, nil
}

// Invoke implements mcp.Tool.
func (w *WaitForIdle) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in waitForIdleInput
	if len(req) > 0 {
		if err := json.Unmarshal(req, &in); err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("decode arguments: %w", err))
		}
	}
	timeout := defaultWaitForIdleTimeout
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	var enrichWait func(context.Context) error
	if w.enrich != nil {
		enrichWait = w.enrich.WaitForBackgroundWork
	}
	timedOut, err := drainLane(waitCtx, enrichWait, "enrichment")
	if err != nil {
		return nil, err
	}
	if !timedOut {
		var vecWait func(context.Context) error
		if w.vec != nil {
			vecWait = w.vec.WaitForBackgroundVec
		}
		var verr error
		timedOut, verr = drainLane(waitCtx, vecWait, "vec")
		if verr != nil {
			return nil, verr
		}
	}

	out := waitForIdleOutput{
		ElapsedMs: time.Since(start).Milliseconds(),
		TimedOut:  timedOut,
	}
	if w.enrich != nil {
		out.EnrichmentInflight = w.enrich.BackgroundWorkDepth()
	}
	if w.vec != nil {
		out.VecInflight = w.vec.BackgroundVecDepth()
	}
	out.IsIdle = !timedOut && out.EnrichmentInflight == 0 && out.VecInflight == 0

	body, mErr := json.Marshal(out)
	if mErr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", mErr))
	}
	return body, nil
}
