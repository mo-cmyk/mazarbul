//go:build integration

package tools_test

// e2e_ollama_bench_test.go is the REAL end-to-end profile bench: same data,
// same code path, real Ollama for chat (qwen2.5:7b) and embed (bge-m3). It
// measures wall-clock per stage so the share of Mazarbul-Go time vs
// Ollama-LLM time vs storage-fsync time becomes visible.
//
// The companion file e2e_profile_bench_test.go uses stubs to isolate the Go
// pipeline cost; this file uses the real provider so the absolute end-to-end
// number lands.
//
// Requires a running Ollama instance with bge-m3 + qwen2.5:7b installed. Skips
// the bench when OLLAMA_BENCH=1 is unset OR when the probe call to Ollama
// fails, so CI never hangs on a missing service.
//
// Run with:
//
//	OLLAMA_BENCH=1 go test -tags 'fts5 integration' -bench=BenchmarkE2E_Ollama \
//	    -benchtime=5x -run=^$ -cpuprofile=/tmp/ollama.prof \
//	    -trace=/tmp/ollama.trace -benchmem -timeout=30m \
//	    ./internal/mcp/tools/

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/cache"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
	"github.com/mo-cmyk/mazarbul/internal/kernel/extractor"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
)

const (
	ollamaBenchURL       = "http://localhost:11434"
	ollamaBenchChatModel = "qwen2.5:7b"
	// ollamaBenchEmbedModel intentionally mirrors ollama.DefaultEmbedModel so
	// the bench reflects what a fresh install of `maz init` would actually
	// run. Matryoshka prefix truncation to storage.VectorDim is handled
	// automatically by the Ollama embed client (see Embed/EmbedBatch).
	ollamaBenchEmbedModel = ollama.DefaultEmbedModel
)

// timingChatProvider wraps a real ChatProvider and logs per-call duration +
// token usage to stderr. Used to decompose the drain (extract + resolver) cost
// into individual LLM call types: extract / disambiguate / predicate_canon /
// contradiction / summary_regen / naming_canon.
//
// Classification is by Tool.Name (ToolUse) or first user-message prefix
// (Complete). Heuristic, not bulletproof; good enough for a profiling pass.
type timingChatProvider struct {
	inner kernel.ChatProvider
	stats *callStats
}

type callStats struct {
	mu    sync.Mutex
	calls map[string][]time.Duration
}

func newCallStats() *callStats {
	return &callStats{calls: make(map[string][]time.Duration)}
}

func (s *callStats) record(kind string, d time.Duration) {
	s.mu.Lock()
	s.calls[kind] = append(s.calls[kind], d)
	s.mu.Unlock()
}

func (s *callStats) reportAndReset(b *testing.B) {
	b.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var total time.Duration
	for kind, ds := range s.calls {
		var sum time.Duration
		for _, d := range ds {
			sum += d
		}
		total += sum
		avg := sum / time.Duration(len(ds))
		b.Logf("LLM %s: n=%d total=%s avg=%s", kind, len(ds), sum, avg)
	}
	b.Logf("LLM total across all kinds: %s", total)
	s.calls = make(map[string][]time.Duration)
}

func (p *timingChatProvider) Complete(ctx context.Context, req kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	start := time.Now()
	resp, err := p.inner.Complete(ctx, req)
	kind := classifyComplete(req)
	p.stats.record(kind, time.Since(start))
	return resp, err
}

func (p *timingChatProvider) ToolUse(ctx context.Context, req kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	start := time.Now()
	resp, err := p.inner.ToolUse(ctx, req)
	kind := "tool:" + req.Tool.Name
	p.stats.record(kind, time.Since(start))
	return resp, err
}

func classifyComplete(req kernel.CompleteRequest) string {
	if len(req.Messages) == 0 {
		return "complete:empty"
	}
	first := req.Messages[0].Content
	switch {
	case strings.Contains(first, "Regenerate the summary"), strings.Contains(strings.ToLower(first), "summary"):
		return "complete:summary"
	case strings.Contains(strings.ToLower(first), "canonical"):
		return "complete:naming"
	default:
		return "complete:other"
	}
}

