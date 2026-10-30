package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// docNavigator is the read surface the document tools consume. Declared here
// (rather than extending the shared Navigator interface) so existing tool fakes
// stay valid. Satisfied by *navigate.Navigator.
type docNavigator interface {
	GetDocumentByPath(ctx context.Context, path string) (navigate.Document, error)
	GetDocumentByPathScope(ctx context.Context, path, scope string) (navigate.Document, error)
	GetDocumentByUID(ctx context.Context, uid string) (navigate.Document, error)
	ListDocuments(ctx context.Context, opts navigate.DocumentFilter) ([]navigate.Document, error)
	EntityRelations(ctx context.Context, uid string, opts navigate.RelationFilter) ([]navigate.Relation, error)
	DocumentUnresolvedLinkTargets(ctx context.Context, docUID string) ([]string, error)
}

// documentImporter is the write surface importDocuments consumes. Satisfied by
// *document.Importer.
type documentImporter interface {
	ImportDir(ctx context.Context, scope kernel.Scope, root string) (document.Result, error)
	ImportContent(ctx context.Context, scope kernel.Scope, path string, raw []byte) (document.Result, error)
}

// docLinkWire is one wikilink edge from the perspective of a document: the
// predicate (mentions / links_to) and the resolved OTHER endpoint, so an agent
// reads "what this note references" / "what references this note" without
// diffing subject/object by hand.
type docLinkWire struct {
	Predicate   string `json:"predicate"`
	UID         string `json:"uid"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
}

// documentWire is a navigate.Document plus the entity resource name so an agent
// can pivot to neighbors/relations on the document-as-entity.
type documentWire struct {
	Name string `json:"name"` // entities/{documentUid}
	navigate.Document
}

func projectDocument(d navigate.Document) documentWire {
	return documentWire{Name: collectionEntities + "/" + d.DocumentUID, Document: d}
}

// splitDocLinks partitions a document's mentions/links_to edges into outgoing
// links (the doc is the subject — what it references) and incoming backlinks
// (the doc is the object — what references it), hydrating the far endpoint.
func splitDocLinks(docUID string, rels []navigate.Relation) (links, backlinks []docLinkWire) {
	// Initialize non-nil so an edge-less document marshals "links":[] /
	// "backlinks":[] rather than omitting the keys — an agent reads an empty
	// array as "no links" but a missing key as "field unsupported".
	links, backlinks = []docLinkWire{}, []docLinkWire{}
	for i := range rels {
		r := &rels[i]
		switch docUID {
		case r.SubjectUID:
			links = append(links, docLinkWire{
				Predicate: r.Predicate, UID: r.ObjectUID,
				Name: collectionEntities + "/" + r.ObjectUID, DisplayName: r.ObjectName,
			})
		case r.ObjectUID:
			backlinks = append(backlinks, docLinkWire{
				Predicate: r.Predicate, UID: r.SubjectUID,
				Name: collectionEntities + "/" + r.SubjectUID, DisplayName: r.SubjectName,
			})
		}
	}
	return links, backlinks
}

// resolveDocArg fetches a document by its path or documentUid argument. Exactly
// one must be set. A missing document is E_NOT_FOUND.
//
// When path is set it is normalized through CleanVaultPath (appends .md when
// the extension is absent) so callers that omit the extension — e.g.
// `maz doc show notes/foo` — resolve identically to `notes/foo.md`.
//
// As a convenience, when the normalized path lookup fails and the raw input
// looks like a uid reference ("entities/<uid>" or a bare 26-character ULID),
// the uid branch is tried automatically. This lets `maz doc show entities/<uid>`
// and `maz doc show <uid>` work — the forms recall/entities emit as the
// document's Name field — without requiring a separate documentUid argument.
func resolveDocArg(ctx context.Context, nav docNavigator, path, documentUID string) (navigate.Document, error) {
	switch {
	case path != "" && documentUID != "":
		return navigate.Document{}, mcp.NewValidationError("set exactly one of path or documentUid, not both", nil)
	case path != "":
		clean, cleanErr := document.CleanVaultPath(path)
		if cleanErr == nil {
			d, derr := nav.GetDocumentByPath(ctx, clean)
			if !errors.Is(derr, navigate.ErrNotFound) {
				return mapDocErr(d, derr, clean)
			}
		}
		// Path lookup failed (or was rejected as invalid). If the raw input looks
		// like a uid reference, retry as a uid — covers "entities/<uid>" and bare
		// 26-char ULIDs that discovery tools emit as the document's Name field.
		if uid, ok := extractDocUID(path); ok {
			d, derr := nav.GetDocumentByUID(ctx, uid)
			return mapDocErr(d, derr, uid)
		}
		// Not a uid: surface the original not-found (or validation) error.
		if cleanErr != nil {
			return navigate.Document{}, mcp.NewValidationError(cleanErr.Error(), nil)
		}
		return navigate.Document{}, mcp.NewNotFoundError(fmt.Sprintf("no document %q", clean))
	case documentUID != "":
		d, err := nav.GetDocumentByUID(ctx, documentUID)
		return mapDocErr(d, err, documentUID)
	default:
		return navigate.Document{}, mcp.NewValidationError("path or documentUid is required", nil)
	}
}

// extractDocUID returns the bare document UID when s is an "entities/<uid>"
// resource name or a bare 26-character ULID (the format recall/entities emit).
// Returns ("", false) for anything else.
func extractDocUID(s string) (string, bool) {
	const prefix = "entities/"
	if strings.HasPrefix(s, prefix) {
		uid := s[len(prefix):]
		if isDocUID(uid) {
			return uid, true
		}
	}
	if isDocUID(s) {
		return s, true
	}
	return "", false
}

// isDocUID reports whether s is a 26-character ULID (uppercase alphanumeric,
// Crockford base-32 alphabet). Document UIDs are stored as ULIDs.
func isDocUID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

func mapDocErr(d navigate.Document, err error, ref string) (navigate.Document, error) {
	if errors.Is(err, navigate.ErrNotFound) {
		return navigate.Document{}, mcp.NewNotFoundError(fmt.Sprintf("no document %q", ref))
	}
	if err != nil {
		return navigate.Document{}, mcp.NewInternalError(fmt.Errorf("read document %q: %w", ref, err))
	}
	return d, nil
}

// docEdges loads a document's mentions/links_to edges (both directions, active
// only) and splits them into links + backlinks.
func docEdges(ctx context.Context, nav docNavigator, docUID string) (links, backlinks []docLinkWire, err error) {
	rels, err := nav.EntityRelations(ctx, docUID, navigate.RelationFilter{
		Predicates: []string{"mentions", "links_to"}, Limit: -1,
	})
	if err != nil {
		return nil, nil, mcp.NewInternalError(fmt.Errorf("read document edges %q: %w", docUID, err))
	}
	links, backlinks = splitDocLinks(docUID, rels)
	return links, backlinks, nil
}

// ============================================================================
// readDocument
// ============================================================================

const readDocumentInputSchema = `{
  "type": "object",
  "description": "Provide exactly ONE of path or documentUid to identify the document.",
  "properties": {
    "path":        {"type": "string", "description": "Vault-relative path of the document, e.g. notes/project-alpha.md. Provide this OR documentUid (exactly one)."},
    "documentUid": {"type": "string", "description": "Stable document UID (= its entity UID). Provide this OR path (exactly one)."}
  },
  "additionalProperties": false
}`

const readDocumentOutputSchema = `{
  "type": "object",
  "properties": {
    "document":  {"type": "object", "description": "The document: path, title, verbatim body, frontmatter, tags, pinned. The name field is the entities/{uid} resource name for pivoting to neighbors/relations."},
    "links":     {"type": "array", "description": "Outgoing wikilink edges (what this note references): mentions → entities, links_to → other documents. Always present; [] when none.", "items": {"type": "object"}},
    "backlinks": {"type": "array", "description": "Incoming edges (what references this note). Always present; [] when none.", "items": {"type": "object"}},
    "observations": {"type": "array", "description": "Structured observations parsed from the body ('- [category] text #tag (context)' lines). EACH is projected to a durable fact on THIS document entity — query them with get(entities/{documentUid}) or 'maz fact get <documentUid>'. Always present; [] when none.", "items": {"type": "object"}},
    "unresolvedLinks": {"type": "array", "description": "Target names of [[wikilinks]] in the body that resolve to nothing in the graph (dead links). These have NO edge in 'links' precisely because they did not resolve; surface them to flag a broken reference. Always present; [] when none.", "items": {"type": "string"}}
  },
  "required": ["document", "links", "backlinks", "observations", "unresolvedLinks"],
  "additionalProperties": false
}`

// ReadDocument fetches a document by path or UID plus its wikilink edges.
type ReadDocument struct{ nav docNavigator }

// NewReadDocument constructs a ReadDocument tool.
func NewReadDocument(nav docNavigator) *ReadDocument { return &ReadDocument{nav: nav} }

// Name implements mcp.Tool.
func (*ReadDocument) Name() string { return "readDocument" }

// Description implements mcp.Tool.
func (*ReadDocument) Description() string {
	return "Read one vault document by `path` or `documentUid`: its title, verbatim body, frontmatter, tags, its wikilink edges — `links` (what it references: mentions of entities, links_to other docs) and `backlinks` (what references it) — and `observations` (the `- [category] text` lines parsed from the body). Each observation is projected to a durable fact ON THIS DOCUMENT ENTITY, so query them with get(entities/{documentUid}) or `maz fact get <documentUid>`. A document is also an entity, so the returned `document.name` (entities/{uid}) pivots straight into neighbors/relations to walk the unified graph."
}

// InputSchema implements mcp.Tool.
func (*ReadDocument) InputSchema() json.RawMessage { return json.RawMessage(readDocumentInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ReadDocument) OutputSchema() json.RawMessage { return json.RawMessage(readDocumentOutputSchema) }

type readDocumentInput struct {
	Path        string `json:"path"`
	DocumentUID string `json:"documentUid"`
}

type readDocumentOutput struct {
	Document        documentWire      `json:"document"`
	Links           []docLinkWire     `json:"links"`
	Backlinks       []docLinkWire     `json:"backlinks"`
	Observations    []observationWire `json:"observations"`
	UnresolvedLinks []string          `json:"unresolvedLinks"`
}

// observationWire is one `- [category] text #tag (context)` line parsed from the
// body. Each observation is projected to a durable fact on THIS document entity,
// so an agent can read the structured claims here and then query them with
// get(entities/{documentUid}) or `maz fact get <documentUid>`.
type observationWire struct {
	Category string   `json:"category"`
	Text     string   `json:"text"`
	Tags     []string `json:"tags,omitempty"`
	Context  string   `json:"context,omitempty"`
}

// parseObservations extracts the document's observation lines from its verbatim
// body for display in readDocument. It uses the SAME partial-on-error contract
// as the importer (document.stage): document.Parse returns a usable Parsed value
// — body, wikilinks, observations — alongside any metadata error, and the
// importer keeps that partial result so a document with, say, malformed
// frontmatter still imports its body and projects its observation facts.
// Dropping observations here on a parse error would hide facts that were
// genuinely projected, contradicting the discoverability this read surface
// exists to provide. So the error is intentionally ignored and the partial
// observations are returned. The result is non-nil so the wire always carries an
// array, never null.
func parseObservations(body string) []observationWire {
	parsed, _ := document.Parse([]byte(body)) //nolint:errcheck // partial result is used by design (mirrors importer.stage)
	// make([]…, 0, n) so an empty result still marshals to [] (never null).
	out := make([]observationWire, 0, len(parsed.Observations))
	for _, ob := range parsed.Observations {
		out = append(out, observationWire{
			Category: ob.Category,
			Text:     ob.Text,
			Tags:     ob.Tags,
			Context:  ob.Context,
		})
	}
	return out
}

// liveUnresolvedLinks filters the document's write-time unresolved-link
// snapshot down to targets that are STILL dead: any snapshot target now
// satisfied by a resolved edge is dropped (the reconcile pass emits the edge
// when a once-dangling link becomes resolvable but does not rewrite the
// snapshot, so cross-checking the live edges keeps the report honest). Matching
// is case-insensitive against the resolved-edge display names; the result is
// deduplicated and always non-nil so the wire carries [] rather than null.
func liveUnresolvedLinks(snapshot []string, resolved []docLinkWire) []string {
	out := make([]string, 0, len(snapshot))
	if len(snapshot) == 0 {
		return out
	}
	resolvedNames := make(map[string]struct{}, len(resolved))
	for _, lk := range resolved {
		if lk.DisplayName != "" {
			resolvedNames[strings.ToLower(lk.DisplayName)] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(snapshot))
	for _, t := range snapshot {
		key := strings.ToLower(strings.TrimSpace(t))
		if key == "" {
			continue
		}
		if _, ok := resolvedNames[key]; ok {
			continue // a reconciled edge now satisfies this link
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, t)
	}
	return out
}

// Invoke implements mcp.Tool.
func (rd *ReadDocument) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in readDocumentInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	doc, err := resolveDocArg(ctx, rd.nav, in.Path, in.DocumentUID)
	if err != nil {
		return nil, err
	}
	links, backlinks, err := docEdges(ctx, rd.nav, doc.DocumentUID)
	if err != nil {
		return nil, err
	}
	snapshot, err := rd.nav.DocumentUnresolvedLinkTargets(ctx, doc.DocumentUID)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("read document unresolved links %q: %w", doc.DocumentUID, err))
	}
	return marshalTool(readDocumentOutput{
		Document:        projectDocument(doc),
		Links:           links,
		Backlinks:       backlinks,
		Observations:    parseObservations(doc.Body),
		UnresolvedLinks: liveUnresolvedLinks(snapshot, links),
	})
}

