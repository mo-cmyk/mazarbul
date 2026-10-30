package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// Compile-time assertion: ChatProvider must satisfy kernel.ChatProvider.
var _ kernel.ChatProvider = (*ChatProvider)(nil)

// ErrNoToolCall is returned (wrapped) by ChatProvider.ToolUse when the
// native Ollama tool-use path receives a response without any tool_calls.
// Callers check with errors.Is. The StopReason in the returned
// ToolUseResponse is still populated so callers can log context.
var ErrNoToolCall = errors.New("ollama: model did not produce a tool call")

// ErrSchemaValidation is returned (wrapped) by ChatProvider.ToolUse when the
// JSON-mode fallback path receives a response that fails validation against
// the tool's InputSchema. The wrapping error carries the validation detail.
// Callers check with errors.Is.
var ErrSchemaValidation = errors.New("ollama: tool response failed schema validation")

// boolPtr returns the address of b. The Ollama wire structs use *bool
// for tri-state fields (think, etc.) so omitempty can distinguish "use
// Ollama default" from "explicitly true/false". Each call site that
// wants false allocates via this helper rather than holding a package
// global (lint forbids unscoped globals).
func boolPtr(b bool) *bool { return &b }

// ChatProvider adapts *Client to the kernel.ChatProvider interface. A
// separate type is required because Client.Complete already carries a
// different signature (ctx, *CompleteRequest) than kernel.ChatProvider
// expects (ctx, kernel.CompleteRequest).
//
// ToolUse dispatches to the native Ollama tool-use path (Ollama >= 0.4) or
// falls back to a JSON-mode system-prompt approach for older daemons. The
// dispatch decision is made once per ChatProvider instance via a lazy
// /api/version probe.
//
// Construct via NewChatProvider. The zero value is not usable.
type ChatProvider struct {
	client *Client

	// probeOnce guards the lazy /api/version probe. nativeTools is set by
	// the probe and read only after probeOnce.Do completes.
	probeOnce   sync.Once
	nativeTools bool

	// chatSlots bounds the number of concurrent /api/chat requests in
	// flight to match the Ollama daemon's real parallelism
	// (OLLAMA_NUM_PARALLEL). Ollama serializes requests beyond its slot
	// count in an internal queue, and our per-request HTTP timeout
	// (Client.http.Timeout, 60s by default) starts ticking the moment we
	// send, queue time included. So a burst of concurrent extractions
	// against a single-slot daemon queues deep enough that the tail
	// requests blow the timeout and fail (observed: 6/8 extractions lost
	// under an 8-wide burst). Bounding our own in-flight count to the slot
	// count means every dispatched request gets a slot immediately — no
	// server-side queue, no timeout deaths. The channel is always non-nil
	// (capacity >= 1); acquire/release happens in acquireChatSlot.
	chatSlots chan struct{}
}

// defaultChatConcurrency is the in-flight /api/chat cap used when the caller
// does not supply a positive limit. Ollama's OLLAMA_NUM_PARALLEL defaults to
// a single inference slot when unset, so 1 is the safe assumption: it
// guarantees no request ever waits in Ollama's queue (and thus none can be
// killed by our HTTP timeout while merely waiting its turn).
const defaultChatConcurrency = 1

// NewChatProvider wraps client in a ChatProvider that satisfies
// kernel.ChatProvider. Client must be non-nil and fully initialised via
// ollama.New.
//
// MaxConcurrent bounds concurrent /api/chat requests; it should be set to the
// daemon's OLLAMA_NUM_PARALLEL. Values <= 0 fall back to
// defaultChatConcurrency (1). There is no unbounded mode: firing unbounded
// concurrent chat at a single-slot Ollama is exactly the burst-timeout bug
// this guards against.
func NewChatProvider(client *Client, maxConcurrent int) (*ChatProvider, error) {
	if client == nil {
		return nil, errors.New("ollama: NewChatProvider requires a non-nil Client")
	}
	if maxConcurrent <= 0 {
		maxConcurrent = defaultChatConcurrency
	}
	return &ChatProvider{
		client:    client,
		chatSlots: make(chan struct{}, maxConcurrent),
	}, nil
}

