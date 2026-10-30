package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

const describeTypeInputSchema = `{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "minLength": 1,
      "description": "Entity kind to describe, e.g. person or user:vehicle. A bare local name (vehicle) also resolves to the user: kind when unambiguous."
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`

const describeTypeOutputSchema = `{
  "type": "object",
  "properties": {
    "name":           {"type": "string", "description": "Canonical kind name, e.g. user:vehicle."},
    "displayName":    {"type": "string", "description": "Human-readable label."},
    "description":    {"type": "string", "description": "One-sentence explanation of the kind."},
    "parent":         {"type": "string", "description": "Parent kind in the subtype hierarchy (extends). Absent for root kinds."},
    "children":       {"type": "array", "items": {"type": "string"}, "description": "Registered kinds that extend this one."},
    "sameAs":         {"type": "array", "items": {"type": "string"}, "description": "Alias IRIs to external vocabularies (schema.org, Wikidata, FOAF, …)."},
    "provenance":     {"type": "string", "description": "Namespace lane: core, user, or extension:<name>."},
    "addedBy":        {"type": "string", "description": "Who registered the kind: seed, llm, user, or extension."},
    "usageCount":     {"type": "integer", "description": "Number of entities currently carrying this kind."},
    "propertySchema": {"type": "object", "description": "JSON Schema for the kind's typed properties (attr_schema). Present only when the kind declares typed properties; use this to validate or populate the properties field in createEntity.", "additionalProperties": true}
  },
  "required": ["name", "displayName", "description", "provenance", "addedBy", "usageCount"],
  "additionalProperties": false
}`

// typeDescriber is the read-side dependency DescribeType consumes, narrowed from
// Navigator (the way relate.go narrows to entityResolver) so the tool's
// definition + usage-count lookup is all the surface a test must fake.
// *navigate.Navigator satisfies it when wired through the daemon.
type typeDescriber interface {
	RegisteredEntityKinds(ctx context.Context) ([]navigate.RegisteredEntityKind, error)
	EntityKinds(ctx context.Context) ([]navigate.KindCount, error)
}

// DescribeType is the rich single-type discovery tool: given one entity kind it
// returns the kind's definition (display name, description, parent, children,
// external aliases, provenance) plus how many entities currently carry it. It
// complements the cheap introspect index — reach for describeType when you have
// a candidate kind and want its full shape before createEntity or defineEntityKind.
type DescribeType struct {
	nav typeDescriber
}

// NewDescribeType constructs a DescribeType tool over nav.
func NewDescribeType(nav typeDescriber) *DescribeType {
	return &DescribeType{nav: nav}
}

// Name implements mcp.Tool.
func (*DescribeType) Name() string { return "describeType" }

// Description implements mcp.Tool.
func (*DescribeType) Description() string {
	return "Describe a single entity kind in full: displayName, description, parent (extends), child kinds, external-vocabulary aliases (sameAs), provenance lane, and how many entities currently carry it. The deep counterpart to introspect's cheap kind index — call it to confirm a kind's shape and reuse before minting a near-duplicate with defineEntityKind, or before createEntity. Accepts a canonical name (person, user:vehicle) or a bare local name (vehicle) that resolves to the user: kind when unambiguous. Returns E_NOT_FOUND with near-name suggestions when the kind is not registered."
}

// InputSchema implements mcp.Tool.
func (*DescribeType) InputSchema() json.RawMessage { return json.RawMessage(describeTypeInputSchema) }

// OutputSchema implements mcp.Tool.
func (*DescribeType) OutputSchema() json.RawMessage { return json.RawMessage(describeTypeOutputSchema) }

type describeTypeInput struct {
	Name string `json:"name"`
}

type describeTypeOutput struct {
	Name           string          `json:"name"`
	DisplayName    string          `json:"displayName"`
	Description    string          `json:"description"`
	Parent         string          `json:"parent,omitempty"`
	Children       []string        `json:"children,omitempty"`
	SameAs         []string        `json:"sameAs,omitempty"`
	Provenance     string          `json:"provenance"`
	AddedBy        string          `json:"addedBy"`
	UsageCount     int             `json:"usageCount"`
	PropertySchema json.RawMessage `json:"propertySchema,omitempty"`
}

