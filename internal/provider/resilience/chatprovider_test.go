package resilience_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/provider/resilience"
)

// fakeChat is a kernel.ChatProvider whose calls return a scripted error,
// letting the breaker tests drive success/failure deterministically.
type fakeChat struct {
	err   error
	calls int
}

func (f *fakeChat) Complete(_ context.Context, _ kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	f.calls++
	return kernel.CompleteResponse{}, f.err
}

func (f *fakeChat) ToolUse(_ context.Context, _ kernel.ToolUseRequest) (kernel.ToolUseResponse, error) { //nolint:gocritic // hugeParam: value signature forced by kernel.ChatProvider interface.
	f.calls++
	return kernel.ToolUseResponse{}, f.err
}

func TestChatProvider_OpensAfterConsecutiveFailures(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	breaker := &resilience.CircuitBreaker{MaxConsecutiveFailures: 2, Cooldown: time.Minute, Now: clock}
	backend := &fakeChat{err: errors.New("backend down")}
	cp, err := resilience.NewChatProvider(backend, breaker)
	if err != nil {
		t.Fatalf("NewChatProvider: %v", err)
	}

	// Two consecutive failures trip the breaker.
	for i := range 2 {
		if _, cerr := cp.Complete(context.Background(), kernel.CompleteRequest{}); cerr == nil {
			t.Fatalf("call %d: err = nil, want backend error", i)
		}
	}
	if got := cp.State(); got != resilience.StateOpen {
		t.Fatalf("state = %v, want open after 2 failures", got)
	}

	// The next call fails fast with ErrOpen WITHOUT reaching the backend.
	callsBefore := backend.calls
	_, openErr := cp.ToolUse(context.Background(), kernel.ToolUseRequest{})
	if !errors.Is(openErr, resilience.ErrOpen) {
		t.Errorf("err = %v, want ErrOpen", openErr)
	}
	if backend.calls != callsBefore {
		t.Errorf("backend called %d times while open, want %d (fail fast)", backend.calls, callsBefore)
	}
}

func TestChatProvider_HalfOpenProbeRecovers(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	breaker := &resilience.CircuitBreaker{MaxConsecutiveFailures: 1, Cooldown: time.Minute, Now: clock}
	backend := &fakeChat{err: errors.New("backend down")}
	cp, err := resilience.NewChatProvider(backend, breaker)
	if err != nil {
		t.Fatalf("NewChatProvider: %v", err)
	}

	// One failure opens the breaker.
	if _, cerr := cp.Complete(context.Background(), kernel.CompleteRequest{}); cerr == nil {
		t.Fatal("want backend error")
	}
	if got := cp.State(); got != resilience.StateOpen {
		t.Fatalf("state = %v, want open", got)
	}

	// Advance past the cooldown and let the backend recover; the half-open
	// probe succeeds and closes the breaker.
	now = now.Add(2 * time.Minute)
	backend.err = nil
	if _, cerr := cp.Complete(context.Background(), kernel.CompleteRequest{}); cerr != nil {
		t.Fatalf("half-open probe: err = %v, want nil", cerr)
	}
	if got := cp.State(); got != resilience.StateClosed {
		t.Errorf("state = %v, want closed after successful probe", got)
	}
}

func TestChatProvider_ContextCancellationDoesNotTrip(t *testing.T) {
	t.Parallel()

	breaker := &resilience.CircuitBreaker{MaxConsecutiveFailures: 1, Cooldown: time.Minute}
	backend := &fakeChat{err: context.Canceled}
	cp, err := resilience.NewChatProvider(backend, breaker)
	if err != nil {
		t.Fatalf("NewChatProvider: %v", err)
	}

	// A cancelled call surfaces the error but must NOT count toward the breaker:
	// the backend's health is unknown when the caller aborts.
	if _, cerr := cp.Complete(context.Background(), kernel.CompleteRequest{}); !errors.Is(cerr, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", cerr)
	}
	if got := cp.State(); got != resilience.StateClosed {
		t.Errorf("state = %v, want closed (cancellation is not a failure)", got)
	}
}

func TestChatProvider_CancelledContextSuppressesNonContextError(t *testing.T) {
	t.Parallel()

	breaker := &resilience.CircuitBreaker{MaxConsecutiveFailures: 1, Cooldown: time.Minute}
	// The backend returns a plain network error (NOT a context error), as can
	// happen when a connection is reset mid-flight during a graceful shutdown.
	backend := &fakeChat{err: errors.New("connection reset by peer")}
	cp, err := resilience.NewChatProvider(backend, breaker)
	if err != nil {
		t.Fatalf("NewChatProvider: %v", err)
	}

	// Cancel the call context BEFORE the call. The error surfaces verbatim, but
	// because the caller's context is cancelled (ctx.Err() != nil) the breaker
	// must not count it: backend health is indeterminate when the caller aborts.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, cerr := cp.Complete(ctx, kernel.CompleteRequest{}); cerr == nil {
		t.Fatal("err = nil, want the backend network error")
	}
	if got := cp.State(); got != resilience.StateClosed {
		t.Errorf("state = %v, want closed (cancelled context suppresses the failure)", got)
	}
}
