package document_test

// semantic_wikilink_test.go proves the semantic name-embedding wikilink
// resolution lane (P3). All tests use a fake embedder and a fake
// nameEmbeddingSearcher — no Ollama, no real sqlite-vec — so they are
// deterministic and fast. The integration tests run the full four-layer stack
// (FileEventLog → dispatcher → SQLite + sqlite-vec) via newStack.

import (
	"context"
	"math"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// ---- fake infrastructure for unit tests ----

// vectorEntry represents one entity name vector in the fake searcher.
type vectorEntry struct {
	uid  string
	kind string
	// sim is the cosine similarity this entry should return when searched.
	// The fake converts it to an L2 distance: d = sqrt(2*(1-sim)).
	sim float32
}

// fakeNameSearcher implements the nameEmbeddingSearcher interface used by
// NewNameEmbeddingResolver. It returns a deterministic ranked list regardless
// of the query vector.
type fakeNameSearcher struct {
	entries []vectorEntry
	calls   int
}

func (f *fakeNameSearcher) SearchNameVectors(_ context.Context, _ []float32, k int) ([]kernel.VectorHit, error) {
	f.calls++
	hits := make([]kernel.VectorHit, 0, k)
	for i, e := range f.entries {
		if i >= k {
			break
		}
		// Convert cosine similarity to L2 distance for L2-normalised vectors:
		// cos = 1 - d²/2  =>  d = sqrt(2*(1-cos)).
		// l2DistanceToCosineDoc inverts this same formula in production.
		d := math.Sqrt(2.0 * (1.0 - float64(e.sim)))
		if d < 0 {
			d = 0
		}
		dist := float32(d)
		hits = append(hits, kernel.VectorHit{ID: e.uid, Score: dist})
	}
	return hits, nil
}

// fakeCountingEmbedder counts Embed calls so idempotency tests can assert that
// unchanged re-imports do not trigger embedding.
type fakeCountingEmbedder struct {
	calls int
	dim   int
}

func (f *fakeCountingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	f.calls++
	return make([]float32, f.dim), nil
}

// buildSemanticResolver creates a SemanticNameResolver for unit tests with the
// given fake searcher and a simple kind map.
func buildSemanticResolver(t *testing.T, searcher *fakeNameSearcher, kindMap map[string]string) document.NameEmbeddingResolverFunc {
	t.Helper()
	emb := &fakeCountingEmbedder{dim: storage.VectorDim}
	kindForUID := func(_ context.Context, uid string) string {
		return kindMap[uid]
	}
	return document.NewNameEmbeddingResolver(emb, searcher, kindForUID, zap.NewNop())
}

// ---- unit tests for the SemanticNameResolver implementation ----

// TestSemanticResolve_HighSimilarityUnambiguous proves the core success path:
// a top hit with similarity above threshold and a margin above the margin
// threshold resolves the target.
func TestSemanticResolve_HighSimilarityUnambiguous(t *testing.T) {
	t.Parallel()
	searcher := &fakeNameSearcher{
		entries: []vectorEntry{
			{uid: "u-sarah", kind: "person", sim: 0.97},
			{uid: "u-alice", kind: "person", sim: 0.80},
		},
	}
	snr := buildSemanticResolver(t, searcher, map[string]string{
		"u-sarah": "person",
		"u-alice": "person",
	})
	uid, kind, ok := snr.ResolveNameSemantically(context.Background(), "Rosie", "other-uid")
	if !ok {
		t.Fatal("expected resolution, got ok=false")
	}
	if uid != "u-sarah" {
		t.Errorf("uid = %q, want u-sarah", uid)
	}
	if kind != "person" {
		t.Errorf("kind = %q, want person", kind)
	}
}

// TestSemanticResolve_BelowThreshold proves that a top hit below the cosine
// similarity threshold leaves the link dangling (ok=false).
func TestSemanticResolve_BelowThreshold(t *testing.T) {
	t.Parallel()
	// 0.85 < default threshold 0.92.
	searcher := &fakeNameSearcher{
		entries: []vectorEntry{
			{uid: "u-sarah", kind: "person", sim: 0.85},
		},
	}
	snr := buildSemanticResolver(t, searcher, map[string]string{"u-sarah": "person"})
	_, _, ok := snr.ResolveNameSemantically(context.Background(), "Sara", "other-uid")
	if ok {
		t.Error("low-similarity hit must not resolve; got ok=true")
	}
}

// TestSemanticResolve_AmbiguousMargin proves that when the top hit and runner-up
// have similar scores (margin < 0.05), the link stays dangling — the resolver
// must not guess between two equally likely entities.
func TestSemanticResolve_AmbiguousMargin(t *testing.T) {
	t.Parallel()
	// Both above threshold, but margin = 0.95 - 0.92 = 0.03 < 0.05.
	searcher := &fakeNameSearcher{
		entries: []vectorEntry{
			{uid: "u-sarah-jones", kind: "person", sim: 0.95},
			{uid: "u-sarah-smith", kind: "person", sim: 0.92},
		},
	}
	snr := buildSemanticResolver(t, searcher, map[string]string{
		"u-sarah-jones": "person",
		"u-sarah-smith": "person",
	})
	_, _, ok := snr.ResolveNameSemantically(context.Background(), "Rosie", "other-uid")
	if ok {
		t.Error("ambiguous margin must leave link dangling; got ok=true")
	}
}

// TestSemanticResolve_SelfSuppression proves that a semantic hit on the
// importing document's own entity UID is suppressed (no self-edge).
func TestSemanticResolve_SelfSuppression(t *testing.T) {
	t.Parallel()
	searcher := &fakeNameSearcher{
		entries: []vectorEntry{
			{uid: "self-doc-uid", kind: "document", sim: 0.97},
		},
	}
	snr := buildSemanticResolver(t, searcher, map[string]string{"self-doc-uid": "document"})
	_, _, ok := snr.ResolveNameSemantically(context.Background(), "My Note", "self-doc-uid")
	if ok {
		t.Error("self-referential semantic hit must be suppressed; got ok=true")
	}
}

// TestSemanticResolve_EmptySearchResults proves that an empty vector search
// (no entities seeded yet) leaves the link dangling without panicking.
func TestSemanticResolve_EmptySearchResults(t *testing.T) {
	t.Parallel()
	searcher := &fakeNameSearcher{entries: nil}
	snr := buildSemanticResolver(t, searcher, nil)
	_, _, ok := snr.ResolveNameSemantically(context.Background(), "Nobody", "x")
	if ok {
		t.Error("empty search results must leave link dangling; got ok=true")
	}
}

// TestSemanticResolve_KindAgnostic proves that the semantic lane resolves
// non-person entity kinds (org, place, project, document) — the lane is
// kind-agnostic unlike resolver.matchMention.
func TestSemanticResolve_KindAgnostic(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"organisation", "place", "project", "document"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			uid := "u-target-" + kind
			searcher := &fakeNameSearcher{
				entries: []vectorEntry{
					{uid: uid, kind: kind, sim: 0.96},
					{uid: "u-other", kind: "person", sim: 0.70},
				},
			}
			snr := buildSemanticResolver(t, searcher, map[string]string{
				uid:       kind,
				"u-other": "person",
			})
			gotUID, gotKind, ok := snr.ResolveNameSemantically(context.Background(), "Target", "other-uid")
			if !ok {
				t.Fatalf("kind %q: expected resolution, got ok=false", kind)
			}
			if gotUID != uid {
				t.Errorf("kind %q: uid = %q, want %q", kind, gotUID, uid)
			}
			if gotKind != kind {
				t.Errorf("kind %q: resolved kind = %q, want %q", kind, gotKind, kind)
			}
		})
	}
}

