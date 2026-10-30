package rewriter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// stubChat is a deterministic ChatProvider for the rewriter unit tests.
// It records the last ToolUse call so tests can assert on the wire shape.
type stubChat struct {
	resp     kernel.ToolUseResponse
	err      error
	lastUser string
}

func (s *stubChat) Complete(_ context.Context, _ kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	return kernel.CompleteResponse{}, errors.New("rewriter must not call Complete")
}

func (s *stubChat) ToolUse(_ context.Context, req kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	if len(req.Messages) > 0 {
		s.lastUser = req.Messages[len(req.Messages)-1].Content
	}
	return s.resp, s.err
}

func newRegistry(t *testing.T) prompts.Registry {
	t.Helper()
	reg := prompts.New(nil)
	if err := RegisterPrompt(context.Background(), reg); err != nil {
		t.Fatalf("RegisterPrompt: %v", err)
	}
	return reg
}

func toolResp(query string, at string) kernel.ToolUseResponse {
	payload := map[string]string{"query": query}
	if at != "" {
		payload["at"] = at
	}
	raw, _ := json.Marshal(payload)
	return kernel.ToolUseResponse{
		ToolUse: &kernel.ToolInvocation{
			Name:  rewriteToolName,
			Input: raw,
		},
		StopReason: "tool_use",
	}
}

func TestNewRewriterValidation(t *testing.T) {
	t.Parallel()
	reg := newRegistry(t)
	chat := &stubChat{}

	cases := []struct {
		name  string
		chat  kernel.ChatProvider
		reg   prompts.Registry
		model string
		want  string
	}{
		{"nil chat", nil, reg, "m", "nil chat"},
		{"nil reg", chat, nil, "m", "nil prompts"},
		{"empty model", chat, reg, "  ", "empty model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.chat, tc.reg, tc.model)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRewriteSuccess(t *testing.T) {
	t.Parallel()
	reg := newRegistry(t)
	chat := &stubChat{resp: toolResp("expanded query", "")}
	rw, err := New(chat, reg, "llama3.2")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := rw.Rewrite(context.Background(), "original", kernel.RewriteHint{})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if got.Query != "expanded query" {
		t.Errorf("Query = %q, want %q", got.Query, "expanded query")
	}
	if got.At != nil {
		t.Errorf("At = %v, want nil (no temporal anchor)", got.At)
	}
}

func TestRewriteResolvesTemporalAnchor(t *testing.T) {
	t.Parallel()
	reg := newRegistry(t)
	chat := &stubChat{resp: toolResp("Rachel ukulele lesson", "2023-02-01T00:00:00Z")}
	rw, _ := New(chat, reg, "m")
	now := time.Date(2023, 4, 4, 0, 0, 0, 0, time.UTC)
	got, err := rw.Rewrite(context.Background(), "Wednesday two months ago", kernel.RewriteHint{CurrentDate: &now})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if got.At == nil {
		t.Fatal("At is nil, want resolved timestamp")
	}
	if !got.At.Equal(time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("At = %v, want 2023-02-01T00:00:00Z", got.At)
	}
	if !strings.Contains(chat.lastUser, "CURRENT_DATE: 2023-04-04") {
		t.Errorf("user message missing CURRENT_DATE hint: %q", chat.lastUser)
	}
}

func TestRewriteFallsBackOnEmpty(t *testing.T) {
	t.Parallel()
	reg := newRegistry(t)
	chat := &stubChat{resp: toolResp("   ", "")}
	rw, _ := New(chat, reg, "m")
	got, err := rw.Rewrite(context.Background(), "fallback", kernel.RewriteHint{})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if got.Query != "fallback" {
		t.Errorf("Query = %q, want pass-through %q", got.Query, "fallback")
	}
}

func TestRewriteFallsBackOnProviderError(t *testing.T) {
	t.Parallel()
	reg := newRegistry(t)
	chat := &stubChat{err: errors.New("boom")}
	rw, _ := New(chat, reg, "m")
	got, err := rw.Rewrite(context.Background(), "ask", kernel.RewriteHint{})
	if err == nil {
		t.Fatal("expected wrapped error")
	}
	if got.Query != "ask" {
		t.Errorf("Query = %q, want pass-through on provider error", got.Query)
	}
}

func TestRewriteFallsBackWhenToolNotInvoked(t *testing.T) {
	t.Parallel()
	reg := newRegistry(t)
	chat := &stubChat{resp: kernel.ToolUseResponse{StopReason: "end_turn"}}
	rw, _ := New(chat, reg, "m")
	got, err := rw.Rewrite(context.Background(), "ask", kernel.RewriteHint{})
	if err == nil {
		t.Fatal("expected error when tool not invoked")
	}
	if got.Query != "ask" {
		t.Errorf("Query = %q, want pass-through %q", got.Query, "ask")
	}
}
