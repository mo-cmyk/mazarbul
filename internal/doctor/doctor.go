// Package doctor runs Mazarbul health checks across storage, MCP, providers,
// and logging, and reports OK/WARN/FAIL per check. It is consumed by the
// `maz doctor` CLI command and (eventually) by an MCP tool surface; keeping
// the runner here, separate from cobra and the TUI, lets agent callers
// invoke Run without dragging in either dependency.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Status is the outcome of a single Check.
type Status string

const (
	// StatusOK means the check passed; no action required.
	StatusOK Status = "ok"
	// StatusWarn means the check passed with a caveat that is not fatal
	// (e.g., MCP daemon not running but the socket file is absent rather
	// than EACCES). Exit code is still 0.
	StatusWarn Status = "warn"
	// StatusFail means the check failed in an actionable way. Any FAIL
	// in the final report flips the process exit code to 1.
	StatusFail Status = "fail"
)

// Result is the per-check report. JSON shape matches issue #52: lowerCamelCase
// field names per AIP-122, durationMs as an unsigned millisecond count.
type Result struct {
	// Index is the position the check held in Runner.Checks at construction
	// time. Used by the live TUI to write completed rows into a stable slot;
	// json:"-" because the wire shape does not surface registration order.
	Index int `json:"-"`

	// Name is the check identifier (e.g. "sqlite.open", "mcp.healthz").
	Name string `json:"name"`

	// Subsystem groups checks for table rendering ("storage", "mcp",
	// "provider", "logging").
	Subsystem string `json:"subsystem"`

	// Status is one of StatusOK, StatusWarn, StatusFail.
	Status Status `json:"status"`

	// Detail is a short human-readable message about the result. May be
	// empty on a successful check; on a failure, carries the wrapped error
	// or the actionable hint (e.g. "ollama pull bge-m3").
	Detail string `json:"detail"`

	// DurationMs is the wall-clock time spent inside Check.Run, in
	// milliseconds. Wraps at uint64 because durations are non-negative.
	DurationMs uint64 `json:"durationMs"`
}

// Check is one named probe. Implementations must be safe to invoke under
// a per-check context.WithTimeout and must not panic; the Runner recovers
// panics and maps them to StatusFail, but a clean Check returns an error
// rather than relying on that net.
type Check interface {
	// Name returns the dotted lowercase identifier ("sqlite.open").
	Name() string

	// Subsystem returns the rendering group ("storage").
	Subsystem() string

	// Run executes the probe. A non-nil error is mapped to StatusFail
	// with the error's string as Detail. Returning (StatusOK, "", nil) is
	// the happy path; (StatusWarn, "<reason>", nil) is a caveat.
	Run(ctx context.Context) (status Status, detail string, err error)
}

// CheckFunc is a function adapter for Check. Useful for inline check
// construction in tests and for the per-check constructors in checks.go.
type CheckFunc struct {
	N   string
	Sub string
	Fn  func(ctx context.Context) (Status, string, error)

	// Rem is the optional self-repair descriptor. Nil (the zero value)
	// marks the check non-remediable: `maz doctor fix` prints its Detail as
	// manual guidance instead of offering a fix. A check that needs more
	// state than a closure can hold (an open writer pool, a dispatcher)
	// implements Remediator on a concrete type instead of using this field.
	Rem *Remediation
}

// Name returns the check identifier.
func (c CheckFunc) Name() string { return c.N }

// Subsystem returns the rendering group.
func (c CheckFunc) Subsystem() string { return c.Sub }

// Run delegates to the wrapped function.
func (c CheckFunc) Run(ctx context.Context) (Status, string, error) {
	return c.Fn(ctx)
}

// Remediation returns the optional self-repair descriptor (nil when Rem is
// unset), satisfying Remediator so `maz doctor fix` can offer the fix.
func (c CheckFunc) Remediation() *Remediation { return c.Rem }

// Remediation describes how a failing Check repairs itself. A Check that can
// fix its own failure returns a non-nil *Remediation from Remediator; any
// Check that does not implement Remediator, or returns nil, is non-remediable
// and `maz doctor fix` prints its Detail as manual guidance.
type Remediation struct {
	// Description is the imperative one-liner shown in the per-check confirm
	// prompt, e.g. "link the local self anchor to the bound principal".
	Description string

	// Safe reports whether `maz doctor fix --yes` may apply this remediation
	// without an interactive confirm. A safe remediation MUST be idempotent
	// and scope-safe: append-only, no destructive rewrite, and never writes
	// outside the personal scope.
	//
	// Migration, audit, and permission remediations are NEVER safe: leave
	// Safe false (or, preferably, expose no Remediation at all). The fix
	// command enforces this: an unsafe remediation is skipped under --yes and
	// only ever runs after an explicit interactive confirm.
	Safe bool

	// Apply performs the fix under ctx. It MUST be idempotent: applying it
	// when the check already passes is a no-op, and applying it twice is
	// equivalent to once. A non-nil error leaves the failure in place for the
	// next `maz doctor` run to re-report.
	Apply func(ctx context.Context) error
}

// Remediator is the optional interface a Check implements when it can repair
// its own failure. It is deliberately separate from Check so the read-only
// `maz doctor` path never depends on it; only `maz doctor fix` consults it.
type Remediator interface {
	// Remediation returns the fix descriptor for this check, or nil when the
	// check offers no automatic repair.
	Remediation() *Remediation
}

