// Package mcp hosts the Mazarbul MCP server: JSON-RPC 2.0 over stdio,
// streamable HTTP, and Unix socket, exposing stable tools (remember,
// recall, forget, relate, traverse, review) and any plugin-provided
// extensions. The server wraps mark3labs/mcp-go and bridges to the
// kernel + plugin Host.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"
)

// httpReadHeaderTimeout caps the time a client may take to send request
// headers. Mitigates Slowloris-style attacks on the HTTP and Unix-socket
// listeners.
const httpReadHeaderTimeout = 10 * time.Second

// serverInstructionsBase is the STATIC base operating manual returned in the MCP
// `initialize` response. Clients (Claude Code, Claude Desktop, Cursor, …)
// typically inject this verbatim into the agent's system prompt. The wording is
// deliberate: agents tend to fall back on their own context window for
// short-term recall and skip the MCP entirely when given vague guidance — which
// makes Mazarbul pure overhead. The text below explicitly instructs the agent to
// ROUTE memory reads/writes through this server, not through native conversation
// memory, and lists the concrete trigger conditions so the model has a decision
// rule it can actually execute.
//
// This is only the BASE: ComposeServerInstructions wraps it with a bootstrap
// pointer and (per session, via the OnAfterInitialize hook) the active-preset
// "Operating instructions" section (#214). The load-bearing routing guidance
// stays in the first ~512 chars so a client that truncates still gets the
// decision rule. Keep it short enough that Claude doesn't paraphrase it away,
// but concrete enough that the model knows when to fire each tool.
const serverInstructionsBase = `Mazarbul is the user's persistent memory across all conversations and sessions. It is the source of truth for what the user has previously told you about themselves, their projects, preferences, decisions, and history. Your own conversation context is volatile; Mazarbul is durable.

ALWAYS use this MCP for memory operations. DO NOT rely on your native conversation memory for facts about the user. Specifically:

• BEFORE answering any question that depends on prior context about the user — their name, role, project, preferences, history, ongoing work, people they know, places they've been, decisions they've made — call ` + "`recall`" + ` first. If you are uncertain whether a fact is in memory, call ` + "`recall`" + ` anyway; a near-zero-cost miss is far better than a confident hallucination.

• WHEN the user states a fact about themselves, their work, the world, or their decisions — names, identities, roles, preferences, plans, deadlines, opinions, corrections to prior facts — call ` + "`remember`" + ` immediately. Do not wait to be asked. Capture it verbatim; the resolver handles deduplication and supersession.

• AFTER a recall, traverse the graph yourself with the LLM-free navigation verbs rather than re-querying: each hit carries entity anchors you can feed into ` + "`get`" + ` (fetch any entities/episodes/relations record by name), ` + "`neighbors`" + ` (expand an entity up to 6 hops), and ` + "`relations`" + ` (list an entity's typed edges). Use ` + "`entities`" + ` to discover an anchor by kind/name and ` + "`introspect`" + ` to learn the predicate + entity-kind vocabulary. You do the reasoning; these calls are cheap and add no model cost.

• WHEN you already hold a fact as a (subject, predicate, object) triple, write it directly with ` + "`relate`" + ` instead of phrasing prose for ` + "`remember`" + `.

• CHOOSE the scope of every write by who should see it: omit scope (personal) for records private to the user; scope=project for records tied to the current repo/codebase (shared with repo collaborators); scope=team for the shared team brain (requires admitted membership, else E_PERMISSION). Reads default to personal+project — team is an explicit opt-in.

• WHEN you retrieve a durable external item (Notion, Linear, Granola, Drive, GitHub) from a live connector, call ` + "`recall`" + ` first; if fresh, reuse the cached summary; if stale or new, fetch live and call ` + "`captureExternal`" + ` to store a pointer and your summary without copying the body. Never capture ephemeral connectors (gmail/gcal/slack).

• ` + "`forget`" + ` is for when the user explicitly asks you to forget a fact. Confirm scope before tombstoning.

• WHEN a tool, result, or instruction here is confusing, wrong, missing, or slow — or you learn a durable lesson about using this memory — call ` + "`agentFeedback`" + ` to record it. Flag durable friction, not one-off disagreements; recurring reports auto-promote into the shared gotchas guidance future sessions see.

Native agent memory and Mazarbul are NOT substitutes — Mazarbul persists across sessions, models, and even agent clients. Anything you only "remember" in conversation context will be lost. When in doubt, call the tool.`

