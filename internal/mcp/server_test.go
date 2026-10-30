package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// echoTool is a Tool whose Invoke returns its arguments verbatim. Used
// to drive the stdio + HTTP round-trip without involving the kernel.
type echoTool struct {
	name string
}

func (e echoTool) Name() string                  { return e.name }
func (e echoTool) Description() string           { return "echoes its arguments" }
func (e echoTool) InputSchema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (e echoTool) OutputSchema() json.RawMessage { return nil }
func (e echoTool) Invoke(_ context.Context, req json.RawMessage) (json.RawMessage, error) {
	if len(req) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return req, nil
}

// unlistedEchoTool is an echoTool that opts out of tools/list via the
// UnlistedTool interface while remaining dispatchable by name.
type unlistedEchoTool struct{ echoTool }

func (unlistedEchoTool) Unlisted() bool { return true }

// blockingTool's Invoke blocks until the test releases it. The tool
// deliberately ignores ctx cancellation so Shutdown cannot drain it
// within a tight grace window. Used to exercise the bounded-grace path
// in Shutdown.
type blockingTool struct {
	name    string
	started chan struct{}
	release chan struct{}
}

func (b blockingTool) Name() string                  { return b.name }
func (b blockingTool) Description() string           { return "blocks until released" }
func (b blockingTool) InputSchema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (b blockingTool) OutputSchema() json.RawMessage { return nil }
func (b blockingTool) Invoke(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-b.release
	return json.RawMessage(`{}`), nil
}

// testHTTPToken is the bearer credential test servers mint for the TCP
// transport. The HTTP request helpers attach it on every /mcp POST so the
// hardenTCP gate admits them; it is ignored on the Unix transport.
const testHTTPToken = "test-http-token"

func newTestOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Name:            "mazarbul-test",
		Version:         "0.0.0",
		Logger:          zap.NewNop(),
		ShutdownTimeout: 2 * time.Second,
		// Tests that enable the TCP transport (opts.HTTPAddr = ...) need a
		// token to pass the fail-closed validate; harmless for the
		// Unix/stdio tests that leave HTTPAddr empty.
		Token: testHTTPToken,
	}
}

// TestNew_RejectsNegativeShutdownTimeout pins the validator on negative
// values. Without this guard a config typo would create an already-
// expired context inside gracefulShutdown, turning Shutdown into a
// no-grace hard stop. Greptile P2 from PR #80 review.
func TestNew_RejectsNegativeShutdownTimeout(t *testing.T) {
	t.Parallel()
	_, err := New(Options{
		Name:            "x",
		Version:         "1",
		Stdio:           true,
		ShutdownTimeout: -1 * time.Second,
	})
	if err == nil {
		t.Fatalf("New with negative ShutdownTimeout = nil, want error")
	}
	if !strings.Contains(err.Error(), "ShutdownTimeout must be non-negative") {
		t.Fatalf("error %q does not mention non-negative requirement", err.Error())
	}
}

// TestServer_StartAfterShutdownReturnsErrServerShutdown pins the
// lifecycle guard against a pre-start Shutdown call leaving listeners
// running. Without the guard, Start would bind listeners, then the
// select would short-circuit on the already-closed shutdownC, leaving
// goroutines + sockets alive with no owner to drain them. Greptile P1
// from PR #80 review.
func TestServer_StartAfterShutdownReturnsErrServerShutdown(t *testing.T) {
	t.Parallel()
	opts := newTestOptions(t)
	opts.HTTPAddr = "127.0.0.1:0"
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("pre-start Shutdown: %v", err)
	}

	startErr := srv.Start(testutil.TestContext(t))
	if !errors.Is(startErr, ErrServerShutdown) {
		t.Fatalf("Start after Shutdown = %v, want ErrServerShutdown", startErr)
	}
	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("Server is %T, want *mcpServer", srv)
	}
	if impl.httpListenerAddr() != nil {
		t.Fatalf("Start after Shutdown bound an HTTP listener; expected nil")
	}
}

// TestServer_ShutdownBoundsWaitOnStartedDoneByContext pins the
// ctx-bounded wait on startedDone inside Shutdown. The unbounded
// `<-started` reproduces deterministically when startedDone is open
// with no goroutine to close it: Shutdown's wait would block forever,
// ignoring the caller's deadline. The bug surface in production is
// any startTransports path that wedges (e.g., a hung wg.Wait inside
// rollback) , defensive bounded wait so the daemon always honours its
// grace deadline. Greptile P1 from PR #80 re-review round 3.
func TestServer_ShutdownBoundsWaitOnStartedDoneByContext(t *testing.T) {
	t.Parallel()
	opts := newTestOptions(t)
	opts.Stdio = true
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("Server is %T, want *mcpServer", srv)
	}

	// Simulate "Start in progress" by claiming startedDone without
	// running startTransports. Nothing will ever close this channel,
	// so the bounded wait is the only thing keeping Shutdown alive.
	impl.lifecycleMu.Lock()
	impl.startedDone = make(chan struct{})
	impl.lifecycleMu.Unlock()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err = srv.Shutdown(shutdownCtx)
	elapsed := time.Since(begin)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Shutdown took %v, want bounded by 100ms ctx", elapsed)
	}
}

// TestServer_ShutdownDuringStartTearsDownTransports pins the lifecycle

