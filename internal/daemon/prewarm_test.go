package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// warmEmbedder is a recording kernel.Embedder for the pre-warm tests.
type warmEmbedder struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (e *warmEmbedder) Embed(context.Context, string) ([]float32, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	return []float32{0}, nil
}

func (e *warmEmbedder) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// warmChat is a recording kernel.ChatProvider for the pre-warm tests.
type warmChat struct {
	mu    sync.Mutex
	calls int
	model string
	err   error
}

func (c *warmChat) Complete(_ context.Context, req kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	c.mu.Lock()
	c.calls++
	c.model = req.Model
	c.mu.Unlock()
	if c.err != nil {
		return kernel.CompleteResponse{}, c.err
	}
	return kernel.CompleteResponse{Content: "ok"}, nil
}

func (c *warmChat) ToolUse(context.Context, kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	return kernel.ToolUseResponse{}, errors.New("warmChat: ToolUse unused")
}

func (c *warmChat) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestWarmTargets_GatesRemoteChat verifies the embedder is always warmed when
// present, but the chat provider is only selected for a local (Ollama) provider
// — a remote provider must be omitted so pre-warm never makes a billable call.
func TestWarmTargets_GatesRemoteChat(t *testing.T) {
	t.Parallel()

	emb := &warmEmbedder{}
	chat := &warmChat{}

	t.Run("local chat is selected with its model", func(t *testing.T) {
		t.Parallel()
		gotEmb, gotChat, model := warmTargets(&Wired{Embedder: emb, Chat: chat, ChatLocal: true, ChatModel: "qwen2.5:7b"})
		if gotEmb == nil {
			t.Fatal("embedder not selected")
		}
		if gotChat == nil {
			t.Fatal("local chat not selected")
		}
		if model != "qwen2.5:7b" {
			t.Fatalf("model = %q, want qwen2.5:7b", model)
		}
	})

	t.Run("remote chat is omitted", func(t *testing.T) {
		t.Parallel()
		gotEmb, gotChat, model := warmTargets(&Wired{Embedder: emb, Chat: chat, ChatLocal: false, ChatModel: ""})
		if gotEmb == nil {
			t.Fatal("embedder not selected")
		}
		if gotChat != nil {
			t.Fatal("remote chat must not be selected for pre-warm")
		}
		if model != "" {
			t.Fatalf("model = %q, want empty for remote chat", model)
		}
	})

	t.Run("nil wired is safe", func(t *testing.T) {
		t.Parallel()
		gotEmb, gotChat, _ := warmTargets(nil)
		if gotEmb != nil || gotChat != nil {
			t.Fatal("nil wired must select nothing")
		}
	})
}

// TestWarmModels_CallsBoth verifies a successful warm-up hits both models and
// passes the configured chat model.
func TestWarmModels_CallsBoth(t *testing.T) {
	t.Parallel()

	emb := &warmEmbedder{}
	chat := &warmChat{}
	warmModels(context.Background(), emb, chat, "qwen2.5:7b", nil)

	if emb.callCount() != 1 {
		t.Fatalf("embed calls = %d, want 1", emb.callCount())
	}
	if chat.callCount() != 1 {
		t.Fatalf("chat calls = %d, want 1", chat.callCount())
	}
	if chat.model != "qwen2.5:7b" {
		t.Fatalf("chat model = %q, want qwen2.5:7b", chat.model)
	}
}

// TestWarmModels_SwallowsErrors verifies the daemon still serves when a model
// cannot be pre-loaded: a warm-up error is logged, never propagated or panicked.
func TestWarmModels_SwallowsErrors(t *testing.T) {
	t.Parallel()

	emb := &warmEmbedder{err: errors.New("ollama cold")}
	chat := &warmChat{err: errors.New("ollama cold")}
	// Must not panic; both are still attempted.
	warmModels(context.Background(), emb, chat, "m", nil)

	if emb.callCount() != 1 || chat.callCount() != 1 {
		t.Fatalf("embed=%d chat=%d, want each attempted once", emb.callCount(), chat.callCount())
	}
}

// TestWarmModels_NilProvidersSafe verifies no-op behavior (no panic) when there
// is nothing local to warm.
func TestWarmModels_NilProvidersSafe(t *testing.T) {
	t.Parallel()
	warmModels(context.Background(), nil, nil, "", nil)
}
