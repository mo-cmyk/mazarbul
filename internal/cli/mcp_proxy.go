package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/version"
)

// reconnectBackoffCap is the maximum interval between reconnect attempts.
// A daemon that has died and been respawned by launchd typically comes back
// within a few seconds; anything beyond 30s is a hard failure scenario and
// we want watchDaemonLiveness to become the backstop instead of hammering.
const reconnectBackoffCap = 30 * time.Second

// proxyDeadThreshold is the number of consecutive liveness-poll failures that
// classify the daemon as truly dead. At 10s per poll, 30 failures = 5 min.
// This is much longer than the original 3 × 10s = 30s so the reconnect loop
// has time to recover from a binary-upgrade restart before the proxy exits.
const proxyDeadThreshold = 30

// errWriteTransportFailure is returned by the proxy handler when a write tool
// call fails with a transport error. It is intentionally not retried because
// writes are not idempotent — a blind retry risks double-writes. The MCP host
// can surface this to the agent and let it decide to resubmit.
var errWriteTransportFailure = errors.New("mcp proxy: daemon unreachable; write not retried (not idempotent — resubmit when the daemon is back)")

// proxyState holds the live daemon client and the read-only tool classification
// derived from the most recent tool snapshot. All fields are protected by mu.
// The client pointer is swapped atomically on reconnect; handlers always call
// get() to obtain the current client rather than closing over the boot-time value.
type proxyState struct {
	mu         sync.RWMutex
	client     *mcpclient.Client
	readOnly   map[string]bool // true → safe to retry on transport failure
	toolNames  map[string]bool // current snapshot set; compared on reconnect
	socketPath string
}

// newProxyState constructs a proxyState from a freshly-snapshotted connection.
func newProxyState(client *mcpclient.Client, tools []mcpgo.Tool, socketPath string) *proxyState {
	ps := &proxyState{
		client:     client,
		readOnly:   classifyReadOnlyTools(tools),
		toolNames:  toolNameSet(tools),
		socketPath: socketPath,
	}
	return ps
}

// get returns the current daemon client under a read lock.
func (ps *proxyState) get() *mcpclient.Client {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.client
}

// isReadOnly reports whether toolName is in the read-only set.
func (ps *proxyState) isReadOnly(toolName string) bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.readOnly[toolName]
}

// swap atomically replaces the client and, when tools is non-nil, the
// read-only classification and tool-name set. It returns true if the tool set
// changed compared to the previous snapshot (so the caller knows to emit
// notifications/tools/list_changed). Passing nil tools (fast-path from a
// read-retry reconnect) only swaps the client, preserving the existing
// classification — it always returns false. The old client is closed after the
// swap so in-flight calls on the old client drain naturally.
func (ps *proxyState) swap(newClient *mcpclient.Client, tools []mcpgo.Tool) (toolsChanged bool) {
	if tools == nil {
		ps.mu.Lock()
		old := ps.client
		ps.client = newClient
		ps.mu.Unlock()
		if old != nil {
			closeQuiet(old)
		}
		return false
	}

	newNames := toolNameSet(tools)
	newRO := classifyReadOnlyTools(tools)

	ps.mu.Lock()
	old := ps.client
	ps.client = newClient
	ps.readOnly = newRO

	// Compare by name-set only; schema changes within the same name set are
	// not detectable here and handled by the daemon's own notifications.
	if len(newNames) != len(ps.toolNames) {
		toolsChanged = true
	} else {
		for n := range newNames {
			if !ps.toolNames[n] {
				toolsChanged = true
				break
			}
		}
	}
	ps.toolNames = newNames
	ps.mu.Unlock()

	if old != nil {
		closeQuiet(old)
	}
	return toolsChanged
}

// toolNameSet converts a tool slice to a set of names.
func toolNameSet(tools []mcpgo.Tool) map[string]bool {
	m := make(map[string]bool, len(tools))
	for i := range tools {
		m[tools[i].Name] = true
	}
	return m
}

