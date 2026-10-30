package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/document"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// lintNavigator is the read surface the lintDocuments tool consumes: the four
// document-health queries plus the document.Reader methods LiveDeadLinkDocs
// re-resolves dangling links with. Satisfied by *navigate.Navigator.
type lintNavigator interface {
	document.Reader
	OrphanDocuments(ctx context.Context) ([]navigate.LintOrphanDoc, error)
	DocumentContradictions(ctx context.Context) ([]navigate.LintContradictionDoc, error)
	DanglingDocumentEdges(ctx context.Context) ([]navigate.LintDanglingEdge, error)
	NearDuplicateDocuments(ctx context.Context, threshold float32) ([]navigate.LintNearDuplicateDoc, error)
}

// lintDeadLinkWire is the agent-facing shape of one dead-link document. It drops
// the navigator's internal RevisionUID + raw unresolved-link JSON snapshot,
// surfacing what an agent needs to act: which document still carries unresolved
// [[wikilinks]] AND the exact target strings that name nothing in the graph, so
// the fix is "edit [[Ghost]] in notes/x.md", not "hunt through notes/x.md".
type lintDeadLinkWire struct {
	DocumentUID string   `json:"documentUid"`
	Path        string   `json:"path"`
	Title       string   `json:"title,omitempty"`
	Unresolved  []string `json:"unresolved"`
}

// lintNameCollisionWire is the agent-facing shape of one name-collision finding:
// a document whose title (display name) shadows a non-document entity that
// shares the exact same display name. The presence of both can silently mislead
// wikilink resolution and entity lookup unless the agent disambiguates.
type lintNameCollisionWire struct {
	// DocumentUID is the UID of the document entity whose title collides.
	DocumentUID string `json:"documentUid"`
	// Path is the vault-relative path of the colliding document.
	Path string `json:"path,omitempty"`
	// Title is the display name that collides.
	Title string `json:"title"`
	// ShadowedKind is the kind of the non-document entity being shadowed,
	// e.g. "product", "person", "organization".
	ShadowedKind string `json:"shadowedKind"`
	// ShadowedUID is the UID of the non-document entity being shadowed.
	ShadowedUID string `json:"shadowedUid"`
	// Hint guides the agent toward disambiguation: how to tell a wikilink
	// apart from the document (e.g. use a kind hint or vault path).
	Hint string `json:"hint"`
}

// lintDocumentsOutput is the lintDocuments tool result. All slices are
// non-nil so an agent always receives arrays, never null. This shape is the
// shared contract with the `maz doc lint` CLI renderer, which unmarshals it
// whether the report came from the daemon transport or the in-process fallback.
type lintDocumentsOutput struct {
	Orphans        []navigate.LintOrphanDoc        `json:"orphans"`
	DeadLinks      []lintDeadLinkWire              `json:"deadLinks"`
	Contradictions []navigate.LintContradictionDoc `json:"contradictions"`
	DanglingEdges  []navigate.LintDanglingEdge     `json:"danglingEdges"`
	NearDuplicates []navigate.LintNearDuplicateDoc `json:"nearDuplicates"`
	NameCollisions []lintNameCollisionWire         `json:"nameCollisions"`
}

const lintDocumentsInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const lintDocumentsOutputSchema = `{
  "type": "object",
  "properties": {
    "orphans":        {"type": "array", "description": "Documents with no inbound or outbound graph edges (isolated notes).", "items": {"type": "object"}},
    "deadLinks":      {"type": "array", "description": "Documents that still carry an unresolved [[wikilink]]; each entry's 'unresolved' array names the exact target strings that resolve to nothing.", "items": {
      "type": "object",
      "properties": {
        "documentUid": {"type": "string", "description": "Stable UID of the document carrying the unresolved link."},
        "path":        {"type": "string", "description": "Vault-relative path of the document, e.g. notes/x.md."},
        "title":       {"type": "string", "description": "Document title, if set."},
        "unresolved":  {"type": "array", "description": "The exact [[wikilink]] target strings that name nothing in the graph.", "items": {"type": "string"}}
      },
      "required": ["documentUid", "path", "unresolved"],
      "additionalProperties": false
    }},
    "contradictions": {"type": "array", "description": "Document pairs joined by an active contradicts edge.", "items": {"type": "object"}},
    "danglingEdges":  {"type": "array", "description": "Active links from a live document to a DELETED document (a backlink left behind when deleteDocument tombstoned its target). Each entry carries the live 'subjectPath' and the deleted 'targetPath'/'targetTitle' it still points at, so the fix is 're-point or remove the link in subjectPath'.", "items": {
      "type": "object",
      "properties": {
        "subjectUid":  {"type": "string", "description": "UID of the live document carrying the dangling link."},
        "subjectPath": {"type": "string", "description": "Vault-relative path of the live document that still links to a deleted note."},
        "predicate":   {"type": "string", "description": "The link predicate (links_to, mentions, or a typed extension)."},
        "targetUid":   {"type": "string", "description": "UID of the deleted target document."},
        "targetPath":  {"type": "string", "description": "Last vault-relative path of the deleted target document."},
        "targetTitle": {"type": "string", "description": "Title of the deleted target document, if set."}
      },
      "required": ["subjectUid", "subjectPath", "predicate", "targetUid", "targetPath"],
      "additionalProperties": false
    }},
    "nearDuplicates": {"type": "array", "description": "Pairs of live documents whose chunk embeddings are suspiciously similar (cosine >= 0.92). Non-blocking lint signal only — does NOT prevent writes or imports. Each pair is reported once.", "items": {
      "type": "object",
      "properties": {
        "subjectUid":  {"type": "string", "description": "UID of the first document in the pair (lexically lower UID)."},
        "subjectPath": {"type": "string", "description": "Vault-relative path of the first document."},
        "objectUid":   {"type": "string", "description": "UID of the second document in the pair."},
        "objectPath":  {"type": "string", "description": "Vault-relative path of the second document."},
        "similarity":  {"type": "number", "description": "Cosine similarity of the most-similar chunk pair (0..1). Higher means more similar."}
      },
      "required": ["subjectUid", "subjectPath", "objectUid", "objectPath", "similarity"],
      "additionalProperties": false
    }},
    "nameCollisions": {"type": "array", "description": "Documents whose title (display name) shadows a non-document entity with the exact same name. Wikilink resolution silently favours the non-document entity in this case; use the vault path or a kind qualifier to disambiguate.", "items": {
      "type": "object",
      "properties": {
        "documentUid":  {"type": "string", "description": "UID of the document entity whose title collides."},
        "path":         {"type": "string", "description": "Vault-relative path of the colliding document."},
        "title":        {"type": "string", "description": "The display name shared by the document and the non-document entity."},
        "shadowedKind": {"type": "string", "description": "Kind of the non-document entity that wins resolution, e.g. 'product', 'person'."},
        "shadowedUid":  {"type": "string", "description": "UID of the non-document entity being shadowed."},
        "hint":         {"type": "string", "description": "Disambiguation guidance: how to target the document vs. the entity unambiguously."}
      },
      "required": ["documentUid", "title", "shadowedKind", "shadowedUid", "hint"],
      "additionalProperties": false
    }}
  },
  "required": ["orphans", "deadLinks", "contradictions", "danglingEdges", "nearDuplicates", "nameCollisions"],
  "additionalProperties": false
}`

// LintDocuments runs the read-only document-health check (orphans, dead links,
// contradictions) over the vault through the navigator. It is the daemon-served
// counterpart of `maz doc lint`: the CLI used to open the store in-process and
// died with a cryptic lock error whenever a daemon held it, so lint now routes
// here like every other document read.
type LintDocuments struct {
	nav      lintNavigator
	resolver func() document.SemanticNameResolver
	log      *zap.Logger
}

