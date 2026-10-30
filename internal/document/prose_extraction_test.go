package document_test

// prose_extraction_test.go verifies three coherent parts added by the
// document-prose-extraction feature:
//
//  1. Prose extraction: a fake ProseExtractor is wired into the Importer;
//     importing a doc whose body mentions a person (no wikilink) produces a
//     mentions edge after extraction settles. Idempotency: re-import with
//     unchanged content hash does not trigger a second extract call. Changed
//     body triggers re-extraction; no duplicate edges from wikilink + extract.
//  2. Frontmatter typed links: about: scalar, about: list, entity: self,
//     unresolvable about (joins dangling snapshot), self-link suppressed.
//  3. Rebuild-replay safety: unchanged content -> no extraction call fired.
//
// All tests use the real four-layer dispatcher stack (same as
// importer_integration_test.go) and run with -race.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// countingExtractor is a fake ProseExtractor that records call count and
// returns a configurable set of mentions.
type countingExtractor struct {
	mu       sync.Mutex
	calls    int64
	mentions []ontology.Mention // returned on every call
}

func (f *countingExtractor) ExtractProse(_ context.Context, _ string, _ string) ([]ontology.Mention, error) {
	atomic.AddInt64(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ontology.Mention, len(f.mentions))
	copy(out, f.mentions)
	return out, nil
}

func (f *countingExtractor) callCount() int64 { return atomic.LoadInt64(&f.calls) }

// stackWithExtractor builds a test stack with the given ProseExtractor wired.
func stackWithExtractor(t *testing.T, ext document.ProseExtractor) *stack {
	t.Helper()
	s := newStack(t)
	s.im = document.NewImporter(s.log, s.nav, zap.NewNop()).WithProseExtractor(ext)
	return s
}

// waitForProseEdge polls the navigator until a mentions edge from docUID to
// entityUID appears or the 5-second deadline is reached.
func waitForProseEdge(t *testing.T, ctx context.Context, nav *navigate.Navigator, docUID, entityUID string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rels, err := nav.EntityRelations(ctx, docUID, navigate.RelationFilter{
			Predicates: []string{"mentions"}, Limit: -1,
		})
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		for i := range rels {
			if rels[i].SubjectUID == docUID && rels[i].ObjectUID == entityUID {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// rewriteFile updates one file inside an existing vault directory.
func rewriteFile(vaultDir, rel, content string) error {
	full := filepath.Join(vaultDir, filepath.FromSlash(rel))
	return os.WriteFile(full, []byte(content), 0o644)
}

// --- Part 1: Prose extraction tests ---

func TestProseExtraction_BodyMentionProducesEdge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sarahMention := ontology.Mention{
		Kind:           ontology.EntityKindPerson,
		DisplayName:    "Rosie Cotton",
		ExtractionText: "Rosie Cotton",
		SourceEventUID: ontology.ULID("01JXXXXXXXXXXXXXXXXXXXXXXXX"),
		GroundingScore: 0.9,
	}
	ext := &countingExtractor{mentions: []ontology.Mention{sarahMention}}
	s := stackWithExtractor(t, ext)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	vault := writeVault(t, map[string]string{
		"notes/meeting.md": "# Meeting Notes\n\nRosie Cotton spoke about the roadmap.\n",
	})
	res, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("Imported = %d, want 1", res.Imported)
	}

	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/meeting.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	// Wait for the async extraction goroutine to fire and append the edge.
	if !waitForProseEdge(t, ctx, s.nav, doc.DocumentUID, sarahUID) {
		t.Fatalf("no mentions edge from doc to Rosie Cotton after prose extraction settled")
	}

	// The person's neighbors must now include the document.
	neighbors, err := s.nav.Neighbors(s.ctx, sarahUID, 1, navigate.RelationFilter{
		Predicates: []string{"mentions"},
	})
	if err != nil {
		t.Fatalf("Neighbors(sarah): %v", err)
	}
	found := false
	for _, nb := range neighbors {
		if nb.Entity.UID == doc.DocumentUID {
			found = true
		}
	}
	if !found {
		t.Error("Neighbors(Rosie) does not include the meeting document after prose extraction")
	}
}

func TestProseExtraction_UnchangedContentNoSecondCall(t *testing.T) {
	t.Parallel()

	sarahMention := ontology.Mention{
		Kind: ontology.EntityKindPerson, DisplayName: "Rosie Cotton",
		ExtractionText: "Rosie Cotton",
		SourceEventUID: ontology.ULID("01JXXXXXXXXXXXXXXXXXXXXXXXX"),
		GroundingScore: 0.9,
	}
	ext := &countingExtractor{mentions: []ontology.Mention{sarahMention}}
	s := stackWithExtractor(t, ext)
	s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	vault := writeVault(t, map[string]string{
		"notes/meeting.md": "# Meeting\n\nRosie Cotton was here.\n",
	})

	// First import.
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("first ImportDir: %v", err)
	}

	// Re-import same content (unchanged hash).
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("second ImportDir: %v", err)
	}
	// Give async goroutines time to settle.
	time.Sleep(300 * time.Millisecond)

	// Extraction should have fired exactly once (first import only).
	if got := ext.callCount(); got != 1 {
		t.Errorf("extract calls = %d, want 1 (unchanged content must not re-extract)", got)
	}
}

