package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// sampleKernelMessages returns a single user message in kernel format.
func sampleKernelMessages() []kernel.Message {
	return []kernel.Message{{Role: "user", Content: "Who is Merry?"}}
}

// sampleToolSchema returns a minimal ToolSchema for test calls.
func sampleToolSchema() kernel.ToolSchema {
	return kernel.ToolSchema{
		Name:        "extract_entity",
		Description: "Extract a named entity from the provided text.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
	}
}

// newChatProviderForTest creates a ChatProvider backed by a fake server that
// routes /api/tags, /api/version, and /api/chat to the supplied handlers.
// Pass nil for versionHandler to serve a 404 (simulates Ollama < 0.4 or
// missing /api/version endpoint).
func newChatProviderForTest(t *testing.T, versionHandler, chatHandler http.HandlerFunc) (*ChatProvider, *fakeServer) {
	t.Helper()
	srv := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel, "llama3.1:8b"))
		case "/api/version":
			if versionHandler != nil {
				versionHandler(w, r)
			} else {
				http.NotFound(w, r)
			}
		case "/api/chat":
			if chatHandler != nil {
				chatHandler(w, r)
			} else {
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	})
	c, err := New(context.Background(), Options{
		BaseURL:    srv.srv.URL,
		ChatModel:  "llama3.1:8b",
		HTTPClient: fastClient(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p, err := NewChatProvider(c, 0)
	if err != nil {
		t.Fatalf("NewChatProvider: %v", err)
	}
	return p, srv
}

// versionJSON returns a canned /api/version response.
func versionJSON(version string) string {
	return `{"version":"` + version + `"}`
}

// toolCallChatJSON returns a canned /api/chat response with a tool_calls entry.
func toolCallChatJSON(t *testing.T, toolName string, args json.RawMessage) []byte {
	t.Helper()
	type toolFunction struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	type toolCall struct {
		Function toolFunction `json:"function"`
	}
	type message struct {
		Role      string     `json:"role"`
		Content   string     `json:"content"`
		ToolCalls []toolCall `json:"tool_calls"`
	}
	type resp struct {
		Model           string  `json:"model"`
		Message         message `json:"message"`
		Done            bool    `json:"done"`
		DoneReason      string  `json:"done_reason"`
		PromptEvalCount int     `json:"prompt_eval_count"`
		EvalCount       int     `json:"eval_count"`
	}
	r := resp{
		Model: "llama3.1:8b",
		Message: message{
			Role:    "assistant",
			Content: "",
			ToolCalls: []toolCall{{
				Function: toolFunction{Name: toolName, Arguments: args},
			}},
		},
		Done:            true,
		DoneReason:      "stop",
		PromptEvalCount: 80,
		EvalCount:       15,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal tool call response: %v", err)
	}
	return b
}

// --- Constructor ---

func TestNewChatProvider_NilClient(t *testing.T) {
	_, err := NewChatProvider(nil, 0)
	if err == nil {
		t.Fatal("NewChatProvider(nil) returned no error")
	}
}

// --- Complete ---

// TestChatProvider_Complete_KernelTypes verifies that ChatProvider.Complete
// correctly translates a kernel.CompleteRequest to Ollama wire format and
// maps the response back to kernel types.
func TestChatProvider_Complete_KernelTypes(t *testing.T) {
	p, _ := newChatProviderForTest(t, nil, func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var wire chatWireRequest
		if err := json.Unmarshal(buf, &wire); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if wire.Model != "llama3.1:8b" {
			t.Errorf("model = %q, want llama3.1:8b", wire.Model)
		}
		if len(wire.Messages) != 2 {
			t.Fatalf("messages len = %d, want 2 (system + user)", len(wire.Messages))
		}
		if wire.Messages[0].Role != string(RoleSystem) || wire.Messages[0].Content != "Be helpful." {
			t.Errorf("system message = %+v, want role=system content='Be helpful.'", wire.Messages[0])
		}
		if wire.Messages[1].Role != "user" || wire.Messages[1].Content != "Who is Merry?" {
			t.Errorf("user message = %+v", wire.Messages[1])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatJSON(t, "Merry is a person.", 50, 10))
	})

	resp, err := p.Complete(context.Background(), kernel.CompleteRequest{
		Model:     "llama3.1:8b",
		System:    "Be helpful.",
		Messages:  sampleKernelMessages(),
		MaxTokens: 128,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "Merry is a person." {
		t.Errorf("Content = %q, want 'Merry is a person.'", resp.Content)
	}
	if resp.Usage.InputTokens != 50 || resp.Usage.OutputTokens != 10 {
		t.Errorf("Usage = %+v, want 50/10", resp.Usage)
	}
}

// --- ToolUse: native path ---

// TestChatProvider_ToolUse_Native_HappyPath verifies that ToolUse sends a
// native Ollama tool-use request (tools array, no format:"json") when the
// daemon version is >= 0.4, and that the tool_calls response is mapped to
// kernel.ToolInvocation.
func TestChatProvider_ToolUse_Native_HappyPath(t *testing.T) {
	args := json.RawMessage(`{"name":"Merry"}`)

	p, _ := newChatProviderForTest(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, versionJSON("0.4.7"))
		},
		func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			// Verify the request carries both a tools array AND a
			// constrained-decoding format schema (the GBNF lever).
			var wireCheck struct {
				Tools  []json.RawMessage `json:"tools"`
				Format json.RawMessage   `json:"format"`
			}
			if err := json.Unmarshal(buf, &wireCheck); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(wireCheck.Tools) != 1 {
				t.Errorf("tools len = %d, want 1", len(wireCheck.Tools))
			}
			if len(wireCheck.Format) == 0 {
				t.Errorf("format absent on native path; expected schema for constrained decoding")
			}
			// Verify tool structure.
			var tool ollamaTool
			if err := json.Unmarshal(wireCheck.Tools[0], &tool); err != nil {
				t.Fatalf("unmarshal tool: %v", err)
			}
			if tool.Type != "function" {
				t.Errorf("tool.type = %q, want function", tool.Type)
			}
			if tool.Function.Name != "extract_entity" {
				t.Errorf("function.name = %q, want extract_entity", tool.Function.Name)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(toolCallChatJSON(t, "extract_entity", args))
		},
	)

	resp, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:     "llama3.1:8b",
		Messages:  sampleKernelMessages(),
		Tool:      sampleToolSchema(),
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("ToolUse: %v", err)
	}
	if resp.ToolUse == nil {
		t.Fatal("ToolUse is nil, want non-nil ToolInvocation")
	}
	if resp.ToolUse.Name != "extract_entity" {
		t.Errorf("ToolInvocation.Name = %q, want extract_entity", resp.ToolUse.Name)
	}
	var input map[string]string
	if err := json.Unmarshal(resp.ToolUse.Input, &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if input["name"] != "Merry" {
		t.Errorf("input[name] = %q, want Merry", input["name"])
	}
	if resp.Usage.InputTokens != 80 || resp.Usage.OutputTokens != 15 {
		t.Errorf("Usage = %+v, want 80/15", resp.Usage)
	}
}

// TestChatProvider_ToolUse_PinsKeepAliveAndThink pins the wire-shape
// invariants the extraction hot path depends on: every ToolUse call
// must carry keep_alive="30m" (so back-to-back extractions don't pay
// the model-load cost) and think=false (so thinking-capable models
// like qwen3:4b don't burn the entire token budget on internal
// reasoning before emitting the JSON answer). The native path is the
// production-default path on Ollama >= 0.4; the fallback path has its
// own pin below.
func TestChatProvider_ToolUse_PinsKeepAliveAndThink(t *testing.T) {
	t.Parallel()

	p, _ := newChatProviderForTest(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, versionJSON("0.24.0"))
		},
		func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			var wire struct {
				KeepAlive string `json:"keep_alive"`
				Think     *bool  `json:"think"`
			}
			if err := json.Unmarshal(buf, &wire); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if wire.KeepAlive != "30m" {
				t.Errorf("keep_alive = %q, want 30m (extraction hot path must pin model in VRAM)", wire.KeepAlive)
			}
			if wire.Think == nil {
				t.Error("think field absent; expected explicit false to disable qwen3-family reasoning mode")
			} else if *wire.Think {
				t.Error("think = true; expected false (reasoning burns the extraction token budget)")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(toolCallChatJSON(t, "extract_entity", json.RawMessage(`{"name":"x"}`)))
		},
	)

	_, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:     "qwen3:4b",
		Messages:  sampleKernelMessages(),
		Tool:      sampleToolSchema(),
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("ToolUse: %v", err)
	}
}

