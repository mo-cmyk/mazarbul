package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/sync/vault"
)

// documentWriter is the ingest surface the write tools consume. Satisfied by
// *document.Importer. Scope selects the storage partition (project / personal /
// team / draft); it is fixed at creation because dedup is scope-exact.
type documentWriter interface {
	ImportContent(ctx context.Context, scope kernel.Scope, path string, raw []byte) (document.Result, error)
}

// vaultProjector materializes documents to the editable vault directory, which
// is partitioned on disk as <root>/<scope>/<path>. Satisfied by
// *vault.Projector.
type vaultProjector interface {
	SyncAll(ctx context.Context) (vault.Stats, error)
	SyncPath(ctx context.Context, rel, scope string) (vault.Stats, error)
	FilePath(rel, scope string) (string, error)
	Root() string
}

// writeCollisionChecker is the optional read surface used by writeDocument and
// editDocument to detect title shadowing after a write. Satisfied by
// *navigate.Navigator; a nil checker disables the collision check. The check
// is non-fatal: it only appends a warning to the result and never mutates the
// written body, preserving byte-stable round-trip and content-hash idempotency.
type writeCollisionChecker interface {
	ListEntities(ctx context.Context, opts navigate.EntityFilter) ([]navigate.Entity, error)
}

// documentWriteOutput is the shared response of writeDocument / editDocument:
// the import result for the one document plus where it materialized on disk.
// All counts reflect the whole document AFTER this write — they are snapshots,
// not deltas versus the previous revision.
type documentWriteOutput struct {
	// Name is the entities/{uid} resource name (a document IS an entity).
	Name        string `json:"name"`
	Path        string `json:"path"`
	DocumentUID string `json:"documentUid"`
	RevisionUID string `json:"revisionUid,omitempty"`
	// Changed is false when the content hash matched the latest revision and
	// the write was a no-op.
	Changed    bool `json:"changed"`
	Edges      int  `json:"edges"`
	Unresolved int  `json:"unresolved"`
	// UnresolvedTargets names the dangling wikilink targets (the text inside
	// [[…]] that matched nothing), so a caller sees WHICH links failed, not just
	// how many. Omitted when none dangle.
	UnresolvedTargets []string `json:"unresolvedTargets,omitempty"`
	Reconciled        int      `json:"reconciled"`
	// File is the absolute path of the materialized vault file.
	File string `json:"file,omitempty"`
	// PendingEdit is true when the vault file was NOT refreshed because it
	// holds an un-ingested manual edit (the revision is safely in the event
	// log; the file reconciles after the edit is imported).
	PendingEdit bool `json:"pendingEdit,omitempty"`
	// Warnings carries non-fatal advisory messages for the agent. Currently
	// populated when the document title shadows a non-document entity with the
	// same display name — a name-collision signal the agent can act on without
	// the write being blocked. Omitted when empty.
	Warnings []string `json:"warnings,omitempty"`
}

