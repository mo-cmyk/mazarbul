//go:build integration

package tools_test

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/mcp/mcptest"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// rememberNamePattern matches the `episodes/{ulid}` AIP resource-name
// format that the remember response surfaces. Duplicates the pattern
// declared on ontology.Episode.Name and the one in remember.go's
// OutputSchema so a drift between the three is loud, not silent.
var rememberNamePattern = regexp.MustCompile(`^episodes/[0-9A-HJKMNP-TV-Z]{26}$`)

// TestRememberTool_Integration_OverStdio is the issue #43 AC #4 pin:
// drive a tools/call remember across an in-memory io.Pipe through a
// fully wired orchestrator (FileEventLog + four-layer dispatcher +
// fake embedder), then assert the event landed in every derived store.
// This is the write-side counterpart to TestRecallTool_Integration in
// recall_integration_test.go, sharing the same JSON-RPC harness.
func TestRememberTool_Integration_OverStdio(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dir := t.TempDir()

	ont := kernel.NewMemoryOntology()
	if err := ontology.RegisterEpisode(ctx, ont); err != nil {
		t.Fatalf("RegisterEpisode: %v", err)
	}

	emb := &rememberFakeEmbedder{model: "fake-bge-m3", dim: storage.VectorDim}

	orch, err := orchestrator.New(ctx, orchestrator.Config{
		DataDir:  dir,
		Ontology: ont,
		Embedder: emb,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	t.Cleanup(func() { _ = orch.Close() })

	stdinW, stdoutR, cleanup := mcptest.StartStdioServer(t, ctx, tools.NewRemember(orch.Log))
	defer cleanup()

	scanner := mcptest.NewScanner(stdoutR)
	mcptest.Handshake(t, stdinW, scanner)

	const wantText = "the cat sat on the mat carrying TOKEN_UNIQUE_REMEMBER"
	if err := mcptest.WriteJSONLine(stdinW, mcptest.JSONRPCToolsCall(2, "remember", map[string]any{
		"text":       wantText,
		"source":     "integration-test",
		"confidence": 0.9,
	})); err != nil {
		t.Fatalf("write tools/call: %v", err)
	}
	resp := mcptest.ReadJSONResponse(t, scanner)
	name := extractRememberName(t, resp)
	if !rememberNamePattern.MatchString(name) {
		t.Fatalf("response name %q does not match %s", name, rememberNamePattern)
	}
	uid := strings.TrimPrefix(name, "episodes/")

	// JSONL layer: exactly one event, payload matches the request. Read
	// through the kernel.EventLog.Read iterator so the test pins the
	// documented contract rather than the on-disk layout (events/YYYY/...
	// is an implementation detail of FileEventLog).
	events := drainEventLog(t, ctx, orch.Log)
	if len(events) != 1 {
		t.Fatalf("event log line count = %d, want 1", len(events))
	}
	if events[0].UID != uid {
		t.Errorf("event uid = %q, want %q", events[0].UID, uid)
	}
	if events[0].Kind != ontology.KindEpisode {
		t.Errorf("event kind = %q, want %q", events[0].Kind, ontology.KindEpisode)
	}
	var ep ontology.Episode
	if err := json.Unmarshal(events[0].Payload, &ep); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if ep.Text != wantText {
		t.Errorf("event payload text = %q, want %q", ep.Text, wantText)
	}
	if ep.Source != "integration-test" {
		t.Errorf("event payload source = %q", ep.Source)
	}
	if ep.Confidence != 0.9 {
		t.Errorf("event payload confidence = %v", ep.Confidence)
	}

	// SQLite relational: exactly one row, with the input text/source.
	db, err := orch.Storage.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var gotText, gotSource string
	if err := db.QueryRowContext(ctx,
		`SELECT text, source FROM episodes WHERE uid = ?`, uid,
	).Scan(&gotText, &gotSource); err != nil {
		t.Fatalf("SELECT episodes: %v", err)
	}
	if gotText != wantText {
		t.Errorf("episodes.text = %q, want %q", gotText, wantText)
	}
	if gotSource != "integration-test" {
		t.Errorf("episodes.source = %q", gotSource)
	}

	// Vector layer: the dispatcher upserted into episodes_vec_ids before
	// commit, so a sidecar row must exist for the new uid.
	var vecRows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM episodes_vec_ids WHERE id = ?`, uid,
	).Scan(&vecRows); err != nil {
		t.Fatalf("SELECT episodes_vec_ids: %v", err)
	}
	if vecRows != 1 {
		t.Errorf("episodes_vec_ids row count for %q = %d, want 1", uid, vecRows)
	}

	// FTS5 layer: search for the unique token threaded through the
	// payload text. The AFTER INSERT trigger on episodes populates the
	// FTS index inside the same SQLite tx as the row write, so the hit
	// is visible immediately.
	fts, err := orch.Storage.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	ftsHits, err := fts.Search(ctx, "TOKEN_UNIQUE_REMEMBER", 5)
	if err != nil {
		t.Fatalf("fts.Search: %v", err)
	}
	if len(ftsHits) != 1 {
		t.Fatalf("fts hits = %d, want 1; hits=%v", len(ftsHits), ftsHits)
	}
	if ftsHits[0].ID != uid {
		t.Errorf("fts hit ID = %q, want %q", ftsHits[0].ID, uid)
	}

	// A second remember call must produce a different name (no dedup at
	// this layer) and ULIDs must remain monotonic.
	if err := mcptest.WriteJSONLine(stdinW, mcptest.JSONRPCToolsCall(3, "remember", map[string]any{
		"text":   "a second remember on the same source",
		"source": "integration-test",
	})); err != nil {
		t.Fatalf("write tools/call 2: %v", err)
	}
	resp2 := mcptest.ReadJSONResponse(t, scanner)
	name2 := extractRememberName(t, resp2)
	if name2 == name {
		t.Errorf("second remember reused name %q; expected a fresh ULID", name2)
	}
	if name2 <= name {
		t.Errorf("ULIDs not monotonic: second %q <= first %q", name2, name)
	}

	// Schema-rejection on the live wire: an empty `text` is rejected by
	// the precompiled InputSchema before the handler runs, so the
	// response is an isError envelope. Pins that the validator is
	// actually attached to the registered tool.
	if err := mcptest.WriteJSONLine(stdinW, mcptest.JSONRPCToolsCall(4, "remember", map[string]any{
		"text":   "",
		"source": "integration-test",
	})); err != nil {
		t.Fatalf("write tools/call (empty text): %v", err)
	}
	reject := mcptest.ReadJSONResponse(t, scanner)
	result, ok := reject["result"].(map[string]any)
	if !ok {
		t.Fatalf("empty-text response missing result object: %v", reject)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("empty-text response should be isError envelope, got: %v", result)
	}
}

// rememberFakeEmbedder is a content-hashed deterministic embedder. Same
// shape as the orchestrator_test fakeEmbedder; duplicated here so the
// tools integration test does not depend on a sibling test package.
type rememberFakeEmbedder struct {
	model string
	dim   int
}

func (f *rememberFakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	src := rand.NewPCG(h.Sum64(), 0)
	r := rand.New(src)
	out := make([]float32, f.dim)
	for i := range out {
		out[i] = r.Float32()
	}
	return out, nil
}

func (f *rememberFakeEmbedder) Model() string { return f.model }

// extractRememberName drains the tools/call response into the
// `result.structuredContent.name` string. Fails the test if any link
// in the unwrap chain is missing or the wrong type. Mirrors
// extractRecallHits's defensive style.
func extractRememberName(t *testing.T, resp map[string]any) string {
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
	name, ok := structured["name"].(string)
	if !ok {
		t.Fatalf("structuredContent.name missing or wrong type: %v", structured)
	}
	return name
}

// drainEventLog reads every event the log holds via the documented
// kernel.EventLog.Read iterator. Using the contract (rather than
// globbing events/YYYY/MM/DD.jsonl) means the assertion survives any
// future change to the on-disk layout.
func drainEventLog(t *testing.T, ctx context.Context, log *kernel.FileEventLog) []kernel.Event {
	t.Helper()
	out := make([]kernel.Event, 0, 8)
	for ev, err := range log.Read(ctx, kernel.ReadOpts{}) {
		if err != nil {
			t.Fatalf("eventlog.Read: %v", err)
		}
		out = append(out, ev)
	}
	return out
}
