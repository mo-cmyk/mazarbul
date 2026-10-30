package document_test

// importer_integration_test.go drives the Importer against the real four-layer
// stack (FileEventLog → dispatcher → SQLite + FTS5 + sqlite-vec + LadybugDB) so
// the intertwining is verified end-to-end, not against mocks: importing a vault
// creates document entities, resolves [[wikilinks]] to the SAME canonical
// entities the structured graph uses, and the edges are queryable through the
// ordinary navigate surface (a person's neighbors include the docs about her).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// fakeEmbedder returns a deterministic zero vector of the configured dimension.
// The importer never depends on embeddings (they are scheduled in the
// background and guarded on d.embed != nil); a fake keeps dispatcher.Open happy.
type fakeEmbedder struct{ dim int }

func (f fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return make([]float32, f.dim), nil
}

// noopOntology validates every payload (the real schemas are exercised in the
// ontology package); this test pins import behaviour, not schema enforcement.
type noopOntology struct{}

func (noopOntology) Schema(context.Context, string) (kernel.Schema, error) {
	return kernel.Schema{}, kernel.ErrNotImplemented
}
func (noopOntology) Register(context.Context, kernel.Schema) error { return kernel.ErrNotImplemented }
func (noopOntology) List(context.Context) ([]kernel.Schema, error) {
	return nil, kernel.ErrNotImplemented
}
func (noopOntology) Validate(context.Context, string, json.RawMessage) error { return nil }

// stack is the wired test world.
type stack struct {
	ctx context.Context
	log *kernel.FileEventLog
	nav *navigate.Navigator
	im  *document.Importer
}

func newStack(t *testing.T) *stack {
	return newStackNav(t, false)
}

// newStackNav builds the wired test world. vaultScopes selects the production
// four-scope navigator (project/personal/team/draft) that the real importer
// wiring uses; false uses the narrower default read set. Draft-boundary tests
// need the four-scope navigator so dedup sees draft while resolution (which the
// importer runs through a draft-blind view) does not.
func newStackNav(t *testing.T, vaultScopes bool) *stack {
	t.Helper()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()

	dis, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	log := kernel.NewFileEventLog(dataDir, noopOntology{}, kernel.WithStorage(dis))
	t.Cleanup(func() { _ = dis.Close() })
	t.Cleanup(func() { _ = log.Close() })

	reader, err := dis.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	nav, err := navigate.New(reader)
	if vaultScopes {
		nav, err = navigate.NewVaultScopes(reader)
	}
	if err != nil {
		t.Fatalf("navigate constructor: %v", err)
	}
	return &stack{ctx: ctx, log: log, nav: nav, im: document.NewImporter(log, nav, zap.NewNop())}
}

// seedEntity appends an entity event (as the resolver would) into the default
// (personal) scope and returns its UID.
func (s *stack) seedEntity(t *testing.T, kind ontology.EntityKind, displayName string) string {
	t.Helper()
	return s.seedScopedEntity(t, "", kind, displayName)
}

// seedScopedEntity is seedEntity into an explicit scope (an empty scope defaults
// to personal at Append). Used to plant a draft-scoped entity that a non-draft
// document's wikilink must NOT resolve to.
func (s *stack) seedScopedEntity(t *testing.T, scope kernel.Scope, kind ontology.EntityKind, displayName string) string {
	t.Helper()
	uid := ulid.Make().String()
	payload, err := json.Marshal(ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             kind,
		DisplayName:      displayName,
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal seed entity: %v", err)
	}
	if _, err := s.log.Append(s.ctx, []kernel.Event{{
		Kind: kernel.EventKind(ontology.KindEntity), Scope: scope, Source: "test", Confidence: 1.0, Payload: payload,
	}}); err != nil {
		t.Fatalf("append seed entity: %v", err)
	}
	return uid
}