// ingestAndMaterialize runs one in-memory document through the import pipeline
// in scope and refreshes its vault file, mapping errors to tool errors. The
// returned struct has all counts as whole-document snapshots; callers may
// annotate it with warnings before marshaling.
//
// Error mapping mirrors create_entity: a schema rejection (ErrInvalidEvent) is
// the caller's input mistake → E_VALIDATION; a team-scope write by a non-member
// (ErrPermission) → E_PERMISSION; a transient lock → E_UNAVAILABLE; anything
// else → E_INTERNAL. Routing every failure through NewInternalError (the prior
// behaviour) mislabelled a non-member team write as E_INTERNAL.
func ingestAndMaterialize(ctx context.Context, w documentWriter, proj vaultProjector, scope kernel.Scope, rel string, body []byte) (documentWriteOutput, error) {
	// Resolve the default scope HERE, at the single choke point, so the importer,
	// the vault SyncPath, and the reported FilePath all agree on one concrete
	// scope. Threading a bare "" through would let ImportContent default it to
	// personal internally while SyncPath("")/FilePath("") addressed a different
	// (invalid) partition — the file would silently never materialize.
	if scope == "" {
		scope = kernel.DefaultScope
	}
	res, err := w.ImportContent(ctx, scope, rel, body)
	if err != nil {
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return documentWriteOutput{}, mcp.NewValidationError(fmt.Sprintf("write document %q: %v", rel, err), nil)
		}
		return documentWriteOutput{}, mapAppendError(fmt.Sprintf("write document %q", rel), err)
	}
	if len(res.Documents) != 1 {
		return documentWriteOutput{}, mcp.NewInternalError(fmt.Errorf("write document %q: import touched %d documents, want 1", rel, len(res.Documents)))
	}
	d := res.Documents[0]
	out := documentWriteOutput{
		Name:              collectionEntities + "/" + d.DocumentUID,
		Path:              d.Path,
		DocumentUID:       d.DocumentUID,
		RevisionUID:       d.RevisionUID,
		Changed:           d.Changed,
		Edges:             res.Edges,
		Unresolved:        res.Unresolved,
		UnresolvedTargets: res.UnresolvedTargets,
		Reconciled:        res.Reconciled,
	}
	stats, syncErr := proj.SyncPath(ctx, d.Path, string(scope))
	if syncErr != nil {
		// The revision is durably in the event log; a materialization failure
		// must not fail the write. Report the file as absent instead.
		return out, nil //nolint:nilerr // intentional: materialization errors are non-fatal
	}
	if file, fpErr := proj.FilePath(d.Path, string(scope)); fpErr == nil {
		out.File = file
	}
	out.PendingEdit = stats.PendingEdits > 0
	return out, nil
}

// docTitleFromBody extracts the document display name from a markdown body:
// the frontmatter `title` wins, then the first `# ` heading, then a
// humanized form of the vault-relative path. The result is never empty and
// matches what the importer stores as the entity display_name.
func docTitleFromBody(body, rel string) string {
	parsed, _ := document.Parse([]byte(body)) //nolint:errcheck // Parse errors are non-fatal; a partial Parsed is always returned
	if t := strings.TrimSpace(parsed.Title); t != "" {
		return t
	}
	// Humanize the vault-relative path: strip the extension, replace dashes and
	// underscores with spaces, capitalize each word. Mirrors document.humaniseFilename.
	base := strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	base = strings.NewReplacer("-", " ", "_", " ").Replace(base)
	words := strings.Fields(base)
	for i, w := range words {
		rr := []rune(w)
		if len(rr) > 0 {
			rr[0] = unicode.ToUpper(rr[0])
			words[i] = string(rr)
		}
	}
	if len(words) == 0 {
		return "Untitled"
	}
	return strings.Join(words, " ")
}

// checkWriteCollision returns a non-empty warning string when the document
// title shadows one or more non-document entities with the same display name.
// It never returns an error that would abort the write; a query failure is
// silently swallowed (the write is already durable). The returned string is
// ready to append to documentWriteOutput.Warnings.
func checkWriteCollision(ctx context.Context, cc writeCollisionChecker, title string) string {
	if cc == nil || title == "" {
		return ""
	}
	const limitCap = 51 // exactNameResolveCap+1 — mirrors navigate.exactNameResolveCap
	ents, err := cc.ListEntities(ctx, navigate.EntityFilter{NameExact: title, Limit: limitCap})
	if err != nil {
		return ""
	}
	var nonDocs []navigate.Entity
	for _, e := range ents {
		if e.Kind != "document" {
			nonDocs = append(nonDocs, e)
		}
	}
	if len(nonDocs) == 0 {
		return ""
	}
	kinds := make([]string, 0, len(nonDocs))
	seen := make(map[string]bool)
	for _, e := range nonDocs {
		if !seen[e.Kind] {
			kinds = append(kinds, e.Kind)
			seen[e.Kind] = true
		}
	}
	return fmt.Sprintf(
		"Title %q shadows a non-document entity (kind: %s). "+
			"To disambiguate in wikilinks use a kind hint, e.g. [[%s|kind:%s]], "+
			"or reference the document by its vault path.",
		title, strings.Join(kinds, ", "), title, kinds[0],
	)
}