// TestServer_ShutdownDuringStartTearsDownTransports pins the lifecycle
// serialization between Start and Shutdown. Without serialization, the
// following interleaving leaks listeners:
//
//	G_A: Start enters; the shutdownC pre-check passes.
//	G_B: Shutdown closes shutdownC; closeTransports snapshots an empty
//	     listener set (Start has not bound anything yet).
//	G_A: startTransports binds the HTTP listener.
//	G_A: select hits <-shutdownC, returns nil; the listener is orphaned.
//
// We can't deterministically force the interleaving, so we run many
// iterations and assert that after every Start+Shutdown pair the bound
// listener (if any) refuses new connections. The serialization fix has
// Shutdown wait for Start's startTransports to finish before
// closeTransports snapshots the listener set, so any listener Start
// bound is always visible and torn down. Greptile P1 from PR #80
// re-review.
func TestServer_ShutdownDuringStartTearsDownTransports(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	for i := range 50 {
		opts := newTestOptions(t)
		opts.HTTPAddr = "127.0.0.1:0"
		srv, err := New(opts)
		if err != nil {
			t.Fatalf("iter %d: New: %v", i, err)
		}

		startErrCh := make(chan error, 1)
		go func() { startErrCh <- srv.Start(ctx) }()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		shutdownErr := srv.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			t.Fatalf("iter %d: Shutdown: %v", i, shutdownErr)
		}

		select {
		case startErr := <-startErrCh:
			if startErr != nil && !errors.Is(startErr, ErrServerShutdown) {
				t.Fatalf("iter %d: Start: %v", i, startErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: Start did not return after Shutdown", i)
		}

		impl, ok := srv.(*mcpServer)
		if !ok {
			t.Fatalf("Server is %T, want *mcpServer", srv)
		}
		addr := impl.httpListenerAddr()
		if addr == nil {
			continue
		}
		var dialer net.Dialer
		dialCtx, dcancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		conn, dialErr := dialer.DialContext(dialCtx, "tcp", addr.String())
		dcancel()
		if dialErr == nil {
			if err := conn.Close(); err != nil {
				t.Logf("close probe conn: %v", err)
			}
			t.Fatalf("iter %d: HTTP listener at %s still accepts connections after Shutdown", i, addr)
		}
	}
}

// TestServer_StartUnixClearsStaleSocket pins the probe-and-remove path
// against a crashed-prior-daemon scenario: a leftover socket file from
// a process that died before Shutdown ran. clearStaleUnixSocket should
// detect that no peer accepts on the path and remove it before bind.
// Greptile P1 from PR #80 review.
func TestServer_StartUnixClearsStaleSocket(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	// Unix socket paths cap at 104 bytes on macOS / 108 on Linux, so the
	// default t.TempDir() (under /var/folders/...) is too long. Use a
	// short /tmp path and clean up explicitly.
	sockDir, err := os.MkdirTemp("/tmp", "mcp-stale-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(sockDir); err != nil {
			t.Logf("cleanup sockDir: %v", err)
		}
	})
	sockPath := filepath.Join(sockDir, "s.sock")

	// Create a stale socket by binding then closing without unlinking.
	// net.UnixListener auto-unlinks on Close by default; SetUnlinkOnClose
	// disables that so the socket file survives as a stale-process
	// artifact for clearStaleUnixSocket to detect.
	var seedConfig net.ListenConfig
	staleLn, err := seedConfig.Listen(ctx, "unix", sockPath)
	if err != nil {
		t.Fatalf("seed stale socket: %v", err)
	}
	unixLn, ok := staleLn.(*net.UnixListener)
	if !ok {
		t.Fatalf("seed listener is %T, want *net.UnixListener", staleLn)
	}
	unixLn.SetUnlinkOnClose(false)
	if err := unixLn.Close(); err != nil {
		t.Fatalf("close seed listener: %v", err)
	}
	if _, statErr := os.Stat(sockPath); statErr != nil {
		t.Fatalf("stale socket missing post-seed: %v", statErr)
	}

	opts := newTestOptions(t)
	opts.UnixSocket = sockPath
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Logf("cleanup Shutdown: %v", err)
		}
		<-startErrCh
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		dialCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		var dialer net.Dialer
		conn, dialErr := dialer.DialContext(dialCtx, "unix", sockPath)
		cancel()
		if dialErr == nil {
			if err := conn.Close(); err != nil {
				t.Fatalf("close probe conn: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server did not bind on cleared stale socket %s within 2s", sockPath)
}

// TestHooks_DistinctSessionsDontCollide pins the session-scoped key in
// invocationKey. With a JSON-RPC-id-only key, two concurrent sessions
// reusing id=1 would collide in the start-time map: the second `before`
// would overwrite the first, and the first `after` would consume the
// only entry, leaving the second with duration_ms=0. The
// session-prefixed key keeps both invocations measurable. Greptile P2
// from PR #80 review.
func TestHooks_DistinctSessionsDontCollide(t *testing.T) {
	t.Parallel()
	h := &loggingHooks{logger: zap.NewNop()}

	sessionA := stubClientSession{id: "session-a"}
	sessionB := stubClientSession{id: "session-b"}
	innerSrv := mcpserver.NewMCPServer("collision-test", "0.0.0")
	ctxA := innerSrv.WithContext(context.Background(), sessionA)
	ctxB := innerSrv.WithContext(context.Background(), sessionB)

	jsonRPCID := 1
	h.before(ctxA, jsonRPCID, nil)
	time.Sleep(5 * time.Millisecond)
	h.before(ctxB, jsonRPCID, nil)

	durA := h.loadDuration(ctxA, jsonRPCID)
	durB := h.loadDuration(ctxB, jsonRPCID)

	if durA == 0 {
		t.Fatalf("session A duration = 0; expected nonzero (collision overwrote start)")
	}
	if durB == 0 {
		t.Fatalf("session B duration = 0; expected nonzero (collision dropped entry)")
	}
	if durA < durB {
		t.Fatalf("session A (older) duration %v < session B (newer) %v; ordering violated", durA, durB)
	}
}

// stubClientSession is the minimum mcpserver.ClientSession implementation
// for hook tests. It just returns a fixed SessionID so invocationKey
// receives the expected disambiguator without needing a real transport.
type stubClientSession struct{ id string }

func (s stubClientSession) SessionID() string { return s.id }
func (s stubClientSession) NotificationChannel() chan<- mcpgo.JSONRPCNotification {
	return nil
}
func (s stubClientSession) Initialize()       {}
func (s stubClientSession) Initialized() bool { return true }

func TestNew_ValidatesRequiredFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{"missing name", Options{Version: "1", Stdio: true}, "Name required"},
		{"missing version", Options{Name: "x", Stdio: true}, "Version required"},
		{"no transport", Options{Name: "x", Version: "1"}, "at least one transport"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.opts)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestServer_RegisterRejectsDuplicates(t *testing.T) {
	t.Parallel()
	opts := newTestOptions(t)
	opts.Stdio = true
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Register(echoTool{name: "echo"}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err = srv.Register(echoTool{name: "echo"})
	if !errors.Is(err, ErrToolAlreadyRegistered) {
		t.Fatalf("second Register err = %v, want ErrToolAlreadyRegistered", err)
	}
}

func TestServer_RegisterRejectsNil(t *testing.T) {
	t.Parallel()
	opts := newTestOptions(t)
	opts.Stdio = true
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Register(nil); err == nil {
		t.Fatalf("Register(nil) = nil, want error")
	}
}

func TestServer_StdioRoundtrip(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	opts := newTestOptions(t)
	opts.Stdio = true
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("New returned %T, want *mcpServer", srv)
	}
	impl.stdin = stdinR
	impl.stdout = stdoutW

	if err := srv.Register(echoTool{name: "echo"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()

	scanner := bufio.NewScanner(stdoutR)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if err := writeJSONLine(stdinW, jsonRPCInitialize(1)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	initResp := readJSONResponse(t, scanner)
	if initResp["error"] != nil {
		t.Fatalf("initialize returned error: %v", initResp["error"])
	}

	if err := writeJSONLine(stdinW, jsonRPCToolsCall(2, "echo", map[string]any{"greeting": "hello"})); err != nil {
		t.Fatalf("write tools/call: %v", err)
	}
	callResp := readJSONResponse(t, scanner)
	if callResp["error"] != nil {
		t.Fatalf("tools/call returned error: %v", callResp["error"])
	}
	if got := extractStructuredGreeting(t, callResp); got != "hello" {
		t.Fatalf("echo round-trip greeting = %q, want %q", got, "hello")
	}

	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}

	if err := <-startErrCh; err != nil {
		t.Fatalf("Start returned: %v", err)
	}
}

func TestServer_StartStopHTTP(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	opts := newTestOptions(t)
	opts.HTTPAddr = "127.0.0.1:0"
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Register(echoTool{name: "echo"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()

	addr := waitForHTTPListener(t, srv)
	endpoint := "http://" + addr + "/mcp"

	sessionID := initializeHTTPSession(t, endpoint)
	greeting := callEchoOverHTTP(t, endpoint, sessionID, "ola")
	if greeting != "ola" {
		t.Fatalf("echo HTTP greeting = %q, want %q", greeting, "ola")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-startErrCh; err != nil {
		t.Fatalf("Start returned: %v", err)
	}
}

func TestServer_ShutdownDeadlineExceeded(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	opts := newTestOptions(t)
	opts.HTTPAddr = "127.0.0.1:0"
	opts.ShutdownTimeout = 100 * time.Millisecond
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tool := blockingTool{
		name:    "block",
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	if err := srv.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()

	addr := waitForHTTPListener(t, srv)
	endpoint := "http://" + addr + "/mcp"
	sessionID := initializeHTTPSession(t, endpoint)

	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		fireAndForgetCall(t.Context(), endpoint, sessionID, jsonRPCToolsCall(99, "block", nil))
	}()
	select {
	case <-tool.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("tool did not start within 2s")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	if err == nil {
		t.Fatalf("Shutdown(deadline) returned nil, want context.DeadlineExceeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown(deadline) err = %v, want context.DeadlineExceeded", err)
	}

	// Release the tool so the leaked in-flight call goroutine can unwind.
	close(tool.release)
	<-callDone
	<-startErrCh
}

// fireAndForgetCall issues a JSON-RPC tool call and ignores the result.
// Used by tests that deliberately tear down the server mid-call: the
// call's eventual failure (connection reset, EOF, etc.) is expected and
// must not fail the test.
func fireAndForgetCall(ctx context.Context, endpoint, sessionID string, msg map[string]any) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+testHTTPToken)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func TestServer_ConcurrentInvocationRace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race-style test in short mode")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)

	opts := newTestOptions(t)
	opts.HTTPAddr = "127.0.0.1:0"
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Register(echoTool{name: "echo"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()
	defer func() { <-startErrCh }()

	addr := waitForHTTPListener(t, srv)
	endpoint := "http://" + addr + "/mcp"

	const callers = 20
	const callsPerCaller = 5

	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func(caller int) {
			defer wg.Done()
			sessionID := initializeHTTPSession(t, endpoint)
			for j := range callsPerCaller {
				_ = callEchoOverHTTP(t, endpoint, sessionID, fmt.Sprintf("c%d-%d", caller, j))
			}
		}(i)
	}
	wg.Wait()

	// Close client-side idle keepalives so the server's idle connections
	// detach immediately instead of being held by the shared default
	// transport pool. Without this, http.Server.Shutdown polls (with an
	// exponential backoff capped at 500ms) until the client side closes,
	// which under -race load can blow past a short test deadline.
	http.DefaultClient.CloseIdleConnections()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestServer_ShutdownIdempotent(t *testing.T) {
	t.Parallel()
	opts := newTestOptions(t)
	opts.HTTPAddr = "127.0.0.1:0"
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(testutil.TestContext(t)) }()
	waitForHTTPListener(t, srv)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	<-startErrCh
}

// waitForHTTPListener spins until the HTTP listener is bound, returning
// its host:port. Reports through t.Fatalf if the listener fails to bind
// within a generous timeout.
func waitForHTTPListener(t *testing.T, srv Server) string {
	t.Helper()
	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("Server is %T, want *mcpServer", srv)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if addr := impl.httpListenerAddr(); addr != nil {
			return addr.String()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("HTTP listener did not bind within 2s")
	return ""
}

// callToolRequest builds a CallToolRequest suitable for handing
// directly to a ToolHandlerFunc, bypassing the JSON-RPC layer. Used by
// benchmarks that target the adapter hot path without transport
// overhead.
func callToolRequest(name string, args map[string]any) mcpgo.CallToolRequest {
	return mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{
			Name:      name,
			Arguments: args,
		},
	}
}

// jsonRPCResultText concatenates the text content blocks of a decoded
// tools/call result map. Error results carry their [CODE] message here, never
// in structuredContent (see toolErrorResult).
func jsonRPCResultText(result map[string]any) string {
	var sb strings.Builder
	content, _ := result["content"].([]any)
	for _, b := range content {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["text"].(string); t != "" {
			sb.WriteString(t)
		}
	}
	return sb.String()
}

// jsonRPCInitialize builds the canonical MCP initialize request body.
func jsonRPCInitialize(id int) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"clientInfo":      map[string]any{"name": "mcp-test-client", "version": "0.0.0"},
			"capabilities":    map[string]any{},
		},
	}
}

// jsonRPCToolsCall builds a tools/call request body.
func jsonRPCToolsCall(id int, name string, args any) map[string]any {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  params,
	}
}

// writeJSONLine marshals msg and writes it as a single newline-terminated
// line to w. Mirrors the wire encoding mcp-go's stdio transport expects.
func writeJSONLine(w io.Writer, msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// readJSONResponse blocks until the next line on scanner contains a
// JSON-RPC response, then decodes it. A scanner failure or non-JSON
// payload fails the test.
func readJSONResponse(t *testing.T, scanner *bufio.Scanner) map[string]any {
	t.Helper()
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			t.Fatalf("scanner.Scan: %v", err)
		}
		t.Fatal("scanner.Scan returned false; pipe closed")
	}
	var resp map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", scanner.Bytes(), err)
	}
	return resp
}

