package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

// DefaultShutdownTimeout is the grace period New gives Shutdown when an
// Options value leaves ShutdownTimeout zero.
const DefaultShutdownTimeout = 10 * time.Second

// DefaultHTTPAddr is the canonical loopback address for the opt-in TCP
// transport — the conventional value to pass to `--http-addr` / MAZ_HTTP_ADDR
// when enabling TCP. It is NOT a default bind: TCP is off unless explicitly
// enabled (the daemon binds only the Unix socket otherwise). Loopback only;
// TLS and non-loopback binds are out of scope.
const DefaultHTTPAddr = "127.0.0.1:7842"

// unixSocketFilename is the basename DefaultUnixSocket appends to the
// caller's data directory. Kept private so callers cannot accidentally
// derive a different path than the one Shutdown removes.
const unixSocketFilename = "mazarbul.sock"

// DefaultUnixSocket returns the conventional Unix-socket path for a
// Mazarbul instance whose data directory is dataDir. The socket lives
// alongside the event log and SQLite database under the same data dir
// so a deployment has a single location to inspect for state.
//
// Path length is the binding constraint: sun_path caps at 104 bytes on
// macOS/BSD and 108 on Linux. A typical Mazarbul data directory under
// ~/Library/Application Support/Mazarbul (darwin) or
// ~/.local/share/mazarbul (linux) yields a socket path well under both
// limits.
func DefaultUnixSocket(dataDir string) string {
	return filepath.Join(dataDir, unixSocketFilename)
}

// Options configures a Server. Name and Version are required, and at
// least one transport (Stdio, HTTPAddr, UnixSocket) must be enabled.
//
// Construct via New(opts) once; the returned Server is reusable across
// many tool calls but not across restarts.
type Options struct {
	// Name is the MCP server name advertised to clients. Typically the
	// Mazarbul application name, e.g. "mazarbul".
	Name string

	// Version is the MCP server version advertised to clients. Typically
	// the daemon build version.
	Version string

	// Logger is the zap logger used for tool-invocation logging and
	// transport lifecycle events. Nil falls back to zap.NewNop(). The
	// daemon supervisor injects the configured logger here.
	Logger *zap.Logger

	// Secrets resolves named secrets at tool invocation time. Threaded
	// onto every tool-call context via WithSecrets. Nil falls back to
	// NoopSecretProvider, which always returns ErrSecretNotFound.
	Secrets SecretProvider

	// Stdio runs the JSON-RPC handler over stdin and stdout. Used by
	// Claude Code and other inspector workflows that spawn the server as
	// a subprocess.
	Stdio bool

	// HTTPAddr is the TCP listen address for the streamable HTTP
	// transport, e.g. "127.0.0.1:7842" or "127.0.0.1:0" for ephemeral.
	// Empty disables HTTP. The TCP transport is off by default; it is an
	// opt-in surface (the Unix socket is the trusted local channel) and
	// when enabled it is gated by Token plus a loopback Host/Origin
	// allow-list (see hardenTCP).
	HTTPAddr string

	// Token is the bearer credential the HTTP (TCP) transport requires on
	// every request except the health probe. It is minted by the daemon
	// supervisor and persisted 0600 in the data dir for local clients to
	// read. Required (and validated non-empty) whenever HTTPAddr is set;
	// unused by the Unix-socket and stdio transports, whose access control
	// is the socket's filesystem permissions / process ownership.
	Token string

	// UnixSocket is the filesystem path for the Unix-socket transport,
	// e.g. "/tmp/mazarbul.sock". Empty disables Unix sockets. The path
	// is removed on Shutdown.
	UnixSocket string

	// ShutdownTimeout bounds the grace period Shutdown gives in-flight
	// requests to complete. Zero defaults to DefaultShutdownTimeout.
	ShutdownTimeout time.Duration

	// ToolInvokeTimeout backstops a single tool invocation that never
	// returns (a hung Ollama call, a stuck lock). The adapter wraps every
	// Tool.Invoke whose tool is not a SelfTimingTool in this deadline so a
	// wedged handler returns a clean E_TIMEOUT and frees its goroutine
	// instead of pinning it forever. Zero defaults to
	// defaultToolInvokeTimeout. Over a proxy the client's shorter deadline
	// fires first; this is the backstop for direct/in-process callers with
	// no shorter bound.
	ToolInvokeTimeout time.Duration

	// ExtraRoutes is an optional handler mounted at "/v1/" on the HTTP and
	// Unix-socket transports, evaluated before the MCP catch-all at "/".
	// Nil leaves the mux unchanged. Used by the daemon supervisor to attach
	// the REST /v1 surface alongside the JSON-RPC MCP surface without
	// forking the transport lifecycle.
	//
	// Note: /v1/ stays read-only. The daemon's write surface is AdminRoutes
	// below, mounted only on the Unix socket.
	ExtraRoutes http.Handler

	// AdminRoutes is an optional handler mounted at "/admin/" on the
	// Unix-socket transport ONLY. It is structurally absent from the TCP
	// transport (buildHandler(false) skips it), so no runtime check is
	// needed. Used by the daemon supervisor to attach the privileged write
	// surface (POST /admin/v1/events:append) that CLI authoring commands
	// use to route writes through the live daemon without stopping it.
	//
	// Never set this on a TCP-only server.
	AdminRoutes http.Handler

	// InstructionComposer, when set, recomputes the MCP `initialize`
	// instructions per connection in an OnAfterInitialize hook so every NEW
	// session re-reads current state (active presets) and folds it into the
	// system prompt (Channel A, #214). It returns the FULL composed
	// instructions (base manual + bootstrap pointer + active-preset section);
	// the daemon builds it from ComposeServerInstructions over the brief
	// assembler. A nil composer, a composition error, or an empty result leaves
	// the static WithInstructions base in place — the hook cannot return an
	// error, so it degrades cleanly to base: a composition error is logged at Warn,
	// while an empty result (the normal "no active preset" case) is left silently
	// to avoid per-connection log noise.
	// This is internal wiring, not user-facing config (no knob).
	InstructionComposer func(ctx context.Context) (string, error)
}

