package resilience

import (
	"context"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// ChatProvider wraps a kernel.ChatProvider with a circuit breaker so a dead
// backend fails fast (ErrOpen) instead of being hammered call after call by
// autonomous jobs (the cognitive-lifecycle resilience the breaker was built
// for). It implements kernel.ChatProvider, so it drops in transparently
// wherever the daemon wires a provider.
//
// Context-cancellation errors are deliberately NOT counted as provider
// failures: they reflect the caller's deadline, not backend health, so a
// cancelled request must not nudge the breaker toward Open.
type ChatProvider struct {
	inner   kernel.ChatProvider
	breaker *CircuitBreaker
}

// Compile-time assertion that the wrapper satisfies the kernel contract.
var _ kernel.ChatProvider = (*ChatProvider)(nil)

// NewChatProvider wraps inner with breaker. Both arguments are required.
func NewChatProvider(inner kernel.ChatProvider, breaker *CircuitBreaker) (*ChatProvider, error) {
	if inner == nil {
		return nil, errors.New("resilience: chat provider: inner provider must not be nil")
	}
	if breaker == nil {
		return nil, errors.New("resilience: chat provider: circuit breaker must not be nil")
	}
	return &ChatProvider{inner: inner, breaker: breaker}, nil
}

// Complete forwards to the wrapped provider when the breaker allows it,
// recording the outcome. It returns ErrOpen (wrapped) without calling the
// backend when the breaker is open.
func (c *ChatProvider) Complete(ctx context.Context, req kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	if !c.breaker.Allow() {
		return kernel.CompleteResponse{}, fmt.Errorf("resilience: chat provider unavailable: %w", ErrOpen)
	}
	resp, err := c.inner.Complete(ctx, req)
	c.record(ctx, err)
	return resp, err
}

// ToolUse forwards to the wrapped provider when the breaker allows it,
// recording the outcome. It returns ErrOpen (wrapped) without calling the
// backend when the breaker is open.
func (c *ChatProvider) ToolUse(ctx context.Context, req kernel.ToolUseRequest) (kernel.ToolUseResponse, error) { //nolint:gocritic // hugeParam: value signature forced by kernel.ChatProvider interface.
	if !c.breaker.Allow() {
		return kernel.ToolUseResponse{}, fmt.Errorf("resilience: chat provider unavailable: %w", ErrOpen)
	}
	resp, err := c.inner.ToolUse(ctx, req)
	c.record(ctx, err)
	return resp, err
}

// record updates the breaker from a call outcome. A nil error is a success; a
// context-cancellation (the caller aborted) leaves the breaker untouched
// because backend health is unknown; any other error is a provider failure.
func (c *ChatProvider) record(ctx context.Context, err error) {
	switch {
	case err == nil:
		c.breaker.RecordSuccess()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		// Caller-driven cancellation: not a provider-health signal.
	default:
		c.breaker.RecordFailure()
	}
}

// State exposes the breaker's current state for diagnostics (e.g. maz doctor).
func (c *ChatProvider) State() State { return c.breaker.State() }
