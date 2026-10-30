package ollama

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Role identifies the speaker of a Message. Open enum so future Ollama roles
// (e.g., "tool") pass through unchanged when M3 lights up tool calling.
type Role string

// Known roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ContentType identifies the shape of a ContentBlock. M1 ships with text;
// tool-use and image blocks land in later milestones.
type ContentType string

// Known content types.
const (
	ContentTypeText ContentType = "text"
)

// Message is one turn in a chat completion request. Ollama's /api/chat wire
// format stores content as a single string per message; the Mazarbul-side
// shape carries a slice of ContentBlock for parity with the Anthropic
// provider. M1 supports only text blocks; non-text blocks make Complete
// return a validation error.
type Message struct {
	Role    Role
	Content []ContentBlock
}

// ContentBlock is one block of message content. M1 supports text blocks
// only; tool-use, tool-result, and image blocks land with M3 tool calling.
type ContentBlock struct {
	Type ContentType
	Text string
}

// TextBlock is a convenience constructor for a plain text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: ContentTypeText, Text: text}
}

// CompleteRequest is the input to Client.Complete. The client translates it
// into Ollama's /api/chat wire JSON; non-streaming only (stream:false is
// always sent).
type CompleteRequest struct {
	// Model selects the chat model. Required; an empty value uses the
	// chatModel configured at construction.
	Model string

	// System is the optional system prompt. When non-empty, the client
	// prepends a synthetic {role:"system", content:System} message to
	// Messages on the wire.
	System string

	// Messages is the conversation so far. Required, non-empty.
	Messages []Message

	// MaxTokens caps the response length. Required (positive); sent as
	// options.num_predict on the wire.
	MaxTokens int

	// Temperature, when non-nil, sets sampling temperature. A pointer is
	// used so the zero value (no override) is distinguishable from an
	// explicit 0.0. Sent as options.temperature on the wire.
	Temperature *float64

	// StopSequences are optional strings that, when encountered, stop the
	// generation. Sent as options.stop on the wire.
	StopSequences []string
}

// Usage mirrors Ollama's token accounting from the non-streaming chat
// response. InputTokens comes from prompt_eval_count, OutputTokens from
// eval_count. The wire response also carries nanosecond-scale duration
// fields (total_duration, load_duration, prompt_eval_duration,
// eval_duration); these are deferred to the M2 budget subsystem.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// CompleteResponse is the parsed /api/chat response. CreateTime is the
// client-side receive time in UTC; the wire created_at is discarded in
// favor of a consistent clock per the AGENTS.md UTC timestamp rule.
type CompleteResponse struct {
	// Model echoes the model that produced the response.
	Model string

	// Role is always RoleAssistant on a successful response.
	Role Role

	// Content is the assistant's reply, packaged as a single text block so
	// the caller does not have to special-case Ollama vs Anthropic.
	Content []ContentBlock

	// StopReason is Ollama's done_reason verbatim ("stop", "length",
	// "load", "unload", ...). Pass-through; no branching.
	StopReason string

	// Usage is the token accounting Ollama returned.
	Usage Usage

	// CreateTime is the client-side receive time in UTC. Synthesized after
	// successful decode; the wire created_at is intentionally ignored to
	// keep all Mazarbul-internal timestamps on the same clock.
	CreateTime time.Time
}