// writeVault writes files (path→content, forward-slash relative) into a fresh
// temp dir and returns it.
func writeVault(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// outgoing returns the doc's outgoing edges of the given predicate.
func (s *stack) outgoing(t *testing.T, docUID, predicate string) []navigate.Relation {
	t.Helper()
	rels, err := s.nav.EntityRelations(s.ctx, docUID, navigate.RelationFilter{
		Predicates: []string{predicate}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations(%s): %v", docUID, err)
	}
	var out []navigate.Relation
	for i := range rels {
		if rels[i].SubjectUID == docUID {
			out = append(out, rels[i])
		}
	}
	return out
}

// relationEventScope scans the event log for the first relation event linking
// subjectUID → objectUID and returns its event-level scope. Used to assert the
// scope stamped on an edge, which the navigate layer does not surface.
func (s *stack) relationEventScope(t *testing.T, subjectUID, objectUID string) (kernel.Scope, bool) {
	t.Helper()
	kinds := []kernel.EventKind{kernel.EventKind(ontology.KindRelation)}
	for ev, err := range s.log.Read(s.ctx, kernel.ReadOpts{Kinds: kinds}) {
		if err != nil {
			t.Fatalf("read relation events: %v", err)
		}
		var r ontology.Relation
		if json.Unmarshal(ev.Payload, &r) != nil {
			continue
		}
		if r.SubjectUID == subjectUID && r.ObjectUID == objectUID {
			return ev.Scope, true
		}
	}
	return "", false
}

// TestImport_ScopeStampedOnDocumentAndCompanionEntity pins Part C's core
// invariant: a document IS an entity, so an import into a non-default scope must
// stamp that scope on BOTH the document row and its companion entity row (the
// same UID). If they diverged, the document-as-entity identity would split
// across scopes.
func TestImport_ScopeStampedOnDocumentAndCompanionEntity(t *testing.T) {
	s := newStack(t)

	res, err := s.im.ImportContent(s.ctx, kernel.ScopeProject, "notes/proj.md",
		[]byte("# Proj\n\nA project note.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	doc, err := s.nav.GetDocumentByPathScope(s.ctx, "notes/proj.md", "project")
	if err != nil {
		t.Fatalf("GetDocumentByPathScope: %v", err)
	}
	if doc.Scope != "project" {
		t.Errorf("document scope = %q, want project", doc.Scope)
	}

	ent, err := s.nav.GetEntity(s.ctx, docUID)
	if err != nil {
		t.Fatalf("GetEntity(docUID): %v", err)
	}
	if ent.Scope != "project" {
		t.Errorf("companion entity scope = %q, want project (doc row and entity row must share scope)", ent.Scope)
	}
}

// TestImport_ReconcileEdgeCarriesReconciledDocScope pins the cross-scope
// reconcile isolation invariant: when a later import makes another document's
// dangling wikilink resolvable, the reconciled edge must carry THAT document's
// own scope, not the triggering batch's scope. A project doc reconciled by a
// personal import must emit a PROJECT edge — else it would leak a personal edge.
// Because an empty scope defaults to personal, project is the only value that
// proves the reconciled doc's own scope was threaded (not the batch scope, and
// not left empty).
func TestImport_ReconcileEdgeCarriesReconciledDocScope(t *testing.T) {
	s := newStack(t)

	// Doc A lives in PROJECT scope and links to a not-yet-existing "Beta".
	aRes, err := s.im.ImportContent(s.ctx, kernel.ScopeProject, "notes/a.md",
		[]byte("# A\n\nSee [[Beta]] for details.\n"))
	if err != nil {
		t.Fatalf("import A: %v", err)
	}
	aUID := aRes.Documents[0].DocumentUID

	// Doc B is imported LATER in PERSONAL (default) scope; its title "Beta"
	// creates the entity that makes A's [[Beta]] link resolvable, triggering the
	// cross-document reconcile pass.
	bRes, err := s.im.ImportContent(s.ctx, kernel.ScopePersonal, "notes/beta.md",
		[]byte("# Beta\n\nThe reconcile target.\n"))
	if err != nil {
		t.Fatalf("import B: %v", err)
	}
	bUID := bRes.Documents[0].DocumentUID
	if bRes.Reconciled == 0 {
		t.Fatalf("expected B's import to reconcile A's dangling link, Reconciled=0")
	}

	got, ok := s.relationEventScope(t, aUID, bUID)
	if !ok {
		t.Fatal("no reconciled relation event from A to B found")
	}
	if got != kernel.ScopeProject {
		t.Errorf("reconciled edge scope = %q, want project (the reconciled doc's own scope, not B's personal batch)", got)
	}
}

// TestImport_NonDraftDocDoesNotResolveDraftEntity pins the Part C resolution
// boundary: the importer dedups across all four scopes (NewVaultScopes) but
// resolves wikilinks through a draft-blind view, so a [[Name]] in a non-draft
// document never resolves to a draft-scoped entity — resolving it would leak a
// private draft into the personal scope's graph as a cross-scope edge.
func TestImport_NonDraftDocDoesNotResolveDraftEntity(t *testing.T) {
	s := newStackNav(t, true) // production wiring: navigator spans all four scopes

	// A confirmed PERSON entity "Zephyrine" exists only in DRAFT scope.
	draftUID := s.seedScopedEntity(t, kernel.ScopeDraft, ontology.EntityKindPerson, "Zephyrine")
	// Sanity: the draft entity really exists (the vault-scoped navigator sees it),
	// so the "no edge" result below is a resolution boundary, not a missing target.
	if ent, err := s.nav.GetEntity(s.ctx, draftUID); err != nil || ent.Scope != "draft" {
		t.Fatalf("seed draft entity not found as draft: ent=%+v err=%v", ent, err)
	}

	// A PERSONAL document links to it by name.
	res, err := s.im.ImportContent(s.ctx, kernel.ScopePersonal, "notes/mention.md",
		[]byte("# Mention\n\nAsking [[Zephyrine]] to review.\n"))
	if err != nil {
		t.Fatalf("ImportContent: %v", err)
	}
	docUID := res.Documents[0].DocumentUID

	// The draft entity must NOT have been resolved: no edge from the personal doc.
	if mentions := s.outgoing(t, docUID, "mentions"); len(mentions) != 0 {
		t.Errorf("personal doc resolved a DRAFT entity: mentions=%+v (want none — draft must not leak into a non-draft doc's edges)", mentions)
	}
	// And the link is recorded as dangling, not silently dropped.
	if res.Unresolved == 0 {
		t.Errorf("expected the [[Zephyrine]] link to stay unresolved (dangling), Unresolved=0")
	}
}

// TestImport_SamePathDifferentScope_YieldsSeparateDocs pins the composite
// (path, scope) identity at the importer level: importing the SAME relative path
// under two scopes mints two DISTINCT documents (different UIDs, own bodies), and
// re-importing each is idempotent to its own scope (no cross-scope clobber).
func TestImport_SamePathDifferentScope_YieldsSeparateDocs(t *testing.T) {
	s := newStackNav(t, true)

	projRes, err := s.im.ImportContent(s.ctx, kernel.ScopeProject, "notes/foo.md", []byte("# Foo\n\nProject copy.\n"))
	if err != nil {
		t.Fatalf("import project: %v", err)
	}
	persRes, err := s.im.ImportContent(s.ctx, kernel.ScopePersonal, "notes/foo.md", []byte("# Foo\n\nPersonal copy.\n"))
	if err != nil {
		t.Fatalf("import personal: %v", err)
	}
	projUID := projRes.Documents[0].DocumentUID
	persUID := persRes.Documents[0].DocumentUID
	if projUID == persUID {
		t.Fatalf("same path in two scopes shared a document UID (%s) — composite (path,scope) identity broken", projUID)
	}

	// Each scope-exact lookup resolves to its own row and body.
	projDoc, err := s.nav.GetDocumentByPathScope(s.ctx, "notes/foo.md", "project")
	if err != nil {
		t.Fatalf("get project doc: %v", err)
	}
	persDoc, err := s.nav.GetDocumentByPathScope(s.ctx, "notes/foo.md", "personal")
	if err != nil {
		t.Fatalf("get personal doc: %v", err)
	}
	if projDoc.DocumentUID != projUID || persDoc.DocumentUID != persUID {
		t.Fatalf("scope-exact lookup crossed rows: proj=%s/%s pers=%s/%s", projDoc.DocumentUID, projUID, persDoc.DocumentUID, persUID)
	}
	if !strings.Contains(projDoc.Body, "Project copy") || !strings.Contains(persDoc.Body, "Personal copy") {
		t.Errorf("bodies crossed scopes: proj=%q pers=%q", projDoc.Body, persDoc.Body)
	}

	// Re-import the project copy unchanged: idempotent to its own scope, no new doc.
	reProj, err := s.im.ImportContent(s.ctx, kernel.ScopeProject, "notes/foo.md", []byte("# Foo\n\nProject copy.\n"))
	if err != nil {
		t.Fatalf("reimport project: %v", err)
	}
	if reProj.Imported != 0 || reProj.Skipped != 1 {
		t.Errorf("reimport of identical project doc not a no-op: Imported=%d Skipped=%d", reProj.Imported, reProj.Skipped)
	}
}

func TestImport_IntertwinesWithStructuredGraph(t *testing.T) {
	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	vault := writeVault(t, map[string]string{
		"notes/about-sarah.md": "---\ntitle: About Rosie\ntags: [people]\n---\n" +
			"# About Rosie\n\n[[Rosie Cotton]] leads [[Project Alpha]].\n",
		"notes/project-alpha.md": "# Project Alpha\n\nThe flagship initiative.\n",
	})

	res, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if res.Imported != 2 {
		t.Fatalf("Imported = %d, want 2", res.Imported)
	}

	about, err := s.nav.GetDocumentByPath(s.ctx, "notes/about-sarah.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath(about-sarah): %v", err)
	}
	alpha, err := s.nav.GetDocumentByPath(s.ctx, "notes/project-alpha.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath(project-alpha): %v", err)
	}

	// [[Rosie Cotton]] resolved to the pre-existing PERSON entity (a mentions
	// edge to the same node structured memory uses) — this is the intertwining.
	mentions := s.outgoing(t, about.DocumentUID, "mentions")
	if len(mentions) != 1 || mentions[0].ObjectUID != sarahUID {
		t.Fatalf("about-sarah mentions = %+v, want one edge → %s", mentions, sarahUID)
	}

	// [[Project Alpha]] resolved to the OTHER document (a links_to edge).
	linksTo := s.outgoing(t, about.DocumentUID, "links_to")
	if len(linksTo) != 1 || linksTo[0].ObjectUID != alpha.DocumentUID {
		t.Fatalf("about-sarah links_to = %+v, want one edge → %s", linksTo, alpha.DocumentUID)
	}

	// The payoff: the person's neighbors now include the document about her,
	// reached through the ordinary graph-navigation surface.
	neighbors, err := s.nav.Neighbors(s.ctx, sarahUID, 1, navigate.RelationFilter{Predicates: []string{"mentions"}})
	if err != nil {
		t.Fatalf("Neighbors(sarah): %v", err)
	}
	found := false
	for i := range neighbors {
		if neighbors[i].Entity.UID == about.DocumentUID {
			found = true
		}
	}
	if !found {
		t.Errorf("Neighbors(Rosie) did not include the about-sarah document: %+v", neighbors)
	}
}

func TestImport_ReimportIsIdempotent(t *testing.T) {
	s := newStack(t)
	s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")
	vault := writeVault(t, map[string]string{
		"a.md": "# A\n\nMentions [[Rosie Cotton]].\n",
	})

	first, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil || first.Imported != 1 {
		t.Fatalf("first import: res=%+v err=%v", first, err)
	}
	doc, _ := s.nav.GetDocumentByPath(s.ctx, "a.md")
	if got := len(s.outgoing(t, doc.DocumentUID, "mentions")); got != 1 {
		t.Fatalf("after first import: %d mentions edges, want 1", got)
	}

	second, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Imported != 0 || second.Skipped != 1 {
		t.Errorf("re-import not a no-op: Imported=%d Skipped=%d", second.Imported, second.Skipped)
	}
	// No duplicate edge.
	if got := len(s.outgoing(t, doc.DocumentUID, "mentions")); got != 1 {
		t.Errorf("after re-import: %d mentions edges, want 1 (no duplicate)", got)
	}
}

func TestImport_ReconcilesLinkWhenTargetArrivesLater(t *testing.T) {
	s := newStack(t)

	// Import A first; its [[Sam Builder]] link dangles (Sam does not exist yet).
	vaultA := writeVault(t, map[string]string{
		"a.md": "# A\n\nWritten by [[Sam Builder]].\n",
	})
	resA, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vaultA)
	if err != nil {
		t.Fatalf("import A: %v", err)
	}
	if resA.Unresolved != 1 {
		t.Fatalf("import A: Unresolved = %d, want 1", resA.Unresolved)
	}
	docA, _ := s.nav.GetDocumentByPath(s.ctx, "a.md")
	if got := len(s.outgoing(t, docA.DocumentUID, "links_to")); got != 0 {
		t.Fatalf("before reconcile: %d links_to edges, want 0", got)
	}

	// Now import a Sam Builder document; the dangling link must reconcile.
	vaultB := writeVault(t, map[string]string{
		"bob.md": "# Sam Builder\n\nProfile.\n",
	})
	resB, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vaultB)
	if err != nil {
		t.Fatalf("import B: %v", err)
	}
	if resB.Reconciled != 1 {
		t.Errorf("import B: Reconciled = %d, want 1", resB.Reconciled)
	}
	bob, _ := s.nav.GetDocumentByPath(s.ctx, "bob.md")
	edges := s.outgoing(t, docA.DocumentUID, "links_to")
	if len(edges) != 1 || edges[0].ObjectUID != bob.DocumentUID {
		t.Errorf("after reconcile: links_to = %+v, want one edge → %s", edges, bob.DocumentUID)
	}

	// Reconciled edges must anchor provenance to A's latest revision EVENT —
	// exactly like edges emitted at import time — not to the document UID.
	docA, err = s.nav.GetDocumentByPath(s.ctx, "a.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath(a.md): %v", err)
	}
	if len(edges) == 1 && edges[0].SourceEventUID != docA.RevisionUID {
		t.Errorf("reconciled edge SourceEventUID = %q, want the revision UID %q", edges[0].SourceEventUID, docA.RevisionUID)
	}
}