// ============================================================================
// getBacklinks
// ============================================================================

const getBacklinksInputSchema = `{
  "type": "object",
  "description": "Provide exactly ONE of path or documentUid to identify the document.",
  "properties": {
    "path":        {"type": "string", "description": "Vault-relative path of the document. Provide this OR documentUid (exactly one)."},
    "documentUid": {"type": "string", "description": "Stable document UID. Provide this OR path (exactly one)."}
  },
  "additionalProperties": false
}`

const getBacklinksOutputSchema = `{
  "type": "object",
  "properties": {
    "backlinks": {"type": "array", "description": "Incoming edges — documents/entities that reference this one. Always present; [] when none.", "items": {"type": "object"}},
    "links":     {"type": "array", "description": "Outgoing edges — what this document references. Always present; [] when none.", "items": {"type": "object"}}
  },
  "required": ["backlinks", "links"],
  "additionalProperties": false
}`

// GetBacklinks returns a document's incoming + outgoing wikilink edges without
// the body — the lightweight "what connects to this note" verb.
type GetBacklinks struct{ nav docNavigator }

// NewGetBacklinks constructs a GetBacklinks tool.
func NewGetBacklinks(nav docNavigator) *GetBacklinks { return &GetBacklinks{nav: nav} }

// Name implements mcp.Tool.
func (*GetBacklinks) Name() string { return "getBacklinks" }

