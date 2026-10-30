//go:build integration

package kernel_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// TestHybridQuery_Integration_CatVsDog exercises the full M1 recall cycle
// against a real SQLite store with sqlite-vec and FTS5 attached. Five
// episodes are written through the same surfaces a production writer uses
// (episodes table + vec.Upsert; FTS is populated by the AFTER INSERT
// trigger). The composer's job is to embed the query, run both
// sub-retrievers, fuse via RRF, and return the three cat episodes ranked
// above the two dog episodes.
//
// The cat/dog split is deliberately stark so the test is not a vector-
// quality benchmark; it confirms the wiring (parallel retrieval, hydration,
// filter ordering, RRF projection) is correct end-to-end.
func TestHybridQuery_Integration_CatVsDog(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	store := openStoreForIntegration(t, ctx)
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

	// Embedder maps queries containing "cat" to the cat unit-vector and
	// queries containing "dog" to the dog unit-vector. The composer never
	// inspects the embedding's content; the stub just produces vectors
	// whose L2 distance to the seeded cat / dog vectors is small.
	emb := dirEmbedder{
		cat: unitVecAt(0),
		dog: unitVecAt(1),
	}

	type ep struct {
		uid  string
		text string
		dog  bool // true for dog episode, false for cat
	}
	episodes := []ep{
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

	hits := drainIntegration(t, h.Recall(ctx, kernel.RecallRequest{
		Query: "cat",
		Limit: 3,
	}))
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	for _, hit := range hits {
		if hit.Kind != "remember" {
			t.Errorf("hit %q kind = %q, want remember", hit.UID, hit.Kind)
		}
		if hit.Confidence != 1.0 {
			t.Errorf("hit %q confidence = %v, want 1.0", hit.UID, hit.Confidence)
		}
		if hit.FusedScore <= 0 {
			t.Errorf("hit %q fused score = %v, want > 0", hit.UID, hit.FusedScore)
		}
		if len(hit.Citations) != 1 || hit.Citations[0] != "episodes/"+hit.UID {
			t.Errorf("hit %q citations = %v", hit.UID, hit.Citations)
		}
		if !containsAny(hit.UID, "CAT") {
			t.Errorf("hit %q is not a cat episode; ranking is wrong", hit.UID)
		}
	}

	// Top three are sorted descending by fused score.
	for i := 1; i < len(hits); i++ {
		if hits[i].FusedScore > hits[i-1].FusedScore {
			t.Errorf("hit %d score %v > hit %d score %v (not sorted)",
				i, hits[i].FusedScore, i-1, hits[i-1].FusedScore)
		}
	}

	// At least one of the top hits carries an FTS snippet (cats appear in
	// every cat episode's text); the FTS snippet wraps the matched token in
	// <b>...</b>.
	sawFTS := false
	for _, hit := range hits {
		if hit.Snippet != "" && hit.Components.FTS > 0 {
			sawFTS = true
			break
		}
	}
	if !sawFTS {
		t.Error("no hit carried an FTS contribution; FTS subquery was effectively skipped")
	}
}

// TestHybridQuery_Integration_KindsFilter confirms that the Kinds filter
// applies at the SQL layer (via payload hydration) on a real store.
func TestHybridQuery_Integration_KindsFilter(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	store := openStoreForIntegration(t, ctx)
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
		kind kernel.EventKind
		text string
	}{
		{uid: "01HX-RM-00000000000000001", kind: "remember", text: "remember the cat"},
		{uid: "01HX-SS-00000000000000001", kind: "supersede", text: "supersede the cat memory"},
		{uid: "01HX-RM-00000000000000002", kind: "remember", text: "another cat memory"},
	}

	now := time.Now().UTC()
	for i, e := range episodes {
		_, err := store.Writer().ExecContext(ctx,
			`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source) VALUES (?, ?, ?, ?, ?, ?)`,
			e.uid,
			now.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano),
			string(e.kind),
			fmt.Sprintf(`{"text":%q,"confidence":1.0,"source":"test"}`, e.text),
			e.text,
			"test",
		)
		if err != nil {
			t.Fatalf("insert %q: %v", e.uid, err)
		}
		if err := vec.Upsert(ctx, []kernel.VectorItem{{ID: e.uid, Embedding: emb.cat}}); err != nil {
			t.Fatalf("vec.Upsert %q: %v", e.uid, err)
		}
	}

	h, err := kernel.NewHybridQuery(emb, vec, fts, store.Reader())
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	hits := drainIntegration(t, h.Recall(ctx, kernel.RecallRequest{
		Query: "cat",
		Kinds: []kernel.EventKind{"remember"},
	}))
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 (only remember kinds)", len(hits))
	}
	for _, hit := range hits {
		if hit.Kind != "remember" {
			t.Errorf("hit %q kind = %q, want remember", hit.UID, hit.Kind)
		}
	}
}

// ---- helpers ---------------------------------------------------------------

func openStoreForIntegration(t *testing.T, ctx context.Context) *sqlite.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(ctx, dir+"/integration.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// dirEmbedder maps query strings to one of two seeded direction vectors. The
// composer only needs Embed; the test never calls anything else.
type dirEmbedder struct {
	cat []float32
	dog []float32
}

func (d dirEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if containsAny(text, "cat") {
		return d.cat, nil
	}
	if containsAny(text, "dog") {
		return d.dog, nil
	}
	// Default to cat direction so tests with neutral queries still
	// produce hits rather than silently returning empty.
	return d.cat, nil
}

// unitVecAt returns a storage.VectorDim-length vector with 1.0 at index i and
// zero elsewhere. The chosen indexes (0 for cat, 1 for dog) give L2 distance
// 0 between an item and its category embedding and L2 distance sqrt(2)
// between categories.
func unitVecAt(i int) []float32 {
	v := make([]float32, storage.VectorDim)
	if i >= 0 && i < storage.VectorDim {
		v[i] = 1.0
	}
	return v
}

func containsAny(haystack, needle string) bool {
	// stdlib strings.Contains avoided to keep the helper file tiny.
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func drainIntegration(t *testing.T, it func(yield func(kernel.Hit, error) bool)) []kernel.Hit {
	t.Helper()
	hits := make([]kernel.Hit, 0, 8)
	for h, err := range it {
		if err != nil {
			t.Fatalf("iterator yielded error: %v", err)
		}
		hits = append(hits, h)
	}
	return hits
}