// runMCPServeProxy bridges a Claude-Desktop-style stdio MCP client
// (the parent process spawned this `maz mcp serve`) to the existing
// background daemon's HTTP MCP endpoint. The use case is the bug
// surfaced when a user runs `maz daemon start` AND Claude Desktop
// launches `maz mcp serve` simultaneously: the daemon holds the
// LadybugDB lock, so a second OpenWiring would fail with "ladybug
// status 1". Instead of opening the DB ourselves, we discover the
// daemon's tools list once at boot and serve a thin stdio MCP server
// whose tool handlers forward each CallTool to the daemon.
//
// Non-tool MCP methods (initialize, ping, etc.) are handled by
// mcp-go's MCPServer itself with the server-info we provide; we do
// not try to forward them. This is sufficient for tools-only clients
// like Claude Desktop, the mcp-inspector, and `maz` CLI workflows;
// resources/prompts/sampling are not yet exposed by Mazarbul anyway.
//
// The proxy keeps its stdio session alive across daemon restarts by
// re-dialing the socket with exponential backoff (C.1). On reconnect
// it re-snapshots the tool list and notifies the host if tools changed
// (C.2). Read-only tool calls failing with a transport error are
// retried within the call deadline (C.3). Write tool calls failing
// with a transport error return a clean, retryable error without
// auto-retrying — protecting against double-writes (C.4). The
// watchDaemonLiveness backstop exits the proxy after proxyDeadThreshold
// consecutive failures so the host can respawn (C.5).
func runMCPServeProxy(ctx context.Context, opts mcpServeOpts, socketPath string, logger *zap.Logger) error {
	if !opts.stdio {
		// Proxy mode currently only supports stdio: the user almost
		// certainly meant to talk to the daemon directly when asking
		// for HTTP/Unix transports while another daemon was running.
		// Surfacing this rather than silently doing nothing prevents
		// two daemons from racing on the same port.
		return fmt.Errorf("mcp serve: refusing to start HTTP/Unix transports while a daemon is already running; stop it first or connect to its socket %s directly", socketPath)
	}

	client, tools, instructions, err := connectAndSnapshotTools(ctx, socketPath)
	if err != nil {
		return fmt.Errorf("mcp serve: daemon proxy: %w", err)
	}

	ps := newProxyState(client, tools, socketPath)
	// Close the live daemon client on proxy exit. proxyState.swap() only closes
	// the previous client across reconnects, so without this the final client
	// would leak until the OS reclaims it at process exit — restore the original
	// defer client.Close() guarantee.
	defer func() {
		if c := ps.get(); c != nil {
			closeQuiet(c)
		}
	}()

	// Forward the daemon's per-session instructions (Channel A, #214): the
	// daemon composes base manual + getContext bootstrap pointer + active-preset
	// section in its initialize result, but mcp-go does not propagate it through
	// a proxy. Without this the Claude-Desktop-while-daemon-running path — the
	// very reason proxy mode exists — would surface an empty system prompt: no
	// operating manual, no preset steering. The snapshot is taken once at boot,
	// which matches this proxy process's 1:1 lifetime with its stdio client.
	//
	// Declare listChanged capability so SetTools (on reconnect with a changed tool
	// set) emits notifications/tools/list_changed to the downstream stdio client.
	srv := mcpserver.NewMCPServer(
		opts.name, version.Version,
		mcpserver.WithInstructions(instructions),
		mcpserver.WithToolCapabilities(true),
	)
	for i := range tools {
		t := tools[i]
		srv.AddTool(t, makeProxyToolHandler(ps, t.Name, logger))
	}

	stdioSrv := mcpserver.NewStdioServer(srv)
	// Surface the snapshot caveat where an operator can actually see it: the
	// instructions are read once at proxy boot, so a preset installed/removed while
	// this proxy runs only reaches new conversations after the proxy is restarted
	// (the non-proxy path re-reads preset state per process launch). instructionsBytes
	// anchors the byte count so a "why is my system prompt empty/stale" debug starts here.
	logger.Info("mcp serve: proxying to running daemon; reconnect loop active (instructions snapshotted once at boot; restart to pick up preset changes)",
		zap.String("socket", socketPath),
		zap.Int("tools", len(tools)),
		zap.Int("instructionsBytes", len(instructions)),
	)

	procCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Background reconnect loop (C.1/C.2): when the daemon drops, keep the
	// proxy's stdio session open and re-dial with bounded exponential backoff.
	// On success, swap the client and, if the tool set changed, update the
	// server's tool map so the host sees notifications/tools/list_changed.
	go runReconnectLoop(procCtx, ps, srv, logger)

	// Backstop (C.5): a daemon that NEVER comes back must not leave a zombie proxy.
	// proxyDeadThreshold × 10s = 5 min of sustained failure before we exit, giving
	// the reconnect loop ample time to recover from binary-upgrade restarts while
	// still eventually cleaning up after a truly-dead daemon.
	go watchDaemonLiveness(procCtx, socketPath, 10*time.Second, proxyDeadThreshold, func() {
		logger.Warn("mcp serve: daemon unreachable for extended period; exiting so MCP host respawns",
			zap.String("socket", socketPath),
			zap.Int("failThreshold", proxyDeadThreshold),
		)
		cancel()
	})

	if err := stdioSrv.Listen(procCtx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// runReconnectLoop is the C.1/C.2 background goroutine. It monitors whether
// the proxy's current daemon client is functional by watching for the daemon
// socket to disappear (via daemonReachable). When the socket drops, it waits
// for the daemon to come back and re-dials with exponential backoff. On a
// successful reconnect it atomically swaps the client in proxyState. If the
// tool set changed (e.g. a binary upgrade added or removed tools) it calls
// srv.SetTools so mcp-go emits notifications/tools/list_changed to all
// connected stdio clients — the host re-fetches tools/list and the session
// continues uninterrupted.
//
// The loop exits when ctx is cancelled (proxy shutting down). It does not
// exit on reconnect failures alone; watchDaemonLiveness is the backstop for a
// truly-dead daemon.
func runReconnectLoop(ctx context.Context, ps *proxyState, srv *mcpserver.MCPServer, logger *zap.Logger) {
	// Poll every 2s to detect the outage quickly without hammering the socket.
	const detectInterval = 2 * time.Second
	runReconnectLoopWithDetect(ctx, ps, srv, logger, detectInterval)
}

// runReconnectLoopWithDetect is the clock-injectable core of runReconnectLoop,
// parameterised on the outage-detection interval so tests can drive it faster
// without sleeps. Production callers use runReconnectLoop (2s detect interval).
func runReconnectLoopWithDetect(ctx context.Context, ps *proxyState, srv *mcpserver.MCPServer, logger *zap.Logger, detectInterval time.Duration) {
	detectTick := time.NewTicker(detectInterval)
	defer detectTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-detectTick.C:
			if daemonReachable(ctx, ps.socketPath) {
				continue
			}
		}
		// Daemon dropped. Run the backoff dial loop until reconnected or ctx cancelled.
		logger.Warn("mcp serve: daemon socket lost; starting reconnect loop",
			zap.String("socket", ps.socketPath),
		)
		if !dialUntilReconnected(ctx, ps, srv, logger) {
			return // ctx cancelled
		}
	}
}