// TestChatProvider_ToolUse_Native_NoToolCall verifies that ErrNoToolCall is
// returned when the native path gets a response with no tool_calls.
func TestChatProvider_ToolUse_Native_NoToolCall(t *testing.T) {
	p, _ := newChatProviderForTest(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, versionJSON("0.5.0"))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			// Return a plain text response (no tool_calls).
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(chatJSON(t, "I cannot extract an entity.", 60, 8))
		},
	)

	resp, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:     "llama3.1:8b",
		Messages:  sampleKernelMessages(),
		Tool:      sampleToolSchema(),
		MaxTokens: 256,
	})
	if err == nil {
		t.Fatal("ToolUse returned no error, want ErrNoToolCall")
	}
	if !errors.Is(err, ErrNoToolCall) {
		t.Errorf("error = %v, want errors.Is(err, ErrNoToolCall) true", err)
	}
	if resp.ToolUse != nil {
		t.Errorf("ToolUse = %+v, want nil on no-tool-call path", resp.ToolUse)
	}
}

// --- ToolUse: JSON fallback path ---

// TestChatProvider_ToolUse_Fallback_HappyPath verifies that ToolUse sends
// a JSON-mode request (format:"json", no tools array) when the daemon version
// is < 0.4, and that a valid response is mapped to kernel.ToolInvocation.
func TestChatProvider_ToolUse_Fallback_HappyPath(t *testing.T) {
	validJSON := `{"name":"Merry"}`

	p, _ := newChatProviderForTest(t,
		func(w http.ResponseWriter, _ *http.Request) {
			// Daemon is 0.3.x - no native tool support.
			_, _ = io.WriteString(w, versionJSON("0.3.14"))
		},
		func(w http.ResponseWriter, r *http.Request) {
			buf, _ := io.ReadAll(r.Body)
			// Verify the request uses format:"json" and has no tools array.
			var wireCheck struct {
				Format string            `json:"format"`
				Tools  []json.RawMessage `json:"tools"`
			}
			if err := json.Unmarshal(buf, &wireCheck); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if wireCheck.Format != "json" {
				t.Errorf("format = %q, want json on fallback path", wireCheck.Format)
			}
			if len(wireCheck.Tools) != 0 {
				t.Errorf("tools = %v, want empty on fallback path", wireCheck.Tools)
			}
			// Verify system message contains the schema and tool description.
			var wireMsg struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(buf, &wireMsg); err != nil {
				t.Fatalf("unmarshal messages: %v", err)
			}
			if len(wireMsg.Messages) == 0 || wireMsg.Messages[0].Role != "system" {
				t.Fatalf("first message is not system: %+v", wireMsg.Messages)
			}
			sys := wireMsg.Messages[0].Content
			if !strings.Contains(sys, "extract_entity") {
				t.Errorf("system prompt missing tool name 'extract_entity': %q", sys)
			}
			if !strings.Contains(sys, "properties") {
				t.Errorf("system prompt missing schema 'properties': %q", sys)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(chatJSON(t, validJSON, 70, 12))
		},
	)

	resp, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:     "llama3.1:8b",
		Messages:  sampleKernelMessages(),
		Tool:      sampleToolSchema(),
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("ToolUse (fallback): %v", err)
	}
	if resp.ToolUse == nil {
		t.Fatal("ToolUse is nil, want non-nil ToolInvocation")
	}
	if resp.ToolUse.Name != "extract_entity" {
		t.Errorf("ToolInvocation.Name = %q, want extract_entity", resp.ToolUse.Name)
	}
	var input map[string]string
	if err := json.Unmarshal(resp.ToolUse.Input, &input); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if input["name"] != "Merry" {
		t.Errorf("input[name] = %q, want Merry", input["name"])
	}
}