// Description implements mcp.Tool.
func (*GetBacklinks) Description() string {
	return "List a document's wikilink edges without its body: `backlinks` (notes/entities that reference it) and `links` (what it references). The fast way to map a note's place in the graph. Identify the document by `path` or `documentUid`."
}

// InputSchema implements mcp.Tool.
func (*GetBacklinks) InputSchema() json.RawMessage { return json.RawMessage(getBacklinksInputSchema) }

// OutputSchema implements mcp.Tool.
func (*GetBacklinks) OutputSchema() json.RawMessage {
	return json.RawMessage(getBacklinksOutputSchema)
}

type getBacklinksOutput struct {
	Backlinks []docLinkWire `json:"backlinks"`
	Links     []docLinkWire `json:"links"`
}

// Invoke implements mcp.Tool.
func (g *GetBacklinks) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in readDocumentInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	doc, err := resolveDocArg(ctx, g.nav, in.Path, in.DocumentUID)
	if err != nil {
		return nil, err
	}
	links, backlinks, err := docEdges(ctx, g.nav, doc.DocumentUID)
	if err != nil {
		return nil, err
	}
	return marshalTool(getBacklinksOutput{Backlinks: backlinks, Links: links})
}

// ============================================================================
// listDocuments
// ============================================================================

const listDocumentsInputSchema = `{
  "type": "object",
  "properties": {
    "pathPrefix": {"type": "string", "description": "Restrict to documents whose path starts with this prefix, e.g. notes/."},
    "tag":        {"type": "string", "description": "Restrict to documents whose frontmatter tags include this value."},
    "pinned":     {"type": "boolean", "description": "When true, only pinned documents."},
    "limit":      {"type": "integer", "minimum": 1, "maximum": 200, "description": "Max documents to return (default 50)."}
  },
  "additionalProperties": false
}`