// NewLintDocuments constructs a LintDocuments tool. A nil logger is tolerated.
//
// The resolver argument is a lazy getter for the semantic name resolver,
// consulted at invoke time so the daemon's dead-link re-resolution can suppress
// a stale snapshot link that a semantically-close entity now satisfies (e.g.
// [[Rosie]] → "Rosie Cotton"). It must be read lazily because the daemon wires
// the resolver onto the importer AFTER the navigation tools are registered. A
// nil getter, or one that returns nil, runs lint over the deterministic lanes only.
func NewLintDocuments(nav lintNavigator, resolver func() document.SemanticNameResolver, log *zap.Logger) *LintDocuments {
	if log == nil {
		log = zap.NewNop()
	}
	return &LintDocuments{nav: nav, resolver: resolver, log: log}
}

// Name implements mcp.Tool.
func (*LintDocuments) Name() string { return "lintDocuments" }

// Description implements mcp.Tool.
func (*LintDocuments) Description() string {
	return "Run a read-only health check over the markdown vault: `orphans` (no graph edges), " +
		"`deadLinks` (unresolved [[wikilinks]]), `contradictions` (contradicts edges), " +
		"`danglingEdges` (live link to a deleted doc), `nearDuplicates` (pairs of docs whose " +
		"chunk embeddings exceed cosine 0.92, non-blocking lint signal only), and " +
		"`nameCollisions` (documents whose title shadows a non-document entity of the same " +
		"display name — wikilink resolution silently favours the entity). " +
		"Takes no arguments; scans the whole vault."
}

// InputSchema implements mcp.Tool.
func (*LintDocuments) InputSchema() json.RawMessage { return json.RawMessage(lintDocumentsInputSchema) }

// OutputSchema implements mcp.Tool.
func (*LintDocuments) OutputSchema() json.RawMessage {
	return json.RawMessage(lintDocumentsOutputSchema)
}

// Invoke implements mcp.Tool. It mirrors the in-process `maz doc lint`
// computation: orphan documents, live dead-link re-resolution, contradiction
// pairs, dangling backlinks, near-duplicate embedding pairs, and name-collision
// documents whose title shadows a same-named non-document entity. The dead-link
// pass re-resolves each write-time unresolved snapshot against the current
// projection, including the semantic name-embedding lane when the daemon wired
// one, so a stale snapshot link now satisfied by a semantically-close entity is
// not reported as dead.
func (l *LintDocuments) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	orphans, err := l.nav.OrphanDocuments(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(err)
	}
	var snr document.SemanticNameResolver
	if l.resolver != nil {
		snr = l.resolver()
	}
	deadDocs, err := document.LiveDeadLinkDocs(ctx, l.nav, snr, l.log)
	if err != nil {
		return nil, mcp.NewInternalError(err)
	}
	contradictions, err := l.nav.DocumentContradictions(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(err)
	}
	dangling, err := l.nav.DanglingDocumentEdges(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(err)
	}
	nearDups, err := l.nav.NearDuplicateDocuments(ctx, navigate.NearDuplicateSimilarityThreshold)
	if err != nil {
		return nil, mcp.NewInternalError(err)
	}
	collisions, err := l.findNameCollisions(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(err)
	}

	out := lintDocumentsOutput{
		Orphans:        orphans,
		DeadLinks:      make([]lintDeadLinkWire, 0, len(deadDocs)),
		Contradictions: contradictions,
		DanglingEdges:  dangling,
		NearDuplicates: nearDups,
		NameCollisions: collisions,
	}
	for _, d := range deadDocs {
		unresolved := d.Unresolved
		if unresolved == nil {
			unresolved = []string{}
		}
		out.DeadLinks = append(out.DeadLinks, lintDeadLinkWire{
			DocumentUID: d.DocumentUID,
			Path:        d.Path,
			Title:       d.Title,
			Unresolved:  unresolved,
		})
	}
	if out.Orphans == nil {
		out.Orphans = []navigate.LintOrphanDoc{}
	}
	if out.Contradictions == nil {
		out.Contradictions = []navigate.LintContradictionDoc{}
	}
	if out.DanglingEdges == nil {
		out.DanglingEdges = []navigate.LintDanglingEdge{}
	}
	if out.NearDuplicates == nil {
		out.NearDuplicates = []navigate.LintNearDuplicateDoc{}
	}
	if out.NameCollisions == nil {
		out.NameCollisions = []lintNameCollisionWire{}
	}
	return marshalTool(out)
}

