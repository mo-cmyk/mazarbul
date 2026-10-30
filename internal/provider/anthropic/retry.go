package anthropic

import (
	"net/http"
	"strconv"
	"time"
)

// Retry policy constants. The values match Anthropic's published guidance
// for clients that talk directly to the Messages API and are intentionally
// kept tighter than the official SDK (the SDK also retries on 408 and 409;
// per issue #49 we keep retries to 5xx and 429).
const (
	// DefaultMaxRetries is the number of attempts beyond the initial request.
	// MaxRetries = 3 means 1 initial attempt + up to 3 retries = 4 attempts.
	defaultMaxRetries = 3

	// InitialBackoff is the first backoff delay before jitter.
	initialBackoff = 1 * time.Second

	// MaxBackoff caps the exponential growth.
	maxBackoff = 60 * time.Second

	// JitterFrac is the maximum fractional reduction applied for jitter. A
	// value of 0.25 means actual sleep is in [0.75x, 1.0x] of the computed
	// backoff, smoothing thundering-herd patterns on shared rate limits.
	jitterFrac = 0.25
)

// retryable reports whether an HTTP status code indicates a transient
// failure worth retrying. Per issue #49: 5xx and 429 yes, every other 4xx
// no (including 408 and 409, which the official SDK retries; we keep the
// surface tight at M1).
func retryable(status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status <= 599
}

// backoff returns the delay before the next retry. Attempt is zero-indexed
// (the first retry uses attempt=0). When retryAfter is non-zero it replaces
// the calculated value (still capped at maxBackoff and never reduced by
// jitter, so the server-suggested delay is honored verbatim). The jitter
// argument is a [0.0, 1.0) random value used to shave up to jitterFrac off
// the computed delay; passing it in (rather than holding an *rand.Rand)
// keeps backoff a pure function so the caller (Client) owns the RNG and
// the mutex that guards it and concurrent retrying Complete calls cannot
// race on shared random state.
func backoff(attempt int, retryAfter time.Duration, jitter float64) time.Duration {
	if retryAfter > 0 {
		if retryAfter > maxBackoff {
			return maxBackoff
		}
		return retryAfter
	}
	delay := initialBackoff << attempt
	if delay <= 0 || delay > maxBackoff {
		delay = maxBackoff
	}
	scale := 1.0 - jitterFrac*jitter
	return time.Duration(float64(delay) * scale)
}

// parseRetryAfter parses an HTTP Retry-After header value. Anthropic returns
// the seconds form; we accept the HTTP-date form too for forward-compat
// (RFC 7231 lets servers pick either). An unparsable value returns zero.
func parseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
