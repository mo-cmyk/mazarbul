package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// fakeLintNav satisfies lintNavigator (document.Reader + the lint queries).
// Lookups that the dead-link re-resolution consults return empty/not-found, so
// a document carrying an unresolved-link snapshot stays dead — which is what
// the dead-link tests assert.
//
// ListEntities filters the entities slice by Kind and NameExact when set, so
// the collision-detection path can be exercised without a real store.
type fakeLintNav struct {
	orphans        []navigate.LintOrphanDoc
	contradictions []navigate.LintContradictionDoc
	dangling       []navigate.LintDanglingEdge
	unresolvedDocs []navigate.UnresolvedLinkDoc
	// entities is the pool ListEntities filters from for name-collision tests.
	entities []navigate.Entity
	// documentsByUID maps a document UID to its full Document record so
	// GetDocumentByUID can return the vault path during collision resolution.
	documentsByUID map[string]navigate.Document
}

func (f *fakeLintNav) GetDocumentByPath(context.Context, string) (navigate.Document, error) {
	return navigate.Document{}, navigate.ErrNotFound
}

func (f *fakeLintNav) GetDocumentByPathScope(context.Context, string, string) (navigate.Document, error) {
	return navigate.Document{}, navigate.ErrNotFound
}

func (f *fakeLintNav) GetDocumentByUID(_ context.Context, uid string) (navigate.Document, error) {
	if d, ok := f.documentsByUID[uid]; ok {
		return d, nil
	}
	return navigate.Document{}, navigate.ErrNotFound
}

// ListEntities filters f.entities by Kind (exact) and NameExact
// (case-insensitive). Both filters are ANDed; an empty filter field is ignored.
// Offset and Limit are honoured so the pagination loop in findNameCollisions
// terminates correctly on a small fixture.
func (f *fakeLintNav) ListEntities(_ context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error) {
	matched := make([]navigate.Entity, 0, len(f.entities))
	for _, e := range f.entities {
		if opts.Kind != "" && e.Kind != opts.Kind {
			continue
		}
		if opts.NameExact != "" && !strings.EqualFold(e.DisplayName, opts.NameExact) {
			continue
		}
		matched = append(matched, e)
	}
	// Apply offset.
	if opts.Offset > 0 {
		if opts.Offset >= len(matched) {
			return nil, nil
		}
		matched = matched[opts.Offset:]
	}
	// Apply limit (0 → return all matched, mirroring the navigator's default
	// for test simplicity — the fixture is always small).
	if opts.Limit > 0 && opts.Limit < len(matched) {
		matched = matched[:opts.Limit]
	}
	return matched, nil
}

func (f *fakeLintNav) EntityRelations(context.Context, string, navigate.RelationFilter) ([]navigate.Relation, error) {
	return nil, nil
}

func (f *fakeLintNav) DocumentsWithUnresolvedLinks(context.Context) ([]navigate.UnresolvedLinkDoc, error) {
	return f.unresolvedDocs, nil
}

func (f *fakeLintNav) OrphanDocuments(context.Context) ([]navigate.LintOrphanDoc, error) {
	return f.orphans, nil
}

func (f *fakeLintNav) DocumentContradictions(context.Context) ([]navigate.LintContradictionDoc, error) {
	return f.contradictions, nil
}

func (f *fakeLintNav) DanglingDocumentEdges(context.Context) ([]navigate.LintDanglingEdge, error) {
	return f.dangling, nil
}

func (f *fakeLintNav) NearDuplicateDocuments(context.Context, float32) ([]navigate.LintNearDuplicateDoc, error) {
	return nil, nil
}

// fakeSemanticResolver resolves every target to a fixed entity, standing in for
// the daemon's name-embedding lane so the lint tool's lazy resolver path can be
// exercised without an embedder.
type fakeSemanticResolver struct {
	uid  string
	kind string
}

func (f *fakeSemanticResolver) ResolveNameSemantically(context.Context, string, string) (string, string, bool) {
	return f.uid, f.kind, true
}

// TestLintDocuments_EmptyEmitsArrays proves a clean vault yields all six keys
// as empty arrays (never null), so an agent can iterate without a nil check.
func TestLintDocuments_EmptyEmitsArrays(t *testing.T) {
	tool := NewLintDocuments(&fakeLintNav{}, nil, nil)
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// Assert the wire literally carries arrays, not null.
	for _, key := range []string{
		`"orphans":[]`,
		`"deadLinks":[]`,
		`"contradictions":[]`,
		`"danglingEdges":[]`,
		`"nearDuplicates":[]`,
		`"nameCollisions":[]`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("output %s missing %s", raw, key)
		}
	}
}

