package render_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/render"
)

// docRevKind is the kernel.EventKind for a documentRevision event.
var docRevKind = kernel.EventKind(ontology.KindDocumentRevision)

// docDelKind is the kernel.EventKind for a documentDeleted tombstone event.
var docDelKind = kernel.EventKind(ontology.KindDocumentDeleted)

// docRevisionPayload builds a minimal valid ontology.DocumentRevision payload.
func docRevisionPayload(docUID, path, title, body string) ontology.DocumentRevision {
	return ontology.DocumentRevision{
		DocumentUID: docUID,
		Path:        path,
		Title:       title,
		Body:        body,
		ContentHash: fmt.Sprintf("%064x", len(body)), // stand-in; render does not verify
	}
}

// firstDocFile returns the single rendered/documents/*.md file, failing if the
// count is not exactly one.
func firstDocFile(t *testing.T, files []render.File) *render.File {
	t.Helper()
	docs := pathsContaining(files, "rendered/documents/")
	if len(docs) != 1 {
		t.Fatalf("want exactly one rendered document file, got %d: %v", len(docs), docs)
	}
	return fileByPath(files, docs[0])
}

func TestRender_DocumentRevision_EmitsVerbatimBody(t *testing.T) {
	t.Parallel()

	docUID := ulidAt(1)
	body := "---\ntitle: Alpha\n---\n\n# Alpha\n\nA note about [[Rosie]].\n"
	events := []kernel.Event{
		makeEvent(ulidAt(1), ontology.KindEntity, entityPayload(docUID, "notes/alpha.md", ontology.EntityKind("document"))),
		makeEvent(ulidAt(2), docRevKind, docRevisionPayload(docUID, "notes/alpha.md", "Alpha", body)),
	}

	view, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	f := firstDocFile(t, view.Files)
	if string(f.Content) != body {
		t.Errorf("rendered body not verbatim:\n got %q\nwant %q", string(f.Content), body)
	}
	if f.Action != render.ActionCreate {
		t.Errorf("Action = %v, want ActionCreate", f.Action)
	}
	if view.Stats.DocumentsAdded != 1 {
		t.Errorf("DocumentsAdded = %d, want 1", view.Stats.DocumentsAdded)
	}
	// The slug must be derived from the path base ("alpha"), with a UID suffix.
	if !strings.Contains(f.Path, "rendered/documents/alpha__") {
		t.Errorf("doc path = %q, want slug derived from path base 'alpha'", f.Path)
	}
}

func TestRender_DocumentDeleted_NoBodyFileAndCounted(t *testing.T) {
	t.Parallel()

	docUID := ulidAt(1)
	// A revision followed by a tombstone in one stream: the head state is
	// deleted, so no body file is emitted and the delete is counted.
	events := []kernel.Event{
		makeEvent(ulidAt(1), ontology.KindEntity, entityPayload(docUID, "notes/a.md", ontology.EntityKind("document"))),
		makeEvent(ulidAt(2), docRevKind, docRevisionPayload(docUID, "notes/a.md", "A", "body one\n")),
		makeEvent(ulidAt(3), docDelKind, ontology.DocumentDeleted{DocumentUID: docUID, Path: "notes/a.md", Reason: "obsolete"}),
	}
	view, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if docs := pathsContaining(view.Files, "rendered/documents/"); len(docs) != 0 {
		t.Errorf("tombstoned document still rendered a body file: %v", docs)
	}
	if view.Stats.DocumentsDeleted != 1 {
		t.Errorf("DocumentsDeleted = %d, want 1", view.Stats.DocumentsDeleted)
	}
	if view.Stats.DocumentsAdded != 0 {
		t.Errorf("DocumentsAdded = %d, want 0 (added-then-deleted nets to deleted)", view.Stats.DocumentsAdded)
	}
}

func TestRender_DocumentMultipleRevisions_HeadOnly(t *testing.T) {
	t.Parallel()

	docUID := ulidAt(1)
	events := []kernel.Event{
		makeEvent(ulidAt(1), ontology.KindEntity, entityPayload(docUID, "notes/a.md", ontology.EntityKind("document"))),
		makeEvent(ulidAt(2), docRevKind, docRevisionPayload(docUID, "notes/a.md", "A", "v1\n")),
		makeEvent(ulidAt(3), docRevKind, docRevisionPayload(docUID, "notes/a.md", "A", "v2 changed\n")),
	}
	view, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Two revisions of one document collapse to one added document and one file.
	if view.Stats.DocumentsAdded != 1 {
		t.Errorf("DocumentsAdded = %d, want 1", view.Stats.DocumentsAdded)
	}
	f := firstDocFile(t, view.Files)
	if string(f.Content) != "v2 changed\n" {
		t.Errorf("body = %q, want the latest revision", string(f.Content))
	}
}

func TestRender_DocumentSlugFrozenAcrossRename(t *testing.T) {
	t.Parallel()

	docUID := ulidAt(1)
	// First revision at notes/a.md; a later revision moves it to notes/b.md.
	events := []kernel.Event{
		makeEvent(ulidAt(1), ontology.KindEntity, entityPayload(docUID, "notes/a.md", ontology.EntityKind("document"))),
		makeEvent(ulidAt(2), docRevKind, docRevisionPayload(docUID, "notes/a.md", "A", "one\n")),
		makeEvent(ulidAt(3), docRevKind, docRevisionPayload(docUID, "notes/b.md", "A", "two\n")),
	}
	view, err := render.Render(render.Input{Scope: kernel.ScopeProject, Events: seqEvents(events)})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	f := firstDocFile(t, view.Files)
	// Slug frozen at the first path ("a"), not the renamed path ("b").
	if !strings.Contains(f.Path, "rendered/documents/a__") {
		t.Errorf("slug not frozen across rename: path = %q, want slug 'a__…'", f.Path)
	}
	if string(f.Content) != "two\n" {
		t.Errorf("body = %q, want latest revision after rename", string(f.Content))
	}
}
