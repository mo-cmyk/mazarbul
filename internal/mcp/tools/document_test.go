package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/sync/vault"
)

// wantToolErrorCode asserts err is a *mcp.ToolError carrying code.
func wantToolErrorCode(t *testing.T, err error, code mcp.ToolErrorCode) {
	t.Helper()
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("want *mcp.ToolError, got %T (%v)", err, err)
	}
	if te.Code != code {
		t.Fatalf("ToolError code = %q, want %q", te.Code, code)
	}
}

// fakeDocNav is a static docNavigator for the document-tool projection tests.
type fakeDocNav struct {
	byPath     map[string]navigate.Document
	byUID      map[string]navigate.Document
	rels       map[string][]navigate.Relation
	list       []navigate.Document
	unresolved map[string][]string // docUID → write-time unresolved-link targets
}

func (f *fakeDocNav) GetDocumentByPath(_ context.Context, p string) (navigate.Document, error) {
	if d, ok := f.byPath[p]; ok {
		return d, nil
	}
	return navigate.Document{}, navigate.ErrNotFound
}

// GetDocumentByPathScope is scope-exact: it matches on both path and scope, so
// the same relative path in two scopes resolves to two distinct fixtures.
func (f *fakeDocNav) GetDocumentByPathScope(_ context.Context, p, scope string) (navigate.Document, error) {
	if d, ok := f.byPath[p]; ok && d.Scope == scope {
		return d, nil
	}
	return navigate.Document{}, navigate.ErrNotFound
}

func (f *fakeDocNav) GetDocumentByUID(_ context.Context, uid string) (navigate.Document, error) {
	if d, ok := f.byUID[uid]; ok {
		return d, nil
	}
	return navigate.Document{}, navigate.ErrNotFound
}

func (f *fakeDocNav) ListDocuments(_ context.Context, _ navigate.DocumentFilter) ([]navigate.Document, error) {
	return f.list, nil
}

func (f *fakeDocNav) EntityRelations(_ context.Context, uid string, _ navigate.RelationFilter) ([]navigate.Relation, error) {
	return f.rels[uid], nil
}

func (f *fakeDocNav) DocumentUnresolvedLinkTargets(_ context.Context, docUID string) ([]string, error) {
	return f.unresolved[docUID], nil
}

func newFakeDocNav() *fakeDocNav {
	doc := navigate.Document{DocumentUID: "d1", Path: "notes/a.md", Title: "A", Body: "hello"}
	return &fakeDocNav{
		byPath: map[string]navigate.Document{"notes/a.md": doc},
		byUID:  map[string]navigate.Document{"d1": doc},
		list:   []navigate.Document{doc},
		rels: map[string][]navigate.Relation{
			"d1": {
				{UID: "r1", SubjectUID: "d1", Predicate: "mentions", ObjectUID: "p1", ObjectName: "Rosie"},
				{UID: "r2", SubjectUID: "d1", Predicate: "links_to", ObjectUID: "d2", ObjectName: "Other"},
				{UID: "r3", SubjectUID: "d9", Predicate: "links_to", ObjectUID: "d1", SubjectName: "Inbound"},
			},
		},
	}
}

func TestReadDocument_SplitsLinksAndBacklinks(t *testing.T) {
	tool := NewReadDocument(newFakeDocNav())
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"notes/a.md"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out readDocumentOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Document.Body != "hello" || out.Document.Name != "entities/d1" {
		t.Errorf("document projection wrong: %+v", out.Document)
	}
	if len(out.Links) != 2 {
		t.Errorf("links = %+v, want 2 outgoing", out.Links)
	}
	if len(out.Backlinks) != 1 || out.Backlinks[0].UID != "d9" {
		t.Errorf("backlinks = %+v, want 1 inbound from d9", out.Backlinks)
	}
}

