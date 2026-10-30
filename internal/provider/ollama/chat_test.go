package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// chatJSON returns a canned /api/chat response with the given assistant reply
// and token counts. Mirrors the documented Ollama wire shape.
func chatJSON(t *testing.T, content string, promptTokens, evalTokens int) []byte {
	t.Helper()
	body := chatWireResponse{
		Model:           "llama3.1:8b",
		CreatedAt:       "2026-05-24T00:00:00Z",
		Done:            true,
		DoneReason:      "stop",
		PromptEvalCount: promptTokens,
		EvalCount:       evalTokens,
	}
	body.Message.Role = "assistant"
	body.Message.Content = content
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal chat: %v", err)
	}
	return b
}

// newChatClient returns a Client whose probe accepts both an embed model and
// llama3.1:8b, with chatHandler wired for /api/chat. The probe handler
// answers /api/tags; everything else hits chatHandler.
func newChatClient(t *testing.T, chatHandler http.HandlerFunc) (*Client, *fakeServer) {
	t.Helper()
	srv := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel, "llama3.1:8b"))
		case "/api/chat":
			chatHandler(w, r)
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
		t.Fatalf("probe failed: %v", err)
	}
	return c, srv
}

// sampleChatRequest returns a minimal valid CompleteRequest.
func sampleChatRequest() *CompleteRequest {
	return &CompleteRequest{
		Model:     "llama3.1:8b",
		MaxTokens: 64,
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("hello")}},
		},
	}
}

func TestNew_ProbesChatModelWhenSet(t *testing.T) {
	t.Run("rejects when chat model not installed", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
		})
		_, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			ChatModel:  "llama3.1:8b",
			HTTPClient: fastClient(),
		})
		if !errors.Is(err, ErrModelMissing) {
			t.Fatalf("err = %v, want ErrModelMissing", err)
		}
		if want := "ollama pull llama3.1:8b"; !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err.Error(), want)
		}
	})

	t.Run("accepts when chat model is installed alongside embed", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel, "llama3.1:8b"))
		})
		c, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			ChatModel:  "llama3.1:8b",
			HTTPClient: fastClient(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c.ChatModel() != "llama3.1:8b" {
			t.Errorf("ChatModel = %q, want %q", c.ChatModel(), "llama3.1:8b")
		}
		// Single round trip: embed + chat probes share the /api/tags call.
		if got := fs.calls.Load(); got != 1 {
			t.Errorf("calls = %d, want 1 (shared probe)", got)
		}
	})

	t.Run("skips chat probe when ChatModel empty", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
		})
		c, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			HTTPClient: fastClient(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c.ChatModel() != "" {
			t.Errorf("ChatModel = %q, want empty", c.ChatModel())
		}
	})
}

func TestNew_ChatOnlyClient(t *testing.T) {
	// Greptile P1 (PR #99): chat-only callers must not be forced to install
	// the embedding model. When Options.EmbedModel is empty AND
	// Options.ChatModel is set, New must skip the embed default + probe and
	// succeed even on a daemon that only carries the chat model.
	t.Run("succeeds when only chat model installed", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, "llama3.1:8b"))
		})
		c, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			ChatModel:  "llama3.1:8b",
			HTTPClient: fastClient(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c.ChatModel() != "llama3.1:8b" {
			t.Errorf("ChatModel = %q, want %q", c.ChatModel(), "llama3.1:8b")
		}
		if c.EmbedModel() != "" {
			t.Errorf("EmbedModel = %q, want empty (chat-only mode)", c.EmbedModel())
		}
	})

	t.Run("rejects with chat-model message when chat model missing", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, "mistral:7b"))
		})
		_, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			ChatModel:  "llama3.1:8b",
			HTTPClient: fastClient(),
		})
		if !errors.Is(err, ErrModelMissing) {
			t.Fatalf("err = %v, want ErrModelMissing", err)
		}
		if want := "ollama pull llama3.1:8b"; !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err.Error(), want)
		}
		// The error must NOT instruct the user to install the embedding model
		// they never asked for.
		if strings.Contains(err.Error(), "bge-m3") {
			t.Errorf("err = %q, must not mention bge-m3 for a chat-only caller", err.Error())
		}
	})
}