// bootstrapPointer tells the agent to fetch its cold-start brief from getContext.
// It rides right after the base manual so the getContext routing lands inside the
// early guidance window, ahead of the (potentially large, untrusted) preset
// section that ComposeServerInstructions appends last.
const bootstrapPointer = "On session start, call `getContext` for your cold-start brief — pinned notes, recently-updated work, key entities, and any active operating presets — instead of asking the user to re-explain context."

// operatingInstructionsHeader labels the appended active-preset section so the
// agent (and a human reading the system prompt) can tell where the trusted base
// manual ends and the installed preset steering begins.
const operatingInstructionsHeader = "## Operating instructions"

// ComposeServerInstructions assembles the MCP `initialize` instructions:
//
//	base operating manual  →  bootstrap pointer  →  active-preset section (LAST)
//
// presetSection is the already-bounded, already-fenced block produced by
// contextbrief.Assembler.OperatingInstructions; an empty string omits the
// trailing section entirely so composition degrades cleanly to base + bootstrap
// (no preset active, or #210 not wired). The preset section is placed LAST — not
// in the issue's literal base+preset+bootstrap order — because preset bodies can
// be multiple KiB of UNTRUSTED installed content: keeping them last leaves the
// load-bearing routing guidance and the getContext pointer in the early window a
// truncating client preserves.
//
// This is the single composition point: the static WithInstructions feed uses
// ComposeServerInstructions(""), and the per-session OnAfterInitialize hook calls
// it (via the daemon composer) with the live preset section.
func ComposeServerInstructions(presetSection, gotchasSection string) string {
	var b strings.Builder
	b.WriteString(serverInstructionsBase)
	b.WriteString("\n\n")
	b.WriteString(bootstrapPointer)
	if section := strings.TrimSpace(presetSection); section != "" {
		b.WriteString("\n\n")
		b.WriteString(operatingInstructionsHeader)
		b.WriteByte('\n')
		b.WriteString(section)
	}
	// Distilled, recurring agent feedback (the dynamic lessons loop) goes LAST —
	// after the preset, in the lowest-trust, truncation-sacrificable window. It is
	// sanitized + capped and carries its own fenced "## Known gotchas & lessons"
	// header; it NEVER rewrites the trusted base manual above it.
	if g := strings.TrimSpace(gotchasSection); g != "" {
		b.WriteString("\n\n")
		b.WriteString(g)
	}
	return b.String()
}

