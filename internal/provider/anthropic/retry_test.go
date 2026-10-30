package anthropic

import (
	"math/rand/v2"
	"net/http"
	"testing"
	"time"
)

func TestRetryable(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusOK, false},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusRequestTimeout, false}, // 408: per #49, no 4xx retries
		{http.StatusConflict, false},       // 409: same
		{http.StatusTooManyRequests, true}, // 429
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
	}
	for _, c := range cases {
		if got := retryable(c.status); got != c.want {
			t.Errorf("retryable(%d) = %v, want %v", c.status, got, c.want)
		}
	}
}

func TestBackoff_Bounds(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for attempt := range 12 {
		d := backoff(attempt, 0, rng.Float64())
		if d <= 0 {
			t.Errorf("attempt %d: backoff %v must be positive", attempt, d)
		}
		if d > maxBackoff {
			t.Errorf("attempt %d: backoff %v exceeds maxBackoff %v", attempt, d, maxBackoff)
		}
	}
}

func TestBackoff_Jitter(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 99))
	// attempt=0 -> initialBackoff with 25% jitter floor, so 750ms .. 1s.
	minWant := time.Duration(float64(initialBackoff) * (1.0 - jitterFrac))
	for range 100 {
		d := backoff(0, 0, rng.Float64())
		if d < minWant {
			t.Errorf("backoff(0) = %v, want >= %v (jitter floor)", d, minWant)
		}
		if d > initialBackoff {
			t.Errorf("backoff(0) = %v, want <= %v (no positive jitter)", d, initialBackoff)
		}
	}
}

func TestBackoff_RetryAfterWins(t *testing.T) {
	got := backoff(0, 2*time.Second, 0.5)
	if got != 2*time.Second {
		t.Errorf("backoff with retry-after=2s returned %v, want 2s", got)
	}
}

func TestBackoff_RetryAfterCapped(t *testing.T) {
	got := backoff(0, 5*time.Minute, 0.5)
	if got != maxBackoff {
		t.Errorf("backoff with retry-after=5m returned %v, want maxBackoff %v", got, maxBackoff)
	}
}

func TestParseRetryAfter_Seconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "3")
	if got := parseRetryAfter(h); got != 3*time.Second {
		t.Errorf("parseRetryAfter(3) = %v, want 3s", got)
	}
}

func TestParseRetryAfter_FractionalSeconds(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "0.5")
	if got := parseRetryAfter(h); got != 500*time.Millisecond {
		t.Errorf("parseRetryAfter(0.5) = %v, want 500ms", got)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(5*time.Second).UTC().Format(http.TimeFormat))
	got := parseRetryAfter(h)
	if got < 3*time.Second || got > 6*time.Second {
		t.Errorf("parseRetryAfter(http-date) = %v, want ~5s", got)
	}
}

func TestParseRetryAfter_Missing(t *testing.T) {
	if got := parseRetryAfter(http.Header{}); got != 0 {
		t.Errorf("parseRetryAfter(empty) = %v, want 0", got)
	}
}

func TestParseRetryAfter_Malformed(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "nonsense")
	if got := parseRetryAfter(h); got != 0 {
		t.Errorf("parseRetryAfter(nonsense) = %v, want 0", got)
	}
}
