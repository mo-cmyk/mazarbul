//go:build smoke

package ollama

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestSmoke_CompleteAgainstLocalOllama runs Complete against a live Ollama
// daemon at the default localhost:11434. Build-tagged so it stays out of the
// normal test run; invoke explicitly with:
//
//	OLLAMA_SMOKE_CHAT_MODEL=llama3.2:latest \
//	  go test -tags=smoke -race -run=TestSmoke ./internal/provider/ollama/...
//
// The test asserts that the wire mapping (system prompt, MaxTokens,
// Temperature, response Usage / StopReason / Content) round-trips correctly
// against the real daemon. The unit tests guard the shape against an
// httptest stub; this smoke is the proof that the contract matches what
// Ollama ships today.
func TestSmoke_CompleteAgainstLocalOllama(t *testing.T) {
	model := os.Getenv("OLLAMA_SMOKE_CHAT_MODEL")
	if model == "" {
		model = "llama3.2:latest"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	c, err := New(ctx, Options{ChatModel: model})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Logf("client: embed=%s chat=%s base=%s", c.EmbedModel(), c.ChatModel(), c.BaseURL())

	temp := 0.1
	resp, err := c.Complete(ctx, &CompleteRequest{
		Model:       model,
		System:      "Answer with a single word, lowercase, no punctuation.",
		MaxTokens:   8,
		Temperature: &temp,
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("What color is the sky on a clear day?")}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	t.Logf("response: role=%s stop=%s in=%d out=%d createTime=%s text=%q",
		resp.Role, resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.CreateTime.Format(time.RFC3339), resp.Content[0].Text)

	if resp.Role != RoleAssistant {
		t.Errorf("Role = %q, want %q", resp.Role, RoleAssistant)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != ContentTypeText {
		t.Errorf("Content = %+v, want one text block", resp.Content)
	}
	if resp.Content[0].Text == "" {
		t.Error("Content[0].Text is empty, want non-empty reply")
	}
	if resp.StopReason == "" {
		t.Error("StopReason is empty, want set when done=true")
	}
	if resp.Usage.InputTokens <= 0 {
		t.Errorf("Usage.InputTokens = %d, want positive", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens <= 0 {
		t.Errorf("Usage.OutputTokens = %d, want positive", resp.Usage.OutputTokens)
	}
	if resp.CreateTime.IsZero() || resp.CreateTime.Location() != time.UTC {
		t.Errorf("CreateTime = %v, want non-zero UTC", resp.CreateTime)
	}
}