// mcpServer is the concrete Server implementation. It wraps mcp-go's
// MCPServer and orchestrates the configured transports.
type mcpServer struct {
	inner      *mcpserver.MCPServer
	streamable *mcpserver.StreamableHTTPServer
	opts       Options

	toolsMu    sync.Mutex
	tools      map[string]struct{}
	validators map[string]*schemaValidator
	// unlisted names the tools to hide from tools/list (still dispatchable
	// via tools/call). Guarded by toolsMu; populated by Register when a tool
	// implements UnlistedTool and returns true. Read by the tool-list filter.
	unlisted map[string]struct{}

	stdin  io.Reader
	stdout io.Writer

	// startTime is captured the first time Start binds transports. It
	// is read by healthHandler under startTimeMu to publish the server's
	// boot timestamp on /healthz. A zero value means Start has not run
	// (or short-circuited on ErrServerShutdown); the HTTP listener does
	// not exist in that case so /healthz is unreachable anyway.
	startTimeMu sync.RWMutex
	startTime   time.Time

	// lnMu guards the listener, transport-server, and per-transport
	// cancel funcs below. Start writes them under Lock; Shutdown,
	// httpListenerAddr, and requestBaseContext read them under RLock.
	lnMu        sync.RWMutex
	httpLn      net.Listener
	httpSrv     *http.Server
	unixLn      net.Listener
	unixSrv     *http.Server
	stdioCancel context.CancelFunc

	// serveCtx is the parent of every HTTP/Unix request context. Cancelling
	// it during Shutdown propagates into in-flight tool handlers so they
	// can observe cancellation via ctx.Done() and return promptly. Read
	// and written under lnMu.
	serveCtx    context.Context //nolint:containedctx // server lifetime context, owned and cancelled by Shutdown
	serveCancel context.CancelFunc

	// lifecycleMu serializes Start and Shutdown's atomic state
	// transitions: the shutdownC pre-check, claiming startedDone, marking
	// the server shut down, and snapshotting startedDone for Shutdown to
	// wait on. It is held only across decisions, never across blocking
	// startTransports work, so Shutdown is not blocked by a slow Start.
	lifecycleMu sync.Mutex
	// startedDone is created by the first Start caller, closed once that
	// caller has finished startTransports (success or failure). Nil
	// before any Start. Shutdown waits on this channel to guarantee any
	// listeners bound by Start are visible to closeTransports.
	startedDone chan struct{}
	startErr    error
	shutdownC   chan struct{}
	runErr      chan error
	wg          sync.WaitGroup

	// stdioClosed is closed exactly once by the stdio goroutine when
	// stdio.Listen returns cleanly (nil or io.EOF — the client closed
	// stdin). Start selects on it and calls gracefulShutdown so the
	// process exits instead of hanging until SIGTERM. It is initialized
	// in New and never reassigned, so no lock is needed to read it. When
	// stdio is not enabled it is never closed and the select case in
	// Start never fires.
	stdioClosed chan struct{}
	stdioOnce   sync.Once
}

// New constructs a Server backed by mark3labs/mcp-go. It validates opts,
// initializes the underlying MCPServer with recovery + tool capabilities
// + invocation-logging hooks, and prepares the transport scaffolding.
// New does not bind any listener; Start does that.
//
// The returned Server is safe for concurrent use by Register, Start, and
// Shutdown.
func New(opts Options) (Server, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	s := &mcpServer{
		opts:        opts,
		tools:       make(map[string]struct{}),
		validators:  make(map[string]*schemaValidator),
		unlisted:    make(map[string]struct{}),
		stdin:       os.Stdin,
		stdout:      os.Stdout,
		shutdownC:   make(chan struct{}),
		runErr:      make(chan error, 3),
		stdioClosed: make(chan struct{}),
	}

	// WithToolFilter runs only on tools/list; tools/call dispatches straight
	// from the handler map, so an unlisted tool stays callable by name while
	// being hidden from agent discovery (see UnlistedTool).
	inner := mcpserver.NewMCPServer(
		opts.Name,
		opts.Version,
		mcpserver.WithRecovery(),
		mcpserver.WithToolCapabilities(true),
		// Resources: subscribe=false (server-push not yet implemented),
		// listChanged=false (static + template set is stable after boot).
		mcpserver.WithResourceCapabilities(false, false),
		mcpserver.WithHooks(newLoggingHooks(opts.Logger, opts.InstructionComposer)),
		// Static fallback = base + bootstrap (no preset). The per-session
		// OnAfterInitialize hook (when a composer is wired) overwrites this with
		// the live base + bootstrap + active-preset composition (#214).
		mcpserver.WithInstructions(ComposeServerInstructions("", "")),
		mcpserver.WithToolFilter(s.filterUnlisted),
	)
	s.inner = inner
	// NewStreamableHTTPServer uses StatelessGeneratingSessionIdManager by
	// default: it generates a session ID on initialize and echoes it back via
	// Mcp-Session-Id, but performs no server-side session state validation.
	// Each POST is therefore independent — concurrent POSTs on the same
	// Mcp-Session-Id each create (or reuse) an ephemeral streamableHttpSession
	// and process their tool call on separate goroutines. Both receive their own
	// HTTP response on their own connection; the server does not serialize
	// concurrent same-session requests.
	//
	// When a persistent GET (SSE) listener exists for the session, concurrent
	// POSTs share the registered session object for notification routing. If the
	// server has previously sent a notification to that session (e.g. via
	// tools/list_changed), upgradeToSSE is set on the session and subsequent
	// POST responses are framed as SSE (text/event-stream) rather than plain
	// JSON. MCP clients MUST declare Accept: application/json, text/event-stream
	// and parse both framings. A client that sends only Accept: application/json
	// and reads the raw body will appear to receive an "empty" reply when the
	// server switches to SSE framing — this is correct transport behavior, not a
	// server bug. The MCP Streamable HTTP spec (§Transport) requires clients to
	// handle both Content-Type values.
	s.streamable = mcpserver.NewStreamableHTTPServer(inner)
	return s, nil
}