// TestLiveDeadLinkDocs_ExcludesReconciledLinks proves the fix for the "lint
// reports reconciled links as dead forever" review finding. The write-time
// unresolved-link snapshot is never pruned after reconcile creates the edge, so
// DocumentsWithUnresolvedLinks (the raw snapshot) keeps returning a.md. lint
// must instead re-resolve the snapshot against the current state, so once b.md
// arrives and reconcile links it, LiveDeadLinkDocs drops a.md.
func TestLiveDeadLinkDocs_ExcludesReconciledLinks(t *testing.T) {
	s := newStack(t)

	// a.md links to a [[Sam Builder]] that does not exist yet — it dangles.
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, writeVault(t, map[string]string{
		"a.md": "# A\n\nWritten by [[Sam Builder]].\n",
	})); err != nil {
		t.Fatalf("import A: %v", err)
	}

	// Before the target arrives, the link is genuinely dead: lint must report it.
	dead, err := document.LiveDeadLinkDocs(s.ctx, s.nav, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("LiveDeadLinkDocs (pre-reconcile): %v", err)
	}
	if len(dead) != 1 || dead[0].Path != "a.md" {
		t.Fatalf("pre-reconcile dead links = %+v, want exactly a.md", dead)
	}

	// bob.md arrives; reconcile resolves a.md's dangling link into a live edge.
	resB, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, writeVault(t, map[string]string{
		"bob.md": "# Sam Builder\n\nProfile.\n",
	}))
	if err != nil {
		t.Fatalf("import B: %v", err)
	}
	if resB.Reconciled != 1 {
		t.Fatalf("import B: Reconciled = %d, want 1", resB.Reconciled)
	}

	// The raw write-time snapshot is NOT pruned by reconcile, so the navigator
	// still flags a.md — this is exactly why lint must not trust it directly.
	snapshot, err := s.nav.DocumentsWithUnresolvedLinks(s.ctx)
	if err != nil {
		t.Fatalf("DocumentsWithUnresolvedLinks: %v", err)
	}
	if len(snapshot) != 1 || snapshot[0].Path != "a.md" {
		t.Fatalf("stale snapshot = %+v, want a.md still recorded (reconcile does not prune it)", snapshot)
	}

	// LiveDeadLinkDocs re-resolves the snapshot, so the now-reconciled link is
	// no longer reported as dead — lint exits clean instead of failing forever.
	dead, err = document.LiveDeadLinkDocs(s.ctx, s.nav, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("LiveDeadLinkDocs (post-reconcile): %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("post-reconcile dead links = %+v, want none (reconciled link must not be reported)", dead)
	}
}