// MaxConcurrent reports the configured ceiling on concurrent /api/chat
// requests (the chat-slot capacity). Useful for startup logging so an
// operator can confirm the bound applied to the daemon.
func (p *ChatProvider) MaxConcurrent() int { return cap(p.chatSlots) }

// acquireChatSlot blocks until a chat slot is free or ctx is cancelled. It
// always returns a non-nil release func so the result is safe to defer
// unconditionally: on success the func returns the slot to the pool; on ctx
// cancellation it is a no-op and the wrapped ctx error is returned. The func
// must be called exactly once.
func (p *ChatProvider) acquireChatSlot(ctx context.Context) (func(), error) {
	select {
	case p.chatSlots <- struct{}{}:
		return func() { <-p.chatSlots }, nil
	case <-ctx.Done():
		return func() {}, fmt.Errorf("ollama: waiting for chat slot: %w", ctx.Err())
	}
}

// Complete satisfies kernel.ChatProvider. It translates the kernel-typed
// request into an ollama.CompleteRequest, delegates to the underlying
// Client.Complete, and maps the response back to kernel types.
func (p *ChatProvider) Complete(ctx context.Context, req kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	release, err := p.acquireChatSlot(ctx)
	if err != nil {
		return kernel.CompleteResponse{}, err
	}
	defer release()

	olReq := &CompleteRequest{
		Model:     req.Model,
		System:    req.System,
		MaxTokens: req.MaxTokens,
	}
	olReq.Messages = kernelMessagesToOllama(req.Messages)
	if req.Temperature != 0 {
		t := req.Temperature
		olReq.Temperature = &t
	}

	resp, err := p.client.Complete(ctx, olReq)
	if err != nil {
		return kernel.CompleteResponse{}, err
	}

	// Flatten the content blocks. Ollama returns a single text block for
	// chat completions; flattenContent is defensive against multi-block futures.
	content := flattenContent(resp.Content)

	return kernel.CompleteResponse{
		Content:    content,
		StopReason: resp.StopReason,
		Usage: kernel.TokenUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}, nil
}

// ToolUse satisfies kernel.ChatProvider. On first call it performs a lazy
// /api/version probe to determine whether the daemon supports native tool
// calling (Ollama >= 0.4). Subsequent calls reuse the cached result.
//
// Structured-output path (req.PreferStructuredOutput): POST /api/chat with
// the schema as the response format and NO tools array. Used for models whose
// native function-calling head returns empty tool_calls (e.g. qwen2.5:7b);
// they fill message.content correctly when the schema drives the response
// grammar instead. Needs no version probe.
//
// Native path: POST /api/chat with a tools array. Returns ErrNoToolCall
// (wrapped) when the response has no tool_calls.
//
// Fallback path: POST /api/chat with format:"json" and the schema embedded
// in the system prompt. Returns ErrSchemaValidation (wrapped) when the
// response is valid JSON but does not conform to the tool's InputSchema.
//
//nolint:gocritic // hugeParam: value signature forced by kernel.ChatProvider interface.
func (p *ChatProvider) ToolUse(ctx context.Context, req kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	// The structured-output path never sends a tools array, so it does not
	// depend on native-tool support and can skip the /api/version round trip.
	if !req.PreferStructuredOutput {
		p.probeOnce.Do(func() {
			p.nativeTools = p.probeNativeTools(ctx)
		})
	}

	// Acquire after the one-time version probe: the probe is a cheap
	// /api/version GET that does not consume an inference slot, so gating it
	// would only add false contention. The slot covers the /api/chat call.
	release, err := p.acquireChatSlot(ctx)
	if err != nil {
		return kernel.ToolUseResponse{}, err
	}
	defer release()

	if req.PreferStructuredOutput {
		return p.toolUseStructured(ctx, &req)
	}
	if p.nativeTools {
		return p.toolUseNative(ctx, &req)
	}
	return p.toolUseJSONFallback(ctx, &req)
}

