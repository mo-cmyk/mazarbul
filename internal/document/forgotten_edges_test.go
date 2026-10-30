package document

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// uidErrReader is an edgeReader whose GetDocumentByUID fails with a non-not-found
// error, to exercise the prose-liveness gate's read-error path.
type uidErrReader struct {
	edgeReader
	uidErr error
}

func (r *uidErrReader) GetDocumentByUID(context.Context, string) (navigate.Document, error) {
	return navigate.Document{}, r.uidErr
}

// stubProseExtractor returns a fixed mention set so runProseExtraction proceeds
// to the liveness gate.
type stubProseExtractor struct{ mentions []ontology.Mention }

func (s *stubProseExtractor) ExtractProse(context.Context, string, string) ([]ontology.Mention, error) {
	return s.mentions, nil
}

// TestProseExtraction_LivenessReadErrorAborts proves the prose-append liveness
// gate does NOT proceed when the document-existence check fails with a
// non-not-found error: liveness is UNKNOWN, so appending could resurrect an edge
// against a since-deleted document. The error is surfaced (caller can retry)
// rather than silently swallowed.
func TestProseExtraction_LivenessReadErrorAborts(t *testing.T) {
	t.Parallel()
	boom := errors.New("reader unavailable")
	im := NewImporter(nil, &uidErrReader{uidErr: boom}, nil).
		WithProseExtractor(&stubProseExtractor{mentions: []ontology.Mention{
			{Kind: ontology.EntityKindPerson, DisplayName: "Rosie Cotton", ExtractionText: "Rosie Cotton", GroundingScore: 0.9},
		}})

	pd := &pendingDoc{
		path:        "notes/x.md",
		documentUID: forgetDocUID,
		parsed:      Parsed{Body: "Rosie Cotton appears here."},
	}
	err := im.runProseExtraction(context.Background(), pd, "01ARZ3NDEKTSV4RRFFQ69G5FC0")
	if !errors.Is(err, boom) {
		t.Fatalf("runProseExtraction err = %v, want it to wrap the read error (no silent proceed)", err)
	}
}

// edgeReader is a minimal Reader whose EntityRelations returns a fixed set of
// active edges, so forgottenEdgeEvents can be exercised in isolation without a
// real dispatcher. Only EntityRelations carries behaviour; the rest satisfy the
// Reader interface and are never called by forgottenEdgeEvents.
type edgeReader struct {
	rels []navigate.Relation
}

func (e *edgeReader) EntityRelations(context.Context, string, navigate.RelationFilter) ([]navigate.Relation, error) {
	return e.rels, nil
}

func (*edgeReader) GetDocumentByPath(context.Context, string) (navigate.Document, error) {
	return navigate.Document{}, navigate.ErrNotFound
}

func (*edgeReader) GetDocumentByPathScope(context.Context, string, string) (navigate.Document, error) {
	return navigate.Document{}, navigate.ErrNotFound
}

func (*edgeReader) UniqueDocumentBySlug(context.Context, string) (navigate.Document, error) {
	return navigate.Document{}, navigate.ErrNotFound
}

func (*edgeReader) DocumentUIDsBySlug(context.Context, string) ([]string, error) {
	return nil, nil
}

func (*edgeReader) GetDocumentByUID(context.Context, string) (navigate.Document, error) {
	return navigate.Document{}, navigate.ErrNotFound
}

func (*edgeReader) ListEntities(context.Context, navigate.EntityFilter) ([]navigate.Entity, error) {
	return nil, nil
}

func (*edgeReader) DocumentsWithUnresolvedLinks(context.Context) ([]navigate.UnresolvedLinkDoc, error) {
	return nil, nil
}

const forgetDocUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// forgetTestEdges returns one wikilink edge and one prose-extracted edge, both
// owned by forgetDocUID, so a forget pass can be checked for the prose-skip
// gate and the verbatim reason.
func forgetTestEdges() []navigate.Relation {
	return []navigate.Relation{
		{
			UID:        "01ARZ3NDEKTSV4RRFFQ69G5FB0",
			SubjectUID: forgetDocUID,
			Predicate:  "mentions",
			ObjectUID:  "01ARZ3NDEKTSV4RRFFQ69G5FB1",
			Provenance: "wikilink [[Merry]]",
		},
		{
			UID:        "01ARZ3NDEKTSV4RRFFQ69G5FB2",
			SubjectUID: forgetDocUID,
			Predicate:  "mentions",
			ObjectUID:  "01ARZ3NDEKTSV4RRFFQ69G5FB3",
			Provenance: proseProvenancePrefix + "Sam",
		},
	}
}

// forgottenReasons decodes every emitted relationForgotten event and returns the
// forgotten relation UIDs mapped to their verbatim reason.
func forgottenReasons(t *testing.T, events []kernel.Event) map[string]string {
	t.Helper()
	out := make(map[string]string, len(events))
	for i := range events {
		var ev ontology.RelationForgottenEvent
		if err := json.Unmarshal(events[i].Payload, &ev); err != nil {
			t.Fatalf("decode relationForgotten payload: %v", err)
		}
		out[ev.RelationUID] = ev.Reason
	}
	return out
}

// TestForgottenEdgeEvents_DeleteUsesCauseAndForgetsProse asserts the delete path
// contract: includeProse=true forgets the prose edge alongside the wikilink
// edge, and the verbatim cause string lands in every RelationForgottenEvent.Reason
// (a delete must not masquerade as a wikilink-removed-in-revision forget).
func TestForgottenEdgeEvents_DeleteUsesCauseAndForgetsProse(t *testing.T) {
	t.Parallel()
	im := NewImporter(nil, &edgeReader{rels: forgetTestEdges()}, nil)

	events, err := im.forgottenEdgeEvents(context.Background(), kernel.DefaultScope, forgetDocUID, nil, "document deleted", true)
	if err != nil {
		t.Fatalf("forgottenEdgeEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("forgot %d edges, want 2 (wikilink + prose) on a delete", len(events))
	}
	reasons := forgottenReasons(t, events)
	for uid, reason := range reasons {
		if reason != "document deleted" {
			t.Errorf("relation %s reason = %q, want \"document deleted\"", uid, reason)
		}
	}
}

// TestForgottenEdgeEvents_ReconcileSkipsProse asserts the reconcile path
// contract: includeProse=false leaves the prose-extracted edge untouched (the
// async extractor reconciles it), so only the wikilink edge is forgotten.
func TestForgottenEdgeEvents_ReconcileSkipsProse(t *testing.T) {
	t.Parallel()
	im := NewImporter(nil, &edgeReader{rels: forgetTestEdges()}, nil)

	events, err := im.forgottenEdgeEvents(context.Background(), kernel.DefaultScope, forgetDocUID, nil, "wikilink removed in document revision 01ARZ3NDEKTSV4RRFFQ69G5FC0", false)
	if err != nil {
		t.Fatalf("forgottenEdgeEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("forgot %d edges, want 1 (wikilink only; prose preserved) on a reconcile", len(events))
	}
	if _, ok := forgottenReasons(t, events)["01ARZ3NDEKTSV4RRFFQ69G5FB0"]; !ok {
		t.Error("the wikilink edge must be forgotten on a reconcile pass")
	}
}