// requireOllama gates each bench behind a real Ollama probe + the
// OLLAMA_BENCH=1 env var. Skips loudly so a runner that forgets the flag
// understands why nothing measured. Probe is HEAD /api/tags with a 2s budget.
func requireOllama(b *testing.B) {
	b.Helper()
	if os.Getenv("OLLAMA_BENCH") != "1" {
		b.Skip("OLLAMA_BENCH=1 not set; skipping real-Ollama bench. Run with: OLLAMA_BENCH=1 go test -tags 'fts5 integration' -bench=BenchmarkE2E_Ollama ...")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer probeCancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, ollamaBenchURL+"/api/tags", nil)
	if err != nil {
		b.Skipf("ollama probe request build failed: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		b.Skipf("Ollama probe failed (%s): %v; start Ollama before running this bench", ollamaBenchURL, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.Skipf("Ollama probe returned %d; expected 200", resp.StatusCode)
	}
}

// ollamaFixture is the real-stack equivalent of benchFixture. Real Ollama for
// chat AND embed; full extractor + resolver + composer + cache wired in.
type ollamaFixture struct {
	orch     *orchestrator.Orchestrator
	hybrid   *kernel.HybridQuery
	composer *composer.Composer
	chat     kernel.ChatProvider
	embed    kernel.Embedder
	stats    *callStats
}

func newOllamaFixture(ctx context.Context, b *testing.B) *ollamaFixture {
	b.Helper()
	dataDir := b.TempDir()

	// Real Ollama client. EmbedModel + ChatModel both set so probes pass.
	client, err := ollama.New(ctx, ollama.Options{
		BaseURL:    ollamaBenchURL,
		EmbedModel: ollamaBenchEmbedModel,
		ChatModel:  ollamaBenchChatModel,
	})
	if err != nil {
		b.Fatalf("ollama.New: %v", err)
	}
	// Single chat slot: the benchmark drives extraction serially, so the
	// concurrency ceiling tracks that (NewChatProvider now takes maxConcurrent).
	rawChat, err := ollama.NewChatProvider(client, 1)
	if err != nil {
		b.Fatalf("ollama.NewChatProvider: %v", err)
	}
	stats := newCallStats()
	chat := kernel.ChatProvider(&timingChatProvider{inner: rawChat, stats: stats})

	ont := kernel.NewMemoryOntology()
	if err := ontology.RegisterEpisode(ctx, ont); err != nil {
		b.Fatalf("RegisterEpisode: %v", err)
	}
	if err := ontology.RegisterEntity(ctx, ont); err != nil {
		b.Fatalf("RegisterEntity: %v", err)
	}
	if err := ontology.RegisterMention(ctx, ont); err != nil {
		b.Fatalf("RegisterMention: %v", err)
	}
	if err := ontology.RegisterRelation(ctx, ont); err != nil {
		b.Fatalf("RegisterRelation: %v", err)
	}
	if err := ontology.RegisterRelationCandidatesProposed(ctx, ont); err != nil {
		b.Fatalf("RegisterRelationCandidatesProposed: %v", err)
	}

	orch, err := orchestrator.New(ctx, orchestrator.Config{
		DataDir:           dataDir,
		Ontology:          ont,
		Embedder:          client,
		Logger:            zap.NewNop(),
		ExtractionWorkers: 4,
	})
	if err != nil {
		b.Fatalf("orchestrator.New: %v", err)
	}
	b.Cleanup(func() { _ = orch.Close() })

	// HybridQuery wiring against the shared sqlite store.
	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "index.db"))
	if err != nil {
		b.Fatalf("sqlite.Open: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	vec, err := sqlite.OpenVector(ctx, store)
	if err != nil {
		b.Fatalf("OpenVector: %v", err)
	}
	b.Cleanup(func() { _ = vec.Close() })
	fts, err := store.OpenFTS(ctx)
	if err != nil {
		b.Fatalf("OpenFTS: %v", err)
	}
	b.Cleanup(func() { _ = fts.Close() })

	// Caching embedder against the shared sqlite pools (mirrors daemon
	// wiring): the orchestrator wraps client internally, but we also need a
	// kernel.Embedder for HybridQuery construction.
	caching := cache.New(client, store.Reader(), store.Writer(), nil)
	b.Cleanup(func() { _ = caching.Close() })

	hybrid, err := kernel.NewHybridQuery(caching, vec, fts, store.Reader())
	if err != nil {
		b.Fatalf("NewHybridQuery: %v", err)
	}

	// Prompts + memory-layer collaborators.
	reg := prompts.New(nil)
	if err := extractor.RegisterExtractPrompt(ctx, reg); err != nil {
		b.Fatalf("register extract: %v", err)
	}
	if err := resolver.RegisterPrompts(ctx, reg); err != nil {
		b.Fatalf("register resolver: %v", err)
	}
	if err := composer.RegisterComposePrompt(ctx, reg); err != nil {
		b.Fatalf("register compose: %v", err)
	}

	predicates, err := resolver.NewRegistry(ctx, store.Writer())
	if err != nil {
		b.Fatalf("predicates: %v", err)
	}
	kindRegistry, err := resolver.NewKindRegistry(ctx, store.Writer())
	if err != nil {
		b.Fatalf("kind registry: %v", err)
	}

	ext := extractor.New(chat, reg, extractor.Config{
		Model:          ollamaBenchChatModel,
		RecentEpisodes: extractor.NewSQLiteRecentEpisodes(store.Writer()),
	})
	res, err := resolver.New(resolver.Config{Model: ollamaBenchChatModel}, resolver.Deps{
		EventLog:     orch.Log,
		Embedder:     caching,
		Chat:         chat,
		Vec:          vec,
		Reader:       store.Reader(),
		Writer:       store.Writer(),
		Prompts:      reg,
		Predicates:   predicates,
		KindRegistry: kindRegistry,
		Logger:       zap.NewNop(),
	})
	if err != nil {
		b.Fatalf("resolver.New: %v", err)
	}
	orch.Log.SetExtractor(ext)
	orch.Log.SetResolver(res)

	comp, err := composer.New(composer.Config{Model: ollamaBenchChatModel}, chat, reg, zap.NewNop())
	if err != nil {
		b.Fatalf("composer.New: %v", err)
	}

	return &ollamaFixture{
		orch:     orch,
		hybrid:   hybrid,
		composer: comp,
		chat:     chat,
		embed:    caching,
		stats:    stats,
	}
}

// stageTimer collects per-stage durations across b.N iterations so the bench
// can ReportMetric ms/stage averages alongside ns/op.
type stageTimer struct {
	mu     sync.Mutex
	totals map[string]time.Duration
	counts map[string]int
}

func newStageTimer() *stageTimer {
	return &stageTimer{totals: make(map[string]time.Duration), counts: make(map[string]int)}
}

func (s *stageTimer) record(stage string, d time.Duration) {
	s.mu.Lock()
	s.totals[stage] += d
	s.counts[stage]++
	s.mu.Unlock()
}

func (s *stageTimer) report(b *testing.B) {
	b.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for stage, total := range s.totals {
		avg := total / time.Duration(s.counts[stage])
		// Use ms units for readability.
		b.ReportMetric(float64(avg.Microseconds())/1000.0, "ms/"+stage)
	}
}

// ---- BenchmarkE2E_Ollama_Remember: writer + extract + resolve, fully real ----

// BenchmarkE2E_Ollama_Remember drives one full remember per iteration against
// real Ollama. Stages timed:
//
//   - append: kernel.EventLog.Append (writer hot path; should be fast post-Chunks 14/17/18)
//   - drain:  WaitForBackgroundWork (extractor + resolver finish)
//
// The append stage is the user-facing latency (what `maz remember` returns
// in). The drain stage is the background cost (what blocks immediate recall
// visibility). Both belong in the report because real workloads notice both.
func BenchmarkE2E_Ollama_Remember(b *testing.B) {
	requireOllama(b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	fx := newOllamaFixture(ctx, b)
	rememberTool := tools.NewRemember(fx.orch.Log)
	st := newStageTimer()

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		text := fmt.Sprintf("Frodo Baggins works at Erebor Inc. based in Berlin. He joined in %d.", 2020+i%6)
		args, _ := json.Marshal(map[string]any{
			"text":       text,
			"source":     "ollama-bench",
			"confidence": 1.0,
		})

		stStart := time.Now()
		if _, err := rememberTool.Invoke(ctx, args); err != nil {
			b.Fatalf("remember[%d]: %v", i, err)
		}
		st.record("append", time.Since(stStart))

		stStart = time.Now()
		if err := fx.orch.Log.WaitForBackgroundWork(ctx); err != nil {
			b.Fatalf("drain[%d]: %v", i, err)
		}
		st.record("drain", time.Since(stStart))
	}
	st.report(b)
	fx.stats.reportAndReset(b)
}

// ---- BenchmarkE2E_Ollama_Recall: real bge-m3 + 4-lane fuse ----

// BenchmarkE2E_Ollama_Recall seeds 20 episodes (drained) then drives the
// recall tool N times. Real Ollama for the query embedding; the four lanes
// (vec / FTS / name-vec / graph) and RRF fusion run on Go. Stages:
//
//   - embed: query embedding via Ollama (cache miss the first time; cache hit thereafter)
//   - recall: HybridQuery.Recall full path
func BenchmarkE2E_Ollama_Recall(b *testing.B) {
	requireOllama(b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	fx := newOllamaFixture(ctx, b)
	ollamaSeed(ctx, b, fx, 20)

	recallTool := tools.NewRecall(fx.hybrid)
	args, _ := json.Marshal(map[string]any{
		"query": "Who works at Erebor?",
		"limit": 10,
	})
	st := newStageTimer()

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_ = i
		stStart := time.Now()
		if _, err := recallTool.Invoke(ctx, args); err != nil {
			b.Fatalf("recall: %v", err)
		}
		st.record("recall_total", time.Since(stStart))
	}
	st.report(b)
}

// ---- BenchmarkE2E_Ollama_Ask: rewrite (no) + recall + real LLM compose ----

// BenchmarkE2E_Ollama_Ask drives one ask per iteration. The compose call uses
// real Ollama chat (qwen2.5:7b); this is THE expensive stage. Reported
// alongside the recall + parse stages so the LLM share of total latency is
// explicit. The first iteration absorbs cold-start (KV cache warm-up); use
// -benchtime=5x or larger and look at p50 ms/compose, not the mean.
func BenchmarkE2E_Ollama_Ask(b *testing.B) {
	requireOllama(b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	fx := newOllamaFixture(ctx, b)
	ollamaSeed(ctx, b, fx, 20)

	askTool := tools.NewAsk(fx.hybrid, fx.composer)
	args, _ := json.Marshal(map[string]any{
		"question": "Who works at Erebor?",
	})
	st := newStageTimer()

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_ = i
		stStart := time.Now()
		raw, err := askTool.Invoke(ctx, args)
		if err != nil {
			b.Fatalf("ask: %v", err)
		}
		st.record("ask_total", time.Since(stStart))

		// Sanity: confirm the response is non-empty and contains an answer.
		// Without this check a silently-broken composer would benchmark as
		// "fast" while doing nothing useful.
		if len(raw) < 32 {
			b.Fatalf("ask returned suspiciously short payload (%d bytes): %s", len(raw), string(raw))
		}
		var ans composer.Answer
		if err := json.Unmarshal(raw, &ans); err != nil {
			b.Fatalf("unmarshal Answer: %v", err)
		}
		if strings.TrimSpace(ans.Answer) == "" {
			b.Fatal("ask returned an empty Answer.Answer; composer is doing zero useful work")
		}
	}
	st.report(b)
}

// ollamaSeed mirrors seedEpisodes for the real-Ollama fixture (different
// fixture type, otherwise identical). Drains background work before returning
// so the recall vector lane is populated.
func ollamaSeed(ctx context.Context, b *testing.B, fx *ollamaFixture, n int) {
	b.Helper()
	rememberTool := tools.NewRemember(fx.orch.Log)
	for i := range n {
		text := fmt.Sprintf("Frodo Baggins works at Erebor Inc. Berlin office. Note #%d for variety.", i)
		args, _ := json.Marshal(map[string]any{
			"text":       text,
			"source":     "ollama-bench",
			"confidence": 1.0,
		})
		if _, err := rememberTool.Invoke(ctx, args); err != nil {
			b.Fatalf("seed remember %d: %v", i, err)
		}
	}
	// One drain at the end is sufficient: the bounded extraction pool runs
	// jobs concurrently, so we don't need to drain after every seed.
	if err := fx.orch.Log.WaitForBackgroundWork(ctx); err != nil {
		// Errors here are warnings; partial extraction is acceptable for
		// a recall bench; the FTS lane still works.
		b.Logf("WaitForBackgroundWork (seed drain): %v", err)
	}
	_ = errors.New // keep errors import alive when build tag toggles
}
