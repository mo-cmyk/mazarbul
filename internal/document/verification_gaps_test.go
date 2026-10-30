package document_test

// verification_gaps_test.go covers two correctness fixes found while
// E2E-testing the built binary against a seeded vault:
//
//   - FIX 3: the deterministic partial-name wikilink lane. A short-form link
//     like [[Rosie]] resolves to "Rosie Cotton" when exactly one entity has the
//     target as a contiguous run of whole words in its display name — without a
//     semantic resolver wired, and never guessing between two candidates.
//   - FIX 4: an observation line's inline #tags are stored verbatim in the Fact
//     value and must not be appended a second time.

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestResolveWikilink_PartialNameResolvesUniqueWholeWordMatch proves the
// deterministic partial-name lane resolves [[Rosie]] -> "Rosie Cotton" with NO
// semantic resolver wired, emitting a mentions edge to the canonical person.
func TestResolveWikilink_PartialNameResolvesUniqueWholeWordMatch(t *testing.T) {
	s := newStack(t) // no WithSemanticNameResolver: only the deterministic lanes run
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "note.md",
		[]byte("# Note\n\n[[Rosie]] leads the project.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0 (partial name must resolve deterministically)", res.Unresolved)
	}

	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	edges := s.outgoing(t, doc.DocumentUID, "mentions")
	if len(edges) != 1 || edges[0].ObjectUID != sarahUID {
		t.Errorf("mentions = %+v, want one edge -> %s (Rosie Cotton)", edges, sarahUID)
	}
}

// TestResolveWikilink_PartialNameAmbiguousStaysDangling proves the lane never
// guesses: when two entities both contain the target as a whole word, the link
// stays unresolved rather than picking one.
func TestResolveWikilink_PartialNameAmbiguousStaysDangling(t *testing.T) {
	s := newStack(t)
	s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")
	s.seedEntity(t, ontology.EntityKindPerson, "Rosie Connor")

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "note.md",
		[]byte("# Note\n\n[[Rosie]] leads.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1 (ambiguous partial name must dangle)", res.Unresolved)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if edges := s.outgoing(t, doc.DocumentUID, "mentions"); len(edges) != 0 {
		t.Errorf("mentions = %+v, want none (ambiguous match must not emit an edge)", edges)
	}
}

// TestResolveWikilink_PartialNameRejectsSubWord proves a sub-word like [[Sara]]
// does NOT match "Rosie Cotton": NameLike's substring filter would surface it,
// but the whole-word check rejects it, so the link dangles.
func TestResolveWikilink_PartialNameRejectsSubWord(t *testing.T) {
	s := newStack(t)
	s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "note.md",
		[]byte("# Note\n\n[[Sara]] leads.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1 (sub-word must not match a whole word)", res.Unresolved)
	}
}

// TestResolveWikilink_PartialNameMatchesContiguousMultiWordRun proves a
// multi-token target like [[Marigold]] resolves to "Marigold Gamgee" because
// the tokens appear as a contiguous run.
func TestResolveWikilink_PartialNameMatchesContiguousMultiWordRun(t *testing.T) {
	s := newStack(t)
	maryUID := s.seedEntity(t, ontology.EntityKindPerson, "Marigold Gamgee")

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "note.md",
		[]byte("# Note\n\n[[Marigold]] stars.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0 (contiguous multi-word run must resolve)", res.Unresolved)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	edges := s.outgoing(t, doc.DocumentUID, "mentions")
	if len(edges) != 1 || edges[0].ObjectUID != maryUID {
		t.Errorf("mentions = %+v, want one edge -> %s (Marigold Gamgee)", edges, maryUID)
	}
}

// TestResolveWikilink_PartialNameRejectsNonContiguousTokens proves [[Marigold
// Gamgee]] does NOT match "Marigold Took Gamgee": the tokens are present but
// not contiguous, so the link dangles rather than matching loosely.
func TestResolveWikilink_PartialNameRejectsNonContiguousTokens(t *testing.T) {
	s := newStack(t)
	s.seedEntity(t, ontology.EntityKindPerson, "Marigold Took Gamgee")

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "note.md",
		[]byte("# Note\n\n[[Marigold Gamgee]] stars.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1 (non-contiguous tokens must not match)", res.Unresolved)
	}
}

// TestImport_ResultSurfacesUnresolvedTargets proves the import Result names the
// dangling wikilink targets (not just a count), so a caller learns WHICH links
// failed to resolve.
func TestImport_ResultSurfacesUnresolvedTargets(t *testing.T) {
	s := newStack(t) // no entities seeded → the link cannot resolve

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "note.md",
		[]byte("# Note\n\n[[Zzyzx Nonentity]] is unknown.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Unresolved != 1 {
		t.Fatalf("Unresolved = %d, want 1", res.Unresolved)
	}
	if len(res.UnresolvedTargets) != 1 || res.UnresolvedTargets[0] != "Zzyzx Nonentity" {
		t.Errorf("UnresolvedTargets = %+v, want [Zzyzx Nonentity]", res.UnresolvedTargets)
	}
}

// TestImport_ObservationInlineTagNotDuplicatedInFactValue proves an
// observation's inline #tag appears exactly once in the projected Fact value —
// regression for the "ship Friday #launch #launch" duplication.
func TestImport_ObservationInlineTagNotDuplicatedInFactValue(t *testing.T) {
	s, dis := newStackWithDis(t)

	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "note.md",
		[]byte("# Plan\n\n- [decision] We ship on Friday #launch\n")); err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	facts := s.queryFacts(t, dis, doc.DocumentUID)
	if len(facts) != 1 {
		t.Fatalf("fact count = %d, want 1", len(facts))
	}
	got := facts[0].Value
	if n := strings.Count(got, "#launch"); n != 1 {
		t.Errorf("fact value = %q contains %d occurrences of #launch, want exactly 1", got, n)
	}
	if got != "We ship on Friday #launch" {
		t.Errorf("fact value = %q, want %q", got, "We ship on Friday #launch")
	}
}