// filterUnlisted drops UnlistedTool tools from a tools/list response. It runs
// per list request; dispatch (tools/call) bypasses it entirely.
func (s *mcpServer) filterUnlisted(_ context.Context, tools []mcpgo.Tool) []mcpgo.Tool {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	if len(s.unlisted) == 0 {
		return tools
	}
	kept := make([]mcpgo.Tool, 0, len(tools))
	for _, t := range tools {
		if _, hidden := s.unlisted[t.Name]; hidden {
			continue
		}
		kept = append(kept, t)
	}
	return kept
}

// SetStdio swaps the reader and writer that the stdio transport binds
// when Start runs. Callers must invoke it before Start; once a transport
// is bound the substitution is silently ignored. A nil reader or writer
// leaves the corresponding side untouched.
//
// SetStdio exists as a test seam so external packages (e.g. tools
// integration tests in a sibling package) can drive the server through
// in-memory io.Pipe pairs without reaching into unexported fields.
// Production callers leave the defaults (os.Stdin / os.Stdout) in place.
func (s *mcpServer) SetStdio(in io.Reader, out io.Writer) {
	if in != nil {
		s.stdin = in
	}
	if out != nil {
		s.stdout = out
	}
}

// Register installs tool into the server. It returns
// ErrToolAlreadyRegistered when a tool with the same Name() is already
// installed; mcp-go would otherwise silently overwrite the prior entry.
// The duplicate-name check runs before schema compilation so a duplicate
// registration with a malformed schema still surfaces the documented
// sentinel rather than a schema error. The tool's InputSchema is then
// compiled once so every subsequent invocation can validate arguments
// without re-parsing; a malformed schema fails registration so the
// server never accepts traffic for a tool with a broken contract.
// Register is safe to call before or after Start.
func (s *mcpServer) Register(tool Tool) error {
	if tool == nil {
		return errNilTool
	}
	name := tool.Name()

	s.toolsMu.Lock()
	if _, exists := s.tools[name]; exists {
		s.toolsMu.Unlock()
		return ErrToolAlreadyRegistered
	}
	// Compile under the lock so a concurrent Register for the same name
	// observes our claim immediately on its dup-check and we never reach
	// a partial state where compile succeeded but another caller stole
	// the slot.
	validator, err := compileSchema(name, tool.InputSchema())
	if err != nil {
		s.toolsMu.Unlock()
		return err
	}
	s.tools[name] = struct{}{}
	s.validators[name] = validator
	if u, ok := tool.(UnlistedTool); ok && u.Unlisted() {
		s.unlisted[name] = struct{}{}
	}
	s.toolsMu.Unlock()

	mcpTool, handler := adaptTool(tool, s.opts.Secrets, validator, s.opts.ToolInvokeTimeout)
	s.inner.AddTool(mcpTool, handler)
	return nil
}

