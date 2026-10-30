package resilience

import (
	"context"
	"errors"
	"fmt"
)

// Attempt is a named, breaker-guarded entry in a Chain.
type Attempt struct {
	// Name identifies the attempt in error messages and the Do return value.
	Name string
	// Breaker is the circuit breaker guarding this attempt.
	Breaker *CircuitBreaker
}

// Chain is an ordered list of named, breaker-guarded attempts. Do tries each
// in order, falling back to the next within the same call when an earlier one
// is open or fails.
//
// NOTE: Chain has no production callers yet — it is the fallback substrate for
// the resilient embedder chain (Ollama primary + in-process fallback) landing
// in #246. It ships now alongside CircuitBreaker so #246 is a pure addition.
type Chain[T any] struct {
	// Attempts is the ordered list of guarded attempts, tried left-to-right.
	Attempts []Attempt
}

// ErrAllAttemptsFailed is included in the joined error returned by Do when
// every attempt either had an open breaker or returned an error.
var ErrAllAttemptsFailed = errors.New("all attempts failed or open")

// Do iterates the attempts in order. Attempts whose breaker is open are
// skipped without calling fn. For each attempt that proceeds, fn is called
// with the context and the attempt name; the outcome is recorded on the
// breaker. Do returns the first successful result and the winning attempt
// name. When all attempts are exhausted, it returns a joined error that
// includes ErrAllAttemptsFailed and each individual failure, so callers can
// inspect the chain via errors.Is.
//
//nolint:ireturn // Chain.Do must return the generic result type T; ireturn incorrectly treats a type parameter constrained by any as an interface return.
func (c *Chain[T]) Do(ctx context.Context, fn func(context.Context, string) (T, error)) (T, string, error) {
	errs := make([]error, 0, len(c.Attempts)+1)
	for _, a := range c.Attempts {
		if !a.Breaker.Allow() {
			errs = append(errs, fmt.Errorf("attempt %q: %w", a.Name, ErrOpen))
			continue
		}
		result, err := fn(ctx, a.Name)
		if err != nil {
			a.Breaker.RecordFailure()
			errs = append(errs, fmt.Errorf("attempt %q: %w", a.Name, err))
			continue
		}
		a.Breaker.RecordSuccess()
		return result, a.Name, nil
	}
	var zero T
	errs = append(errs, ErrAllAttemptsFailed)
	return zero, "", fmt.Errorf("%w", errors.Join(errs...))
}