// ---- integration tests against the real four-layer stack ----

// TestSemanticWikilink_ResolvesPartialName proves the end-to-end happy path:
// [[Rosie]] resolves to the "Rosie Cotton" person entity via the semantic lane
// (exact match misses because "Rosie" != "Rosie Cotton"), producing a mentions
// edge, and the person's neighbors include the document.
func TestSemanticWikilink_ResolvesPartialName(t *testing.T) {
	sarahUID := "u-sarah-" + t.Name()
	searcher := &fakeNameSearcher{
		entries: []vectorEntry{
			{uid: sarahUID, kind: "person", sim: 0.97},
		},
	}
	s := newStack(t)

	// Seed the Rosie Cotton entity so the kind lookup has a real row.
	// We override sarahUID via a seeded entity so the SNR returns a
	// UID that actually exists in the projection.
	sarahUID = s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")
	searcher.entries[0].uid = sarahUID

	snr := buildSemanticResolver(t, searcher, map[string]string{sarahUID: "person"})
	s.im.WithSemanticNameResolver(snr)

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "about-sarah.md",
		[]byte("# About Rosie\n\n[[Rosie]] leads the project.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("Imported = %d, want 1", res.Imported)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "about-sarah.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	// The semantic lane must produce a mentions edge to Rosie Cotton.
	edges := s.outgoing(t, doc.DocumentUID, "mentions")
	if len(edges) != 1 || edges[0].ObjectUID != sarahUID {
		t.Fatalf("mentions edges = %+v, want one edge → %s (Rosie Cotton)", edges, sarahUID)
	}

	// Neighbors of Rosie must include the document (backlink surface).
	neighbors, err := s.nav.Neighbors(s.ctx, sarahUID, 1, navigate.RelationFilter{
		Predicates: []string{"mentions"},
	})
	if err != nil {
		t.Fatalf("Neighbors(sarah): %v", err)
	}
	found := false
	for _, n := range neighbors {
		if n.Entity.UID == doc.DocumentUID {
			found = true
		}
	}
	if !found {
		t.Errorf("Rosie's neighbors do not include the document: %+v", neighbors)
	}
}

// TestSemanticWikilink_ExactMatchWins proves that when an exact display-name
// match is available, the semantic resolver is not called.
func TestSemanticWikilink_ExactMatchWins(t *testing.T) {
	// Track call count on the searcher to assert the semantic lane did not fire.
	searcher := &fakeNameSearcher{}
	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")
	snr := buildSemanticResolver(t, searcher, map[string]string{sarahUID: "person"})
	s.im.WithSemanticNameResolver(snr)

	// [[Rosie Cotton]] matches exactly — semantic lane must not fire.
	_, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "note.md",
		[]byte("# Note\n\n[[Rosie Cotton]] is mentioned.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if searcher.calls != 0 {
		t.Errorf("searcher called %d times; exact match must short-circuit semantic lane", searcher.calls)
	}
}

// TestSemanticWikilink_AmbiguousStaysUnresolved proves that when the searcher
// returns two hits with margin < threshold, the link stays unresolved (no wrong
// edge is emitted).
func TestSemanticWikilink_AmbiguousStaysUnresolved(t *testing.T) {
	s := newStack(t)
	uid1 := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")
	uid2 := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Smith")

	// Both above threshold, margin = 0.95 - 0.92 = 0.03 < 0.05.
	searcher := &fakeNameSearcher{
		entries: []vectorEntry{
			{uid: uid1, kind: "person", sim: 0.95},
			{uid: uid2, kind: "person", sim: 0.92},
		},
	}
	snr := buildSemanticResolver(t, searcher, map[string]string{
		uid1: "person",
		uid2: "person",
	})
	s.im.WithSemanticNameResolver(snr)

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "ambiguous.md",
		[]byte("# Ambiguous\n\n[[Rosie]] is mentioned.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	// The link must count as unresolved.
	if res.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1 (ambiguous link must stay dangling)", res.Unresolved)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "ambiguous.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	// No mentions edge to either entity.
	for _, uid := range []string{uid1, uid2} {
		for _, pred := range []string{"mentions", "links_to"} {
			edges := s.outgoing(t, doc.DocumentUID, pred)
			for _, e := range edges {
				if e.ObjectUID == uid {
					t.Errorf("ambiguous link produced %s edge to %q; must stay unresolved", pred, uid)
				}
			}
		}
	}
}

// TestSemanticWikilink_IdempotentReimport proves that re-importing an unchanged
// document with a wired semantic resolver does not re-call the embedder. The
// content-hash dedup skips the file before any embedding occurs.
func TestSemanticWikilink_IdempotentReimport(t *testing.T) {
	emb := &fakeCountingEmbedder{dim: storage.VectorDim}
	sarahUID := ""

	s := newStack(t)
	sarahUID = s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	searcher := &fakeNameSearcher{
		entries: []vectorEntry{
			{uid: sarahUID, kind: "person", sim: 0.97},
		},
	}
	kindForUID := func(_ context.Context, uid string) string {
		if uid == sarahUID {
			return "person"
		}
		return ""
	}
	snr := document.NewNameEmbeddingResolver(emb, searcher, kindForUID, zap.NewNop())
	s.im.WithSemanticNameResolver(snr)

	// First import: [[Rosie]] does not exact-match, so semantic lane fires.
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "idem.md",
		[]byte("# Idempotent\n\n[[Rosie]] leads.\n")); err != nil {
		t.Fatalf("first import: %v", err)
	}
	callsAfterFirst := emb.calls

	// Re-import unchanged content: content-hash dedup skips the file.
	res2, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "idem.md",
		[]byte("# Idempotent\n\n[[Rosie]] leads.\n"))
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if res2.Skipped != 1 || res2.Imported != 0 {
		t.Errorf("re-import: Skipped=%d Imported=%d, want Skipped=1", res2.Skipped, res2.Imported)
	}
	if emb.calls != callsAfterFirst {
		t.Errorf("embedder called %d times on re-import, want 0 new calls (content-hash dedup must prevent re-embedding)",
			emb.calls-callsAfterFirst)
	}

	// Also confirm no duplicate edge was emitted.
	doc, err := s.nav.GetDocumentByPath(s.ctx, "idem.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	edges := s.outgoing(t, doc.DocumentUID, "mentions")
	if len(edges) != 1 {
		t.Errorf("after re-import: %d mentions edges, want 1 (no duplicate)", len(edges))
	}
}

// TestSemanticWikilink_ReconcilePicksUpSemanticHit proves that a previously
// dangling link (no exact match and semantic lane absent at import time) is
// resolved in the reconcile pass when the semantic resolver is later available.
// We simulate this by importing with snr=nil first (link dangles), then calling
// ImportContent again with a different doc that triggers reconcile — with snr
// wired this time — confirming that the reconcile pass uses the semantic lane.
func TestSemanticWikilink_ReconcilePicksUpSemanticHit(t *testing.T) {
	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	// First import without the semantic resolver: [[Sari]] dangles. "Sari" is a
	// nickname that is neither an exact match nor a whole-word token of "Rosie
	// Jones", so the deterministic partial-name lane does NOT resolve it — only
	// the semantic lane can, which isolates this test to that lane.
	res1, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "a.md",
		[]byte("# A\n\n[[Sari]] leads.\n"))
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if res1.Unresolved != 1 {
		t.Fatalf("first import: Unresolved = %d, want 1", res1.Unresolved)
	}
	docA, _ := s.nav.GetDocumentByPath(s.ctx, "a.md")
	if len(s.outgoing(t, docA.DocumentUID, "mentions")) != 0 {
		t.Fatal("precondition: no edge before semantic resolver is wired")
	}

	// Wire the semantic resolver on the importer.
	searcher := &fakeNameSearcher{
		entries: []vectorEntry{
			{uid: sarahUID, kind: "person", sim: 0.97},
		},
	}
	snr := buildSemanticResolver(t, searcher, map[string]string{sarahUID: "person"})
	s.im.WithSemanticNameResolver(snr)

	// Import a new document to trigger the reconcile pass.
	// Reconcile re-resolves a.md's dangling [[Sari]] with snr now active.
	res2, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "b.md",
		[]byte("# B\n\nAnother document.\n"))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if res2.Reconciled != 1 {
		t.Errorf("Reconciled = %d, want 1 (semantic lane must reconcile the dangling link)", res2.Reconciled)
	}

	// a.md must now have a mentions edge to Rosie.
	edges := s.outgoing(t, docA.DocumentUID, "mentions")
	if len(edges) != 1 || edges[0].ObjectUID != sarahUID {
		t.Errorf("after reconcile: mentions = %+v, want one edge → %s", edges, sarahUID)
	}
}