// probeNativeTools calls GET /api/version and returns true when the daemon
// version is 0.4 or newer. Any error is treated as "no native support" so
// the fallback path is used transparently on probe failure.
func (p *ChatProvider) probeNativeTools(ctx context.Context) bool {
	var resp ollamaVersionResponse
	if err := p.client.doJSON(ctx, http.MethodGet, "/api/version", nil, &resp); err != nil {
		return false
	}
	return parseVersionSupportsToolUse(resp.Version)
}

// toolUseNative sends a native Ollama tool-use request (Ollama >= 0.4) and
// maps the first tool_call in the response to a kernel.ToolInvocation.
func (p *ChatProvider) toolUseNative(ctx context.Context, req *kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	model := req.Model
	if model == "" {
		model = p.client.chatModel
	}

	messages := buildMessages(req.System, req.Messages)
	wire := ollamaToolWireRequest{
		Model:    model,
		Stream:   false,
		Messages: messages,
		Tools: []ollamaTool{{
			Type: "function",
			Function: ollamaFunction{
				Name:        req.Tool.Name,
				Description: req.Tool.Description,
				Parameters:  req.Tool.InputSchema,
			},
		}},
		// Constrained decoding via the llama.cpp grammar sampler.  When the
		// model gives up on the tool-call path it must still emit a
		// schema-conforming message.content, so the tool-call-absent
		// fallback can parse content as JSON of the same schema.
		Format:    req.Tool.InputSchema,
		KeepAlive: "30m",
		Think:     boolPtr(false),
	}
	if req.MaxTokens > 0 || req.Temperature != 0 {
		opts := &chatWireOptions{NumPredict: req.MaxTokens}
		if req.Temperature != 0 {
			t := req.Temperature
			opts.Temperature = &t
		}
		wire.Options = opts
	}

	var resp ollamaToolWireResponse
	if err := p.client.doJSON(ctx, http.MethodPost, "/api/chat", wire, &resp); err != nil {
		return kernel.ToolUseResponse{}, fmt.Errorf("ollama: native tool-use: %w", err)
	}

	usage := kernel.TokenUsage{
		InputTokens:  resp.PromptEvalCount,
		OutputTokens: resp.EvalCount,
	}

	if len(resp.Message.ToolCalls) == 0 {
		// With format-constrained decoding, the model occasionally emits the
		// payload as schema-shaped JSON in message.content instead of a
		// tool_call wrapper. Accept it ONLY when it parses as JSON AND
		// validates against req.Tool.InputSchema, so the ToolUse interface
		// contract (schema-conformant input) holds on this branch too.
		if content := strings.TrimSpace(resp.Message.Content); content != "" {
			raw := json.RawMessage(content)
			var probe map[string]json.RawMessage
			if err := json.Unmarshal([]byte(content), &probe); err == nil {
				if verr := validateToolSchema(req.Tool.InputSchema, raw); verr != nil {
					return kernel.ToolUseResponse{
						StopReason: resp.DoneReason,
						Usage:      usage,
					}, fmt.Errorf("ollama: native tool-use fallback: %w: %w", ErrSchemaValidation, verr)
				}
				return kernel.ToolUseResponse{
					ToolUse: &kernel.ToolInvocation{
						Name:  req.Tool.Name,
						Input: raw,
					},
					StopReason: resp.DoneReason,
					Usage:      usage,
				}, nil
			}
		}
		return kernel.ToolUseResponse{
			StopReason: resp.DoneReason,
			Usage:      usage,
		}, fmt.Errorf("ollama: native tool-use (done_reason=%q): %w", resp.DoneReason, ErrNoToolCall)
	}

	tc := resp.Message.ToolCalls[0].Function
	return kernel.ToolUseResponse{
		ToolUse: &kernel.ToolInvocation{
			Name:  tc.Name,
			Input: tc.Arguments,
		},
		StopReason: resp.DoneReason,
		Usage:      usage,
	}, nil
}

