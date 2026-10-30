package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/sync/vault"
)

// fakeWriter records ImportContent calls and returns a canned single-doc result.
type fakeWriter struct {
	gotScope kernel.Scope
	gotPath  string
	gotBody  string
	err      error
	// unresolved, when set, is reported back as the dangling wikilink targets so
	// tests can assert writeDocument surfaces them.
	unresolved []string
}

func (f *fakeWriter) ImportContent(_ context.Context, scope kernel.Scope, path string, raw []byte) (document.Result, error) {
	f.gotScope = scope
	f.gotPath = path
	f.gotBody = string(raw)
	if f.err != nil {
		return document.Result{}, f.err
	}
	return document.Result{
		Imported:          1,
		Edges:             2,
		Unresolved:        len(f.unresolved),
		UnresolvedTargets: f.unresolved,
		Documents: []document.ImportedDoc{
			{Path: path, DocumentUID: "d1", RevisionUID: "r1", Changed: true},
		},
	}, nil
}

// fakeProjector records sync calls and returns canned stats.
type fakeProjector struct {
	syncedPath  string
	syncedScope string
	syncedAll   bool
	stats       vault.Stats
	err         error
}

func (f *fakeProjector) SyncAll(context.Context) (vault.Stats, error) {
	f.syncedAll = true
	return f.stats, f.err
}

func (f *fakeProjector) SyncPath(_ context.Context, rel, scope string) (vault.Stats, error) {
	f.syncedPath = rel
	f.syncedScope = scope
	return f.stats, f.err
}

func (f *fakeProjector) FilePath(rel, scope string) (string, error) {
	return "/vault/" + scope + "/" + rel, nil
}

func (f *fakeProjector) Root() string { return "/vault" }

func decodeWriteOutput(t *testing.T, raw json.RawMessage) documentWriteOutput {
	t.Helper()
	var out documentWriteOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	return out
}

func TestWriteDocument_VerbatimBodyAndMaterialization(t *testing.T) {
	w := &fakeWriter{}
	p := &fakeProjector{stats: vault.Stats{Written: 1}}
	raw, err := NewWriteDocument(w, p).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/a.md","body":"# A\n\nhello\n"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if w.gotBody != "# A\n\nhello\n" {
		t.Fatalf("body = %q, want verbatim", w.gotBody)
	}
	if p.syncedPath != "notes/a.md" {
		t.Fatalf("synced path = %q", p.syncedPath)
	}
	// No scope given → defaults to personal, threaded to the importer, the vault
	// SyncPath, and the reported (partitioned) file path alike.
	if w.gotScope != kernel.DefaultScope {
		t.Fatalf("import scope = %q, want default (personal)", w.gotScope)
	}
	if p.syncedScope != string(kernel.DefaultScope) {
		t.Fatalf("synced scope = %q, want personal", p.syncedScope)
	}
	out := decodeWriteOutput(t, raw)
	if out.Name != "entities/d1" || out.RevisionUID != "r1" || !out.Changed || out.Edges != 2 {
		t.Fatalf("output = %+v", out)
	}
	if out.File != "/vault/personal/notes/a.md" {
		t.Fatalf("file = %q, want /vault/personal/notes/a.md", out.File)
	}
	if out.PendingEdit {
		t.Fatal("pendingEdit should be false")
	}
}

// TestWriteDocument_SurfacesUnresolvedTargets pins UX-8d: a write with a
// dangling wikilink reports both the count AND the target names, so a caller
// sees WHICH links failed to wire (typo vs not-yet-created) instead of silently
// succeeding.
func TestWriteDocument_SurfacesUnresolvedTargets(t *testing.T) {
	w := &fakeWriter{unresolved: []string{"Nonexistent Topic", "Ghost Person"}}
	p := &fakeProjector{stats: vault.Stats{Written: 1}}
	raw, err := NewWriteDocument(w, p).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/a.md","body":"# A\n\n[[Nonexistent Topic]] and [[Ghost Person]]\n"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decodeWriteOutput(t, raw)
	if out.Unresolved != 2 {
		t.Errorf("unresolved = %d, want 2", out.Unresolved)
	}
	if len(out.UnresolvedTargets) != 2 ||
		out.UnresolvedTargets[0] != "Nonexistent Topic" ||
		out.UnresolvedTargets[1] != "Ghost Person" {
		t.Errorf("unresolvedTargets = %+v, want [Nonexistent Topic, Ghost Person]", out.UnresolvedTargets)
	}
}

