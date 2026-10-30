//go:build !windows

package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// defaultOllamaURL is the base URL used when OLLAMA_HOST is unset.
// Matches Ollama's own default bind address. The path "/api/version"
// is appended by defaultHTTPProbe; defaultOllamaURL must not include
// a trailing slash.
const defaultOllamaURL = "http://127.0.0.1:11434"

// httpProbeTimeout bounds the per-probe HTTP request so a wedged
// Ollama (e.g. stuck loading model weights, OOM-spinning) cannot pin
// the supervisor goroutine. Keep this well below ollamaProbeInterval
// so even on the slowest tick the probe completes within one cycle.
const httpProbeTimeout = 500 * time.Millisecond

// wedgedFailureThreshold is the number of consecutive HTTP-probe
// failures, in delegated mode, that signal the external Ollama has
// gone silent and the supervisor should take over by spawning its
// own child. Three ticks at the 5s ollamaProbeInterval means a
// silent endpoint is taken over inside ~15s, short enough that
// callers do not see prolonged timeouts and long enough that a
// single dropped packet does not trigger a needless takeover.
//
// Supervised mode does NOT consult this threshold; there, signal-0
// liveness via ProbeAlive is the sole gate. A wedged-but-alive
// supervised child is therefore not currently detected — a
// follow-up could extend tick()'s supervised branch to add HTTP
// debounce too, but the supervisor's responsibility today is "did
// the PID we spawned exit?", not "is the endpoint serving?".
const wedgedFailureThreshold = 3

// resolveOllamaURL picks the base URL the supervisor should probe.
// Honours OLLAMA_HOST exactly the way Ollama's own client does: bare
// "host" or "host:port" gets an http:// scheme prepended; an explicit
// scheme is left untouched. Trailing slashes are trimmed so probe URL
// concatenation is unambiguous.
func resolveOllamaURL() string {
	host := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if host == "" {
		return defaultOllamaURL
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	// TrimSuffix (not TrimRight) so "http://h//" -> "http://h/" rather
	// than "http://h" — a character-set trim would silently drop path
	// components for hosts that happen to end with multiple slashes.
	return strings.TrimSuffix(host, "/")
}

// defaultHTTPProbe returns a probe that GETs baseURL+/api/version
// bounded by a context deadline of httpProbeTimeout. The returned
// closure is goroutine-safe and shares a single http.Client so
// connection reuse keeps probe latency under 1ms after the first
// call. Any non-2xx response or transport error reports the endpoint
// as unhealthy.
//
// The timeout is enforced via context only (not http.Client.Timeout)
// so traces and zap fields surface the actual cancellation reason
// when a probe gets stuck — http.Client.Timeout silently swaps the
// error to "context deadline exceeded" with no caller signal.
func defaultHTTPProbe(baseURL string) func(context.Context) bool {
	client := &http.Client{}
	target := strings.TrimSuffix(baseURL, "/") + "/api/version"
	return func(ctx context.Context) bool {
		ctx, cancel := context.WithTimeout(ctx, httpProbeTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		// Drain so the underlying TCP connection can be reused; the
		// close error after a successful probe is non-actionable
		// (the next probe opens a fresh connection on its own).
		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // probe drain
		if cerr := resp.Body.Close(); cerr != nil {
			_ = cerr //nolint:wsl // intentional discard, see comment above
		}
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
}

// isOllamaExe reports whether the executable path returned by
// defaultPidExePath looks like the `ollama` binary. An empty path is
// treated as "platform cannot tell us" and returns true so the
// fallback `_other` build still adopts PIDs. Production paths look
// like "ollama" (macOS ps comm column) or "/usr/local/bin/ollama"
// (Linux /proc/<pid>/exe readlink); both end in the basename "ollama"
// with an optional version suffix on some distros (e.g.
// "/snap/ollama/current/ollama").
func isOllamaExe(path string) bool {
	if path == "" {
		return true
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return false
	}
	// Path may be a basename ("ollama") or an absolute path
	// ("/usr/local/bin/ollama"). lastSlash + 1 yields the basename in
	// both cases without an extra filepath.Base allocation.
	last := strings.LastIndex(trimmed, "/")
	base := trimmed[last+1:]
	return base == "ollama" || strings.HasPrefix(base, "ollama-")
}