// Start runs every transport enabled in Options. It returns nil on a
// clean shutdown, the first transport's fatal error otherwise, or the
// shutdown error when ctx is cancelled. Subsequent calls return the
// outcome of the first call.
func (s *mcpServer) Start(ctx context.Context) error {
	// Atomically check that Shutdown has not run and claim ownership of
	// startTransports. Holding lifecycleMu across both decisions stops the
	// pre-bind race where Shutdown closes shutdownC after the pre-check
	// but before startTransports binds, leaving listeners orphaned.
	// Greptile P1 from PR #80 re-review.
	s.lifecycleMu.Lock()
	select {
	case <-s.shutdownC:
		s.lifecycleMu.Unlock()
		return ErrServerShutdown
	default:
	}
	first := s.startedDone == nil
	if first {
		s.startedDone = make(chan struct{})
	}
	done := s.startedDone
	s.lifecycleMu.Unlock()

	if first {
		s.startTimeMu.Lock()
		s.startTime = time.Now()
		s.startTimeMu.Unlock()
		err := s.startTransports(ctx, s.stdin, s.stdout)
		s.lifecycleMu.Lock()
		s.startErr = err
		s.lifecycleMu.Unlock()
		close(done)
	} else {
		<-done
	}

	s.lifecycleMu.Lock()
	startErr := s.startErr
	s.lifecycleMu.Unlock()
	if startErr != nil {
		return startErr
	}

	select {
	case err := <-s.runErr:
		if shutdownErr := s.gracefulShutdown(ctx); shutdownErr != nil {
			return fmt.Errorf("mcp start: %w", errors.Join(err, shutdownErr))
		}
		return err
	case <-ctx.Done():
		return s.gracefulShutdown(ctx)
	case <-s.shutdownC:
		return nil
	case <-s.stdioClosed:
		// The stdio client closed stdin cleanly. Shut down all transports
		// (including any co-located Unix socket) and return nil so the
		// process exits instead of hanging until SIGTERM. This is the
		// correct MCP stdio contract: when the parent process that spawned
		// us closes its end of the pipe, we exit.
		return s.gracefulShutdown(ctx)
	}
}

// gracefulShutdown derives a shutdown context that inherits ctx's values
// (tracing, request id) but drops its cancellation, then runs Shutdown
// bounded by the configured grace timeout.
func (s *mcpServer) gracefulShutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownTimeout)
	defer cancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("mcp graceful shutdown: %w", err)
	}
	return nil
}

// startTransports binds every configured transport and spawns its
// goroutine. Stdin/stdout are parameterized so tests can substitute
// io.Pipe streams without touching os.Stdin/os.Stdout.
//
// On a bind failure mid-sequence (e.g. stdio launched but HTTP listen
// returns address-in-use), startTransports unwinds the partial state:
// every already-started transport is cancelled and its goroutine is
// awaited so the caller does not observe a half-constructed server.
func (s *mcpServer) startTransports(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	serveCtx, serveCancel := context.WithCancel(ctx)
	s.lnMu.Lock()
	s.serveCtx = serveCtx
	s.serveCancel = serveCancel
	s.lnMu.Unlock()

	if s.opts.Stdio {
		s.startStdio(ctx, stdin, stdout)
	}
	if s.opts.HTTPAddr != "" {
		if err := s.startHTTP(ctx); err != nil {
			s.rollbackPartialStart(ctx)
			return err
		}
	}
	if s.opts.UnixSocket != "" {
		if err := s.startUnix(ctx); err != nil {
			s.rollbackPartialStart(ctx)
			return err
		}
	}
	return nil
}

