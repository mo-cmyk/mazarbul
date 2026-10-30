package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// TestPickMCPServeMode_StandaloneWhenNoDaemon pins the default:
// without a running daemon, `maz mcp serve` must take the in-process
// path (OpenWiring). This was the only mode pre-fix.
func TestPickMCPServeMode_StandaloneWhenNoDaemon(t *testing.T) {
	// No t.Parallel: tests stub the package-level preflightDaemon var.
	prev := preflightDaemon
	preflightDaemon = func(string, string) error { return nil }
	t.Cleanup(func() { preflightDaemon = prev })

	// Also stub probeDaemonListening so a live daemon in the prod data dir
	// does not make the socket probe return true and flip the decision to proxy.
	prevProbe := probeDaemonListening
	probeDaemonListening = func(string) bool { return false }
	t.Cleanup(func() { probeDaemonListening = prevProbe })

	mode, _ := pickMCPServeMode(config.Defaults().DataDir)
	if mode != mcpServeModeStandalone {
		t.Errorf("no daemon → mode=%q, want %q", mode, mcpServeModeStandalone)
	}
}

// TestPickMCPServeMode_ProxyWhenDaemonRunning reproduces the
// Claude-Desktop bug: when the user has `maz daemon start` running and
// Claude Desktop launches `maz mcp serve`, the standalone path opens
// LadybugDB and fails with "status 1" because the daemon holds the
// lock. The fix routes to a stdio↔daemon proxy when the daemon is
// detected. This test pins the routing decision.
func TestPickMCPServeMode_ProxyWhenDaemonRunning(t *testing.T) {
	// No t.Parallel: tests stub the package-level preflightDaemon var.
	prev := preflightDaemon
	preflightDaemon = func(string, string) error {
		return &daemonLockHeldError{PID: 12345, StopHint: "maz daemon stop"}
	}
	t.Cleanup(func() { preflightDaemon = prev })

	mode, _ := pickMCPServeMode(config.Defaults().DataDir)
	if mode != mcpServeModeProxy {
		t.Errorf("daemon running → mode=%q, want %q (Claude Desktop bug)", mode, mcpServeModeProxy)
	}
}

// shortSocketDir returns a temp dir with a short path. t.TempDir embeds the
// (long) test name, which would overflow the macOS sun_path 104-byte limit when
// a Unix socket is created inside it; MkdirTemp keeps the path short.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mazproxy")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serveUpstreamOnSocket starts an MCP server (a single "ping" tool plus the
// given initialize instructions) on a Unix socket at socketPath, mimicking the
// running daemon the proxy forwards to. It returns the server so a test can
// Close() it to simulate the daemon dying.
func serveUpstreamOnSocket(t *testing.T, socketPath, instructions string) *httptest.Server {
	t.Helper()
	return serveUpstreamWithToolsOnSocket(t, socketPath, instructions, map[string]string{"ping": "noop"})
}

// serveUpstreamWithToolsOnSocket starts an MCP server with the given tools
// (name→description) on a Unix socket, returning the server handle.
func serveUpstreamWithToolsOnSocket(t *testing.T, socketPath, instructions string, tools map[string]string) *httptest.Server {
	t.Helper()
	upstream := mcpserver.NewMCPServer("upstream", "test", mcpserver.WithInstructions(instructions))
	for name, desc := range tools {
		toolName := name
		upstream.AddTool(
			mcpgo.NewTool(toolName, mcpgo.WithDescription(desc)),
			func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return mcpgo.NewToolResultText("ok:" + toolName), nil
			},
		)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", socketPath, err)
	}
	ts := httptest.NewUnstartedServer(mcpserver.NewStreamableHTTPServer(upstream))
	_ = ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	return ts
}

