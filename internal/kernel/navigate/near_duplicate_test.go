package navigate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// nearDupVecDeadline caps the WaitForBackgroundVec call in near-dup tests.
const nearDupVecDeadline = 10 * time.Second

// nearDupEmbedder assigns vectors based on a "group" suffix in the text:
//
//   - text containing "groupA" -> unit vector along axis 0
//   - text containing "groupB" -> unit vector along axis 1
//   - text containing "groupC" -> unit vector along axis 2
//
// GroupA texts are near-identical (cosine=1.0); groupA vs groupB are
// orthogonal (cosine=0.0). All vectors are L2-normalized so the
// CosineSimilarityFromDistance formula (1 - d²/2) is exact.
type nearDupEmbedder struct{}

func (nearDupEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	out := make([]float32, storage.VectorDim)
	switch {
	case contains(text, "groupA"):
		out[0] = 1
	case contains(text, "groupB"):
		out[1] = 1
	default:
		out[2] = 1
	}
	return out, nil
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && findSubstring(s, sub)
}

func findSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// openNearDupDispatcher opens a Dispatcher with nearDupEmbedder.
func openNearDupDispatcher(t *testing.T, ctx context.Context) *dispatcher.Dispatcher {
	t.Helper()
	d, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  t.TempDir(),
		Embedder: nearDupEmbedder{},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// applyDocRevision applies one documentRevision event and returns the document UID.
func applyDocRevision(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, docUID, path, body string) {
	t.Helper()
	payload, err := json.Marshal(ontology.DocumentRevision{
		DocumentUID: docUID,
		Path:        path,
		Title:       "Test",
		Body:        body,
		ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatalf("marshal DocumentRevision: %v", err)
	}
	ev := kernel.Event{
		UID:        docUID + "-rev",
		Kind:       kernel.EventKind(ontology.KindDocumentRevision),
		Source:     "test",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 8, 10, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply %q: %v", path, err)
	}
}

// TestNearDuplicateDocuments_FindsDuplicatePair proves that two documents with
// identical chunk embeddings (cosine=1.0 via groupA keyword) are returned as a
// near-duplicate pair when the threshold is 0.90, while a third document with an
// orthogonal embedding (groupB) is NOT paired with either groupA doc.
func TestNearDuplicateDocuments_FindsDuplicatePair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openNearDupDispatcher(t, ctx)

	// Two near-identical docs (groupA vector, cosine=1.0 with each other).
	applyDocRevision(t, ctx, d, "docA1", "notes/alpha1.md", "content groupA version one")
	applyDocRevision(t, ctx, d, "docA2", "notes/alpha2.md", "content groupA version two")
	// A distinct doc (groupB vector, cosine=0.0 with groupA docs).
	applyDocRevision(t, ctx, d, "docB1", "notes/beta1.md", "content groupB unrelated")

	// Drain background embedding goroutines before querying.
	waitCtx, cancel := context.WithTimeout(ctx, nearDupVecDeadline)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	pairs, err := nav.NearDuplicateDocuments(ctx, 0.90)
	if err != nil {
		t.Fatalf("NearDuplicateDocuments: %v", err)
	}

	// Expect exactly one pair: docA1 <-> docA2 (they share the groupA vector).
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs, want exactly 1 (docA1<->docA2): %+v", len(pairs), pairs)
	}
	p := pairs[0]
	// Canonical order is lexicographic UID (lower first).
	if (p.SubjectUID != "docA1" || p.ObjectUID != "docA2") &&
		(p.SubjectUID != "docA2" || p.ObjectUID != "docA1") {
		t.Errorf("pair UIDs = (%q, %q), want (docA1, docA2)", p.SubjectUID, p.ObjectUID)
	}
	if p.Similarity < 0.90 {
		t.Errorf("similarity = %.3f, want >= 0.90", p.Similarity)
	}
	// Canonical order: lower UID first.
	if p.SubjectUID > p.ObjectUID {
		t.Errorf("subjectUid %q > objectUid %q: want lexicographically lower UID first",
			p.SubjectUID, p.ObjectUID)
	}
}

// TestNearDuplicateDocuments_NoPairsWhenDistinct proves that documents with
// orthogonal embeddings (cosine=0.0) produce no near-duplicate pairs.
func TestNearDuplicateDocuments_NoPairsWhenDistinct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openNearDupDispatcher(t, ctx)

	applyDocRevision(t, ctx, d, "docX1", "notes/x1.md", "content groupA topic")
	applyDocRevision(t, ctx, d, "docY1", "notes/y1.md", "content groupB topic")

	waitCtx, cancel := context.WithTimeout(ctx, nearDupVecDeadline)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	pairs, err := nav.NearDuplicateDocuments(ctx, 0.90)
	if err != nil {
		t.Fatalf("NearDuplicateDocuments: %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("got %d pairs, want 0 for orthogonal docs: %+v", len(pairs), pairs)
	}
}

