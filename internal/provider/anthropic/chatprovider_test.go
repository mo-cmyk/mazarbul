package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// newTestChatProvider builds a ChatProvider backed by a test server.
// It reuses the newTestClient helper from client_test.go so all the
// same overrides (instantSleep, deterministic rng, fixed now) apply.
func newTestChatProvider(t *testing.T, ts *httptest.Server) *ChatProvider {
	t.Helper()
	c := newTestClient(t, ts, nil)
	p, err := NewChatProvider(c)
	if err != nil {
		t.Fatalf("NewChatProvider: %v", err)
	}
	return p
}

// sampleToolSchema returns a minimal ToolSchema for test calls.
func sampleToolSchema() kernel.ToolSchema {
	return kernel.ToolSchema{
		Name:        "extract_entity",
		Description: "Extract a named entity from the provided text.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
	}
}

// sampleKernelMessages returns a single user message in kernel format.
func sampleKernelMessages() []kernel.Message {
	return []kernel.Message{{Role: "user", Content: "Who is Merry?"}}
}

// toolUseSuccessResponse is a 200 body with a tool_use content block.
const toolUseSuccessResponse = `{
  "id": "msg_tool_001",
  "type": "message",
  "role": "assistant",
  "model": "claude-opus-4-5",
  "content": [
    {
      "type": "tool_use",
      "id": "toolu_abc123",
      "name": "extract_entity",
      "input": {"name":"Merry","type":"person"}
    }
  ],
  "stop_reason": "tool_use",
  "usage": {
    "input_tokens": 100,
    "output_tokens": 20,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0
  }
}`

// TestNewChatProvider_NilClient verifies the nil-guard in NewChatProvider.
func TestNewChatProvider_NilClient(t *testing.T) {
	_, err := NewChatProvider(nil)
	if err == nil {
		t.Fatal("NewChatProvider(nil) returned no error")
	}
}

