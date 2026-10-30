package document

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// fakeReader is a static Reader for the deterministic resolver tests: no DB,
// no events. entities is keyed by lower-case display name; docsByPath by path.
type fakeReader struct {
	entities map[string][]navigate.Entity
	docs     map[string]navigate.Document
}

func (f *fakeReader) GetDocumentByPath(_ context.Context, path string) (navigate.Document, error) {
	if d, ok := f.docs[path]; ok {
		return d, nil
	}
	return navigate.Document{}, navigate.ErrNotFound
}

func (f *fakeReader) GetDocumentByPathScope(_ context.Context, path, scope string) (navigate.Document, error) {
	if d, ok := f.docs[path]; ok && (d.Scope == scope || d.Scope == "") {
		return d, nil
	}
	return navigate.Document{}, navigate.ErrNotFound
}

func (f *fakeReader) GetDocumentByUID(_ context.Context, uid string) (navigate.Document, error) {
	for path := range f.docs {
		d := f.docs[path]
		if d.DocumentUID == uid {
			return d, nil
		}
	}
	return navigate.Document{}, navigate.ErrNotFound
}

// fakeDefaultCap mimics the real navigator's default page cap at miniature
// scale: a ListEntities call that does not set an explicit Limit gets at most
// this many rows, so tests can prove the resolver requests enough rows to
// detect ambiguity instead of trusting the default page.
const fakeDefaultCap = 2

//nolint:gocritic // hugeParam: EntityFilter is passed by value to satisfy the navigate.Reader interface.
func (f *fakeReader) ListEntities(_ context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error) {
	ents := f.entities[lower(opts.NameExact)]
	limit := opts.Limit
	if limit <= 0 {
		limit = fakeDefaultCap
	}
	if len(ents) > limit {
		ents = ents[:limit]
	}
	return ents, nil
}

func (f *fakeReader) EntityRelations(context.Context, string, navigate.RelationFilter) ([]navigate.Relation, error) {
	return nil, nil
}

func (f *fakeReader) DocumentsWithUnresolvedLinks(context.Context) ([]navigate.UnresolvedLinkDoc, error) {
	return nil, nil
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func wl(target string) ontology.WikiLink {
	return ontology.WikiLink{Raw: "[[" + target + "]]", Target: target}
}

func TestResolveTarget_PersonIsMentions(t *testing.T) {
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"rosie cotton": {{UID: "u-rosie", Kind: string(ontology.EntityKindPerson), DisplayName: "Rosie Cotton"}},
	}}
	uid, pred, ok := resolveTarget(context.Background(), r, "self", "Rosie Cotton")
	if !ok || uid != "u-rosie" || pred != predicateMentions {
		t.Fatalf("got uid=%q pred=%q ok=%v, want u-rosie/mentions/true", uid, pred, ok)
	}
}

func TestResolveTarget_DocumentIsLinksTo(t *testing.T) {
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"project alpha": {{UID: "u-doc", Kind: string(ontology.EntityKindDocument), DisplayName: "Project Alpha"}},
	}}
	uid, pred, ok := resolveTarget(context.Background(), r, "self", "Project Alpha")
	if !ok || uid != "u-doc" || pred != predicateLinksTo {
		t.Fatalf("got uid=%q pred=%q ok=%v, want u-doc/links_to/true", uid, pred, ok)
	}
}

func TestResolveTarget_AmbiguousIsUnresolved(t *testing.T) {
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"chris": {{UID: "a", Kind: "person"}, {UID: "b", Kind: "person"}},
	}}
	if _, _, ok := resolveTarget(context.Background(), r, "self", "Chris"); ok {
		t.Error("ambiguous name must resolve to nothing")
	}
}

func TestResolveTarget_AmbiguityBeyondDefaultCap(t *testing.T) {
	// Three same-name entities, the importing document itself listed first. A
	// resolver that trusted the reader's default page (here 2 rows: self + one
	// other) would see a single non-self match and wrongly resolve it; the
	// explicit Limit 3 page reveals the second non-self match → unresolved.
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"chris": {
			{UID: "self", Kind: "document", DisplayName: "Chris"},
			{UID: "a", Kind: "person", DisplayName: "Chris"},
			{UID: "b", Kind: "person", DisplayName: "Chris"},
		},
	}}
	if uid, _, ok := resolveTarget(context.Background(), r, "self", "Chris"); ok {
		t.Errorf("ambiguous name hidden beyond the default page must stay unresolved; resolved to %q", uid)
	}
}