// dialUntilReconnected retries connectAndSnapshotTools with exponential backoff
// until a connection is established or ctx is cancelled. On success it atomically
// swaps the new client into ps, and if the tool set changed it calls srv.SetTools
// so mcp-go emits notifications/tools/list_changed. It returns true on reconnect
// and false when ctx is cancelled (proxy shutting down).
func dialUntilReconnected(ctx context.Context, ps *proxyState, srv *mcpserver.MCPServer, logger *zap.Logger) bool {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}

		newClient, tools, _, dialErr := connectAndSnapshotTools(ctx, ps.socketPath)
		if dialErr != nil {
			logger.Info("mcp serve: reconnect attempt failed; retrying",
				zap.String("socket", ps.socketPath),
				zap.Duration("nextBackoff", backoff),
				zap.Error(dialErr),
			)
			backoff = min(backoff*2, reconnectBackoffCap)
			continue
		}

		// Swap the new client and read-only classification atomically.
		changed := ps.swap(newClient, tools)
		notifyReconnect(ps, srv, tools, changed, logger)
		return true
	}
}

// notifyReconnect updates the stdio server's tool map when the tool set changed
// after a reconnect. If changed is true it calls srv.SetTools (which emits
// notifications/tools/list_changed because we declared WithToolCapabilities(true))
// so the host re-fetches tools/list without dropping the session. When the tool
// set is unchanged it only logs the reconnect.
func notifyReconnect(ps *proxyState, srv *mcpserver.MCPServer, tools []mcpgo.Tool, changed bool, logger *zap.Logger) {
	if !changed {
		logger.Info("mcp serve: daemon reconnected; tool set unchanged",
			zap.String("socket", ps.socketPath),
			zap.Int("tools", len(tools)),
		)
		return
	}
	serverTools := make([]mcpserver.ServerTool, len(tools))
	for i := range tools {
		t := tools[i]
		serverTools[i] = mcpserver.ServerTool{
			Tool:    t,
			Handler: makeProxyToolHandler(ps, t.Name, logger),
		}
	}
	// SetTools replaces the map atomically and — because we declared
	// WithToolCapabilities(listChanged:true) — emits
	// notifications/tools/list_changed to all connected clients so the
	// host re-fetches the tool list without dropping the session.
	srv.SetTools(serverTools...)
	logger.Info("mcp serve: tool set changed on reconnect; host notified via tools/list_changed",
		zap.String("socket", ps.socketPath),
		zap.Int("tools", len(tools)),
	)
}

