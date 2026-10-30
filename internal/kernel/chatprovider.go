package kernel

import (
	"context"
	"encoding/json"
)

// ChatProvider is the unified LLM abstraction Mazarbul's kernel uses for
// all eight LLM leverage points (extraction, composition, disambiguation,
// contradiction detection, entity summary, predicate canonicalisation,
// query rewriting, naming canonicalisation). Implementations route calls
// to Anthropic (tool-use), Ollama (JSON-mode), or a prose-fallback for
// providers that support neither structured-output mechanism natively.
//
// The interface is intentionally small: two methods cover the two call
// shapes the kernel needs. Complete is the free-form text-generation
// path; ToolUse is the structured-output path that returns a typed JSON
// object rather than prose.
//
// Implementations live in internal/provider/; this file defines only the
// contract. Wiring into daemon/wiring.go is the backend teammate's
// responsibility (Phase 0, parallel track).
type ChatProvider interface {
	// Complete issues a non-streaming chat completion and returns the
	// model's prose response. Used for composition, query rewriting, and
	// naming canonicalisation where the caller wants free-form text
	// rather than a structured JSON object.
	Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)

	// ToolUse issues a structured-output call and returns the tool
	// invocation the model selected. Implementations route to Anthropic
	// tool-use, Ollama JSON-mode, or a schema-as-prose fallback when
	// neither native mechanism is available. The InputSchema field on
	// ToolUseRequest carries a JSON Schema document that constrains the
	// shape of the returned Input field.
	ToolUse(ctx context.Context, req ToolUseRequest) (ToolUseResponse, error)
}

// Message is one turn in a chat conversation. Role is "user" or
// "assistant"; Content is the turn's text.
type Message struct {
	// Role identifies the speaker. Required. "user" for human turns;
	// "assistant" for model turns.
	Role string `json:"role"`

	// Content is the turn text. Required. Non-empty.
	Content string `json:"content"`
}

// TokenUsage records the token counts returned by the provider for a
// single call. Fields are zero when the provider does not report them.
type TokenUsage struct {
	// InputTokens is the number of tokens in the prompt sent to the
	// model.
	InputTokens int `json:"inputTokens"`

	// OutputTokens is the number of tokens in the model's response.
	OutputTokens int `json:"outputTokens"`
}

// CompleteRequest is the input to ChatProvider.Complete. It mirrors the
// shape of internal/provider/anthropic/types.go:CompleteRequest but is
// expressed in kernel-native types so the kernel package does not import
// the provider packages.
type CompleteRequest struct {
	// Model selects the provider model, e.g. "claude-opus-4-5" or
	// "llama3.1:8b". Required.
	Model string

	// System is the optional system prompt. Empty string omits it.
	System string

	// Messages is the conversation history. Required. Non-empty.
	Messages []Message

	// MaxTokens caps the response length. Required. Positive.
	MaxTokens int

	// Temperature, when non-zero, sets the sampling temperature. A
	// zero value means "use provider default".
	Temperature float64
}

// CompleteResponse is the output of ChatProvider.Complete.
type CompleteResponse struct {
	// Content is the model's prose response. Non-empty on success.
	Content string `json:"content"`

	// Usage records the token counts for this call.
	Usage TokenUsage `json:"usage"`

	// StopReason indicates why generation stopped, e.g. "end_turn" or
	// "max_tokens". Provider-specific values pass through verbatim.
	StopReason string `json:"stopReason"`
}

// ToolSchema describes the single tool the model may invoke in a ToolUse
// call. Mazarbul always passes exactly one tool per ToolUse call; the
// model is expected to invoke it (i.e. there is no "I don't want to use
// a tool" path in the structured-output flow).
type ToolSchema struct {
	// Name is the tool identifier the model uses when invoking it.
	// Required. Non-empty.
	Name string `json:"name"`

	// Description is a natural-language explanation of what the tool
	// does and what shape of input it expects. Required. Non-empty.
	// Good descriptions significantly improve structured-output quality
	// on both Anthropic and Ollama.
	Description string `json:"description"`

	// InputSchema is a JSON Schema document (Draft 2020-12) that
	// constrains the shape of the JSON object the model must return as
	// the tool's input. Required. The extractor, resolver, and composer
	// each pass their respective output schemas here.
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolInvocation is the model's structured response to a ToolUse call.
// Name echoes the tool the model chose; Input is the JSON object
// validated against ToolSchema.InputSchema.
type ToolInvocation struct {
	// Name is the tool the model invoked. Matches ToolSchema.Name when
	// the provider correctly constrains the response.
	Name string `json:"name"`

	// Input is the raw JSON object the model produced as the tool
	// input. Callers unmarshal it into the concrete type they expect;
	// the kernel does not decode it further.
	Input json.RawMessage `json:"input"`
}

// ToolUseRequest is the input to ChatProvider.ToolUse. It carries the
// conversation context, the tool definition, and generation parameters.
type ToolUseRequest struct {
	// Model selects the provider model. Required.
	Model string

	// System is the optional system prompt. Empty string omits it.
	System string

	// Messages is the conversation history. Required. Non-empty.
	Messages []Message

	// Tool describes the single tool the model should invoke. Required.
	Tool ToolSchema

	// MaxTokens caps the response length. Required. Positive.
	MaxTokens int

	// Temperature, when non-zero, sets the sampling temperature.
	Temperature float64

	// PreferStructuredOutput asks the provider to elicit the tool input via
	// structured-output decoding (the schema sent as a response grammar, with
	// no tools array) instead of native function-calling. Some local models
	// (notably qwen2.5:7b on Ollama) emit an EMPTY tool_call when handed a tools
	// array — the function-calling head produces no arguments — yet fill
	// message.content correctly when the same schema is supplied as the response
	// format. Callers that drive such a model set this so extraction does not
	// silently return zero mentions. Providers with reliable native tool-use
	// (e.g. a hosted API) may ignore it.
	PreferStructuredOutput bool
}

// ToolUseResponse is the output of ChatProvider.ToolUse.
type ToolUseResponse struct {
	// ToolUse is the structured invocation the model produced. Nil
	// when the model did not invoke the tool (e.g. stop reason is
	// "end_turn" without a tool call); callers must handle this case.
	ToolUse *ToolInvocation `json:"toolUse,omitempty"`

	// Usage records the token counts for this call.
	Usage TokenUsage `json:"usage"`

	// StopReason indicates why generation stopped. "tool_use" is the
	// expected value when ToolUse is non-nil.
	StopReason string `json:"stopReason"`
}
