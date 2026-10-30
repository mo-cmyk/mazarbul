package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// defaultToolInvokeTimeout backstops a tool invocation that never returns.
// It is generous on purpose: over a proxy the client's own (shorter) deadline
// fires first, so this only bites a direct/in-process caller holding the
// connection open with no shorter bound. 300s matches the recall drain budget
// (defaultWaitForIdleTimeout), the longest legitimate single-call wait in the
// codebase. Tools that own their time budget opt out via SelfTimingTool.
const defaultToolInvokeTimeout = 300 * time.Second

// adaptTool converts a Mazarbul Tool into the (mcpgo.Tool, ToolHandlerFunc)
// pair that mark3labs/mcp-go's AddTool consumes.
//
// The bridge passes the tool's JSON Schema through byte-for-byte via
// RawInputSchema; ownership of schema validity stays with the Tool. The
// handler attaches the configured SecretProvider to the ctx, validates
// arguments against the precompiled InputSchema, then dispatches to
// Tool.Invoke. Errors are routed per MCP convention: application
// failures become CallToolResult{IsError: true} content blocks carrying
// the typed code, not JSON-RPC errors, so the calling LLM can see the
// failure and self-correct. Pure JSON-RPC errors (-32600/-32601) stay
// reserved for envelope-level faults that mcp-go produces itself.
func adaptTool(t Tool, sp SecretProvider, v *schemaValidator, timeout time.Duration) (mcpgo.Tool, mcpserver.ToolHandlerFunc) {
	mcpTool := mcpgo.NewToolWithRawSchema(t.Name(), t.Description(), t.InputSchema())
	if out := t.OutputSchema(); len(out) > 0 {
		mcpTool.RawOutputSchema = out
	}

	if timeout <= 0 {
		timeout = defaultToolInvokeTimeout
	}
	// Decide the deadline policy once at registration, not per call: a tool
	// that owns its own time budget (waitForIdle, bulk ingest) opts out of
	// the backstop so the adapter never truncates legitimate long work.
	selfTiming := false
	if st, ok := t.(SelfTimingTool); ok && st.SelfTiming() {
		selfTiming = true
	}

	handler := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx = WithSecrets(ctx, sp)

		if !selfTiming {
			// Backstop a handler that never returns. asToolError maps the
			// resulting context.DeadlineExceeded to E_TIMEOUT so the caller
			// gets a clean, retryable failure instead of an open-ended hang.
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		args, err := marshalArguments(req.GetRawArguments())
		if err != nil {
			return toolErrorResult(NewValidationError(
				fmt.Sprintf("invalid arguments encoding: %v", err),
				nil,
			)), nil
		}

		if vErr := v.Validate(args); vErr != nil {
			return toolErrorResult(vErr), nil
		}

		resp, err := t.Invoke(ctx, args)
		if err != nil {
			return toolErrorResult(asToolError(err)), nil
		}

		return wrapResult(resp), nil
	}
	return mcpTool, handler
}

// toolErrorResult renders a *ToolError as a CallToolResult{IsError:true}
// carrying the failure as a TEXT content block only — never as
// StructuredContent.
//
// Error results MUST NOT be placed in StructuredContent: a tool's declared
// output schema describes its SUCCESS shape (often additionalProperties:false),
// and MCP clients that validate received structured content against that
// schema — Claude Desktop does — reject the {code,message,details} envelope,
// surfacing a useless "-32602: Structured content does not match the tool's
// output schema" (or a generic "Tool execution failed") instead of the real
// error. Mcp-go's own server-side validator already exempts IsError results,
// but a strict client (or an older server binary) does not, so we keep the
// payload out of StructuredContent entirely.
//
// The text is "[CODE] message" — the code is substring-parsed by the CLI's
// toolErrorCode and is visible verbatim to the calling LLM — with any
// structured details appended as JSON so callers keep the actionable detail
// (e.g. ambiguous-name candidates) without tripping output-schema validation.
func toolErrorResult(e *ToolError) *mcpgo.CallToolResult {
	if e == nil {
		return mcpgo.NewToolResultError("unknown error")
	}
	text := fmt.Sprintf("[%s] %s", e.Code, e.Message)
	if len(e.Details) > 0 {
		text += "\ndetails: " + string(e.Details)
	}
	return mcpgo.NewToolResultError(text)
}

// marshalArguments coerces mcp-go's any-typed Arguments back into the
// JSON byte form that Tool.Invoke expects. A nil payload becomes the
// canonical empty JSON object so tools can rely on a non-nil request.
func marshalArguments(raw any) (json.RawMessage, error) {
	if raw == nil {
		return json.RawMessage("{}"), nil
	}
	if msg, ok := raw.(json.RawMessage); ok {
		return msg, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal arguments: %w", err)
	}
	return b, nil
}

// wrapResult turns a Tool.Invoke response into a CallToolResult. JSON
// responses become StructuredContent with a JSON-text fallback for
// clients that don't read StructuredContent; non-JSON bytes degrade
// gracefully to a text content block.
func wrapResult(resp json.RawMessage) *mcpgo.CallToolResult {
	if len(resp) == 0 {
		return mcpgo.NewToolResultText("")
	}
	var structured any
	if err := json.Unmarshal(resp, &structured); err != nil {
		return mcpgo.NewToolResultText(string(resp))
	}
	return mcpgo.NewToolResultStructured(structured, string(resp))
}

// errNilTool is returned when Register is called with a nil Tool.
var errNilTool = errors.New("mcp: nil tool")