// connectAndSnapshotTools dials the daemon over its Unix socket, initializes
// a session, and returns the live client + the daemon's tools list + the
// daemon's composed initialize instructions (Channel A, #214). The session is
// left open: the proxy reuses it for every CallTool forwarded from stdio. The
// URL host is a placeholder the socket dialer ignores; the socket's 0600
// permissions are the credential.
func connectAndSnapshotTools(ctx context.Context, socketPath string) (*mcpclient.Client, []mcpgo.Tool, string, error) {
	c, err := mcpclient.NewStreamableHttpClient(
		"http://unix/mcp",
		transport.WithHTTPBasicClient(mcp.UnixHTTPClient(socketPath, 0)),
	)
	if err != nil {
		return nil, nil, "", fmt.Errorf("new streamable unix client: %w", err)
	}

	startCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Start(startCtx); err != nil {
		closeQuiet(c)
		return nil, nil, "", fmt.Errorf("start client: %w", err)
	}

	initCtx, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	initResult, err := c.Initialize(initCtx, mcpgo.InitializeRequest{
		Params: mcpgo.InitializeParams{
			ProtocolVersion: mcpgo.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcpgo.Implementation{Name: "maz-mcp-proxy", Version: version.Version},
		},
	})
	if err != nil {
		closeQuiet(c)
		return nil, nil, "", fmt.Errorf("initialize: %w", err)
	}
	var instructions string
	if initResult != nil {
		instructions = initResult.Instructions
	}

	listCtx, cancel3 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel3()
	listResp, err := c.ListTools(listCtx, mcpgo.ListToolsRequest{})
	if err != nil {
		closeQuiet(c)
		return nil, nil, "", fmt.Errorf("list tools: %w", err)
	}
	return c, listResp.Tools, instructions, nil
}

// closeQuiet closes a client and discards the error. Used on the
// error paths where we already have a more interesting error to
// surface; ignoring close failures keeps the caller's error
// signature clean. Errcheck flags `_ = c.Close()` as a smell, so
// the helper centralises the suppression in one named function.
func closeQuiet(c *mcpclient.Client) { _ = c.Close() } //nolint:errcheck // close on error path

// daemonReachable reports whether something is accepting connections on the
// daemon's Unix socket. It is a cheap liveness probe independent of any MCP
// session or the proxy's snapshotted connection: a successful dial means the
// daemon process is up and listening; a refused or missing socket means it is
// down. We dial-and-close rather than reuse the proxy client so a wedged client
// connection cannot mask a dead daemon.
func daemonReachable(ctx context.Context, socketPath string) bool {
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "unix", socketPath)
	if err != nil {
		return false
	}
	_ = conn.Close() //nolint:errcheck // liveness probe; close failure is irrelevant
	return true
}

// watchDaemonLiveness polls the daemon socket every interval and, once the
// daemon has been unreachable for failThreshold consecutive polls, invokes
// onDead once and returns. It also returns (without calling onDead) when ctx is
// cancelled — the proxy's normal shutdown path. A single reachable poll resets
// the streak, so a brief restart blip (launchd relaunch is a few seconds) does
// not trip it; the threshold is sized to outlast a restart yet still surface a
// daemon that is truly gone.
//
// This closes the orphaned-proxy gap: a `maz mcp serve` otherwise blocks on its
// stdio client forever even after its daemon backend dies, so wedged proxies
// pile up and the MCP host never respawns. Wiring onDead to cancel the proxy
// context makes the process exit on a sustained outage, prompting the host to
// spawn a fresh proxy against a live daemon.
func watchDaemonLiveness(ctx context.Context, socketPath string, interval time.Duration, failThreshold int, onDead func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	watchLiveness(ctx, ticker.C, func() bool { return daemonReachable(ctx, socketPath) }, failThreshold, onDead)
}