// initializeHTTPSession runs the MCP initialize handshake against
// endpoint using the default HTTP client.
func initializeHTTPSession(t *testing.T, endpoint string) string {
	t.Helper()
	return initializeHTTPSessionWithClient(t, http.DefaultClient, endpoint)
}

// initializeHTTPSessionWithClient runs the MCP initialize handshake
// against endpoint using the supplied client, so a Unix-socket test
// can reuse the same handshake logic by swapping in a client whose
// Transport dials a Unix path.
func initializeHTTPSessionWithClient(t *testing.T, client *http.Client, endpoint string) string {
	t.Helper()
	resp, raw := sendJSONRPCRawWithClient(t, client, endpoint, "", jsonRPCInitialize(1))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize HTTP status = %d, want 200; body=%s", resp.StatusCode, raw)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatalf("initialize response missing Mcp-Session-Id header")
	}
	return sessionID
}

// callEchoOverHTTP sends a tools/call for the echo tool over endpoint
// using the default HTTP client.
func callEchoOverHTTP(t *testing.T, endpoint, sessionID, greeting string) string {
	t.Helper()
	return callEchoOverHTTPWithClient(t, http.DefaultClient, endpoint, sessionID, greeting)
}

// callEchoOverHTTPWithClient sends a tools/call using the supplied
// HTTP client. Mirrors callEchoOverHTTP but lets callers thread in a
// client backed by a Unix-socket dialer.
func callEchoOverHTTPWithClient(t *testing.T, client *http.Client, endpoint, sessionID, greeting string) string {
	t.Helper()
	body := sendJSONRPCWithClient(t, client, endpoint, sessionID, jsonRPCToolsCall(2, "echo", map[string]any{"greeting": greeting}))
	return extractStructuredGreeting(t, body)
}

