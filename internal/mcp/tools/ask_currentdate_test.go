package tools_test

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// systemPromptRecorder is a kernel.ChatProvider that captures the system
// prompt of the most recent Complete call so the test can grep for
// "TODAY'S DATE" — proof that currentDate was non-zero by the time
// composer.ComposeWithCurrentDate ran.
type systemPromptRecorder struct {
	lastSystem atomic.Pointer[string]
}

func (s *systemPromptRecorder) Complete(_ context.Context, req kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	sys := req.System
	s.lastSystem.Store(&sys)
	return kernel.CompleteResponse{Content: "Today is the anchor [1].", StopReason: "end_turn"}, nil
}

func (s *systemPromptRecorder) ToolUse(_ context.Context, _ kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	return kernel.ToolUseResponse{}, nil
}

// askSingleHitQuery yields exactly one synthetic hit so the composer has
// non-empty context and renders the full prompt template.
type askSingleHitQuery struct {
	kernel.NoopQuery
	hit kernel.Hit
}

func (q *askSingleHitQuery) Recall(_ context.Context, _ kernel.RecallRequest) iter.Seq2[kernel.Hit, error] {
	hit := q.hit
	return func(yield func(kernel.Hit, error) bool) { yield(hit, nil) }
}

// TestAsk_InjectsSystemClockWhenCurrentDateOmitted is the regression
// guard for the user-reported "How long ago did I work at Erebor
// Inc.?" abstain. The MCP ask tool received no currentDate from the
// CLI, defaulted to time.Time{}, the composer's TODAY'S DATE block
// was conditional on a non-zero anchor, and the prompt sent to the
// LLM omitted it entirely — the model had no reference for "now" and
// abstained. This test fails before the system-clock fallback lands
// and passes after.
func TestAsk_InjectsSystemClockWhenCurrentDateOmitted(t *testing.T) {
	reg := prompts.New(nil)
	if err := composer.RegisterComposePrompt(context.Background(), reg); err != nil {
		t.Fatalf("RegisterComposePrompt: %v", err)
	}
	chat := &systemPromptRecorder{}
	comp, err := composer.New(
		composer.Config{Model: "qwen2.5:7b"},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("composer.New: %v", err)
	}

	q := &askSingleHitQuery{hit: kernel.Hit{
		UID:        "01JSAMPLEEPISODE0000000001",
		Snippet:    "I worked at Erebor Inc. on Tuesday.",
		Confidence: 0.9,
		CreateTime: time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC),
		FusedScore: 0.5,
	}}

	a := tools.NewAsk(q, comp)

	// Request without a currentDate field — the CLI default path.
	req := json.RawMessage(`{"question":"How long ago did I work at Erebor Inc.?"}`)
	if _, err := a.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	sys := chat.lastSystem.Load()
	if sys == nil {
		t.Fatal("composer never called the chat provider")
	}
	// The TODAY'S DATE block is conditional in the template; its
	// rendered text is "<YYYY-MM-DD> is the reference date for ...".
	// The literal string "TODAY'S DATE" also appears in the always-
	// rendered DATE ARITHMETIC worked examples, so we look for the
	// concrete sentinel that only the block produces.
	const blockSentinel = "is the reference date for \"today\""
	if !strings.Contains(*sys, blockSentinel) {
		t.Errorf("rendered system prompt missing TODAY'S DATE block sentinel %q — currentDate was not injected:\n%s",
			blockSentinel, *sys)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if !strings.Contains(*sys, today) {
		t.Errorf("rendered system prompt missing today's date %q — fallback didn't use time.Now():\n%s", today, *sys)
	}
}