// findNameCollisions pages through all document entities and checks whether any
// document's title (display name) collides with a non-document entity of the
// same name. The detection mirrors navigate.ResolveDisplayName's partition
// logic: navigate.PartitionAndResolve returns ShadowedDocument=true when at
// least one canonical (non-document) entity AND at least one document namesake
// share the same display name and the canonical entity wins resolution.
func (l *LintDocuments) findNameCollisions(ctx context.Context) ([]lintNameCollisionWire, error) {
	const batchSize = 200
	var out []lintNameCollisionWire
	offset := 0
	for {
		docs, err := l.nav.ListEntities(ctx, navigate.EntityFilter{
			Kind:   "document",
			Limit:  batchSize,
			Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, doc := range docs {
			if doc.DisplayName == "" {
				continue
			}
			w, found, cerr := l.checkTitleCollision(ctx, doc)
			if cerr != nil {
				return nil, cerr
			}
			if found {
				out = append(out, w)
			}
		}
		if len(docs) < batchSize {
			break
		}
		offset += batchSize
	}
	return out, nil
}

// checkTitleCollision checks whether doc's display name collides with a
// non-document entity. It fetches one more than the resolve cap (51) so
// overflow is detectable, then delegates partition + tie-break to
// navigate.PartitionAndResolve. A collision is found when the result is
// ResolvedOK with ShadowedDocument=true (a canonical entity won but at least
// one document namesake was present). Returns the wire and true on a hit.
func (l *LintDocuments) checkTitleCollision(ctx context.Context, doc navigate.Entity) (lintNameCollisionWire, bool, error) {
	// Fetch cap+1 so overflow is detectable (mirrors ResolveDisplayName).
	const fetchCap = 51
	ents, err := l.nav.ListEntities(ctx, navigate.EntityFilter{
		NameExact: doc.DisplayName,
		Limit:     fetchCap,
	})
	if err != nil {
		return lintNameCollisionWire{}, false, err
	}
	overflowed := len(ents) >= fetchCap
	if overflowed {
		ents = ents[:fetchCap-1]
	}

	res := navigate.PartitionAndResolve(ents, "", overflowed)
	if !res.ShadowedDocument || res.Status != navigate.ResolvedOK {
		return lintNameCollisionWire{}, false, nil
	}

	// Identify the winning canonical entity's kind from the fetched slice.
	var shadowedKind string
	for _, e := range ents {
		if e.UID == res.UID {
			shadowedKind = e.Kind
			break
		}
	}
	if shadowedKind == "" {
		return lintNameCollisionWire{}, false, nil
	}

	// Fetch the document's vault path (only done on a confirmed collision).
	docPath := doc.DisplayName // safe fallback when GetDocumentByUID misses
	if d, derr := l.nav.GetDocumentByUID(ctx, doc.UID); derr == nil {
		docPath = d.Path
	}

	hint := fmt.Sprintf(
		"Use the vault path %q to target the document unambiguously. "+
			"The existing %q entity (entities/%s) shares this display name and wins "+
			"wikilink resolution. Add a kind qualifier (e.g. [[%s|kind:%s]]) to "+
			"explicitly target that entity instead.",
		docPath, shadowedKind, res.UID, doc.DisplayName, shadowedKind,
	)

	return lintNameCollisionWire{
		DocumentUID:  doc.UID,
		Path:         docPath,
		Title:        doc.DisplayName,
		ShadowedKind: shadowedKind,
		ShadowedUID:  res.UID,
		Hint:         hint,
	}, true, nil
}