// sendJSONRPCWithClient POSTs msg using the supplied client and returns
// the decoded response body.
func sendJSONRPCWithClient(t *testing.T, client *http.Client, endpoint, sessionID string, msg map[string]any) map[string]any {
	t.Helper()
	resp, raw := sendJSONRPCRawWithClient(t, client, endpoint, sessionID, msg)
	defer resp.Body.Close()
	return decodeStreamableResponse(t, resp, raw)
}

// sendJSONRPCRawWithClient POSTs msg and returns the response plus the
// raw body bytes. The caller closes resp.Body.
func sendJSONRPCRawWithClient(t *testing.T, client *http.Client, endpoint, sessionID string, msg map[string]any) (*http.Response, []byte) {
	t.Helper()
	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+testHTTPToken)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, raw
}

// decodeStreamableResponse parses either an application/json response
// body or an SSE-framed JSON-RPC payload. mcp-go's streamable transport
// chooses the framing based on Accept; we accept both for parity with
// real clients.
func decodeStreamableResponse(t *testing.T, resp *http.Response, raw []byte) map[string]any {
	t.Helper()
	switch {
	case strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json"):
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("json decode: %v; body=%s", err, raw)
		}
		return out
	case strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"):
		return decodeSSEMessage(t, raw)
	default:
		t.Fatalf("unexpected Content-Type %q; body=%s", resp.Header.Get("Content-Type"), raw)
		return nil
	}
}