func TestEmbed_RejectsWhenEmbedDisabled(t *testing.T) {
	// Chat-only client: Embed must fail loud, not silently send the empty
	// model name to /api/embed.
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = w.Write(tagsJSON(t, "llama3.1:8b"))
			return
		}
		t.Errorf("unexpected hit: %s", r.URL.Path)
	})
	c, err := New(context.Background(), Options{
		BaseURL:    fs.srv.URL,
		ChatModel:  "llama3.1:8b",
		HTTPClient: fastClient(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("Embed succeeded, want error")
	}
	if !strings.Contains(err.Error(), "embed not configured") {
		t.Errorf("err = %q, want it to mention embed not configured", err.Error())
	}
}

func TestComplete_Success(t *testing.T) {
	var chatCalls atomic.Int32
	c, _ := newChatClient(t, func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var got chatWireRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if got.Model != "llama3.1:8b" {
			t.Errorf("req.Model = %q, want llama3.1:8b", got.Model)
		}
		if got.Stream {
			t.Errorf("req.Stream = true, want false (streaming is M2+)")
		}
		if len(got.Messages) != 1 {
			t.Fatalf("messages len = %d, want 1", len(got.Messages))
		}
		if got.Messages[0].Role != "user" || got.Messages[0].Content != "hello" {
			t.Errorf("messages[0] = %+v, want {user, hello}", got.Messages[0])
		}
		if got.Options == nil || got.Options.NumPredict != 64 {
			t.Errorf("options = %+v, want NumPredict=64", got.Options)
		}

		_, _ = w.Write(chatJSON(t, "hi there", 5, 3))
	})

	resp, err := c.Complete(context.Background(), sampleChatRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("chat calls = %d, want 1", got)
	}
	if resp.Role != RoleAssistant {
		t.Errorf("Role = %q, want %q", resp.Role, RoleAssistant)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "hi there" {
		t.Errorf("Content = %+v, want one text block 'hi there'", resp.Content)
	}
	if resp.StopReason != "stop" {
		t.Errorf("StopReason = %q, want stop", resp.StopReason)
	}
	if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want {5, 3}", resp.Usage)
	}
	if resp.CreateTime.IsZero() {
		t.Error("CreateTime is zero, want set")
	}
	if resp.CreateTime.Location() != time.UTC {
		t.Errorf("CreateTime location = %v, want UTC", resp.CreateTime.Location())
	}
}

func TestComplete_PrependsSystemMessage(t *testing.T) {
	c, _ := newChatClient(t, func(w http.ResponseWriter, r *http.Request) {
		var got chatWireRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if len(got.Messages) != 2 {
			t.Fatalf("messages len = %d, want 2 (system + user)", len(got.Messages))
		}
		if got.Messages[0].Role != "system" || got.Messages[0].Content != "you are terse" {
			t.Errorf("messages[0] = %+v, want {system, you are terse}", got.Messages[0])
		}
		if got.Messages[1].Role != "user" {
			t.Errorf("messages[1].Role = %q, want user", got.Messages[1].Role)
		}
		_, _ = w.Write(chatJSON(t, "ok", 7, 1))
	})

	req := sampleChatRequest()
	req.System = "you are terse"
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestComplete_SystemPlusMultiTurn(t *testing.T) {
	// System prompt + alternating user/assistant turns + a final user
	// follow-up. Verifies that the system message is prepended exactly once
	// and that subsequent messages preserve their order and roles.
	c, _ := newChatClient(t, func(w http.ResponseWriter, r *http.Request) {
		var got chatWireRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		want := []chatWireMessage{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "second"},
		}
		if len(got.Messages) != len(want) {
			t.Fatalf("messages len = %d, want %d", len(got.Messages), len(want))
		}
		for i, w := range want {
			if got.Messages[i] != w {
				t.Errorf("messages[%d] = %+v, want %+v", i, got.Messages[i], w)
			}
		}
		_, _ = w.Write(chatJSON(t, "done", 9, 1))
	})

	req := &CompleteRequest{
		Model:     "llama3.1:8b",
		System:    "be terse",
		MaxTokens: 64,
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("first")}},
			{Role: RoleAssistant, Content: []ContentBlock{TextBlock("ok")}},
			{Role: RoleUser, Content: []ContentBlock{TextBlock("second")}},
		},
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestComplete_FlattensMultipleContentBlocks(t *testing.T) {
	// End-to-end: a Message carrying two text blocks must serialize as a
	// single concatenated content string on the wire. Guards the contract
	// the flattenContent unit test asserts in isolation.
	c, _ := newChatClient(t, func(w http.ResponseWriter, r *http.Request) {
		var got chatWireRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("messages len = %d, want 1", len(got.Messages))
		}
		if want := "foobar"; got.Messages[0].Content != want {
			t.Errorf("messages[0].Content = %q, want %q", got.Messages[0].Content, want)
		}
		_, _ = w.Write(chatJSON(t, "ok", 2, 1))
	})

	req := &CompleteRequest{
		Model:     "llama3.1:8b",
		MaxTokens: 64,
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("foo"), TextBlock("bar")}},
		},
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestComplete_OmitsOptionsWhenEmpty(t *testing.T) {
	// MaxTokens is required + positive, so NumPredict will always be set.
	// Confirm Temperature + Stop omit when not configured. (We can't get a
	// fully empty options object without violating the positive-MaxTokens
	// invariant, so this test guards the partial-omit case.)
	c, _ := newChatClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := readBody(r)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if strings.Contains(raw, `"temperature"`) {
			t.Errorf("body contains temperature: %s", raw)
		}
		if strings.Contains(raw, `"stop"`) {
			t.Errorf("body contains stop: %s", raw)
		}
		_, _ = w.Write(chatJSON(t, "ok", 1, 1))
	})

	if _, err := c.Complete(context.Background(), sampleChatRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestComplete_SendsTemperatureAndStop(t *testing.T) {
	c, _ := newChatClient(t, func(w http.ResponseWriter, r *http.Request) {
		var got chatWireRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if got.Options == nil {
			t.Fatal("options is nil, want set")
		}
		if got.Options.Temperature == nil || *got.Options.Temperature != 0.3 {
			t.Errorf("Temperature = %v, want 0.3", got.Options.Temperature)
		}
		if len(got.Options.Stop) != 1 || got.Options.Stop[0] != "</done>" {
			t.Errorf("Stop = %v, want [</done>]", got.Options.Stop)
		}
		_, _ = w.Write(chatJSON(t, "ok", 1, 1))
	})

	temp := 0.3
	req := sampleChatRequest()
	req.Temperature = &temp
	req.StopSequences = []string{"</done>"}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestComplete_RetriesOn5xx(t *testing.T) {
	var chatCalls atomic.Int32
	c, _ := newChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if chatCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(chatJSON(t, "second try", 4, 2))
	})

	resp, err := c.Complete(context.Background(), sampleChatRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content[0].Text != "second try" {
		t.Errorf("Content[0].Text = %q, want second try", resp.Content[0].Text)
	}
	if got := chatCalls.Load(); got != 2 {
		t.Errorf("chat calls = %d, want 2 (1 fail + 1 retry)", got)
	}
}

func TestComplete_ExhaustsRetries(t *testing.T) {
	var chatCalls atomic.Int32
	c, _ := newChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		chatCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})

	_, err := c.Complete(context.Background(), sampleChatRequest())
	if err == nil {
		t.Fatal("Complete succeeded, want error")
	}
	if got := chatCalls.Load(); got != maxRetries+1 {
		t.Errorf("chat calls = %d, want %d", got, maxRetries+1)
	}
}