// RemediationOf returns the Remediation a Check offers, or nil when the check
// is non-remediable (does not implement Remediator, or returns nil).
// Centralised so the CLI fix path and tests agree on the lookup.
func RemediationOf(c Check) *Remediation {
	r, ok := c.(Remediator)
	if !ok {
		return nil
	}
	return r.Remediation()
}

// DefaultTimeout is the per-check budget when Runner.Timeout is zero. Each
// check runs under context.WithTimeout(ctx, DefaultTimeout); a hung network
// call therefore surfaces as a FAIL within the budget rather than wedging
// the report. 5 seconds matches the Anthropic ping's typical p99.
const DefaultTimeout = 5 * time.Second

// Runner fans Checks out to a bounded goroutine pool and gathers the
// results. The zero value is not usable; populate Checks before calling
// Run or RunStream.
type Runner struct {
	// Checks is the registered probe list. Run and RunStream both honour
	// the registration order in the returned slice (Run sorts; RunStream
	// emits in completion order but tags each Result with Index).
	Checks []Check

	// Parallelism caps the number of in-flight checks. Zero means
	// runtime.NumCPU().
	Parallelism int

	// Clock supplies the timing source. Zero means time.Now. Tests inject
	// a fake clock for deterministic DurationMs assertions.
	Clock func() time.Time

	// Timeout caps each check. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Run blocks until every check finishes (or its per-check timeout fires).
// Returned slice is in registration order; durations are populated. Run
// is a thin wrapper over RunStream that collects every emitted Result.
func (r *Runner) Run(ctx context.Context) []Result {
	ch := r.RunStream(ctx)
	out := make([]Result, 0, len(r.Checks))
	for res := range ch {
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// CheckList returns a shallow copy of the registered Check slice in
// registration order. The TUI consumes this to pre-allocate one row per
// check before any Result arrives. A copy (not the slice header) is
// returned so a caller mutating the returned slice cannot reshape the
// Runner's order between consecutive Run/RunStream calls.
func (r *Runner) CheckList() []Check {
	out := make([]Check, len(r.Checks))
	copy(out, r.Checks)
	return out
}

// RunStream emits one Result per completed check, in completion order
// (so the live TUI can flip pending rows to final as they land). The
// channel is closed after the last Result. Each emitted Result carries
// its registration Index so the caller can place it in a stable row.
func (r *Runner) RunStream(ctx context.Context) <-chan Result {
	clock := r.Clock
	if clock == nil {
		clock = time.Now
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	parallelism := r.Parallelism
	if parallelism <= 0 {
		parallelism = runtime.NumCPU()
	}
	if parallelism > len(r.Checks) && len(r.Checks) > 0 {
		parallelism = len(r.Checks)
	}

	out := make(chan Result, len(r.Checks))

	if len(r.Checks) == 0 {
		close(out)
		return out
	}

	type job struct {
		index int
		check Check
	}
	jobs := make(chan job)

	var wg sync.WaitGroup
	wg.Add(parallelism)
	for range parallelism {
		go func() {
			defer wg.Done()
			for j := range jobs {
				out <- runOne(ctx, j.check, j.index, timeout, clock)
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i, c := range r.Checks {
			select {
			case <-ctx.Done():
				return
			case jobs <- job{index: i, check: c}:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

// runOne executes a single Check under a per-call timeout and recovers
// from any panic. The duration covers the full call window including the
// timeout; the Result.Index is set so consumers can place it in a row.
func runOne(parent context.Context, c Check, index int, timeout time.Duration, clock func() time.Time) (res Result) {
	res = Result{
		Index:     index,
		Name:      c.Name(),
		Subsystem: c.Subsystem(),
	}
	start := clock()
	defer func() {
		res.DurationMs = uint64(clock().Sub(start) / time.Millisecond)
	}()
	defer func() {
		if r := recover(); r != nil {
			res.Status = StatusFail
			res.Detail = fmt.Sprintf("panic: %v", r)
		}
	}()

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	status, detail, err := c.Run(ctx)
	switch {
	case err != nil:
		res.Status = StatusFail
		res.Detail = errDetail(err)
	default:
		res.Status = status
		res.Detail = detail
	}
	return res
}

// errDetail formats an error for a Result.Detail field, surfacing the
// underlying ctx.Err when the failure was a per-check timeout so the
// caller can distinguish "doctor budget blew" from "service returned
// an error".
func errDetail(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out: " + err.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "ctx canceled: " + err.Error()
	}
	return err.Error()
}

// ExitCode returns 1 if any Result has StatusFail, else 0. The CLI layer
// returns this value to os.Exit; keeping the helper here means the rule
// is unit-tested once and reused identically by the TUI and CLI paths.
func ExitCode(results []Result) int {
	for _, r := range results {
		if r.Status == StatusFail {
			return 1
		}
	}
	return 0
}

// Summary aggregates the result slice into counts for the trailing
// "summary: N checks · X ok · Y warn · Z fail" line.
type Summary struct {
	Total int
	OK    int
	Warn  int
	Fail  int
}

// Summarize counts the results by status. Total is len(results).
func Summarize(results []Result) Summary {
	s := Summary{Total: len(results)}
	for _, r := range results {
		switch r.Status {
		case StatusOK:
			s.OK++
		case StatusWarn:
			s.Warn++
		case StatusFail:
			s.Fail++
		}
	}
	return s
}