// TestNearDuplicateDocuments_ClusterExceedsInitialWindow is the regression test
// for the KNN-cap omission: a cluster of identical documents LARGER than the
// initial per-chunk KNN window (50) must still yield EVERY pair. A fixed window
// silently dropped pairs once a cluster exceeded it; the adaptive window grows
// until the corpus is exhausted, so all C(n,2) pairs are reported.
func TestNearDuplicateDocuments_ClusterExceedsInitialWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openNearDupDispatcher(t, ctx)

	// 60 byte-distinct but semantically identical docs (all groupA -> same unit
	// vector, cosine 1.0). 60 > the initial KNN window of 50, so a fixed cap
	// would miss pairs; the adaptive window must find all of them.
	const n = 60
	for i := range n {
		uid := fmt.Sprintf("docC%03d", i)
		path := fmt.Sprintf("notes/clone-%03d.md", i)
		applyDocRevision(t, ctx, d, uid, path, fmt.Sprintf("content groupA clone number %d", i))
	}

	waitCtx, cancel := context.WithTimeout(ctx, nearDupVecDeadline)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	pairs, err := nav.NearDuplicateDocuments(ctx, 0.90)
	if err != nil {
		t.Fatalf("NearDuplicateDocuments: %v", err)
	}

	// Every unordered pair of the n identical docs must appear exactly once.
	wantPairs := n * (n - 1) / 2
	if len(pairs) != wantPairs {
		t.Fatalf("got %d pairs, want %d (all C(%d,2) combinations)", len(pairs), wantPairs, n)
	}
	seen := map[[2]string]bool{}
	for _, p := range pairs {
		if p.SubjectUID > p.ObjectUID {
			t.Errorf("pair (%q, %q) not in canonical order (lower UID first)", p.SubjectUID, p.ObjectUID)
		}
		key := [2]string{p.SubjectUID, p.ObjectUID}
		if seen[key] {
			t.Errorf("duplicate pair reported: (%q, %q)", p.SubjectUID, p.ObjectUID)
		}
		seen[key] = true
	}
}

// TestNearDuplicateDocuments_PartiallyEmbeddedSkipsGracefully is the regression
// test for the crash a real vault surfaced: a document written without an
// embedder has a documents_vec_ids sidecar row but NO documents_vec vector.
// Using such a rowid as the KNN query vector made sqlite-vec fail with
// "Query vector ... found NULL", aborting the whole lint. Near-duplicate
// detection must instead skip un-embedded chunks and return cleanly.
func TestNearDuplicateDocuments_PartiallyEmbeddedSkipsGracefully(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// No Embedder: applyDocumentRevision writes the document + sidecar rows, but
	// schedules no embedding, so documents_vec stays empty.
	d, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	applyDocRevision(t, ctx, d, "docU1", "notes/u1.md", "content one for the partial vault")
	applyDocRevision(t, ctx, d, "docU2", "notes/u2.md", "content two for the partial vault")

	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	pairs, err := nav.NearDuplicateDocuments(ctx, 0.90)
	if err != nil {
		t.Fatalf("must not error on a partially-embedded vault: %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("got %d pairs, want 0 (no embeddings written)", len(pairs))
	}
}

// TestNearDuplicateDocuments_EmptyStoreReturnsEmpty proves the function handles
// an empty vault gracefully (no documents, no embeddings) by returning an empty
// slice rather than an error.
func TestNearDuplicateDocuments_EmptyStoreReturnsEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _ := openNavStore(t)
	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	pairs, err := nav.NearDuplicateDocuments(ctx, navigate.NearDuplicateSimilarityThreshold)
	if err != nil {
		t.Fatalf("NearDuplicateDocuments on empty store: %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("got %d pairs on empty store, want 0", len(pairs))
	}
}

// TestNearDuplicateDocuments_SymmetricPairDeduplication proves each pair is
// reported exactly once regardless of iteration order (A,B) and (B,A) must
// collapse to a single pair.
func TestNearDuplicateDocuments_SymmetricPairDeduplication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openNearDupDispatcher(t, ctx)

	// Three groupA docs: each pair (A1,A2), (A1,A3), (A2,A3) is a near-duplicate.
	applyDocRevision(t, ctx, d, "docP1", "notes/p1.md", "content groupA item one")
	applyDocRevision(t, ctx, d, "docP2", "notes/p2.md", "content groupA item two")
	applyDocRevision(t, ctx, d, "docP3", "notes/p3.md", "content groupA item three")

	waitCtx, cancel := context.WithTimeout(ctx, nearDupVecDeadline)
	defer cancel()
	if err := d.WaitForBackgroundVec(waitCtx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	pairs, err := nav.NearDuplicateDocuments(ctx, 0.90)
	if err != nil {
		t.Fatalf("NearDuplicateDocuments: %v", err)
	}

	// Three groupA docs produce 3 unique pairs: (P1,P2), (P1,P3), (P2,P3).
	// No symmetric duplicates.
	seen := map[[2]string]bool{}
	for _, p := range pairs {
		key := [2]string{p.SubjectUID, p.ObjectUID}
		revKey := [2]string{p.ObjectUID, p.SubjectUID}
		if seen[key] || seen[revKey] {
			t.Errorf("duplicate pair reported: (%q, %q)", p.SubjectUID, p.ObjectUID)
		}
		seen[key] = true
		if p.SubjectUID > p.ObjectUID {
			t.Errorf("pair (%q, %q) not in canonical order (lower UID first)", p.SubjectUID, p.ObjectUID)
		}
	}
	if len(pairs) != 3 {
		t.Errorf("got %d pairs, want 3 (all combinations of 3 identical-vector docs): %+v", len(pairs), pairs)
	}
}