func TestComplete_DoesNotRetry4xx(t *testing.T) {
	var chatCalls atomic.Int32
	c, _ := newChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		chatCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := c.Complete(context.Background(), sampleChatRequest())
	if err == nil {
		t.Fatal("Complete succeeded, want error")
	}
	if got := chatCalls.Load(); got != 1 {
		t.Errorf("chat calls = %d, want 1 (no retry on 4xx)", got)
	}
}

func TestComplete_ContextCancelDuringBackoff(t *testing.T) {
	c, _ := newChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err := c.Complete(ctx, sampleChatRequest())
	if err == nil {
		t.Fatal("Complete succeeded, want ctx error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(..., context.Canceled)", err)
	}
}

func TestComplete_RejectsBadInput(t *testing.T) {
	// Build a client without hitting the server; validation runs before
	// any HTTP. The probe still happens once during New.
	c, _ := newChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server hit; validation should reject before send")
	})

	cases := []struct {
		name string
		req  *CompleteRequest
		want string
	}{
		{
			name: "nil request",
			req:  nil,
			want: "CompleteRequest is required",
		},
		{
			name: "empty messages",
			req:  &CompleteRequest{MaxTokens: 64},
			want: "Messages must be non-empty",
		},
		{
			name: "non-positive MaxTokens",
			req: &CompleteRequest{
				Messages: []Message{{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}}},
			},
			want: "MaxTokens must be positive",
		},
		{
			name: "missing role",
			req: &CompleteRequest{
				MaxTokens: 64,
				Messages:  []Message{{Content: []ContentBlock{TextBlock("hi")}}},
			},
			want: "Role is required",
		},
		{
			name: "non-text block type",
			req: &CompleteRequest{
				MaxTokens: 64,
				Messages: []Message{
					{Role: RoleUser, Content: []ContentBlock{{Type: "image", Text: ""}}},
				},
			},
			want: "not supported (text only in M1)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Complete(context.Background(), tc.req)
			if err == nil {
				t.Fatalf("Complete succeeded, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestComplete_RejectsWhenChatDisabled(t *testing.T) {
	// Construct a client without ChatModel; Complete must fail loud.
	srv := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
			return
		}
		t.Errorf("unexpected hit: %s", r.URL.Path)
	})
	c, err := New(context.Background(), Options{
		BaseURL:    srv.srv.URL,
		HTTPClient: fastClient(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Complete(context.Background(), sampleChatRequest())
	if err == nil {
		t.Fatal("Complete succeeded, want error")
	}
	if !strings.Contains(err.Error(), "chat not configured") {
		t.Errorf("err = %q, want it to mention chat not configured", err.Error())
	}
}

func TestFlattenContent(t *testing.T) {
	cases := []struct {
		name   string
		blocks []ContentBlock
		want   string
	}{
		{"empty", nil, ""},
		{"single block", []ContentBlock{TextBlock("hello")}, "hello"},
		{"two blocks concat", []ContentBlock{TextBlock("foo"), TextBlock("bar")}, "foobar"},
		{"three blocks concat", []ContentBlock{TextBlock("a"), TextBlock("b"), TextBlock("c")}, "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flattenContent(tc.blocks); got != tc.want {
				t.Errorf("flattenContent = %q, want %q", got, tc.want)
			}
		})
	}
}

// readBody reads the request body fully and returns it as a string. Helper
// for tests that need to assert on raw JSON shape (e.g., field omission).
func readBody(r *http.Request) (string, error) {
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return string(buf), nil
			}
			return "", fmt.Errorf("read body: %w", err)
		}
	}
}