// TestSemanticWikilink_NilSNRLeavesLinkDangling proves that a nil snr (semantic
// resolver not wired) is handled gracefully — the link stays unresolved without
// panic or error.
func TestSemanticWikilink_NilSNRLeavesLinkDangling(t *testing.T) {
	s := newStack(t) // no WithSemanticNameResolver called
	s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "no-snr.md",
		[]byte("# No SNR\n\n[[Sari]] leads.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1 (nil snr + non-substring nickname must dangle)", res.Unresolved)
	}
}

// TestSemanticWikilink_LiveDeadLinkDocs proves that LiveDeadLinkDocs with a
// wired semantic resolver drops a previously-dangling link from the dead-link
// report when the semantic lane can now resolve it.
func TestSemanticWikilink_LiveDeadLinkDocs(t *testing.T) {
	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	// Import with nil snr so [[Sari]] stays dangling (non-substring nickname:
	// the deterministic partial-name lane cannot resolve it, only the semantic
	// lane can).
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "lint.md",
		[]byte("# Lint\n\n[[Sari]] leads.\n")); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Without snr: the link is dead.
	deadBefore, err := document.LiveDeadLinkDocs(s.ctx, s.nav, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("LiveDeadLinkDocs (no snr): %v", err)
	}
	if len(deadBefore) != 1 || deadBefore[0].Path != "lint.md" {
		t.Fatalf("pre-snr: dead links = %+v, want lint.md", deadBefore)
	}

	// With snr resolving [[Rosie]] → Rosie Cotton: the link is NOT dead.
	searcher := &fakeNameSearcher{
		entries: []vectorEntry{
			{uid: sarahUID, kind: "person", sim: 0.97},
		},
	}
	snr := buildSemanticResolver(t, searcher, map[string]string{sarahUID: "person"})
	deadAfter, err := document.LiveDeadLinkDocs(s.ctx, s.nav, snr, zap.NewNop())
	if err != nil {
		t.Fatalf("LiveDeadLinkDocs (with snr): %v", err)
	}
	if len(deadAfter) != 0 {
		t.Errorf("with snr: dead links = %+v, want none (semantic lane resolves [[Sari]])", deadAfter)
	}
}

