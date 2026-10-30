//go:build integration

package tools_test

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
	"github.com/mo-cmyk/mazarbul/internal/kernel/embedder"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp/mcptest"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// ---- stub ChatProvider ----

// stubChatProvider is a deterministic kernel.ChatProvider for ask E2E tests.
// Complete always returns a fixed prose answer mentioning "Frodo" with a
// [1] citation marker. ToolUse is never called in the ask flow (the extractor
// is not wired in this test); it returns a zero ToolUseResponse so the stub
// satisfies the interface without triggering a test failure if called
// unexpectedly.
type stubChatProvider struct{}

func (stubChatProvider) Complete(_ context.Context, _ kernel.CompleteRequest) (kernel.CompleteResponse, error) {
	return kernel.CompleteResponse{
		Content:    "Frodo works at Erebor [1].",
		StopReason: "end_turn",
	}, nil
}

func (stubChatProvider) ToolUse(_ context.Context, _ kernel.ToolUseRequest) (kernel.ToolUseResponse, error) {
	return kernel.ToolUseResponse{StopReason: "end_turn"}, nil
}

// ---- local embedder helpers (package-local copies to avoid cross-package
// helper sharing; each test file is self-contained per test isolation policy)

type askHashEmbedder struct{}

func (askHashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	src := rand.NewPCG(h.Sum64(), 0)
	r := rand.New(src)
	out := make([]float32, storage.VectorDim)
	for i := range out {
		out[i] = r.Float32()
	}
	return out, nil
}

func (askHashEmbedder) Model() string { return "ask-e2e-hash-embedder" }

// ---- orchestrator factory ----

func newAskE2EOrchestrator(t *testing.T, ctx context.Context, dataDir string, emb embedder.Embedder) *orchestrator.Orchestrator {
	t.Helper()
	ont := kernel.NewMemoryOntology()
	if err := ontology.RegisterEpisode(ctx, ont); err != nil {
		t.Fatalf("RegisterEpisode: %v", err)
	}
	orch, err := orchestrator.New(ctx, orchestrator.Config{
		DataDir:  dataDir,
		Ontology: ont,
		Embedder: emb,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	t.Cleanup(func() {
		if err := orch.Close(); err != nil {
			t.Logf("orch.Close: %v", err)
		}
	})
	return orch
}

// ---- HybridQuery factory ----

func newAskHybridQuery(t *testing.T, ctx context.Context, dataDir string, emb kernel.Embedder) kernel.Query {
	t.Helper()
	sqlitePath := filepath.Join(dataDir, "index.db")
	sqlStore, err := sqlite.Open(ctx, sqlitePath)
	if err != nil {
		t.Fatalf("sqlite.Open %q: %v", sqlitePath, err)
	}
	t.Cleanup(func() {
		if err := sqlStore.Close(); err != nil {
			t.Logf("sqlStore.Close: %v", err)
		}
	})
	vec, err := sqlite.OpenVector(ctx, sqlStore)
	if err != nil {
		t.Fatalf("OpenVector: %v", err)
	}
	t.Cleanup(func() {
		if err := vec.Close(); err != nil {
			t.Logf("vec.Close: %v", err)
		}
	})
	fts, err := sqlStore.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	t.Cleanup(func() {
		if err := fts.Close(); err != nil {
			t.Logf("fts.Close: %v", err)
		}
	})
	q, err := kernel.NewHybridQuery(emb, vec, fts, sqlStore.Reader())
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}
	return q
}

// ---- composer factory ----

func newAskComposer(t *testing.T, ctx context.Context, chat kernel.ChatProvider) *composer.Composer {
	t.Helper()
	reg := prompts.New(nil)
	if err := composer.RegisterComposePrompt(ctx, reg); err != nil {
		t.Fatalf("RegisterComposePrompt: %v", err)
	}
	c, err := composer.New(
		composer.Config{Model: "stub-model"},
		chat,
		reg,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("composer.New: %v", err)
	}
	return c
}

// ---- scenario: ask returns grounded answer with citation ----

// TestE2E_AskGroundedAnswer seeds one episode containing "Frodo works at
// Erebor", then drives the ask tool end-to-end:
//
//  1. remember: seeds the episode via the remember MCP tool.
//  2. ask: calls the ask MCP tool with "Who works at Erebor?".
//  3. asserts the response unmarshals into composer.Answer, that Answer
//     contains "Frodo", and that Citations has at least one entry whose
//     EpisodeUID matches the seeded episode.
//
// The stubChatProvider returns a deterministic answer mentioning "Frodo"
// and a [1] citation so no real Ollama dependency is needed. Synchronisation
// is via the synchronous remember-then-ask tool call order (no time.Sleep).
func TestE2E_AskGroundedAnswer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	orch := newAskE2EOrchestrator(t, ctx, dataDir, askHashEmbedder{})

	// Seed phase: use the remember tool to write the episode.
	remSrv, remOut, remCleanup := mcptest.StartStdioServer(t, ctx, tools.NewRemember(orch.Log))
	remScanner := mcptest.NewScanner(remOut)
	mcptest.Handshake(t, remSrv, remScanner)

	if err := mcptest.WriteJSONLine(remSrv, mcptest.JSONRPCToolsCall(2, "remember", map[string]any{
		"text":   "Frodo works at Erebor",
		"source": "ask-e2e",
	})); err != nil {
		t.Fatalf("write remember: %v", err)
	}
	remResp := mcptest.ReadJSONResponse(t, remScanner)
	result := resultOfAsk(remResp)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("remember returned isError: %v", result)
	}
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("remember result missing structuredContent: %v", result)
	}
	episodeName, _ := sc["name"].(string)
	if episodeName == "" {
		t.Fatalf("remember structuredContent.name is empty")
	}
	// episodeName is "episodes/{uid}"; strip the prefix to get the raw UID.
	episodeUID := strings.TrimPrefix(episodeName, "episodes/")
	remCleanup()

	// Ask phase: build the ask tool with a stub composer.
	q := newAskHybridQuery(t, ctx, dataDir, askHashEmbedder{})
	chat := stubChatProvider{}
	comp := newAskComposer(t, ctx, chat)
	askTool := tools.NewAsk(q, comp)

	askSrv, askOut, askCleanup := mcptest.StartStdioServer(t, ctx, askTool)
	defer askCleanup()
	askScanner := mcptest.NewScanner(askOut)
	mcptest.Handshake(t, askSrv, askScanner)

	if err := mcptest.WriteJSONLine(askSrv, mcptest.JSONRPCToolsCall(2, "ask", map[string]any{
		"question": "Who works at Erebor?",
	})); err != nil {
		t.Fatalf("write ask: %v", err)
	}
	askResp := mcptest.ReadJSONResponse(t, askScanner)
	askResult := resultOfAsk(askResp)
	if isErr, _ := askResult["isError"].(bool); isErr {
		t.Fatalf("ask returned isError: %v", askResult)
	}

	// The ask tool returns the Answer directly as the result body. Extract the
	// text content from the MCP result envelope.
	rawAnswer := extractTextContent(t, askResult)

	var answer composer.Answer
	if err := json.Unmarshal([]byte(rawAnswer), &answer); err != nil {
		t.Fatalf("unmarshal Answer: %v (raw: %q)", err, rawAnswer)
	}

	if !strings.Contains(answer.Answer, "Frodo") {
		t.Errorf("answer.Answer %q does not mention \"Frodo\"", answer.Answer)
	}

	if len(answer.Citations) == 0 {
		t.Fatal("answer.Citations is empty; expected at least one citation")
	}

	found := false
	for _, c := range answer.Citations {
		if c.EpisodeUID == episodeUID {
			found = true
			break
		}
	}
	if !found {
		uids := make([]string, 0, len(answer.Citations))
		for _, c := range answer.Citations {
			uids = append(uids, c.EpisodeUID)
		}
		t.Errorf("seeded episode %q not found in citations %v", episodeUID, uids)
	}
}

