package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/correlation"
)

// newLoggingHooks builds the mcp-go Hooks struct that emits one
// structured log line per tool invocation. Every line carries the
// canonical fields: tool, request_id, duration_ms, status.
//
// Status values:
//
//	"ok"    Tool returned a successful CallToolResult (IsError false).
//	"error" Tool returned an IsError result OR the protocol layer
//	        surfaced a transport-level error via OnError.
//
// request_id resolution order: correlation.FromContext(ctx) first
// (matches the project-wide convention in internal/correlation), then
// the JSON-RPC id mcp-go received on the wire.
//
// State is owned by a per-server loggingHooks struct so there are no
// package-level globals; LoadAndDelete on every terminal hook keeps the
// start-time map bounded.
// The composer, when non-nil, recomputes the `initialize` instructions per
// session (Channel A, #214) and is registered as an OnAfterInitialize hook below.
func newLoggingHooks(logger *zap.Logger, composer func(context.Context) (string, error)) *mcpserver.Hooks {
	h := &loggingHooks{logger: logger}
	hooks := &mcpserver.Hooks{}
	hooks.AddBeforeCallTool(h.before)
	hooks.AddAfterCallTool(h.after)
	hooks.AddOnError(h.onError)
	if composer != nil {
		hooks.AddAfterInitialize(afterInitializeInstructions(logger, composer))
	}
	return hooks
}

// afterInitializeInstructions returns an OnAfterInitialize hook that overwrites
// result.Instructions with the per-session composed value. The mcp-go runtime
// builds the initialize result (copying the static WithInstructions base into
// result.Instructions) and THEN calls this hook with a pointer to that result,
// so the mutation here wins for the connection — every NEW session re-reads
// current state via the composer (#214). The hook cannot return an error, so it
// degrades cleanly to the static base on both non-success paths, never leaving an
// empty or errored instructions field:
//   - composition error: logged at Warn (with the session ID) and left as base.
//   - empty result: left as base SILENTLY — "no active preset" is the normal
//     case for most connections, so logging it per-connection would be noise.
func afterInitializeInstructions(
	logger *zap.Logger,
	composer func(context.Context) (string, error),
) func(context.Context, any, *mcpgo.InitializeRequest, *mcpgo.InitializeResult) {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(ctx context.Context, _ any, _ *mcpgo.InitializeRequest, result *mcpgo.InitializeResult) {
		instr, err := composer(ctx)
		if err != nil {
			// Tag the session so concurrent clients failing the composer are
			// distinguishable — the same correlation the tool logs carry via
			// invocationKey. The session is established by the time this hook runs.
			sessionID := ""
			if sess := mcpserver.ClientSessionFromContext(ctx); sess != nil {
				sessionID = sess.SessionID()
			}
			logger.Warn("compose server instructions; using static base",
				zap.String("session_id", sessionID),
				zap.Error(err),
			)
			return
		}
		if strings.TrimSpace(instr) == "" {
			return
		}
		// Defensive nil-guard: mcp-go always passes a non-nil result in production,
		// but the two early-return paths above are already nil-safe, so guarding the
		// success path keeps the hook uniformly callable with a nil result (tests,
		// future integrations) rather than panicking.
		if result == nil {
			return
		}
		result.Instructions = instr
	}
}

// loggingHooks holds the per-server state that pairs BeforeCallTool with
// its terminal hook. Mcp-go's hook API does not pass a derived context,
// so a sync.Map keyed on the JSON-RPC id is the cleanest available
// bridge between the two halves of the lifecycle.
type loggingHooks struct {
	logger *zap.Logger
	starts sync.Map // map[string]time.Time
}

func (h *loggingHooks) before(ctx context.Context, id any, _ *mcpgo.CallToolRequest) {
	h.starts.Store(invocationKey(ctx, id), time.Now())
}

func (h *loggingHooks) after(ctx context.Context, id any, req *mcpgo.CallToolRequest, result any) {
	status := "ok"
	if r, ok := result.(*mcpgo.CallToolResult); ok && r != nil && r.IsError {
		status = "error"
	}
	h.emit(ctx, id, req, status, nil)
}

func (h *loggingHooks) onError(ctx context.Context, id any, method mcpgo.MCPMethod, message any, err error) {
	if method != mcpgo.MethodToolsCall {
		return
	}
	var req *mcpgo.CallToolRequest
	if r, ok := message.(*mcpgo.CallToolRequest); ok {
		req = r
	}
	h.emit(ctx, id, req, "error", err)
}

func (h *loggingHooks) emit(ctx context.Context, id any, req *mcpgo.CallToolRequest, status string, err error) {
	dur := h.loadDuration(ctx, id)
	toolName := ""
	if req != nil {
		toolName = req.Params.Name
	}

	log := h.logger
	if log == nil {
		log = zap.NewNop()
	}
	fields := []zap.Field{
		zap.String("tool", toolName),
		zap.String("request_id", resolveRequestID(ctx, id)),
		zap.Duration("duration_ms", dur),
		zap.String("status", status),
	}
	if err != nil {
		fields = append(fields, zap.Error(err))
		log.Warn("Tool invocation failed", fields...)
		return
	}
	log.Info("Tool invocation", fields...)
}

func (h *loggingHooks) loadDuration(ctx context.Context, id any) time.Duration {
	v, ok := h.starts.LoadAndDelete(invocationKey(ctx, id))
	if !ok {
		return 0
	}
	start, ok := v.(time.Time)
	if !ok {
		return 0
	}
	return time.Since(start)
}

// resolveRequestID prefers the per-context correlation ID (set by upstream
// transports that already participate in correlation propagation) over
// mcp-go's JSON-RPC id, falling back to the latter when no correlation
// is attached.
func resolveRequestID(ctx context.Context, id any) string {
	if cid := correlation.FromContext(ctx); cid != "" {
		return cid
	}
	return toolCallKey(id)
}

// invocationKey composes a per-call sync.Map key that uniquely identifies
// an in-flight tool invocation across concurrent client sessions. JSON-RPC
// ids are client-chosen (typically 1, 2, 3...) And scoped to a single
// session, so two clients can both hold id=1 simultaneously. Keying only
// on the id lets the second client's `before` overwrite the first's start
// time and the terminal hook would then log duration_ms=0 or a duration
// from the wrong call. The mcp-go session ID, when available via
// ClientSessionFromContext, disambiguates.
func invocationKey(ctx context.Context, id any) string {
	sessionID := ""
	if sess := mcpserver.ClientSessionFromContext(ctx); sess != nil {
		sessionID = sess.SessionID()
	}
	return sessionID + ":" + toolCallKey(id)
}

// toolCallKey normalizes mcp-go's any-typed JSON-RPC id into a string
// suitable as both a map key and a log field.
func toolCallKey(id any) string {
	if id == nil {
		return ""
	}
	return fmt.Sprintf("%v", id)
}