// validate normalizes optional fields and returns an error if a required
// field is missing or every transport is disabled. It mutates the
// receiver in place; callers should pass a pointer.
func (o *Options) validate() error {
	if o.Name == "" {
		return errors.New("mcp: Options.Name required")
	}
	if o.Version == "" {
		return errors.New("mcp: Options.Version required")
	}
	if !o.Stdio && o.HTTPAddr == "" && o.UnixSocket == "" {
		return errors.New("mcp: Options must enable at least one transport (Stdio, HTTPAddr, UnixSocket)")
	}
	if o.HTTPAddr != "" && o.Token == "" {
		// Fail closed: the TCP transport is reachable by any local process
		// and, via DNS rebinding, by a hostile web page. It must never be
		// mounted without a bearer credential.
		return errors.New("mcp: HTTPAddr requires a non-empty Token")
	}
	if o.ShutdownTimeout < 0 {
		return errors.New("mcp: Options.ShutdownTimeout must be non-negative")
	}
	if o.ShutdownTimeout == 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	if o.Logger == nil {
		o.Logger = zap.NewNop()
	}
	if o.Secrets == nil {
		o.Secrets = NoopSecretProvider{}
	}
	return nil
}

// SecretProvider resolves named secrets at tool-invocation time. Tools
// that need provider API keys, encryption keys, or any sensitive material
// pull them from a context-threaded SecretProvider rather than reading
// env vars directly. This lets the daemon supervisor swap providers
// (file, keychain, remote vault) without tools knowing the difference.
//
// Implementations should return ErrSecretNotFound when name is unknown
// and wrap any retrieval failure (decryption, network) for callers.
type SecretProvider interface {
	Secret(ctx context.Context, name string) (string, error)
}

// NoopSecretProvider returns ErrSecretNotFound for every name. The
// default when Options.Secrets is nil.
type NoopSecretProvider struct{}

// Secret implements SecretProvider. It always returns ErrSecretNotFound.
func (NoopSecretProvider) Secret(_ context.Context, name string) (string, error) {
	return "", fmt.Errorf("mcp: secret %q: %w", name, ErrSecretNotFound)
}

// ErrSecretNotFound is the sentinel returned when a named secret is
// absent. Implementations wrap it with the requested name for context.
var ErrSecretNotFound = sentinelError("mcp: secret not found")

type secretsKey struct{}

// WithSecrets returns ctx carrying sp. The server attaches the
// configured provider to every tool-call context before dispatching to
// Tool.Invoke; tools read it with SecretsFromContext.
//
// A nil provider is silently ignored; the original ctx is returned.
func WithSecrets(ctx context.Context, sp SecretProvider) context.Context {
	if sp == nil {
		return ctx
	}
	return context.WithValue(ctx, secretsKey{}, sp)
}

// SecretsFromContext returns the SecretProvider attached to ctx, or
// NoopSecretProvider{} when none is present.
func SecretsFromContext(ctx context.Context) SecretProvider {
	if v, ok := ctx.Value(secretsKey{}).(SecretProvider); ok && v != nil {
		return v
	}
	return NoopSecretProvider{}
}