// chatWireMessage is the JSON shape Ollama expects for a single message.
// Content is a single string per the documented /api/chat schema.
type chatWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatWireOptions is the optional options object on /api/chat requests. Each
// field is omitempty so callers who do not set Temperature / StopSequences
// do not send Ollama defaults they did not ask for.
type chatWireOptions struct {
	NumPredict  int      `json:"num_predict,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

// isEmpty reports whether opt would serialize to the empty object {}. We
// omit the entire options field on the wire when this is true.
func (opt chatWireOptions) isEmpty() bool {
	return opt.NumPredict == 0 && opt.Temperature == nil && len(opt.Stop) == 0
}

// chatWireRequest is the full JSON body POSTed to /api/chat. Stream is
// always false; streaming is M2+. KeepAlive pins the model in VRAM
// between calls so back-to-back extraction (a haystack ingest fires
// dozens to hundreds of calls in close succession) doesn't pay the
// model-load cost on each call. Empty value defers to Ollama's default
// (5 min); the production hot path sets "30m" so eviction can't
// surprise an in-flight benchmark. Think disables Ollama's reasoning
// mode on thinking-capable models (Qwen3, DeepSeek-R1); the composer
// and resolver are structured-output paths, not reasoning paths, so
// thinking only burns tokens without improving the result.
type chatWireRequest struct {
	Model     string            `json:"model"`
	Stream    bool              `json:"stream"`
	Messages  []chatWireMessage `json:"messages"`
	Options   *chatWireOptions  `json:"options,omitempty"`
	KeepAlive string            `json:"keep_alive,omitempty"`
	Think     *bool             `json:"think,omitempty"`
}

// chatWireResponse mirrors the documented Ollama /api/chat response shape
// when stream:false. All duration fields are decoded but discarded; the
// budget subsystem (#82, M2) will surface them when needed.
type chatWireResponse struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

// Complete sends a chat request to /api/chat and returns the assistant's
// reply. Complete retries transport errors and HTTP 5xx twice with
// exponential backoff (200ms, 400ms; ±25% jitter), does not retry 4xx, and
// respects ctx cancellation between attempts. Total wall-clock budget per
// call is bounded by the HTTPClient's Timeout (60s by default) and the
// backoff schedule.
//
// Complete returns a validation error when the client was constructed with
// an empty Options.ChatModel (chat disabled), when req.Messages is empty,
// when req.MaxTokens is non-positive, or when any ContentBlock has a non-
// text Type. M1 supports text blocks only; tool and image blocks land in M3.
func (c *Client) Complete(ctx context.Context, req *CompleteRequest) (CompleteResponse, error) {
	if req == nil {
		return CompleteResponse{}, errors.New("ollama: CompleteRequest is required")
	}
	if c.chatModel == "" {
		return CompleteResponse{}, errors.New("ollama: chat not configured; pass Options.ChatModel to New")
	}
	if err := req.validate(); err != nil {
		return CompleteResponse{}, err
	}

	wire, err := c.toWireRequest(req)
	if err != nil {
		return CompleteResponse{}, err
	}

	var resp chatWireResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/chat", wire, &resp); err != nil {
		return CompleteResponse{}, err
	}

	return CompleteResponse{
		Model:      resp.Model,
		Role:       RoleAssistant,
		Content:    []ContentBlock{TextBlock(resp.Message.Content)},
		StopReason: resp.DoneReason,
		Usage: Usage{
			InputTokens:  resp.PromptEvalCount,
			OutputTokens: resp.EvalCount,
		},
		CreateTime: time.Now().UTC(),
	}, nil
}

// validate enforces the invariants Ollama itself enforces, plus M1-only
// constraints (text content blocks only).
func (r *CompleteRequest) validate() error {
	if len(r.Messages) == 0 {
		return errors.New("ollama: CompleteRequest.Messages must be non-empty")
	}
	if r.MaxTokens <= 0 {
		return errors.New("ollama: CompleteRequest.MaxTokens must be positive")
	}
	for i, msg := range r.Messages {
		if msg.Role == "" {
			return fmt.Errorf("ollama: CompleteRequest.Messages[%d].Role is required", i)
		}
		for j, blk := range msg.Content {
			if blk.Type != "" && blk.Type != ContentTypeText {
				return fmt.Errorf("ollama: CompleteRequest.Messages[%d].Content[%d].Type %q not supported (text only in M1)", i, j, blk.Type)
			}
		}
	}
	return nil
}

// toWireRequest builds the /api/chat JSON body. The chat model defaults to
// the client's configured model when req.Model is empty. System prompts are
// prepended as a synthetic system message; multiple text blocks per message
// are concatenated into a single content string per Ollama's wire shape.
func (c *Client) toWireRequest(req *CompleteRequest) (*chatWireRequest, error) {
	model := req.Model
	if model == "" {
		model = c.chatModel
	}

	messages := make([]chatWireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		messages = append(messages, chatWireMessage{
			Role:    string(RoleSystem),
			Content: req.System,
		})
	}
	for _, msg := range req.Messages {
		messages = append(messages, chatWireMessage{
			Role:    string(msg.Role),
			Content: flattenContent(msg.Content),
		})
	}

	out := &chatWireRequest{
		Model:    model,
		Stream:   false,
		Messages: messages,
		// keep_alive pins the model in VRAM for 30 minutes between
		// calls. Without this, Ollama's default 5-minute eviction —
		// combined with multiple models loaded for the same agent
		// (chat + embed) — causes the extractor model to get
		// repeatedly evicted and reloaded, paying multi-second load
		// cost on every back-to-back extraction call during a
		// haystack ingest. Empirically: a 2-token response on
		// qwen2.5:7b took 60s under eviction churn vs ~1s warm.
		KeepAlive: "30m",
		Think:     boolPtr(false),
	}

	opts := chatWireOptions{
		NumPredict:  req.MaxTokens,
		Temperature: req.Temperature,
		Stop:        req.StopSequences,
	}
	if !opts.isEmpty() {
		out.Options = &opts
	}
	return out, nil
}

// flattenContent concatenates the Text fields of every ContentBlock in
// order, separated by no delimiter (Ollama joins on the wire by treating
// the message content as one document). M1 callers typically pass a single
// TextBlock per message; the loop is defensive against future helpers that
// split a message across blocks.
func flattenContent(blocks []ContentBlock) string {
	switch len(blocks) {
	case 0:
		return ""
	case 1:
		return blocks[0].Text
	}
	total := 0
	for _, b := range blocks {
		total += len(b.Text)
	}
	buf := make([]byte, 0, total)
	for _, b := range blocks {
		buf = append(buf, b.Text...)
	}
	return string(buf)
}