// rollbackPartialStart tears down every transport that successfully
// started before the failing one, then waits for their goroutines to
// exit. Used only on the partial-failure path inside startTransports;
// success and explicit Shutdown follow the closeTransports + wg.Wait
// path in Shutdown.
func (s *mcpServer) rollbackPartialStart(ctx context.Context) {
	s.lnMu.RLock()
	stdioCancel := s.stdioCancel
	serveCancel := s.serveCancel
	httpSrv := s.httpSrv
	unixSrv := s.unixSrv
	s.lnMu.RUnlock()
	if stdioCancel != nil {
		stdioCancel()
	}
	if serveCancel != nil {
		serveCancel()
	}
	if httpSrv != nil {
		if err := httpSrv.Shutdown(ctx); err != nil {
			s.opts.Logger.Warn("rollback http shutdown", zap.Error(err))
		}
	}
	if unixSrv != nil {
		if err := unixSrv.Shutdown(ctx); err != nil {
			s.opts.Logger.Warn("rollback unix shutdown", zap.Error(err))
		}
	}
	s.wg.Wait()
	// Mark the server as already-shutdown so a caller who mistakenly
	// invokes Shutdown after this failure observes idempotent behavior.
	s.markShutdown()
}

func (s *mcpServer) startStdio(ctx context.Context, stdin io.Reader, stdout io.Writer) {
	stdioCtx, cancel := context.WithCancel(ctx)
	s.lnMu.Lock()
	s.stdioCancel = cancel
	s.lnMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		stdio := mcpserver.NewStdioServer(s.inner)
		err := stdio.Listen(stdioCtx, stdin, stdout)
		if err == nil || errors.Is(err, io.EOF) {
			// Clean return: the client closed stdin. Signal Start so it
			// can run gracefulShutdown and the process exits rather than
			// hanging until SIGTERM. The once guard makes the close safe
			// against double-invocation (e.g. if the transport library
			// returns nil twice in a retry scenario).
			s.stdioOnce.Do(func() { close(s.stdioClosed) })
			return
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		s.reportRunError(fmt.Errorf("stdio listen: %w", err))
	}()
}

// buildHandler constructs the request mux for a transport. The liveness probe
// is mounted at HealthPath on each: TCP exposes it token-exempt (hardenTCP
// lets HealthPath through) and the Unix socket exposes it behind the 0600
// permission boundary. First-party probes (doctor's mcp.healthz check, the
// daemon-install readiness wait) reach the daemon over the Unix socket now
// that TCP is off by default, so the socket MUST serve /healthz — otherwise
// the probe falls through to the MCP catch-all and hangs on an open JSON-RPC
// stream. Both transports also include the optional REST surface at /v1/ (when
// ExtraRoutes is set) and the MCP JSON-RPC catch-all at /.
//
// IncludeAdmin controls whether AdminRoutes (the socket-only privileged write
// surface at /admin/) is mounted. Pass true for the Unix transport; false for
// TCP. The admin surface is structurally absent from TCP — it literally does
// not exist on that mux — so no runtime check is needed inside the handler.
func (s *mcpServer) buildHandler(includeAdmin bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, healthHandler(s.opts.Logger, s.boundStartTime))
	if s.opts.ExtraRoutes != nil {
		mux.Handle("/v1/", s.opts.ExtraRoutes)
	}
	if includeAdmin && s.opts.AdminRoutes != nil {
		mux.Handle("/admin/", s.opts.AdminRoutes)
	}
	mux.Handle("/", s.streamable)
	return mux
}

func (s *mcpServer) startHTTP(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.opts.HTTPAddr)
	if err != nil {
		return fmt.Errorf("http listen %q: %w", s.opts.HTTPAddr, err)
	}
	srv := &http.Server{
		// The TCP transport is hardened: a loopback Host/Origin allow-list
		// plus a constant-time bearer-token check (hardenTCP), under the
		// shared security headers. The Unix socket skips hardenTCP because
		// its 0600 permissions are the credential.
		// AdminRoutes (false) is structurally absent from TCP.
		Handler:           securityHeaders(hardenTCP(s.buildHandler(false), s.opts.Token, s.opts.Logger)),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		BaseContext:       s.requestBaseContext,
	}
	s.lnMu.Lock()
	s.httpLn = ln
	s.httpSrv = srv
	s.lnMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		serveErr := srv.Serve(ln)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			s.reportRunError(fmt.Errorf("http serve: %w", serveErr))
		}
	}()
	return nil
}

