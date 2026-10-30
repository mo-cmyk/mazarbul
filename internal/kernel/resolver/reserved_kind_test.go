package resolver

// reserved_kind_test.go pins the reserved-extraction-kind guard: the structural
// `document` kind is owned by the document write path (a document entity must
// own a documents-table row + body + path). Extraction must NEVER mint a
// `document`-kind entity — that produces a body-less "ghost document" that
// breaks every doc-oriented surface (recall doc views, `maz doc show`, the
// explorer). The guard remaps an extracted `document` kind to the `topic`
// concept catch-all, and the lexicon's document-ish aliases (file/report/...)
// no longer funnel onto the structural kind either.

import (
	"testing"
)

// TestRemapReservedExtractionKind verifies the resolver remaps the structural
// `document` kind to `topic` on the extraction path and leaves every other kind
// untouched. This is the exact method resolveMentions calls on each mention.
func TestRemapReservedExtractionKind(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newKindTestResolver(t, log, &countingChat{}, store)

	if got := r.remapReservedExtractionKind(ctx, "document", &ingestMintCounter{}); got != "topic" {
		t.Errorf("remapReservedExtractionKind(document) = %q, want topic (extraction must not mint the structural document kind)", got)
	}

	// Non-reserved kinds pass through unchanged.
	for _, kind := range []string{"person", "organization", "place", "project", "event", "topic", "tool", "standard"} {
		if got := r.remapReservedExtractionKind(ctx, kind, &ingestMintCounter{}); got != kind {
			t.Errorf("remapReservedExtractionKind(%q) = %q, want %q (only document is reserved)", kind, got, kind)
		}
	}
}

// TestEntityKindLexicon_NoAliasTargetsDocument guards against the regression
// that produced ghost documents for extracted standards: the lexicon must not alias any
// surface form onto the structural `document` kind, so an extracted "report" /
// "file" / "paper" never becomes a body-less document entity.
func TestEntityKindLexicon_NoAliasTargetsDocument(t *testing.T) {
	t.Parallel()
	for surface, entry := range entityKindLexicon {
		if entry.entryType == kindLexiconAlias && entry.target == "document" {
			t.Errorf("lexicon alias %q targets the structural kind %q; extraction must not mint documents", surface, entry.target)
		}
	}
}

// TestAutoRegisterEntityKind_DocumentishAliasesAvoidStructuralDocument drives
// the real autoRegisterEntityKind path for the document-ish surface forms and
// confirms none of them resolve to the structural `document` kind (they now
// land on `topic`). This is deterministic — the lexicon alias path takes no LLM
// round-trip.
func TestAutoRegisterEntityKind_DocumentishAliasesAvoidStructuralDocument(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openKindStore(t)
	log := &captureLog{}
	r := newKindTestResolver(t, log, &countingChat{}, store)

	for _, surface := range []string{"file", "report", "article", "paper", "email", "note"} {
		got, err := r.autoRegisterEntityKind(ctx, surface, "", &ingestMintCounter{})
		if err != nil {
			t.Fatalf("autoRegisterEntityKind(%q): %v", surface, err)
		}
		if got == "document" {
			t.Errorf("autoRegisterEntityKind(%q) = document; document-ish forms must not become structural documents", surface)
		}
		if got != "topic" {
			t.Errorf("autoRegisterEntityKind(%q) = %q, want topic", surface, got)
		}
	}
}
