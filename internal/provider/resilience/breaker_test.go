package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic breaker tests.
type fakeClock struct {
	current time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{current: time.Unix(1_000_000, 0).UTC()}
}

func (fc *fakeClock) now() time.Time { return fc.current }

func (fc *fakeClock) advance(d time.Duration) { fc.current = fc.current.Add(d) }

// newTestBreaker returns a CircuitBreaker wired to fc with the given thresholds.
func newTestBreaker(fc *fakeClock, maxFailures int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		MaxConsecutiveFailures: maxFailures,
		Cooldown:               cooldown,
		Now:                    fc.now,
	}
}

func TestCircuitBreakerOpensAfterExactlyNFailures(t *testing.T) {
	fc := newFakeClock()
	cb := newTestBreaker(fc, 3, 10*time.Second)

	// Two failures: still closed.
	cb.RecordFailure()
	cb.RecordFailure()
	if got := cb.State(); got != StateClosed {
		t.Fatalf("after 2 failures want StateClosed, got %s", got)
	}
	if !cb.Allow() {
		t.Fatal("Allow should return true while Closed")
	}

	// Third failure: trips open.
	cb.RecordFailure()
	if got := cb.State(); got != StateOpen {
		t.Fatalf("after 3 failures want StateOpen, got %s", got)
	}
}

func TestCircuitBreakerSuccessMidStreakResetsCount(t *testing.T) {
	fc := newFakeClock()
	cb := newTestBreaker(fc, 3, 10*time.Second)

	cb.RecordFailure()
	cb.RecordFailure()
	cb.RecordSuccess() // resets streak

	// Two more failures: streak starts from zero, should still be closed.
	cb.RecordFailure()
	cb.RecordFailure()
	if got := cb.State(); got != StateClosed {
		t.Fatalf("want StateClosed after reset + 2 failures, got %s", got)
	}

	// One more: now at 3 → opens.
	cb.RecordFailure()
	if got := cb.State(); got != StateOpen {
		t.Fatalf("want StateOpen after 3 post-reset failures, got %s", got)
	}
}

func TestCircuitBreakerOpenRejectsViaExecute(t *testing.T) {
	fc := newFakeClock()
	cb := newTestBreaker(fc, 1, 10*time.Second)

	cb.RecordFailure() // trips open immediately

	called := false
	err := cb.Execute(context.Background(), func(_ context.Context) error {
		called = true
		return nil
	})

	if !errors.Is(err, ErrOpen) {
		t.Fatalf("Execute on open breaker: want ErrOpen, got %v", err)
	}
	if called {
		t.Fatal("fn must not be called when breaker is open")
	}
}

func TestCircuitBreakerCooldownThenProbeSuccessCloses(t *testing.T) {
	fc := newFakeClock()
	cb := newTestBreaker(fc, 1, 10*time.Second)

	cb.RecordFailure() // open

	// Before cooldown: still rejected.
	fc.advance(9 * time.Second)
	if cb.Allow() {
		t.Fatal("Allow must return false before cooldown elapses")
	}
	if got := cb.State(); got != StateOpen {
		t.Fatalf("want StateOpen before cooldown, got %s", got)
	}

	// Cooldown elapses: Allow transitions to HalfOpen and permits the probe.
	fc.advance(1 * time.Second) // total = 10s == Cooldown
	if !cb.Allow() {
		t.Fatal("Allow must return true once cooldown elapses")
	}
	if got := cb.State(); got != StateHalfOpen {
		t.Fatalf("want StateHalfOpen after cooldown Allow, got %s", got)
	}

	// A second Allow while probe is in flight must return false.
	if cb.Allow() {
		t.Fatal("second Allow in HalfOpen must return false")
	}

	// Probe succeeds: back to Closed.
	cb.RecordSuccess()
	if got := cb.State(); got != StateClosed {
		t.Fatalf("want StateClosed after probe success, got %s", got)
	}
	if !cb.Allow() {
		t.Fatal("Allow must return true after recovery")
	}
}

func TestCircuitBreakerCooldownThenProbeFailureReopens(t *testing.T) {
	fc := newFakeClock()
	cb := newTestBreaker(fc, 1, 10*time.Second)

	cb.RecordFailure() // open

	fc.advance(10 * time.Second) // cooldown elapses

	if !cb.Allow() {
		t.Fatal("Allow must return true once cooldown elapses")
	}
	if got := cb.State(); got != StateHalfOpen {
		t.Fatalf("want StateHalfOpen, got %s", got)
	}

	// Probe fails: back to Open, cooldown restarts.
	cb.RecordFailure()
	if got := cb.State(); got != StateOpen {
		t.Fatalf("want StateOpen after probe failure, got %s", got)
	}

	// New cooldown has not elapsed: still rejected.
	fc.advance(5 * time.Second)
	if cb.Allow() {
		t.Fatal("Allow must return false before new cooldown elapses")
	}

	// Advance through the new cooldown: probe permitted again.
	fc.advance(5 * time.Second)
	if !cb.Allow() {
		t.Fatal("Allow must return true once new cooldown elapses")
	}
}

func TestCircuitBreakerStateString(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateClosed, "closed"},
		{StateOpen, "open"},
		{StateHalfOpen, "half-open"},
		{State(99), "unknown"},
	}
	for _, tc := range tests {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestCircuitBreakerExecuteRecordsSuccessAndFailure(t *testing.T) {
	fc := newFakeClock()
	cb := newTestBreaker(fc, 3, 10*time.Second)
	sentinel := errors.New("provider down")

	// Successful execute: failures remain 0, still closed.
	if err := cb.Execute(context.Background(), func(_ context.Context) error { return nil }); err != nil {
		t.Fatalf("Execute success: want nil, got %v", err)
	}

	// Failing execute propagates the fn error and records the failure.
	err := cb.Execute(context.Background(), func(_ context.Context) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Execute failure: want sentinel, got %v", err)
	}
	if got := cb.State(); got != StateClosed {
		t.Fatalf("want StateClosed after 1 failure, got %s", got)
	}
}