// toolUseJSONFallback uses Ollama's JSON-mode (format:"json") to elicit a
// structured response. The tool schema and description are injected into the
// system prompt so the model knows what shape to produce. The raw response
// content is validated against req.Tool.InputSchema before being returned.
func (p *ChatProvider) toolUseJSONFallback(ctx context.Context, req *kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	model := req.Model
	if model == "" {
		model = p.client.chatModel
	}

	systemMsg := buildFallbackSystemPrompt(req.System, req.Tool)
	messages := make([]chatWireMessage, 0, len(req.Messages)+1)
	messages = append(messages, chatWireMessage{
		Role:    string(RoleSystem),
		Content: systemMsg,
	})
	for _, m := range req.Messages {
		messages = append(messages, chatWireMessage{
			Role:    m.Role,
			Content: m.Content,
		})
	}

	wire := jsonModeWireRequest{
		Model:     model,
		Stream:    false,
		Messages:  messages,
		Format:    "json",
		KeepAlive: "30m",
		Think:     boolPtr(false),
	}
	if req.MaxTokens > 0 || req.Temperature != 0 {
		opts := &chatWireOptions{NumPredict: req.MaxTokens}
		if req.Temperature != 0 {
			t := req.Temperature
			opts.Temperature = &t
		}
		wire.Options = opts
	}

	var resp chatWireResponse
	if err := p.client.doJSON(ctx, http.MethodPost, "/api/chat", wire, &resp); err != nil {
		return kernel.ToolUseResponse{}, fmt.Errorf("ollama: json-fallback tool-use: %w", err)
	}

	usage := kernel.TokenUsage{
		InputTokens:  resp.PromptEvalCount,
		OutputTokens: resp.EvalCount,
	}

	raw := json.RawMessage(resp.Message.Content)

	if err := validateToolSchema(req.Tool.InputSchema, raw); err != nil {
		return kernel.ToolUseResponse{
			StopReason: resp.DoneReason,
			Usage:      usage,
		}, fmt.Errorf("ollama: json-fallback tool-use: %w: %w", ErrSchemaValidation, err)
	}

	return kernel.ToolUseResponse{
		ToolUse: &kernel.ToolInvocation{
			Name:  req.Tool.Name,
			Input: raw,
		},
		StopReason: resp.DoneReason,
		Usage:      usage,
	}, nil
}

// toolUseStructured elicits the tool input via Ollama's structured-output mode:
// the tool's JSON Schema is sent as the response format (plumbed to the
// llama.cpp grammar sampler) and NO tools array is sent. It exists because some
// local models — qwen2.5:7b in particular — return an EMPTY tool_call when
// handed a tools array (the function-calling head emits a call with no
// arguments), so the native path yields zero mentions on otherwise-correct
// input. Given the identical schema as the response grammar instead, the same
// model fills message.content with a conforming object. The schema is also
// described in the system prompt (reusing buildFallbackSystemPrompt) so content
// guidance and the structural grammar agree.
//
// The content is schema-validated before it is returned, exactly as the
// json-fallback path does: the response grammar SHOULD already guarantee the
// shape, but if Ollama did not enforce it (older server, grammar miss) an
// off-schema body is surfaced as ErrSchemaValidation rather than handed to the
// tolerant parser, which would silently treat it as a valid zero-mention result.
func (p *ChatProvider) toolUseStructured(ctx context.Context, req *kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	model := req.Model
	if model == "" {
		model = p.client.chatModel
	}

	messages := make([]chatWireMessage, 0, len(req.Messages)+1)
	messages = append(messages, chatWireMessage{
		Role:    string(RoleSystem),
		Content: buildFallbackSystemPrompt(req.System, req.Tool),
	})
	for _, m := range req.Messages {
		messages = append(messages, chatWireMessage{Role: m.Role, Content: m.Content})
	}

	wire := ollamaToolWireRequest{
		Model:    model,
		Stream:   false,
		Messages: messages,
		// Schema as the response format, no Tools array: structured output
		// without the function-calling head that misfires on qwen2.5:7b.
		Format:    req.Tool.InputSchema,
		KeepAlive: "30m",
		Think:     boolPtr(false),
	}
	if req.MaxTokens > 0 || req.Temperature != 0 {
		opts := &chatWireOptions{NumPredict: req.MaxTokens}
		if req.Temperature != 0 {
			t := req.Temperature
			opts.Temperature = &t
		}
		wire.Options = opts
	}

	var resp ollamaToolWireResponse
	if err := p.client.doJSON(ctx, http.MethodPost, "/api/chat", wire, &resp); err != nil {
		return kernel.ToolUseResponse{}, fmt.Errorf("ollama: structured-output tool-use: %w", err)
	}

	usage := kernel.TokenUsage{
		InputTokens:  resp.PromptEvalCount,
		OutputTokens: resp.EvalCount,
	}

	content := strings.TrimSpace(resp.Message.Content)
	if content == "" {
		return kernel.ToolUseResponse{StopReason: resp.DoneReason, Usage: usage},
			fmt.Errorf("ollama: structured-output tool-use (done_reason=%q): %w", resp.DoneReason, ErrNoToolCall)
	}

	raw := json.RawMessage(content)
	if err := validateToolSchema(req.Tool.InputSchema, raw); err != nil {
		return kernel.ToolUseResponse{
			StopReason: resp.DoneReason,
			Usage:      usage,
		}, fmt.Errorf("ollama: structured-output tool-use: %w: %w", ErrSchemaValidation, err)
	}

	return kernel.ToolUseResponse{
		ToolUse: &kernel.ToolInvocation{
			Name:  req.Tool.Name,
			Input: raw,
		},
		StopReason: resp.DoneReason,
		Usage:      usage,
	}, nil
}

