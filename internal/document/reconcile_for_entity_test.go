package document

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// reconcileAppender is a minimal Appender stub that captures appended events.
type reconcileAppender struct {
	events []kernel.Event
	err    error
}

func (a *reconcileAppender) Append(_ context.Context, evs []kernel.Event) ([]kernel.Receipt, error) {
	if a.err != nil {
		return nil, a.err
	}
	a.events = append(a.events, evs...)
	receipts := make([]kernel.Receipt, len(evs))
	return receipts, nil
}

// reconcileReader extends fakeReader with controllable DocumentsWithUnresolvedLinks
// and EntityRelations results, so ReconcileForEntity can be tested in isolation.
type reconcileReader struct {
	fakeReader
	unresolvedDocs []navigate.UnresolvedLinkDoc
	relations      []navigate.Relation
}

func (r *reconcileReader) DocumentsWithUnresolvedLinks(context.Context) ([]navigate.UnresolvedLinkDoc, error) {
	return r.unresolvedDocs, nil
}

func (r *reconcileReader) EntityRelations(_ context.Context, _ string, _ navigate.RelationFilter) ([]navigate.Relation, error) {
	return r.relations, nil
}

// mustMarshalLinks encodes a []ontology.WikiLink to JSON for use as
// UnresolvedJSON in test fixtures.
func mustMarshalLinks(t *testing.T, links []ontology.WikiLink) string {
	t.Helper()
	b, err := json.Marshal(links)
	if err != nil {
		t.Fatalf("marshal links: %v", err)
	}
	return string(b)
}

