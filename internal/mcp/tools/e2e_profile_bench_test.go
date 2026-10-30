//go:build integration

package tools_test

// e2e_profile_bench_test.go is the granular end-to-end profiler for the three
// agent-facing verbs: remember, recall, ask.
//
// Why "go test -bench" instead of /usr/bin/time on the daemon: the harness
// fully isolates LLM cost (stub ChatProvider, zero latency) and embedding
// cost (deterministic hash embedder) so the measurement reflects ONLY the
// Mazarbul Go pipeline overhead. Comparing this against a real-Ollama run
// gives the share of total latency that lives in our pipeline vs the model.
//
// Run with:
//
//	go test -tags 'fts5 integration' -bench=BenchmarkE2E -benchtime=1x -run=^$ \
//	    -cpuprofile=/tmp/e2e.prof -benchmem ./internal/mcp/tools/
//
// Then inspect with `go tool pprof -top /tmp/e2e.prof`.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
	"github.com/mo-cmyk/mazarbul/internal/kernel/embedder"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// ---- benchmark fixture: orchestrator + composer with stubs ----

// benchFixture bundles every collaborator the three benchmarks need so each
// can spin up a fresh isolated stack without copy-pasting setup.
type benchFixture struct {
	orch     *orchestrator.Orchestrator
	hybrid   kernel.Query
	composer *composer.Composer
	chat     stubChatProvider
}

// newBenchFixture wires a minimal end-to-end pipeline against a fresh data
// dir. Uses askHashEmbedder + stubChatProvider so no Ollama is needed.
// Returns the fixture; caller is responsible for nothing (orchestrator
// registers t.Cleanup via newAskE2EOrchestrator).
func newBenchFixture(ctx context.Context, b *testing.B) *benchFixture {
	b.Helper()
	dataDir := b.TempDir()
	orch := newBenchOrchestrator(ctx, b, dataDir, askHashEmbedder{})
	hybrid := newBenchHybridQuery(ctx, b, dataDir, askHashEmbedder{})
	chat := stubChatProvider{}
	comp := newBenchComposer(ctx, b, chat)
	return &benchFixture{
		orch:     orch,
		hybrid:   hybrid,
		composer: comp,
		chat:     chat,
	}
}

func newBenchOrchestrator(ctx context.Context, b *testing.B, dataDir string, emb embedder.Embedder) *orchestrator.Orchestrator {
	b.Helper()
	ont := kernel.NewMemoryOntology()
	if err := ontology.RegisterEpisode(ctx, ont); err != nil {
		b.Fatalf("RegisterEpisode: %v", err)
	}
	o, err := orchestrator.New(ctx, orchestrator.Config{
		DataDir:  dataDir,
		Ontology: ont,
		Embedder: emb,
	})
	if err != nil {
		b.Fatalf("orchestrator.New: %v", err)
	}
	b.Cleanup(func() { _ = o.Close() })
	return o
}

func newBenchHybridQuery(ctx context.Context, b *testing.B, dataDir string, emb kernel.Embedder) kernel.Query {
	b.Helper()
	sqlitePath := filepath.Join(dataDir, "index.db")
	store, err := sqlite.Open(ctx, sqlitePath)
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
	q, err := kernel.NewHybridQuery(emb, vec, fts, store.Reader())
	if err != nil {
		b.Fatalf("NewHybridQuery: %v", err)
	}
	return q
}

func newBenchComposer(ctx context.Context, b *testing.B, chat kernel.ChatProvider) *composer.Composer {
	b.Helper()
	reg := prompts.New(nil)
	if err := composer.RegisterComposePrompt(ctx, reg); err != nil {
		b.Fatalf("RegisterComposePrompt: %v", err)
	}
	c, err := composer.New(
		composer.Config{Model: "stub-model"},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		b.Fatalf("composer.New: %v", err)
	}
	return c
}

// seedEpisodes drives the remember tool N times against fx so subsequent
// recall benchmarks have a non-empty store. Returns the seeded episode UIDs.
func seedEpisodes(ctx context.Context, b *testing.B, fx *benchFixture, n int) []string {
	b.Helper()
	rememberTool := tools.NewRemember(fx.orch.Log)
	uids := make([]string, n)
	for i := range n {
		text := fmt.Sprintf("Frodo works at Erebor Inc. based in Berlin. Note #%d for variety.", i)
		args, _ := json.Marshal(map[string]any{
			"text":       text,
			"source":     "bench",
			"confidence": 1.0,
		})
		raw, err := rememberTool.Invoke(ctx, args)
		if err != nil {
			b.Fatalf("seed remember %d: %v", i, err)
		}
		var out struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			b.Fatalf("seed unmarshal %d: %v", i, err)
		}
		uids[i] = out.Name
	}
	return uids
}

