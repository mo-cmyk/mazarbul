package mcp

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"
)

// HealthPath is the URL at which the HTTP transport exposes its
// liveness probe. Exported so supervisors, scripts, and tests share a
// single source of truth instead of stringly-typed copies.
const HealthPath = "/healthz"

// HealthResponse is the JSON body emitted on the health endpoint. The
// fields follow AIP-122 lowerCamelCase: status is a fixed sentinel
// callers can match against; startTime is RFC3339-encoded and lets
// scrapers compute uptime as (now - startTime), matching Prometheus's
// "export the timestamp, not a precomputed duration" guidance. Pid is the
// daemon's own process id, so a supervisor can confirm the process
// answering this endpoint is the exact one it expects (e.g. cross-checking
// against a launchd job PID) rather than some other maz daemon on the port.
type HealthResponse struct {
	Status    string    `json:"status"`
	StartTime time.Time `json:"startTime"`
	PID       int       `json:"pid"`
}

// healthHandler returns an http.HandlerFunc that serves HealthResponse
// snapshotting startFn() at request time. StartFn is a function (not a
// time.Time) so the handler captures the server's lifecycle: Start sets
// the value, and a Shutdown-then-Start cycle (a fresh Server) re-binds
// the listener anyway, so there is no stale-time hazard.
//
// Only GET is accepted; any other method returns 405 with an Allow
// header so a misconfigured probe cannot observe a healthy 200 for the
// wrong verb. The handler is intentionally quiet on the success path:
// liveness probes can fire dozens of times a minute and would otherwise
// flood the log. Encoding failures are logged at Warn because they
// signal a programmer error, not a runtime one.
func healthHandler(logger *zap.Logger, startFn func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		body := HealthResponse{
			Status:    "ok",
			StartTime: startFn(),
			PID:       os.Getpid(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			logger.Warn("healthz encode", zap.Error(err))
		}
	}
}