// TestChatProvider_ToolUse_Fallback_SchemaValidationFailure verifies that
// ErrSchemaValidation is returned when the fallback path produces a response
// that does not conform to the tool's InputSchema. This is the primary safety
// net for the JSON-mode path.
func TestChatProvider_ToolUse_Fallback_SchemaValidationFailure(t *testing.T) {
	// The schema requires "name" (string) to be present; this response is
	// missing the required field.
	invalidJSON := `{"wrong_field": 42}`

	p, _ := newChatProviderForTest(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, versionJSON("0.3.0"))
		},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(chatJSON(t, invalidJSON, 55, 9))
		},
	)

	resp, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:     "llama3.1:8b",
		Messages:  sampleKernelMessages(),
		Tool:      sampleToolSchema(),
		MaxTokens: 256,
	})
	if err == nil {
		t.Fatal("ToolUse returned no error, want ErrSchemaValidation")
	}
	if !errors.Is(err, ErrSchemaValidation) {
		t.Errorf("error = %v, want errors.Is(err, ErrSchemaValidation) true", err)
	}
	// ToolUse in the response should be nil when validation failed.
	if resp.ToolUse != nil {
		t.Errorf("ToolUse = %+v, want nil on schema validation failure", resp.ToolUse)
	}
}

// TestChatProvider_ToolUse_FallbackOnVersionProbeFailure verifies that when
// the /api/version probe returns a 404 (old Ollama with no version endpoint),
// ToolUse transparently falls back to the JSON-mode path.
func TestChatProvider_ToolUse_FallbackOnVersionProbeFailure(t *testing.T) {
	validJSON := `{"name":"Sam"}`

	// versionHandler is nil: newChatProviderForTest serves 404 for /api/version.
	p, _ := newChatProviderForTest(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatJSON(t, validJSON, 40, 8))
	})

	resp, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:     "llama3.1:8b",
		Messages:  sampleKernelMessages(),
		Tool:      sampleToolSchema(),
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("ToolUse (fallback after 404 version probe): %v", err)
	}
	if resp.ToolUse == nil || resp.ToolUse.Name != "extract_entity" {
		t.Errorf("ToolUse = %+v, want extract_entity invocation", resp.ToolUse)
	}
}