// buildFallbackSystemPrompt constructs the system prompt for JSON-mode
// fallback. It combines the caller's existing system text (when non-empty)
// with tool instructions and the JSON Schema so the model knows the exact
// shape to return.
func buildFallbackSystemPrompt(callerSystem string, tool kernel.ToolSchema) string {
	schemaStr := string(tool.InputSchema)

	var sb strings.Builder
	if callerSystem != "" {
		sb.WriteString(callerSystem)
		sb.WriteString("\n\n")
	}
	sb.WriteString("You are a function-calling assistant. Invoke the following tool by ")
	sb.WriteString("returning a JSON object that conforms to its schema exactly.\n")
	sb.WriteString("Return ONLY the JSON object. No explanation. No markdown. No code fences.\n\n")
	sb.WriteString("Tool name: ")
	sb.WriteString(tool.Name)
	sb.WriteString("\nTool description: ")
	sb.WriteString(tool.Description)
	sb.WriteString("\n\nJSON Schema:\n")
	sb.WriteString(schemaStr)
	return sb.String()
}

// validateToolSchema compiles schemaDoc as a JSON Schema document and
// validates data against it. Returns a descriptive error on validation
// failure; the error is NOT wrapped with ErrSchemaValidation here so the
// caller can choose the wrapping level.
func validateToolSchema(schemaDoc json.RawMessage, data json.RawMessage) error {
	var doc any
	if err := json.Unmarshal(schemaDoc, &doc); err != nil {
		return fmt.Errorf("parse tool schema: %w", err)
	}
	const resource = "mem://ollama/tool-schema.json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(resource, doc); err != nil {
		return fmt.Errorf("add tool schema: %w", err)
	}
	compiled, err := c.Compile(resource)
	if err != nil {
		return fmt.Errorf("compile tool schema: %w", err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("parse tool response as JSON: %w", err)
	}
	if err := compiled.Validate(decoded); err != nil {
		return fmt.Errorf("response does not match schema: %w", err)
	}
	return nil
}

// kernelMessagesToOllama converts kernel.Message slice to Ollama wire
// messages. Ollama's chatWireMessage carries a plain string Content so
// each kernel message becomes a single string.
func kernelMessagesToOllama(msgs []kernel.Message) []Message {
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = Message{
			Role:    Role(m.Role),
			Content: []ContentBlock{TextBlock(m.Content)},
		}
	}
	return out
}