func (s *mcpServer) startUnix(ctx context.Context) error {
	if err := clearStaleUnixSocket(ctx, s.opts.UnixSocket); err != nil {
		return err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", s.opts.UnixSocket)
	if err != nil {
		return fmt.Errorf("unix listen %q: %w", s.opts.UnixSocket, err)
	}
	// Restrict the socket to the owning user. Bind creates the inode
	// under the process umask, which may leave it world-readable; the
	// chmod closes that gap. A narrow race window remains between bind
	// and chmod (another local user could connect during it) which is
	// acceptable for loopback single-user M1 and revisited if Mazarbul
	// targets multi-user hosts.
	if chmodErr := os.Chmod(s.opts.UnixSocket, 0o600); chmodErr != nil {
		if closeErr := ln.Close(); closeErr != nil {
			return fmt.Errorf("chmod unix socket %q: %w (close failed: %w)", s.opts.UnixSocket, chmodErr, closeErr)
		}
		return fmt.Errorf("chmod unix socket %q: %w", s.opts.UnixSocket, chmodErr)
	}
	srv := &http.Server{
		// No hardenTCP gate: the socket's 0600 permissions are the
		// credential. Still carries the shared security headers.
		// AdminRoutes (true) is mounted only here — structurally absent from TCP.
		Handler:           securityHeaders(s.buildHandler(true)),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		BaseContext:       s.requestBaseContext,
	}
	s.lnMu.Lock()
	s.unixLn = ln
	s.unixSrv = srv
	s.lnMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		serveErr := srv.Serve(ln)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			s.reportRunError(fmt.Errorf("unix serve: %w", serveErr))
		}
	}()
	return nil
}

// requestBaseContext is the BaseContext callback for the HTTP/Unix
// http.Server instances. Returning s.serveCtx makes every in-flight
// request observe Shutdown via ctx.Done() once s.serveCancel fires.
func (s *mcpServer) requestBaseContext(_ net.Listener) context.Context {
	s.lnMu.RLock()
	serveCtx := s.serveCtx
	s.lnMu.RUnlock()
	if serveCtx == nil {
		return context.Background()
	}
	return serveCtx
}

// reportRunError non-blockingly publishes a fatal transport error. The
// buffered runErr channel holds the first failure; subsequent failures
// are dropped so Shutdown is not deadlocked.
func (s *mcpServer) reportRunError(err error) {
	select {
	case s.runErr <- err:
	default:
	}
}

// Shutdown stops every transport and waits for in-flight requests to
// drain, bounded by ctx. It returns context.DeadlineExceeded if the
// grace period elapses before drain completes, plus any transport-level
// shutdown errors joined and wrapped.
//
// Shutdown is idempotent: subsequent calls observe the already-closed
// shutdown channel and return nil.
func (s *mcpServer) Shutdown(ctx context.Context) error {
	first, started := s.markShutdown()
	if !first {
		return nil
	}

	// If a Start call is in flight, wait for its startTransports to
	// finish so the listeners it bound are visible to closeTransports.
	// Without this wait, the race window between Start passing the
	// shutdownC check and Start binding the listener would let
	// closeTransports snapshot an empty set, orphaning the listener.
	// The wait is bounded by ctx so a stuck startTransports cannot pin
	// Shutdown past the caller's grace deadline; in the timeout path we
	// still tear down whatever has already been published. Greptile P1
	// from PR #80 re-review round 3.
	var errs []error
	if started != nil {
		select {
		case <-started:
		case <-ctx.Done():
			errs = append(s.closeTransports(ctx), ctx.Err())
			if joined := errors.Join(errs...); joined != nil {
				return fmt.Errorf("mcp shutdown: %w", joined)
			}
			return nil
		}
	}

	errs = s.closeTransports(ctx)

	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}

	s.lnMu.RLock()
	hadUnix := s.unixLn != nil
	s.lnMu.RUnlock()
	if hadUnix {
		if err := os.Remove(s.opts.UnixSocket); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove unix socket: %w", err))
		}
	}

	if joined := errors.Join(errs...); joined != nil {
		return fmt.Errorf("mcp shutdown: %w", joined)
	}
	return nil
}

