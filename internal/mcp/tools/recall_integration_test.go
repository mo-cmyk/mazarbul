//go:build integration

package tools_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp/mcptest"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// TestRecallTool_Integration_OverStdio is the AC-pinning end-to-end: it
// seeds five episodes through the same surfaces a production writer uses
// (episodes row + vec.Upsert; FTS5 follows via the AFTER INSERT trigger),
// wires kernel.HybridQuery into the recall tool, registers the tool on a
// real mcp.New server with stdio enabled, then drives a tools/call across
// an in-memory io.Pipe. The cat / dog split is deliberately stark so the
// test does not depend on embedding quality; it confirms the wiring
// (composer -> handler -> projection -> JSON-RPC envelope) is correct
// end-to-end.
func TestRecallTool_Integration_OverStdio(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := openTestStore(t, ctx)
	vec, err := sqlite.OpenVector(ctx, store)
	if err != nil {
		t.Fatalf("OpenVector: %v", err)
	}
	t.Cleanup(func() { _ = vec.Close() })

	fts, err := store.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	t.Cleanup(func() { _ = fts.Close() })

	emb := dirEmbedder{cat: unitVecAt(0), dog: unitVecAt(1)}

	episodes := []struct {
		uid  string
		text string
		dog  bool
	}{
		{uid: "01HX-CAT-0000000000000001", text: "cats are graceful creatures", dog: false},
		{uid: "01HX-CAT-0000000000000002", text: "the cat purred on the rug", dog: false},
		{uid: "01HX-CAT-0000000000000003", text: "training a cat is futile", dog: false},
		{uid: "01HX-DOG-0000000000000001", text: "dogs love to run in the park", dog: true},
		{uid: "01HX-DOG-0000000000000002", text: "the dog wagged its tail", dog: true},
	}

	now := time.Now().UTC()
	for i, e := range episodes {
		_, err := store.Writer().ExecContext(ctx,
			`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source) VALUES (?, ?, ?, ?, ?, ?)`,
			e.uid,
			now.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano),
			"remember",
			fmt.Sprintf(`{"text":%q,"confidence":1.0,"source":"test"}`, e.text),
			e.text,
			"test",
		)
		if err != nil {
			t.Fatalf("insert %q: %v", e.uid, err)
		}
		v := emb.cat
		if e.dog {
			v = emb.dog
		}
		if err := vec.Upsert(ctx, []kernel.VectorItem{{ID: e.uid, Embedding: v}}); err != nil {
			t.Fatalf("vec.Upsert %q: %v", e.uid, err)
		}
	}

	h, err := kernel.NewHybridQuery(emb, vec, fts, store.Reader())
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	stdinW, stdoutR, cleanup := mcptest.StartStdioServer(t, ctx, tools.NewRecall(h))
	defer cleanup()

	scanner := mcptest.NewScanner(stdoutR)
	mcptest.Handshake(t, stdinW, scanner)

	if err := mcptest.WriteJSONLine(stdinW, mcptest.JSONRPCToolsCall(2, "recall", map[string]any{"query": "cat", "limit": 3})); err != nil {
		t.Fatalf("write tools/call: %v", err)
	}
	resp := mcptest.ReadJSONResponse(t, scanner)
	hits := extractRecallHits(t, resp)
	if len(hits) != 3 {
		t.Fatalf("hits len = %d, want 3 (top cat episodes); resp=%v", len(hits), resp)
	}
	prevScore := 2.0
	for _, hit := range hits {
		name, _ := hit["name"].(string)
		if !strings.HasPrefix(name, "episodes/01HX-CAT-") {
			t.Errorf("hit %q is not a cat episode; ranking is wrong", name)
		}
		score, _ := hit["fusedScore"].(float64)
		if score > prevScore {
			t.Errorf("hits out of order: %v > previous %v", score, prevScore)
		}
		prevScore = score
		if _, ok := hit["createTime"].(string); !ok {
			t.Errorf("hit %q createTime missing or wrong type", name)
		}
		citations, _ := hit["citations"].([]any)
		if len(citations) == 0 {
			t.Errorf("hit %q has no citations", name)
		}
	}

	// At least one cat hit carries an FTS contribution. Without this
	// assertion the recall path could silently fall back to vec-only and
	// the test would still pass on the cat / dog stark split.
	sawFTS := false
	for _, hit := range hits {
		comps, _ := hit["components"].(map[string]any)
		if comps == nil {
			continue
		}
		if v, _ := comps["fts"].(float64); v > 0 {
			sawFTS = true
			break
		}
	}
	if !sawFTS {
		t.Error("no hit carried an FTS contribution; FTS subquery was effectively skipped")
	}

	// Schema-rejection path on the live wire: an empty query is rejected
	// by the precompiled InputSchema before the handler runs, so the
	// response is an isError envelope carrying E_VALIDATION. Pins that
	// the schema is actually attached to the registered tool, not just
	// declared.
	if err := mcptest.WriteJSONLine(stdinW, mcptest.JSONRPCToolsCall(3, "recall", map[string]any{"query": ""})); err != nil {
		t.Fatalf("write tools/call (empty query): %v", err)
	}
	rejectResp := mcptest.ReadJSONResponse(t, scanner)
	result, ok := rejectResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("empty-query response missing result object: %v", rejectResp)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("empty-query response should be isError envelope, got: %v", result)
	}
}

// ---- helpers local to this file ----

func openTestStore(t *testing.T, ctx context.Context) *sqlite.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(ctx, dir+"/recall-tool.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// dirEmbedder maps query text containing "cat" or "dog" to the matching
// unit vector. Inputs not matching either default to the cat direction so
// the empty-result test still produces well-formed vectors.
type dirEmbedder struct {
	cat []float32
	dog []float32
}

func (d dirEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if strings.Contains(text, "cat") {
		return d.cat, nil
	}
	if strings.Contains(text, "dog") {
		return d.dog, nil
	}
	return d.cat, nil
}

func unitVecAt(i int) []float32 {
	v := make([]float32, storage.VectorDim)
	if i >= 0 && i < storage.VectorDim {
		v[i] = 1.0
	}
	return v
}

func extractRecallHits(t *testing.T, resp map[string]any) []map[string]any {
	t.Helper()
	if errObj := resp["error"]; errObj != nil {
		t.Fatalf("tools/call response carries JSON-RPC error: %v", errObj)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result object: %v", resp)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tools/call result is error envelope: %v", result)
	}
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("result missing structuredContent: %v", result)
	}
	hits, ok := structured["hits"].([]any)
	if !ok {
		t.Fatalf("structuredContent.hits missing or wrong type: %v", structured)
	}
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		m, _ := h.(map[string]any)
		out = append(out, m)
	}
	return out
}