// ============================================================================
// writeDocument
// ============================================================================

const writeDocumentInputSchema = `{
  "type": "object",
  "properties": {
    "path":   {"type": "string", "minLength": 1, "description": "Vault-relative path, e.g. notes/project-alpha.md (.md appended when no extension)."},
    "body":   {"type": "string", "description": "The full markdown body, stored verbatim. May include its own YAML frontmatter block."},
    "scope":  {"type": "string", "enum": ["project", "personal", "team", "draft"], "description": "Storage scope, fixed at creation. personal (default) = only you; project = a shared project; team = every team member (requires membership, else E_PERMISSION); draft = private staging. The same path in a different scope is a SEPARATE document."},
    "title":  {"type": "string", "description": "Optional title, synthesized into frontmatter. Only when body has no frontmatter of its own."},
    "pinned": {"type": "boolean", "description": "Pin the document (synthesized into frontmatter). Pinned documents surface first in context packs."},
    "tags":   {"type": "array", "items": {"type": "string"}, "description": "Tags, synthesized into frontmatter."},
    "frontmatter": {"type": "object", "description": "Additional frontmatter keys (JSON object), synthesized into a YAML block. Only when body has no frontmatter of its own."}
  },
  "required": ["path", "body"],
  "additionalProperties": false
}`

const writeDocumentOutputSchema = `{
  "type": "object",
  "properties": {
    "name":        {"type": "string", "description": "entities/{uid} resource name of the document-as-entity."},
    "path":        {"type": "string"},
    "documentUid": {"type": "string"},
    "revisionUid": {"type": "string"},
    "changed":     {"type": "boolean", "description": "False when the content matched the latest revision (no-op)."},
    "edges":       {"type": "integer", "description": "Total resolved wikilink and frontmatter edges the whole document holds after this write (snapshot, not a delta — counts all [[links]] in the current body that resolved, regardless of how many were already in the graph)."},
    "unresolved":  {"type": "integer", "description": "Count of [[wikilinks]] left dangling in the whole document after this write (snapshot, not a delta)."},
    "unresolvedTargets": {"type": "array", "items": {"type": "string"}, "description": "The dangling wikilink targets by name (text inside [[…]] that matched nothing) — fix a typo or create the target. Omitted when none dangle."},
    "reconciled":  {"type": "integer", "description": "Count of wikilinks in OTHER vault documents that this write newly made resolvable (cross-document reconcile pass; separate from the document's own edge snapshot above)."},
    "file":        {"type": "string", "description": "Absolute path of the materialized vault file."},
    "pendingEdit": {"type": "boolean", "description": "True when the vault file holds an un-ingested manual edit and was left untouched."},
    "warnings":    {"type": "array", "items": {"type": "string"}, "description": "Non-fatal advisory messages — e.g. a title-collision warning when the document's display name shadows a non-document entity. Does not affect the written content; the document body is stored byte-for-byte as supplied. Omitted when empty."}
  },
  "required": ["name", "path", "documentUid", "changed"],
  "additionalProperties": false
}`

// WriteDocument creates or replaces a vault document: the body is stored
// verbatim as a documentRevision event, wikilinks resolve into graph edges,
// and the vault file re-materializes.
type WriteDocument struct {
	writer    documentWriter
	proj      vaultProjector
	collision writeCollisionChecker // optional; nil disables collision warning
}

// NewWriteDocument constructs a WriteDocument tool.
func NewWriteDocument(writer documentWriter, proj vaultProjector) *WriteDocument {
	return &WriteDocument{writer: writer, proj: proj}
}