// decodeSSEMessage extracts the first JSON-RPC message from an SSE
// frame. Mcp-go emits one event per response with the JSON payload on
// the "data:" line.
func decodeSSEMessage(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	for line := range strings.SplitSeq(string(raw), "\n") {
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimSpace(data)
			if data == "" {
				continue
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(data), &out); err != nil {
				t.Fatalf("sse decode %q: %v", data, err)
			}
			return out
		}
	}
	t.Fatalf("no SSE data line in: %s", raw)
	return nil
}

// extractStructuredGreeting reaches into a tools/call result and pulls
// the "greeting" field from the structured content, which the echo tool
// emits verbatim from its input.
func extractStructuredGreeting(t *testing.T, resp map[string]any) string {
	t.Helper()
	if err, ok := resp["error"]; ok && err != nil {
		t.Fatalf("tools/call returned error: %v", err)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result: %v", resp)
	}
	if sc, ok := result["structuredContent"].(map[string]any); ok {
		if g, ok := sc["greeting"].(string); ok {
			return g
		}
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result missing content: %v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] not an object: %v", content[0])
	}
	text, ok := first["text"].(string)
	if !ok {
		t.Fatalf("content[0].text missing: %v", first)
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(text), &inner); err != nil {
		t.Fatalf("text not JSON: %v; got %q", err, text)
	}
	if g, ok := inner["greeting"].(string); ok {
		return g
	}
	t.Fatalf("greeting missing in: %v", inner)
	return ""
}

// probeTool records whether Invoke was entered and how often. Used by
// schema-validation tests to assert that pre-handler validation aborts
// dispatch before the tool body runs.
type probeTool struct {
	name   string
	schema json.RawMessage
	mu     sync.Mutex
	calls  int
}

func (p *probeTool) Name() string                  { return p.name }
func (p *probeTool) Description() string           { return "probe" }
func (p *probeTool) InputSchema() json.RawMessage  { return p.schema }
func (p *probeTool) OutputSchema() json.RawMessage { return nil }
func (p *probeTool) Invoke(_ context.Context, req json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return req, nil
}

func (p *probeTool) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestServer_RegisterDuplicateWithBadSchemaReturnsDuplicate pins the
// documented contract that a second registration of an already-known
// name returns ErrToolAlreadyRegistered, even when the duplicate
// happens to ship a malformed schema. The dup-check must run before
// schema compilation so callers that route on
// errors.Is(err, ErrToolAlreadyRegistered) (e.g. plugin reload) keep
// working. Greptile P2 from PR #90 review.
func TestServer_RegisterDuplicateWithBadSchemaReturnsDuplicate(t *testing.T) {
	t.Parallel()
	opts := newTestOptions(t)
	opts.Stdio = true
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Register(echoTool{name: "echo"}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	duplicate := &probeTool{
		name:   "echo",
		schema: json.RawMessage(`{"type":"not-a-real-type"}`),
	}
	err = srv.Register(duplicate)
	if !errors.Is(err, ErrToolAlreadyRegistered) {
		t.Fatalf("duplicate Register err = %v, want ErrToolAlreadyRegistered", err)
	}
}

// TestServer_RegisterCompilesSchemaOnce pins the registration-time
// validation gate: a tool whose InputSchema is not a valid JSON Schema
// must fail Register and never reach the in-memory tool map, so the
// server never accepts traffic for a tool with a broken contract.
func TestServer_RegisterCompilesSchemaOnce(t *testing.T) {
	t.Parallel()
	opts := newTestOptions(t)
	opts.Stdio = true
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bad := &probeTool{
		name:   "bad-schema",
		schema: json.RawMessage(`{"type":"not-a-real-type"}`),
	}
	err = srv.Register(bad)
	if err == nil {
		t.Fatalf("Register with invalid schema = nil, want error")
	}
	if !strings.Contains(err.Error(), "compile input schema") {
		t.Fatalf("error %q does not mention compile failure", err.Error())
	}

	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("Server is %T, want *mcpServer", srv)
	}
	impl.toolsMu.Lock()
	_, registered := impl.tools[bad.name]
	impl.toolsMu.Unlock()
	if registered {
		t.Fatalf("bad-schema tool present in registry after failed Register")
	}
}