func TestResolveTarget_CanonicalEntityBeatsDocumentNamesake(t *testing.T) {
	// A note titled "Rosie Cotton" (a document entity) coexists with the person
	// "Rosie Cotton". A bare [[Rosie Cotton]] wikilink names the canonical
	// subject — the person — not the note about her (that is addressed by its
	// vault path). The unique non-document entity wins over the document
	// namesake instead of dangling on a false ambiguity, so a note's most
	// important links connect to the entities it is actually about.
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"rosie cotton": {
			{UID: "u-doc", Kind: string(ontology.EntityKindDocument), DisplayName: "Rosie Cotton"},
			{UID: "u-person", Kind: string(ontology.EntityKindPerson), DisplayName: "Rosie Cotton"},
		},
	}}
	uid, pred, ok := resolveTarget(context.Background(), r, "self", "Rosie Cotton")
	if !ok || uid != "u-person" || pred != predicateMentions {
		t.Fatalf("got uid=%q pred=%q ok=%v, want u-person/mentions/true", uid, pred, ok)
	}
}

func TestResolveTarget_CrossKindNamesakeResolvesByPriority(t *testing.T) {
	// "Mithrilmail" exists as both a product and a project — DIFFERENT kinds, so a
	// bare [[Mithrilmail]] is not true ambiguity. The kind-priority tiebreaker picks
	// the higher-salience kind (project over product) deterministically instead
	// of dangling, which is what left every [[Mithrilmail]] link unresolved in
	// testing. Order in the slice is reversed to prove the pick is by priority,
	// not by position.
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"mithrilmail": {
			{UID: "u-product", Kind: "product", DisplayName: "Mithrilmail"},
			{UID: "u-project", Kind: "project", DisplayName: "Mithrilmail"},
		},
	}}
	uid, pred, ok := resolveTarget(context.Background(), r, "self", "Mithrilmail")
	if !ok || uid != "u-project" || pred != predicateMentions {
		t.Fatalf("got uid=%q pred=%q ok=%v, want u-project/mentions/true", uid, pred, ok)
	}
}

func TestResolveTarget_SameKindNamesakesStayAmbiguous(t *testing.T) {
	// Two products both named "Mithrilmail" share the top priority — a genuine
	// ambiguity the tiebreaker must NOT resolve, since picking one would
	// silently link the wrong entity.
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"mithrilmail": {
			{UID: "p1", Kind: "product", DisplayName: "Mithrilmail"},
			{UID: "p2", Kind: "product", DisplayName: "Mithrilmail"},
		},
	}}
	if uid, _, ok := resolveTarget(context.Background(), r, "self", "Mithrilmail"); ok {
		t.Errorf("two same-kind namesakes must stay ambiguous; resolved to %q", uid)
	}
}

func TestResolveTarget_TwoCanonicalNamesakesStayAmbiguous(t *testing.T) {
	// Two people named "Rosie Cotton" plus a same-titled note: the canonical
	// names are genuinely ambiguous, so the document-preference rule must NOT
	// collapse real ambiguity — the link stays dangling rather than guessing
	// which person is meant.
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"rosie cotton": {
			{UID: "u-doc", Kind: string(ontology.EntityKindDocument), DisplayName: "Rosie Cotton"},
			{UID: "p1", Kind: string(ontology.EntityKindPerson), DisplayName: "Rosie Cotton"},
			{UID: "p2", Kind: string(ontology.EntityKindPerson), DisplayName: "Rosie Cotton"},
		},
	}}
	if uid, _, ok := resolveTarget(context.Background(), r, "self", "Rosie Cotton"); ok {
		t.Errorf("two canonical namesakes must stay ambiguous; resolved to %q", uid)
	}
}

func TestResolveTarget_TwoDocumentNamesakesStayAmbiguous(t *testing.T) {
	// No canonical entity, two same-titled notes: ambiguous among documents, so
	// the link dangles (the document-preference rule only fires for a UNIQUE
	// canonical entity, never to break a tie between two documents).
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"design plan": {
			{UID: "d1", Kind: string(ontology.EntityKindDocument), DisplayName: "Design Plan"},
			{UID: "d2", Kind: string(ontology.EntityKindDocument), DisplayName: "Design Plan"},
		},
	}}
	if uid, _, ok := resolveTarget(context.Background(), r, "self", "Design Plan"); ok {
		t.Errorf("two document namesakes must stay ambiguous; resolved to %q", uid)
	}
}

func TestResolveTarget_SelfLinkExcluded(t *testing.T) {
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"me": {{UID: "self", Kind: string(ontology.EntityKindDocument), DisplayName: "Me"}},
	}}
	if _, _, ok := resolveTarget(context.Background(), r, "self", "Me"); ok {
		t.Error("a self-referential link must not resolve to a self-edge")
	}
}

func TestResolveTarget_PathMatch(t *testing.T) {
	r := &fakeReader{
		entities: map[string][]navigate.Entity{},
		docs: map[string]navigate.Document{
			"notes/foo.md": {DocumentUID: "u-foo", Path: "notes/foo.md"},
		},
	}
	// "notes/foo" should match "notes/foo.md" via the .md path candidate.
	uid, pred, ok := resolveTarget(context.Background(), r, "self", "notes/foo")
	if !ok || uid != "u-foo" || pred != predicateLinksTo {
		t.Fatalf("got uid=%q pred=%q ok=%v, want u-foo/links_to/true", uid, pred, ok)
	}
}