// --- structured-output tool-use ---

// structuredChatJSON returns an /api/chat response whose message.content is the
// model's structured-output object (no tool_calls) — what a model emits when
// the schema drives the response format instead of a tools array.
func structuredChatJSON(t *testing.T, content string) []byte {
	t.Helper()
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	r := struct {
		Model           string  `json:"model"`
		Message         message `json:"message"`
		Done            bool    `json:"done"`
		DoneReason      string  `json:"done_reason"`
		PromptEvalCount int     `json:"prompt_eval_count"`
		EvalCount       int     `json:"eval_count"`
	}{
		Model:           "llama3.1:8b",
		Message:         message{Role: "assistant", Content: content},
		Done:            true,
		DoneReason:      "stop",
		PromptEvalCount: 120,
		EvalCount:       40,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal freeform response: %v", err)
	}
	return b
}

// TestChatProvider_ToolUse_Structured_SendsSchemaFormatNoTools is the anchor for
// the structured-output extraction path: when PreferStructuredOutput is set, the
// request must carry the tool schema as "format" (the response grammar) and NO
// "tools" array — the tools array is exactly what makes qwen2.5:7b emit an empty
// tool_call and return zero mentions. The provider returns message.content as the
// tool input. The flag must win even when native tool support is advertised.
func TestChatProvider_ToolUse_Structured_SendsSchemaFormatNoTools(t *testing.T) {
	var sawTools bool
	var format json.RawMessage
	var body []byte
	chat := func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(body, &m)
		format = m["format"]
		_, sawTools = m["tools"]
		_, _ = w.Write(structuredChatJSON(t, `{"mentions":[],"relations":[]}`))
	}
	// Claim native tool support to prove PreferStructuredOutput overrides it
	// (and that no /api/version probe is required to reach this path).
	version := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(versionJSON("0.5.0"))) }
	p, _ := newChatProviderForTest(t, version, chat)

	schema := json.RawMessage(`{"type":"object","properties":{"mentions":{"type":"array"}}}`)
	resp, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:                  "qwen2.5:7b",
		System:                 "Extract entities.",
		Messages:               []kernel.Message{{Role: "user", Content: "Frodo lives in the Shire."}},
		Tool:                   kernel.ToolSchema{Name: "extract", Description: "d", InputSchema: schema},
		MaxTokens:              256,
		PreferStructuredOutput: true,
	})
	if err != nil {
		t.Fatalf("ToolUse: %v", err)
	}
	if sawTools {
		t.Errorf("structured-output request must NOT send a \"tools\" array; body=%s", body)
	}
	if strings.TrimSpace(string(format)) != strings.TrimSpace(string(schema)) {
		t.Errorf("structured-output request must send the schema as \"format\"; got %s", format)
	}
	if resp.ToolUse == nil {
		t.Fatal("expected a ToolUse invocation, got nil")
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(resp.ToolUse.Input, &got); err != nil {
		t.Fatalf("tool input is not a JSON object: %v (input=%q)", err, resp.ToolUse.Input)
	}
	if _, ok := got["mentions"]; !ok {
		t.Errorf("tool input missing expected key; input=%q", resp.ToolUse.Input)
	}
}