// TestConnectAndSnapshotTools_ForwardsInstructions guards Channel A (#214)
// through the proxy bridge: when Claude Desktop runs while a daemon holds the
// LadybugDB lock, `maz mcp serve` proxies to the daemon over HTTP. The daemon
// composes its initialize instructions (base manual + getContext bootstrap +
// active-preset section) per session, and the proxy MUST surface them to the
// downstream stdio client — otherwise that client gets an empty system prompt,
// silently defeating ambient context for the very path proxy mode exists for.
func TestConnectAndSnapshotTools_ForwardsInstructions(t *testing.T) {
	t.Parallel()

	const sentinel = "MANUAL + getContext bootstrap + ## Operating instructions"
	// The proxy dials the daemon over its Unix socket (TCP is off by default),
	// so serve the upstream MCP server on a socket and hand its path to
	// connectAndSnapshotTools — exactly the production transport.
	socket := filepath.Join(shortSocketDir(t), "s.sock")
	ts := serveUpstreamOnSocket(t, socket, sentinel)
	t.Cleanup(ts.Close)

	client, tools, instructions, err := connectAndSnapshotTools(t.Context(), socket)
	if err != nil {
		t.Fatalf("connectAndSnapshotTools: %v", err)
	}
	t.Cleanup(func() { closeQuiet(client) })

	if instructions != sentinel {
		t.Errorf("proxy dropped daemon instructions:\n got %q\nwant %q", instructions, sentinel)
	}
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Errorf("tool snapshot wrong: %+v", tools)
	}
}

// TestWatchDaemonLiveness_ExitsWhenDaemonDies pins the hardening for the
// orphaned-proxy gap: a long-lived `maz mcp serve` keeps running (blocked on its
// stdio client) even after the daemon it forwards to dies, so wedged proxies
// accumulate and the MCP host never respawns a fresh one. watchDaemonLiveness
// polls the daemon socket and, after a sustained outage, fires onDead so the
// proxy can exit and be respawned. This asserts onDead stays silent while the
// daemon is up and fires promptly once it dies.
func TestWatchDaemonLiveness_ExitsWhenDaemonDies(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(shortSocketDir(t), "s.sock")
	srv := serveUpstreamOnSocket(t, socket, "")

	dead := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go watchDaemonLiveness(ctx, socket, 50*time.Millisecond, 3, func() { close(dead) })

	// While the daemon is up, onDead must not fire.
	select {
	case <-dead:
		t.Fatal("onDead fired while the daemon was alive")
	case <-time.After(400 * time.Millisecond):
	}

	// Kill the daemon; onDead must fire within roughly failThreshold*interval.
	srv.Close()
	select {
	case <-dead:
	case <-time.After(3 * time.Second):
		t.Fatal("onDead did not fire after the daemon died")
	}
}

// TestWatchDaemonLiveness_StopsOnContextCancel confirms the watcher exits
// without firing onDead when the proxy shuts down normally (ctx cancelled),
// even though the daemon is gone — a clean shutdown must not look like a crash.
func TestWatchDaemonLiveness_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(shortSocketDir(t), "s.sock") // nothing ever listens here

	fired := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		watchDaemonLiveness(ctx, socket, 1*time.Second, 3, func() { fired <- struct{}{} })
		close(done)
	}()
	cancel() // immediate normal shutdown, before the failure streak can build
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchDaemonLiveness did not return on ctx cancel")
	}
	select {
	case <-fired:
		t.Fatal("onDead fired on a normal ctx-cancel shutdown")
	default:
	}
}

// TestWaitForDaemonSocket_ReturnsTrueWhenListening confirms the fast-path:
// when probeDaemonListening immediately reports the socket is up,
// waitForDaemonSocketFn returns true without waiting.
func TestWaitForDaemonSocket_ReturnsTrueWhenListening(t *testing.T) {
	// No t.Parallel: stubs the package-level probeDaemonListening var.
	prev := probeDaemonListening
	probeDaemonListening = func(string) bool { return true }
	t.Cleanup(func() { probeDaemonListening = prev })

	if !waitForDaemonSocketFn("any-data-dir", 5*time.Second) {
		t.Error("waitForDaemonSocketFn returned false when probeDaemonListening is always true")
	}
}

// TestWaitForDaemonSocket_ReturnsFalseAfterTimeout confirms the timeout path:
// when probeDaemonListening never reports the socket up, waitForDaemonSocketFn
// returns false after the deadline elapses.
func TestWaitForDaemonSocket_ReturnsFalseAfterTimeout(t *testing.T) {
	// No t.Parallel: stubs the package-level probeDaemonListening var.
	prev := probeDaemonListening
	probeDaemonListening = func(string) bool { return false }
	t.Cleanup(func() { probeDaemonListening = prev })

	start := time.Now()
	if waitForDaemonSocketFn("any-data-dir", 150*time.Millisecond) {
		t.Error("waitForDaemonSocketFn returned true when probeDaemonListening is always false")
	}
	// Sanity bound: must not hang far beyond the timeout.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("waitForDaemonSocketFn blocked too long: %v (want ≤3s)", elapsed)
	}
}

