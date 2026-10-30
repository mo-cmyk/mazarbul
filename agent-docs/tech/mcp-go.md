# mcp-go

Mazarbul exposes its primary integration surface as an MCP server. The Go library is **[`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go)** (pinned at `v0.54.0`), wrapped behind `internal/mcp` per the design doc at `agent-docs/designs/mcp-server.md`. See issue [#23](https://github.com/mo-cmyk/mazarbul/issues/23).

## Wrapping surface

The kernel never imports `mcp-go` directly. `internal/mcp/server.go` provides:

- `mcp.New(opts Options) (Server, error)` — constructor.
- `mcp.Server` interface — `Register(Tool)`, `Start(ctx)`, `Shutdown(ctx)`.
- `mcp.Tool` interface — `Invoke(ctx, json.RawMessage) (json.RawMessage, error)`.
- `mcp.SecretProvider` — context-threaded secrets resolver.
- `mcp.ToolError` + `Code{Validation,NotFound,Internal}` — typed errors with stable codes; see "Error codes" below.

Tools are registered before `Start` and dispatched concurrently across the configured transports (stdio, streamable HTTP, Unix socket). At registration time, `Register` compiles the tool's `InputSchema` once via `santhosh-tekuri/jsonschema/v6`; every invocation validates arguments against the compiled schema before reaching `Tool.Invoke`. A malformed schema fails `Register` so the server never accepts traffic for a tool with a broken contract.

## Stdio: prefer `Listen(ctx, ...)` over `ServeStdio`

`mcp-go` exposes two stdio entry points:

| Function | File | Context-aware? |
|---|---|---|
| `server.ServeStdio(srv)` | `server/stdio.go:866` | No, stops only on SIGINT/SIGTERM. |
| `(*StdioServer).Listen(ctx, in, out)` | `server/stdio.go:530` | Yes, see doc string at line 527. |

We use `Listen(ctx, ...)` so the daemon supervisor (sibling issue #15) can call `Shutdown(ctx)` with a deadline. The supervisor owns SIGINT/SIGTERM; the MCP server stays signal-agnostic.

A blocked `bufio.NewReader(stdin).ReadString('\n')` is not interruptible by ctx cancellation. The reader returns only on peer EOF, the next byte, or process exit. The supervisor is responsible for the process-exit fallback.

## HTTP and Unix sockets: own the `http.Server`

`mcp-go`'s `StreamableHTTPServer.Start(addr)` constructs its own internal `http.Server`. We bypass `Start` and use `StreamableHTTPServer` directly as an `http.Handler`, wiring it into our own `http.Server` per transport. That gets us:

- Listener discovery via `net.ListenConfig{}.Listen(ctx, "tcp", "127.0.0.1:0")` for tests.
- `BaseContext` injection so `Shutdown` cancellation reaches in-flight tool handlers.
- `ReadHeaderTimeout` for Slowloris protection.

We still call `streamable.Shutdown(ctx)` on shutdown so the session sweeper goroutine terminates cleanly.

### Default addresses

`internal/mcp` exports two constants the daemon supervisor (#42) and the foreground `maz mcp serve` command both consume:

- `mcp.DefaultHTTPAddr = "127.0.0.1:7842"`. Loopback only, no TLS in M1.
- `mcp.DefaultUnixSocket(dataDir)` returns `<dataDir>/mazarbul.sock`. Colocated with the event log so a deployment has a single state directory. Path length stays comfortably under the 104-byte `sun_path` limit on macOS/BSD (108 on Linux) for both XDG and `~/Library/Application Support` resolutions.

### Unix socket file mode

After `lc.Listen(ctx, "unix", path)` returns, `internal/mcp` calls `os.Chmod(path, 0o600)` so the inode is reachable only by the owning user. Bind creates the file under the process umask, which may leave it world-readable on systems with `umask 022`; the chmod closes that gap. A narrow window remains between bind and chmod where a local user could connect; acceptable for loopback single-user M1 and revisited if Mazarbul targets multi-user hosts.

### Health endpoint (HTTP transport only)

The HTTP `http.Server` is constructed with a `*http.ServeMux` that routes `/healthz` to a local handler and delegates everything else to `StreamableHTTPServer`. The path is exported as `mcp.HealthPath`. The Unix listener serves the bare streamable handler with no mux: `/healthz` is not reachable over Unix by design, so probes do not silently report "healthy" when the HTTP transport is down.

Response shape on `GET /healthz`:

```jsonc
// HTTP/1.1 200 OK
// Content-Type: application/json
{
  "status": "ok",
  "startTime": "2026-05-24T14:32:18.482Z"
}
```

`startTime` is captured the first time `Start` runs and serialized as RFC3339. Consumers compute uptime as `now - startTime`, mirroring Prometheus's "export the timestamp, not the elapsed duration" guidance and AIP-142's preference for instants over durations.

## Observability: `WithHooks`

Every tool invocation is logged via `mcpserver.WithHooks(*Hooks)`:

| Hook | Behavior |
|---|---|
| `AddBeforeCallTool` | Stores start time keyed on the JSON-RPC id. |
| `AddAfterCallTool` | Emits one zap log line; `status="error"` when `result.IsError`. |
| `AddOnError` | Emits one zap log line on JSON-RPC error (filtered to `MethodToolsCall`). |

`request_id` resolves to `correlation.FromContext(ctx)` first, falling back to mcp-go's JSON-RPC id.

## Ambient context: per-session `initialize` instructions (#214)

The MCP `initialize` response carries an `instructions` string that clients (Claude Code, Claude Desktop, Cursor, …) typically inject into the agent's system prompt **from turn zero, without the agent calling a tool**. Mazarbul uses it as **Channel A**: the operating manual + active-preset steering arrive automatically.

It is **composed per session, not compiled in**. `internal/mcp.New` registers a stock `Hooks.AddAfterInitialize` callback alongside the logging hooks (same `*mcpserver.Hooks`). mcp-go builds the initialize result — copying the static `WithInstructions` base into `result.Instructions` — and *then* calls the after-initialize hook with a pointer to that result, so the hook's mutation wins. The hook calls `Options.InstructionComposer`; the daemon wires that composer over the brief assembler (`ComposeServerInstructions(base + bootstrap pointer + active-preset section)`). No wrapper, no fork.

Composition shape (`ComposeServerInstructions`):

1. **Base manual** — the static routing rules (recall/remember/relate/forget/navigate). Load-bearing routing stays in the first ~512 chars: clients may truncate, so the decision rule leads.
2. **Bootstrap pointer** — one line telling the agent to call `getContext` for its cold-start brief.
3. **Active-preset "Operating instructions" section, LAST** — every active preset's fenced body (#210's single-source projection), bounded by `contextbrief`'s per-section budget. It is placed last because preset bodies are multiple KiB of UNTRUSTED installed content; keeping them last leaves the trusted routing + getContext pointer in the early window a truncating client preserves. The bodies are markdown-escaped and wrapped in a non-spoofable untrusted-content fence inside `contextbrief.Assembler` before they ever reach this sink.

**Freshness contract — per-SESSION, not mid-session.**

- ✅ **Per-session refresh is real and client-agnostic.** Every NEW connection re-runs the composer, so it re-reads the current active-preset projection. Activate a preset, reconnect, and the new session reflects it.
- ❌ **Mid-session push is structurally impossible over MCP.** There is no server→client channel to replace `instructions` on an already-open connection. Documented, not attempted. (`notifications/list_changed`-style push — "Channel D" — is **cut**, matching the presets decision.)

**Proxy-mode caveat (Claude Desktop + running daemon).** When a daemon already holds the LadybugDB lock, `maz mcp serve` does not open the store; it runs as a thin stdio↔HTTP **proxy** (`internal/cli/mcp_proxy.go`) bridging the client to the daemon's `/mcp` endpoint. The proxy builds its own local `mcpserver.MCPServer`, so it must explicitly carry the daemon's composed instructions across the bridge: `connectAndSnapshotTools` captures `InitializeResult.Instructions` at connect time and feeds them to the proxy server via `WithInstructions`. Without this the proxied `initialize` returns an empty system prompt — silently defeating Channel A for the very path proxy mode exists for. The snapshot is per-proxy-process (1:1 with the stdio client's session), so it still honors the per-session contract above; a re-launched client re-reads current preset state.

**Channel map.** Only Channel A ships in v1.

| Channel | What | Status |
|---|---|---|
| A | Per-session `initialize` instructions (this section) | **Ships.** Client-agnostic. |
| B | Claude Code plugin + SessionStart/UserPromptSubmit hooks + fast `maz context read` | **Deferred** (#215, closed-as-deferred). Claude-Code-only; re-introduces a per-turn auto-injection hazard with zero agent discretion. |
| C | MCP resources/prompts auto-attach (`@getContext` handle) | **Deferred** to additive v2 (thin-client support). |
| D | `notifications/list_changed` push-refresh | **Cut.** Mid-session push is impossible over MCP. |

**Trust boundary (corrected).** Channel A lands content in the system prompt — the highest-trust sink. The CLI reaches the daemon over **unauthenticated loopback TCP `127.0.0.1:7842`** (`internal/cli/preflight.go`), not a 0600 Unix socket; any local process can dial it. That is why all brief content — preset bodies, document excerpts, recall-hit text — is escaped + fenced in `contextbrief.Assembler` (the `excerptBody`/`hitBullet` codepath shares one neutralization with the preset fence in `provenance.go`). The Unix-socket transport's 0600 mode (above) bounds only that transport; the loopback TCP listener is the one the CLI and most clients actually use.

## Gotchas

- **Duplicate tool names panic.** `mcp-go`'s `AddTool` silently overwrites; without guarding, two tools registered under the same name would clobber each other. `mcp.Server.Register` pre-checks an in-package map and returns `ErrToolAlreadyRegistered`.
- **Argument shape varies.** `req.Params.Arguments` is `any`; sometimes `map[string]any`, sometimes `json.RawMessage`. Use `req.GetRawArguments()` then `json.Marshal` for a stable byte stream.
- **Application errors return through `IsError`.** A non-nil error from `Tool.Invoke` becomes `IsError: true` content, not a JSON-RPC error. JSON-RPC errors are reserved for protocol failures (parse error, method not found).
- **Schema validation is mandatory at the adapter.** Tools cannot bypass it by returning early in `Invoke`; the validator runs first. If you want to accept any shape, declare `{"type":"object"}` (or an empty schema) and validate downstream.

## Tool definition pattern (library-agnostic)

Regardless of library, MCP tools follow the same shape:

```jsonc
{
  "name": "remember",
  "description": "Append a fact to the event log with provenance. Required: fact.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "fact":   { "type": "string",  "description": "Required. The fact to remember." },
      "scope":  { "type": "string",  "description": "Optional. project|personal|team. Default: personal." },
      "source": { "type": "string",  "description": "Optional. Source identifier for provenance." }
    },
    "required": ["fact"]
  }
}
```

Naming conventions follow Google AIP (see `agent-docs/synthesis.md` section 3). Tools return either the resource (for `Get*` / `Create*`) or a `*Response` wrapper (for `List*` / `Batch*`).

## Error codes

Two distinct error surfaces, by intent:

**Protocol-level (JSON-RPC envelope).** Emitted by `mcp-go` itself when the JSON-RPC envelope is malformed or the method does not exist. Never synthesized by Mazarbul code.

- `-32600` invalid request
- `-32601` method not found (unknown tool)
- `-32700` parse error

**Tool-level (`CallToolResult.IsError = true`).** Surfaced via `*mcp.ToolError`. Mazarbul does NOT route tool failures through JSON-RPC error responses; the MCP spec says tool errors live on the result object so the calling LLM can see and self-correct. The wire shape is:

```jsonc
{
  "result": {
    "isError": true,
    "content": [{"type": "text", "text": "[E_VALIDATION] fact is required"}],
    "structuredContent": {
      "code": "E_VALIDATION",
      "message": "fact is required",
      "details": { /* AIP-193 errorInfo-style metadata, optional */ }
    }
  }
}
```

Stable codes (additive over time):

| Code            | When                                                                                   |
| --------------- | -------------------------------------------------------------------------------------- |
| `E_VALIDATION`  | Arguments did not satisfy the tool's `InputSchema`. Returned **before** `Tool.Invoke`. |
| `E_NOT_FOUND`   | Handler ran but the target resource is absent (e.g. recall by missing id).             |
| `E_INTERNAL`    | Handler returned an unexpected error. Bare `error` returns fall back to this code.     |

**Tool authors:** return `*mcp.ToolError` to surface a specific code:

```go
return nil, mcp.NewNotFoundError(fmt.Sprintf("person %q not found", id))
```

Bare `error` returns get wrapped automatically as `E_INTERNAL` with the message preserved as the `Cause`. Schema validation never reaches your handler: a malformed payload is rejected at the adapter and surfaces as `E_VALIDATION` with the validator's structured error tree carried in `details` (from `BasicOutput()`).

## Foreground debug: `maz mcp serve`

The daemon supervisor (#42) is the production entry point. For inspectors (mcp-inspector, Claude Desktop) and ad-hoc debug, run the server in the foreground via `maz mcp serve`:

```
maz mcp serve                          # stdio only, the default
maz mcp serve --http                   # also enable HTTP at 127.0.0.1:7842
maz mcp serve --http=127.0.0.2:9000    # explicit address
maz mcp serve --unix                   # also enable Unix at <data-dir>/mazarbul.sock
maz mcp serve --no-stdio --http        # HTTP only, no stdio
```

`--http` and `--unix` use pflag's `NoOptDefVal` so the bare flag opts into the well-known default; an explicit `--http=<addr>` overrides. `--no-stdio --http=...` is the form an HTTP-only debug session uses; the command refuses to start if all three transports end up disabled.
