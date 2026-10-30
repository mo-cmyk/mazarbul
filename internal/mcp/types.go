package mcp

import (
	"context"
	"encoding/json"
	"io"
)

// Tool is one MCP tool exposed by the server. Real tools wrap kernel
// operations (remember, recall, forget, relate, traverse, review) and
// connector capabilities. M1 ships with the six core tools; M3+ adds
// tools registered by connectors.
//
// JSON Schemas for inputs and outputs follow Google AIP naming
// (createTime, plural collection paths, CRUD verbs) and are validated
// against the schemas the ontology registry holds.
type Tool interface {
	// Name is the MCP tool name shown to agents. AIP verb form preferred:
	// "remember", "recall", "forget", "relate", "traverse", "review".
	Name() string

	// Description is a short sentence shown to agents in MCP tool discovery.
	// Should be agent-readable: "Append a fact to the event log with
	// provenance" rather than "Calls eventlog.Append".
	Description() string

	// InputSchema is the JSON Schema for the tool's input payload.
	// Returned to MCP clients during tool discovery.
	InputSchema() json.RawMessage

	// OutputSchema is the JSON Schema for the tool's output payload.
	OutputSchema() json.RawMessage

	// Invoke runs the tool. The request and response are JSON-RPC payloads
	// already conforming to InputSchema and OutputSchema respectively.
	Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error)
}

// UnlistedTool is an optional interface a Tool may implement to opt out of the
// advertised tools/list while remaining fully dispatchable via tools/call. The
// server registers the tool's handler as normal but filters it out of tool
// discovery, so external agents never see it; in-process callers and explicit
// tools/call by name still reach it.
//
// The motivating case is `ask`: it composes an answer with the daemon's own
// LLM, which is wasted compute when the caller is itself an agent. Hiding it
// from tools/list keeps agents on the LLM-free navigation verbs (recall, get,
// neighbors, relations, …) while `maz ask` — which dispatches by name through
// the daemon — keeps working.
type UnlistedTool interface {
	Tool

	// Unlisted reports whether this tool should be excluded from tools/list.
	// A true value hides it from discovery only; dispatch is unaffected.
	Unlisted() bool
}

// SelfTimingTool is an optional interface a Tool may implement to opt out of
// the adapter's default per-invocation deadline. A tool implements it when it
// owns its own time budget: waitForIdle takes a caller-supplied timeoutSeconds
// (up to an hour) and returns timedOut:true rather than erroring, and the bulk
// ingest tools run for as long as the source legitimately takes.
//
// For every other tool the adapter wraps Invoke in a default deadline
// (Options.ToolInvokeTimeout) so a wedged handler — a hung Ollama call, a
// stuck lock — returns a clean E_TIMEOUT and frees its goroutine instead of
// pinning it forever. Over a proxy the client's shorter deadline fires first,
// so this is the backstop for direct/in-process callers with no shorter bound.
type SelfTimingTool interface {
	Tool

	// SelfTiming reports whether the tool bounds its own execution time. A
	// true value exempts it from the adapter's default deadline.
	SelfTiming() bool
}

// Transport identifies the wire on which the server accepts requests. M1
// supports all three; the daemon supervises all three concurrently.
type Transport string

// MCP transport identifiers. Stdio matches the inspector / subprocess flow;
// HTTP and Unix socket support multi-client connections.
const (
	TransportStdio      Transport = "stdio"
	TransportHTTP       Transport = "http"
	TransportUnixSocket Transport = "unix"
)

// Server is the MCP server contract. Real implementations are built on
// mark3labs/mcp-go and bridge to the kernel + plugin Host. Transport
// selection lives in Options at construction time; Start blocks until
// shutdown or ctx cancellation.
type Server interface {
	// Register installs a tool. Tools must have unique Name() values within
	// a Server; the second registration of the same name returns
	// ErrToolAlreadyRegistered.
	Register(tool Tool) error

	// Start runs every transport enabled in Options. It blocks until ctx is
	// cancelled, Shutdown is called, or any transport returns a fatal error.
	// Returns nil on a clean shutdown.
	Start(ctx context.Context) error

	// Shutdown stops every listener and waits for in-flight requests to
	// complete, bounded by ctx. Returns context.DeadlineExceeded if the
	// grace deadline elapses before drain completes.
	Shutdown(ctx context.Context) error
}

// StdioConfigurer is the optional capability the concrete Server
// implementation exposes for swapping the stdio reader/writer pair before
// Start. External tests that drive the server through in-memory io.Pipes
// type-assert Server to this interface; production callers do not need
// it. Sibling implementations (the test NoopServer) deliberately do not
// satisfy it.
type StdioConfigurer interface {
	SetStdio(in io.Reader, out io.Writer)
}