// TestResolveWikilink_PartialNameAmbiguousBlocksSemanticLane proves an ambiguous
// whole-word partial match (two entities both containing the target as a whole
// word) leaves the link dangling EVEN when a confident semantic resolver is
// wired. The deterministic lane already proved the target names more than one
// entity, so the semantic top hit must not silently pick one. Regression for
// the "ambiguous partial still resolved through the semantic fallback" gap.
func TestResolveWikilink_PartialNameAmbiguousBlocksSemanticLane(t *testing.T) {
	s := newStack(t)
	jonesUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")
	connorUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Connor")

	// A confident, unambiguous semantic hit on Rosie Cotton. If the ambiguity
	// gate were absent, this would resolve [[Rosie]] -> Rosie Cotton.
	searcher := &fakeNameSearcher{
		entries: []vectorEntry{{uid: jonesUID, kind: "person", sim: 0.99}},
	}
	snr := buildSemanticResolver(t, searcher, map[string]string{jonesUID: "person"})
	s.im.WithSemanticNameResolver(snr)

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "note.md",
		[]byte("# Note\n\n[[Rosie]] leads.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1 (ambiguous partial must dangle, not fall through to semantic)", res.Unresolved)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	for _, uid := range []string{jonesUID, connorUID} {
		for _, e := range s.outgoing(t, doc.DocumentUID, "mentions") {
			if e.ObjectUID == uid {
				t.Errorf("ambiguous partial produced a mentions edge to %q; the semantic lane must stay blocked", uid)
			}
		}
	}
}

// TestResolveWikilink_PartialNameMatchesAcrossPunctuation proves the partial
// lane matches token-equivalent names that differ only in punctuation:
// [[Apollo Launch]] resolves to "Apollo-Launch Project". The candidate
// prefilter narrows on the leading token (alphanumeric, punctuation-robust), so
// the hyphenated name is retrieved and containsWholeWordRun confirms the run.
// Regression for the raw-substring prefilter that dropped such links.
func TestResolveWikilink_PartialNameMatchesAcrossPunctuation(t *testing.T) {
	s := newStack(t)
	projUID := s.seedEntity(t, ontology.EntityKindProject, "Apollo-Launch Project")

	res, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "note.md",
		[]byte("# Note\n\n[[Apollo Launch]] is on track.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	if res.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0 (token-equivalent name must resolve across punctuation)", res.Unresolved)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	edges := s.outgoing(t, doc.DocumentUID, "mentions")
	if len(edges) != 1 || edges[0].ObjectUID != projUID {
		t.Errorf("mentions = %+v, want one edge -> %s (Apollo-Launch Project)", edges, projUID)
	}
}

// TestImportDir_PartialNameResolvesAgainstStagedDoc proves an intra-batch
// short-form link resolves against a document staged in the SAME batch: a.md
// links [[Rosie]] and b.md is titled "Rosie Cotton". Neither is durable when
// resolution runs, so the partial lane must see the staged b.md through the
// batch overlay's NameLike path. Regression for the overlay only merging staged
// docs on NameExact.
func TestImportDir_PartialNameResolvesAgainstStagedDoc(t *testing.T) {
	s := newStack(t)
	vault := writeVault(t, map[string]string{
		"a.md": "# A\n\n[[Rosie]] leads.\n",
		"b.md": "# Rosie Cotton\n\nProfile.\n",
	})

	res, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if res.Imported != 2 {
		t.Fatalf("Imported = %d, want 2", res.Imported)
	}
	if res.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0 (intra-batch partial name must resolve against the staged doc)", res.Unresolved)
	}

	docA, err := s.nav.GetDocumentByPath(s.ctx, "a.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath(a): %v", err)
	}
	docB, err := s.nav.GetDocumentByPath(s.ctx, "b.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath(b): %v", err)
	}
	edges := s.outgoing(t, docA.DocumentUID, "links_to")
	if len(edges) != 1 || edges[0].ObjectUID != docB.DocumentUID {
		t.Errorf("a.md links_to = %+v, want one edge -> %s (b.md, Rosie Cotton)", edges, docB.DocumentUID)
	}
}

// TestImportDir_PartialNameAmbiguousAcrossStagedAndProjected proves the staged
// overlay does not hide an ambiguity that spans the batch and the projection: a
// pre-existing "Rosie Connor" plus a staged "Rosie Cotton" both whole-word-match
// [[Rosie]], so the link must stay dangling rather than pick the staged doc.
func TestImportDir_PartialNameAmbiguousAcrossStagedAndProjected(t *testing.T) {
	s := newStack(t)
	s.seedEntity(t, ontology.EntityKindPerson, "Rosie Connor")

	vault := writeVault(t, map[string]string{
		"a.md": "# A\n\n[[Rosie]] leads.\n",
		"b.md": "# Rosie Cotton\n\nProfile.\n",
	})
	res, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if res.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1 (staged + projected whole-word matches must dangle as ambiguous)", res.Unresolved)
	}
	docA, err := s.nav.GetDocumentByPath(s.ctx, "a.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath(a): %v", err)
	}
	if edges := s.outgoing(t, docA.DocumentUID, "links_to"); len(edges) != 0 {
		t.Errorf("a.md links_to = %+v, want none (ambiguous match must not emit an edge)", edges)
	}
	if edges := s.outgoing(t, docA.DocumentUID, "mentions"); len(edges) != 0 {
		t.Errorf("a.md mentions = %+v, want none (ambiguous match must not emit an edge)", edges)
	}
}