// TestReconcileForEntity_EmitsMentionsEdge is the core G6 regression test:
// seed a document with a dangling [[Foo]] wikilink, then call
// ReconcileForEntity with an entity named "Foo" that now exists. Expect exactly
// one relation event to be emitted.
func TestReconcileForEntity_EmitsMentionsEdge(t *testing.T) {
	t.Parallel()

	const (
		docUID    = "01ARZ3NDEKTSV4RRFFQ69G5FA0"
		entityUID = "01ARZ3NDEKTSV4RRFFQ69G5FB0"
		revUID    = "01ARZ3NDEKTSV4RRFFQ69G5FC0"
	)

	links := []ontology.WikiLink{{Raw: "[[Foo]]", Target: "Foo"}}
	rdr := &reconcileReader{
		fakeReader: fakeReader{
			entities: map[string][]navigate.Entity{
				"foo": {{UID: entityUID, Kind: string(ontology.EntityKindPerson), DisplayName: "Foo"}},
			},
		},
		unresolvedDocs: []navigate.UnresolvedLinkDoc{{
			DocumentUID:    docUID,
			Path:           "notes/bar.md",
			RevisionUID:    revUID,
			UnresolvedJSON: mustMarshalLinks(t, links),
		}},
	}
	app := &reconcileAppender{}
	im := NewImporter(app, rdr, nil)

	n, err := im.ReconcileForEntity(context.Background(), entityUID, []string{"Foo"})
	if err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	if n != 1 {
		t.Errorf("reconciled %d edges, want 1", n)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	if string(app.events[0].Kind) != string(ontology.KindRelation) {
		t.Errorf("event kind = %q, want %q", app.events[0].Kind, ontology.KindRelation)
	}
	// Verify the relation payload points from the document to the new entity.
	var rel ontology.Relation
	if err := json.Unmarshal(app.events[0].Payload, &rel); err != nil {
		t.Fatalf("unmarshal relation: %v", err)
	}
	if rel.SubjectUID != docUID {
		t.Errorf("SubjectUID = %q, want %q", rel.SubjectUID, docUID)
	}
	if rel.ObjectUID != entityUID {
		t.Errorf("ObjectUID = %q, want %q", rel.ObjectUID, entityUID)
	}
	if rel.Predicate != predicateMentions {
		t.Errorf("Predicate = %q, want %q", rel.Predicate, predicateMentions)
	}
}

// TestReconcileForEntity_Idempotent verifies that calling ReconcileForEntity
// twice does not emit a duplicate edge: the second call must see the existing
// relation in EntityRelations and skip re-emitting it.
func TestReconcileForEntity_Idempotent(t *testing.T) {
	t.Parallel()

	const (
		docUID    = "01ARZ3NDEKTSV4RRFFQ69G5FA1"
		entityUID = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
		revUID    = "01ARZ3NDEKTSV4RRFFQ69G5FC1"
		relUID    = "01ARZ3NDEKTSV4RRFFQ69G5FD1"
	)

	links := []ontology.WikiLink{{Raw: "[[Bar]]", Target: "Bar"}}
	// Pre-seed the relation that the first reconcile would have emitted.
	existingRelation := navigate.Relation{
		UID:        relUID,
		SubjectUID: docUID,
		Predicate:  predicateMentions,
		ObjectUID:  entityUID,
	}
	rdr := &reconcileReader{
		fakeReader: fakeReader{
			entities: map[string][]navigate.Entity{
				"bar": {{UID: entityUID, Kind: string(ontology.EntityKindPerson), DisplayName: "Bar"}},
			},
		},
		unresolvedDocs: []navigate.UnresolvedLinkDoc{{
			DocumentUID:    docUID,
			Path:           "notes/baz.md",
			RevisionUID:    revUID,
			UnresolvedJSON: mustMarshalLinks(t, links),
		}},
		// The edge already exists in the relations table.
		relations: []navigate.Relation{existingRelation},
	}
	app := &reconcileAppender{}
	im := NewImporter(app, rdr, nil)

	n, err := im.ReconcileForEntity(context.Background(), entityUID, []string{"Bar"})
	if err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	if n != 0 {
		t.Errorf("reconciled %d edges, want 0 (already present)", n)
	}
	if len(app.events) != 0 {
		t.Errorf("appended %d events, want 0 on duplicate suppression", len(app.events))
	}
}

// TestReconcileForEntity_SkipsNonMatchingDoc verifies the name pre-filter:
// a document with a dangling link to a different name must be skipped entirely
// (resolver and appender must not be called).
func TestReconcileForEntity_SkipsNonMatchingDoc(t *testing.T) {
	t.Parallel()

	const entityUID = "01ARZ3NDEKTSV4RRFFQ69G5FB2"

	links := []ontology.WikiLink{{Raw: "[[Unrelated]]", Target: "Unrelated"}}
	rdr := &reconcileReader{
		fakeReader: fakeReader{
			entities: map[string][]navigate.Entity{
				"qux": {{UID: entityUID, Kind: string(ontology.EntityKindPerson), DisplayName: "Qux"}},
			},
		},
		unresolvedDocs: []navigate.UnresolvedLinkDoc{{
			DocumentUID:    "01ARZ3NDEKTSV4RRFFQ69G5FA2",
			Path:           "notes/skip.md",
			RevisionUID:    "01ARZ3NDEKTSV4RRFFQ69G5FC2",
			UnresolvedJSON: mustMarshalLinks(t, links),
		}},
	}
	app := &reconcileAppender{}
	im := NewImporter(app, rdr, nil)

	n, err := im.ReconcileForEntity(context.Background(), entityUID, []string{"Qux"})
	if err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	if n != 0 {
		t.Errorf("reconciled %d edges, want 0 (target name mismatch)", n)
	}
	if len(app.events) != 0 {
		t.Errorf("appended %d events, want 0 (doc skipped by pre-filter)", len(app.events))
	}
}

// TestReconcileForEntity_AlternateName verifies that a dangling link resolves
// when it matches an alternate name (not the display name).
func TestReconcileForEntity_AlternateName(t *testing.T) {
	t.Parallel()

	const (
		docUID    = "01ARZ3NDEKTSV4RRFFQ69G5FA3"
		entityUID = "01ARZ3NDEKTSV4RRFFQ69G5FB3"
		revUID    = "01ARZ3NDEKTSV4RRFFQ69G5FC3"
	)

	// Document links to "Pippin" but the entity is registered as "Peregrin Took".
	links := []ontology.WikiLink{{Raw: "[[Pippin]]", Target: "Pippin"}}
	rdr := &reconcileReader{
		fakeReader: fakeReader{
			entities: map[string][]navigate.Entity{
				"pippin": {{UID: entityUID, Kind: string(ontology.EntityKindPerson), DisplayName: "Pippin"}},
			},
		},
		unresolvedDocs: []navigate.UnresolvedLinkDoc{{
			DocumentUID:    docUID,
			Path:           "notes/shire.md",
			RevisionUID:    revUID,
			UnresolvedJSON: mustMarshalLinks(t, links),
		}},
	}
	app := &reconcileAppender{}
	im := NewImporter(app, rdr, nil)

	// Pass "Peregrin Took" as display name and "Pippin" as alternate name.
	n, err := im.ReconcileForEntity(context.Background(), entityUID, []string{"Peregrin Took", "Pippin"})
	if err != nil {
		t.Fatalf("ReconcileForEntity: %v", err)
	}
	if n != 1 {
		t.Errorf("reconciled %d edges via alternate name, want 1", n)
	}
}

// TestReconcileForEntity_EmptyNames_NoOp verifies that passing no names is a
// cheap no-op: no reader call, no appender call.
func TestReconcileForEntity_EmptyNames_NoOp(t *testing.T) {
	t.Parallel()

	app := &reconcileAppender{}
	// Wiring a nil reader is safe here because ReconcileForEntity must return
	// before touching the reader when names is empty.
	im := NewImporter(app, &reconcileReader{}, nil)

	n, err := im.ReconcileForEntity(context.Background(), "any-uid", nil)
	if err != nil {
		t.Fatalf("ReconcileForEntity with no names: %v", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	if len(app.events) != 0 {
		t.Errorf("appended %d events, want 0 for empty names", len(app.events))
	}
}
