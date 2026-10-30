package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// resultText concatenates the text content blocks of a CallToolResult. Error
// results carry their failure here (never in StructuredContent — see
// toolErrorResult), so tests read the code/message from the text.
func resultText(res *mcpgo.CallToolResult) string {
	var sb strings.Builder
	for _, b := range res.Content {
		if tb, ok := b.(mcpgo.TextContent); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// blockingPlainTool blocks until its context is cancelled, then returns
// ctx.Err(). It deliberately does NOT implement SelfTimingTool, so the adapter
// always wraps it in the per-invocation deadline — letting a test prove the
// backstop fires and maps to E_TIMEOUT without a real workload.
type blockingPlainTool struct{}

func (blockingPlainTool) Name() string                  { return "blocking" }
func (blockingPlainTool) Description() string           { return "blocks until ctx is done" }
func (blockingPlainTool) InputSchema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (blockingPlainTool) OutputSchema() json.RawMessage { return nil }
func (blockingPlainTool) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// deadlineProbe reports, as its result, whether its invocation context carried
// a deadline. With selfTiming=false it satisfies SelfTimingTool but opts in to
// the backstop (so a deadline must be present); with selfTiming=true it opts
// out (so no deadline must be present). This asserts the adapter's policy
// directly, without timing races.
type deadlineProbe struct{ selfTiming bool }

func (deadlineProbe) Name() string                  { return "deadlineProbe" }
func (deadlineProbe) Description() string           { return "reports ctx deadline presence" }
func (deadlineProbe) InputSchema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (deadlineProbe) OutputSchema() json.RawMessage { return nil }
func (deadlineProbe) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	_, has := ctx.Deadline()
	return json.Marshal(map[string]bool{"hasDeadline": has})
}
func (d deadlineProbe) SelfTiming() bool { return d.selfTiming }

// TestAdaptToolDeadlineMapsToTimeout verifies a wedged (non-self-timing)
// handler is cut off at the adapter's per-invocation deadline and surfaced as
// an E_TIMEOUT tool result rather than an open-ended hang.
func TestAdaptToolDeadlineMapsToTimeout(t *testing.T) {
	t.Parallel()

	_, handler := adaptTool(blockingPlainTool{}, NoopSecretProvider{}, &schemaValidator{}, 20*time.Millisecond)
	res, err := handler(context.Background(), callToolRequest("blocking", nil))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	// Error results carry the code in TEXT content, never StructuredContent —
	// putting the {code,message} envelope in StructuredContent makes strict
	// clients validate it against the success output schema and reject it.
	if res.StructuredContent != nil {
		t.Fatalf("error result has StructuredContent %#v, want nil", res.StructuredContent)
	}
	if text := resultText(res); !strings.Contains(text, string(CodeTimeout)) {
		t.Fatalf("error text = %q, want it to contain %q", text, CodeTimeout)
	}
}

// strictOutputTool returns a tool error on Invoke and declares a strict success
// output schema (additionalProperties:false). It reproduces the #4 condition:
// before the fix the error envelope was placed in StructuredContent and a
// strict client validated it against this schema, surfacing a useless
// "-32602: Structured content does not match the tool's output schema".
type strictOutputTool struct{ details json.RawMessage }

func (strictOutputTool) Name() string                 { return "strict" }
func (strictOutputTool) Description() string          { return "always errors; strict output schema" }
func (strictOutputTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (strictOutputTool) OutputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"status":{"type":"string"}},"required":["name","status"],"additionalProperties":false}`)
}

func (s strictOutputTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, NewValidationError(`"Erebor" matches 2 entities; pass the specific entities/{uid}`, s.details)
}

// TestAdaptToolErrorIsTextOnlyForStrictOutputSchema is the #4 regression: a tool
// with a strict (additionalProperties:false) success output schema that returns
// an error must produce a text-only IsError result — no StructuredContent — so
// no client can validate the error envelope against the success schema. The
// code and the message (including the actionable detail) stay readable in text.
func TestAdaptToolErrorIsTextOnlyForStrictOutputSchema(t *testing.T) {
	t.Parallel()

	tool := strictOutputTool{details: json.RawMessage(`{"reason":"AMBIGUOUS_NAME","candidates":["entities/A","entities/B"]}`)}
	_, handler := adaptTool(tool, NoopSecretProvider{}, &schemaValidator{}, time.Minute)
	res, err := handler(context.Background(), callToolRequest("strict", nil))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if res.StructuredContent != nil {
		t.Fatalf("error result has StructuredContent %#v, want nil (would trip strict output-schema validation)", res.StructuredContent)
	}
	text := resultText(res)
	if !strings.Contains(text, string(CodeValidation)) {
		t.Fatalf("error text = %q, want it to contain code %q", text, CodeValidation)
	}
	if !strings.Contains(text, "entities/{uid}") {
		t.Fatalf("error text = %q, want it to preserve the actionable message", text)
	}
	if !strings.Contains(text, "AMBIGUOUS_NAME") {
		t.Fatalf("error text = %q, want it to preserve structured details", text)
	}
}

// TestToolErrorResultTextOnly asserts the rendering invariant directly: a
// *ToolError becomes a text-only IsError result, with details appended as text.
func TestToolErrorResultTextOnly(t *testing.T) {
	t.Parallel()

	res := toolErrorResult(NewNotFoundError("entities/01ABC not found"))
	if !res.IsError || res.StructuredContent != nil {
		t.Fatalf("got IsError=%v StructuredContent=%#v, want IsError=true StructuredContent=nil", res.IsError, res.StructuredContent)
	}
	if text := resultText(res); !strings.Contains(text, string(CodeNotFound)) || !strings.Contains(text, "not found") {
		t.Fatalf("error text = %q, want code %q and message", text, CodeNotFound)
	}
}

// TestAdaptToolSelfTimingSkipsDeadline verifies the adapter applies its
// backstop deadline to ordinary tools but exempts SelfTimingTool implementers.
func TestAdaptToolSelfTimingSkipsDeadline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		selfTiming bool
		want       bool // expected hasDeadline
	}{
		{"default tool gets a deadline", false, true},
		{"self timing tool is exempt", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, handler := adaptTool(deadlineProbe{selfTiming: tc.selfTiming}, NoopSecretProvider{}, &schemaValidator{}, time.Minute)
			res, err := handler(context.Background(), callToolRequest("deadlineProbe", nil))
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			if res.IsError {
				t.Fatalf("unexpected error result: %+v", res)
			}
			structured, ok := res.StructuredContent.(map[string]any)
			if !ok {
				t.Fatalf("StructuredContent is %T, want map[string]any", res.StructuredContent)
			}
			if got, _ := structured["hasDeadline"].(bool); got != tc.want {
				t.Fatalf("hasDeadline = %v, want %v", got, tc.want)
			}
		})
	}
}