// TestServer_ToolsListIncludesRegistered pins the criterion that the
// tool catalogue is observable per the MCP spec: every registered tool
// must appear in the tools/list response.
func TestServer_ToolsListIncludesRegistered(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	opts := newTestOptions(t)
	opts.Stdio = true
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("Server is %T, want *mcpServer", srv)
	}
	impl.stdin = stdinR
	impl.stdout = stdoutW

	if err := srv.Register(echoTool{name: "echo"}); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	if err := srv.Register(echoTool{name: "ping"}); err != nil {
		t.Fatalf("Register ping: %v", err)
	}

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()

	scanner := bufio.NewScanner(stdoutR)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if err := writeJSONLine(stdinW, jsonRPCInitialize(1)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if got := readJSONResponse(t, scanner); got["error"] != nil {
		t.Fatalf("initialize returned error: %v", got["error"])
	}

	listReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	}
	if err := writeJSONLine(stdinW, listReq); err != nil {
		t.Fatalf("write tools/list: %v", err)
	}
	resp := readJSONResponse(t, scanner)
	if resp["error"] != nil {
		t.Fatalf("tools/list returned error: %v", resp["error"])
	}

	names := toolNamesFromListResponse(t, resp)
	for _, want := range []string{"echo", "ping"} {
		if !slices.Contains(names, want) {
			t.Fatalf("tools/list missing %q; got %v", want, names)
		}
	}

	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
	if err := <-startErrCh; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// TestServer_UnlistedToolHiddenButDispatchable pins the UnlistedTool
// contract that backs hiding `ask` from agents: a tool that reports
// Unlisted()==true must be absent from tools/list yet still callable by name
// via tools/call. A regression that applied the list filter to dispatch (or
// dropped the handler entirely) would break `maz ask`, which routes a
// tools/call through the daemon.
func TestServer_UnlistedToolHiddenButDispatchable(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	opts := newTestOptions(t)
	opts.Stdio = true
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("Server is %T, want *mcpServer", srv)
	}
	impl.stdin = stdinR
	impl.stdout = stdoutW

	if err := srv.Register(echoTool{name: "echo"}); err != nil {
		t.Fatalf("Register echo: %v", err)
	}
	if err := srv.Register(unlistedEchoTool{echoTool{name: "secret"}}); err != nil {
		t.Fatalf("Register secret: %v", err)
	}

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()

	scanner := bufio.NewScanner(stdoutR)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if err := writeJSONLine(stdinW, jsonRPCInitialize(1)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if got := readJSONResponse(t, scanner); got["error"] != nil {
		t.Fatalf("initialize returned error: %v", got["error"])
	}

	// tools/list must include the visible tool and exclude the unlisted one.
	if err := writeJSONLine(stdinW, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{},
	}); err != nil {
		t.Fatalf("write tools/list: %v", err)
	}
	listResp := readJSONResponse(t, scanner)
	if listResp["error"] != nil {
		t.Fatalf("tools/list returned error: %v", listResp["error"])
	}
	names := toolNamesFromListResponse(t, listResp)
	if !slices.Contains(names, "echo") {
		t.Fatalf("tools/list missing visible tool echo; got %v", names)
	}
	if slices.Contains(names, "secret") {
		t.Fatalf("tools/list leaked the unlisted tool secret; got %v", names)
	}

	// tools/call on the unlisted tool must still dispatch (echo back args).
	if err := writeJSONLine(stdinW, jsonRPCToolsCall(3, "secret", map[string]any{"k": "v"})); err != nil {
		t.Fatalf("write tools/call: %v", err)
	}
	callResp := readJSONResponse(t, scanner)
	if callResp["error"] != nil {
		t.Fatalf("tools/call secret returned transport error: %v", callResp["error"])
	}
	result, ok := callResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call secret: result is %T, want object (got %v)", callResp["result"], callResp)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tools/call secret dispatched to an error result; the unlisted tool must remain callable: %v", result)
	}

	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
	if err := <-startErrCh; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// TestServer_InvalidArgumentsReturnValidation pins the schema-validation
