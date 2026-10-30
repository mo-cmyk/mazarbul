package resolver

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// countingEmbedder is a kernel.Embedder + embedder.Batcher stub that returns
// a deterministic per-text vector and counts call shapes, so tests can pin
// "one batch, zero singles" against "N singles".
type countingEmbedder struct {
	embedCalls int
	batchCalls int
	batchErr   error
	lastBatch  []string
}

// vecFor derives a stable fake vector from the text so identity checks work.
func vecFor(text string) []float32 {
	return []float32{float32(len(text)), float32(text[0])}
}

func (c *countingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	c.embedCalls++
	return vecFor(text), nil
}

func (c *countingEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	c.batchCalls++
	c.lastBatch = append([]string(nil), texts...)
	if c.batchErr != nil {
		return nil, c.batchErr
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = vecFor(t)
	}
	return out, nil
}

// singleEmbedder implements only kernel.Embedder (no Batcher).
type singleEmbedder struct{ embedCalls int }

func (s *singleEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	s.embedCalls++
	return vecFor(text), nil
}

func testResolverWith(e any) *Resolver {
	r := &Resolver{logger: zap.NewNop()}
	switch v := e.(type) {
	case *countingEmbedder:
		r.embedder = v
	case *singleEmbedder:
		r.embedder = v
	}
	return r
}

// meaningfulMention builds a mention whose summary text passes
// mentionHasMeaningfulSummary.
func meaningfulMention(name, ctx string) ontology.Mention {
	return ontology.Mention{
		Kind:              ontology.EntityKind("person"),
		DisplayName:       name,
		ExtractionText:    name,
		DisambiguationCtx: ctx,
	}
}

func TestPreEmbedMentions_OneBatchCoversNamesAndSummaries(t *testing.T) {
	t.Parallel()
	stub := &countingEmbedder{}
	r := testResolverWith(stub)

	mentions := []ontology.Mention{
		meaningfulMention("Merry Brandybuck", "robotics engineer at Mithril"),
		meaningfulMention("Mithril Robotics", "robotics company in Zurich"),
		// Duplicate name with no meaningful summary: must dedupe, not re-add.
		{Kind: ontology.EntityKind("person"), DisplayName: "Merry Brandybuck"},
	}
	pre := r.preEmbedMentions(context.Background(), mentions)
	if pre == nil {
		t.Fatal("preEmbedMentions returned nil with a Batcher available")
	}
	if stub.batchCalls != 1 || stub.embedCalls != 0 {
		t.Fatalf("calls = %d batch / %d single, want 1/0", stub.batchCalls, stub.embedCalls)
	}
	// 2 unique names + 2 summary texts.
	if len(stub.lastBatch) != 4 {
		t.Fatalf("batched %d texts (%v), want 4", len(stub.lastBatch), stub.lastBatch)
	}

	// embedName/embedSummary must consume the batch without single calls
	// and return the identical vectors the single path would produce.
	vec, err := r.embedName(context.Background(), mentions[0], pre)
	if err != nil {
		t.Fatalf("embedName: %v", err)
	}
	if !reflect.DeepEqual(vec, vecFor("Merry Brandybuck")) {
		t.Errorf("embedName vector mismatch: %v", vec)
	}
	sumVec, err := r.embedSummary(context.Background(), mentions[1], pre)
	if err != nil {
		t.Fatalf("embedSummary: %v", err)
	}
	if !reflect.DeepEqual(sumVec, vecFor(buildMentionSummaryText(mentions[1]))) {
		t.Errorf("embedSummary vector mismatch: %v", sumVec)
	}
	if stub.embedCalls != 0 {
		t.Errorf("single Embed calls after batch = %d, want 0", stub.embedCalls)
	}
}

func TestPreEmbedMentions_NonBatcherFallsBackToSingles(t *testing.T) {
	t.Parallel()
	stub := &singleEmbedder{}
	r := testResolverWith(stub)

	mentions := []ontology.Mention{
		meaningfulMention("Merry Brandybuck", "robotics engineer at Mithril"),
	}
	if pre := r.preEmbedMentions(context.Background(), mentions); pre != nil {
		t.Fatal("expected nil preEmbeds without a Batcher")
	}
	// nil pre is valid: embed paths fall back to single calls.
	if _, err := r.embedName(context.Background(), mentions[0], nil); err != nil {
		t.Fatalf("embedName fallback: %v", err)
	}
	if stub.embedCalls != 1 {
		t.Errorf("single calls = %d, want 1", stub.embedCalls)
	}
}

func TestPreEmbedMentions_BatchErrorFallsBack(t *testing.T) {
	t.Parallel()
	stub := &countingEmbedder{batchErr: errors.New("engine busy")}
	r := testResolverWith(stub)

	mentions := []ontology.Mention{
		meaningfulMention("Merry Brandybuck", "robotics engineer at Mithril"),
		meaningfulMention("Mithril Robotics", "robotics company in Zurich"),
	}
	if pre := r.preEmbedMentions(context.Background(), mentions); pre != nil {
		t.Fatal("expected nil preEmbeds on batch error")
	}
	if _, err := r.embedName(context.Background(), mentions[0], nil); err != nil {
		t.Fatalf("embedName fallback: %v", err)
	}
	if stub.embedCalls != 1 {
		t.Errorf("fallback single calls = %d, want 1", stub.embedCalls)
	}
}

func TestPreEmbedMentions_SingleTextSkipsBatch(t *testing.T) {
	t.Parallel()
	stub := &countingEmbedder{}
	r := testResolverWith(stub)

	// One mention, no meaningful summary -> one unique text -> batching
	// gains nothing and must be skipped.
	mentions := []ontology.Mention{{Kind: ontology.EntityKind("person"), DisplayName: "Bo"}}
	if pre := r.preEmbedMentions(context.Background(), mentions); pre != nil {
		t.Fatal("expected nil preEmbeds for a single unique text")
	}
	if stub.batchCalls != 0 {
		t.Errorf("batch calls = %d, want 0", stub.batchCalls)
	}
}

func TestPreEmbedsLookup_NilSafe(t *testing.T) {
	t.Parallel()
	var pre *preEmbeds
	if _, ok := pre.lookup("anything"); ok {
		t.Fatal("nil preEmbeds must miss")
	}
}
