package anthropic

import (
	"encoding/json"
	"time"
)

// Role identifies the speaker of a Message. Anthropic's vocabulary; open
// enum to keep room for future roles.
type Role string

// Known roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ContentType identifies the shape of a ContentBlock. M1 ships with text;
// image and tool-use blocks land in later milestones.
type ContentType string

// Known content types.
const (
	ContentTypeText    ContentType = "text"
	ContentTypeToolUse ContentType = "tool_use"
)

// Message is one turn in a chat completion request. The Anthropic Messages
// API requires content to be an array of blocks (not a bare string), even
// when the message holds a single piece of text.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one block of message content. Text and tool_use blocks
// are supported; image and tool_result blocks land when the composer needs them.
//
// When Type == ContentTypeToolUse, ID, Name, and Input carry the tool
// invocation data; Text and CacheControl are empty. When Type ==
// ContentTypeText, only Type, Text, and CacheControl are meaningful.
type ContentBlock struct {
	Type         ContentType   `json:"type"`
	Text         string        `json:"text,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
	// Tool-use fields: non-empty only when Type == ContentTypeToolUse.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// TextBlock is a convenience constructor for a plain text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: ContentTypeText, Text: text}
}

// CacheControl marks a content block as a prompt-cache breakpoint. Anthropic
// charges a higher rate to write the cache entry and a discounted rate to
// read it; see agent-docs/designs/chat-provider.md for placement guidance.
type CacheControl struct {
	// Type is "ephemeral" today. Pinned exactly as Anthropic specifies.
	Type string `json:"type"`
}

// EphemeralCache returns a CacheControl marker for the default ephemeral
// (5-minute) cache. Callers attach it to a ContentBlock to opt that block
// into prompt caching.
func EphemeralCache() *CacheControl {
	return &CacheControl{Type: "ephemeral"}
}

// CompleteRequest is the input to Client.Complete. It is the Mazarbul-side
// shape; the client translates it into Anthropic's wire JSON.
type CompleteRequest struct {
	// Model selects the Anthropic model. Required.
	Model string

	// System is the optional system prompt, expressed as an array of content
	// blocks so callers can attach CacheControl breakpoints.
	System []ContentBlock

	// Messages is the conversation so far. Required, non-empty.
	Messages []Message

	// MaxTokens caps the response length. Required by Anthropic and by this
	// client (positive).
	MaxTokens int

	// Temperature, when non-nil, sets sampling temperature. A pointer is
	// used so the zero value (no temperature override) is distinguishable
	// from an explicit 0.0.
	Temperature *float64

	// StopSequences are optional strings that, when encountered, stop the
	// generation.
	StopSequences []string

	// Metadata is the optional Anthropic metadata object. Currently only
	// "user_id" is meaningful upstream; future keys pass through verbatim.
	Metadata map[string]string
}

// CompleteResponse is the parsed Messages API response. CreateTime is
// synthesized client-side at receipt; every other field is decoded from
// Anthropic's JSON.
type CompleteResponse struct {
	// ID is the Anthropic-side message identifier.
	ID string `json:"id"`

	// Role is always RoleAssistant for completion responses.
	Role Role `json:"role"`

	// Model echoes the model that produced the response.
	Model string `json:"model"`

	// Content is the list of content blocks the model emitted.
	Content []ContentBlock `json:"content"`

	// StopReason indicates why generation stopped: "end_turn", "max_tokens",
	// "stop_sequence", "tool_use", "pause_turn", "refusal".
	StopReason string `json:"stop_reason"`

	// Usage is the token accounting Anthropic returned.
	Usage Usage `json:"usage"`

	// CreateTime is the client-side receive time in UTC. Synthesized after
	// successful decode; not present on the wire.
	CreateTime time.Time `json:"-"`
}

// Usage mirrors Anthropic's usage object, including the cache token fields
// the prompt-caching feature populates.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// wireRequest is the JSON shape Anthropic expects. Built from a
// CompleteRequest in Client.Complete and never exposed to callers.
type wireRequest struct {
	Model         string            `json:"model"`
	MaxTokens     int               `json:"max_tokens"`
	Messages      []Message         `json:"messages"`
	System        []ContentBlock    `json:"system,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}