// ---- BenchmarkE2E_Remember: writer hot path through the MCP tool ----

// BenchmarkE2E_Remember measures one remember tool invocation end-to-end:
// JSON-RPC decode (handled by the tool adapter, skipped here — we call
// Invoke directly), Episode payload marshal, kernel.EventLog.Append,
// dispatcher fan-out (4 layers, async embed). No LLM extractor or resolver
// is wired so this isolates the storage-stack cost.
func BenchmarkE2E_Remember(b *testing.B) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := newBenchFixture(ctx, b)
	rememberTool := tools.NewRemember(fx.orch.Log)
	argTemplate := map[string]any{
		"text":       "Frodo works at Erebor Inc. based in Berlin.",
		"source":     "bench",
		"confidence": 1.0,
	}

	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		argTemplate["text"] = fmt.Sprintf("Frodo works at Erebor #%d.", i)
		args, _ := json.Marshal(argTemplate)
		if _, err := rememberTool.Invoke(ctx, args); err != nil {
			b.Fatalf("remember[%d]: %v", i, err)
		}
	}
}

// ---- BenchmarkE2E_Recall: hybrid query against a pre-populated store ----

// BenchmarkE2E_Recall_N20 seeds 20 episodes (small store) then drives the
// recall tool N times. Measures the full HybridQuery pipeline: query embed,
// parallel fetch across 4 lanes (vec, FTS, name-vec, graph), hydrate, RRF
// fuse, sort. The hash embedder is deterministic and synchronous, so this
// isolates the Go-side fusion overhead.
func BenchmarkE2E_Recall_N20(b *testing.B) {
	benchRecallN(b, 20)
}

// BenchmarkE2E_Recall_N100 is the 100-episode store variant. The Cypher
// traversal latency grows non-linearly with graph density; measuring at two
// scales shows whether recall remains O(1) per query as the store grows.
func BenchmarkE2E_Recall_N100(b *testing.B) {
	benchRecallN(b, 100)
}

func benchRecallN(b *testing.B, seedN int) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := newBenchFixture(ctx, b)
	seedEpisodes(ctx, b, fx, seedN)

	// Drain async embeds so the vector lane has every episode indexed.
	// Without this the first few recall calls hit an empty vec index and
	// only the FTS lane fires; the steady-state per-query cost we want to
	// measure is the all-lanes path.
	if err := fx.orch.Log.WaitForBackgroundWork(ctx); err != nil {
		b.Fatalf("WaitForBackgroundWork (seed): %v", err)
	}

	recallTool := tools.NewRecall(fx.hybrid)
	args, _ := json.Marshal(map[string]any{
		"query": "Who works at Erebor?",
		"limit": 10,
	})

	b.ResetTimer()
	for b.Loop() {
		if _, err := recallTool.Invoke(ctx, args); err != nil {
			b.Fatalf("recall: %v", err)
		}
	}
}

// ---- BenchmarkE2E_Ask: full ask pipeline including compose ----

// BenchmarkE2E_Ask_N20 seeds 20 episodes then drives ask N times. The full
// path: recall (HybridQuery), context render, ChatProvider.Complete (stub,
// instant), Answer marshal. With the stub the LLM cost is zero; this isolates
// the Go-side glue of compose: prompt rendering + response parsing.
func BenchmarkE2E_Ask_N20(b *testing.B) {
	benchAskN(b, 20)
}

func benchAskN(b *testing.B, seedN int) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fx := newBenchFixture(ctx, b)
	seedEpisodes(ctx, b, fx, seedN)
	if err := fx.orch.Log.WaitForBackgroundWork(ctx); err != nil {
		b.Fatalf("WaitForBackgroundWork (seed): %v", err)
	}

	askTool := tools.NewAsk(fx.hybrid, fx.composer)
	args, _ := json.Marshal(map[string]any{
		"question": "Who works at Erebor?",
	})

	b.ResetTimer()
	for b.Loop() {
		if _, err := askTool.Invoke(ctx, args); err != nil {
			b.Fatalf("ask: %v", err)
		}
	}
}
