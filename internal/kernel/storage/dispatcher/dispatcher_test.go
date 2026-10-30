package dispatcher_test

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"math/rand"
	"runtime"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// fakeEmbedder returns a constant-length vector seeded from the input text
// so tests get deterministic but distinct embeddings without needing a real
// embedding provider. Its dim is configurable to exercise the wrong-size
// rejection path.
type fakeEmbedder struct {
	dim int
	err error
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	r := rand.New(rand.NewSource(int64(h.Sum64())))
	out := make([]float32, f.dim)
	for i := range out {
		out[i] = r.Float32()
	}
	return out, nil
}

// newDispatcher builds a Dispatcher rooted at a fresh temp dir with the
// given Embedder. Cleans up on test completion.
func newDispatcher(t *testing.T, ctx context.Context, embed dispatcher.Embedder) *dispatcher.Dispatcher {
	t.Helper()
	d, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  t.TempDir(),
		Embedder: embed,
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// makeEpisodeEvent builds a kernel.Event whose Payload is a valid Episode.
func makeEpisodeEvent(t *testing.T, uid, text, source string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"text":       text,
		"source":     source,
		"confidence": 0.9,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindEpisode),
		Source:     source,
		Confidence: 0.9,
		Payload:    payload,
	}
}

func TestApply_EmptySliceIsNoop(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	if err := d.Apply(ctx, nil); err != nil {
		t.Fatalf("Apply(nil): %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{}); err != nil {
		t.Fatalf("Apply(empty): %v", err)
	}

	assertEpisodeCount(t, ctx, d, 0)
}

func TestApply_SkipsNonEpisodeKinds(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	other := kernel.Event{
		UID:        "01HZZZZZZZZZZZZZZZZZZZZZZZ",
		Kind:       kernel.EventKind("recall_hit"),
		Source:     "test",
		Confidence: 1.0,
		Payload:    json.RawMessage(`{}`),
	}
	if err := d.Apply(ctx, []kernel.Event{other}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertEpisodeCount(t, ctx, d, 0)
}

func TestApply_NilEmbedderSkipsVectorLayer(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, nil)

	ev := makeEpisodeEvent(t, "01HAAAAAAAAAAAAAAAAAAAAAAA", "hello world", "cli:test")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	assertEpisodeCount(t, ctx, d, 1)
	assertVectorCount(t, ctx, d, 0)
	assertEpisodeNodeCount(t, ctx, d, 1)
}

// TestApply_WrongDimEmbedding_AsyncDoesNotFailApply pins the new contract
// for episodes: a wrong-dim embedding never fails Apply. The episode row +
// graph node land synchronously and remain durable; the background embed
// worker observes the wrong dimension and warn-logs without writing vec0.
// The previous contract surfaced ErrEmbeddingDim from Apply itself, which
// was correct when embedding was inline but is impossible to honour once
// the embed has been pushed off the hot path. Recall via FTS still finds
// the episode; recall via the vector lane does not. Rebuild can reconcile
// once the embedder is fixed.
func TestApply_WrongDimEmbedding_AsyncDoesNotFailApply(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim + 1})

	ev := makeEpisodeEvent(t, "01HBBBBBBBBBBBBBBBBBBBBBBB", "bad dim", "cli:test")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v (must not fail on async dim mismatch)", err)
	}

	// Wait for the background worker so the warn-log fires before the
	// test ends; otherwise the assertion races with the goroutine.
	if err := d.WaitForBackgroundVec(ctx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	assertEpisodeCount(t, ctx, d, 1)
	assertVectorCount(t, ctx, d, 0)
	assertEpisodeNodeCount(t, ctx, d, 1)
}