// TestReadDocument_SurfacesUnresolvedLinks: a document's write-time
// unresolved-link snapshot is reported as unresolvedLinks, EXCEPT targets a
// resolved edge now satisfies (reconcile emits the edge but does not rewrite the
// snapshot, so the live edges are the tiebreaker).
func TestReadDocument_SurfacesUnresolvedLinks(t *testing.T) {
	nav := newFakeDocNav()
	// "Rosie" matches the resolved mentions edge (ObjectName "Rosie") → suppressed
	// as since-reconciled; "Mythical Module" stays dead; the dup is collapsed.
	nav.unresolved = map[string][]string{
		"d1": {"Mythical Module", "Rosie", "mythical module"},
	}
	// The resolved edge object name must carry through as a docLinkWire DisplayName.
	nav.rels["d1"] = []navigate.Relation{
		{UID: "r1", SubjectUID: "d1", Predicate: "mentions", ObjectUID: "p1", ObjectName: "Rosie"},
	}
	tool := NewReadDocument(nav)
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"notes/a.md"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out readDocumentOutput
	if uErr := json.Unmarshal(raw, &out); uErr != nil {
		t.Fatalf("unmarshal: %v", uErr)
	}
	if len(out.UnresolvedLinks) != 1 || out.UnresolvedLinks[0] != "Mythical Module" {
		t.Errorf("unresolvedLinks = %v, want [Mythical Module] (Rosie suppressed, dup collapsed)", out.UnresolvedLinks)
	}
}

// TestLiveUnresolvedLinks unit-tests the suppression: a snapshot target matching
// a resolved-edge display name is dropped, case-insensitively; duplicates
// collapse; the result is non-nil.
func TestLiveUnresolvedLinks(t *testing.T) {
	t.Parallel()
	resolved := []docLinkWire{{Predicate: "mentions", DisplayName: "Auth Service"}}
	got := liveUnresolvedLinks([]string{"Ghost", "auth service", "Ghost", " "}, resolved)
	if len(got) != 1 || got[0] != "Ghost" {
		t.Errorf("liveUnresolvedLinks = %v, want [Ghost]", got)
	}
	if empty := liveUnresolvedLinks(nil, resolved); empty == nil || len(empty) != 0 {
		t.Errorf("liveUnresolvedLinks(nil) = %v, want non-nil empty slice", empty)
	}
}

func TestReadDocument_NotFound(t *testing.T) {
	tool := NewReadDocument(newFakeDocNav())
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"missing.md"}`))
	wantToolErrorCode(t, err, mcp.CodeNotFound)
}

// TestResolveDocArg_BarePathGetsExtension proves a path argument without the
// .md extension resolves identically to the full path — `maz doc show notes/a`
// must find the document created as notes/a.md (the inconsistency agents hit:
// new/mv/rm accept a bare path but show/links/diff rejected it).
func TestResolveDocArg_BarePathGetsExtension(t *testing.T) {
	nav := newFakeDocNav() // holds notes/a.md
	d, err := resolveDocArg(context.Background(), nav, "notes/a", "")
	if err != nil {
		t.Fatalf("resolveDocArg bare path: %v", err)
	}
	if d.Path != "notes/a.md" {
		t.Errorf("resolved path = %q, want notes/a.md", d.Path)
	}
}

// TestResolveDocArg_AcceptsUIDInPathArg proves the path argument also accepts a
// document's uid — both the bare 26-char ULID and the "entities/<uid>"
// resource name that recall/entities emit — so an agent can pivot from a
// discovery result straight into readDocument without first looking up the path.
func TestResolveDocArg_AcceptsUIDInPathArg(t *testing.T) {
	const uid = "01KTK1DK2AE3JXKRHV9K6HNHQS"
	doc := navigate.Document{DocumentUID: uid, Path: "projects/apollo.md", Title: "Apollo"}
	nav := &fakeDocNav{
		byPath: map[string]navigate.Document{"projects/apollo.md": doc},
		byUID:  map[string]navigate.Document{uid: doc},
	}
	for _, arg := range []string{uid, "entities/" + uid} {
		d, err := resolveDocArg(context.Background(), nav, arg, "")
		if err != nil {
			t.Fatalf("resolveDocArg %q: %v", arg, err)
		}
		if d.DocumentUID != uid {
			t.Errorf("arg %q resolved to uid %q, want %q", arg, d.DocumentUID, uid)
		}
	}
}

func TestReadDocument_RequiresAnArg(t *testing.T) {
	tool := NewReadDocument(newFakeDocNav())
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
	wantToolErrorCode(t, err, mcp.CodeValidation)
}

func TestGetBacklinks_OmitsBody(t *testing.T) {
	tool := NewGetBacklinks(newFakeDocNav())
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{"documentUid":"d1"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// The response must not carry a body field.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, hasDoc := m["document"]; hasDoc {
		t.Error("getBacklinks must not return the document body")
	}
	var out getBacklinksOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}
	if len(out.Links) != 2 || len(out.Backlinks) != 1 {
		t.Errorf("links=%d backlinks=%d, want 2/1", len(out.Links), len(out.Backlinks))
	}
}

// docNavNoEdges returns a navigator with a document that has zero relations, so
// readDocument/getBacklinks must still emit links/backlinks as empty arrays.
func docNavNoEdges() *fakeDocNav {
	doc := navigate.Document{DocumentUID: "lonely", Path: "orphan.md", Title: "Orphan", Body: "alone"}
	return &fakeDocNav{
		byPath: map[string]navigate.Document{"orphan.md": doc},
		byUID:  map[string]navigate.Document{"lonely": doc},
		list:   []navigate.Document{doc},
		rels:   map[string][]navigate.Relation{}, // no edges for "lonely"
	}
}

// TestReadDocument_EmptyLinksEmitAsArrays pins UX-8c: an edge-less document
// serializes "links":[] and "backlinks":[], never omitting the keys or emitting
// null. An agent reads a missing key as "field unsupported" and null as a type
// error; [] unambiguously means "no links".
func TestReadDocument_EmptyLinksEmitAsArrays(t *testing.T) {
	tool := NewReadDocument(docNavNoEdges())
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"orphan.md"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"links":[]`) {
		t.Errorf("readDocument output must contain \"links\":[], got: %s", s)
	}
	if !strings.Contains(s, `"backlinks":[]`) {
		t.Errorf("readDocument output must contain \"backlinks\":[], got: %s", s)
	}
}