// Invoke implements mcp.Tool.
func (d *DescribeType) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in describeTypeInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, mcp.NewValidationError("name is required (e.g. person or user:vehicle)", nil)
	}

	kinds, err := d.nav.RegisteredEntityKinds(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("list registered entity kinds: %w", err))
	}

	match, ambiguous := resolveKindByName(kinds, in.Name)
	if match.Name == "" {
		if len(ambiguous) > 1 {
			return nil, mcp.NewValidationError(
				fmt.Sprintf("%q is ambiguous across namespaces: %s; pass the full kind name", in.Name, kindNames(ambiguous)), nil)
		}
		return nil, mcp.NewNotFoundError(
			fmt.Sprintf("no entity kind named %q; %s", in.Name, kindSuggestion(kinds, in.Name)))
	}

	out := describeTypeOutput{
		Name:           match.Name,
		DisplayName:    match.DisplayName,
		Description:    match.Description,
		Parent:         match.Parent,
		SameAs:         match.SameAs,
		Provenance:     match.Provenance,
		AddedBy:        match.AddedBy,
		Children:       childKinds(kinds, match.Name),
		UsageCount:     usageCount(ctx, d.nav, match.Name),
		PropertySchema: match.PropertySchema,
	}
	return marshalOutput(out)
}

// resolveKindByName resolves the argument to a single registered kind. An exact
// full-name match (person, user:vehicle) wins outright. Otherwise the argument
// is treated as a bare local name and matched against each kind's local segment
// (the part after the last colon): a single local match resolves; zero or more
// than one returns an empty match plus the candidate set, so the caller can
// distinguish not-found from an ambiguous bare name spanning namespaces.
func resolveKindByName(kinds []navigate.RegisteredEntityKind, name string) (navigate.RegisteredEntityKind, []navigate.RegisteredEntityKind) {
	for _, k := range kinds {
		if k.Name == name {
			return k, nil
		}
	}
	var locals []navigate.RegisteredEntityKind
	for _, k := range kinds {
		if localSegment(k.Name) == name {
			locals = append(locals, k)
		}
	}
	if len(locals) == 1 {
		return locals[0], nil
	}
	return navigate.RegisteredEntityKind{}, locals
}

// localSegment returns the local name of a (possibly namespaced) kind: the
// substring after the last colon, or the whole string when un-namespaced.
func localSegment(name string) string {
	if i := strings.LastIndex(name, ":"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// kindNames renders the candidate kinds' full names for a disambiguation hint.
func kindNames(kinds []navigate.RegisteredEntityKind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, k.Name)
	}
	return strings.Join(names, ", ")
}

// childKinds returns the names of every registered kind whose parent is name,
// alphabetically.
func childKinds(kinds []navigate.RegisteredEntityKind, name string) []string {
	var children []string
	for _, k := range kinds {
		if k.Parent == name {
			children = append(children, k.Name)
		}
	}
	sort.Strings(children)
	return children
}

// usageCount returns how many entities carry kind name, from the live
// EntityKinds GROUP BY. A listing error degrades to 0 rather than failing the
// whole describe call — the count is informational.
func usageCount(ctx context.Context, nav typeDescriber, name string) int {
	counts, err := nav.EntityKinds(ctx)
	if err != nil {
		return 0
	}
	for _, c := range counts {
		if c.Kind == name {
			return c.Count
		}
	}
	return 0
}

// kindSuggestion offers a "did you mean" hint: registered kinds whose name
// contains the queried token, or a pointer to introspect when nothing is close.
func kindSuggestion(kinds []navigate.RegisteredEntityKind, name string) string {
	const maxShown = 5
	lower := strings.ToLower(name)
	var near []string
	for _, k := range kinds {
		if strings.Contains(strings.ToLower(k.Name), lower) {
			near = append(near, k.Name)
			if len(near) == maxShown {
				break
			}
		}
	}
	if len(near) == 0 {
		return "list registered kinds with introspect, or register it with defineEntityKind"
	}
	return "did you mean: " + strings.Join(near, ", ") + "?"
}