// WithCollisionChecker installs the optional entity-lookup surface used to
// detect title-shadowing collisions after a write. Returns wd so calls can be
// chained. A nil checker is a safe no-op (collision warnings are disabled).
func (wd *WriteDocument) WithCollisionChecker(cc writeCollisionChecker) *WriteDocument {
	wd.collision = cc
	return wd
}

// Name implements mcp.Tool.
func (*WriteDocument) Name() string { return "writeDocument" }

// Description implements mcp.Tool.
func (*WriteDocument) Description() string {
	return "Create or replace a markdown document at `path` with `body` (stored verbatim, event-sourced — full history is kept). [[Wikilinks]] in the body resolve to canonical entities and other documents as graph edges; dangling links reconcile automatically when their target appears. Optional `title`/`pinned`/`tags`/`frontmatter` synthesize a YAML frontmatter block when the body has none. Frontmatter `about: <name|entities/{uid}>` records the document's primary subject as a high-confidence `describes` edge (distinct from body [[wikilinks]], which are `mentions`); resolve it by uid where you can, since a renamed subject keeps the link. Frontmatter `canonical: true` lets this document win name resolution over a same-named non-document entity, so a later-created entity cannot silently steal the name. The document materializes as an editable file in the vault. Idempotent: rewriting identical content is a no-op. `scope` picks who sees the document (personal by default; team requires membership, else E_PERMISSION) and is fixed at creation — the same path in a different scope is a separate document. Result counts (`edges`, `unresolved`, `reconciled`) are whole-document snapshots after the write, not deltas. A non-fatal `warnings` field appears when the title shadows a same-named non-document entity."
}

// InputSchema implements mcp.Tool.
func (*WriteDocument) InputSchema() json.RawMessage { return json.RawMessage(writeDocumentInputSchema) }

// OutputSchema implements mcp.Tool.
func (*WriteDocument) OutputSchema() json.RawMessage {
	return json.RawMessage(writeDocumentOutputSchema)
}

type writeDocumentInput struct {
	Path        string          `json:"path"`
	Body        string          `json:"body"`
	Scope       string          `json:"scope"`
	Title       string          `json:"title"`
	Pinned      *bool           `json:"pinned"`
	Tags        []string        `json:"tags"`
	Frontmatter json.RawMessage `json:"frontmatter"`
}

// hasMetadata reports whether any frontmatter-synthesizing argument was set.
func (in *writeDocumentInput) hasMetadata() bool {
	return in.Title != "" || in.Pinned != nil || in.Tags != nil || len(in.Frontmatter) > 0
}

// bodyHasFrontmatter reports whether body opens with its own ----fenced block.
func bodyHasFrontmatter(body string) bool {
	return strings.HasPrefix(body, "---\n") || strings.HasPrefix(body, "---\r\n")
}

// composeBody synthesizes a YAML frontmatter block from the metadata arguments
// and prepends it to the body. Keys marshal in sorted order (yaml.v3 map
// behavior), so the composition is deterministic.
func composeBody(in *writeDocumentInput) (string, error) {
	if !in.hasMetadata() {
		return in.Body, nil
	}
	if bodyHasFrontmatter(in.Body) {
		return "", mcp.NewValidationError("body already begins with a frontmatter block; merge title/pinned/tags/frontmatter into it or drop those arguments", nil)
	}
	meta := map[string]any{}
	if len(in.Frontmatter) > 0 {
		if err := json.Unmarshal(in.Frontmatter, &meta); err != nil {
			return "", mcp.NewValidationError(fmt.Sprintf("frontmatter must be a JSON object: %v", err), nil)
		}
	}
	if in.Title != "" {
		meta["title"] = in.Title
	}
	if in.Pinned != nil {
		meta["pinned"] = *in.Pinned
	}
	if in.Tags != nil {
		meta["tags"] = in.Tags
	}
	raw, err := yaml.Marshal(meta)
	if err != nil {
		return "", mcp.NewValidationError(fmt.Sprintf("frontmatter does not encode as YAML: %v", err), nil)
	}
	return "---\n" + string(raw) + "---\n" + in.Body, nil
}