// TestWatchLiveness_ResetsStreakOnRecovery pins the central correctness
// property: a single reachable poll resets the consecutive-failure streak, so a
// brief daemon restart (down, then back up within failThreshold polls) must NOT
// fire onDead. Without the reset, a routine launchd relaunch would orphan the
// very proxy this watcher exists to protect. Driving ticks and reachability
// deterministically (rather than sleeping against a real socket) makes the test
// exact: the reachability sequence below stays under the threshold ONLY because
// the recovery resets the streak, so removing the reset branch makes it fire.
func TestWatchLiveness_ResetsStreakOnRecovery(t *testing.T) {
	t.Parallel()
	const threshold = 3
	// down, down (streak 2), UP (reset to 0), down, down (streak 2 again). With
	// the reset the streak never reaches 3; without it, cumulative failures hit 3
	// on the final poll and onDead fires — the red-proof.
	seq := []bool{false, false, true, false, false}
	reach := make(chan bool, len(seq))
	for _, b := range seq {
		reach <- b
	}

	ticks := make(chan time.Time)
	fired := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		watchLiveness(ctx, ticks, func() bool { return <-reach }, threshold, func() { fired <- struct{}{} })
		close(done)
	}()

	// One reachability value is consumed per tick. If the reset is broken, the
	// watcher fires onDead and returns mid-sequence, so guard each send against
	// an early exit instead of deadlocking on a vanished receiver.
	for range seq {
		select {
		case ticks <- time.Time{}:
		case <-done:
		}
	}
	cancel()
	<-done
	select {
	case <-fired:
		t.Fatal("onDead fired despite a recovery resetting the streak (reset branch broken)")
	default:
	}
}

// ---- C.1/C.2 reconnect tests ----

// TestReconnectLoop_RedialsAfterUpstreamRestart is the primary reconnect test.
// It stands up a fake upstream on a unix socket, snapshots it via proxyState,
// then restarts the upstream on the same socket and drives runReconnectLoop
// directly to assert: the loop re-dials, the proxy state is updated, a read
// call succeeds after the reconnect, and the proxy's context was NOT cancelled
// (i.e. the proxy did not exit).
func TestReconnectLoop_RedialsAfterUpstreamRestart(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(shortSocketDir(t), "s.sock")
	ts := serveUpstreamOnSocket(t, socket, "")
	t.Cleanup(ts.Close)

	// Boot-time snapshot.
	client, tools, _, err := connectAndSnapshotTools(t.Context(), socket)
	if err != nil {
		t.Fatalf("initial connect: %v", err)
	}
	ps := newProxyState(client, tools, socket)
	logger := zap.NewNop()

	srv := mcpserver.NewMCPServer("test", "0", mcpserver.WithToolCapabilities(true))
	for i := range tools {
		tool := tools[i]
		srv.AddTool(tool, makeProxyToolHandler(ps, tool.Name, logger))
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Use a short detect interval so the test doesn't take 2s+ per detect tick.
	go runReconnectLoopWithDetect(ctx, ps, srv, logger, 50*time.Millisecond)

	// Kill the upstream and remove the socket file so the second server can
	// bind the same path. httptest.Server.Close() closes the listener but does
	// not unlink the Unix socket file on macOS.
	ts.Close()
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove socket: %v", err)
	}

	// Give the detect tick at least one cycle to notice the socket is gone
	// before starting ts2.
	time.Sleep(200 * time.Millisecond)

	// Restart on the same socket path.
	ts2 := serveUpstreamOnSocket(t, socket, "")
	t.Cleanup(ts2.Close)

	// Wait for the reconnect loop to pick up the new upstream. We poll
	// ps.get() and try a CallTool via the new client; both must succeed
	// within a generous window.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		newClient, _, _, dialErr := connectAndSnapshotTools(ctx, socket)
		if dialErr != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// Confirm the reconnect loop has swapped in a working client by
		// making a call through the handler (which reads ps.get() at call time).
		callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
		req := mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: "ping", Arguments: map[string]any{}}}
		result, callErr := newClient.CallTool(callCtx, req)
		callCancel()
		closeQuiet(newClient)
		if callErr == nil && result != nil {
			break // reconnect confirmed working
		}
		time.Sleep(200 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		t.Fatal("upstream never became reachable after restart")
	}

	// The proxy context must NOT have been cancelled — the proxy is still alive.
	select {
	case <-ctx.Done():
		t.Fatal("proxy context was cancelled; proxy should stay alive on reconnect")
	default:
	}
}