// path: a tools/call whose arguments fail the InputSchema returns an
// IsError CallToolResult carrying code E_VALIDATION and never enters
// the tool's Invoke body.
func TestServer_InvalidArgumentsReturnValidation(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	opts := newTestOptions(t)
	opts.Stdio = true
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("Server is %T, want *mcpServer", srv)
	}
	impl.stdin = stdinR
	impl.stdout = stdoutW

	tool := &probeTool{
		name: "strict",
		schema: json.RawMessage(`{
			"type":"object",
			"properties":{"fact":{"type":"string"}},
			"required":["fact"]
		}`),
	}
	if err := srv.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()

	scanner := bufio.NewScanner(stdoutR)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if err := writeJSONLine(stdinW, jsonRPCInitialize(1)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if got := readJSONResponse(t, scanner); got["error"] != nil {
		t.Fatalf("initialize returned error: %v", got["error"])
	}

	if err := writeJSONLine(stdinW, jsonRPCToolsCall(2, "strict", map[string]any{})); err != nil {
		t.Fatalf("write tools/call: %v", err)
	}
	resp := readJSONResponse(t, scanner)
	if resp["error"] != nil {
		t.Fatalf("tools/call envelope returned JSON-RPC error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result: %v", resp)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("result.isError = false, want true; result=%v", result)
	}
	// Error results carry the code in TEXT content and never in
	// structuredContent — a structured error envelope would be validated by a
	// strict client against the success output schema and rejected (#4).
	if result["structuredContent"] != nil {
		t.Fatalf("error result has structuredContent %#v, want nil", result["structuredContent"])
	}
	if text := jsonRPCResultText(result); !strings.Contains(text, string(CodeValidation)) {
		t.Fatalf("result text = %q, want it to contain %q", text, CodeValidation)
	}
	if got := tool.Calls(); got != 0 {
		t.Fatalf("probe.Invoke entered %d times, want 0", got)
	}

	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
	if err := <-startErrCh; err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// TestDefaultHTTPAddr pins the documented loopback default the daemon
// supervisor (#42) and the foreground debug CLI both consume. A change
// would silently move the well-known port and break agent
// configurations referencing 127.0.0.1:7842.
func TestDefaultHTTPAddr(t *testing.T) {
	t.Parallel()
	if DefaultHTTPAddr != "127.0.0.1:7842" {
		t.Fatalf("DefaultHTTPAddr = %q, want %q", DefaultHTTPAddr, "127.0.0.1:7842")
	}
}

// TestDefaultUnixSocket pins the data-dir composition. The supervisor
// and CLI both build their default socket path through this helper so
// no caller reinvents the join (and risks a mismatch with the
// Shutdown-time unlink path).
func TestDefaultUnixSocket(t *testing.T) {
	t.Parallel()
	got := DefaultUnixSocket("/var/lib/maz")
	want := filepath.Join("/var/lib/maz", "mazarbul.sock")
	if got != want {
		t.Fatalf("DefaultUnixSocket = %q, want %q", got, want)
	}
}

// TestServer_HealthzReturnsOK pins the HTTP transport's liveness probe
// contract: 200 OK with Content-Type application/json and a body shape
// of {"status":"ok","startTime":"<RFC3339>"}. The startTime field must
// reflect the time Start ran so consumers can compute uptime as
// (now - startTime), matching Prometheus's "export the timestamp"
// guidance.
func TestServer_HealthzReturnsOK(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	opts := newTestOptions(t)
	opts.HTTPAddr = "127.0.0.1:0"
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before := time.Now()
	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()
	addr := waitForHTTPListener(t, srv)

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Logf("cleanup Shutdown: %v", err)
		}
		<-startErrCh
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+HealthPath, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q, want %q", body.Status, "ok")
	}
	if body.StartTime.Before(before) {
		t.Fatalf("startTime %v < pre-start %v", body.StartTime, before)
	}
	if body.StartTime.After(time.Now()) {
		t.Fatalf("startTime %v in the future", body.StartTime)
	}
	// The PID lets a supervisor confirm the process answering /healthz is
	// the one it expects (e.g. cross-checking a launchd job PID), so it
	// must be the serving process's own id.
	if body.PID != os.Getpid() {
		t.Fatalf("pid = %d, want serving process pid %d", body.PID, os.Getpid())
	}
}

// TestServer_HealthzRejectsNonGET pins the documented `GET /healthz`
// contract: any other method must surface 405 Method Not Allowed with
// an Allow: GET response header. Without the method check, a probe
// firing POST /healthz would observe a healthy 200 response even
// though the verb is wrong, masking misconfiguration. Greptile P2 on
// PR #100.
func TestServer_HealthzRejectsNonGET(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	opts := newTestOptions(t)
	opts.HTTPAddr = "127.0.0.1:0"
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()
	addr := waitForHTTPListener(t, srv)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Logf("cleanup Shutdown: %v", err)
		}
		<-startErrCh
	})

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+HealthPath, nil)
		if err != nil {
			t.Fatalf("NewRequest %s: %v", method, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do %s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s /healthz status = %d, want 405", method, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != http.MethodGet {
			t.Fatalf("%s /healthz Allow header = %q, want %q", method, got, http.MethodGet)
		}
	}
}

// Healthz is served on BOTH transports now that TCP is off by default and
// first-party liveness probes go over the Unix socket; the socket /healthz
// route is covered by TestUnixTransport_ServesHealthz in middleware_test.go.

