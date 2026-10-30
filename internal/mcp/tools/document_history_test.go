package tools

import (
	"context"
	"encoding/json"
	"iter"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// fakeHistNav is a static docHistoryNavigator for the documentHistory tests.
type fakeHistNav struct {
	byPath  map[string]navigate.Document
	byUID   map[string]navigate.Document
	deleted []navigate.Document
}

func (f *fakeHistNav) GetDocumentByPath(_ context.Context, p string) (navigate.Document, error) {
	if d, ok := f.byPath[p]; ok {
		return d, nil
	}
	return navigate.Document{}, navigate.ErrNotFound
}

func (f *fakeHistNav) GetDocumentByUID(_ context.Context, uid string) (navigate.Document, error) {
	if d, ok := f.byUID[uid]; ok {
		return d, nil
	}
	return navigate.Document{}, navigate.ErrNotFound
}

func (f *fakeHistNav) DeletedDocuments(_ context.Context) ([]navigate.Document, error) {
	return f.deleted, nil
}

// fakeEventReader yields a fixed event slice, mimicking the event log's
// chronological iterator.
type fakeEventReader struct{ events []kernel.Event }

func (f *fakeEventReader) Read(_ context.Context, _ kernel.ReadOpts) iter.Seq2[kernel.Event, error] {
	return func(yield func(kernel.Event, error) bool) {
		for _, ev := range f.events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func histRevEvent(uid, docUID, body string, prev string) kernel.Event {
	p := ontology.DocumentRevision{DocumentUID: docUID, Path: "notes/a.md", Title: "A", Body: body, ContentHash: "h"}
	if prev != "" {
		p.PreviousRevisionUID = &prev
	}
	raw, _ := json.Marshal(p)
	return kernel.Event{UID: uid, Kind: kernel.EventKind(ontology.KindDocumentRevision), Payload: raw}
}

func histDelEvent(uid, docUID string) kernel.Event {
	return histDelEventAt(uid, docUID, "")
}

func histDelEventAt(uid, docUID, path string) kernel.Event {
	raw, _ := json.Marshal(ontology.DocumentDeleted{DocumentUID: docUID, Path: path, Reason: "obsolete"})
	return kernel.Event{UID: uid, Kind: kernel.EventKind(ontology.KindDocumentDeleted), Payload: raw}
}

func invokeHistory(t *testing.T, tool *DocumentHistory, in map[string]any) documentHistoryOutput {
	t.Helper()
	req, _ := json.Marshal(in)
	raw, err := tool.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out documentHistoryOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	return out
}

func TestDocumentHistory_OrdersAndFiltersByDocument(t *testing.T) {
	nav := &fakeHistNav{
		byPath: map[string]navigate.Document{"notes/a.md": {DocumentUID: "DOCA", Path: "notes/a.md"}},
		byUID:  map[string]navigate.Document{"DOCA": {DocumentUID: "DOCA", Path: "notes/a.md"}},
	}
	log := &fakeEventReader{events: []kernel.Event{
		histRevEvent("EV1", "DOCA", "v1", ""),
		histRevEvent("EV2", "DOCB", "other doc", ""), // different doc, must be filtered out
		histRevEvent("EV3", "DOCA", "v2", "EV1"),
		histDelEvent("EV4", "DOCA"),
	}}
	tool := NewDocumentHistory(log, nav)

	out := invokeHistory(t, tool, map[string]any{"path": "notes/a.md"})
	if out.DocumentUID != "DOCA" {
		t.Fatalf("DocumentUID = %q, want DOCA", out.DocumentUID)
	}
	if len(out.Revisions) != 3 {
		t.Fatalf("Revisions = %d, want 3 (DOCB filtered out)", len(out.Revisions))
	}
	wantUIDs := []string{"EV1", "EV3", "EV4"}
	for i, w := range wantUIDs {
		if out.Revisions[i].UID != w {
			t.Errorf("Revisions[%d].UID = %q, want %q (chronological)", i, out.Revisions[i].UID, w)
		}
	}
	if out.Revisions[2].Kind != docHistoryKindDeleted {
		t.Errorf("last entry Kind = %q, want %q", out.Revisions[2].Kind, docHistoryKindDeleted)
	}
	if out.Revisions[1].PreviousRevisionUID != "EV1" {
		t.Errorf("EV3 previousRevisionUid = %q, want EV1", out.Revisions[1].PreviousRevisionUID)
	}
}

func TestDocumentHistory_IncludeBodiesGate(t *testing.T) {
	nav := &fakeHistNav{
		byPath: map[string]navigate.Document{"notes/a.md": {DocumentUID: "DOCA", Path: "notes/a.md"}},
		byUID:  map[string]navigate.Document{"DOCA": {DocumentUID: "DOCA", Path: "notes/a.md"}},
	}
	log := &fakeEventReader{events: []kernel.Event{histRevEvent("EV1", "DOCA", "the body", "")}}
	tool := NewDocumentHistory(log, nav)

	off := invokeHistory(t, tool, map[string]any{"path": "notes/a.md"})
	if off.Revisions[0].Body != "" {
		t.Errorf("body present without includeBodies: %q", off.Revisions[0].Body)
	}
	if off.Revisions[0].Bytes != len("the body") {
		t.Errorf("Bytes = %d, want %d", off.Revisions[0].Bytes, len("the body"))
	}

	on := invokeHistory(t, tool, map[string]any{"path": "notes/a.md", "includeBodies": true})
	if on.Revisions[0].Body != "the body" {
		t.Errorf("body = %q, want 'the body' with includeBodies", on.Revisions[0].Body)
	}
}

func TestDocumentHistory_ResolvesDeletedDocViaTombstoneList(t *testing.T) {
	// No live document at the path; only a tombstone record exists.
	nav := &fakeHistNav{
		byPath:  map[string]navigate.Document{},
		byUID:   map[string]navigate.Document{},
		deleted: []navigate.Document{{DocumentUID: "DOCA", Path: "notes/gone.md"}},
	}
	log := &fakeEventReader{events: []kernel.Event{
		histRevEvent("EV1", "DOCA", "v1", ""),
		histDelEvent("EV2", "DOCA"),
	}}
	tool := NewDocumentHistory(log, nav)

	out := invokeHistory(t, tool, map[string]any{"path": "notes/gone.md"})
	if out.DocumentUID != "DOCA" {
		t.Fatalf("DocumentUID = %q, want DOCA via tombstone list", out.DocumentUID)
	}
	if len(out.Revisions) != 2 {
		t.Fatalf("Revisions = %d, want 2", len(out.Revisions))
	}
}

func TestDocumentHistory_ResolvesOldDeletedPathViaEventLog(t *testing.T) {
	// DOCA was deleted at notes/a.md, then revived/moved to notes/b.md. The live
	// projection knows it only at notes/b.md; the CURRENT tombstone list no
	// longer carries notes/a.md (HEAD state is live again). The old deletion
	// path survives only in the documentDeleted event, so `maz doc log
	// notes/a.md` must still resolve it from the log.
	nav := &fakeHistNav{
		byPath:  map[string]navigate.Document{"notes/b.md": {DocumentUID: "DOCA", Path: "notes/b.md"}},
		byUID:   map[string]navigate.Document{"DOCA": {DocumentUID: "DOCA", Path: "notes/b.md"}},
		deleted: nil, // HEAD is live, so no current tombstone
	}
	log := &fakeEventReader{events: []kernel.Event{
		histRevEvent("EV1", "DOCA", "v1", ""),
		histDelEventAt("EV2", "DOCA", "notes/a.md"),
		histRevEvent("EV3", "DOCA", "revived", ""),
	}}
	tool := NewDocumentHistory(log, nav)

	out := invokeHistory(t, tool, map[string]any{"path": "notes/a.md"})
	if out.DocumentUID != "DOCA" {
		t.Fatalf("DocumentUID = %q, want DOCA resolved via documentDeleted event", out.DocumentUID)
	}
	// The resolved path reflects the deletion-time path recorded in the log.
	if out.Path != "notes/a.md" {
		t.Errorf("Path = %q, want notes/a.md (deletion-time path)", out.Path)
	}
	if len(out.Revisions) != 3 {
		t.Fatalf("Revisions = %d, want 3 (full chain)", len(out.Revisions))
	}
}

func TestDocumentHistory_FlagsTruncatedBodies(t *testing.T) {
	big := make([]byte, docHistoryBodyCap+2048)
	for i := range big {
		big[i] = 'a'
	}
	nav := &fakeHistNav{
		byPath: map[string]navigate.Document{"notes/a.md": {DocumentUID: "DOCA", Path: "notes/a.md"}},
		byUID:  map[string]navigate.Document{"DOCA": {DocumentUID: "DOCA", Path: "notes/a.md"}},
	}
	log := &fakeEventReader{events: []kernel.Event{
		histRevEvent("EV1", "DOCA", "small", ""),
		histRevEvent("EV2", "DOCA", string(big), "EV1"),
	}}
	tool := NewDocumentHistory(log, nav)

	out := invokeHistory(t, tool, map[string]any{"path": "notes/a.md", "includeBodies": true})
	if out.Revisions[0].BodyTruncated {
		t.Errorf("small revision flagged truncated")
	}
	if !out.Revisions[1].BodyTruncated {
		t.Errorf("over-cap revision not flagged truncated")
	}
	// Bytes still reports the FULL size, not the capped length.
	if out.Revisions[1].Bytes != len(big) {
		t.Errorf("Bytes = %d, want full size %d", out.Revisions[1].Bytes, len(big))
	}
	if len(out.Revisions[1].Body) > docHistoryBodyCap+64 {
		t.Errorf("body not capped: %d bytes", len(out.Revisions[1].Body))
	}
}

func TestDocumentHistory_NotFound(t *testing.T) {
	nav := &fakeHistNav{byPath: map[string]navigate.Document{}, byUID: map[string]navigate.Document{}}
	tool := NewDocumentHistory(&fakeEventReader{}, nav)
	req, _ := json.Marshal(map[string]any{"path": "notes/missing.md"})
	_, err := tool.Invoke(context.Background(), req)
	wantToolErrorCode(t, err, mcp.CodeNotFound)
}

func TestDocumentHistory_RequiresExactlyOneSelector(t *testing.T) {
	tool := NewDocumentHistory(&fakeEventReader{}, &fakeHistNav{})
	req, _ := json.Marshal(map[string]any{"path": "a", "documentUid": "b"})
	_, err := tool.Invoke(context.Background(), req)
	wantToolErrorCode(t, err, mcp.CodeValidation)
}
