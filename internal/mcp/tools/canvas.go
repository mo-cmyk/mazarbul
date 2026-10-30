package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/canvas"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// canvasDefaultHops is applied when the caller omits hops.
const canvasDefaultHops = 1

// canvasExcerptRunes caps the body excerpt embedded in a text-fallback document
// node, bounding both the canvas size and the plaintext footprint of the export.
const canvasExcerptRunes = 280

// canvasNavigator is the read surface ExportCanvas consumes: the shared
// navigation surface plus the subgraph walk and document hydration. Declared
// here (not on the shared Navigator interface) so existing tool fakes stay
// valid. Satisfied by *navigate.Navigator.
type canvasNavigator interface {
	Navigator
	Subgraph(ctx context.Context, seed string, hops int, opts navigate.RelationFilter) (navigate.SubgraphResult, error)
	GetDocumentByUID(ctx context.Context, uid string) (navigate.Document, error)
}

// canvasNavWithScopes narrows a canvasNavigator to a request's scope span. It
// mirrors navWithScopes but preserves the richer interface: Go interface return
// types are invariant, so the shared helper (which returns tools.Navigator)
// would drop Subgraph/GetDocumentByUID. A fake without WithScopes is returned
// unchanged, exactly as navWithScopes does.
func canvasNavWithScopes(nav canvasNavigator, scopes kernel.ScopeSet) canvasNavigator {
	type scopable interface {
		WithScopes(kernel.ScopeSet) *navigate.Navigator
	}
	if s, ok := nav.(scopable); ok {
		return s.WithScopes(scopes)
	}
	return nav
}

// exportCanvasInputSchema mirrors the neighbors walk knobs (hops instead of
// depth). The hops `maximum` is kept in sync with navigate.MaxHops by
// TestExportCanvasSchemaHopsMatchMaxHops.
const exportCanvasInputSchema = `{
  "type": "object",
  "properties": {
    "seed":            {"type": "string", "minLength": 1, "description": "Entity to root the subgraph at: an entities/{uid} ref, a bare uid, or a display name (resolved case-insensitively; an unknown or ambiguous name returns an error naming the candidate uids)."},
    "hops":            {"type": "integer", "minimum": 1, "maximum": 6, "description": "Subgraph radius in hops from the seed (1..6). Defaults to 1."},
    "predicates":      {"type": "array", "items": {"type": "string"}, "description": "Only include edges with these predicates. Omit to include any."},
    "at":              {"type": "string", "format": "date-time", "description": "Valid-time anchor (RFC 3339 UTC). Include edges live at this instant. Defaults to now."},
    "asOf":            {"type": "string", "format": "date-time", "description": "Transaction-time anchor (RFC 3339 UTC). Ignore edges recorded after this instant."},
    "includeInactive": {"type": "boolean", "description": "Include superseded / expired edges too. Off by default."},
    "limit":           {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum nodes across all hops. Defaults to 50."},
    "scopes":          {"type": "array", "items": {"type": "string"}, "description": "Scopes to read from: project, personal, team. Defaults to project+personal. Unknown values are rejected."}
  },
  "required": ["seed"],
  "additionalProperties": false
}`

// exportCanvasOutputSchema documents the response: the JSON Canvas document and
// the vault-relative paths of the documents rendered as file nodes.
const exportCanvasOutputSchema = `{
  "type": "object",
  "properties": {
    "canvas":    {"type": "object", "description": "The JSON Canvas 1.0 document: {nodes, edges}."},
    "documents": {"type": "array", "items": {"type": "string"}, "description": "Vault-relative paths of documents rendered as file nodes, so the caller can materialize them before opening the canvas."}
  },
  "required": ["canvas", "documents"],
  "additionalProperties": false
}`

// ExportCanvas walks an N-hop subgraph from a seed and serializes it to a JSON
// Canvas document. It is the engine behind `maz doc canvas`; the CLI writes the
// returned canvas into the vault and materializes the referenced documents so
// their file nodes resolve in Obsidian. Unlisted: a local export verb driven by
// the CLI, not a navigation verb agents browse.
type ExportCanvas struct {
	nav    canvasNavigator
	binder selfBinder
}

// NewExportCanvas constructs an ExportCanvas tool over nav.
func NewExportCanvas(nav canvasNavigator) *ExportCanvas { return &ExportCanvas{nav: nav} }

// SetSelfBinder injects the self->principal binder so a `self` seed resolves to
// the bound principal on read (mirrors neighbors/relations). Nil = no rebinding.
func (e *ExportCanvas) SetSelfBinder(b selfBinder) { e.binder = b }

// Name implements mcp.Tool.
func (*ExportCanvas) Name() string { return "exportCanvas" }