// watchLiveness is the clock- and probe-independent core of
// watchDaemonLiveness. On each tick it probes reachability, resetting the
// consecutive-failure streak on any success and invoking onDead exactly once the
// streak reaches failThreshold (then returning). It returns without firing
// onDead when ctx is cancelled, and a probe that fails only because ctx already
// ended is treated as shutdown, not an outage — so a clean client disconnect
// never trips a false "daemon unreachable". Splitting the clock (ticks) and the
// probe (reachable) out as parameters lets tests drive the streak/reset logic
// deterministically, without sleeps or real sockets.
func watchLiveness(ctx context.Context, ticks <-chan time.Time, reachable func() bool, failThreshold int, onDead func()) {
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if reachable() {
				fails = 0
				continue
			}
			if ctx.Err() != nil {
				return
			}
			fails++
			if fails >= failThreshold {
				onDead()
				return
			}
		}
	}
}

// makeProxyToolHandler returns a mcp-go tool handler that forwards an incoming
// CallTool to the current daemon client (retrieved from ps on each call, so
// reconnects are transparent) and returns the daemon's CallToolResult verbatim.
//
// Transport-error handling (C.3/C.4):
//   - READ-ONLY tools: if the call fails with a transport error (connection
//     refused/reset/EOF — not an application-level error the daemon returned),
//     the handler re-dials the daemon and retries once within the call's
//     remaining context deadline. Reads are safe to retry because they do not
//     mutate state; a duplicate read is always correct.
//   - WRITE tools: transport errors are NOT retried. A blind retry on a write
//     risks double-writes (e.g. `remember` appending the same observation twice).
//     The handler returns errWriteTransportFailure so the MCP host surfaces a
//     clear, retryable-typed error to the agent, which can then resubmit once
//     the daemon is back. This is the key safety property: retry is typed
//     (reads only), never blind.
//
// Arguments are passed as json.RawMessage-shaped any-maps to avoid lossy
// re-marshalling of numbers and nested structures the daemon's JSON Schema
// validators care about.
func makeProxyToolHandler(ps *proxyState, toolName string, logger *zap.Logger) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		out := mcpgo.CallToolRequest{
			Params: mcpgo.CallToolParams{
				Name:      toolName,
				Arguments: req.Params.Arguments,
				Meta:      req.Params.Meta,
			},
		}
		callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()

		result, err := ps.get().CallTool(callCtx, out)
		if err == nil {
			return result, nil
		}

		// Check whether this is a transport error (not an application error). An
		// application error is returned as (result, nil) by the mcp-go client —
		// result.IsError is true but err is nil. An err here means the HTTP round
		// trip itself failed (connection refused, reset, EOF, context timeout).
		if !isTransportError(err) {
			return nil, err
		}

		// Transport failure. Classify and handle by mutability.
		if !ps.isReadOnly(toolName) {
			// C.4: non-idempotent write — fail clean, let the agent decide to retry.
			logger.Warn("mcp serve proxy: write tool transport failure; not retried",
				zap.String("tool", toolName),
				zap.Error(err),
			)
			return nil, errWriteTransportFailure
		}

		// C.3: read-only — re-dial and retry once within the remaining deadline.
		logger.Info("mcp serve proxy: read tool transport failure; retrying after re-dial",
			zap.String("tool", toolName),
			zap.Error(err),
		)
		newClient, _, _, dialErr := connectAndSnapshotTools(callCtx, ps.socketPath)
		if dialErr != nil {
			return nil, fmt.Errorf("mcp proxy: reconnect for retry failed: %w (original: %w)", dialErr, err)
		}
		// Optimistically swap the new client into the shared state so other handlers
		// benefit too. Tool set notification is handled by the background reconnect
		// loop; here we just need a working client.
		_ = ps.swap(newClient, nil)

		retryResult, retryErr := newClient.CallTool(callCtx, out)
		if retryErr != nil {
			return nil, fmt.Errorf("mcp proxy: retry after reconnect failed: %w", retryErr)
		}
		return retryResult, nil
	}
}