const listDocumentsOutputSchema = `{
  "type": "object",
  "properties": {
    "documents": {"type": "array", "description": "Matching documents, most-recently-updated first. Bodies are omitted; call readDocument for the full body.", "items": {"type": "object"}}
  },
  "required": ["documents"],
  "additionalProperties": false
}`

// ListDocuments lists documents matching a path prefix / tag / pinned filter.
type ListDocuments struct{ nav docNavigator }

// NewListDocuments constructs a ListDocuments tool.
func NewListDocuments(nav docNavigator) *ListDocuments { return &ListDocuments{nav: nav} }

// Name implements mcp.Tool.
func (*ListDocuments) Name() string { return "listDocuments" }

// Description implements mcp.Tool.
func (*ListDocuments) Description() string {
	return "List vault documents, most-recently-updated first, filtered by `pathPrefix`, `tag`, and/or `pinned`. Bodies are omitted to stay lightweight; call readDocument for one document's full body and edges."
}

// InputSchema implements mcp.Tool.
func (*ListDocuments) InputSchema() json.RawMessage { return json.RawMessage(listDocumentsInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ListDocuments) OutputSchema() json.RawMessage {
	return json.RawMessage(listDocumentsOutputSchema)
}

type listDocumentsInput struct {
	PathPrefix string `json:"pathPrefix"`
	Tag        string `json:"tag"`
	Pinned     bool   `json:"pinned"`
	Limit      int    `json:"limit"`
}

type listDocumentsOutput struct {
	Documents []documentWire `json:"documents"`
}

// Invoke implements mcp.Tool.
func (l *ListDocuments) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in listDocumentsInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	// The schema contract is 1..200 (0 = unset → server default). A negative
	// limit means "no cap" to navigate.ListDocuments, so reject it here rather
	// than let one call return the entire vault.
	limit := in.Limit
	if limit < 0 {
		return nil, mcp.NewValidationError("limit must be between 1 and 200", nil)
	}
	if limit > maxNavigateLimit {
		limit = maxNavigateLimit
	}
	docs, err := l.nav.ListDocuments(ctx, navigate.DocumentFilter{
		PathPrefix: in.PathPrefix, Tag: in.Tag, PinnedOnly: in.Pinned, Limit: limit,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("list documents: %w", err))
	}
	out := listDocumentsOutput{Documents: make([]documentWire, 0, len(docs))}
	for i := range docs {
		out.Documents = append(out.Documents, projectDocument(docs[i]))
	}
	return marshalTool(out)
}