// Invoke implements mcp.Tool.
func (wd *WriteDocument) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in writeDocumentInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	rel, err := document.CleanVaultPath(in.Path)
	if err != nil {
		return nil, mcp.NewValidationError(err.Error(), nil)
	}
	scope, err := parseWriteScope(in.Scope)
	if err != nil {
		return nil, err
	}
	body, err := composeBody(&in)
	if err != nil {
		return nil, err
	}
	out, err := ingestAndMaterialize(ctx, wd.writer, wd.proj, scope, rel, []byte(body))
	if err != nil {
		return nil, err
	}
	if warn := checkWriteCollision(ctx, wd.collision, docTitleFromBody(body, rel)); warn != "" {
		out.Warnings = append(out.Warnings, warn)
	}
	return marshalTool(out)
}

// ============================================================================
// editDocument
// ============================================================================

const editDocumentInputSchema = `{
  "type": "object",
  "properties": {
    "path":       {"type": "string", "minLength": 1, "description": "Vault-relative path of the document to edit."},
    "scope":      {"type": "string", "enum": ["project", "personal", "team", "draft"], "description": "Disambiguates when the same path exists in more than one scope. Omit when the path is unambiguous. An edit never changes a document's scope; it is preserved."},
    "oldString":  {"type": "string", "minLength": 1, "description": "Exact text to replace. Must match the current body exactly (including whitespace) and, unless replaceAll, be unique in it."},
    "newString":  {"type": "string", "description": "Replacement text. Must differ from oldString."},
    "replaceAll": {"type": "boolean", "description": "Replace every occurrence instead of requiring oldString to be unique."}
  },
  "required": ["path", "oldString", "newString"],
  "additionalProperties": false
}`

// EditDocument applies an exact-string replacement to a document's latest body
// and ingests the result as a new revision — the same semantics as a coding
// agent's Edit tool, so partial updates don't require resending the whole body.
type EditDocument struct {
	nav       docNavigator
	writer    documentWriter
	proj      vaultProjector
	collision writeCollisionChecker // optional; nil disables collision warning
}

// NewEditDocument constructs an EditDocument tool.
func NewEditDocument(nav docNavigator, writer documentWriter, proj vaultProjector) *EditDocument {
	return &EditDocument{nav: nav, writer: writer, proj: proj}
}

// WithCollisionChecker installs the optional entity-lookup surface used to
// detect title-shadowing collisions after an edit. Returns ed so calls can be
// chained. A nil checker is a safe no-op (collision warnings are disabled).
func (ed *EditDocument) WithCollisionChecker(cc writeCollisionChecker) *EditDocument {
	ed.collision = cc
	return ed
}

// Name implements mcp.Tool.
func (*EditDocument) Name() string { return "editDocument" }

// Description implements mcp.Tool.
func (*EditDocument) Description() string {
	return "Edit a document by exact string replacement (Claude-Code Edit semantics): `oldString` must match the current body exactly and be unique unless `replaceAll`. The result is ingested as a new revision (history kept; wikilink edges re-resolved; removed links forgotten) and the vault file refreshes. Use writeDocument to replace a whole body. Result counts (`edges`, `unresolved`) are whole-document snapshots after the edit."
}

// InputSchema implements mcp.Tool.
func (*EditDocument) InputSchema() json.RawMessage { return json.RawMessage(editDocumentInputSchema) }

// OutputSchema implements mcp.Tool.
func (*EditDocument) OutputSchema() json.RawMessage {
	return json.RawMessage(writeDocumentOutputSchema)
}

type editDocumentInput struct {
	Path       string `json:"path"`
	Scope      string `json:"scope"`
	OldString  string `json:"oldString"`
	NewString  string `json:"newString"`
	ReplaceAll bool   `json:"replaceAll"`
}

