//go:build integration

package mcp_test

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/embedder"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp/mcptest"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// ---- fake embedders ----

// hashEmbedder is a content-hash deterministic embedder: same text always
// yields the same vector, different texts yield different vectors with high
// probability. Used for write-side seeding where vector quality does not
// affect correctness assertions.
type hashEmbedder struct{}

func (hashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
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

func (hashEmbedder) Model() string { return "fake-hash-embedder" }

// dirEmbedder maps query text containing "dog" to a unit vector on axis 1;
// everything else to axis 0 (the "cat" direction). Orthogonal unit vectors
// make cat/dog recall ranking unambiguous without depending on embedding
// quality. Used for the read side only.
type dirEmbedder struct {
	cat []float32
	dog []float32
}

func newDirEmbedder() dirEmbedder {
	cat := make([]float32, storage.VectorDim)
	cat[0] = 1.0
	dog := make([]float32, storage.VectorDim)
	dog[1] = 1.0
	return dirEmbedder{cat: cat, dog: dog}
}

func (d dirEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if strings.Contains(text, "dog") {
		return d.dog, nil
	}
	return d.cat, nil
}

func (dirEmbedder) Model() string { return "fake-dir-embedder" }

// ---- orchestrator factory ----

// newE2EOrchestrator builds a fully-wired orchestrator in dataDir with the
// Episode schema registered. It registers a test cleanup to close the
// orchestrator. Fails the test immediately on any construction error.
func newE2EOrchestrator(t *testing.T, ctx context.Context, dataDir string, emb embedder.Embedder) *orchestrator.Orchestrator {
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

// ---- recall query factory ----

// newHybridQuery opens a second sqlite.Store on the dispatcher's index.db,
// derives vector and FTS handles, and returns a kernel.HybridQuery bound to
// emb for query vectorisation. The additional sqlite.Store is read-only from
// the kernel.HybridQuery's perspective; SQLite WAL mode allows concurrent
// readers alongside the dispatcher's writer pool.
func newHybridQuery(t *testing.T, ctx context.Context, dataDir string, emb kernel.Embedder) kernel.Query {
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

// ---- scenario 1: bulk write + ranked recall ----

// TestE2E_BulkWriteRankedRecall seeds 50 episodes (25 "cat", 25 "dog")
// through the remember tool, then asserts that recall "cat" returns 10
// hits sorted by fusedScore descending, each with citations, and at least
// one nonzero FTS component. Mirrors scenario 1.
//
// Both write-side and read-side use dirEmbedder so the cat/dog vectors are
// orthogonal on both paths. Using hashEmbedder for writes and dirEmbedder
// for reads would produce random write-side vectors that don't align with
// the query axes, collapsing the vector signal. The recall_integration_test
// seeds its fixed five episodes directly via SQL+vec.Upsert with the same
// dirEmbedder, which is equivalent.
func TestE2E_BulkWriteRankedRecall(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	// dirEmbedder on the write side so cat episodes land on axis-0 vectors
	// and dog episodes on axis-1 vectors; the recall-side dirEmbedder then
	// produces matching query vectors and the ANN search separates them.
	orch := newE2EOrchestrator(t, ctx, dataDir, newDirEmbedder())

	// Seed phase: remember tool only.
	remSrv, remOut, remCleanup := mcptest.StartStdioServer(t, ctx, tools.NewRemember(orch.Log))
	remScanner := mcptest.NewScanner(remOut)
	mcptest.Handshake(t, remSrv, remScanner)

	// Episode texts use the singular "cat"/"dog" tokens so an FTS query
	// for "cat" or "dog" matches exactly. SQLite FTS5 tokenizes on word
	// boundaries without stemming, so "cats" would not match a "cat" query.
	for i := range 25 {
		text := fmt.Sprintf("the cat sat gracefully TOKEN_CAT_%03d", i)
		if err := mcptest.WriteJSONLine(remSrv, mcptest.JSONRPCToolsCall(i+2, "remember", map[string]any{
			"text":   text,
			"source": "e2e-bulk",
		})); err != nil {
			t.Fatalf("write remember cat %d: %v", i, err)
		}
		resp := mcptest.ReadJSONResponse(t, remScanner)
		if isErr, _ := resultOf(resp)["isError"].(bool); isErr {
			t.Fatalf("remember cat %d isError: %v", i, resp)
		}
	}
	for i := range 25 {
		text := fmt.Sprintf("the dog ran in the park TOKEN_DOG_%03d", i)
		if err := mcptest.WriteJSONLine(remSrv, mcptest.JSONRPCToolsCall(27+i, "remember", map[string]any{
			"text":   text,
			"source": "e2e-bulk",
		})); err != nil {
			t.Fatalf("write remember dog %d: %v", i, err)
		}
		resp := mcptest.ReadJSONResponse(t, remScanner)
		if isErr, _ := resultOf(resp)["isError"].(bool); isErr {
			t.Fatalf("remember dog %d isError: %v", i, resp)
		}
	}
	remCleanup()

	// Recall phase: direction embedder so cat/dog separation is sharp.
	dirEmb := newDirEmbedder()
	q := newHybridQuery(t, ctx, dataDir, dirEmb)
	recSrv, recOut, recCleanup := mcptest.StartStdioServer(t, ctx, tools.NewRecall(q))
	defer recCleanup()
	recScanner := mcptest.NewScanner(recOut)
	mcptest.Handshake(t, recSrv, recScanner)

	// cat query
	if err := mcptest.WriteJSONLine(recSrv, mcptest.JSONRPCToolsCall(2, "recall", map[string]any{
		"query": "cat",
		"limit": 10,
	})); err != nil {
		t.Fatalf("write recall cat: %v", err)
	}
	catHits := mustHits(t, mcptest.ReadJSONResponse(t, recScanner))
	if len(catHits) != 10 {
		t.Fatalf("cat hits = %d, want 10", len(catHits))
	}
	checkDescending(t, "cat", catHits)
	checkCitations(t, catHits)
	checkFTSPresent(t, "cat", catHits)
	for _, h := range catHits {
		snip, _ := h["snippet"].(string)
		if !strings.Contains(snip, "cat") && !strings.Contains(snip, "TOKEN_CAT") {
			t.Errorf("cat hit snippet %q does not mention cat", snip)
		}
	}

	// dog query
	if err := mcptest.WriteJSONLine(recSrv, mcptest.JSONRPCToolsCall(3, "recall", map[string]any{
		"query": "dog",
		"limit": 10,
	})); err != nil {
		t.Fatalf("write recall dog: %v", err)
	}
	dogHits := mustHits(t, mcptest.ReadJSONResponse(t, recScanner))
	if len(dogHits) != 10 {
		t.Fatalf("dog hits = %d, want 10", len(dogHits))
	}
	checkDescending(t, "dog", dogHits)
	checkCitations(t, dogHits)
	checkFTSPresent(t, "dog", dogHits)
	for _, h := range dogHits {
		snip, _ := h["snippet"].(string)
		if !strings.Contains(snip, "dog") && !strings.Contains(snip, "TOKEN_DOG") {
			t.Errorf("dog hit snippet %q does not mention dog", snip)
		}
	}
}

// ---- scenario 2: single round-trip ----

// TestE2E_RememberRecallRoundTrip seeds one episode with a unique token,
// then asserts recall returns at least one hit whose name matches the name
// remember returned. Mirrors scenario 2.
func TestE2E_RememberRecallRoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	orch := newE2EOrchestrator(t, ctx, dataDir, hashEmbedder{})

	remSrv, remOut, remCleanup := mcptest.StartStdioServer(t, ctx, tools.NewRemember(orch.Log))
	remScanner := mcptest.NewScanner(remOut)
	mcptest.Handshake(t, remSrv, remScanner)

	const token = "XYZZY_ROUNDTRIP_TOKEN"
	if err := mcptest.WriteJSONLine(remSrv, mcptest.JSONRPCToolsCall(2, "remember", map[string]any{
		"text":   "the wizard spoke the word " + token + " and the door opened",
		"source": "e2e-roundtrip",
	})); err != nil {
		t.Fatalf("write remember: %v", err)
	}
	episodeName := mustStructuredName(t, mcptest.ReadJSONResponse(t, remScanner))
	remCleanup()

	dirEmb := newDirEmbedder()
	q := newHybridQuery(t, ctx, dataDir, dirEmb)
	recSrv, recOut, recCleanup := mcptest.StartStdioServer(t, ctx, tools.NewRecall(q))
	defer recCleanup()
	recScanner := mcptest.NewScanner(recOut)
	mcptest.Handshake(t, recSrv, recScanner)

	if err := mcptest.WriteJSONLine(recSrv, mcptest.JSONRPCToolsCall(2, "recall", map[string]any{
		"query": token,
	})); err != nil {
		t.Fatalf("write recall: %v", err)
	}
	hits := mustHits(t, mcptest.ReadJSONResponse(t, recScanner))
	if len(hits) == 0 {
		t.Fatalf("recall returned 0 hits for unique token %q", token)
	}
	found := false
	for _, h := range hits {
		if name, _ := h["name"].(string); name == episodeName {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(hits))
		for _, h := range hits {
			names = append(names, fmt.Sprintf("%v", h["name"]))
		}
		t.Fatalf("recall hits %v do not include episode %q", names, episodeName)
	}
}

// ---- scenario 3: M1 update semantics ----

// TestE2E_M1UpdateSemantics seeds two episodes from the same source, the
// second containing SUPERSEDE_TOKEN. Asserts recall finds v2 as the top
// hit and that v1 is also independently retrievable (M1 has no dedup).
// Mirrors scenario 3.
func TestE2E_M1UpdateSemantics(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	orch := newE2EOrchestrator(t, ctx, dataDir, hashEmbedder{})

	remSrv, remOut, remCleanup := mcptest.StartStdioServer(t, ctx, tools.NewRemember(orch.Log))
	remScanner := mcptest.NewScanner(remOut)
	mcptest.Handshake(t, remSrv, remScanner)

	if err := mcptest.WriteJSONLine(remSrv, mcptest.JSONRPCToolsCall(2, "remember", map[string]any{
		"text":   "fact version 1 about the experiment",
		"source": "e2e-supersede",
	})); err != nil {
		t.Fatalf("write remember v1: %v", err)
	}
	v1Name := mustStructuredName(t, mcptest.ReadJSONResponse(t, remScanner))

	const superToken = "SUPERSEDE_TOKEN_E2E_M1"
	if err := mcptest.WriteJSONLine(remSrv, mcptest.JSONRPCToolsCall(3, "remember", map[string]any{
		"text":   "fact version 2 about the experiment " + superToken,
		"source": "e2e-supersede",
	})); err != nil {
		t.Fatalf("write remember v2: %v", err)
	}
	v2Name := mustStructuredName(t, mcptest.ReadJSONResponse(t, remScanner))
	remCleanup()

	if v1Name == v2Name {
		t.Fatalf("v1 name %q == v2 name; expected distinct ULIDs", v1Name)
	}

	dirEmb := newDirEmbedder()
	q := newHybridQuery(t, ctx, dataDir, dirEmb)
	recSrv, recOut, recCleanup := mcptest.StartStdioServer(t, ctx, tools.NewRecall(q))
	defer recCleanup()
	recScanner := mcptest.NewScanner(recOut)
	mcptest.Handshake(t, recSrv, recScanner)

	// Query with supersede token: top hit must mention "version 2".
	if err := mcptest.WriteJSONLine(recSrv, mcptest.JSONRPCToolsCall(2, "recall", map[string]any{
		"query": superToken,
	})); err != nil {
		t.Fatalf("write recall supersede: %v", err)
	}
	superHits := mustHits(t, mcptest.ReadJSONResponse(t, recScanner))
	if len(superHits) == 0 {
		t.Fatalf("recall returned 0 hits for %q", superToken)
	}
	if snip, _ := superHits[0]["snippet"].(string); !strings.Contains(snip, "version 2") {
		t.Errorf("top hit snippet %q does not contain \"version 2\"", snip)
	}

	// v1 must also be retrievable (no dedup at M1).
	if err := mcptest.WriteJSONLine(recSrv, mcptest.JSONRPCToolsCall(3, "recall", map[string]any{
		"query": "fact version 1",
	})); err != nil {
		t.Fatalf("write recall v1: %v", err)
	}
	v1Hits := mustHits(t, mcptest.ReadJSONResponse(t, recScanner))
	if len(v1Hits) == 0 {
		t.Fatalf("recall returned 0 hits for v1; both episodes must persist in M1")
	}
	foundV1 := false
	for _, h := range v1Hits {
		if name, _ := h["name"].(string); name == v1Name {
			foundV1 = true
			if snip, _ := h["snippet"].(string); !strings.Contains(stripHighlight(snip), "version 1") {
				t.Errorf("v1 hit snippet %q does not mention \"version 1\"", snip)
			}
			break
		}
	}
	if !foundV1 {
		names := make([]string, 0, len(v1Hits))
		for _, h := range v1Hits {
			names = append(names, fmt.Sprintf("%v", h["name"]))
		}
		t.Fatalf("v1 episode %q not found in recall hits %v; M1 persistence broken", v1Name, names)
	}
}

// ---- scenario 4: validation rejection ----

// TestE2E_ValidationRejection sends empty text to remember and empty query
// to recall over the wire and asserts both return isError:true. Mirrors
// scenario 4.
func TestE2E_ValidationRejection(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	orch := newE2EOrchestrator(t, ctx, dataDir, hashEmbedder{})

	// empty text -> remember must reject
	remSrv, remOut, remCleanup := mcptest.StartStdioServer(t, ctx, tools.NewRemember(orch.Log))
	defer remCleanup()
	remScanner := mcptest.NewScanner(remOut)
	mcptest.Handshake(t, remSrv, remScanner)

	if err := mcptest.WriteJSONLine(remSrv, mcptest.JSONRPCToolsCall(2, "remember", map[string]any{
		"text":   "",
		"source": "e2e-validation",
	})); err != nil {
		t.Fatalf("write remember empty text: %v", err)
	}
	remReject := mcptest.ReadJSONResponse(t, remScanner)
	if isErr, _ := resultOf(remReject)["isError"].(bool); !isErr {
		t.Fatalf("remember(text:'') should be isError; got %v", remReject)
	}

	// empty query -> recall must reject
	dirEmb := newDirEmbedder()
	q := newHybridQuery(t, ctx, dataDir, dirEmb)
	recSrv, recOut, recCleanup := mcptest.StartStdioServer(t, ctx, tools.NewRecall(q))
	defer recCleanup()
	recScanner := mcptest.NewScanner(recOut)
	mcptest.Handshake(t, recSrv, recScanner)

	if err := mcptest.WriteJSONLine(recSrv, mcptest.JSONRPCToolsCall(2, "recall", map[string]any{
		"query": "",
	})); err != nil {
		t.Fatalf("write recall empty query: %v", err)
	}
	recReject := mcptest.ReadJSONResponse(t, recScanner)
	if isErr, _ := resultOf(recReject)["isError"].(bool); !isErr {
		t.Fatalf("recall(query:'') should be isError; got %v", recReject)
	}
}

// ---- scenario 5: absent tool E_NOT_FOUND ----

// TestE2E_AbsentToolNotFound calls a tool name that was never registered
// and asserts the response carries isError:true. Mirrors scenario 5.
func TestE2E_AbsentToolNotFound(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dataDir := t.TempDir()
	orch := newE2EOrchestrator(t, ctx, dataDir, hashEmbedder{})

	srv, stdoutR, cleanup := mcptest.StartStdioServer(t, ctx, tools.NewRemember(orch.Log))
	defer cleanup()
	scanner := mcptest.NewScanner(stdoutR)
	mcptest.Handshake(t, srv, scanner)

	if err := mcptest.WriteJSONLine(srv, mcptest.JSONRPCToolsCall(2, "noSuchTool", nil)); err != nil {
		t.Fatalf("write noSuchTool: %v", err)
	}
	resp := mcptest.ReadJSONResponse(t, scanner)
	// mcp-go surfaces an unknown tool name as a JSON-RPC protocol error
	// (code -32602 Invalid Params) rather than an isError result envelope.
	// Either form is a valid error signal to the caller; we accept both so
	// the test is not brittle to library internals.
	protocolErr := resp["error"] != nil
	isErr, _ := resultOf(resp)["isError"].(bool)
	if !protocolErr && !isErr {
		t.Fatalf("noSuchTool should produce a protocol error or isError result; got %v", resp)
	}
}

// ---- local helpers ----

// resultOf returns the "result" object from a JSON-RPC response map, or an
// empty map when the field is absent.
func resultOf(resp map[string]any) map[string]any {
	if r, ok := resp["result"].(map[string]any); ok {
		return r
	}
	return map[string]any{}
}

// mustHits extracts the hits array from a recall response. Fails the test
// if the response is an error envelope or the hits field is missing.
func mustHits(t *testing.T, resp map[string]any) []map[string]any {
	t.Helper()
	result := resultOf(resp)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("recall result is error envelope: %v", result)
	}
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("result missing structuredContent: %v", result)
	}
	raw, ok := sc["hits"].([]any)
	if !ok {
		t.Fatalf("structuredContent.hits missing or wrong type: %v", sc)
	}
	out := make([]map[string]any, 0, len(raw))
	for i, h := range raw {
		m, ok := h.(map[string]any)
		if !ok {
			t.Fatalf("structuredContent.hits[%d] missing or wrong type: %v", i, h)
		}
		out = append(out, m)
	}
	return out
}

