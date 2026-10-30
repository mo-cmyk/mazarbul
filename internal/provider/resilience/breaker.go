package resilience

import (
	"context"
	"errors"
	"sync"
	"time"
)

// State is the operating state of a CircuitBreaker.
type State int

const (
	// StateClosed is the normal operating state; all requests pass through.
	StateClosed State = iota
	// StateOpen means the breaker has tripped; requests are rejected until the
	// cooldown elapses.
	StateOpen
	// StateHalfOpen means one probe request is in flight to test whether the
	// dependency has recovered.
	StateHalfOpen
)

// String returns the human-readable name of the state.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ErrOpen is returned by Execute when the circuit breaker is open and the
// request was not forwarded to the underlying function.
var ErrOpen = errors.New("circuit breaker open")

// CircuitBreaker is a thread-safe implementation of the circuit-breaker
// pattern. It allows normal operation (Closed), trips after
// MaxConsecutiveFailures consecutive failures (Open), and probes for recovery
// after Cooldown elapses (HalfOpen). Inject a custom Now function for
// deterministic tests without real sleeps.
type CircuitBreaker struct {
	// MaxConsecutiveFailures is the number of consecutive failures that trip
	// the breaker from Closed to Open.
	MaxConsecutiveFailures int

	// Cooldown is the duration the breaker stays in Open before permitting a
	// single probe request (transitioning to HalfOpen).
	Cooldown time.Duration

	// Now is the clock source used for timing decisions. When nil, time.Now is
	// used.
	Now func() time.Time

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
}

func (cb *CircuitBreaker) clock() time.Time {
	if cb.Now != nil {
		return cb.Now()
	}
	return time.Now()
}

// State returns the current state of the circuit breaker.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// Allow reports whether the next request may proceed. In Open, it returns
// false until the cooldown elapses, then transitions to HalfOpen and returns
// true for the single probe. In HalfOpen it returns false because a probe is
// already in flight. In Closed it always returns true.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		if cb.clock().Sub(cb.openedAt) >= cb.Cooldown {
			cb.state = StateHalfOpen
			return true
		}
		return false
	case StateHalfOpen:
		return false
	default:
		return false
	}
}

// RecordSuccess records a successful request outcome. In HalfOpen, it
// transitions the breaker back to Closed and resets the failure count. In
// Closed, it resets the consecutive failure streak.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case StateClosed:
		cb.failures = 0
	case StateHalfOpen:
		cb.state = StateClosed
		cb.failures = 0
	default: // StateOpen: no-op.
	}
}

// RecordFailure records a failed request outcome. In Closed, it increments the
// consecutive failure count and trips the breaker to Open once
// MaxConsecutiveFailures is reached. In HalfOpen, the probe failed so the
// breaker returns to Open and the cooldown restarts.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case StateClosed:
		cb.failures++
		if cb.failures >= cb.MaxConsecutiveFailures {
			cb.state = StateOpen
			cb.openedAt = cb.clock()
		}
	case StateHalfOpen:
		cb.state = StateOpen
		cb.openedAt = cb.clock()
	default: // StateOpen: no-op.
	}
}

// Execute checks whether the breaker allows the request, runs fn if so, and
// records the outcome. It returns ErrOpen when the breaker is open and fn was
// not called. Any error returned by fn is recorded as a failure and returned
// unchanged.
func (cb *CircuitBreaker) Execute(ctx context.Context, fn func(context.Context) error) error {
	if !cb.Allow() {
		return ErrOpen
	}
	if err := fn(ctx); err != nil {
		cb.RecordFailure()
		return err
	}
	cb.RecordSuccess()
	return nil
}