// TestReadDocument_SurfacesObservations proves the `- [category] text` lines in
// a body are parsed and returned as structured observations, so an agent reading
// a note sees the projected facts (and knows to query them on the document
// entity) instead of guessing they vanished. Multiple same-category lines all
// surface — the read mirror of the no-collision fact projection.
func TestReadDocument_SurfacesObservations(t *testing.T) {
	body := "# Rosie\n\n" +
		"- [fact] Rosie prefers async written updates #comms\n" +
		"- [fact] Rosie works in the EU timezone\n" +
		"- [decision] Adopt the new review flow #process (Q3)\n"
	doc := navigate.Document{DocumentUID: "d1", Path: "notes/sarah.md", Title: "Rosie", Body: body}
	nav := &fakeDocNav{
		byPath: map[string]navigate.Document{"notes/sarah.md": doc},
		byUID:  map[string]navigate.Document{"d1": doc},
	}
	tool := NewReadDocument(nav)
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"notes/sarah.md"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out readDocumentOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Observations) != 3 {
		t.Fatalf("observations = %+v, want 3 (both [fact] lines + the [decision])", out.Observations)
	}
	var facts, decisions int
	for _, ob := range out.Observations {
		switch ob.Category {
		case "fact":
			facts++
		case "decision":
			decisions++
			if ob.Context != "Q3" {
				t.Errorf("decision context = %q, want Q3", ob.Context)
			}
		}
	}
	if facts != 2 || decisions != 1 {
		t.Errorf("category split = %d fact / %d decision, want 2/1", facts, decisions)
	}
}