// Unlisted hides exportCanvas from tools/list: it is a CLI-driven local export
// op, not a navigation verb, matching importDocuments / materializeVault.
func (*ExportCanvas) Unlisted() bool { return true }

// Description implements mcp.Tool.
func (*ExportCanvas) Description() string {
	return "Export an N-hop subgraph rooted at `seed` as a JSON Canvas (.canvas) document: nodes are entities and documents, edges are their relations as labeled arrows. Drives `maz doc canvas`; the CLI writes the result into the vault for Obsidian. Documents render as file nodes (the live vault .md), entities as text cards. The walk is scope- and validity-gated exactly like neighbors, and `limit` caps total nodes."
}

// InputSchema implements mcp.Tool.
func (*ExportCanvas) InputSchema() json.RawMessage { return json.RawMessage(exportCanvasInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ExportCanvas) OutputSchema() json.RawMessage { return json.RawMessage(exportCanvasOutputSchema) }

type exportCanvasInput struct {
	Seed   string   `json:"seed"`
	Hops   *int     `json:"hops,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
	relationFilterInput
}

type exportCanvasOutput struct {
	Canvas    canvas.Canvas `json:"canvas"`
	Documents []string      `json:"documents"`
}

// Invoke implements mcp.Tool.
func (e *ExportCanvas) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in exportCanvasInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	hops := canvasDefaultHops
	if in.Hops != nil {
		hops = *in.Hops
	}
	filter, err := in.toFilter()
	if err != nil {
		return nil, err
	}
	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}
	nav := canvasNavWithScopes(e.nav, scopeSet)
	uid, err := resolveEntityArg(ctx, nav, e.binder, in.Seed)
	if err != nil {
		return nil, err
	}

	sg, err := nav.Subgraph(ctx, uid, hops, filter)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return nil, mcp.NewNotFoundError(fmt.Sprintf("seed %q not found", uid))
		}
		return nil, mcp.NewInternalError(fmt.Errorf("subgraph of %q: %w", uid, err))
	}

	nodes, docs, err := e.graphNodes(ctx, nav, sg.Nodes)
	if err != nil {
		return nil, err
	}
	cv := canvas.Build(canvas.Subgraph{
		SeedUID: uid,
		Nodes:   nodes,
		Edges:   canvasEdges(sg.Edges),
	})
	return marshalTool(exportCanvasOutput{Canvas: cv, Documents: docs})
}

// graphNodes maps walk nodes into canvas nodes, hydrating each document-kind
// node with its path/title/body-excerpt so it renders as a file node. It returns
// the canvas nodes and the sorted vault-relative paths of the documents (for the
// CLI to materialize so the file nodes resolve).
func (e *ExportCanvas) graphNodes(ctx context.Context, nav canvasNavigator, nodes []navigate.SubgraphNode) ([]canvas.GraphNode, []string, error) {
	out := make([]canvas.GraphNode, 0, len(nodes))
	docs := make([]string, 0)
	for i := range nodes {
		n := nodes[i]
		gn := canvas.GraphNode{
			UID:         n.Entity.UID,
			Kind:        n.Entity.Kind,
			DisplayName: n.Entity.DisplayName,
			Hops:        n.Hops,
		}
		if n.Entity.Kind == string(ontology.EntityKindDocument) {
			doc, derr := nav.GetDocumentByUID(ctx, n.Entity.UID)
			switch {
			case derr == nil:
				gn.Doc = &canvas.DocRef{Path: doc.Path, Title: doc.Title, Excerpt: canvasExcerpt(doc.Body)}
				if doc.Path != "" {
					docs = append(docs, doc.Path)
				}
			case !errors.Is(derr, navigate.ErrNotFound):
				return nil, nil, mcp.NewInternalError(fmt.Errorf("load document %q: %w", n.Entity.UID, derr))
			}
		}
		out = append(out, gn)
	}
	sort.Strings(docs)
	return out, docs, nil
}

// canvasEdges maps walk relations into canvas edges.
func canvasEdges(rels []navigate.Relation) []canvas.GraphEdge {
	out := make([]canvas.GraphEdge, 0, len(rels))
	for i := range rels {
		r := rels[i]
		out = append(out, canvas.GraphEdge{
			UID:        r.UID,
			FromUID:    r.SubjectUID,
			ToUID:      r.ObjectUID,
			Predicate:  r.Predicate,
			Confidence: r.Confidence,
		})
	}
	return out
}

// canvasExcerpt trims a document body to a single bounded excerpt for a
// text-fallback node, appending an ellipsis marker when truncated.
func canvasExcerpt(body string) string {
	body = strings.TrimSpace(body)
	runes := []rune(body)
	if len(runes) <= canvasExcerptRunes {
		return body
	}
	return strings.TrimSpace(string(runes[:canvasExcerptRunes])) + "..."
}