// ---- scenario: ask returns validation error for empty question ----

// TestE2E_AskValidationRejection sends an empty question to the ask tool and
// asserts the response carries isError:true. Mirrors the validation test in
// remember_recall_e2e_test.go.
func TestE2E_AskValidationRejection(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	orch := newAskE2EOrchestrator(t, ctx, dataDir, askHashEmbedder{})

	q := newAskHybridQuery(t, ctx, dataDir, askHashEmbedder{})
	chat := stubChatProvider{}
	comp := newAskComposer(t, ctx, chat)
	askTool := tools.NewAsk(q, comp)

	// Ensure the orchestrator is cleanly closed even though we only use it for
	// the query setup side; avoid the unused variable lint error.
	_ = orch

	askSrv, askOut, cleanup := mcptest.StartStdioServer(t, ctx, askTool)
	defer cleanup()
	askScanner := mcptest.NewScanner(askOut)
	mcptest.Handshake(t, askSrv, askScanner)

	if err := mcptest.WriteJSONLine(askSrv, mcptest.JSONRPCToolsCall(2, "ask", map[string]any{
		"question": "",
	})); err != nil {
		t.Fatalf("write ask empty question: %v", err)
	}
	resp := mcptest.ReadJSONResponse(t, askScanner)
	protocolErr := resp["error"] != nil
	isErr, _ := resultOfAsk(resp)["isError"].(bool)
	if !protocolErr && !isErr {
		t.Fatalf("ask(question:'') should return protocol error or isError; got %v", resp)
	}
}

// ---- local helpers ----

// resultOfAsk mirrors resultOf from remember_recall_e2e_test.go. Duplicated
// here because the two files are in different packages (mcp_test vs tools_test)
// and each test file must be self-contained.
func resultOfAsk(resp map[string]any) map[string]any {
	if r, ok := resp["result"].(map[string]any); ok {
		return r
	}
	return map[string]any{}
}

// extractTextContent pulls the first text content item out of the MCP result
// envelope. The ask tool returns the Answer JSON as a text content item.
//
// MCP result shape for tools/call:
//
//	{ "result": { "content": [ { "type": "text", "text": "..." } ] } }
func extractTextContent(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result.content missing or empty: %v", result)
	}
	item, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("result.content[0] not an object: %v", content[0])
	}
	text, ok := item["text"].(string)
	if !ok || text == "" {
		t.Fatalf("result.content[0].text missing or empty: %v", item)
	}
	return text
}