// TestWriteDocument_NoUnresolvedTargetsOmitted confirms the field is omitted
// (not null/[]) when every link resolves, keeping the happy-path output lean.
func TestWriteDocument_NoUnresolvedTargetsOmitted(t *testing.T) {
	w := &fakeWriter{}
	p := &fakeProjector{stats: vault.Stats{Written: 1}}
	raw, err := NewWriteDocument(w, p).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/a.md","body":"# A\n\nhello\n"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if strings.Contains(string(raw), "unresolvedTargets") {
		t.Errorf("unresolvedTargets must be omitted when none dangle, got: %s", raw)
	}
}

func TestWriteDocument_SynthesizesFrontmatter(t *testing.T) {
	w := &fakeWriter{}
	p := &fakeProjector{}
	_, err := NewWriteDocument(w, p).Invoke(context.Background(),
		json.RawMessage(`{"path":"a.md","body":"hello\n","title":"T","pinned":true,"tags":["x","y"],"frontmatter":{"author":"me"}}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// Note: yaml.v3 quotes "y" because bare y is a YAML 1.1 boolean literal.
	want := "---\nauthor: me\npinned: true\ntags:\n    - x\n    - \"y\"\ntitle: T\n---\nhello\n"
	if w.gotBody != want {
		t.Fatalf("composed body =\n%q\nwant\n%q", w.gotBody, want)
	}
}

func TestWriteDocument_MetadataConflictsWithBodyFrontmatter(t *testing.T) {
	_, err := NewWriteDocument(&fakeWriter{}, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"a.md","body":"---\ntitle: own\n---\nbody\n","title":"T"}`))
	wantToolErrorCode(t, err, mcp.CodeValidation)
}

func TestWriteDocument_BodyWithOwnFrontmatterAndNoMetadataIsVerbatim(t *testing.T) {
	w := &fakeWriter{}
	body := "---\ntitle: own\n---\nbody\n"
	_, err := NewWriteDocument(w, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"a.md","body":"---\ntitle: own\n---\nbody\n"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if w.gotBody != body {
		t.Fatalf("body = %q, want untouched", w.gotBody)
	}
}

func TestWriteDocument_EscapingPathRejected(t *testing.T) {
	_, err := NewWriteDocument(&fakeWriter{}, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"../evil.md","body":"x"}`))
	wantToolErrorCode(t, err, mcp.CodeValidation)
}

func TestWriteDocument_MaterializationFailureDoesNotFailWrite(t *testing.T) {
	p := &fakeProjector{err: errors.New("disk full")}
	raw, err := NewWriteDocument(&fakeWriter{}, p).Invoke(context.Background(),
		json.RawMessage(`{"path":"a.md","body":"x"}`))
	if err != nil {
		t.Fatalf("Invoke must succeed when only materialization fails: %v", err)
	}
	out := decodeWriteOutput(t, raw)
	if out.File != "" {
		t.Fatalf("file = %q, want empty when materialization failed", out.File)
	}
	if out.RevisionUID != "r1" {
		t.Fatalf("revision = %q — the event write must still be reported", out.RevisionUID)
	}
}

func TestWriteDocument_PendingEditSurfaced(t *testing.T) {
	p := &fakeProjector{stats: vault.Stats{PendingEdits: 1}}
	raw, err := NewWriteDocument(&fakeWriter{}, p).Invoke(context.Background(),
		json.RawMessage(`{"path":"a.md","body":"x"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out := decodeWriteOutput(t, raw); !out.PendingEdit {
		t.Fatal("pendingEdit not surfaced")
	}
}

// TestWriteDocument_ScopeThreadedToImporterAndVault pins that an explicit scope
// reaches BOTH the importer (so the document + companion entity rows land in
// that scope) AND the vault projection (so the file materializes under the
// correct <root>/<scope>/ partition), never a mix.
func TestWriteDocument_ScopeThreadedToImporterAndVault(t *testing.T) {
	w := &fakeWriter{}
	p := &fakeProjector{stats: vault.Stats{Written: 1}}
	raw, err := NewWriteDocument(w, p).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/a.md","body":"# A\n\nhi\n","scope":"team"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if w.gotScope != kernel.ScopeTeam {
		t.Errorf("import scope = %q, want team", w.gotScope)
	}
	if p.syncedScope != "team" {
		t.Errorf("synced scope = %q, want team", p.syncedScope)
	}
	if out := decodeWriteOutput(t, raw); out.File != "/vault/team/notes/a.md" {
		t.Errorf("file = %q, want /vault/team/notes/a.md (team partition)", out.File)
	}
}

// TestWriteDocument_InvalidScopeRejected pins that an unknown scope is a caller
// input error (E_VALIDATION), not a silent fallback to personal.
func TestWriteDocument_InvalidScopeRejected(t *testing.T) {
	_, err := NewWriteDocument(&fakeWriter{}, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"a.md","body":"x","scope":"galaxy"}`))
	wantToolErrorCode(t, err, mcp.CodeValidation)
}

// TestWriteDocument_NonMemberTeamWriteMapsToPermission is the regression guard
// for the error-mapping bug: a team-scope write rejected at append with
// kernel.ErrPermission must surface as E_PERMISSION, not E_INTERNAL (the prior
// blanket NewInternalError wrapping).
func TestWriteDocument_NonMemberTeamWriteMapsToPermission(t *testing.T) {
	w := &fakeWriter{err: fmt.Errorf("team authority: %w", kernel.ErrPermission)}
	_, err := NewWriteDocument(w, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"a.md","body":"x","scope":"team"}`))
	wantToolErrorCode(t, err, mcp.CodePermission)
}

