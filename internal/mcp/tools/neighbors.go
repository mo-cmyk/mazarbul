package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// neighborsDefaultDepth is applied when the caller omits depth.
const neighborsDefaultDepth = 1

// neighborsInputSchema is the JSON Schema for the tools/call arguments. The
// `seed` is an entities/{uid} name (or bare uid). Depth is capped at MaxHops
// (6) hops; the validity + predicate knobs gate which edges the walk follows.
// The depth `maximum` is kept in sync with navigate.MaxHops by
// TestNeighborsSchemaDepthMatchesMaxHops.
const neighborsInputSchema = `{
  "type": "object",
  "properties": {
    "seed":            {"type": "string", "minLength": 1, "description": "Entity to expand from: an entities/{uid} ref (as returned in a recall hit's entities[]), a bare uid, or a display name (resolved case-insensitively; an unknown or ambiguous name returns an error naming the candidate uids so you can retry)."},
    "depth":           {"type": "integer", "minimum": 1, "maximum": 6, "description": "Hops to expand. 1 = direct neighbors, 2 = neighbors-of-neighbors, up to 6. Defaults to 1."},
    "predicates":      {"type": "array", "items": {"type": "string"}, "description": "Only follow edges with these predicates. Omit to follow any."},
    "at":              {"type": "string", "format": "date-time", "description": "Valid-time anchor (RFC 3339 UTC). Walk edges live at this instant. Defaults to now."},
    "asOf":            {"type": "string", "format": "date-time", "description": "Transaction-time anchor (RFC 3339 UTC). Ignore edges recorded after this instant."},
    "includeInactive": {"type": "boolean", "description": "Follow superseded / expired edges too. Off by default."},
    "limit":           {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum neighbors to return across all hops. Defaults to 50."},
    "scopes":          {"type": "array", "items": {"type": "string"}, "description": "Scopes to read from: project, personal, team. Defaults to project+personal. Unknown values are rejected."}
  },
  "required": ["seed"],
  "additionalProperties": false
}`

// neighborsOutputSchema documents the response: hydrated neighbor entities,
// the edge each was reached through, direction, and hop distance.
const neighborsOutputSchema = `{
  "type": "object",
  "properties": {
    "neighbors": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "entity":    {"type": "object", "description": "The hydrated reached entity (with its resource name)."},
          "via":       {"type": "object", "description": "The relation the entity was reached through."},
          "direction": {"type": "string", "enum": ["outgoing", "incoming"], "description": "Edge direction relative to the seed."},
          "hops":      {"type": "integer", "minimum": 1, "description": "Hop distance from the seed."}
        },
        "required": ["entity", "via", "direction", "hops"],
        "additionalProperties": false
      }
    }
  },
  "required": ["neighbors"],
  "additionalProperties": false
}`

// Neighbors expands an entity's neighborhood, returning hydrated neighbor
// entities and the connecting edges — the hydrated evolution of `maz traverse`
// (which returns bare uids). LLM-free; the agent decides depth itself.
type Neighbors struct {
	nav    Navigator
	binder selfBinder
}

// NewNeighbors constructs a Neighbors tool over nav.
func NewNeighbors(nav Navigator) *Neighbors { return &Neighbors{nav: nav} }

// SetSelfBinder injects the self->principal binder so a `self` seed resolves to
// the bound principal on read (mirrors relate/fact). Nil = no rebinding.
func (n *Neighbors) SetSelfBinder(b selfBinder) { n.binder = b }

// Name implements mcp.Tool.
func (*Neighbors) Name() string { return "neighbors" }

// Description implements mcp.Tool.
func (*Neighbors) Description() string {
	return "Expand an entity's neighborhood up to 6 hops and return the hydrated neighbor entities plus the relations connecting them, each hop tagged direction=outgoing|incoming relative to the seed. LLM-free graph walk: pass a recall anchor as the seed and traverse the structure yourself. The DEFAULT view is point-in-time NOW: only edges valid now and not superseded; pass includeInactive=true for full history, or time-travel with at/asOf. Filter edges by predicate. The `limit` (default 50) caps total neighbors across all hops, so deep walks stay bounded."
}

// InputSchema implements mcp.Tool.
func (*Neighbors) InputSchema() json.RawMessage { return json.RawMessage(neighborsInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Neighbors) OutputSchema() json.RawMessage { return json.RawMessage(neighborsOutputSchema) }

type neighborsInput struct {
	Seed   string   `json:"seed"`
	Depth  *int     `json:"depth,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
	relationFilterInput
}

type neighborsOutput struct {
	Neighbors []neighborWire `json:"neighbors"`
}

// Invoke implements mcp.Tool.
func (n *Neighbors) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in neighborsInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	// Validate cheap request syntax (depth, time anchors) before the entity
	// lookup, so a malformed request fails fast without a resolution round-trip.
	depth := neighborsDefaultDepth
	if in.Depth != nil {
		depth = *in.Depth
	}
	filter, err := in.toFilter()
	if err != nil {
		return nil, err
	}
	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}
	nav := navWithScopes(n.nav, scopeSet)
	uid, err := resolveEntityArg(ctx, nav, n.binder, in.Seed)
	if err != nil {
		return nil, err
	}

	hops, err := nav.Neighbors(ctx, uid, depth, filter)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("neighbors of %q: %w", uid, err))
	}

	body, err := json.Marshal(neighborsOutput{Neighbors: projectNeighbors(hops)})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}