func TestApply_MalformedPayload(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	ev := kernel.Event{
		UID:     "01HCCCCCCCCCCCCCCCCCCCCCCC",
		Kind:    kernel.EventKind(ontology.KindEpisode),
		Payload: json.RawMessage(`not valid json`),
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err == nil {
		t.Fatal("Apply: expected error for malformed payload, got nil")
	}

	assertEpisodeCount(t, ctx, d, 0)
	assertEpisodeNodeCount(t, ctx, d, 0)
}

// TestApply_EmbedderErrorAsyncDoesNotFailApply pins the new contract: a
// failing embedder never fails the synchronous Apply. The episode lands
// durably; the background worker warn-logs and skips the vec0 write. This
// is intentional: the embedder is a network round-trip, and a slow / down
// provider must not block the writer or surface as a remember() failure.
// The eval feeder used to time out on intermittent Ollama hiccups under
// the old shape; the new shape keeps remember fast and lets Rebuild fill
// vec0 once the embedder recovers.
func TestApply_EmbedderErrorAsyncDoesNotFailApply(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	want := errors.New("embedder offline")
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim, err: want})

	ev := makeEpisodeEvent(t, "01HDDDDDDDDDDDDDDDDDDDDDDD", "x", "cli:test")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply: %v (must not fail on async embedder error)", err)
	}

	if err := d.WaitForBackgroundVec(ctx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	assertEpisodeCount(t, ctx, d, 1)
	assertVectorCount(t, ctx, d, 0)
	assertEpisodeNodeCount(t, ctx, d, 1)
}

func TestOpen_RejectsEmptyDataDir(t *testing.T) {
	t.Parallel()
	if _, err := dispatcher.Open(testutil.TestContext(t), dispatcher.Config{}); err == nil {
		t.Fatal("expected error for empty DataDir, got nil")
	}
}

// TestOpenGraph_BorrowedCloseIsNoop guards against a read-side caller
// closing the dispatcher-owned graph handle. The interface contract
// returned by OpenGraph must not propagate Close to the underlying
// LadybugDB connection; otherwise the next Apply fails with ErrClosed.
// (Greptile #95 P1.)
func TestOpenGraph_BorrowedCloseIsNoop(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	g, err := d.OpenGraph(ctx)
	if err != nil {
		t.Fatalf("OpenGraph: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("borrowed Close: %v", err)
	}

	// The dispatcher must still be able to write through the graph layer.
	ev := makeEpisodeEvent(t, "01HXXXXXXXXXXXXXXXXXXXXXXX", "still works", "cli:test")
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply after borrowed graph Close: %v", err)
	}
	assertEpisodeNodeCount(t, ctx, d, 1)
}

// TestClose_RaceWithApply exercises the lifecycle synchronisation between
// Apply (which reads d.sqlite / d.graph) and Close (which zeroes them).
// Run with -race; without the lifecycle mutex, the data race fires.
// (Greptile #95 P1.)
func TestClose_RaceWithApply(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  t.TempDir(),
		Embedder: &fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ev := makeEpisodeEvent(t, "01HRACERACERACERACERACERAC", "race target", "cli:test")
		// Either succeeds (Close lost the race) or returns a clean
		// closed-dispatcher error; both are acceptable. The contract is
		// no data race and no panic.
		_ = d.Apply(ctx, []kernel.Event{ev})
	}()
	runtime.Gosched()
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-done
}

func TestClose_IsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	dir := t.TempDir()
	d, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{makeEpisodeEvent(t, "01HEEEEEEEEEEEEEEEEEEEEEEE", "x", "cli:test")}); err == nil {
		t.Fatal("Apply after Close: expected error, got nil")
	}
}

// -- helpers -----------------------------------------------------------------

func assertEpisodeCount(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, want int) {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var got int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM episodes`).Scan(&got); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if got != want {
		t.Errorf("episodes count = %d, want %d", got, want)
	}
}

func assertVectorCount(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, want int) {
	t.Helper()
	db, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	var got int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM episodes_vec_ids`).Scan(&got); err != nil {
		t.Fatalf("count episodes_vec_ids: %v", err)
	}
	if got != want {
		t.Errorf("episodes_vec_ids count = %d, want %d", got, want)
	}
}

func assertEpisodeNodeCount(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, want int64) {
	t.Helper()
	g, err := d.OpenGraph(ctx)
	if err != nil {
		t.Fatalf("OpenGraph: %v", err)
	}
	r, err := g.Execute(ctx, "MATCH (e:Episode) RETURN count(e)", nil)
	if err != nil {
		t.Fatalf("count episode nodes: %v", err)
	}
	defer func() { _ = r.Close() }()
	if !r.Next() {
		if err := r.Err(); err != nil {
			t.Fatalf("episode node iter: %v", err)
		}
		t.Fatal("episode node count: zero rows")
	}
	var got int64
	if err := r.Scan(&got); err != nil {
		t.Fatalf("scan episode node count: %v", err)
	}
	if got != want {
		t.Errorf("episode node count = %d, want %d", got, want)
	}
}