// TestLintDocuments_ReportsDanglingEdges proves a live document still linking to
// a DELETED document surfaces in the danglingEdges category with the live
// subject and the deleted target — the lint signal that makes the deleteDocument
// "backlinks left intact as dead links" contract observable to an agent.
func TestLintDocuments_ReportsDanglingEdges(t *testing.T) {
	nav := &fakeLintNav{
		dangling: []navigate.LintDanglingEdge{{
			SubjectUID:  "linker",
			SubjectPath: "notes/linker.md",
			Predicate:   "links_to",
			TargetUID:   "gone",
			TargetPath:  "notes/gone.md",
			TargetTitle: "Gone Note",
		}},
	}
	tool := NewLintDocuments(nav, nil, nil)
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out lintDocumentsOutput
	if uErr := json.Unmarshal(raw, &out); uErr != nil {
		t.Fatalf("unmarshal: %v", uErr)
	}
	if len(out.DanglingEdges) != 1 {
		t.Fatalf("danglingEdges = %+v, want one", out.DanglingEdges)
	}
	de := out.DanglingEdges[0]
	if de.SubjectPath != "notes/linker.md" || de.TargetPath != "notes/gone.md" || de.TargetTitle != "Gone Note" {
		t.Errorf("dangling edge = %+v, want notes/linker.md -> notes/gone.md (Gone Note)", de)
	}
}

// TestLintDocuments_ReportsAllThreeCategories proves orphans, dead links, and
// contradictions all surface, and that a dead-link entry carries the clean
// documentUid/path/title wire shape (not the navigator's internal snapshot).
func TestLintDocuments_ReportsAllThreeCategories(t *testing.T) {
	nav := &fakeLintNav{
		orphans: []navigate.LintOrphanDoc{
			{DocumentUID: "o1", Path: "notes/orphan.md", Title: "Orphan"},
		},
		contradictions: []navigate.LintContradictionDoc{
			{SubjectUID: "a", SubjectPath: "a.md", ObjectUID: "b", ObjectPath: "b.md"},
		},
		// A document whose write-time snapshot names a target that resolves to
		// nothing (ListEntities/GetDocumentByPath all miss) → stays dead.
		unresolvedDocs: []navigate.UnresolvedLinkDoc{
			{
				DocumentUID:    "d1",
				Path:           "notes/dangling.md",
				Title:          "Dangling",
				RevisionUID:    "rev1",
				UnresolvedJSON: `[{"raw":"[[Ghost]]","target":"Ghost"}]`,
			},
		},
	}
	tool := NewLintDocuments(nav, nil, nil)
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out lintDocumentsOutput
	if uErr := json.Unmarshal(raw, &out); uErr != nil {
		t.Fatalf("unmarshal: %v", uErr)
	}
	if len(out.Orphans) != 1 || out.Orphans[0].Path != "notes/orphan.md" {
		t.Errorf("orphans = %+v, want one notes/orphan.md", out.Orphans)
	}
	if len(out.Contradictions) != 1 || out.Contradictions[0].SubjectPath != "a.md" {
		t.Errorf("contradictions = %+v, want one a.md<->b.md", out.Contradictions)
	}
	if len(out.DeadLinks) != 1 {
		t.Fatalf("deadLinks = %+v, want one", out.DeadLinks)
	}
	dl := out.DeadLinks[0]
	if dl.DocumentUID != "d1" || dl.Path != "notes/dangling.md" || dl.Title != "Dangling" {
		t.Errorf("deadLink wire = %+v, want {d1, notes/dangling.md, Dangling}", dl)
	}
	// The wire must name WHICH link is dead so the report is actionable.
	if len(dl.Unresolved) != 1 || dl.Unresolved[0] != "Ghost" {
		t.Errorf("deadLink unresolved = %+v, want [Ghost]", dl.Unresolved)
	}
}