// markShutdown closes shutdownC exactly once under lifecycleMu. The
// first caller receives (true, startedDone) where startedDone is nil if
// no Start has been attempted yet, or the channel a concurrent Start
// will close after startTransports finishes. Subsequent callers receive
// (false, nil) and should treat Shutdown as a no-op.
func (s *mcpServer) markShutdown() (bool, chan struct{}) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	select {
	case <-s.shutdownC:
		return false, nil
	default:
		close(s.shutdownC)
		return true, s.startedDone
	}
}

// closeTransports stops every active transport. Errors are collected
// rather than returned so the caller can continue draining in-flight
// requests even when one listener's shutdown failed.
func (s *mcpServer) closeTransports(ctx context.Context) []error {
	var errs []error
	s.lnMu.RLock()
	stdioCancel := s.stdioCancel
	serveCancel := s.serveCancel
	httpSrv := s.httpSrv
	unixSrv := s.unixSrv
	s.lnMu.RUnlock()

	if stdioCancel != nil {
		stdioCancel()
	}
	if serveCancel != nil {
		serveCancel()
	}

	if httpSrv != nil {
		if err := httpSrv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("http shutdown: %w", err))
		}
	}
	if unixSrv != nil {
		if err := unixSrv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("unix shutdown: %w", err))
		}
	}
	if s.streamable != nil {
		if err := s.streamable.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("streamable shutdown: %w", err))
		}
	}
	return errs
}

// clearStaleUnixSocket removes a leftover socket file at path so a fresh
// bind can succeed. A previous daemon that crashed before its Shutdown
// ran will have left the file behind; without this probe the next start
// fails with "address already in use" even though no live owner exists.
//
// The probe distinguishes stale from live: a successful DialTimeout to
// the socket means a peer is accepting connections and we MUST NOT remove
// it (returning an error so the caller surfaces "address already in
// use"); a dial failure with the file present means stale and we remove
// it. ENOENT means clean state, nothing to do.
func clearStaleUnixSocket(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat unix socket %q: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("unix socket path %q exists and is not a socket", path)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	dialer := net.Dialer{}
	conn, dialErr := dialer.DialContext(dialCtx, "unix", path)
	if dialErr == nil {
		if closeErr := conn.Close(); closeErr != nil {
			return fmt.Errorf("close stale-probe conn for %q: %w", path, closeErr)
		}
		return fmt.Errorf("unix socket %q is in use by another process", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale unix socket %q: %w", path, err)
	}
	return nil
}

// boundStartTime returns the time Start captured when it first bound
// transports. Used by the /healthz handler so the JSON response reflects
// the live server's boot time. A zero value means Start has not run.
func (s *mcpServer) boundStartTime() time.Time {
	s.startTimeMu.RLock()
	defer s.startTimeMu.RUnlock()
	return s.startTime
}

// httpListenerAddr returns the bound TCP listener's address, if any. It
// is the test-friendly seam for discovering an ephemeral port chosen by
// net.ListenConfig.Listen with HTTPAddr "127.0.0.1:0".
func (s *mcpServer) httpListenerAddr() net.Addr {
	s.lnMu.RLock()
	ln := s.httpLn
	s.lnMu.RUnlock()
	if ln == nil {
		return nil
	}
	return ln.Addr()
}