// ContextAssembler is the brief-building surface the MCP resource handlers
// consume. Satisfied by *contextbrief.Assembler. Declared here (in the mcp
// package rather than contextbrief) so the daemon wiring layer can thread it
// through Wired without creating an import cycle.
type ContextAssembler interface {
	Build(ctx context.Context, req ContextBriefRequest) (ContextBrief, error)
	BuildIndex(ctx context.Context, req ContextIndexRequest) (ContextIndex, error)
}

// DocumentReader is the minimal navigator surface the document MCP resource
// handler consumes. Satisfied by *navigate.Navigator.
type DocumentReader interface {
	GetDocumentByPath(ctx context.Context, path string) (DocumentRecord, error)
	GetDocumentBySlug(ctx context.Context, slug string) (DocumentRecord, error)
	ListDocuments(ctx context.Context, opts DocumentListOpts) ([]DocumentRecord, error)
}

// ResourceRegistrar is the optional capability the concrete MCP server
// exposes for installing context + document resources after the assembler and
// navigator are available. The daemon type-asserts Server to this interface
// after building it; implementations that do not support resources (e.g.
// NoopServer) simply do not satisfy it and resource registration is skipped.
type ResourceRegistrar interface {
	RegisterContextResources(assembler ContextAssembler, nav DocumentReader)
}

// ContextBriefRequest is the wire shape contextbrief.Request exposes through
// the ContextAssembler interface. The zero value is valid: produces a
// query-less brief at the default budget.
type ContextBriefRequest struct {
	Query        string
	BudgetTokens int
}

// ContextBrief is the assembled context brief returned by ContextAssembler.Build.
type ContextBrief struct {
	Markdown string `json:"markdown"`
	Tokens   int    `json:"tokens"`
	Budget   int    `json:"budget"`
	Dropped  int    `json:"dropped"`
}

// ContextIndexRequest is the wire shape contextbrief.IndexRequest exposes
// through the ContextAssembler interface.
type ContextIndexRequest struct {
	RecentLimit int
	EntityLimit int
}

// ContextIndex is the assembled vault directory returned by ContextAssembler.BuildIndex.
type ContextIndex struct {
	Markdown  string `json:"markdown"`
	Documents int    `json:"documents"`
	Folders   int    `json:"folders"`
}

// DocumentRecord is the document shape DocumentReader returns.
type DocumentRecord struct {
	DocumentUID string
	Path        string
	Title       string
	Body        string
	Pinned      bool
	// Scope is the document's storage partition (project / personal / team /
	// draft). With composite (path, scope) uniqueness a path can name one
	// document per scope, so a resource consumer needs the scope to know which
	// partition it was handed.
	Scope string
}

// DocumentListOpts narrows ListDocuments. PinnedOnly restricts to pinned
// documents; Limit of -1 removes the cap.
type DocumentListOpts struct {
	PinnedOnly bool
	Limit      int
}

// ErrDocumentNotFound is returned by DocumentReader.GetDocumentByPath when no
// live document matches the requested path. Resource handlers map it to a
// not-found error response.
var ErrDocumentNotFound = sentinelError("mcp: document not found")

// ErrToolAlreadyRegistered is returned by Register when a tool with the
// same Name() is already installed.
var ErrToolAlreadyRegistered = sentinelError("mcp: tool already registered")

// ErrNotImplemented is returned by NoopServer methods. Production code
// uses New(opts) instead.
var ErrNotImplemented = sentinelError("mcp: not yet implemented")

// ErrServerShutdown is returned by Start when Shutdown has already been
// called on the server. The instance is in a terminal state; construct a
// fresh Server via New to listen again.
var ErrServerShutdown = sentinelError("mcp: server shut down")

type sentinelError string

func (e sentinelError) Error() string { return string(e) }

// NoopServer is the test-only placeholder MCP server. Returns
// ErrNotImplemented from Register and Start. Used by callers that need a
// Server-shaped value but no real transport.
type NoopServer struct{}

// Register implements Server. It returns ErrNotImplemented.
func (NoopServer) Register(_ Tool) error { return ErrNotImplemented }

// Start implements Server. It returns ErrNotImplemented.
func (NoopServer) Start(_ context.Context) error { return ErrNotImplemented }

// Shutdown implements Server. It is a no-op.
func (NoopServer) Shutdown(_ context.Context) error { return nil }