// TestWriteDocument_SchemaRejectionMapsToValidation pins that an ErrInvalidEvent
// (a malformed event the ontology rejects) is the caller's mistake → E_VALIDATION,
// matching create_entity's mapping.
func TestWriteDocument_SchemaRejectionMapsToValidation(t *testing.T) {
	w := &fakeWriter{err: fmt.Errorf("bad payload: %w", kernel.ErrInvalidEvent)}
	_, err := NewWriteDocument(w, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"a.md","body":"x"}`))
	wantToolErrorCode(t, err, mcp.CodeValidation)
}

// TestEditDocument_PreservesScope pins that an edit never changes a document's
// scope: the resolved document's own scope is threaded to the importer and the
// vault, whether or not the caller passed a disambiguating scope argument.
func TestEditDocument_PreservesScope(t *testing.T) {
	// A team-scoped fixture at notes/t.md.
	teamDoc := navigate.Document{DocumentUID: "dt", Path: "notes/t.md", Title: "T", Body: "hello", Scope: "team"}
	nav := &fakeDocNav{
		byPath: map[string]navigate.Document{"notes/t.md": teamDoc},
		byUID:  map[string]navigate.Document{"dt": teamDoc},
	}

	// Without a scope argument: resolves by path, reuses the doc's team scope.
	w := &fakeWriter{}
	p := &fakeProjector{}
	if _, err := NewEditDocument(nav, w, p).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/t.md","oldString":"hello","newString":"bye"}`)); err != nil {
		t.Fatalf("Invoke (no scope): %v", err)
	}
	if w.gotScope != kernel.ScopeTeam {
		t.Errorf("edit import scope = %q, want team (preserved)", w.gotScope)
	}
	if p.syncedScope != "team" {
		t.Errorf("edit synced scope = %q, want team", p.syncedScope)
	}

	// With an explicit disambiguating scope: resolves the exact row, still team.
	w2 := &fakeWriter{}
	if _, err := NewEditDocument(nav, w2, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/t.md","scope":"team","oldString":"hello","newString":"ciao"}`)); err != nil {
		t.Fatalf("Invoke (scope=team): %v", err)
	}
	if w2.gotScope != kernel.ScopeTeam {
		t.Errorf("scope-disambiguated edit scope = %q, want team", w2.gotScope)
	}
}