// TestReadDocument_SurfacesObservationsDespiteParseError proves the partial-on-
// error contract: a body whose frontmatter is malformed (so document.Parse
// returns an error) but which still carries a valid observation line MUST surface
// that observation, because the importer keeps the same partial result and DOES
// project the fact. Dropping it here would tell an agent "no observations" while
// `fact get <documentUid>` shows the fact — the exact mismatch this read surface
// is meant to prevent.
func TestReadDocument_SurfacesObservationsDespiteParseError(t *testing.T) {
	// `tags: [unterminated` is an unterminated YAML flow sequence -> frontmatter
	// decode fails, but the body (and its observation) parses fine.
	body := "---\ntags: [unterminated\n---\n# Broken\n\n- [fact] survives a frontmatter parse error #resilience\n"
	if _, err := document.Parse([]byte(body)); err == nil {
		t.Fatal("precondition: expected document.Parse to error on malformed frontmatter, got nil")
	}
	doc := navigate.Document{DocumentUID: "d-broken", Path: "notes/broken.md", Title: "Broken", Body: body}
	nav := &fakeDocNav{
		byPath: map[string]navigate.Document{"notes/broken.md": doc},
		byUID:  map[string]navigate.Document{"d-broken": doc},
	}
	raw, err := NewReadDocument(nav).Invoke(context.Background(), json.RawMessage(`{"path":"notes/broken.md"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out readDocumentOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Observations) != 1 || out.Observations[0].Category != "fact" {
		t.Fatalf("observations = %+v, want exactly one [fact] despite the parse error", out.Observations)
	}
}

// TestReadDocument_EmptyObservationsEmitAsArray pins the [] (never null) contract
// for a body with no observation lines, so an agent can iterate unconditionally.
func TestReadDocument_EmptyObservationsEmitAsArray(t *testing.T) {
	tool := NewReadDocument(docNavNoEdges()) // body "alone": no observations
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{"path":"orphan.md"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(string(raw), `"observations":[]`) {
		t.Errorf("readDocument output must contain \"observations\":[], got: %s", raw)
	}
}

// TestGetBacklinks_EmptyLinksEmitAsArrays is the getBacklinks counterpart.
func TestGetBacklinks_EmptyLinksEmitAsArrays(t *testing.T) {
	tool := NewGetBacklinks(docNavNoEdges())
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{"documentUid":"lonely"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"links":[]`) || !strings.Contains(s, `"backlinks":[]`) {
		t.Errorf("getBacklinks must emit empty arrays for links and backlinks, got: %s", s)
	}
}

func TestListDocuments_Projects(t *testing.T) {
	tool := NewListDocuments(newFakeDocNav())
	raw, err := tool.Invoke(context.Background(), json.RawMessage(`{"pinned":true}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out listDocumentsOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Documents) != 1 || out.Documents[0].Name != "entities/d1" {
		t.Errorf("documents = %+v", out.Documents)
	}
}

func TestListDocuments_NegativeLimitRejected(t *testing.T) {
	// A negative limit means "no cap" to the navigator; the tool contract is
	// 1..200, so a direct call with limit:-1 must be a validation error rather
	// than a whole-vault dump.
	tool := NewListDocuments(newFakeDocNav())
	_, err := tool.Invoke(context.Background(), json.RawMessage(`{"limit":-1}`))
	wantToolErrorCode(t, err, mcp.CodeValidation)
}

func TestImportDocuments_IsUnlisted(t *testing.T) {
	var tool mcp.Tool = NewImportDocuments(nil, "", nil)
	u, ok := tool.(mcp.UnlistedTool)
	if !ok || !u.Unlisted() {
		t.Error("importDocuments must be unlisted")
	}
}

// fakeImporter records which import surface importDocuments routed to. When
// dirResult is set it is returned from ImportDir (lets a test drive the
// post-import materialization with specific ImportedDoc entries).
type fakeImporter struct {
	dirRoot      string
	dirScope     kernel.Scope
	contentPath  string
	contentScope kernel.Scope
	contentBody  string
	dirResult    document.Result
}

func (f *fakeImporter) ImportDir(_ context.Context, scope kernel.Scope, root string) (document.Result, error) {
	f.dirScope = scope
	f.dirRoot = root
	return f.dirResult, nil
}

func (f *fakeImporter) ImportContent(_ context.Context, scope kernel.Scope, path string, raw []byte) (document.Result, error) {
	f.contentScope = scope
	f.contentPath = path
	f.contentBody = string(raw)
	return document.Result{Imported: 1}, nil
}

// recordingProjector records every SyncPath call so a test can assert which
// imported documents were re-materialized into the vault.
type recordingProjector struct {
	synced []string
}

func (r *recordingProjector) SyncAll(context.Context) (vault.Stats, error) {
	return vault.Stats{}, nil
}

func (r *recordingProjector) SyncPath(_ context.Context, rel, _ string) (vault.Stats, error) {
	r.synced = append(r.synced, rel)
	return vault.Stats{Written: 1}, nil
}

func (r *recordingProjector) FilePath(rel, scope string) (string, error) {
	return "/vault/" + scope + "/" + rel, nil
}

func (r *recordingProjector) Root() string { return "/vault" }

// TestImportDocuments_MaterializesChangedRevisions is the regression for the
// "importDocuments must re-materialize into the vault" gap: a directory import
// from OUTSIDE the vault appends revisions that the watcher never sees, so the
// tool itself must project each CHANGED document onto disk. Unchanged documents
// are skipped (their on-disk hash already matches; SyncPath would be a no-op).
func TestImportDocuments_MaterializesChangedRevisions(t *testing.T) {
	fi := &fakeImporter{dirResult: document.Result{
		Imported: 2,
		Skipped:  1,
		Documents: []document.ImportedDoc{
			{Path: "notes/alpha.md", Changed: true},
			{Path: "notes/beta.md", Changed: true},
			{Path: "notes/gamma.md", Changed: false}, // unchanged: must NOT be synced
		},
	}}
	proj := &recordingProjector{}
	tool := NewImportDocuments(fi, "", proj)

	outside := filepath.Join(t.TempDir(), "external")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	req, _ := json.Marshal(map[string]string{"path": outside})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	want := []string{"notes/alpha.md", "notes/beta.md"}
	if len(proj.synced) != len(want) {
		t.Fatalf("synced paths = %v, want %v", proj.synced, want)
	}
	for i, p := range want {
		if proj.synced[i] != p {
			t.Errorf("synced[%d] = %q, want %q", i, proj.synced[i], p)
		}
	}
}

// TestImportDocuments_NilProjectorSkipsMaterialize verifies materialization is
// a safe no-op when no vault is wired (the revision stays in the event log).
func TestImportDocuments_NilProjectorSkipsMaterialize(t *testing.T) {
	fi := &fakeImporter{dirResult: document.Result{
		Documents: []document.ImportedDoc{{Path: "notes/alpha.md", Changed: true}},
	}}
	tool := NewImportDocuments(fi, "", nil)
	req, _ := json.Marshal(map[string]string{"path": t.TempDir()})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke with nil projector: %v", err)
	}
}

// TestImportDocuments_VaultFileKeepsRelativePath pins the documented
// round-trip `vim "$(maz doc path notes/foo)" && maz doc import "$(maz doc
// path notes/foo)"`: a single FILE inside the configured vault is imported by
// its vault-relative path (notes/foo.md), not rebased on its parent directory
// (which would create a basename-only sibling document foo.md). Directories —
// including the vault root itself — and paths outside the vault keep the
// ImportDir behaviour.
func TestImportDocuments_VaultFileKeepsRelativePath(t *testing.T) {
	vaultRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vaultRoot, "notes"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inside := filepath.Join(vaultRoot, "notes", "foo.md")
	if err := os.WriteFile(inside, []byte("# Foo\n\nedited body\n"), 0o644); err != nil {
		t.Fatalf("write vault file: %v", err)
	}

	// File inside the vault → ImportContent with the vault-relative path.
	fi := &fakeImporter{}
	tool := NewImportDocuments(fi, vaultRoot, nil)
	req, _ := json.Marshal(map[string]string{"path": inside})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke inside-vault file: %v", err)
	}
	if fi.contentPath != "notes/foo.md" {
		t.Fatalf("ImportContent path = %q, want %q", fi.contentPath, "notes/foo.md")
	}
	if fi.contentBody != "# Foo\n\nedited body\n" {
		t.Fatalf("ImportContent body = %q, want the on-disk bytes", fi.contentBody)
	}
	if fi.dirRoot != "" {
		t.Fatalf("ImportDir also called (root %q), want ImportContent only", fi.dirRoot)
	}

	// The vault root itself (a directory) → ImportDir unchanged.
	fi = &fakeImporter{}
	tool = NewImportDocuments(fi, vaultRoot, nil)
	req, _ = json.Marshal(map[string]string{"path": vaultRoot})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke vault root: %v", err)
	}
	if fi.dirRoot == "" || fi.contentPath != "" {
		t.Fatalf("vault-root import: dirRoot=%q contentPath=%q, want ImportDir only", fi.dirRoot, fi.contentPath)
	}

	// A file outside the vault → ImportDir (single-file mode) unchanged.
	outside := filepath.Join(t.TempDir(), "loose.md")
	if err := os.WriteFile(outside, []byte("# Loose\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	fi = &fakeImporter{}
	tool = NewImportDocuments(fi, vaultRoot, nil)
	req, _ = json.Marshal(map[string]string{"path": outside})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke outside file: %v", err)
	}
	if fi.dirRoot != outside || fi.contentPath != "" {
		t.Fatalf("outside import: dirRoot=%q contentPath=%q, want ImportDir(%q)", fi.dirRoot, fi.contentPath, outside)
	}

	// No vault configured → mapping disabled, ImportDir for everything.
	fi = &fakeImporter{}
	tool = NewImportDocuments(fi, "", nil)
	req, _ = json.Marshal(map[string]string{"path": inside})
	if _, err := tool.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke without vault root: %v", err)
	}
	if fi.dirRoot != inside || fi.contentPath != "" {
		t.Fatalf("no-vault import: dirRoot=%q contentPath=%q, want ImportDir(%q)", fi.dirRoot, fi.contentPath, inside)
	}
}
