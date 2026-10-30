package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// BenchmarkComplete_HTTPRoundtrip measures the marshal / network / decode
// path on /api/chat against an httptest server that returns a constant
// 32-token response. The real-Ollama bottleneck is model inference (out of
// scope for this benchmark); this number is the floor for the wrapper
// itself.
//
// Reported as the baseline in the PR for issue #86. Per
// `agent-docs/profiling.md` + AGENTS.md rule 10 (hot-path code gets a
// benchmark with baseline numbers).
func BenchmarkComplete_HTTPRoundtrip(b *testing.B) {
	body := chatBenchBody(32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"bge-m3:latest","model":"bge-m3:latest"},{"name":"llama3.1:8b","model":"llama3.1:8b"}]}`))
		case "/api/chat":
			_, _ = w.Write(body)
		}
	}))
	b.Cleanup(srv.Close)

	c, err := New(context.Background(), Options{
		BaseURL:    srv.URL,
		ChatModel:  "llama3.1:8b",
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	})
	if err != nil {
		b.Fatalf("New: %v", err)
	}

	req := &CompleteRequest{
		Model:     "llama3.1:8b",
		MaxTokens: 64,
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("hi")}},
		},
	}
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		resp, err := c.Complete(ctx, req)
		if err != nil {
			b.Fatalf("Complete: %v", err)
		}
		if resp.Usage.OutputTokens != 32 {
			b.Fatalf("OutputTokens = %d, want 32", resp.Usage.OutputTokens)
		}
	}
}

// chatBenchBody returns a pre-marshalled /api/chat response with the given
// eval_count. Built once outside the bench loop so b.N does not include
// formatting cost.
func chatBenchBody(evalCount int) []byte {
	const head = "{\"model\":\"llama3.1:8b\",\"created_at\":\"2026-05-24T00:00:00Z\",\"message\":{\"role\":\"assistant\",\"content\":\"ok ok ok\"},\"done\":true,\"done_reason\":\"stop\",\"prompt_eval_count\":5,\"eval_count\":"
	const tail = `}`
	buf := make([]byte, 0, len(head)+8+len(tail))
	buf = append(buf, head...)
	buf = strconv.AppendInt(buf, int64(evalCount), 10)
	buf = append(buf, tail...)
	return buf
}
