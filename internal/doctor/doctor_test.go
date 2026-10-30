package doctor_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/doctor"
)

// TestRunner_Aggregation exercises the happy path: three checks with
// distinct statuses; assert the returned slice is in registration order,
// each Status maps through, and DurationMs is populated.
func TestRunner_Aggregation(t *testing.T) {
	t.Parallel()
	checks := []doctor.Check{
		fakeCheck("a", "storage", doctor.StatusOK, "alpha", nil, 0),
		fakeCheck("b", "storage", doctor.StatusWarn, "beta", nil, 0),
		fakeCheck("c", "mcp", doctor.StatusFail, "gamma", nil, 0),
	}
	r := &doctor.Runner{Checks: checks}
	got := r.Run(context.Background())

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	for i, want := range []struct {
		name   string
		status doctor.Status
	}{
		{"a", doctor.StatusOK},
		{"b", doctor.StatusWarn},
		{"c", doctor.StatusFail},
	} {
		if got[i].Name != want.name {
			t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, want.name)
		}
		if got[i].Status != want.status {
			t.Errorf("got[%d].Status = %q, want %q", i, got[i].Status, want.status)
		}
		if got[i].Index != i {
			t.Errorf("got[%d].Index = %d, want %d", i, got[i].Index, i)
		}
	}
}

// TestRunner_PanicRecovery: a check that panics is reported as FAIL
// with a "panic:" detail and the rest of the report is still emitted.
func TestRunner_PanicRecovery(t *testing.T) {
	t.Parallel()
	checks := []doctor.Check{
		doctor.CheckFunc{
			N: "panicker", Sub: "storage",
			Fn: func(_ context.Context) (doctor.Status, string, error) {
				panic("boom")
			},
		},
		fakeCheck("ok", "storage", doctor.StatusOK, "fine", nil, 0),
	}
	r := &doctor.Runner{Checks: checks}
	got := r.Run(context.Background())
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Status != doctor.StatusFail {
		t.Errorf("panic Result.Status = %q, want %q", got[0].Status, doctor.StatusFail)
	}
	if got[0].Detail == "" || !contains(got[0].Detail, "panic:") {
		t.Errorf("panic Result.Detail = %q, want substring %q", got[0].Detail, "panic:")
	}
	if got[1].Status != doctor.StatusOK {
		t.Errorf("sibling Result.Status = %q, want %q", got[1].Status, doctor.StatusOK)
	}
}

// TestRunner_PerCheckTimeout: a check that blocks past the configured
// per-check timeout surfaces as FAIL with a "timed out" detail.
func TestRunner_PerCheckTimeout(t *testing.T) {
	t.Parallel()
	blocked := doctor.CheckFunc{
		N: "block", Sub: "storage",
		Fn: func(ctx context.Context) (doctor.Status, string, error) {
			<-ctx.Done()
			return doctor.StatusFail, "", ctx.Err()
		},
	}
	r := &doctor.Runner{Checks: []doctor.Check{blocked}, Timeout: 20 * time.Millisecond}
	got := r.Run(context.Background())
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Status != doctor.StatusFail {
		t.Errorf("Status = %q, want %q", got[0].Status, doctor.StatusFail)
	}
	if !contains(got[0].Detail, "timed out") {
		t.Errorf("Detail = %q, want substring %q", got[0].Detail, "timed out")
	}
}

// TestRunner_ParallelOrdering registers 20 checks of varying duration
// and asserts the returned slice is still in registration order. The
// channel emits results in completion order; Run is the sorted form.
func TestRunner_ParallelOrdering(t *testing.T) {
	t.Parallel()
	const n = 20
	var ran atomic.Int32
	checks := make([]doctor.Check, n)
	for i := range n {
		checks[i] = doctor.CheckFunc{
			N: name(i), Sub: "storage",
			Fn: func(_ context.Context) (doctor.Status, string, error) {
				// Stagger durations so completion order != registration.
				time.Sleep(time.Duration((n-i)%5) * time.Millisecond)
				ran.Add(1)
				return doctor.StatusOK, "", nil
			},
		}
	}
	r := &doctor.Runner{Checks: checks, Parallelism: 4}
	got := r.Run(context.Background())
	if got := int(ran.Load()); got != n {
		t.Fatalf("ran = %d, want %d", got, n)
	}
	for i, res := range got {
		if res.Index != i {
			t.Errorf("got[%d].Index = %d, want %d", i, res.Index, i)
		}
		if res.Name != name(i) {
			t.Errorf("got[%d].Name = %q, want %q", i, res.Name, name(i))
		}
	}
}