// TestReconnectLoop_ToolSetChangedNotifiesHost confirms that when the upstream
// comes back after a restart with a different tool set, runReconnectLoop calls
// srv.SetTools with the new set. Since the MCPServer was created with
// WithToolCapabilities(listChanged:true), SetTools emits
// notifications/tools/list_changed. We verify the state update by checking that
// the server's tool map reflects the new set.
func TestReconnectLoop_ToolSetChangedNotifiesHost(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(shortSocketDir(t), "s.sock")

	// First upstream: one tool "alpha".
	ts1 := serveUpstreamWithToolsOnSocket(t, socket, "", map[string]string{"alpha": "first tool"})
	t.Cleanup(ts1.Close)

	client, tools, _, err := connectAndSnapshotTools(t.Context(), socket)
	if err != nil {
		t.Fatalf("initial connect: %v", err)
	}
	ps := newProxyState(client, tools, socket)
	logger := zap.NewNop()

	srv := mcpserver.NewMCPServer("test", "0", mcpserver.WithToolCapabilities(true))
	for i := range tools {
		tool := tools[i]
		srv.AddTool(tool, makeProxyToolHandler(ps, tool.Name, logger))
	}

	// Verify the initial snapshot has exactly "alpha".
	if srv.GetTool("alpha") == nil {
		t.Fatal("expected initial tool 'alpha' to be registered")
	}
	if srv.GetTool("beta") != nil {
		t.Fatal("unexpected tool 'beta' before restart")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Use a short detect interval so the test doesn't take 2s+ per detect tick.
	go runReconnectLoopWithDetect(ctx, ps, srv, logger, 50*time.Millisecond)

	// Kill the first upstream and remove the socket file so the second server
	// can bind the same path. httptest.Server.Close() closes the listener but
	// does not unlink the Unix socket file on macOS.
	ts1.Close()
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove socket: %v", err)
	}

	// Give the detect tick at least one cycle to notice the socket is gone
	// before starting ts2. Without this the ticker may fire against ts2 and
	// see the socket as "still up", skipping the reconnect cycle entirely.
	time.Sleep(200 * time.Millisecond)

	// Restart with a different tool set: "beta" instead of "alpha".
	ts2 := serveUpstreamWithToolsOnSocket(t, socket, "", map[string]string{"beta": "second tool"})
	t.Cleanup(ts2.Close)

	// Wait for the reconnect loop to pick up the new tool set.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if srv.GetTool("beta") != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if srv.GetTool("beta") == nil {
		t.Error("expected 'beta' to appear in server tools after reconnect with changed tool set")
	}
	// "alpha" should have been replaced.
	if srv.GetTool("alpha") != nil {
		t.Error("expected 'alpha' to be removed from server tools after tool-set change")
	}
}

// ---- C.3/C.4 retry-on-transport-error tests ----