// TestChatProvider_ToolUse_Structured_EmptyContentIsNoToolCall pins the
// degenerate case: empty message.content (the model produced nothing) maps to
// ErrNoToolCall rather than a bogus empty invocation the parser would treat as
// "valid, zero mentions".
func TestChatProvider_ToolUse_Structured_EmptyContentIsNoToolCall(t *testing.T) {
	chat := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(structuredChatJSON(t, "   "))
	}
	p, _ := newChatProviderForTest(t, nil, chat)

	resp, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:                  "qwen2.5:7b",
		Messages:               []kernel.Message{{Role: "user", Content: "x"}},
		Tool:                   kernel.ToolSchema{Name: "extract", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)},
		MaxTokens:              256,
		PreferStructuredOutput: true,
	})
	if !errors.Is(err, ErrNoToolCall) {
		t.Fatalf("expected ErrNoToolCall on empty content, got %v", err)
	}
	if resp.ToolUse != nil {
		t.Errorf("expected nil ToolUse on empty content, got %+v", resp.ToolUse)
	}
}

func TestParseVersionSupportsToolUse(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"0.4.0", true},
		{"0.4.7", true},
		{"0.5.0", true},
		{"1.0.0", true},
		{"0.3.14", false},
		{"0.3.0", false},
		{"0.0.1", false},
		// Edge cases.
		{"", false},
		{"not-a-version", false},
		{"0.4", true}, // only major.minor
		{"1", false},  // only major
	}
	for _, tc := range cases {
		got := parseVersionSupportsToolUse(tc.version)
		if got != tc.want {
			t.Errorf("parseVersionSupportsToolUse(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}