// TestSemanticWikilink_FullStack_SearchNameVectorsWired is an integration test
// that runs the complete four-layer stack including sqlite-vec to verify that
// the production SearchNameVectors path works end-to-end. It seeds a person
// entity via the event log (which triggers the dispatcher to write
// entities_vec_ids), then imports a document with [[Rosie]], using the real
// fakeEmbedder (zero vector) for both the entity seed and the query.
//
// Because both the entity name vector and the query vector are identical (all
// zeros), the top hit will be the seeded entity — so the semantic lane resolves
// the partial name to the full entity without any LLM.
func TestSemanticWikilink_FullStack_SearchNameVectorsWired(t *testing.T) {
	ctx := context.Background()

	// Use newStackWithDis so we can open the vector layer.
	s, dis := newStackWithDis(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	// Wait for the dispatcher's background entity-vector write. Without this,
	// SearchNameVectors returns no hits because the entities_vec row has not
	// landed yet.
	if err := dis.WaitForBackgroundVec(ctx); err != nil {
		t.Fatalf("WaitForBackgroundVec: %v", err)
	}

	vec, err := dis.OpenVector(ctx)
	if err != nil {
		t.Fatalf("OpenVector: %v", err)
	}

	// The fakeEmbedder (dim=VectorDim, all zeros) is the same embedder the
	// dispatcher used for the entity name vector, so cosine similarity == 1.0.
	emb := fakeEmbedder{dim: storage.VectorDim}

	// The production *sqlite.Vector satisfies nameEmbeddingSearcher via
	// SearchNameVectors. Use a type assertion to build the resolver.
	type nameSearcher interface {
		SearchNameVectors(ctx context.Context, query []float32, k int) ([]kernel.VectorHit, error)
	}
	ns, ok := vec.(nameSearcher)
	if !ok {
		t.Skip("vector conn does not implement SearchNameVectors (not the production sqlite.Vector)")
	}

	kindForUID := func(ctx context.Context, uid string) string {
		ents, err := s.nav.ListEntities(ctx, navigate.EntityFilter{Limit: 10})
		if err != nil {
			return ""
		}
		for _, e := range ents {
			if e.UID == uid {
				return e.Kind
			}
		}
		return ""
	}
	snr := document.NewNameEmbeddingResolver(emb, ns, kindForUID, zap.NewNop())
	s.im.WithSemanticNameResolver(snr)

	// Import a document with [[Sari]] — exact AND partial-name lanes miss (not a
	// substring of "Rosie Cotton"); only the production SearchNameVectors semantic
	// lane can resolve it, which is exactly what this test exercises.
	res, err := s.im.ImportContent(ctx, kernel.DefaultScope, "full-stack.md",
		[]byte("# Full Stack\n\n[[Sari]] leads the project.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(ctx, "full-stack.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	edges := s.outgoing(t, doc.DocumentUID, "mentions")
	if len(edges) != 1 || edges[0].ObjectUID != sarahUID {
		t.Errorf("mentions = %+v (Imported=%d Unresolved=%d), want one edge → %s (Rosie Cotton)",
			edges, res.Imported, res.Unresolved, sarahUID)
	}
}
