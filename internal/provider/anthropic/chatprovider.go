package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// Compile-time assertion: ChatProvider must satisfy kernel.ChatProvider.
var _ kernel.ChatProvider = (*ChatProvider)(nil)

// ErrNoToolUse is returned (wrapped) by ChatProvider.ToolUse when the model
// produces stop_reason "end_turn" without a tool_use content block. Callers
// check with errors.Is; the wrapping error carries the actual stop_reason so
// logs give enough context without a separate field.
var ErrNoToolUse = errors.New("anthropic: model did not invoke a tool")

// ChatProvider adapts *Client to the kernel.ChatProvider interface. A
// separate adapter type is required because Client.Complete already carries
// a different signature (ctx, *CompleteRequest) than the kernel interface
// expects (ctx, kernel.CompleteRequest); Go does not permit method
// overloading on a single type.
//
// Construct via NewChatProvider. The zero value is not usable.
type ChatProvider struct {
	client *Client
}

// NewChatProvider wraps client in a ChatProvider that satisfies
// kernel.ChatProvider. Client must be non-nil and fully initialised via
// anthropic.New.
func NewChatProvider(client *Client) (*ChatProvider, error) {
	if client == nil {
		return nil, errors.New("anthropic: NewChatProvider requires a non-nil Client")
	}
	return &ChatProvider{client: client}, nil
}

// Complete satisfies kernel.ChatProvider. It translates the kernel-typed
// request into an anthropic.CompleteRequest, delegates to the underlying
// Client, and maps the response back to kernel types.
//
// A zero Temperature in req means "use provider default" and maps to a nil
// *float64 on the wire (omitempty). A non-zero Temperature is forwarded
// verbatim.
func (p *ChatProvider) Complete(ctx context.Context, req kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	anReq := &CompleteRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
	}
	if req.System != "" {
		anReq.System = []ContentBlock{TextBlock(req.System)}
	}
	anReq.Messages = kernelMessagesToAnthropic(req.Messages)
	if req.Temperature != 0 {
		t := req.Temperature
		anReq.Temperature = &t
	}

	resp, err := p.client.Complete(ctx, anReq)
	if err != nil {
		return kernel.CompleteResponse{}, err
	}

	// Concatenate all text blocks. Anthropic may return multiple text blocks
	// when extended thinking is active; we join them to produce a single
	// string for kernel consumers that expect prose.
	var sb strings.Builder
	for _, block := range resp.Content {
		if block.Type == ContentTypeText {
			sb.WriteString(block.Text)
		}
	}

	return kernel.CompleteResponse{
		Content:    sb.String(),
		StopReason: resp.StopReason,
		Usage: kernel.TokenUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}, nil
}

// ToolUse satisfies kernel.ChatProvider. It issues a native Anthropic
// tool-use call with tool_choice:{type:"any"} so the model is forced to
// invoke the supplied tool rather than responding with prose.
//
// The method reuses the underlying Client's retry loop and API-key cache,
// so transient HTTP failures and rate-limit responses are retried
// transparently.
//
// ErrNoToolUse is returned (wrapped) when the model produces stop_reason
// "end_turn" without a tool_use content block. This is the only sentinel
// error this method introduces; all other errors propagate as-is from the
// transport or Anthropic's error body.
func (p *ChatProvider) ToolUse(ctx context.Context, req kernel.ToolUseRequest) (kernel.ToolUseResponse, error) { //nolint:gocritic // hugeParam: value signature forced by kernel.ChatProvider interface.
	wire := toolUseWireReq{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Messages:  kernelMessagesToAnthropic(req.Messages),
		Tools: []toolWireTool{{
			Name:        req.Tool.Name,
			Description: req.Tool.Description,
			InputSchema: req.Tool.InputSchema,
		}},
		ToolChoice: toolWireChoice{Type: "any"},
	}
	if req.System != "" {
		wire.System = []ContentBlock{TextBlock(req.System)}
	}
	if req.Temperature != 0 {
		t := req.Temperature
		wire.Temperature = &t
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return kernel.ToolUseResponse{}, fmt.Errorf("anthropic: marshal tool-use request: %w", err)
	}

	apiKey, err := p.client.loadAPIKey(ctx)
	if err != nil {
		return kernel.ToolUseResponse{}, err
	}

	// Reuse the retry loop. The body already encodes tools + tool_choice so
	// runWithRetries POSTs it as-is to /v1/messages. decode() returns a
	// CompleteResponse whose Content slice now includes tool_use blocks now
	// that ContentBlock carries ID/Name/Input.
	resp, err := p.client.runWithRetries(ctx, body, apiKey)
	if err != nil {
		return kernel.ToolUseResponse{}, err
	}

	usage := kernel.TokenUsage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
	}

	// Locate the first tool_use block. With tool_choice:{type:"any"} and a
	// single tool definition there should be exactly one such block; taking
	// the first is safe and forward-compatible with multi-tool responses.
	for _, block := range resp.Content {
		if block.Type != ContentTypeToolUse {
			continue
		}
		return kernel.ToolUseResponse{
			ToolUse: &kernel.ToolInvocation{
				Name:  block.Name,
				Input: block.Input,
			},
			StopReason: resp.StopReason,
			Usage:      usage,
		}, nil
	}

	// The model stopped without invoking a tool. This is unexpected when
	// tool_choice is "any", but it can happen when the model is confused or
	// the prompt is malformed. Surface it as a typed sentinel so callers can
	// handle or retry.
	return kernel.ToolUseResponse{
		StopReason: resp.StopReason,
		Usage:      usage,
	}, fmt.Errorf("anthropic: tool-use call (stop_reason=%q): %w", resp.StopReason, ErrNoToolUse)
}

// kernelMessagesToAnthropic converts a kernel.Message slice to the
// Anthropic wire type. Each kernel message holds a plain string Content;
// the adapter wraps it in a single TextBlock to satisfy the Anthropic
// Messages API's array-of-blocks requirement.
func kernelMessagesToAnthropic(msgs []kernel.Message) []Message {
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = Message{
			Role:    Role(m.Role),
			Content: []ContentBlock{TextBlock(m.Content)},
		}
	}
	return out
}

// toolUseWireReq is the JSON body sent to /v1/messages for a tool-use call.
// It differs from wireRequest in that it carries the tools array and a
// tool_choice directive. Never exposed outside the package.
type toolUseWireReq struct {
	Model       string         `json:"model"`
	MaxTokens   int            `json:"max_tokens"`
	Messages    []Message      `json:"messages"`
	System      []ContentBlock `json:"system,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	Tools       []toolWireTool `json:"tools"`
	ToolChoice  toolWireChoice `json:"tool_choice"`
}

// toolWireTool is the Anthropic wire shape for a single tool definition.
// Anthropic uses "input_schema" (snake_case) on the wire; the kernel uses
// "inputSchema" (camelCase) in its Go types. The struct tag handles the
// mapping.
type toolWireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// toolWireChoice instructs Anthropic on how to select a tool.
//   - "any"  forces the model to invoke at least one of the supplied tools.
//   - "auto" lets the model decide whether to use a tool.
//   - "tool" pins a specific tool by name (requires an additional Name field).
//
// ChatProvider.ToolUse always sends "any" so callers get a guaranteed
// ToolInvocation or ErrNoToolUse rather than an ambiguous prose response.
type toolWireChoice struct {
	Type string `json:"type"`
}
