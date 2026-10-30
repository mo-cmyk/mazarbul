package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// countDefaultGroupLimit caps a group-by breakdown when the caller omits limit.
const countDefaultGroupLimit = 50

// countInputSchema is the JSON Schema for the count tool. Filter/kind narrow the
// population; groupBy switches from a single total to a per-value breakdown.
const countInputSchema = `{
  "type": "object",
  "properties": {
    "filter":  {"type": "string", "description": "AIP-160 filter expression selecting which entities to count. Same grammar as the entities tool: reference entity columns (kind, displayName, confidence, resolutionStatus, createTime), fact slots by state_key (e.g. status = \"pending\"), and related(\"predicate\", \"target\"). Operators = != < > <= >= : with AND / OR / NOT."},
    "kind":    {"type": "string", "description": "Convenience prefilter to one entity kind, AND-combined with filter."},
    "groupBy": {"type": "string", "description": "When set, return a per-value breakdown counted by this field instead of a single total. The field is an entity column (kind, resolutionStatus, …) or a fact slot state_key (status, …); entities with no open fact for that slot bucket under the empty value."},
    "limit":   {"type": "integer", "minimum": 1, "maximum": 200, "description": "Max groups to return when groupBy is set, most-populous first. Defaults to 50. Ignored without groupBy."},
    "scopes":  {"type": "array", "items": {"type": "string"}, "description": "Scope filter: which data partitions to count. Allowed values: project, personal, team. Defaults to [project, personal] when omitted — team entities are hidden unless you pass scopes:[\"team\"]."}
  },
  "additionalProperties": false
}`

// countOutputSchema documents the response: count XOR groups depending on
// whether groupBy was supplied.
const countOutputSchema = `{
  "type": "object",
  "properties": {
    "count":  {"type": "integer", "minimum": 0, "description": "Total matching entities. Present when groupBy is not set."},
    "groups": {
      "type": "array",
      "description": "Per-value breakdown, most-populous first. Present when groupBy is set.",
      "items": {
        "type": "object",
        "properties": {
          "value": {"type": "string", "description": "The group value; empty string for entities lacking the grouped field."},
          "count": {"type": "integer", "minimum": 0}
        },
        "required": ["value", "count"],
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}`

// Count answers "how many …" deterministically: it counts the entities matching
// an AIP-160 filter, or — with groupBy — returns a per-value breakdown. It is
// the structured counterpart to the entities listing: same filter grammar, an
// integer (or histogram) instead of a page of rows.
type Count struct {
	nav Navigator
}

// NewCount constructs a Count tool over the navigator.
func NewCount(nav Navigator) *Count { return &Count{nav: nav} }

// Name implements mcp.Tool.
func (*Count) Name() string { return "count" }

// Description implements mcp.Tool.
func (*Count) Description() string {
	return "Count entities matching an AIP-160 filter — the deterministic answer to \"how many …\". With groupBy, returns a per-value breakdown (e.g. people per status) instead of a single total. Same filter grammar as the entities tool. Scope: defaults to project+personal; pass scopes:[\"team\"] to include team entities."
}

// InputSchema implements mcp.Tool.
func (*Count) InputSchema() json.RawMessage { return json.RawMessage(countInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Count) OutputSchema() json.RawMessage { return json.RawMessage(countOutputSchema) }

type countInput struct {
	Filter  string   `json:"filter,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	GroupBy string   `json:"groupBy,omitempty"`
	Limit   *int     `json:"limit,omitempty"`
	Scopes  []string `json:"scopes,omitempty"`
}

type countGroup struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// countOutput carries the total XOR the breakdown. Pointer count distinguishes
// "zero matches" from "grouped, so total omitted"; both omitempty so exactly one
// field serializes.
type countOutput struct {
	Count  *int64       `json:"count,omitempty"`
	Groups []countGroup `json:"groups,omitempty"`
}

// Invoke implements mcp.Tool.
func (c *Count) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in countInput
	if len(req) > 0 {
		if err := json.Unmarshal(req, &in); err != nil {
			return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
		}
	}
	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}
	scoped := navWithScopes(c.nav, scopeSet)

	var out countOutput
	if in.GroupBy != "" {
		groups, err := c.aggregate(ctx, in, scoped)
		if err != nil {
			return nil, err
		}
		out.Groups = groups
	} else {
		n, err := scoped.CountEntities(ctx, navigate.EntityFilter{Kind: in.Kind, Filter: in.Filter})
		if err != nil {
			return nil, navQueryError("count entities", err)
		}
		out.Count = &n
	}

	body, mErr := json.Marshal(out)
	if mErr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", mErr))
	}
	return body, nil
}

// aggregate runs the group-by path using nav (already scope-narrowed by Invoke)
// and clamps limit to the schema ceiling for direct (CLI / in-process) callers
// that bypass the MCP schema validator.
func (c *Count) aggregate(ctx context.Context, in countInput, nav Navigator) ([]countGroup, error) {
	limit := countDefaultGroupLimit
	if in.Limit != nil && *in.Limit > 0 {
		limit = *in.Limit
		if limit > maxNavigateLimit {
			limit = maxNavigateLimit
		}
	}
	groups, err := nav.AggregateEntities(ctx, navigate.AggregateSpec{
		Kind:    in.Kind,
		Filter:  in.Filter,
		GroupBy: in.GroupBy,
		Limit:   limit,
	})
	if err != nil {
		return nil, navQueryError("aggregate entities", err)
	}
	out := make([]countGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, countGroup{Value: g.Value, Count: g.Count})
	}
	return out, nil
}