func TestProseExtraction_ChangedBodyReExtracts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sarahMention := ontology.Mention{
		Kind: ontology.EntityKindPerson, DisplayName: "Rosie Cotton",
		ExtractionText: "Rosie Cotton",
		SourceEventUID: ontology.ULID("01JXXXXXXXXXXXXXXXXXXXXXXXX"),
		GroundingScore: 0.9,
	}
	ext := &countingExtractor{mentions: []ontology.Mention{sarahMention}}
	s := stackWithExtractor(t, ext)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	vault := writeVault(t, map[string]string{
		"notes/note.md": "# Note\n\nRosie Cotton is here.\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("first ImportDir: %v", err)
	}

	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if !waitForProseEdge(t, ctx, s.nav, doc.DocumentUID, sarahUID) {
		t.Fatal("no mentions edge after first import")
	}

	// Change the body content.
	if err := rewriteFile(vault, "notes/note.md", "# Note\n\nRosie Cotton returned with updates.\n"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("second ImportDir: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// Extraction called twice (one per changed body).
	if got := ext.callCount(); got != 2 {
		t.Errorf("extract calls = %d, want 2 (changed content must re-extract)", got)
	}

	// Still exactly one mentions edge (no duplicate from re-extraction).
	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	count := 0
	for i := range rels {
		r := rels[i]
		if r.SubjectUID == doc.DocumentUID && r.ObjectUID == sarahUID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("mentions edges doc→sarah = %d, want 1 (no duplicates after re-extraction)", count)
	}
}

func TestProseExtraction_NoDuplicateVsWikilink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Body has [[Rosie Cotton]] wikilink AND extractor returns Rosie as mention.
	sarahMention := ontology.Mention{
		Kind: ontology.EntityKindPerson, DisplayName: "Rosie Cotton",
		ExtractionText: "Rosie Cotton",
		SourceEventUID: ontology.ULID("01JXXXXXXXXXXXXXXXXXXXXXXXX"),
		GroundingScore: 0.9,
	}
	ext := &countingExtractor{mentions: []ontology.Mention{sarahMention}}
	s := stackWithExtractor(t, ext)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	vault := writeVault(t, map[string]string{
		"notes/note.md": "# Note\n\n[[Rosie Cotton]] is mentioned explicitly.\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}

	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if !waitForProseEdge(t, ctx, s.nav, doc.DocumentUID, sarahUID) {
		t.Fatal("no mentions edge after extraction")
	}
	time.Sleep(100 * time.Millisecond)

	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	count := 0
	for i := range rels {
		r := rels[i]
		if r.SubjectUID == doc.DocumentUID && r.ObjectUID == sarahUID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("mentions edges = %d, want 1 (wikilink + extract must not duplicate)", count)
	}
}

// TestProseExtraction_ExplicitWikilinkWinsOverSameNameDifferentEntity is the
// regression for the same-display-name ambiguity in the MCP failure report:
// "Mithrilmail" exists as BOTH a person and a project. The body's explicit
// [[Mithrilmail]] resolves to the kind-priority winner (person), and an async LLM
// prose mention that resolves the same name to the OTHER entity (the project)
// must NOT add a second, competing edge. The user's explicit link is
// authoritative for that name, so the document ends with exactly one mentions
// edge — to the person, never the project.
func TestProseExtraction_ExplicitWikilinkWinsOverSameNameDifferentEntity(t *testing.T) {
	t.Parallel()

	// The extractor resolves "Mithrilmail" to the PROJECT (kind-gated), the opposite
	// of the wikilink's person winner — the exact divergence we must suppress.
	projectMention := ontology.Mention{
		Kind: ontology.EntityKindProject, DisplayName: "Mithrilmail",
		ExtractionText: "Mithrilmail", GroundingScore: 0.9,
	}
	ext := &countingExtractor{mentions: []ontology.Mention{projectMention}}
	s := stackWithExtractor(t, ext)
	personUID := s.seedEntity(t, ontology.EntityKindPerson, "Mithrilmail")
	projectUID := s.seedEntity(t, ontology.EntityKindProject, "Mithrilmail")

	vault := writeVault(t, map[string]string{
		"notes/note.md": "# Note\n\nSee [[Mithrilmail]] for the milestone.\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	// Wait for the async extraction to fire (so the suppression path has run),
	// then let any append settle before asserting the edge set is final.
	deadline := time.Now().Add(2 * time.Second)
	for ext.callCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	var toPerson, toProject int
	for i := range rels {
		r := rels[i]
		if r.SubjectUID != doc.DocumentUID {
			continue
		}
		switch r.ObjectUID {
		case personUID:
			toPerson++
		case projectUID:
			toProject++
		}
	}
	if toPerson != 1 {
		t.Errorf("mentions edges doc→person Mithrilmail = %d, want 1 (the explicit [[Mithrilmail]] winner)", toPerson)
	}
	if toProject != 0 {
		t.Errorf("mentions edges doc→project Mithrilmail = %d, want 0 (prose must defer to the explicit wikilink)", toProject)
	}
}

// --- Part 2: Frontmatter typed-link tests ---

func TestFrontmatterAbout_Scalar(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	vault := writeVault(t, map[string]string{
		"notes/note.md": "---\nabout: Rosie Cotton\n---\n# Note\n\nNo wikilink here.\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"describes"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	found := false
	for i := range rels {
		r := rels[i]
		if r.SubjectUID == doc.DocumentUID && r.ObjectUID == sarahUID {
			found = true
		}
	}
	if !found {
		t.Error("frontmatter about: scalar did not produce a describes edge to Rosie Cotton")
	}
}

func TestFrontmatterAbout_WikilinkSyntax(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	vault := writeVault(t, map[string]string{
		"notes/note.md": "---\nabout: \"[[Rosie Cotton]]\"\n---\n# Note\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"describes"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	found := false
	for i := range rels {
		r := rels[i]
		if r.SubjectUID == doc.DocumentUID && r.ObjectUID == sarahUID {
			found = true
		}
	}
	if !found {
		t.Error("frontmatter about: [[wikilink]] syntax did not produce a describes edge")
	}
}

func TestFrontmatterAbout_List(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")
	acmeUID := s.seedEntity(t, ontology.EntityKindOrganization, "Mithril Corp")

	vault := writeVault(t, map[string]string{
		"notes/note.md": "---\nabout:\n  - Rosie Cotton\n  - Mithril Corp\n---\n# Note\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"describes"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	targets := make(map[string]bool)
	for i := range rels {
		r := rels[i]
		if r.SubjectUID == doc.DocumentUID {
			targets[r.ObjectUID] = true
		}
	}
	if !targets[sarahUID] {
		t.Error("frontmatter about: list missing describes edge to Rosie Cotton")
	}
	if !targets[acmeUID] {
		t.Error("frontmatter about: list missing describes edge to Mithril Corp")
	}
}

func TestFrontmatterEntitySelf(t *testing.T) {
	t.Parallel()

	// The canonical self entity is seeded automatically by dispatcher.Open:
	// SQLite migration 0012 (entities row) and LadybugDB migration 0004 (graph
	// node, issue #175). The assertions below use the pure-SQLite Navigator
	// surface; the graph node is exercised in
	// TestImport_EntitySelfFrontmatter_SchemaValidating.
	s, _ := newStackWithDis(t)

	vault := writeVault(t, map[string]string{
		"notes/diary.md": "---\nentity: self\n---\n# Diary\n\nPersonal note.\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/diary.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	found := false
	for i := range rels {
		r := rels[i]
		if r.SubjectUID == doc.DocumentUID && r.ObjectUID == "self" {
			found = true
		}
	}
	if !found {
		t.Error("entity: self did not produce a mentions edge to the self entity")
	}
}

func TestFrontmatterAbout_Unresolvable(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	// Do NOT seed the entity — the link should be unresolvable.

	vault := writeVault(t, map[string]string{
		"notes/note.md": "---\nabout: Unknown Person XYZ\n---\n# Note\n",
	})
	res, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	// Unresolved frontmatter link is counted in res.Unresolved.
	if res.Unresolved < 1 {
		t.Errorf("Unresolved = %d, want >= 1 for unresolvable about: target", res.Unresolved)
	}
	// Import must still succeed (non-fatal).
	if res.Imported != 1 {
		t.Errorf("Imported = %d, want 1", res.Imported)
	}
}

func TestFrontmatterAbout_NoDuplicateVsBodyWikilink(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	// Both frontmatter and body wikilink name Rosie.
	vault := writeVault(t, map[string]string{
		"notes/note.md": "---\nabout: Rosie Cotton\n---\n# Note\n\n[[Rosie Cotton]] is great.\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	count := 0
	for i := range rels {
		r := rels[i]
		if r.SubjectUID == doc.DocumentUID && r.ObjectUID == sarahUID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("mentions edges doc→sarah = %d, want 1 (frontmatter + body wikilink must not duplicate)", count)
	}
}

func TestFrontmatterEntitySelf_TargetsUserEntityNotDoc(t *testing.T) {
	t.Parallel()

	// The self entity is seeded automatically by dispatcher.Open (SQLite
	// migration 0012 + LadybugDB migration 0004). See TestFrontmatterEntitySelf.
	s, _ := newStackWithDis(t)

	vault := writeVault(t, map[string]string{
		"notes/diary.md": "---\nentity: self\ntitle: My Diary\n---\nPersonal.\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/diary.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	selfEdge := false
	selfLoopEdge := false
	for i := range rels {
		r := rels[i]
		if r.SubjectUID == doc.DocumentUID && r.ObjectUID == "self" {
			selfEdge = true
		}
		if r.SubjectUID == doc.DocumentUID && r.ObjectUID == doc.DocumentUID {
			selfLoopEdge = true
		}
	}
	if !selfEdge {
		t.Error("entity: self must produce a mentions edge to the self entity UID")
	}
	if selfLoopEdge {
		t.Error("entity: self must not produce a self-loop on the document")
	}
}

// togglingExtractor returns its mentions on the first call and nothing on
// every subsequent call, simulating a transient LLM hiccup (or extraction
// going dark) on a later edit while the prose still names the same entity.
type togglingExtractor struct {
	mu       sync.Mutex
	n        int
	mentions []ontology.Mention
}

func (e *togglingExtractor) ExtractProse(_ context.Context, _ string, _ string) ([]ontology.Mention, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n++
	if e.n == 1 {
		out := make([]ontology.Mention, len(e.mentions))
		copy(out, e.mentions)
		return out, nil
	}
	return nil, nil
}

// gatedExtractor blocks in ExtractProse until its gate is closed, widening the
// read-then-append window so concurrent same-document extractions overlap.
type gatedExtractor struct {
	gate     chan struct{}
	calls    int64
	mentions []ontology.Mention
}

func (e *gatedExtractor) ExtractProse(_ context.Context, _ string, _ string) ([]ontology.Mention, error) {
	atomic.AddInt64(&e.calls, 1)
	<-e.gate
	out := make([]ontology.Mention, len(e.mentions))
	copy(out, e.mentions)
	return out, nil
}

// TestProseExtraction_EdgeSurvivesEditWhenReExtractEmpty is the regression for
// Greptile finding 1: a prose-extracted edge must NOT be deleted by the
// deterministic forget pass on a later edit, even when re-extraction returns
// nothing (the prose edge is best-effort but durable until the body changes
// meaningfully — it must never silently vanish on an unrelated edit).
func TestProseExtraction_EdgeSurvivesEditWhenReExtractEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sarah := ontology.Mention{
		Kind: ontology.EntityKindPerson, DisplayName: "Rosie Cotton",
		ExtractionText: "Rosie Cotton", GroundingScore: 0.9,
	}
	ext := &togglingExtractor{mentions: []ontology.Mention{sarah}}
	s := stackWithExtractor(t, ext)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	// First import: prose extracts Rosie (no wikilink in body).
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/n.md", []byte("# N\n\nRosie Cotton discussed plans.\n")); err != nil {
		t.Fatalf("import1: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/n.md")
	if err != nil {
		t.Fatalf("get doc: %v", err)
	}
	if !waitForProseEdge(t, ctx, s.nav, doc.DocumentUID, sarahUID) {
		t.Fatal("no prose edge after first import")
	}

	// Edit the body (still prose-mentions Rosie) but the extractor now returns
	// nothing. The deterministic forget pass must leave the prose edge alone.
	if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/n.md", []byte("# N\n\nRosie Cotton discussed updated plans here.\n")); err != nil {
		t.Fatalf("import2: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("relations: %v", err)
	}
	found := false
	for i := range rels {
		if rels[i].SubjectUID == doc.DocumentUID && rels[i].ObjectUID == sarahUID {
			found = true
		}
	}
	if !found {
		t.Error("prose edge was lost after an edit whose re-extraction returned empty")
	}
}

// TestProseExtraction_ConcurrentNoDuplicateEdges is the regression for Greptile
// finding 2: two concurrent extractions of the SAME document must not both
// append the same mentions edge. The gated extractor blocks both goroutines in
// Extract, then releases them together so they race into the dedup-and-append
// critical section; the per-document lock must serialize them to one edge.
func TestProseExtraction_ConcurrentNoDuplicateEdges(t *testing.T) {
	t.Parallel()

	sarah := ontology.Mention{
		Kind: ontology.EntityKindPerson, DisplayName: "Rosie Cotton",
		ExtractionText: "Rosie Cotton", GroundingScore: 0.9,
	}
	ext := &gatedExtractor{gate: make(chan struct{}), mentions: []ontology.Mention{sarah}}
	s := stackWithExtractor(t, ext)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")

	// Two edits of the same doc with different bodies (both change the content
	// hash and both prose-mention Rosie), so two extraction goroutines launch.
	for _, body := range []string{
		"# Note v1\n\nRosie Cotton first.\n",
		"# Note v2\n\nRosie Cotton second edit here.\n",
	} {
		if _, err := s.im.ImportContent(s.ctx, kernel.DefaultScope, "notes/edit.md", []byte(body)); err != nil {
			t.Fatalf("import: %v", err)
		}
	}

	// Wait until both extraction goroutines are blocked in Extract, then release.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&ext.calls) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(ext.gate)
	time.Sleep(500 * time.Millisecond)

	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/edit.md")
	if err != nil {
		t.Fatalf("get doc: %v", err)
	}
	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"mentions"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("relations: %v", err)
	}
	count := 0
	for i := range rels {
		if rels[i].SubjectUID == doc.DocumentUID && rels[i].ObjectUID == sarahUID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("mentions edges doc→sarah = %d, want 1 (concurrent extraction must not duplicate)", count)
	}
}

// TestFrontmatterAbout_FlowListWikilink is the regression for Greptile finding
// 4: the documented `about: [Rosie Cotton, [[Mithril]]]` flow-list shape decodes
// the unquoted [[Mithril]] as a nested sequence; the parser must still recover it
// as a wikilink target rather than silently dropping it.
func TestFrontmatterAbout_FlowListWikilink(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	sarahUID := s.seedEntity(t, ontology.EntityKindPerson, "Rosie Cotton")
	acmeUID := s.seedEntity(t, ontology.EntityKindOrganization, "Mithril")

	vault := writeVault(t, map[string]string{
		"notes/note.md": "---\nabout: [Rosie Cotton, [[Mithril]]]\n---\n# Note\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	doc, err := s.nav.GetDocumentByPath(s.ctx, "notes/note.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	rels, err := s.nav.EntityRelations(s.ctx, doc.DocumentUID, navigate.RelationFilter{
		Predicates: []string{"describes"}, Limit: -1,
	})
	if err != nil {
		t.Fatalf("EntityRelations: %v", err)
	}
	targets := make(map[string]bool)
	for i := range rels {
		if rels[i].SubjectUID == doc.DocumentUID {
			targets[rels[i].ObjectUID] = true
		}
	}
	if !targets[sarahUID] {
		t.Error("flow-list about: missing describes edge to Rosie Cotton")
	}
	if !targets[acmeUID] {
		t.Error("flow-list about: dropped [[Mithril]] nested-wikilink target (finding 4)")
	}
}

// --- Part 3: Rebuild-replay does not re-extract ---

func TestProseExtraction_ReplayDoesNotReExtract(t *testing.T) {
	t.Parallel()

	ext := &countingExtractor{mentions: []ontology.Mention{}}
	s := stackWithExtractor(t, ext)
	s.seedEntity(t, ontology.EntityKindPerson, "Merry")

	vault := writeVault(t, map[string]string{
		"notes/note.md": "# Note\n\nAlice was here.\n",
	})
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	firstCount := ext.callCount()

	// Re-import with NO content change. The importer's content-hash dedup
	// skips the file entirely (pd.changed == false), so no extraction goroutine
	// is launched. This mirrors what rebuild replay would do.
	if _, err := s.im.ImportDir(s.ctx, kernel.DefaultScope, vault); err != nil {
		t.Fatalf("re-ImportDir: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if got := ext.callCount(); got != firstCount {
		t.Errorf("extract calls after re-import (same content) = %d, want %d (no re-extraction)", got, firstCount)
	}
}