// ============================================================================
// importDocuments (unlisted: a local-filesystem batch op, driven by `maz doc
// import`; kept off the agent tool list so the agent surface stays focused on
// read/navigate verbs)
// ============================================================================

const importDocumentsInputSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "minLength": 1, "description": "Filesystem path to a markdown file or a directory of them (recursive)."}
  },
  "required": ["path"],
  "additionalProperties": false
}`

const importDocumentsOutputSchema = `{
  "type": "object",
  "properties": {
    "imported":   {"type": "integer"},
    "skipped":    {"type": "integer"},
    "edges":      {"type": "integer"},
    "unresolved": {"type": "integer"},
    "reconciled": {"type": "integer"},
    "documents":  {"type": "array", "items": {"type": "object"}}
  },
  "additionalProperties": false
}`

// ImportDocuments ingests a markdown file or vault directory, intertwining each
// document with the structured graph (wikilink → entity/doc edges). Unlisted.
type ImportDocuments struct {
	im documentImporter
	// vaultRoot is the configured editable-vault directory. A single-file
	// import whose target lives INSIDE this root is addressed by its
	// vault-relative path (see Invoke); empty disables that mapping.
	vaultRoot string
	// proj re-materializes each imported revision into the vault so a directory
	// import from OUTSIDE the vault lands on disk in the canonical location
	// (the design's "a revision appended via importDocuments re-materializes the
	// file" guarantee). May be nil (no vault wired / tests): materialization is
	// then skipped — the revisions are still durable in the event log and a
	// later materializeVault / rebuild projects them.
	proj vaultProjector
}

// NewImportDocuments constructs an ImportDocuments tool over im. VaultRoot is
// the configured vault directory ("" when no vault is wired, e.g. tests). The
// projector re-materializes imported revisions to the vault and may be nil.
func NewImportDocuments(im documentImporter, vaultRoot string, proj vaultProjector) *ImportDocuments {
	return &ImportDocuments{im: im, vaultRoot: vaultRoot, proj: proj}
}

// Name implements mcp.Tool.
func (*ImportDocuments) Name() string { return "importDocuments" }

// Description implements mcp.Tool.
func (*ImportDocuments) Description() string {
	return "Import a markdown file or vault directory (recursive) as event-sourced documents, resolving [[wikilinks]] to canonical entities and other documents. Idempotent by content hash. Driven by `maz doc import`."
}

// InputSchema implements mcp.Tool.
func (*ImportDocuments) InputSchema() json.RawMessage {
	return json.RawMessage(importDocumentsInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*ImportDocuments) OutputSchema() json.RawMessage {
	return json.RawMessage(importDocumentsOutputSchema)
}

// Unlisted hides importDocuments from tools/list (see ask.go for the pattern):
// it is a local-filesystem batch op surfaced through the CLI, not a verb agents
// should reach for during navigation.
func (*ImportDocuments) Unlisted() bool { return true }

type importDocumentsInput struct {
	Path string `json:"path"`
}

// Invoke implements mcp.Tool.
func (id *ImportDocuments) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in importDocumentsInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.Path == "" {
		return nil, mcp.NewValidationError("path is required", nil)
	}
	var (
		res   document.Result
		err   error
		scope = kernel.DefaultScope
	)
	if rel, ok := vaultRelativeFile(id.vaultRoot, in.Path); ok {
		// A single file INSIDE the configured vault keeps its vault-relative
		// path, so the documented round-trip
		//   vim "$(maz doc path notes/foo)" && maz doc import "$(maz doc path notes/foo)"
		// updates notes/foo.md. ImportDir's single-file mode rebases on the
		// file's parent directory, which would re-import a nested vault file
		// as a basename-only sibling (foo.md) — a different document. The vault is
		// partitioned <root>/<scope>/<path>, so the leading segment is the scope
		// and the remainder is the document's vault-relative path.
		var relInScope string
		scope, relInScope = splitVaultScope(rel)
		raw, rErr := os.ReadFile(in.Path)
		if rErr != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("read %q: %w", in.Path, rErr))
		}
		res, err = id.im.ImportContent(ctx, scope, relInScope, raw)
	} else {
		// A directory (or a file outside the vault) is an external bulk import; it
		// carries no scope partition, so it lands in personal for v1. A --scope
		// flag for bulk import is a documented follow-up.
		res, err = id.im.ImportDir(ctx, scope, in.Path)
	}
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("import %q: %w", in.Path, err))
	}
	id.materialize(ctx, scope, res)
	return marshalTool(res)
}

// splitVaultScope splits a vault-root-relative path (<scope>/<path>) into its
// scope partition and the document's vault-relative path. It only treats the
// leading segment as a scope when it is BOTH a valid scope AND followed by a
// non-empty document path; anything else (a flat pre-partition file, a file
// directly at the root, or a bare scope directory name like "personal" with no
// trailing path) falls back to the default scope and the whole path unchanged.
func splitVaultScope(rel string) (kernel.Scope, string) {
	seg, remainder, found := strings.Cut(rel, "/")
	if found && remainder != "" && kernel.Scope(seg).Valid() {
		return kernel.Scope(seg), remainder
	}
	return kernel.DefaultScope, rel
}

// materialize re-projects every CHANGED imported document into the vault so the
// canonical body lands on disk (the editable-loop guarantee for non-watcher
// append paths). Unchanged documents are skipped — their on-disk hash already
// matches (and a Projector.SyncPath would be an I1 no-op anyway). All work is
// best-effort: a materialization failure must not fail the import, since the
// revisions are already durable in the event log and a later
// materializeVault/rebuild re-projects them. No-op when no vault is wired.
func (id *ImportDocuments) materialize(ctx context.Context, scope kernel.Scope, res document.Result) {
	if id.proj == nil {
		return
	}
	for _, d := range res.Documents {
		if !d.Changed {
			continue
		}
		if _, err := id.proj.SyncPath(ctx, d.Path, string(scope)); err != nil {
			// Tombstoned/uncommitted races and projection skips are expected;
			// the revision stays durable in the event log regardless.
			continue
		}
	}
}

// vaultRelativeFile reports whether p names a regular file inside vaultRoot,
// returning its forward-slash vault-relative path when it does. Symlinks are
// resolved on both sides so paths compare equal across e.g. the macOS
// /tmp → /private/tmp indirection; resolution failures fall back to the
// lexical absolute paths (the containment test then simply stays lexical).
// An empty vaultRoot disables the mapping.
func vaultRelativeFile(vaultRoot, p string) (string, bool) {
	if vaultRoot == "" {
		return "", false
	}
	absRoot, err := filepath.Abs(vaultRoot)
	if err != nil {
		return "", false
	}
	absP, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	if r, rErr := filepath.EvalSymlinks(absRoot); rErr == nil {
		absRoot = r
	}
	if r, rErr := filepath.EvalSymlinks(absP); rErr == nil {
		absP = r
	}
	rel, err := filepath.Rel(absRoot, absP)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// marshalTool marshals a tool response, mapping a marshal failure to an
// internal error.
func marshalTool(v any) (json.RawMessage, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}