// isTransportError reports whether err is a network-layer failure — connection
// refused, reset, or an unexpected EOF — as opposed to a well-formed application
// error returned by the daemon. Application errors arrive as a successful HTTP
// response with an MCP error payload: the mcp-go client returns them as
// (result, nil) with result.IsError=true, so they never reach this function.
//
// We recognise transport failures by unwrapping to:
//   - io.EOF / io.ErrUnexpectedEOF — server closed the connection mid-stream
//   - syscall.ECONNREFUSED — socket exists but nobody is accepting
//   - syscall.ECONNRESET — connection torn down by the peer
//   - net.Error with Temporary()==true — transient network hiccup
//   - *url.Error wrapping any of the above — how net/http surfaces them
//
// Context cancellation (context.Canceled / context.DeadlineExceeded) is
// explicitly excluded: those are intentional deadline management, not transport
// failures, and must not trigger a retry.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	// Explicit context errors are not transport failures.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Unexpected EOF: server closed the connection before sending a full response.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Syscall-level errors.
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	// net.Error (covers temporary/timeout variants from net/http).
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// *url.Error wraps the above when returned from net/http.Do.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return isTransportError(urlErr.Err)
	}
	// Final string-based heuristic for errors that slip through unwrapping
	// (e.g. opaque strings from mcp-go's internal SSE/streaming layer).
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof")
}

// classifyReadOnlyTools returns a name→true map for every tool that is safe to
// retry transparently on a transport failure. The classification uses two tiers:
//
//  1. Explicit allowlist: well-known read-only Mazarbul tool names, enumerated
//     here from the MCP tool registry. Adding a new read-only tool requires a
//     deliberate edit here — this is intentional; the list must not grow by
//     accident.
//
//  2. Heuristic fallback: tools whose names start with a read-only verb prefix
//     (get, list, read, recall, count, query, introspect, browse, describe).
//     The fallback catches tools added by plugins or future binary versions
//     that were not in the snapshot at proxy boot.
//
// When in doubt (name not in either tier), the tool is classified as non-retryable.
// This is the conservative safe default: a false negative wastes a call; a false
// positive could double-write.
func classifyReadOnlyTools(tools []mcpgo.Tool) map[string]bool {
	// Explicit allowlist derived from internal/mcp/tools/ — these are the tools
	// whose Invoke methods only read from the store and never call Append.
	explicit := map[string]bool{
		// Retrieval
		"recall":       true,
		"get":          true,
		"entities":     true,
		"relations":    true,
		"neighbors":    true,
		"traverse":     true,
		"count":        true,
		"query":        true,
		"introspect":   true,
		"describeType": true,
		// Status / observability
		"idleStatus":  true,
		"waitForIdle": true,
		"listJobs":    true,
		// Context / agent helpers
		"getContext":       true,
		"getEntityContext": true,
		"getIndex":         true,
		// Presets (read-only list; install/uninstall are writes via CLI not MCP)
		"listPresets": true,
		// Document reads
		"readDocument":    true,
		"listDocuments":   true,
		"getBacklinks":    true,
		"documentHistory": true,
		"lintDocuments":   true,
		"exportCanvas":    true,
		// Source / reference reads
		"listSources":      true,
		"getSourceRef":     true,
		"getCapacity":      true,
		"getBudgetStatus":  true,
		"getUsageSnapshot": true,
		"getCostBreakdown": true,
		// Context pins read
		"contextPins": true,
		// Feedback / insights reads
		"feedbackQuery": true,
		"listInsights":  true,
		// Team reads
		"team_members": true,
		"team_key":     true,
		// Git / activity reads
		"activity":      true,
		"log":           true,
		"digest":        true,
		"explain":       true,
		"show":          true,
		"status":        true,
		"agentFeedback": true, // read-path for gotcha queries
		// Reference read
		"reference": true,
	}

	// Build the classification map for the snapshotted tools, applying both
	// tiers. A tool not present in the snapshot cannot be classified; if it
	// appears later (e.g. reconnect adds a new tool) the reconnect loop will
	// call classifyReadOnlyTools again with the new snapshot.
	result := make(map[string]bool, len(tools))
	for i := range tools {
		name := tools[i].Name
		if explicit[name] {
			result[name] = true
			continue
		}
		if isReadOnlyByPrefix(name) {
			result[name] = true
		}
		// Absent entry means non-retryable (the zero value false).
	}
	return result
}

// isReadOnlyByPrefix is the heuristic tier of classifyReadOnlyTools. It
// returns true when the tool name starts with a verb that conventionally
// signals a read-only operation in the Mazarbul MCP surface. This is a
// conservative set — verbs that are ambiguous (e.g. "set", "update",
// "trigger") are deliberately excluded.
func isReadOnlyByPrefix(name string) bool {
	readPrefixes := []string{
		"get", "list", "read", "recall", "count",
		"query", "introspect", "browse", "describe",
	}
	lower := strings.ToLower(name)
	for _, p := range readPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}
