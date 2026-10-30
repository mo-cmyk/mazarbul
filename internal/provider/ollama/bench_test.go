package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
)

// BenchmarkEmbed_HTTPRoundtrip measures the marshal / network / decode /
// dim-check path against an httptest server that returns a constant
// 1024-float embedding. The real-Ollama bottleneck is model inference (out
// of scope for this benchmark); this number is the floor for the wrapper
// itself.
//
// Reported as the baseline in the PR for issue #50. Per `agent-docs/profiling.md`
// + AGENTS.md rule 10 (hot-path code gets a benchmark with baseline numbers).
func BenchmarkEmbed_HTTPRoundtrip(b *testing.B) {
	body := embedBenchBody(storage.VectorDim)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"bge-m3:latest","model":"bge-m3:latest"}]}`))
		case "/api/embed":
			_, _ = w.Write(body)
		}
	}))
	b.Cleanup(srv.Close)

	c, err := New(context.Background(), Options{
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	})
	if err != nil {
		b.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		vec, err := c.Embed(ctx, "the quick brown fox jumps over the lazy dog")
		if err != nil {
			b.Fatalf("Embed: %v", err)
		}
		if len(vec) != storage.VectorDim {
			b.Fatalf("len = %d, want %d", len(vec), storage.VectorDim)
		}
	}
}

// embedBenchBody returns a pre-marshalled /api/embed response of the
// given dimension. Reused across iterations so b.N does not include marshal
// cost of the canned response. The plural-array shape matches the new
// /api/embed endpoint: {"embeddings":[[f32,...]]}.
func embedBenchBody(dim int) []byte {
	// Build by hand to avoid pulling encoding/json into the bench setup; the
	// resulting buffer is tiny and deterministic.
	buf := make([]byte, 0, dim*16+32)
	buf = append(buf, `{"embeddings":[[`...)
	for i := range dim {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '0', '.', '1')
	}
	buf = append(buf, `]]}`...)
	return buf
}