// TestChatProvider_Complete_KernelTypes verifies that ChatProvider.Complete
// correctly translates kernel.CompleteRequest into an Anthropic wire request
// and maps the response back to kernel.CompleteResponse.
func TestChatProvider_Complete_KernelTypes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the wire request shape.
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var wire wireRequest
		if err := json.Unmarshal(buf, &wire); err != nil {
			t.Fatalf("unmarshal body: %v\nbody: %s", err, buf)
		}
		if wire.Model != "claude-opus-4-5" {
			t.Errorf("model = %q, want claude-opus-4-5", wire.Model)
		}
		if wire.MaxTokens != 512 {
			t.Errorf("max_tokens = %d, want 512", wire.MaxTokens)
		}
		if len(wire.Messages) != 1 {
			t.Fatalf("messages len = %d, want 1", len(wire.Messages))
		}
		if wire.Messages[0].Role != RoleUser {
			t.Errorf("message role = %q, want user", wire.Messages[0].Role)
		}
		if len(wire.Messages[0].Content) != 1 || wire.Messages[0].Content[0].Text != "Who is Merry?" {
			t.Errorf("message content = %+v, want text block with 'Who is Merry?'", wire.Messages[0].Content)
		}
		// System prompt should be a single text block.
		if len(wire.System) != 1 || wire.System[0].Text != "You are helpful." {
			t.Errorf("system = %+v, want one text block 'You are helpful.'", wire.System)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, successResponse)
	}))
	t.Cleanup(ts.Close)

	p := newTestChatProvider(t, ts)
	resp, err := p.Complete(context.Background(), kernel.CompleteRequest{
		Model:     "claude-opus-4-5",
		System:    "You are helpful.",
		Messages:  sampleKernelMessages(),
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "hello" {
		t.Errorf("Content = %q, want %q", resp.Content, "hello")
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 7 {
		t.Errorf("Usage = %+v, want 42/7", resp.Usage)
	}
}

// TestChatProvider_Complete_EmptySystem verifies that an empty System string
// results in no system field on the wire (the omitempty tag does the work;
// this test makes the contract explicit).
func TestChatProvider_Complete_EmptySystem(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var wire wireRequest
		if err := json.Unmarshal(buf, &wire); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(wire.System) != 0 {
			t.Errorf("system = %+v, want empty (omitted)", wire.System)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, successResponse)
	}))
	t.Cleanup(ts.Close)

	p := newTestChatProvider(t, ts)
	_, err := p.Complete(context.Background(), kernel.CompleteRequest{
		Model:     "claude-opus-4-5",
		Messages:  sampleKernelMessages(),
		MaxTokens: 128,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// TestChatProvider_ToolUse_HappyPath verifies that ChatProvider.ToolUse
// sends a correctly shaped tool-use request and unmarshals the tool_use
// content block into kernel.ToolInvocation.
func TestChatProvider_ToolUse_HappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		// Decode using a local struct so we can inspect tools + tool_choice
		// without coupling to toolUseWireReq.
		var wire struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Tools     []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"input_schema"`
			} `json:"tools"`
			ToolChoice struct {
				Type string `json:"type"`
			} `json:"tool_choice"`
			System   []ContentBlock `json:"system"`
			Messages []Message      `json:"messages"`
		}
		if err := json.Unmarshal(buf, &wire); err != nil {
			t.Fatalf("unmarshal body: %v\nbody: %s", err, buf)
		}
		if wire.Model != "claude-opus-4-5" {
			t.Errorf("model = %q, want claude-opus-4-5", wire.Model)
		}
		if wire.MaxTokens != 256 {
			t.Errorf("max_tokens = %d, want 256", wire.MaxTokens)
		}
		if len(wire.Tools) != 1 {
			t.Fatalf("tools len = %d, want 1", len(wire.Tools))
		}
		if wire.Tools[0].Name != "extract_entity" {
			t.Errorf("tool name = %q, want extract_entity", wire.Tools[0].Name)
		}
		if !strings.Contains(wire.Tools[0].Description, "Extract") {
			t.Errorf("tool description missing 'Extract': %q", wire.Tools[0].Description)
		}
		// Anthropic expects "input_schema" (snake_case) on the wire.
		if wire.Tools[0].InputSchema == nil {
			t.Error("tool input_schema is nil")
		}
		if wire.ToolChoice.Type != "any" {
			t.Errorf("tool_choice.type = %q, want any", wire.ToolChoice.Type)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, toolUseSuccessResponse)
	}))
	t.Cleanup(ts.Close)

	p := newTestChatProvider(t, ts)
	resp, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:     "claude-opus-4-5",
		Messages:  sampleKernelMessages(),
		Tool:      sampleToolSchema(),
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("ToolUse: %v", err)
	}
	if resp.ToolUse == nil {
		t.Fatal("ToolUse.ToolUse is nil, want non-nil ToolInvocation")
	}
	if resp.ToolUse.Name != "extract_entity" {
		t.Errorf("ToolInvocation.Name = %q, want extract_entity", resp.ToolUse.Name)
	}
	// Verify the raw JSON input round-trips.
	var input map[string]string
	if err := json.Unmarshal(resp.ToolUse.Input, &input); err != nil {
		t.Fatalf("unmarshal ToolInvocation.Input: %v", err)
	}
	if input["name"] != "Merry" {
		t.Errorf("input[name] = %q, want Merry", input["name"])
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 20 {
		t.Errorf("Usage = %+v, want 100/20", resp.Usage)
	}
}

// TestChatProvider_ToolUse_NoToolCall verifies that ErrNoToolUse is returned
// (and is errors.Is-detectable) when the model responds with end_turn and
// a text content block rather than a tool_use block. This exercises the
// error sentinel path that callers are expected to handle.
func TestChatProvider_ToolUse_NoToolCall(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return the standard text-only response (stop_reason="end_turn").
		_, _ = io.WriteString(w, successResponse)
	}))
	t.Cleanup(ts.Close)

	p := newTestChatProvider(t, ts)
	resp, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:     "claude-opus-4-5",
		Messages:  sampleKernelMessages(),
		Tool:      sampleToolSchema(),
		MaxTokens: 256,
	})
	if err == nil {
		t.Fatal("ToolUse returned no error, want ErrNoToolUse")
	}
	if !errors.Is(err, ErrNoToolUse) {
		t.Errorf("error = %v, want errors.Is(err, ErrNoToolUse) true", err)
	}
	// ToolUse in the response should be nil when no tool was called.
	if resp.ToolUse != nil {
		t.Errorf("ToolUse = %+v, want nil on no-tool-call path", resp.ToolUse)
	}
	// StopReason should still be populated from the response.
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", resp.StopReason)
	}
}

// TestChatProvider_ToolUse_SystemPrompt verifies that a non-empty System
// field is sent as a single text block in the system array.
func TestChatProvider_ToolUse_SystemPrompt(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var wire struct {
			System []ContentBlock `json:"system"`
		}
		if err := json.Unmarshal(buf, &wire); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(wire.System) != 1 || wire.System[0].Text != "Be precise." {
			t.Errorf("system = %+v, want single block 'Be precise.'", wire.System)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, toolUseSuccessResponse)
	}))
	t.Cleanup(ts.Close)

	p := newTestChatProvider(t, ts)
	_, err := p.ToolUse(context.Background(), kernel.ToolUseRequest{
		Model:     "claude-opus-4-5",
		System:    "Be precise.",
		Messages:  sampleKernelMessages(),
		Tool:      sampleToolSchema(),
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("ToolUse: %v", err)
	}
}