// applyEdit performs the exact-string replacement with Edit-tool validation.
func applyEdit(body string, in *editDocumentInput) (string, error) {
	if in.OldString == in.NewString {
		return "", mcp.NewValidationError("newString must differ from oldString", nil)
	}
	n := strings.Count(body, in.OldString)
	switch {
	case n == 0:
		return "", mcp.NewValidationError("oldString not found in the document body; read the document and match the current text exactly", nil)
	case n > 1 && !in.ReplaceAll:
		return "", mcp.NewValidationError(fmt.Sprintf("oldString matches %d places; extend it with surrounding context to make it unique, or set replaceAll", n), nil)
	}
	if in.ReplaceAll {
		return strings.ReplaceAll(body, in.OldString, in.NewString), nil
	}
	return strings.Replace(body, in.OldString, in.NewString, 1), nil
}

// Invoke implements mcp.Tool.
func (ed *EditDocument) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in editDocumentInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.OldString == "" {
		return nil, mcp.NewValidationError("oldString is required", nil)
	}
	rel, err := document.CleanVaultPath(in.Path)
	if err != nil {
		return nil, mcp.NewValidationError(err.Error(), nil)
	}
	// selectScope only disambiguates WHICH document to edit when a path exists in
	// more than one scope; it never becomes the write scope. The edit always
	// REUSES the resolved document's own scope below (an edit never changes scope).
	selectScope, err := parseWriteScope(in.Scope)
	if err != nil {
		return nil, err
	}
	// Resolve the target. With composite (path, scope) uniqueness a path can name
	// one document per scope, so an explicit scope selects exactly that row;
	// otherwise the deterministic by-path getter (most-recently-updated across
	// the read set) resolves it.
	var doc navigate.Document
	if selectScope != "" {
		doc, err = ed.nav.GetDocumentByPathScope(ctx, rel, string(selectScope))
	} else {
		doc, err = ed.nav.GetDocumentByPath(ctx, rel)
	}
	if doc, err = mapDocErr(doc, err, rel); err != nil {
		return nil, err
	}
	body, err := applyEdit(doc.Body, &in)
	if err != nil {
		return nil, err
	}
	out, err := ingestAndMaterialize(ctx, ed.writer, ed.proj, kernel.Scope(doc.Scope), rel, []byte(body))
	if err != nil {
		return nil, err
	}
	if warn := checkWriteCollision(ctx, ed.collision, docTitleFromBody(body, rel)); warn != "" {
		out.Warnings = append(out.Warnings, warn)
	}
	return marshalTool(out)
}

// ============================================================================
// materializeVault (unlisted: a local-filesystem projection op, driven by
// `maz doc sync` / `maz rebuild`; agents write through writeDocument)
// ============================================================================

const materializeVaultInputSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Vault-relative path to materialize. Omit to sync the whole vault."},
    "scope": {"type": "string", "enum": ["project", "personal", "team", "draft"], "description": "Scope partition of the single document to materialize (the vault is laid out as <root>/<scope>/<path>). Defaults to personal. Ignored for a whole-vault sync."},
    "mustExist": {"type": "boolean", "description": "When true, a path that names no existing document is an error (E_NOT_FOUND) rather than a no-op. Set by 'maz doc sync <path>' so a typo is surfaced; left false by the path/edit/open helpers, which resolve the location of a not-yet-created file."}
  },
  "additionalProperties": false
}`

const materializeVaultOutputSchema = `{
  "type": "object",
  "properties": {
    "root":         {"type": "string", "description": "Vault root directory."},
    "file":         {"type": "string", "description": "Absolute on-disk path of the single document materialized (omitted for a whole-vault sync). Reflects THIS server's vault root, so a client need not recompute it from its own data dir."},
    "written":      {"type": "integer"},
    "unchanged":    {"type": "integer"},
    "pendingEdits": {"type": "integer", "description": "Files left untouched because they hold un-ingested manual edits."},
    "deleted":      {"type": "integer"},
    "kept":         {"type": "integer"}
  },
  "additionalProperties": false
}`

// MaterializeVault projects event-sourced documents to the editable vault
// directory. Unlisted.
type MaterializeVault struct{ proj vaultProjector }

// NewMaterializeVault constructs a MaterializeVault tool.
func NewMaterializeVault(proj vaultProjector) *MaterializeVault {
	return &MaterializeVault{proj: proj}
}

// Name implements mcp.Tool.
func (*MaterializeVault) Name() string { return "materializeVault" }

// Description implements mcp.Tool.
func (*MaterializeVault) Description() string {
	return "Materialize event-sourced documents as editable markdown files under the vault root (whole vault, or one document by `path`). Never overwrites un-ingested manual edits. Driven by `maz doc sync`."
}

// InputSchema implements mcp.Tool.
func (*MaterializeVault) InputSchema() json.RawMessage {
	return json.RawMessage(materializeVaultInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*MaterializeVault) OutputSchema() json.RawMessage {
	return json.RawMessage(materializeVaultOutputSchema)
}

// Unlisted hides materializeVault from tools/list: it is a local projection op
// surfaced through the CLI, not an agent navigation verb.
func (*MaterializeVault) Unlisted() bool { return true }

type materializeVaultInput struct {
	Path      string `json:"path"`
	Scope     string `json:"scope"`
	MustExist bool   `json:"mustExist"`
}

type materializeVaultOutput struct {
	Root string `json:"root"`
	File string `json:"file,omitempty"`
	vault.Stats
}

// Invoke implements mcp.Tool.
func (mv *MaterializeVault) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in materializeVaultInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	out := materializeVaultOutput{Root: mv.proj.Root()}
	if in.Path == "" {
		stats, err := mv.proj.SyncAll(ctx)
		if err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("materialize vault: %w", err))
		}
		out.Stats = stats
		return marshalTool(out)
	}
	// Normalize through the same CleanVaultPath the write/edit tools use, so a
	// document created as notes/foo.md is addressable as notes/foo here too
	// (the projector looks paths up by the canonical form).
	rel, cleanErr := document.CleanVaultPath(in.Path)
	if cleanErr != nil {
		return nil, mcp.NewValidationError(cleanErr.Error(), nil)
	}
	scope, scopeErr := parseWriteScope(in.Scope)
	if scopeErr != nil {
		return nil, scopeErr
	}
	if scope == "" {
		scope = kernel.DefaultScope
	}
	stats, err := mv.proj.SyncPath(ctx, rel, string(scope))
	switch {
	case errors.Is(err, navigate.ErrNotFound) && in.MustExist:
		// `maz doc sync <path>` asked to refresh a SPECIFIC document; a path
		// that names none is a typo or a stale reference, not a no-op. Surface
		// it so the agent/human learns the sync did nothing, instead of a
		// success line that hides the mistake.
		return nil, mcp.NewNotFoundError(fmt.Sprintf("no document %q", in.Path))
	case errors.Is(err, navigate.ErrNotFound):
		// A document that does not exist yet still has a well-defined vault
		// location. Materialization is a no-op (zero stats), but reporting the
		// path keeps `maz doc new/path/edit/open` daemon-rooted: the client
		// learns where THIS server's vault file lives instead of falling back
		// to a phantom path under its own default data dir.
	case err != nil:
		return nil, mcp.NewInternalError(fmt.Errorf("materialize vault: %w", err))
	default:
		out.Stats = stats
	}
	// Surface the absolute on-disk path so `maz doc path/edit/open/new` resolve
	// against THIS (the daemon's) vault root rather than recomputing it from the
	// client's default data dir — the data-dir split-brain those commands
	// otherwise hit when a daemon serves a non-default dir. Best-effort: any
	// stats above already committed the materialization.
	if file, fpErr := mv.proj.FilePath(rel, string(scope)); fpErr == nil {
		out.File = file
	}
	return marshalTool(out)
}