// mustStructuredName extracts structuredContent.name from a remember
// response. Fails the test if the field is absent or the result is an
// error envelope.
func mustStructuredName(t *testing.T, resp map[string]any) string {
	t.Helper()
	result := resultOf(resp)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("remember result is error envelope: %v", result)
	}
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("result missing structuredContent: %v", result)
	}
	name, ok := sc["name"].(string)
	if !ok || name == "" {
		t.Fatalf("structuredContent.name missing or empty: %v", sc)
	}
	return name
}

// checkDescending asserts fusedScore is non-increasing across hits.
func checkDescending(t *testing.T, label string, hits []map[string]any) {
	t.Helper()
	prev := 2.0
	for i, h := range hits {
		score, _ := h["fusedScore"].(float64)
		if score > prev {
			t.Errorf("%s: hit[%d] fusedScore %.4f > previous %.4f (not sorted)", label, i, score, prev)
		}
		prev = score
	}
}

// checkCitations asserts every hit carries at least one citation.
func checkCitations(t *testing.T, hits []map[string]any) {
	t.Helper()
	for _, h := range hits {
		name, _ := h["name"].(string)
		if cits, _ := h["citations"].([]any); len(cits) == 0 {
			t.Errorf("hit %q has no citations", name)
		}
	}
}

// stripHighlight removes the FTS5 snippet highlight tags (<b> and </b>) so
// assertions can match plain substrings without being sensitive to per-token
// wrapping. The tags are intentional product behaviour surfaced to callers;
// tests that want to assert on textual content should strip them here rather
// than in the production snippet producer.
func stripHighlight(s string) string {
	r := strings.NewReplacer("<b>", "", "</b>", "")
	return r.Replace(s)
}

// checkFTSPresent asserts that at least one hit in the set carries a
// nonzero FTS component score.
func checkFTSPresent(t *testing.T, label string, hits []map[string]any) {
	t.Helper()
	for _, h := range hits {
		if comps, _ := h["components"].(map[string]any); comps != nil {
			if v, _ := comps["fts"].(float64); v > 0 {
				return
			}
		}
	}
	t.Errorf("%s: no hit carried a nonzero FTS component", label)
}
