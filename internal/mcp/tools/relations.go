package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// relationsInputSchema is the JSON Schema for the tools/call arguments. The
// `entity` is an entities/{uid} name (or bare uid). By default only relations
// live now and not superseded are returned; the temporal + predicate knobs
// narrow or time-travel the set.
const relationsInputSchema = `{
  "type": "object",
  "properties": {
    "entity":          {"type": "string", "minLength": 1, "description": "Entity whose relations to list: an entities/{uid} ref (as returned in a recall hit's entities[]), a bare uid, or a display name (resolved case-insensitively; an unknown or ambiguous name returns an error naming the candidate uids so you can retry)."},
    "predicates":      {"type": "array", "items": {"type": "string"}, "description": "Restrict to these predicates, e.g. [\"works_at\",\"reports_to\"]. Omit to return all."},
    "at":              {"type": "string", "format": "date-time", "description": "Valid-time anchor (RFC 3339 UTC). Returns relations live at this instant. Defaults to now."},
    "asOf":            {"type": "string", "format": "date-time", "description": "Transaction-time anchor (RFC 3339 UTC). Hides relations recorded after this instant."},
    "includeInactive": {"type": "boolean", "description": "Return superseded / expired relations too (full history). Off by default."},
    "limit":           {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum relations to return. Defaults to 50."},
    "scopes":          {"type": "array", "items": {"type": "string"}, "description": "Scopes to read from: project, personal, team. Defaults to project+personal. Unknown values are rejected."}
  },
  "required": ["entity"],
  "additionalProperties": false
}`

// relationsOutputSchema documents the response: edges with hydrated endpoint
// names + validity + attributes.
const relationsOutputSchema = `{
  "type": "object",
  "properties": {
    "relations": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name":            {"type": "string", "description": "Resource name relations/{uid}."},
          "subject":         {"type": "string", "description": "Subject entity resource name entities/{uid}."},
          "object":          {"type": "string", "description": "Object entity resource name entities/{uid}."},
          "direction":       {"type": "string", "enum": ["outgoing", "incoming"], "description": "Edge direction relative to the queried entity: 'outgoing' when it is the subject of this relation, 'incoming' when it is the object. Present on every row of this listing — read it instead of comparing subjectUid yourself to answer directional questions (who reports_to / manages whom)."},
          "uid":             {"type": "string"},
          "subjectUid":      {"type": "string"},
          "subjectName":     {"type": "string"},
          "predicate":       {"type": "string"},
          "objectUid":       {"type": "string"},
          "objectName":      {"type": "string"},
          "confidence":      {"type": "number"},
          "createTime":      {"type": "string"},
          "validFrom":       {"type": "string"},
          "validUntil":      {"type": "string"},
          "supersededByUid": {"type": "string"},
          "attributes":      {"type": "object", "additionalProperties": {"type": "string"}},
          "status":          {"type": "string", "enum": ["asserted", "proposed"], "description": "Trust posture of the edge. 'asserted' is authoritative (the default for your own writes + extraction). 'proposed' means it was extracted from an UNTRUSTED connector source (a meeting transcript or chat export): it ranks normally but is excluded from supersession until verified — promote it with 'maz promote relations/{uid}' (or the promoteRelation tool) to assert it."},
          "retracted":       {"type": "boolean", "description": "True when at least one relationRetracted tombstone targets this edge (via unrelate or resolver supersession). False (or absent) means any valid_until was set at creation (a born-bounded edge with no tombstone). Use this to distinguish a tombstone-close from a born-bounded validity window in timeline rendering."}
        },
        "required": ["name", "subject", "object", "uid", "subjectUid", "predicate", "objectUid", "confidence", "createTime", "status"],
        "additionalProperties": false
      }
    }
  },
  "required": ["relations"],
  "additionalProperties": false
}`

// Relations lists an entity's in/out edges, LLM-free. It is the structured
// pivot a recall anchor or get(entity) leads into when an agent wants the
// typed facts about an entity rather than a free-text episode.
type Relations struct {
	nav    Navigator
	binder selfBinder
}

// NewRelations constructs a Relations tool over nav.
func NewRelations(nav Navigator) *Relations { return &Relations{nav: nav} }

// SetSelfBinder injects the self->principal binder so a `self` entity arg
// resolves to the bound principal on read (mirrors relate/fact). Nil = none.
func (r *Relations) SetSelfBinder(b selfBinder) { r.binder = b }

// Name implements mcp.Tool.
func (*Relations) Name() string { return "relations" }

// Description implements mcp.Tool.
func (*Relations) Description() string {
	return "List an entity's typed relations (incoming + outgoing), each row tagged direction=outgoing|incoming relative to the queried entity so directional predicates (reports_to, manages) read unambiguously. Endpoint names are hydrated. LLM-free. The DEFAULT view is point-in-time NOW: only edges valid now and not superseded; pass includeInactive=true for expired/superseded history, or time-travel with at/asOf. Filter by predicate. Role/title is often carried as an attributes.role string on a works_at/founded edge rather than a separate has_role edge; each row's sourceEventUid links back to the originating episode. Pair with neighbors to walk further."
}

// InputSchema implements mcp.Tool.
func (*Relations) InputSchema() json.RawMessage { return json.RawMessage(relationsInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Relations) OutputSchema() json.RawMessage { return json.RawMessage(relationsOutputSchema) }

type relationsInput struct {
	Entity string   `json:"entity"`
	Scopes []string `json:"scopes,omitempty"`
	relationFilterInput
}

type relationsOutput struct {
	Relations []relationWire `json:"relations"`
}

// Invoke implements mcp.Tool.
func (r *Relations) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in relationsInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	// Validate cheap request syntax (time anchors) before the entity lookup,
	// so a malformed request fails fast without an entity-resolution round-trip.
	filter, err := in.toFilter()
	if err != nil {
		return nil, err
	}
	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}
	nav := navWithScopes(r.nav, scopeSet)
	uid, err := resolveEntityArg(ctx, nav, r.binder, in.Entity)
	if err != nil {
		return nil, err
	}

	rels, err := nav.EntityRelations(ctx, uid, filter)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("relations for %q: %w", uid, err))
	}

	body, err := json.Marshal(relationsOutput{Relations: projectRelations(uid, rels)})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}