func TestImport_RemovedLinkIsForgotten(t *testing.T) {
	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")
	s.seedEntity(t, ontology.EntityKindPerson, "Sam Builder")

	vault := writeVault(t, map[string]string{
		"a.md": "# A\n\nWith [[Rosie Cotton]] and [[Sam Builder]].\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("first import: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "a.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if got := len(s.outgoing(t, doc.DocumentUID, "mentions")); got != 2 {
		t.Fatalf("after first import: %d mentions edges, want 2", got)
	}

	// Drop the [[Sam Builder]] link and re-import: the stale edge must be
	// forgotten so backlinks/reads mirror the latest body.
	if err := os.WriteFile(filepath.Join(vault, "a.md"),
		[]byte("# A\n\nWith [[Rosie Cotton]] only now.\n"), 0o644); err != nil {
		t.Fatalf("rewrite a.md: %v", err)
	}
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	edges := s.outgoing(t, doc.DocumentUID, "mentions")
	if len(edges) != 1 || edges[0].ObjectUID != sarahUID {
		t.Errorf("after link removal: mentions = %+v, want exactly one edge → %s (Sam's edge forgotten)", edges, sarahUID)
	}
}

// flakyAppender wraps a real Appender. It fails the FIRST Append call that
// carries a documentRevision event (simulating a transient log failure during
// the revisions+edges stage) and records every successfully forwarded batch so
// the test can assert batch composition.
type flakyAppender struct {
	inner    document.Appender
	failOnce bool
	batches  [][]kernel.Event
}

func (f *flakyAppender) Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	hasRevision := false
	for i := range events {
		if events[i].Kind == kernel.EventKind(ontology.KindDocumentRevision) {
			hasRevision = true
			break
		}
	}
	if hasRevision && f.failOnce {
		f.failOnce = false
		return nil, errors.New("injected transient append failure")
	}
	f.batches = append(f.batches, events)
	return f.inner.Append(ctx, events)
}

// TestImport_RevisionAndEdgesAppendAtomically pins the import consistency
// contract: a document's entity event, its documentRevision, and ALL its
// wikilink edge events go to the log in ONE Append call (atomic batch), so a
// transient append failure can never leave partial import state behind —
// neither a durable revision whose content_hash makes a retry skip the file
// while its link updates were lost, nor an orphaned document entity that a
// retry (which mints a fresh document UID) would duplicate, making
// exact-name wikilinks to its title permanently ambiguous. Sequence: import
// fails on an injected append error → nothing is durable → re-import treats
// the file as CHANGED (not skipped), exactly one document entity exists for
// the title, and the entity + revision + mentions edge land in the same
// batch with the edge's provenance citing the revision's event UID.
func TestImport_RevisionAndEdgesAppendAtomically(t *testing.T) {
	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	flaky := &flakyAppender{inner: s.log, failOnce: true}
	im := document.NewImporter(flaky, s.nav, zap.NewNop())

	vault := writeVault(t, map[string]string{
		"a.md": "# A\n\nWith [[Rosie Cotton]].\n",
	})

	// First import: the combined entity+revision+edge batch fails. The whole
	// batch must be rejected — no entity, no revision row, no content_hash,
	// no edges.
	if _, err := im.ImportDir(s.ctx, kernel.DefaultScope, vault); err == nil {
		t.Fatal("first ImportDir succeeded, want injected append failure")
	}
	if _, err := s.nav.GetDocumentByPath(s.ctx, "a.md"); err == nil {
		t.Fatal("document projected despite failed revision batch; revision must not be durable")
	}
	if ents, err := s.nav.ListEntities(s.ctx, navigate.EntityFilter{NameExact: "A", Limit: 10}); err != nil {
		t.Fatalf("ListEntities after failed import: %v", err)
	} else if len(ents) != 0 {
		t.Fatalf("found %d document entities after failed import, want 0 (entity must ride the same atomic batch)", len(ents))
	}

	// Retry: the file must be treated as changed (content hash never stored),
	// re-emitting the revision AND its edges.
	res, err := im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil {
		t.Fatalf("retry ImportDir: %v", err)
	}
	if res.Imported != 1 || res.Skipped != 0 {
		t.Fatalf("retry: Imported = %d, Skipped = %d; want 1 imported, 0 skipped (failed batch must leave the file retriable)", res.Imported, res.Skipped)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "a.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath after retry: %v", err)
	}
	mentions := s.outgoing(t, doc.DocumentUID, "mentions")
	if len(mentions) != 1 || mentions[0].ObjectUID != sarahUID {
		t.Fatalf("after retry: mentions = %+v, want one edge → %s", mentions, sarahUID)
	}

	// The revision and its edge arrived in the SAME Append batch, with the
	// edge's provenance citing the revision's pre-minted event UID.
	var revBatch []kernel.Event
	for _, batch := range flaky.batches {
		for i := range batch {
			if batch[i].Kind == kernel.EventKind(ontology.KindDocumentRevision) {
				revBatch = batch
				break
			}
		}
	}
	if revBatch == nil {
		t.Fatal("no forwarded batch contains a documentRevision event")
	}
	var revUID string
	var relations []ontology.Relation
	for i := range revBatch {
		switch revBatch[i].Kind {
		case kernel.EventKind(ontology.KindDocumentRevision):
			revUID = revBatch[i].UID
		case kernel.EventKind(ontology.KindRelation):
			var rel ontology.Relation
			if uErr := json.Unmarshal(revBatch[i].Payload, &rel); uErr != nil {
				t.Fatalf("decode relation payload: %v", uErr)
			}
			relations = append(relations, rel)
		}
	}
	if revUID == "" {
		t.Fatal("documentRevision event in batch has no pre-minted UID")
	}
	if len(relations) != 1 {
		t.Fatalf("revision batch carries %d relation events, want 1 (edges must ride the same atomic batch)", len(relations))
	}
	if relations[0].SourceEventUID != revUID {
		t.Errorf("edge SourceEventUID = %q, want the batch's revision UID %q", relations[0].SourceEventUID, revUID)
	}
	entityEvents := 0
	for i := range revBatch {
		if revBatch[i].Kind == kernel.EventKind(ontology.KindEntity) {
			entityEvents++
		}
	}
	if entityEvents != 1 {
		t.Errorf("revision batch carries %d entity events, want 1 (the document entity must ride the same atomic batch)", entityEvents)
	}

	// Exactly one document entity exists for the title — the failed first
	// attempt left no orphan to duplicate.
	ents, err := s.nav.ListEntities(s.ctx, navigate.EntityFilter{NameExact: "A", Limit: 10})
	if err != nil {
		t.Fatalf("ListEntities after retry: %v", err)
	}
	if len(ents) != 1 || ents[0].UID != doc.DocumentUID {
		t.Errorf("document entities named %q after retry = %+v, want exactly one with UID %s", "A", ents, doc.DocumentUID)
	}
}

