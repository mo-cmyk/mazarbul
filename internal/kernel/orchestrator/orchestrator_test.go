package orchestrator_test

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/embedder"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// fakeEmbedder returns a deterministic constant-dimension vector. Same
// shape as dispatcher_test.go's fakeEmbedder but exported via the
// embedder.Embedder interface so the orchestrator can install it.
type fakeEmbedder struct {
	model string
	dim   int
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
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

func (f *fakeEmbedder) Model() string { return f.model }

func newOntology(t *testing.T) kernel.Ontology {
	t.Helper()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterEpisode(testutil.TestContext(t), o); err != nil {
		t.Fatalf("RegisterEpisode: %v", err)
	}
	return o
}

func sampleEpisode(t *testing.T, text string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"text":       text,
		"source":     "orchestrator_test",
		"confidence": 0.9,
	})
	if err != nil {
		t.Fatalf("marshal episode: %v", err)
	}
	return kernel.Event{
		Kind:       ontology.KindEpisode,
		Source:     "orchestrator_test",
		Confidence: 0.9,
		Payload:    payload,
	}
}

func TestNew_RequiresDataDir(t *testing.T) {
	t.Parallel()
	_, err := orchestrator.New(testutil.TestContext(t), orchestrator.Config{
		Ontology: newOntology(t),
	})
	if err == nil {
		t.Fatal("expected error for empty DataDir, got nil")
	}
}

func TestNew_RequiresOntology(t *testing.T) {
	t.Parallel()
	_, err := orchestrator.New(testutil.TestContext(t), orchestrator.Config{
		DataDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for nil Ontology, got nil")
	}
}

func TestNew_NilEmbedder_SkipsCache(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	o, err := orchestrator.New(ctx, orchestrator.Config{
		DataDir:  t.TempDir(),
		Ontology: newOntology(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = o.Close() })

	if o.Embedder != nil {
		t.Errorf("Embedder must be nil when Config.Embedder is nil; got %T", o.Embedder)
	}
	if o.Log == nil {
		t.Error("Log must be non-nil even when Embedder is nil")
	}
	if o.Storage == nil {
		t.Error("Storage must be non-nil")
	}
}

func TestNew_WithEmbedder_WiresCache(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dir := t.TempDir()
	o, err := orchestrator.New(ctx, orchestrator.Config{
		DataDir:  dir,
		Ontology: newOntology(t),
		Embedder: &fakeEmbedder{model: "fake-bge-m3", dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = o.Close() })

	if o.Embedder == nil {
		t.Fatal("Embedder must be the CachingEmbedder, got nil")
	}
	// CachingEmbedder propagates its inner Model(), so the model
	// identifier should round-trip.
	if got := o.Embedder.Model(); got != "fake-bge-m3" {
		t.Errorf("Embedder.Model() = %q, want %q", got, "fake-bge-m3")
	}

	// The Log must be wired against the same Storage: an Append goes
	// all the way through to the dispatcher.
	receipts, err := o.Log.Append(ctx, []kernel.Event{sampleEpisode(t, "orchestrator wires up")})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("Append receipts: got %d, want 1", len(receipts))
	}

	// The events JSONL must exist at the expected layout.
	matches, err := filepath.Glob(filepath.Join(dir, "events", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) == 0 {
		t.Errorf("expected at least one JSONL day file under %s, found none", dir)
	}
}

func TestClose_Idempotent(t *testing.T) {
	t.Parallel()
	o, err := orchestrator.New(testutil.TestContext(t), orchestrator.Config{
		DataDir:  t.TempDir(),
		Ontology: newOntology(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close must be a no-op; the closer slice was cleared.
	if err := o.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Compile-time check that fakeEmbedder satisfies the embedder.Embedder
// contract; if it ever drifts, this fails at build time, not at the
// test invocation site.
var _ embedder.Embedder = (*fakeEmbedder)(nil)