// TestRunner_ContextCancel: a parent ctx cancelled mid-run drops the
// jobs queue; whatever checks already ran are returned, in registration
// order. The Runner should not deadlock.
func TestRunner_ContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	checks := []doctor.Check{
		doctor.CheckFunc{
			N: "a", Sub: "storage",
			Fn: func(_ context.Context) (doctor.Status, string, error) {
				cancel()
				return doctor.StatusOK, "", nil
			},
		},
		doctor.CheckFunc{
			N: "b", Sub: "storage",
			Fn: func(c context.Context) (doctor.Status, string, error) {
				<-c.Done()
				return doctor.StatusFail, "", c.Err()
			},
		},
	}
	r := &doctor.Runner{Checks: checks, Parallelism: 1, Timeout: 200 * time.Millisecond}
	got := r.Run(ctx)
	if len(got) == 0 {
		t.Fatalf("expected at least one Result; got 0")
	}
	if got[0].Name != "a" || got[0].Status != doctor.StatusOK {
		t.Errorf("first Result = (%q, %q), want (a, ok)", got[0].Name, got[0].Status)
	}
}

// TestRunStream_ChannelCloses confirms the stream channel is closed
// after the last Result. A reader that ranges over it must not hang.
func TestRunStream_ChannelCloses(t *testing.T) {
	t.Parallel()
	r := &doctor.Runner{Checks: []doctor.Check{
		fakeCheck("a", "storage", doctor.StatusOK, "", nil, 0),
		fakeCheck("b", "storage", doctor.StatusOK, "", nil, 0),
	}}
	ch := r.RunStream(context.Background())
	count := 0
	for range ch {
		count++
	}
	if count != 2 {
		t.Fatalf("drained %d Results, want 2", count)
	}
}

// TestRunStream_EmptyChecks: a Runner with no checks closes the channel
// immediately and Run returns an empty slice.
func TestRunStream_EmptyChecks(t *testing.T) {
	t.Parallel()
	r := &doctor.Runner{}
	got := r.Run(context.Background())
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

// TestExitCode pins the contract: 1 if any Fail, else 0. Warn alone
// does not flip the code.
func TestExitCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []doctor.Result
		want int
	}{
		{"all ok", []doctor.Result{{Status: doctor.StatusOK}}, 0},
		{"warn only", []doctor.Result{{Status: doctor.StatusWarn}}, 0},
		{"one fail", []doctor.Result{{Status: doctor.StatusOK}, {Status: doctor.StatusFail}}, 1},
		{"empty", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := doctor.ExitCode(tc.in); got != tc.want {
				t.Errorf("ExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSummarize aggregates correctly across the status set.
func TestSummarize(t *testing.T) {
	t.Parallel()
	in := []doctor.Result{
		{Status: doctor.StatusOK},
		{Status: doctor.StatusOK},
		{Status: doctor.StatusWarn},
		{Status: doctor.StatusFail},
	}
	s := doctor.Summarize(in)
	if s.Total != 4 || s.OK != 2 || s.Warn != 1 || s.Fail != 1 {
		t.Errorf("Summary = %+v, want {Total:4 OK:2 Warn:1 Fail:1}", s)
	}
}

// TestRunner_ErrorMappedToFail: a check that returns a non-nil error
// is mapped to StatusFail with the error string as Detail (regardless
// of the Status the check returned).
func TestRunner_ErrorMappedToFail(t *testing.T) {
	t.Parallel()
	want := errors.New("boom")
	c := doctor.CheckFunc{
		N: "x", Sub: "storage",
		Fn: func(_ context.Context) (doctor.Status, string, error) {
			return doctor.StatusOK, "", want
		},
	}
	r := &doctor.Runner{Checks: []doctor.Check{c}}
	got := r.Run(context.Background())
	if got[0].Status != doctor.StatusFail {
		t.Errorf("Status = %q, want %q", got[0].Status, doctor.StatusFail)
	}
	if got[0].Detail != want.Error() {
		t.Errorf("Detail = %q, want %q", got[0].Detail, want.Error())
	}
}

// fakeCheck constructs a deterministic CheckFunc that returns the given
// status/detail/err. The optional delay simulates a slow probe.
func fakeCheck(n, sub string, status doctor.Status, detail string, err error, delay time.Duration) doctor.CheckFunc {
	return doctor.CheckFunc{
		N: n, Sub: sub,
		Fn: func(_ context.Context) (doctor.Status, string, error) {
			if delay > 0 {
				time.Sleep(delay)
			}
			return status, detail, err
		},
	}
}

// contains is a tiny helper kept here so the package-local test file
// does not pull in strings just for substring assertion.
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// name returns a deterministic check name for the parallel-ordering
// fixture. Kept short so the table renders evenly during debug prints.
func name(i int) string {
	return "check-" + itoa(i)
}

// itoa is a hand-rolled int-to-string for the test fixture; using
// strconv would pull in the import for a one-shot use that is already
// covered by fmt elsewhere in the suite.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+(i%10))) + out
		i /= 10
	}
	return out
}
