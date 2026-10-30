package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

// openBreaker returns a CircuitBreaker that is already in StateOpen.
func openBreaker(fc *fakeClock) *CircuitBreaker {
	cb := newTestBreaker(fc, 1, 10*time.Second)
	cb.RecordFailure()
	return cb
}

// closedBreaker returns a fresh CircuitBreaker in StateClosed.
func closedBreaker(fc *fakeClock) *CircuitBreaker {
	return newTestBreaker(fc, 3, 10*time.Second)
}

func TestChainFallsBackToSecondAttemptWhenFirstIsOpen(t *testing.T) {
	fc := newFakeClock()
	chain := &Chain[string]{
		Attempts: []Attempt{
			{Name: "primary", Breaker: openBreaker(fc)},
			{Name: "fallback", Breaker: closedBreaker(fc)},
		},
	}

	result, winner, err := chain.Do(context.Background(), func(_ context.Context, name string) (string, error) {
		return "result-from-" + name, nil
	})
	if err != nil {
		t.Fatalf("Do: want nil error, got %v", err)
	}
	if winner != "fallback" {
		t.Fatalf("Do: want winner %q, got %q", "fallback", winner)
	}
	if result != "result-from-fallback" {
		t.Fatalf("Do: want result %q, got %q", "result-from-fallback", result)
	}
}

func TestChainReturnsWinningAttemptName(t *testing.T) {
	fc := newFakeClock()
	chain := &Chain[int]{
		Attempts: []Attempt{
			{Name: "alpha", Breaker: closedBreaker(fc)},
			{Name: "beta", Breaker: closedBreaker(fc)},
		},
	}

	_, winner, err := chain.Do(context.Background(), func(_ context.Context, name string) (int, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("Do: want nil error, got %v", err)
	}
	// First attempt succeeds, so winner must be "alpha".
	if winner != "alpha" {
		t.Fatalf("Do: want winner %q, got %q", "alpha", winner)
	}
}

func TestChainFallsThroughOnFnError(t *testing.T) {
	fc := newFakeClock()
	primaryErr := errors.New("primary failed")
	chain := &Chain[string]{
		Attempts: []Attempt{
			{Name: "primary", Breaker: closedBreaker(fc)},
			{Name: "fallback", Breaker: closedBreaker(fc)},
		},
	}

	calls := 0
	result, winner, err := chain.Do(context.Background(), func(_ context.Context, name string) (string, error) {
		calls++
		if name == "primary" {
			return "", primaryErr
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Do: want nil error, got %v", err)
	}
	if winner != "fallback" {
		t.Fatalf("Do: want winner %q, got %q", "fallback", winner)
	}
	if result != "ok" {
		t.Fatalf("Do: want result %q, got %q", "ok", result)
	}
	if calls != 2 {
		t.Fatalf("want 2 fn calls, got %d", calls)
	}
}

func TestChainAllOpenReturnsJoinedError(t *testing.T) {
	fc := newFakeClock()
	chain := &Chain[string]{
		Attempts: []Attempt{
			{Name: "primary", Breaker: openBreaker(fc)},
			{Name: "fallback", Breaker: openBreaker(fc)},
		},
	}

	fnCalled := false
	_, _, err := chain.Do(context.Background(), func(_ context.Context, _ string) (string, error) {
		fnCalled = true
		return "", nil
	})
	if err == nil {
		t.Fatal("Do: want non-nil error when all breakers are open")
	}
	if fnCalled {
		t.Fatal("fn must not be called when all breakers are open")
	}
	if !errors.Is(err, ErrOpen) {
		t.Errorf("Do: want errors.Is(err, ErrOpen) = true, got %v", err)
	}
	if !errors.Is(err, ErrAllAttemptsFailed) {
		t.Errorf("Do: want errors.Is(err, ErrAllAttemptsFailed) = true, got %v", err)
	}
}

func TestChainAllFailedReturnsJoinedError(t *testing.T) {
	fc := newFakeClock()
	sentinelA := errors.New("provider A down")
	sentinelB := errors.New("provider B down")
	chain := &Chain[string]{
		Attempts: []Attempt{
			{Name: "a", Breaker: closedBreaker(fc)},
			{Name: "b", Breaker: closedBreaker(fc)},
		},
	}

	_, _, err := chain.Do(context.Background(), func(_ context.Context, name string) (string, error) {
		if name == "a" {
			return "", sentinelA
		}
		return "", sentinelB
	})
	if err == nil {
		t.Fatal("Do: want non-nil error when all fn calls fail")
	}
	if !errors.Is(err, sentinelA) {
		t.Errorf("want sentinelA in error chain, got %v", err)
	}
	if !errors.Is(err, sentinelB) {
		t.Errorf("want sentinelB in error chain, got %v", err)
	}
	if !errors.Is(err, ErrAllAttemptsFailed) {
		t.Errorf("want ErrAllAttemptsFailed in error chain, got %v", err)
	}
}

func TestChainSuccessRecordedOnBreakerAfterFallback(t *testing.T) {
	fc := newFakeClock()
	primary := openBreaker(fc)
	fallback := closedBreaker(fc)
	chain := &Chain[int]{
		Attempts: []Attempt{
			{Name: "primary", Breaker: primary},
			{Name: "fallback", Breaker: fallback},
		},
	}

	// Run successfully via fallback.
	_, _, err := chain.Do(context.Background(), func(_ context.Context, _ string) (int, error) {
		return 1, nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	// Fallback breaker recorded a success; failure streak is reset.
	// Advance clock past the primary cooldown so it can also be probed.
	fc.advance(10 * time.Second)
	if !primary.Allow() {
		t.Fatal("primary breaker should permit a probe after cooldown")
	}
}
