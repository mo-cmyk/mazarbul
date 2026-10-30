package prompts_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// body4KB constructs a ~4 KB template body with 3 substitution sites.
// The prefix and suffix strings pad the body to just over 4000 bytes so
// the benchmark exercises a realistic hot-path allocation profile.
func body4KB() string {
	prefix := strings.Repeat("The system has been instructed to act as a helpful assistant. ", 20)
	suffix := strings.Repeat("Please respond with as much detail as possible. ", 20)
	return prefix +
		"User: {{index . \"query\"}} " +
		"Context: {{index . \"context\"}} " +
		"Format: {{index . \"format\"}} " +
		suffix
}

// BenchmarkRender_4KB measures the hot-path Render cost for a ~4 KB template
// with 3 substitutions. Target: <100 µs on M1 Mac. b.ReportAllocs shows
// allocation count and bytes per operation.
func BenchmarkRender_4KB(b *testing.B) {
	r := prompts.New(nil) // nil log: skip audit goroutine overhead in this benchmark
	ctx := context.Background()

	tmpl := prompts.Template{
		Name:        "bench.4kb",
		Version:     1,
		Body:        body4KB(),
		Description: "4 KB benchmark template",
	}
	if err := r.Register(ctx, tmpl); err != nil {
		b.Fatalf("Register: %v", err)
	}

	vars := prompts.RenderVars{
		"query":   "what is the capital of France",
		"context": "The user is asking a geography question.",
		"format":  "plain text",
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_, err := r.Render(ctx, "bench.4kb", 1, vars)
		if err != nil {
			b.Fatalf("Render: %v", err)
		}
	}
}

// BenchmarkRender_Concurrent_4KB runs BenchmarkRender_4KB with 8 goroutines
// to surface RWMutex contention. Each goroutine acquires the read lock for
// the lookup, so N goroutines can read concurrently; the benchmark should
// scale linearly with GOMAXPROCS up to the lock-free execution portion.
func BenchmarkRender_Concurrent_4KB(b *testing.B) {
	r := prompts.New(nil)
	ctx := context.Background()

	tmpl := prompts.Template{
		Name:        "bench.concurrent.4kb",
		Version:     1,
		Body:        body4KB(),
		Description: "4 KB concurrent benchmark template",
	}
	if err := r.Register(ctx, tmpl); err != nil {
		b.Fatalf("Register: %v", err)
	}

	vars := prompts.RenderVars{
		"query":   "what is the capital of France",
		"context": "The user is asking a geography question.",
		"format":  "plain text",
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.SetParallelism(8)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := r.Render(ctx, "bench.concurrent.4kb", 1, vars)
			if err != nil {
				b.Fatalf("Render: %v", err)
			}
		}
	})
}
