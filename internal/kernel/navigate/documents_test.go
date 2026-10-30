package navigate_test

import (
	"context"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// TestDocumentUnresolvedLinkTargets proves the per-document snapshot read: a
// document's write-time unresolved-link JSON is decoded to its target strings;
// an empty/absent snapshot and an unknown document both yield no targets.
func TestDocumentUnresolvedLinkTargets(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "d1", "document", "Doc One", 1.0, "confirmed", now)
	exec(ent, "d2", "document", "Doc Two", 1.0, "confirmed", now)

	doc := `INSERT INTO documents (document_uid, path, title, content_hash, revision_uid, create_time, update_time, deleted, unresolved_links_json) VALUES (?,?,?,?,?,?,?,?,?)`
	exec(doc, "d1", "notes/d1.md", "Doc One", "h1", "r1", now, now, 0, `[{"raw":"[[Ghost]]","target":"Ghost"},{"raw":"[[Phantom]]","target":"Phantom"}]`)
	exec(doc, "d2", "notes/d2.md", "Doc Two", "h2", "r2", now, now, 0, `[]`)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	ctx := context.Background()

	got, err := nav.DocumentUnresolvedLinkTargets(ctx, "d1")
	if err != nil {
		t.Fatalf("DocumentUnresolvedLinkTargets(d1): %v", err)
	}
	if len(got) != 2 || got[0] != "Ghost" || got[1] != "Phantom" {
		t.Errorf("d1 targets = %v, want [Ghost Phantom]", got)
	}

	empty, err := nav.DocumentUnresolvedLinkTargets(ctx, "d2")
	if err != nil || len(empty) != 0 {
		t.Errorf("d2 targets = %v (err %v), want none", empty, err)
	}

	missing, err := nav.DocumentUnresolvedLinkTargets(ctx, "nope")
	if err != nil || len(missing) != 0 {
		t.Errorf("missing-doc targets = %v (err %v), want none", missing, err)
	}
}

// TestDanglingDocumentEdges proves the lint query behind the deleteDocument
// contract: a live document that still links to a now-DELETED document is
// surfaced as a dangling edge, while edges to live documents, edges to
// non-document entities, edges whose own subject is deleted, and superseded
// edges are all excluded. This is what makes `maz doc lint` honour its promise
// that "backlinks to a deleted note are left intact as dead links."
func TestDanglingDocumentEdges(t *testing.T) {
	t.Parallel()
	store, exec := openNavStore(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)

	ent := `INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`
	exec(ent, "linker", "document", "Linker Note", 1.0, "confirmed", now)
	exec(ent, "gone", "document", "Gone Note", 1.0, "confirmed", now)
	exec(ent, "live2", "document", "Live Two", 1.0, "confirmed", now)
	exec(ent, "person", "person", "Dana Okafor", 1.0, "confirmed", now)

	// documents rows: linker + live2 are live; gone is tombstoned.
	doc := `INSERT INTO documents (document_uid, path, title, content_hash, revision_uid, create_time, update_time, deleted) VALUES (?,?,?,?,?,?,?,?)`
	exec(doc, "linker", "notes/linker.md", "Linker Note", "h1", "r1", now, now, 0)
	exec(doc, "gone", "notes/gone.md", "Gone Note", "h2", "r2", now, now, 1)
	exec(doc, "live2", "notes/live2.md", "Live Two", "h3", "r3", now, now, 0)

	rel := `INSERT INTO relations (uid, subject_uid, predicate, object_uid, confidence, source_event_uid, create_time, valid_from, valid_until, superseded_by_uid, attributes) VALUES (?,?,?,?,?,?,?,?,?,?,?)`
	// SHOULD surface: live linker -> deleted gone.
	exec(rel, "r_dangling", "linker", "links_to", "gone", 1.0, "ep1", now, nil, nil, nil, `{}`)
	// Excluded: target is live.
	exec(rel, "r_live", "linker", "links_to", "live2", 1.0, "ep2", now, nil, nil, nil, `{}`)
	// Excluded: object is a person, not a document.
	exec(rel, "r_person", "linker", "mentions", "person", 1.0, "ep3", now, nil, nil, nil, `{}`)
	// Excluded: subject is the deleted doc (outgoing edge from a tombstone).
	exec(rel, "r_outgoing", "gone", "mentions", "person", 1.0, "ep4", now, nil, nil, nil, `{}`)
	// Excluded: superseded edge to the deleted doc.
	exec(rel, "r_superseded", "live2", "links_to", "gone", 1.0, "ep5", now, nil, nil, "r_newer", `{}`)
	// Excluded: temporally-closed edge (valid_until in the past) to the deleted
	// doc — a closed relation is no longer current truth, so it must not surface
	// as a live dangling lint issue.
	exec(rel, "r_closed", "live2", "links_to", "gone", 1.0, "ep6", now, nil, now, nil, `{}`)
	// Excluded: future-dated edge (valid_from in the future) to the deleted doc —
	// not yet active, so every current-relation reader hides it; the lint must
	// not report a link nothing else considers live.
	exec(rel, "r_future", "live2", "links_to", "gone", 1.0, "ep7", now, future, nil, nil, `{}`)
	// SHOULD surface: live2 -> deleted gone with a FUTURE valid_until is still
	// open right now (current truth), so it is a genuine dangling edge.
	exec(rel, "r_openfuture", "live2", "links_to", "gone", 1.0, "ep8", now, nil, future, nil, `{}`)

	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	got, err := nav.DanglingDocumentEdges(context.Background())
	if err != nil {
		t.Fatalf("DanglingDocumentEdges: %v", err)
	}
	// Expect exactly the two currently-current dangling edges: linker->gone and
	// live2->gone (the open future-valid_until one). The superseded, past-closed,
	// and future-valid_from edges to gone are all excluded.
	type pair struct{ subj, obj string }
	gotPairs := map[pair]LintDanglingEdgeView{}
	for _, e := range got {
		gotPairs[pair{e.SubjectUID, e.TargetUID}] = LintDanglingEdgeView{e.SubjectPath, e.TargetPath, e.Predicate, e.TargetTitle}
	}
	if len(got) != 2 {
		t.Fatalf("got %d dangling edges, want exactly 2 (linker->gone, live2->gone open-future): %+v", len(got), got)
	}
	linker, ok := gotPairs[pair{"linker", "gone"}]
	if !ok {
		t.Fatalf("missing linker->gone dangling edge: %+v", got)
	}
	if linker.SubjectPath != "notes/linker.md" || linker.TargetPath != "notes/gone.md" ||
		linker.Predicate != "links_to" || linker.TargetTitle != "Gone Note" {
		t.Errorf("linker->gone = %+v, want notes/linker.md -> notes/gone.md links_to (Gone Note)", linker)
	}
	if _, ok := gotPairs[pair{"live2", "gone"}]; !ok {
		t.Errorf("missing live2->gone (open future-valid_until) dangling edge: %+v", got)
	}
}

// LintDanglingEdgeView flattens the fields asserted in the dangling-edge test.
type LintDanglingEdgeView struct {
	SubjectPath, TargetPath, Predicate, TargetTitle string
}