// buildMessages prepends a system message when system is non-empty and
// converts the kernel messages to the chatWireMessage slice used by the
// native tool-use path.
func buildMessages(system string, msgs []kernel.Message) []chatWireMessage {
	out := make([]chatWireMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, chatWireMessage{
			Role:    string(RoleSystem),
			Content: system,
		})
	}
	for _, m := range msgs {
		out = append(out, chatWireMessage{
			Role:    m.Role,
			Content: m.Content,
		})
	}
	return out
}

// parseVersionSupportsToolUse parses an Ollama version string (e.g. "0.4.7")
// and returns true when major.minor >= 0.4. Any parse error returns false so
// the JSON fallback is used instead of crashing.
func parseVersionSupportsToolUse(version string) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return major > 0 || minor >= 4
}

// ollamaVersionResponse is the JSON shape of GET /api/version.
type ollamaVersionResponse struct {
	Version string `json:"version"`
}

// ollamaToolWireRequest is the /api/chat body for native tool-use calls
// (Ollama >= 0.4). It extends the standard chat request with a tools array.
// Format, when non-nil, is a JSON Schema document that Ollama plumbs down
// to the llama.cpp grammar-constrained sampler so the model literally
// cannot emit non-conforming JSON.  Three sub-7B failure modes (stringified
// arrays, single-quoted inner JSON, objects in place of arrays) disappear
// by construction once Format is set.
type ollamaToolWireRequest struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Messages []chatWireMessage `json:"messages"`
	Tools    []ollamaTool      `json:"tools,omitempty"`
	Format   json.RawMessage   `json:"format,omitempty"`
	Options  *chatWireOptions  `json:"options,omitempty"`
	// KeepAlive pins the model in VRAM between tool-use calls. Without
	// it, Ollama's default 5-minute eviction repeatedly reloads the
	// extractor model during a haystack ingest (every Episode → one
	// ToolUse), turning a ~1s warm extraction into a ~50s cold one.
	KeepAlive string `json:"keep_alive,omitempty"`
	// Think toggles Ollama's reasoning mode on thinking-capable models
	// (Qwen3, DeepSeek-R1, etc.). nil leaves the model default; pointer
	// to false disables thinking. The extractor sets this to false:
	// schema-constrained extraction is a structured-output task, not a
	// reasoning task, and thinking burns the entire output token budget
	// (qwen3:4b ate 1024 tokens of <think> before failing to produce
	// the actual JSON; with think:false it produces the same 6 entities
	// + 5 relations in 269 tokens). Ignored on non-thinking models.
	Think *bool `json:"think,omitempty"`
}

// ollamaTool is one entry in the tools array. Ollama requires Type to be
// "function"; other tool types are not supported as of 0.4.
type ollamaTool struct {
	Type     string         `json:"type"`
	Function ollamaFunction `json:"function"`
}

// ollamaFunction carries the tool definition inside an ollamaTool. Parameters
// is a raw JSON Schema document; Ollama passes it to the model without
// further interpretation.
type ollamaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ollamaToolWireResponse mirrors the /api/chat response shape when the model
// invokes a tool. Tool_calls is populated when the model chose to call a
// function; otherwise Content carries the prose response.
type ollamaToolWireResponse struct {
	Model   string `json:"model"`
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
		// ToolCalls is populated when the model invoked a function tool.
		ToolCalls []struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

// jsonModeWireRequest is the /api/chat body for the JSON-mode fallback path.
// Format:"json" instructs Ollama to constrain output to valid JSON; the tool
// schema is embedded in the system prompt rather than the tools array.
type jsonModeWireRequest struct {
	Model     string            `json:"model"`
	Stream    bool              `json:"stream"`
	Messages  []chatWireMessage `json:"messages"`
	Format    string            `json:"format"`
	Options   *chatWireOptions  `json:"options,omitempty"`
	KeepAlive string            `json:"keep_alive,omitempty"`
	// Think mirrors ollamaToolWireRequest.Think; see that field for
	// rationale. Set to false on the fallback path for the same reason
	// it's set on the native path.
	Think *bool `json:"think,omitempty"`
}