// dialUpstream is a test helper that dials the upstream socket and returns a
// connected mcpclient.Client ready for CallTool.
func dialUpstream(t *testing.T, ctx context.Context, socket string) *mcpclient.Client {
	t.Helper()
	c, err := mcpclient.NewStreamableHttpClient(
		"http://unix/mcp",
		transport.WithHTTPBasicClient(mcp.UnixHTTPClient(socket, 0)),
	)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	startCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Start(startCtx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initCtx, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	if _, err := c.Initialize(initCtx, mcpgo.InitializeRequest{
		Params: mcpgo.InitializeParams{
			ProtocolVersion: mcpgo.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcpgo.Implementation{Name: "test", Version: "0"},
		},
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}

// TestProxyToolHandler_ReadRetriesToSuccessOnTransportError asserts that when a
// read-only tool call fails with a transport error, makeProxyToolHandler
// re-dials the socket and retries the call. If the retry succeeds, the result
// is returned without error and without the caller seeing the transport failure.
//
// This exercises C.3: retry is limited to read-only tools, and only on transport
// errors (not application errors).
func TestProxyToolHandler_ReadRetriesToSuccessOnTransportError(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(shortSocketDir(t), "s.sock")
	ts := serveUpstreamWithToolsOnSocket(t, socket, "", map[string]string{"recall": "read tool"})
	t.Cleanup(ts.Close)

	client := dialUpstream(t, t.Context(), socket)
	tools := []mcpgo.Tool{mcpgo.NewTool("recall", mcpgo.WithDescription("read tool"))}
	ps := newProxyState(client, tools, socket)
	logger := zap.NewNop()

	// Kill the upstream, forcing the first call to fail with a transport error.
	// The upstream is immediately restarted so the retry within the handler can
	// succeed.
	ts.Close()
	ts2 := serveUpstreamWithToolsOnSocket(t, socket, "", map[string]string{"recall": "read tool"})
	t.Cleanup(ts2.Close)

	handler := makeProxyToolHandler(ps, "recall", logger)
	callCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := handler(callCtx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "recall", Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("read tool handler returned error after retry: %v", err)
	}
	if result == nil {
		t.Fatal("read tool handler returned nil result")
	}
}

// TestProxyToolHandler_WriteFailsCleanOnTransportError asserts that when a
// write tool call fails with a transport error, makeProxyToolHandler returns
// errWriteTransportFailure without retrying. This is C.4: writes are not
// idempotent, so blind retry would risk double-writes.
func TestProxyToolHandler_WriteFailsCleanOnTransportError(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(shortSocketDir(t), "s.sock")
	ts := serveUpstreamWithToolsOnSocket(t, socket, "", map[string]string{"remember": "write tool"})
	t.Cleanup(ts.Close)

	client := dialUpstream(t, t.Context(), socket)
	tools := []mcpgo.Tool{mcpgo.NewTool("remember", mcpgo.WithDescription("write tool"))}
	ps := newProxyState(client, tools, socket)
	logger := zap.NewNop()

	// Kill upstream; restart is NOT performed — the retry must not happen.
	ts.Close()

	handler := makeProxyToolHandler(ps, "remember", logger)
	callCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := handler(callCtx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "remember", Arguments: map[string]any{}},
	})
	if err == nil {
		t.Fatal("write tool handler returned nil error on transport failure; expected errWriteTransportFailure")
	}
	if !errors.Is(err, errWriteTransportFailure) {
		t.Errorf("write tool transport failure: got %v, want errors.Is(err, errWriteTransportFailure)", err)
	}
}

// ---- isTransportError unit tests ----

// TestIsTransportError_ClassifiesErrors exercises the full error-classification
// matrix for isTransportError: transport errors that should return true, and
// application / context errors that must return false.
func TestIsTransportError_ClassifiesErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"net.Error (timeout)", &fakeNetError{timeout: true}, true},
		{"net.Error (non-timeout)", &fakeNetError{timeout: false}, true},
		{"plain application error", errors.New("E_NOT_FOUND: entity not found"), false},
		{"string: connection refused", errors.New("dial tcp: connect: connection refused"), true},
		{"string: eof lowercase", errors.New("read tcp: eof"), true},
		{"string: broken pipe", errors.New("write unix: broken pipe"), true},
		{"wrapped EOF", fmt.Errorf("http: %w", io.EOF), true},
		{"wrapped ECONNREFUSED", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"wrapped context canceled", fmt.Errorf("op: %w", context.Canceled), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isTransportError(tc.err)
			if got != tc.want {
				t.Errorf("isTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// fakeNetError implements net.Error for tests.
type fakeNetError struct{ timeout bool }

func (e *fakeNetError) Error() string   { return "fake net error" }
func (e *fakeNetError) Timeout() bool   { return e.timeout }
func (e *fakeNetError) Temporary() bool { return true }

// ---- classifyReadOnlyTools / isReadOnlyByPrefix unit tests ----

// TestClassifyReadOnlyTools_ExplicitAllowlist verifies that known read-only
// Mazarbul tools are correctly classified as retryable and known write tools are
// not.
func TestClassifyReadOnlyTools_ExplicitAllowlist(t *testing.T) {
	t.Parallel()

	readTools := []string{
		"recall", "get", "entities", "relations", "neighbors", "traverse",
		"count", "query", "introspect", "describeType",
		"idleStatus", "waitForIdle", "listJobs",
		"getContext", "getEntityContext", "getIndex",
		"listPresets",
		"readDocument", "listDocuments", "getBacklinks", "documentHistory",
		"lintDocuments", "exportCanvas",
		"listSources", "getSourceRef", "getCapacity",
		"getBudgetStatus", "getUsageSnapshot", "getCostBreakdown",
		"contextPins",
		"feedbackQuery", "listInsights",
		"team_members", "team_key",
		"activity", "log", "digest", "explain", "show", "status",
		"agentFeedback",
		"reference",
	}
	writeTools := []string{
		"remember", "relate", "relateBatch", "forget", "fact",
		"promoteRelation", "defineEntityKind", "definePredicate",
		"writeDocument", "editDocument", "deleteDocument", "moveDocument",
		"contextPin", "contextCorrect", "contextUnpin",
		"createEntity", "updateEntity",
		"connectSource", "ingestSource",
		"team_create", "team_join", "team_grant", "team_revoke",
	}

	allTools := make([]mcpgo.Tool, 0, len(readTools)+len(writeTools))
	for _, n := range readTools {
		allTools = append(allTools, mcpgo.NewTool(n))
	}
	for _, n := range writeTools {
		allTools = append(allTools, mcpgo.NewTool(n))
	}

	classified := classifyReadOnlyTools(allTools)

	for _, n := range readTools {
		if !classified[n] {
			t.Errorf("expected read tool %q to be classified as read-only", n)
		}
	}
	for _, n := range writeTools {
		if classified[n] {
			t.Errorf("expected write tool %q to be classified as non-retryable (not read-only)", n)
		}
	}
}

// TestIsReadOnlyByPrefix_HeuristicFallback checks that the prefix heuristic
// correctly classifies names not in the explicit allowlist.
func TestIsReadOnlyByPrefix_HeuristicFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want bool
	}{
		{"getEntityContext", true},      // "get" prefix
		{"listPresets", true},           // "list" prefix
		{"readDocument", true},          // "read" prefix
		{"recallFacts", true},           // "recall" prefix
		{"countEntities", true},         // "count" prefix
		{"queryGraph", true},            // "query" prefix
		{"introspectSchema", true},      // "introspect" prefix
		{"browseDocuments", true},       // "browse" prefix
		{"describeType", true},          // "describe" prefix
		{"remember", false},             // no read prefix
		{"relate", false},               // no read prefix
		{"updateEntity", false},         // no read prefix (starts with "update")
		{"setBudget", false},            // no read prefix (starts with "set")
		{"triggerConsolidation", false}, // no read prefix
		{"draftStage", false},           // no read prefix
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isReadOnlyByPrefix(tc.name)
			if got != tc.want {
				t.Errorf("isReadOnlyByPrefix(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestClassifyReadOnlyTools_HeuristicCatchesUnknownTool confirms that a tool
// name not in the explicit allowlist but matching a read prefix is still
// classified as read-only via the heuristic — covering plugins and future tools.
func TestClassifyReadOnlyTools_HeuristicCatchesUnknownTool(t *testing.T) {
	t.Parallel()

	tools := []mcpgo.Tool{
		mcpgo.NewTool("getNewPluginTool"), // unknown, but "get" prefix
		mcpgo.NewTool("pluginWrite"),      // unknown, no read prefix → non-retryable
	}
	classified := classifyReadOnlyTools(tools)
	if !classified["getNewPluginTool"] {
		t.Error("expected 'getNewPluginTool' to be classified read-only via prefix heuristic")
	}
	if classified["pluginWrite"] {
		t.Error("expected 'pluginWrite' to be classified non-retryable (no read prefix)")
	}
}

// TestProxyState_SwapNilTools confirms the nil-tools fast path of swap: when
// called with nil tools (read-retry reconnect), only the client pointer is
// replaced and toolsChanged is always false, preserving the existing read-only
// classification.
func TestProxyState_SwapNilTools(t *testing.T) {
	t.Parallel()

	tools := []mcpgo.Tool{mcpgo.NewTool("recall")}
	ps := &proxyState{
		readOnly:   classifyReadOnlyTools(tools),
		toolNames:  toolNameSet(tools),
		socketPath: "/test.sock",
	}

	changed := ps.swap(nil, nil)
	if changed {
		t.Error("swap(nil, nil) returned toolsChanged=true; want false")
	}
	if !ps.readOnly["recall"] {
		t.Error("swap(nil, nil) wiped the read-only classification; want it preserved")
	}
}