func TestImport_SingleFileBasenameCollision(t *testing.T) {
	s := newStack(t)

	dirA := writeVault(t, map[string]string{"note.md": "# One\n\ncontent one\n"})
	dirB := writeVault(t, map[string]string{"note.md": "# Two\n\ncontent two\n"})

	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, filepath.Join(dirA, "note.md")); err != nil {
		t.Fatalf("import first file: %v", err)
	}
	first, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath after first: %v", err)
	}

	// A second loose file with the same basename addresses the same vault path:
	// it must become a new REVISION of the existing document (copy-into-vault
	// overwrite semantics, prior content kept in the event log) — never a
	// second document or a unique-path conflict.
	res, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, filepath.Join(dirB, "note.md"))
	if err != nil {
		t.Fatalf("import colliding file: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("colliding import: Imported = %d, want 1", res.Imported)
	}
	second, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath after second: %v", err)
	}
	if second.DocumentUID != first.DocumentUID {
		t.Errorf("documentUID changed across the collision: %q → %q (must stay one lineage)", first.DocumentUID, second.DocumentUID)
	}
	if second.Body != "# Two\n\ncontent two\n" {
		t.Errorf("body = %q, want the second file's content", second.Body)
	}
	docs, err := s.nav.ListDocuments(s.ctx, navigate.DocumentFilter{Limit: -1})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("vault has %d documents, want 1 (no duplicate from the basename collision)", len(docs))
	}
}

// ---- P6 typed-relation + observation tests ----