// TestServer_UnixSocketFileMode0600 pins #39 AC3a: the socket file mode
// must be 0600 once bind + chmod return, so the inode is reachable only
// by the owning user even when the process umask would have left it
// world-readable.
func TestServer_UnixSocketFileMode0600(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	sockDir, err := os.MkdirTemp("/tmp", "mcp-mode-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(sockDir); err != nil {
			t.Logf("cleanup sockDir: %v", err)
		}
	})
	sockPath := filepath.Join(sockDir, "s.sock")

	opts := newTestOptions(t)
	opts.UnixSocket = sockPath
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Logf("cleanup Shutdown: %v", err)
		}
		<-startErrCh
	})
	waitForUnixListener(t, sockPath)

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat %s: %v", sockPath, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %o, want 0600", got)
	}
}

// waitForUnixListener spins until the Unix socket at path accepts a
// connection. Mirrors waitForHTTPListener for Unix tests.
func waitForUnixListener(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var dialer net.Dialer
	for time.Now().Before(deadline) {
		dialCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		conn, err := dialer.DialContext(dialCtx, "unix", path)
		cancel()
		if err == nil {
			if err := conn.Close(); err != nil {
				t.Fatalf("close probe: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Unix listener did not bind at %s within 2s", path)
}

// TestServer_StdinEOFExitsStart pins the MCP stdio contract: when the
// client closes stdin (EOF), Start must return nil promptly — the server
// must not hang until SIGTERM. This is the root cause of orphaned
// "maz mcp serve" processes when an editor/agent that spawned the server
// quits without sending SIGTERM.
//
// The test provides an already-closed reader as stdin so stdio.Listen
// returns immediately, then asserts Start unblocks within a generous
// deadline. A regression hangs until the test's context expires and
// the bounded select fires t.Fatal.
func TestServer_StdinEOFExitsStart(t *testing.T) {
	t.Parallel()

	// Use a context with a long deadline — if Start hangs it is the
	// bounded select below that catches it, not a context cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// pr is a pipe whose write end we close immediately, giving the
	// server an stdin that is already at EOF.
	pr, pw := io.Pipe()
	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}

	opts := newTestOptions(t)
	opts.Stdio = true
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	impl, ok := srv.(*mcpServer)
	if !ok {
		t.Fatalf("Server is %T, want *mcpServer", srv)
	}
	// Discard stdout — we only care that Start returns, not what it writes.
	impl.stdin = pr
	impl.stdout = io.Discard

	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned non-nil error on stdin EOF: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return within 5s after stdin EOF; server is hanging")
	}
}

// toolNamesFromListResponse extracts the tool names from a tools/list
// JSON-RPC response.
func toolNamesFromListResponse(t *testing.T, resp map[string]any) []string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result: %v", resp)
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("result missing tools: %v", result)
	}
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		entry, ok := tool.(map[string]any)
		if !ok {
			t.Fatalf("tool entry not an object: %v", tool)
		}
		name, ok := entry["name"].(string)
		if !ok {
			t.Fatalf("tool entry missing name: %v", entry)
		}
		out = append(out, name)
	}
	return out
}

// TestServer_AdminRoutes_UnixOnly verifies the structural isolation of the
// admin surface: /admin/ routes are reachable on the Unix socket transport
// and return 404 (not found, not even the path) on the TCP transport.
func TestServer_AdminRoutes_UnixOnly(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "a.sock")

	// Build a minimal admin handler that always returns 200 with a marker body.
	adminHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"admin":true}`))
	})

	opts := newTestOptions(t)
	opts.UnixSocket = sockPath
	// Also enable TCP so we can probe both transports.
	opts.HTTPAddr = "127.0.0.1:0"
	opts.AdminRoutes = adminHandler

	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startErrCh := make(chan error, 1)
	go func() { startErrCh <- srv.Start(ctx) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Logf("Shutdown: %v", err)
		}
		<-startErrCh
	})

	// Wait for both listeners.
	waitForUnixListener(t, sockPath)
	tcpAddr := waitForHTTPListener(t, srv)

	// --- Unix socket: /admin/ path must be reachable (200) ---
	unixClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(dialCtx, "unix", sockPath)
			},
		},
	}
	unixReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://daemon/admin/v1/events:append", nil)
	unixResp, err := unixClient.Do(unixReq)
	if err != nil {
		t.Fatalf("unix POST /admin/v1/events:append: %v", err)
	}
	_ = unixResp.Body.Close()
	if unixResp.StatusCode != http.StatusOK {
		t.Errorf("unix /admin/: status = %d, want 200", unixResp.StatusCode)
	}

	// --- TCP: /admin/ path must NOT be served by the admin handler ---
	// The TCP mux has a catch-all "/" that routes unmatched paths to the
	// streamable MCP handler; it does NOT mount "/admin/". We verify that the
	// admin marker JSON produced by adminHandler is absent from the response,
	// confirming that the admin surface did not run.
	tcpURL := "http://" + tcpAddr + "/admin/v1/events:append"
	tcpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, tcpURL, nil)
	tcpReq.Header.Set("Authorization", "Bearer "+testHTTPToken)
	tcpResp, err := http.DefaultClient.Do(tcpReq)
	if err != nil {
		t.Fatalf("tcp POST /admin/: %v", err)
	}
	tcpBody, _ := io.ReadAll(tcpResp.Body)
	_ = tcpResp.Body.Close()
	if strings.Contains(string(tcpBody), `"admin":true`) {
		t.Errorf("tcp /admin/: admin handler ran on TCP (status=%d body=%s); admin must be absent from TCP",
			tcpResp.StatusCode, tcpBody)
	}
}