// TestLintDocuments_SemanticResolverSuppressesDeadLink proves the lazy resolver
// getter is consulted: a stale snapshot link the deterministic lanes still miss
// is dropped from the report when the semantic lane resolves it (the case a nil
// resolver would over-report as dead).
func TestLintDocuments_SemanticResolverSuppressesDeadLink(t *testing.T) {
	nav := &fakeLintNav{
		// Same dangling snapshot the deterministic-only test reports as dead.
		unresolvedDocs: []navigate.UnresolvedLinkDoc{
			{
				DocumentUID:    "d1",
				Path:           "notes/dangling.md",
				Title:          "Dangling",
				RevisionUID:    "rev1",
				UnresolvedJSON: `[{"raw":"[[Ghost]]","target":"Ghost"}]`,
			},
		},
	}
	// resolver getter returns a resolver that satisfies the link semantically.
	tool := NewLintDocuments(nav, func() document.SemanticNameResolver {
		return &fakeSemanticResolver{uid: "ghost-uid", kind: "person"}
	}, nil)
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out lintDocumentsOutput
	if uErr := json.Unmarshal(raw, &out); uErr != nil {
		t.Fatalf("unmarshal: %v", uErr)
	}
	if len(out.DeadLinks) != 0 {
		t.Errorf("deadLinks = %+v, want none (semantic lane resolved the link)", out.DeadLinks)
	}
}

// TestLintDocuments_TitleCollision proves that a document whose title (display
// name) shadows a non-document entity of the same name surfaces in
// nameCollisions. The finding must carry the document UID, vault path, kind of
// the shadowed entity, the shadowed entity UID, and a non-empty disambiguation
// hint that mentions the shadowed entity kind.
func TestLintDocuments_TitleCollision(t *testing.T) {
	const (
		docUID  = "doc-mithrilmail"
		prodUID = "prod-mithrilmail"
	)
	nav := &fakeLintNav{
		entities: []navigate.Entity{
			{UID: docUID, Kind: "document", DisplayName: "Mithrilmail"},
			{UID: prodUID, Kind: "product", DisplayName: "Mithrilmail"},
		},
		documentsByUID: map[string]navigate.Document{
			docUID: {DocumentUID: docUID, Path: "products/mithrilmail.md", Title: "Mithrilmail"},
		},
	}
	tool := NewLintDocuments(nav, nil, nil)
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out lintDocumentsOutput
	if uErr := json.Unmarshal(raw, &out); uErr != nil {
		t.Fatalf("unmarshal: %v", uErr)
	}
	if len(out.NameCollisions) != 1 {
		t.Fatalf("nameCollisions = %+v, want one", out.NameCollisions)
	}
	c := out.NameCollisions[0]
	if c.DocumentUID != docUID {
		t.Errorf("documentUid = %q, want %q", c.DocumentUID, docUID)
	}
	if c.Path != "products/mithrilmail.md" {
		t.Errorf("path = %q, want 'products/mithrilmail.md'", c.Path)
	}
	if c.Title != "Mithrilmail" {
		t.Errorf("title = %q, want 'Mithrilmail'", c.Title)
	}
	if c.ShadowedKind != "product" {
		t.Errorf("shadowedKind = %q, want 'product'", c.ShadowedKind)
	}
	if c.ShadowedUID != prodUID {
		t.Errorf("shadowedUid = %q, want %q", c.ShadowedUID, prodUID)
	}
	if !strings.Contains(c.Hint, "product") {
		t.Errorf("hint %q does not mention 'product'", c.Hint)
	}
}

// TestLintDocuments_NoCollisionWhenDocumentOnly proves that when only document
// entities share a name (no non-document shadow), no name-collision entry is
// emitted — the collision category is specific to cross-kind conflicts.
func TestLintDocuments_NoCollisionWhenDocumentOnly(t *testing.T) {
	nav := &fakeLintNav{
		entities: []navigate.Entity{
			{UID: "doc-a", Kind: "document", DisplayName: "Mithrilmail"},
			{UID: "doc-b", Kind: "document", DisplayName: "Mithrilmail"},
		},
		documentsByUID: map[string]navigate.Document{
			"doc-a": {DocumentUID: "doc-a", Path: "a/mithrilmail.md", Title: "Mithrilmail"},
		},
	}
	tool := NewLintDocuments(nav, nil, nil)
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out lintDocumentsOutput
	if uErr := json.Unmarshal(raw, &out); uErr != nil {
		t.Fatalf("unmarshal: %v", uErr)
	}
	if len(out.NameCollisions) != 0 {
		t.Errorf("nameCollisions = %+v, want none (all same-named entities are documents)", out.NameCollisions)
	}
}