// TestImport_TypedWikilinkUsesTypedPredicate verifies that a wikilink with a
// relType prefix ("[contradicts:0.8] [[Entity]]") is projected as a relation
// with the typed predicate and the stated confidence, not as a "mentions" edge.
func TestImport_TypedWikilinkUsesTypedPredicate(t *testing.T) {
	s := newStack(t)
	targetUID := s.seedEntity(t, ontology.EntityKindPerson, "Merry Brandybuck")

	vault := writeVault(t, map[string]string{
		"note.md": "# Note\n\n[contradicts:0.75] [[Merry Brandybuck]]\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	// Should have a "contradicts" edge, not "mentions".
	contradicts := s.outgoing(t, doc.DocumentUID, "contradicts")
	if len(contradicts) != 1 {
		t.Fatalf("contradicts edges = %d, want 1 (typed predicate must be used)", len(contradicts))
	}
	if contradicts[0].ObjectUID != targetUID {
		t.Errorf("contradicts edge → %q, want %q", contradicts[0].ObjectUID, targetUID)
	}
	if contradicts[0].Confidence < 0.74 || contradicts[0].Confidence > 0.76 {
		t.Errorf("confidence = %f, want ~0.75", contradicts[0].Confidence)
	}
	// Must NOT have a "mentions" edge (the typed predicate replaces, not augments).
	if got := len(s.outgoing(t, doc.DocumentUID, "mentions")); got != 0 {
		t.Errorf("mentions edges = %d, want 0 (typed predicate should replace default)", got)
	}
}

// TestImport_TypedEdgeReimportIsNoop verifies that re-importing an unchanged
// document that contains a typed wikilink does not emit duplicate typed edges.
func TestImport_TypedEdgeReimportIsNoop(t *testing.T) {
	s := newStack(t)
	s.seedEntity(t, ontology.EntityKindPerson, "Merry Brandybuck")

	vault := writeVault(t, map[string]string{
		"note.md": "# Note\n\n[supports] [[Merry Brandybuck]]\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("first import: %v", err)
	}
	doc, _ := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if got := len(s.outgoing(t, doc.DocumentUID, "supports")); got != 1 {
		t.Fatalf("after first import: %d supports edges, want 1", got)
	}

	// Unchanged content: must be skipped (content-hash dedup).
	res2, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if res2.Skipped != 1 || res2.Imported != 0 {
		t.Errorf("re-import: Skipped=%d Imported=%d, want Skipped=1 Imported=0", res2.Skipped, res2.Imported)
	}
	if got := len(s.outgoing(t, doc.DocumentUID, "supports")); got != 1 {
		t.Errorf("after re-import: %d supports edges, want 1 (no duplicate)", got)
	}
}

// TestImport_RemovedTypedLinkIsForgotten verifies that when a typed wikilink
// is removed from a document body and the document is re-imported, the typed
// edge is emitted as a relationForgotten event (the derived row disappears).
func TestImport_RemovedTypedLinkIsForgotten(t *testing.T) {
	s := newStack(t)
	s.seedEntity(t, ontology.EntityKindPerson, "Merry Brandybuck")

	vaultDir := writeVault(t, map[string]string{
		"note.md": "# Note\n\n[contradicts] [[Merry Brandybuck]]\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vaultDir); err != nil {
		t.Fatalf("first import: %v", err)
	}
	doc, _ := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if got := len(s.outgoing(t, doc.DocumentUID, "contradicts")); got != 1 {
		t.Fatalf("after first import: %d contradicts edges, want 1", got)
	}

	// Remove the typed link and re-import: the edge must be forgotten.
	if err := os.WriteFile(filepath.Join(vaultDir, "note.md"),
		[]byte("# Note\n\nAlice is mentioned in passing now.\n"), 0o644); err != nil {
		t.Fatalf("rewrite note.md: %v", err)
	}
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vaultDir); err != nil {
		t.Fatalf("re-import after removal: %v", err)
	}
	if got := len(s.outgoing(t, doc.DocumentUID, "contradicts")); got != 0 {
		t.Errorf("after link removal: %d contradicts edges, want 0 (edge must be forgotten)", got)
	}
}

// recordingAppender wraps an inner Appender and records every batch of events
// that flows through it. Used to assert that relationForgotten events carry the
// correct Reason without disassembling the SQLite projection.
type recordingAppender struct {
	inner  document.Appender
	events []kernel.Event
}

func (a *recordingAppender) Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	a.events = append(a.events, events...)
	return a.inner.Append(ctx, events)
}

// TestImport_ForgottenEdge_WikilinkRemovedReason verifies that the
// RelationForgottenEvent emitted by the deterministic reimport path carries a
// reason string that identifies the triggering revision rather than a generic
// message. The cause string is parameterized in forgottenEdgeEvents so the
// delete path (delete_move.go, #173) can supply "document deleted" without
// touching this call site.
func TestImport_ForgottenEdge_WikilinkRemovedReason(t *testing.T) {
	t.Parallel()

	s, dis := newStackWithDis(t)

	// Wire a recording appender around the real log so we can inspect the raw
	// RelationForgottenEvent payload.
	rec := &recordingAppender{inner: s.log}
	s.im = document.NewImporter(rec, s.nav, zap.NewNop())

	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	vault := writeVault(t, map[string]string{
		"note.md": "# Note\n\nWith [[Rosie Cotton]] here.\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("first import: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if got := len(s.outgoing(t, doc.DocumentUID, "mentions")); got != 1 {
		t.Fatalf("after first import: %d mentions edges, want 1", got)
	}
	_ = sarahUID // edge asserted via outgoing; sarahUID confirms resolution succeeded

	// Remove the wikilink and re-import: a relationForgotten event must be emitted.
	if err := os.WriteFile(filepath.Join(vault, "note.md"),
		[]byte("# Note\n\nNo more wikilink.\n"), 0o644); err != nil {
		t.Fatalf("rewrite note.md: %v", err)
	}
	rec.events = nil // reset; we only want events from the reimport
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("reimport: %v", err)
	}

	// Find the relationForgotten event.
	var forgottenPayload ontology.RelationForgottenEvent
	found := false
	for i := range rec.events {
		if rec.events[i].Kind == kernel.EventKind(ontology.KindRelationForgotten) {
			if err := json.Unmarshal(rec.events[i].Payload, &forgottenPayload); err != nil {
				t.Fatalf("unmarshal RelationForgottenEvent: %v", err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no RelationForgottenEvent emitted on reimport with wikilink removed")
	}

	// The reason must identify the triggering revision, not a generic string.
	if forgottenPayload.Reason == "" {
		t.Error("RelationForgottenEvent.Reason is empty")
	}
	if !strings.Contains(forgottenPayload.Reason, "wikilink removed in document revision") {
		t.Errorf("RelationForgottenEvent.Reason = %q, want prefix 'wikilink removed in document revision'", forgottenPayload.Reason)
	}
	// A document revision UID is a 26-char ULID; the reason must contain one.
	if len(forgottenPayload.Reason) <= len("wikilink removed in document revision ") {
		t.Errorf("RelationForgottenEvent.Reason = %q, want a trailing ULID revision UID", forgottenPayload.Reason)
	}

	// Edge is gone from the projection.
	if got := len(s.outgoing(t, doc.DocumentUID, "mentions")); got != 0 {
		t.Errorf("after wikilink removal: %d mentions edges, want 0", got)
	}

	_ = dis // dis is held open for cleanup via t.Cleanup; not queried in this test
}

// queryFacts returns facts from the SQLite facts table whose subject_uid matches.
func (s *stack) queryFacts(t *testing.T, dis *dispatcher.Dispatcher, subjectUID string) []struct {
	StateKey string
	Value    string
	Hash     string
} {
	t.Helper()
	reader, err := dis.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	rows, err := reader.QueryContext(s.ctx,
		`SELECT state_key, value, content_hash FROM facts WHERE subject_uid = ? ORDER BY rowid`,
		subjectUID)
	if err != nil {
		t.Fatalf("query facts: %v", err)
	}
	defer rows.Close()
	var out []struct {
		StateKey string
		Value    string
		Hash     string
	}
	for rows.Next() {
		var r struct {
			StateKey string
			Value    string
			Hash     string
		}
		if err := rows.Scan(&r.StateKey, &r.Value, &r.Hash); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate facts: %v", err)
	}
	return out
}

// queryOpenFacts returns the (state_key, value) of the currently-OPEN facts
// (event_end IS NULL) for a subject. Observation facts must each occupy their
// own open slot; a collision on one state_key would silently supersede all but
// the last, so a row-count alone (queryFacts) would hide the data loss.
func (s *stack) queryOpenFacts(t *testing.T, dis *dispatcher.Dispatcher, subjectUID string) map[string]string {
	t.Helper()
	reader, err := dis.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	rows, err := reader.QueryContext(s.ctx,
		`SELECT state_key, value FROM facts WHERE subject_uid = ? AND event_end IS NULL ORDER BY rowid`,
		subjectUID)
	if err != nil {
		t.Fatalf("query open facts: %v", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatalf("scan open fact: %v", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate open facts: %v", err)
	}
	return out
}

// newStackWithDis returns the stack and the underlying dispatcher so tests
// can query the facts table directly via SQLiteReader.
func newStackWithDis(t *testing.T) (*stack, *dispatcher.Dispatcher) {
	t.Helper()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()

	dis, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	log := kernel.NewFileEventLog(dataDir, noopOntology{}, kernel.WithStorage(dis))
	t.Cleanup(func() { _ = dis.Close() })
	t.Cleanup(func() { _ = log.Close() })

	reader, err := dis.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	st := &stack{ctx: ctx, log: log, nav: nav, im: document.NewImporter(log, nav, zap.NewNop())}
	return st, dis
}

// TestImport_ObservationsProjectToFacts verifies that observation lines in a
// document body are projected to Fact events in the SQLite facts table, with
// the correct stateKey (observation.<category>) and value (text + context + tags).
func TestImport_ObservationsProjectToFacts(t *testing.T) {
	s, dis := newStackWithDis(t)

	vault := writeVault(t, map[string]string{
		"note.md": "# Research\n\n" +
			"- [finding] The earth is round #science (confirmed)\n" +
			"- [todo] Write unit tests #testing\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	facts := s.queryFacts(t, dis, doc.DocumentUID)
	if len(facts) != 2 {
		t.Fatalf("fact count = %d, want 2 (one per observation line)", len(facts))
	}

	// Verify stateKey format: timeless.<category>.<discriminator>. The category
	// is the prefix; a per-value discriminator follows so same-category
	// observations don't collide on one slot.
	var findingKey string
	for _, f := range facts {
		switch {
		case strings.HasPrefix(f.StateKey, "timeless.finding."):
			findingKey = f.StateKey
		case strings.HasPrefix(f.StateKey, "timeless.todo."):
		default:
			t.Errorf("unexpected stateKey %q, want a timeless.<category>.<disc> form", f.StateKey)
		}
	}
	if findingKey == "" {
		t.Errorf("no timeless.finding.* stateKey present in %v", facts)
	}

	// The value for "finding" should contain the text, context, and tag.
	for _, f := range facts {
		if f.StateKey == findingKey {
			if f.Value == "" {
				t.Error("observation.finding value is empty")
			}
			// Text is "The earth is round #science"
			if !contains(f.Value, "The earth is round") {
				t.Errorf("observation.finding value = %q, want text 'The earth is round'", f.Value)
			}
			// Context "(confirmed)" is appended.
			if !contains(f.Value, "confirmed") {
				t.Errorf("observation.finding value = %q, want context 'confirmed'", f.Value)
			}
		}
	}

	// Content hash must be set (non-empty) for dedup.
	for _, f := range facts {
		if f.Hash == "" {
			t.Errorf("fact %q has empty content_hash; dedup will not work", f.StateKey)
		}
	}
}

// TestImport_ObservationsReimportIsNoop verifies that re-importing an unchanged
// document does not produce duplicate fact rows.
func TestImport_ObservationsReimportIsNoop(t *testing.T) {
	s, dis := newStackWithDis(t)

	vault := writeVault(t, map[string]string{
		"note.md": "# Note\n\n- [fact] Coffee is delicious #beverage\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("first import: %v", err)
	}
	doc, _ := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if got := len(s.queryFacts(t, dis, doc.DocumentUID)); got != 1 {
		t.Fatalf("after first import: %d facts, want 1", got)
	}

	// Re-import unchanged content: content-hash dedup should skip the file
	// entirely, so fact count stays at 1.
	res2, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if res2.Skipped != 1 {
		t.Errorf("re-import: Skipped=%d, want 1", res2.Skipped)
	}
	if got := len(s.queryFacts(t, dis, doc.DocumentUID)); got != 1 {
		t.Errorf("after re-import: %d facts, want 1 (no duplicate)", got)
	}
}

// TestImport_MultipleSameCategoryObservationsAllSurvive proves that a note with
// several observations of the SAME category each project to their own open fact
// slot. Keying every "[fact]" line on a single timeless.fact slot would make the
// last line supersede the rest — silent data loss — so each distinct observation
// value must get a distinct state_key and stay open.
func TestImport_MultipleSameCategoryObservationsAllSurvive(t *testing.T) {
	s, dis := newStackWithDis(t)

	vault := writeVault(t, map[string]string{
		"note.md": "# Rosie\n\n" +
			"- [fact] Rosie prefers async written updates #comms\n" +
			"- [fact] Rosie works in the EU timezone #logistics\n" +
			"- [fact] Rosie owns the billing service #ownership\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	open := s.queryOpenFacts(t, dis, doc.DocumentUID)
	if len(open) != 3 {
		t.Fatalf("open observation facts = %d (%v), want 3 (one per [fact] line; none superseded)", len(open), open)
	}
	// Every distinct observation value must be present as an open fact.
	wantValues := []string{
		"Rosie prefers async written updates #comms",
		"Rosie works in the EU timezone #logistics",
		"Rosie owns the billing service #ownership",
	}
	present := make(map[string]bool, len(open))
	for _, v := range open {
		present[v] = true
	}
	for _, want := range wantValues {
		if !present[want] {
			t.Errorf("open facts %v missing value %q", open, want)
		}
	}
}

// TestImport_CollidingCategoryObservationsKeepBothFacts proves that two
// observations whose [category] labels are DISTINCT but sanitize to the SAME
// StateKey segment do not collapse onto one slot when they share the same value
// text. The category sanitiser is lossy ("a-b" and "ab" both reduce to segment
// "ab"), so a value-only discriminator would key both on
// timeless.ab.h<hash(value)> and the supersede pipeline would silently drop one
// observation. Folding the normalized category into the discriminator keeps the
// slots distinct.
func TestImport_CollidingCategoryObservationsKeepBothFacts(t *testing.T) {
	s, dis := newStackWithDis(t)

	vault := writeVault(t, map[string]string{
		"note.md": "# Release\n\n" +
			"- [a-b] ships on friday\n" +
			"- [ab] ships on friday\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	open := s.queryOpenFacts(t, dis, doc.DocumentUID)
	if len(open) != 2 {
		t.Fatalf("open observation facts = %d (%v), want 2 (distinct categories must not collide on one slot)", len(open), open)
	}
}

// TestImport_SameCategoryObservationsReimportIsNoop proves the per-observation
// state_key is content-stable: re-importing the identical note re-emits the same
// slots as no-ops, so the open-fact set neither grows nor churns.
func TestImport_SameCategoryObservationsReimportIsNoop(t *testing.T) {
	s, dis := newStackWithDis(t)

	vault := writeVault(t, map[string]string{
		"note.md": "# Rosie\n\n" +
			"- [fact] Rosie prefers async written updates #comms\n" +
			"- [fact] Rosie works in the EU timezone #logistics\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("first import: %v", err)
	}
	doc, _ := s.nav.GetDocumentByPath(s.ctx, "note.md")
	first := s.queryOpenFacts(t, dis, doc.DocumentUID)
	if len(first) != 2 {
		t.Fatalf("after first import: %d open facts, want 2", len(first))
	}

	// Re-import the identical vault: content-hash dedup skips the file, so the
	// open-fact set is unchanged (no duplicate slot, no supersede churn).
	res2, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if res2.Skipped != 1 {
		t.Errorf("re-import: Skipped=%d, want 1", res2.Skipped)
	}
	second := s.queryOpenFacts(t, dis, doc.DocumentUID)
	if len(second) != 2 {
		t.Errorf("after re-import: %d open facts, want 2 (no duplicate, no churn)", len(second))
	}
}

// validatingOntologyStack is like newStackWithDis but wires the production
// MemoryOntology with all schemas registered (including Fact). Tests that use
// this stack exercise the validating path, ensuring observation Fact events pass
// kernel schema validation on Append and are not silently bypassed.
func validatingOntologyStack(t *testing.T) (*stack, *dispatcher.Dispatcher) {
	t.Helper()
	ctx := testutil.TestContext(t)
	dataDir := t.TempDir()

	dis, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  dataDir,
		Embedder: fakeEmbedder{dim: storage.VectorDim},
	})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}

	// Build the real validating ontology: register all schemas the importer
	// produces (Entity, Relation, DocumentRevision, DocumentDeleted, Fact).
	// A Fact event that fails schema validation will surface as an Append error.
	ont := kernel.NewMemoryOntology()
	for _, reg := range []func(context.Context, kernel.Ontology) error{
		ontology.RegisterEpisode,
		ontology.RegisterEntity,
		ontology.RegisterMention,
		ontology.RegisterRelation,
		ontology.RegisterDocumentRevision,
		ontology.RegisterDocumentDeleted,
		ontology.RegisterFact,
	} {
		if err := reg(ctx, ont); err != nil {
			t.Fatalf("register ontology: %v", err)
		}
	}

	log := kernel.NewFileEventLog(dataDir, ont, kernel.WithStorage(dis))
	t.Cleanup(func() { _ = dis.Close() })
	t.Cleanup(func() { _ = log.Close() })

	reader, err := dis.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	nav, err := navigate.New(reader)
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	st := &stack{ctx: ctx, log: log, nav: nav, im: document.NewImporter(log, nav, zap.NewNop())}
	return st, dis
}

// TestImport_ObservationFactValidatesAgainstSchema proves that observation-derived
// Fact events pass the kernel's JSON Schema validation gate at Append time (the
// real MemoryOntology is wired, not the noopOntology that bypasses validation).
// A stateKey of the form "timeless.<category>" satisfies the Fact schema regex
// and the mem0 first-segment requirement; "observation.<category>" would satisfy
// the regex but not the semantic constraint documented in ontology.Fact.
func TestImport_ObservationFactValidatesAgainstSchema(t *testing.T) {
	t.Parallel()
	s, dis := validatingOntologyStack(t)

	// Import a document with two observation bullets. If the generated Fact
	// stateKey is invalid, the kernel rejects it at Append with ErrInvalidEvent.
	vault := writeVault(t, map[string]string{
		"research.md": "# Research\n\n" +
			"- [decision] Adopt Go 1.22 for new services #infra\n" +
			"- [risk] Dependency on external API #vendor\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir with validating ontology: %v (schema validation rejected a Fact payload)", err)
	}

	// Confirm facts landed in the table and are retrievable via the navigator.
	doc, err := s.nav.GetDocumentByPath(s.ctx, "research.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	facts := s.queryFacts(t, dis, doc.DocumentUID)
	if len(facts) != 2 {
		t.Fatalf("fact count = %d, want 2; stateKeys present: %v",
			len(facts), func() []string {
				ss := make([]string, len(facts))
				for i, f := range facts {
					ss[i] = f.StateKey
				}
				return ss
			}())
	}

	// Confirm the stateKey scheme: timeless.<category>.
	for _, f := range facts {
		if len(f.StateKey) < len("timeless.") || f.StateKey[:len("timeless.")] != "timeless." {
			t.Errorf("stateKey = %q, want prefix timeless.", f.StateKey)
		}
	}

	// Navigator must retrieve them via FactsForSubject.
	navFacts, err := s.nav.FactsForSubject(s.ctx, doc.DocumentUID, navigate.FactFilter{})
	if err != nil {
		t.Fatalf("FactsForSubject: %v", err)
	}
	if len(navFacts) != 2 {
		t.Fatalf("navFacts count = %d, want 2; open facts in slot: %+v", len(navFacts), navFacts)
	}
}

// TestImport_EntitySelfFrontmatter_SchemaValidating reproduces the live
// binary bug: a note with `entity: self` frontmatter emits a Relation
// event with objectUid="self", which the Relation JSON Schema previously
// rejected (objectUid pattern was ULID-only). The atomic import batch
// then aborted — no document row, no edges.
//
// This test uses the REAL validating MemoryOntology (not the noopOntology
// that bypasses schema checks), so it fails before the objectUid schema
// pattern is widened to accept the "self" sentinel and passes after.
//
// Verification:
//  1. ImportDir succeeds (no E_INTERNAL schema error).
//  2. The document row exists in the navigator.
//  3. A mentions edge from the document to the self entity ("self") exists.
//  4. Neighbors of the self entity includes the document (backlink surface).
//  5. The doc --mentions--> self edge is traversable in the LadybugDB graph
//     (issue #175: the self node is now seeded by migration 0004, so the
//     graph-hop lane resolves the edge instead of silently dropping it).
func TestImport_EntitySelfFrontmatter_SchemaValidating(t *testing.T) {
	t.Parallel()
	s, dis := validatingOntologyStack(t)

	// The canonical self entity is seeded by dispatcher.Open: SQLite migration
	// 0012 (entities row) and LadybugDB migration 0004 (graph node). The doc↔self
	// edge is asserted via both the pure-SQLite Navigator surface and the graph
	// lane below.

	vault := writeVault(t, map[string]string{
		"notes/diary.md": "---\nentity: self\ntitle: My Diary\n---\n# My Diary\n\nPersonal note.\n",
	})

	// Import must succeed. Before the schema fix this returned
	// [E_INTERNAL] ... at '/objectUid': 'self' does not match pattern
	// '^[0-9A-HJKMNP-TV-Z]{26}$' and created no document row.
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir with entity: self frontmatter failed (schema validation rejected objectUid='self'): %v", err)
	}

	// Document row must exist.
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/diary.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: no document row after import — import was aborted: %v", err)
	}

	// A mentions edge from the document to the self entity must exist.
	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	selfEdge := false
	for i := range rels {
		if rels[i].SubjectUID == doc.DocumentUID && rels[i].ObjectUID == "self" {
			selfEdge = true
		}
	}
	if !selfEdge {
		t.Error("no mentions edge from document to self entity; entity: self frontmatter must produce a doc --mentions--> self edge")
	}

	// Backlink surface: neighbors of self must include the document.
	neighbors, err := s.nav.Neighbors(s.ctx, "self", 1, navigate.RelationFilter{
		Predicates: []string{"mentions"},
	})
	if err != nil {
		t.Fatalf("Neighbors(self): %v", err)
	}
	docInNeighbors := false
	for i := range neighbors {
		if neighbors[i].Entity.UID == doc.DocumentUID {
			docInNeighbors = true
		}
	}
	if !docInNeighbors {
		t.Error("document is not in the neighbors of the self entity; backlink surface is broken")
	}

	// Graph lane (issue #175): the doc --mentions--> self edge must be
	// traversable in LadybugDB. Before migration 0004 seeded the self node,
	// mergeRelationEdge's MATCH (o:Entity {uid:"self"}) bound nothing and the
	// edge was silently dropped from the graph; this asserts it is now present.
	graph, err := dis.OpenGraph(s.ctx)
	if err != nil {
		t.Fatalf("OpenGraph: %v", err)
	}
	gr, err := graph.Execute(s.ctx,
		`MATCH (d:Entity {uid: $doc})-[r:RELATES_TO {predicate: 'mentions'}]->(:Entity {uid: 'self'}) RETURN count(r)`,
		map[string]any{"doc": doc.DocumentUID})
	if err != nil {
		t.Fatalf("graph MATCH doc->self: %v", err)
	}
	defer func() { _ = gr.Close() }()
	if !gr.Next() {
		if err := gr.Err(); err != nil {
			t.Fatalf("graph result err: %v", err)
		}
		t.Fatal("graph query returned no rows")
	}
	var edgeCount int64
	if err := gr.Scan(&edgeCount); err != nil {
		t.Fatalf("scan edge count: %v", err)
	}
	if edgeCount != 1 {
		t.Errorf("doc --mentions--> self graph edges = %d, want 1 (self node must be graph-traversable)", edgeCount)
	}
}

// contains reports whether s contains sub (simple substring check for tests).
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := range s {
		if i+len(sub) <= len(s) && s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