func TestResolveWikilinks_DedupAndSplit(t *testing.T) {
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"rosie cotton": {{UID: "u-rosie", Kind: "person", DisplayName: "Rosie Cotton"}},
	}}
	resolved, unresolved := resolveWikilinks(context.Background(), r, nil, "self",
		[]ontology.WikiLink{wl("Rosie Cotton"), wl("Rosie Cotton"), wl("Ghost")})
	if len(resolved) != 1 {
		t.Fatalf("resolved = %+v, want 1 (deduped)", resolved)
	}
	if resolved[0].objectUID != "u-rosie" || resolved[0].predicate != predicateMentions {
		t.Errorf("resolved[0] = %+v", resolved[0])
	}
	if len(unresolved) != 1 || unresolved[0].Target != "Ghost" {
		t.Errorf("unresolved = %+v, want [Ghost]", unresolved)
	}
}

// TestResolveFrontmatterLinks_AboutDescribesEdgeStoresUID verifies G3: a
// frontmatter `about:` link resolves to a describes edge whose objectUID is the
// resolved entity UID (not the display string). Renaming the subject's display
// name must not break the stored edge because it references the UID.
func TestResolveFrontmatterLinks_AboutDescribesEdgeStoresUID(t *testing.T) {
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"rosie cotton": {{UID: "u-rosie-uid", Kind: string(ontology.EntityKindPerson), DisplayName: "Rosie Cotton"}},
	}}
	// Simulate a WikiLink produced by parseFrontmatterLinks for about: Rosie Cotton.
	links := []ontology.WikiLink{
		{Raw: "Rosie Cotton", Target: "Rosie Cotton", RelType: predicateDescribes},
	}
	resolved, unresolved := resolveFrontmatterLinks(context.Background(), r, nil, "doc-uid", links)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %+v, want none", unresolved)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved = %+v, want 1 edge", resolved)
	}
	got := resolved[0]
	if got.predicate != predicateDescribes {
		t.Errorf("predicate = %q, want %q", got.predicate, predicateDescribes)
	}
	// G3: objectUID must be the entity UID, not the display name.
	if got.objectUID != "u-rosie-uid" {
		t.Errorf("objectUID = %q, want %q (UID, not display name)", got.objectUID, "u-rosie-uid")
	}
}

// TestResolveFrontmatterLinks_EntitySelfStillMentions is a regression guard:
// the `entity: self` link must continue to produce a mentions edge after the
// about-to-describes change.
func TestResolveFrontmatterLinks_EntitySelfStillMentions(t *testing.T) {
	links := []ontology.WikiLink{
		{Raw: "self", Target: "self", RelType: predicateMentions},
	}
	resolved, unresolved := resolveFrontmatterLinks(context.Background(), &fakeReader{}, nil, "doc-uid", links)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %+v, want none", unresolved)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved = %+v, want 1 edge", resolved)
	}
	got := resolved[0]
	if got.predicate != predicateMentions {
		t.Errorf("entity: self predicate = %q, want %q", got.predicate, predicateMentions)
	}
	if got.objectUID != selfEntityUID {
		t.Errorf("objectUID = %q, want %q", got.objectUID, selfEntityUID)
	}
}

// TestIsKnownDocumentPredicate_Describes confirms that "describes" is accepted
// as a valid document-relation predicate after migration 0041 registers it.
func TestIsKnownDocumentPredicate_Describes(t *testing.T) {
	if !isKnownDocumentPredicate(predicateDescribes) {
		t.Errorf("isKnownDocumentPredicate(%q) = false, want true", predicateDescribes)
	}
}

func TestResolveWikilinks_CanonicalWinsOverDocumentNamesake(t *testing.T) {
	// A note titled "Rosie Cotton" (document) and a Person "Rosie Cotton": the
	// bare [[Rosie Cotton]] wikilink names the canonical subject, so it resolves
	// to the unique non-document entity (the person) with a mentions edge rather
	// than dangling. The note about Rosie is addressed by its vault path, never
	// by the bare name. This is what makes a note's links connect to the very
	// entities it is about (the document-namesake collision the unbiased agents
	// hit repeatedly).
	r := &fakeReader{entities: map[string][]navigate.Entity{
		"rosie cotton": {
			{UID: "u-person", Kind: "person", DisplayName: "Rosie Cotton"},
			{UID: "u-doc", Kind: "document", DisplayName: "Rosie Cotton"},
		},
	}}
	resolved, unresolved := resolveWikilinks(context.Background(), r, nil, "self", []ontology.WikiLink{wl("Rosie Cotton")})
	if len(resolved) != 1 || len(unresolved) != 0 {
		t.Fatalf("canonical namesake must resolve: resolved=%+v unresolved=%+v", resolved, unresolved)
	}
	if resolved[0].objectUID != "u-person" || resolved[0].predicate != predicateMentions {
		t.Errorf("got %+v, want u-person/mentions", resolved[0])
	}
}
