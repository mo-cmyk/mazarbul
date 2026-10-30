package git

// commit_documents_test.go pins the document additions to the structured commit
// message: the subject gains "+N documents", the body grows a Documents section,
// repeated revisions of one document collapse to a single row, and revision vs
// new-document is distinguished by previousRevisionUid.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func docRevEvent(uid, docUID, path, title string, prev string) kernel.Event {
	p := ontology.DocumentRevision{DocumentUID: docUID, Path: path, Title: title, Body: "x", ContentHash: strings.Repeat("a", 64)}
	if prev != "" {
		p.PreviousRevisionUID = &prev
	}
	raw, _ := json.Marshal(p)
	return kernel.Event{UID: uid, Kind: kernel.EventKind(ontology.KindDocumentRevision), Payload: raw}
}

func docDelEvent(uid, docUID, path string) kernel.Event {
	raw, _ := json.Marshal(ontology.DocumentDeleted{DocumentUID: docUID, Path: path, Reason: "dup"})
	return kernel.Event{UID: uid, Kind: kernel.EventKind(ontology.KindDocumentDeleted), Payload: raw}
}

func TestAppendEventsToDetails_ClassifiesDocuments(t *testing.T) {
	d := &commitDetails{}
	appendEventsToDetails(d, []kernel.Event{
		docRevEvent("01EV1", "01DOCA", "notes/a.md", "A", ""),         // new → "+"
		docRevEvent("01EV2", "01DOCB", "notes/b.md", "B", "01PREV00"), // revision → "~"
		docDelEvent("01EV3", "01DOCC", "notes/c.md"),                  // tombstone → "-"
	})
	if len(d.Documents) != 3 {
		t.Fatalf("Documents = %d, want 3", len(d.Documents))
	}
	byUID := map[string]documentChange{}
	for _, dc := range d.Documents {
		byUID[dc.UID] = dc
	}
	if byUID["01DOCA"].Action != "+" {
		t.Errorf("first revision action = %q, want +", byUID["01DOCA"].Action)
	}
	if byUID["01DOCB"].Action != "~" {
		t.Errorf("chained revision action = %q, want ~", byUID["01DOCB"].Action)
	}
	if byUID["01DOCC"].Action != "-" {
		t.Errorf("tombstone action = %q, want -", byUID["01DOCC"].Action)
	}
}

func TestDedupeDocuments_CollapsesRepeatRevisions(t *testing.T) {
	d := &commitDetails{}
	appendEventsToDetails(d, []kernel.Event{
		docRevEvent("01EV1", "01DOCA", "notes/a.md", "A", ""),         // +
		docRevEvent("01EV2", "01DOCA", "notes/a.md", "A", "01EV1000"), // same doc, ~
		docDelEvent("01EV3", "01DOCA", "notes/a.md"),                  // same doc, -
	})
	dedupeAndResolve(d)
	if len(d.Documents) != 1 {
		t.Fatalf("after dedupe Documents = %d, want 1", len(d.Documents))
	}
	if d.Documents[0].Action != "-" {
		t.Errorf("collapsed action = %q, want - (last write wins)", d.Documents[0].Action)
	}
}

func TestBuildCommitMessage_DocumentsInSubjectAndBody(t *testing.T) {
	s := sampleSummary()
	s.Details.Documents = []documentChange{
		{UID: "01DOCA", Path: "notes/alpha.md", Title: "Alpha", Action: "+"},
		{UID: "01DOCB", Path: "notes/beta.md", Title: "Beta", Action: "~"},
	}
	msg := buildCommitMessage(s)
	if !strings.Contains(firstLine(msg), "+2 documents") {
		t.Errorf("subject missing +2 documents: %q", firstLine(msg))
	}
	for _, want := range []string{
		"Documents:",
		"+ documents/01DOCA",
		"notes/alpha.md",
		"~ documents/01DOCB",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, msg)
		}
	}
}

func TestSubjectCountsPlaintext_SingularDocument(t *testing.T) {
	d := &commitDetails{Documents: []documentChange{{UID: "01DOCA", Path: "notes/a.md", Action: "+"}}}
	if got := subjectCountsPlaintext(d); !strings.Contains(got, "+1 document") || strings.Contains(got, "+1 documents") {
		t.Errorf("subjectCountsPlaintext = %q, want singular '+1 document'", got)
	}
}