func TestEditDocument_ReplacesAndIngests(t *testing.T) {
	nav := newFakeDocNav() // notes/a.md body = "hello"
	w := &fakeWriter{}
	raw, err := NewEditDocument(nav, w, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/a.md","oldString":"hello","newString":"goodbye"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if w.gotBody != "goodbye" {
		t.Fatalf("ingested body = %q", w.gotBody)
	}
	if out := decodeWriteOutput(t, raw); out.RevisionUID != "r1" {
		t.Fatalf("output = %+v", out)
	}
}

func TestEditDocument_OldStringNotFound(t *testing.T) {
	_, err := NewEditDocument(newFakeDocNav(), &fakeWriter{}, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/a.md","oldString":"absent","newString":"x"}`))
	wantToolErrorCode(t, err, mcp.CodeValidation)
}

func TestEditDocument_AmbiguousWithoutReplaceAll(t *testing.T) {
	nav := newFakeDocNav()
	d := nav.byPath["notes/a.md"]
	d.Body = "dup one dup" // two occurrences, non-adjacent
	nav.byPath["notes/a.md"] = d

	_, err := NewEditDocument(nav, &fakeWriter{}, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/a.md","oldString":"dup","newString":"x"}`))
	wantToolErrorCode(t, err, mcp.CodeValidation)

	w := &fakeWriter{}
	if _, err := NewEditDocument(nav, w, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/a.md","oldString":"dup","newString":"x","replaceAll":true}`)); err != nil {
		t.Fatalf("replaceAll Invoke: %v", err)
	}
	if w.gotBody != "x one x" {
		t.Fatalf("replaceAll body = %q", w.gotBody)
	}
}

func TestEditDocument_EqualStringsRejected(t *testing.T) {
	_, err := NewEditDocument(newFakeDocNav(), &fakeWriter{}, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/a.md","oldString":"hello","newString":"hello"}`))
	wantToolErrorCode(t, err, mcp.CodeValidation)
}

func TestEditDocument_UnknownPathIsNotFound(t *testing.T) {
	_, err := NewEditDocument(newFakeDocNav(), &fakeWriter{}, &fakeProjector{}).Invoke(context.Background(),
		json.RawMessage(`{"path":"missing.md","oldString":"a","newString":"b"}`))
	wantToolErrorCode(t, err, mcp.CodeNotFound)
}

func TestMaterializeVault_AllAndSinglePath(t *testing.T) {
	p := &fakeProjector{stats: vault.Stats{Written: 3, Deleted: 1}}
	mv := NewMaterializeVault(p)

	raw, err := mv.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke all: %v", err)
	}
	if !p.syncedAll {
		t.Fatal("SyncAll not called")
	}
	var out materializeVaultOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Root != "/vault" || out.Written != 3 || out.Deleted != 1 {
		t.Fatalf("output = %+v", out)
	}
	// A whole-vault sync names no single file.
	if out.File != "" {
		t.Errorf("whole-vault file = %q, want empty", out.File)
	}

	raw, err = mv.Invoke(context.Background(), json.RawMessage(`{"path":"a.md"}`))
	if err != nil {
		t.Fatalf("Invoke path: %v", err)
	}
	if p.syncedPath != "a.md" {
		t.Fatalf("synced path = %q", p.syncedPath)
	}
	// A single-path sync surfaces the absolute on-disk path (from the projector's
	// vault root) so `maz doc path/edit/open` need not recompute it from the
	// client's default data dir — the data-dir split-brain fix.
	var single materializeVaultOutput
	if err := json.Unmarshal(raw, &single); err != nil {
		t.Fatalf("decode single: %v", err)
	}
	// No scope given → defaults to personal, so the file lands in that partition.
	if single.File != "/vault/personal/a.md" {
		t.Errorf("single-path file = %q, want /vault/personal/a.md", single.File)
	}
}

// TestMaterializeVault_NormalizesPath pins that a single-path sync runs the
// caller's path through CleanVaultPath exactly like the write/edit tools, so
// `maz doc sync notes/foo` finds the document created as notes/foo.md instead
// of failing not-found on the extensionless form. Escaping paths are rejected
// as validation errors rather than reaching the projector.
func TestMaterializeVault_NormalizesPath(t *testing.T) {
	p := &fakeProjector{}
	mv := NewMaterializeVault(p)

	if _, err := mv.Invoke(context.Background(), json.RawMessage(`{"path":"notes/foo"}`)); err != nil {
		t.Fatalf("Invoke extensionless path: %v", err)
	}
	if p.syncedPath != "notes/foo.md" {
		t.Fatalf("synced path = %q, want %q (CleanVaultPath must default the .md extension)", p.syncedPath, "notes/foo.md")
	}

	_, err := mv.Invoke(context.Background(), json.RawMessage(`{"path":"../escape"}`))
	wantToolErrorCode(t, err, mcp.CodeValidation)
}

// TestMaterializeVault_MissingDocumentReportsFile pins that a not-yet-existing
// document is NOT an error: materialization is a no-op (zero stats) but the
// well-defined vault location is still reported, so `maz doc new/path/edit/open`
// stay rooted at THIS server's vault instead of falling back to a phantom path
// under the client's default data dir.
func TestMaterializeVault_MissingDocumentReportsFile(t *testing.T) {
	p := &fakeProjector{err: fmt.Errorf("vault: sync %q: %w", "ghost.md", navigate.ErrNotFound)}
	mv := NewMaterializeVault(p)

	raw, err := mv.Invoke(context.Background(), json.RawMessage(`{"path":"ghost.md"}`))
	if err != nil {
		t.Fatalf("Invoke missing document: %v, want nil (no-op, not an error)", err)
	}
	var out materializeVaultOutput
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	// The vault is partitioned as <root>/<scope>/<path>; with no scope given,
	// MaterializeVault defaults to personal.
	if out.File != "/vault/personal/ghost.md" {
		t.Errorf("missing-doc file = %q, want /vault/personal/ghost.md", out.File)
	}
	if out.Written != 0 || out.Unchanged != 0 || out.Deleted != 0 {
		t.Errorf("missing-doc stats = %+v, want all zero (no-op)", out.Stats)
	}
}

// TestMaterializeVault_MustExistRejectsMissingDocument pins the doc-sync
// contract: when mustExist is set (as `maz doc sync <path>` does), a path that
// names no document is E_NOT_FOUND, not a no-op success — so a typo or a stale
// reference surfaces instead of a misleading "synced 0 files" line.
func TestMaterializeVault_MustExistRejectsMissingDocument(t *testing.T) {
	p := &fakeProjector{err: fmt.Errorf("vault: sync %q: %w", "ghost.md", navigate.ErrNotFound)}
	mv := NewMaterializeVault(p)

	_, err := mv.Invoke(context.Background(), json.RawMessage(`{"path":"ghost.md","mustExist":true}`))
	wantToolErrorCode(t, err, mcp.CodeNotFound)
}

// TestMaterializeVault_RealErrorStaysFatal pins that a genuine projector failure
// (anything other than not-found) is surfaced as a tool error rather than being
// swallowed into a phantom-path success.
func TestMaterializeVault_RealErrorStaysFatal(t *testing.T) {
	p := &fakeProjector{err: errors.New("disk on fire")}
	mv := NewMaterializeVault(p)

	_, err := mv.Invoke(context.Background(), json.RawMessage(`{"path":"notes/a.md"}`))
	wantToolErrorCode(t, err, mcp.CodeInternal)
}

func TestMaterializeVault_IsUnlisted(t *testing.T) {
	if !(&MaterializeVault{}).Unlisted() {
		t.Fatal("materializeVault must stay off the agent tool list")
	}
}

func TestWriteDocument_DescriptionsMentionKeyArgs(t *testing.T) {
	// Cheap guardrails: the agent-facing contract names the arguments.
	if d := (&WriteDocument{}).Description(); !strings.Contains(d, "path") || !strings.Contains(d, "body") {
		t.Fatalf("writeDocument description lacks arg names: %q", d)
	}
	if d := (&EditDocument{}).Description(); !strings.Contains(d, "oldString") {
		t.Fatalf("editDocument description lacks oldString: %q", d)
	}
}

// fakeCollisionChecker implements writeCollisionChecker for tests.
type fakeCollisionChecker struct {
	// entities is the set returned for any ListEntities call keyed by exact
	// display name (case-insensitive). An absent key returns nil, no error.
	byName map[string][]navigate.Entity
}

func (f *fakeCollisionChecker) ListEntities(_ context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error) {
	if f.byName == nil {
		return nil, nil
	}
	return f.byName[strings.ToLower(opts.NameExact)], nil
}

// TestWriteDocumentOutputSchema_WholeDocLabel pins the agent-facing contract
// that the output schema and description use unambiguous whole-document
// snapshot language for the edge/unresolved counts and not "emitted" or "delta"
// language that implies a comparison against the previous revision.
func TestWriteDocumentOutputSchema_WholeDocLabel(t *testing.T) {
	schema := string((&WriteDocument{}).OutputSchema())
	for _, want := range []string{
		"snapshot",
		"whole document",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("writeDocument output schema does not mention %q — update the edges/unresolved descriptions to clarify they are whole-document snapshots", want)
		}
	}
	desc := (&WriteDocument{}).Description()
	if !strings.Contains(desc, "snapshot") && !strings.Contains(desc, "whole-document") {
		t.Errorf("writeDocument description does not mention snapshot/whole-document framing: %q", desc)
	}
	// editDocument shares the same output schema.
	editSchema := string((&EditDocument{}).OutputSchema())
	if !strings.Contains(editSchema, "snapshot") {
		t.Errorf("editDocument output schema does not contain snapshot framing")
	}
}

// TestWriteDocument_CollisionWarning verifies that when a document title
// shadows a non-document entity the result carries a non-fatal warnings entry
// and the body byte-content reaching the writer is unchanged (content-hash
// idempotency preserved).
func TestWriteDocument_CollisionWarning(t *testing.T) {
	const body = "---\ntitle: Mithrilmail\n---\n\nProduct notes.\n"
	cc := &fakeCollisionChecker{
		byName: map[string][]navigate.Entity{
			"mithrilmail": {
				{UID: "e-product", Kind: "product", DisplayName: "Mithrilmail"},
			},
		},
	}
	w := &fakeWriter{}
	p := &fakeProjector{stats: vault.Stats{Written: 1}}
	tool := NewWriteDocument(w, p).WithCollisionChecker(cc)
	raw, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/mithrilmail.md","body":"---\ntitle: Mithrilmail\n---\n\nProduct notes.\n"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decodeWriteOutput(t, raw)
	// Non-fatal: the write succeeded.
	if !out.Changed {
		t.Error("changed must be true")
	}
	// Warning must be present and name the colliding kind.
	if len(out.Warnings) == 0 {
		t.Fatal("expected at least one warning for title collision, got none")
	}
	if !strings.Contains(out.Warnings[0], "product") {
		t.Errorf("collision warning should name the shadowed kind 'product', got: %q", out.Warnings[0])
	}
	// Body reaching the writer must be byte-identical to the input body —
	// the warning must never mutate what gets stored.
	if w.gotBody != body {
		t.Errorf("body sent to writer was mutated by the collision check\ngot:  %q\nwant: %q", w.gotBody, body)
	}
}

// TestWriteDocument_NoCollisionWarningWhenNoShadow verifies no warnings are
// emitted when the document title only matches other document entities (no
// non-document entity shares the name).
func TestWriteDocument_NoCollisionWarningWhenNoShadow(t *testing.T) {
	cc := &fakeCollisionChecker{
		byName: map[string][]navigate.Entity{
			"guide": {
				{UID: "d-other", Kind: "document", DisplayName: "Guide"},
			},
		},
	}
	w := &fakeWriter{}
	p := &fakeProjector{stats: vault.Stats{Written: 1}}
	tool := NewWriteDocument(w, p).WithCollisionChecker(cc)
	raw, err := tool.Invoke(context.Background(),
		json.RawMessage(`{"path":"notes/guide.md","body":"---\ntitle: Guide\n---\n\nContent.\n"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decodeWriteOutput(t, raw)
	if len(out.Warnings) != 0 {
		t.Errorf("expected no warnings when only document entities share the name, got: %v", out.Warnings)
	}
}
