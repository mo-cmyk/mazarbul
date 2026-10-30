package daemonclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/daemonclient"
)

// rebuildHandler serves the admin rebuild surface for tests. phase drives the
// GET status response: 0=building, 1=succeeded, 2=failed. When conflict is set,
// POSTs return 409 (a rebuild already running).
func rebuildHandler(phase *atomic.Int32, conflict bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && (r.URL.Path == "/admin/v1/rebuild" || r.URL.Path == "/admin/v1/reindex"):
			if conflict {
				w.WriteHeader(http.StatusConflict)

				_ = json.NewEncoder(w).Encode(map[string]string{"message": "already running"})
				return
			}
			kind := "rebuild"
			if r.URL.Path == "/admin/v1/reindex" {
				kind = "reindex"
			}
			w.WriteHeader(http.StatusAccepted)

			_ = json.NewEncoder(w).Encode(map[string]string{"jobUid": "job-1", "kind": kind})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/v1/rebuild":
			st := map[string]any{"jobUid": "job-1", "kind": "rebuild", "state": "building", "progress": 0.5}
			switch phase.Load() {
			case 1:
				st["state"] = "succeeded"
				st["progress"] = 1.0
			case 2:
				st["state"] = "failed"
				st["error"] = "kaboom"
			}

			_ = json.NewEncoder(w).Encode(st)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// shortSockDir returns a short-lived temp dir whose path is short enough for a
// Unix socket (macOS caps sun_path at ~104 bytes — t.TempDir() embeds the full
// test name and can blow that budget for long-named tests).
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mz")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serveRebuild binds an HTTP server on a Unix socket at sock and returns a stop
// func that closes the listener (unlinking the socket). Unlike newUnixTestServer
// it does not register t.Cleanup, so the reconnect test can drop + rebind the
// same path mid-poll.
func serveRebuild(t *testing.T, sock string, h http.Handler) func() {
	t.Helper()
	_ = os.Remove(sock)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }
}

func TestTriggerRebuildRoundTrip(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortSockDir(t), "s")
	var phase atomic.Int32
	newUnixTestServer(t, sock, rebuildHandler(&phase, false))
	c, err := daemonclient.NewWithSocket(sock)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := c.TriggerRebuild(context.Background(), false)
	if err != nil {
		t.Fatalf("TriggerRebuild: %v", err)
	}
	if resp.JobUID != "job-1" || resp.Kind != "rebuild" {
		t.Fatalf("rebuild resp = %+v; want {job-1 rebuild}", resp)
	}

	rresp, err := c.TriggerReindex(context.Background())
	if err != nil {
		t.Fatalf("TriggerReindex: %v", err)
	}
	if rresp.Kind != "reindex" {
		t.Fatalf("reindex resp kind = %q; want reindex", rresp.Kind)
	}
}

func TestTriggerRebuildConflict(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortSockDir(t), "s")
	var phase atomic.Int32
	newUnixTestServer(t, sock, rebuildHandler(&phase, true))
	c, _ := daemonclient.NewWithSocket(sock)

	_, err := c.TriggerRebuild(context.Background(), false)
	var he *daemonclient.HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 *HTTPError, got %v", err)
	}
}

func TestPollRebuildSucceeds(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortSockDir(t), "s")
	var phase atomic.Int32
	newUnixTestServer(t, sock, rebuildHandler(&phase, false))
	c, _ := daemonclient.NewWithSocket(sock)

	go func() {
		time.Sleep(40 * time.Millisecond)
		phase.Store(1)
	}()
	if err := c.PollRebuild(context.Background(), 10*time.Millisecond, nil); err != nil {
		t.Fatalf("PollRebuild: %v", err)
	}
}

func TestPollRebuildReportsFailure(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortSockDir(t), "s")
	var phase atomic.Int32
	phase.Store(2)
	newUnixTestServer(t, sock, rebuildHandler(&phase, false))
	c, _ := daemonclient.NewWithSocket(sock)

	err := c.PollRebuild(context.Background(), 10*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("want failure surfacing 'kaboom', got %v", err)
	}
}

// TestPollRebuildToleratesSwapReload is the load-bearing test for the no-stop
// guarantee: when the daemon drops the socket during the swap's serving-stack
// reload, PollRebuild keeps polling (treating the dial error as "swapping")
// rather than failing, and reads the terminal state once the socket returns.
func TestPollRebuildToleratesSwapReload(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(shortSockDir(t), "s")
	var phase atomic.Int32
	var sawSwapping atomic.Bool

	stop1 := serveRebuild(t, sock, rebuildHandler(&phase, false))
	c, _ := daemonclient.NewWithSocket(sock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.PollRebuild(ctx, 10*time.Millisecond, func(st daemonclient.RebuildStatus) {
			if st.State == "swapping" {
				sawSwapping.Store(true)
			}
		})
	}()

	time.Sleep(40 * time.Millisecond) // poll observes "building"
	stop1()                           // swap window: socket drops
	time.Sleep(40 * time.Millisecond) // poll sees dial errors → swapping
	phase.Store(1)                    // rebuilt projection swapped in
	stop2 := serveRebuild(t, sock, rebuildHandler(&phase, false))
	defer stop2()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PollRebuild across reload: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("PollRebuild did not complete after the socket returned")
	}
	if !sawSwapping.Load() {
		t.Errorf("expected a swapping update during the socket-drop window")
	}
}

// TestTriggerRebuildNoTCPFallback verifies the admin trigger never routes over
// TCP: a TCP-only client refuses rather than silently 404ing.
func TestTriggerRebuildNoTCPFallback(t *testing.T) {
	t.Parallel()
	c := daemonclient.NewWithHTTP("127.0.0.1:0")
	_, err := c.TriggerRebuild(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "Unix socket") {
		t.Fatalf("TCP-only TriggerRebuild should refuse; got %v", err)
	}
}
