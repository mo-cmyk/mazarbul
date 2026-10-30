package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// introspectInputSchema takes no arguments: introspection is a snapshot of the
// store's navigable shape.
const introspectInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

// introspectOutputSchema documents the navigable ontology: the predicates an
// agent can filter/traverse by, the entity kinds present (with taxonomy
// metadata from the registered kind registry), the event types the store
// records, and the provenance namespaces active in the registry.
const introspectOutputSchema = `{
  "type": "object",
  "properties": {
    "predicates": {
      "type": "array",
      "description": "Registered relation predicates — the edge vocabulary for neighbors/relations filtering.",
      "items": {
        "type": "object",
        "properties": {
          "predicate":   {"type": "string"},
          "kind":        {"type": "string", "enum": ["functional", "non-functional"], "description": "functional = at most one live object per subject (a new value supersedes the old); non-functional = many may coexist."},
          "description": {"type": "string"},
          "subjectKinds": {"type": "array", "items": {"type": "string"}, "description": "Declared domain: entity kinds allowed as the subject. Absent = any kind (open-world)."},
          "objectKinds":  {"type": "array", "items": {"type": "string"}, "description": "Declared range: entity kinds allowed as the object. Absent = any kind (open-world)."},
          "sameAs":       {"type": "array", "items": {"type": "string"}, "description": "Alias IRIs to external vocabularies (schema.org, Wikidata, FOAF). Absent = none."}
        },
        "required": ["predicate", "kind"],
        "additionalProperties": false
      }
    },
    "entityKinds": {
      "type": "array",
      "description": "Entity kinds present in the store or registered in the taxonomy, with live counts and optional taxonomy metadata. Includes registered-but-zero-count kinds (e.g. core kinds not yet ingested). Custom kinds added at runtime appear in user or extension namespaces.",
      "items": {
        "type": "object",
        "properties": {
          "kind":           {"type": "string"},
          "count":          {"type": "integer", "minimum": 0},
          "displayName":    {"type": "string",  "description": "Human-readable label from the kind registry. Absent when the kind has data but is not in the registry."},
          "description":    {"type": "string",  "description": "One-sentence explanation from the kind registry. Absent when unregistered."},
          "provenance":     {"type": "string",  "description": "Namespace lane: core, user, or extension:<name>. Absent when unregistered."},
          "extends":        {"type": "string",  "description": "Parent kind in the subtype hierarchy. Absent for root kinds or unregistered kinds."},
          "sameAs":         {"type": "array", "items": {"type": "string"}, "description": "External ontology URIs this kind maps to (e.g. schema.org/Person). Absent when none registered."},
          "propertySchema": {"type": "object", "description": "JSON Schema for the kind's typed properties. Absent when the kind declares no typed properties.", "additionalProperties": true}
        },
        "required": ["kind", "count"],
        "additionalProperties": false
      }
    },
    "namespaces": {
      "type": "array",
      "description": "Provenance namespaces aggregated from the registered entity kinds: core (built-in), user (manually added), extension:<name> (plugin-contributed). Predicate provenance joins in a later phase.",
      "items": {
        "type": "object",
        "properties": {
          "namespace": {"type": "string",  "description": "Provenance namespace, e.g. core, user, extension:github."},
          "kindCount": {"type": "integer", "minimum": 0, "description": "Number of registered kinds in this namespace."}
        },
        "required": ["namespace", "kindCount"],
        "additionalProperties": false
      }
    },
    "eventTypes": {
      "type": "array",
      "description": "Registered event-schema kinds the store records. Empty when no ontology registry is wired.",
      "items": {
        "type": "object",
        "properties": {
          "kind":       {"type": "string"},
          "version":    {"type": "string"},
          "provenance": {"type": "string"}
        },
        "required": ["kind", "version"],
        "additionalProperties": false
      }
    },
    "views": {
      "type": "array",
      "description": "Curated read-only views for the query tool — the stable SQL schema. Query these names, not the physical tables.",
      "items": {
        "type": "object",
        "properties": {
          "name":        {"type": "string"},
          "description": {"type": "string"},
          "columns": {
            "type": "array",
            "items": {
              "type": "object",
              "properties": {
                "name":        {"type": "string"},
                "type":        {"type": "string"},
                "description": {"type": "string"}
              },
              "required": ["name", "type"],
              "additionalProperties": false
            }
          }
        },
        "required": ["name", "description", "columns"],
        "additionalProperties": false
      }
    }
  },
  "required": ["predicates", "entityKinds", "namespaces", "eventTypes", "views"],
  "additionalProperties": false
}`

// ontologyLister is the slice of kernel.Ontology the introspect tool needs:
// enumerate registered schemas. *kernel.MemoryOntology and the on-disk registry
// satisfy it; tests inject a stub.
type ontologyLister interface {
	List(ctx context.Context) ([]kernel.Schema, error)
}

// Introspect returns the ontology an agent navigates by — predicates, entity
// kinds, and event types — so it can construct correct neighbors/relations/
// entities calls without guessing the vocabulary. LLM-free.
type Introspect struct {
	nav Navigator
	ont ontologyLister // optional; nil → eventTypes empty
}

// NewIntrospect constructs an Introspect tool. Ont may be nil, in which case
// eventTypes is returned empty rather than failing.
func NewIntrospect(nav Navigator, ont ontologyLister) *Introspect {
	return &Introspect{nav: nav, ont: ont}
}

// Name implements mcp.Tool.
func (*Introspect) Name() string { return "introspect" }

// Description implements mcp.Tool.
func (*Introspect) Description() string {
	return "Return the ontology you navigate by: the registered relation predicates (edge vocabulary, with functional/non-functional semantics), the entity kinds present (node vocabulary with live counts, plus taxonomy metadata — displayName, description, provenance namespace, extends/parent hierarchy, sameAs external URIs — for kinds in the registry), a namespaces summary (core/user/extension:<name> buckets), and the event types recorded. Custom kinds added at runtime appear under the user or extension:<name> provenance namespace. Call once to learn the vocabulary before filtering neighbors/relations/entities. NOTE: this lists what has been registered or extracted, so it can include rarely- or never-populated predicates — e.g. a role/title is usually an attributes.role string on a works_at/founded edge rather than a populated has_role edge — so confirm a predicate is actually in use (via relations) before relying on it. LLM-free."
}

// InputSchema implements mcp.Tool.
func (*Introspect) InputSchema() json.RawMessage { return json.RawMessage(introspectInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Introspect) OutputSchema() json.RawMessage { return json.RawMessage(introspectOutputSchema) }

type predicateJSON struct {
	Predicate   string `json:"predicate"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	// SubjectKinds / ObjectKinds are the predicate's declared domain/range
	// (P5 typed relations): the entity kinds allowed on each end. Omitted when
	// the predicate accepts any kind (open-world default).
	SubjectKinds []string `json:"subjectKinds,omitempty"`
	ObjectKinds  []string `json:"objectKinds,omitempty"`
	// SameAs lists alias IRIs to external vocabularies. Omitted when none.
	SameAs []string `json:"sameAs,omitempty"`
}

// entityKindJSON is the enriched per-kind entry: required kind+count plus
// optional taxonomy fields. All optional fields use omitempty so old consumers
// that only read {kind,count} keep working unchanged.
type entityKindJSON struct {
	Kind           string          `json:"kind"`
	Count          int             `json:"count"`
	DisplayName    string          `json:"displayName,omitempty"`
	Description    string          `json:"description,omitempty"`
	Provenance     string          `json:"provenance,omitempty"`
	Extends        string          `json:"extends,omitempty"`
	SameAs         []string        `json:"sameAs,omitempty"`
	PropertySchema json.RawMessage `json:"propertySchema,omitempty"`
}

// namespaceJSON is one provenance namespace bucket aggregated from the
// registered entity kinds.
type namespaceJSON struct {
	Namespace string `json:"namespace"`
	KindCount int    `json:"kindCount"`
}

type eventTypeJSON struct {
	Kind       string `json:"kind"`
	Version    string `json:"version"`
	Provenance string `json:"provenance,omitempty"`
}

type viewColumnJSON struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

type viewJSON struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Columns     []viewColumnJSON `json:"columns"`
}

type introspectOutput struct {
	Predicates  []predicateJSON  `json:"predicates"`
	EntityKinds []entityKindJSON `json:"entityKinds"`
	Namespaces  []namespaceJSON  `json:"namespaces"`
	EventTypes  []eventTypeJSON  `json:"eventTypes"`
	Views       []viewJSON       `json:"views"`
}

// queryViews is the documented schema of the curated read-only views (migration
// 0017) that the query tool exposes. It is the agent-facing contract: introspect
// reports it verbatim so an agent writes correct SQL in one shot. The columns
// must match the views exactly; TestQueryViewCatalog_MatchesLiveViews validates
// every column against the live schema so this catalog cannot silently drift.
//
//nolint:gochecknoglobals // immutable schema catalog, package-private
var queryViews = []viewJSON{
	{
		Name:        "v_entities",
		Description: "One row per entity — the node vocabulary, with optional spatial coordinates.",
		Columns: []viewColumnJSON{
			{Name: "uid", Type: "TEXT", Description: "Entity UID; pass as entities/{uid} to the get tool."},
			{Name: "kind", Type: "TEXT", Description: "Entity kind, e.g. person, organization, place."},
			{Name: "display_name", Type: "TEXT"},
			{Name: "confidence", Type: "REAL", Description: "Resolution confidence, 0..1."},
			{Name: "resolution_status", Type: "TEXT"},
			{Name: "create_time", Type: "TEXT", Description: "RFC 3339 instant the entity was first recorded."},
			{Name: "latitude", Type: "REAL", Description: "Nullable; set only for placed entities."},
			{Name: "longitude", Type: "REAL", Description: "Nullable; set only for placed entities."},
			{Name: "properties", Type: "TEXT", Description: "Nullable JSON object of typed attributes (validated against the kind's propertySchema, written by createEntity); use json_extract to read keys."},
			{Name: "scope", Type: "TEXT", Description: "Write scope: project, personal, or team. Filter to isolate records by scope."},
		},
	},
	{
		Name:        "v_relations",
		Description: "One row per typed edge, with both endpoint display names hydrated.",
		Columns: []viewColumnJSON{
			{Name: "uid", Type: "TEXT"},
			{Name: "subject_uid", Type: "TEXT"},
			{Name: "subject_name", Type: "TEXT", Description: "Subject entity's display name (NULL if unresolved)."},
			{Name: "predicate", Type: "TEXT", Description: "Edge type, e.g. works_at, reports_to."},
			{Name: "object_uid", Type: "TEXT"},
			{Name: "object_name", Type: "TEXT", Description: "Object entity's display name (NULL if unresolved)."},
			{Name: "confidence", Type: "REAL"},
			{Name: "valid_from", Type: "TEXT", Description: "RFC 3339; NULL/'' if unbounded."},
			{Name: "valid_until", Type: "TEXT", Description: "RFC 3339; NULL/'' if still open."},
			{Name: "attributes", Type: "TEXT", Description: "JSON qualifier object; read keys with json_extract(attributes, '$.role')."},
			{Name: "active", Type: "INTEGER", Description: "1 when the edge is live now (not superseded and no valid_until), else 0."},
			{Name: "scope", Type: "TEXT", Description: "Write scope: project, personal, or team. Filter to isolate records by scope."},
		},
	},
	{
		Name:        "v_facts",
		Description: "An entity's currently-open scalar facts — one row per live slot. Closed/superseded history is excluded.",
		Columns: []viewColumnJSON{
			{Name: "uid", Type: "TEXT"},
			{Name: "subject_uid", Type: "TEXT"},
			{Name: "subject_name", Type: "TEXT", Description: "Subject entity's display name (NULL if unresolved)."},
			{Name: "state_key", Type: "TEXT", Description: "Hierarchical slot address, e.g. state.task.status."},
			{Name: "value", Type: "TEXT"},
			{Name: "event_start", Type: "TEXT", Description: "RFC 3339; NULL means create_time."},
			{Name: "create_time", Type: "TEXT"},
			{Name: "source_priority", Type: "REAL", Description: "Recall-lane priority, 0..1."},
			{Name: "scope", Type: "TEXT", Description: "Write scope: project, personal, or team. Filter to isolate records by scope."},
		},
	},
}

// Invoke implements mcp.Tool. Each section degrades independently: a missing
// ontology registry yields an empty eventTypes array, not an error. A failing
// RegisteredEntityKinds call is logged and the enrichment fields are omitted
// (count-only shape) rather than erroring the whole call.
func (i *Introspect) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	preds, err := i.nav.Predicates(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("predicates: %w", err))
	}
	kinds, err := i.nav.EntityKinds(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("entity kinds: %w", err))
	}
	events, err := i.eventTypes(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("event types: %w", err))
	}

	// Enrich entity kinds with taxonomy metadata; degrade gracefully on failure.
	kindItems, namespaces := i.enrichKinds(ctx, kinds)

	out := introspectOutput{
		Predicates:  make([]predicateJSON, 0, len(preds)),
		EntityKinds: kindItems,
		Namespaces:  namespaces,
		EventTypes:  events,
		Views:       queryViews,
	}
	for _, p := range preds {
		out.Predicates = append(out.Predicates, predicateJSON{
			Predicate:    p.Predicate,
			Kind:         p.Kind,
			Description:  p.Description,
			SubjectKinds: p.SubjectKinds,
			ObjectKinds:  p.ObjectKinds,
			SameAs:       p.SameAs,
		})
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// enrichKinds joins live counts from EntityKinds with taxonomy rows from
// RegisteredEntityKinds. It includes registered-but-zero-count kinds and
// handles kinds present in data but missing from the registry (no enrichment
// fields). On registry failure it logs and returns the count-only shape.
func (i *Introspect) enrichKinds(ctx context.Context, counts []navigate.KindCount) ([]entityKindJSON, []namespaceJSON) {
	// Build a count index: kind → count.
	countMap := make(map[string]int, len(counts))
	for _, kc := range counts {
		countMap[kc.Kind] = kc.Count
	}

	// Attempt to load the registry; degrade on failure.
	registered, err := i.nav.RegisteredEntityKinds(ctx)
	if err != nil {
		logger := logging.FromContext(ctx)
		logger.Warn("introspect: registered entity kinds unavailable, returning count-only shape", zap.Error(err))
		// Fall back to count-only shape: one entry per observed kind.
		items := make([]entityKindJSON, 0, len(counts))
		for _, kc := range counts {
			items = append(items, entityKindJSON{Kind: kc.Kind, Count: kc.Count})
		}
		return items, []namespaceJSON{}
	}

	// Build enriched items: one entry per registered kind (with live count,
	// possibly 0) plus one entry per observed kind not in the registry.
	seen := make(map[string]bool, len(registered))
	items := make([]entityKindJSON, 0, len(registered)+len(counts))
	nsCounts := make(map[string]int)

	for _, rk := range registered {
		seen[rk.Name] = true
		item := entityKindJSON{
			Kind:           rk.Name,
			Count:          countMap[rk.Name], // 0 when not yet ingested
			DisplayName:    rk.DisplayName,
			Description:    rk.Description,
			Provenance:     rk.Provenance,
			Extends:        rk.Parent,
			SameAs:         rk.SameAs,
			PropertySchema: rk.PropertySchema,
		}
		items = append(items, item)
		if rk.Provenance != "" {
			nsCounts[rk.Provenance]++
		}
	}

	// Append observed kinds that have no registry entry.
	for _, kc := range counts {
		if !seen[kc.Kind] {
			items = append(items, entityKindJSON{Kind: kc.Kind, Count: kc.Count})
		}
	}

	// Build namespace summary, stable order: core first, then user, then
	// extension:* alphabetically.
	namespaces := buildNamespaceSummary(nsCounts)
	return items, namespaces
}

// Provenance namespace constants used for ordering the namespaces summary.
const (
	nsCore = "core"
	nsUser = "user"
)

// buildNamespaceSummary converts a namespace→count map to a sorted slice:
// "core" first, "user" second, then extension:* in lexicographic order.
func buildNamespaceSummary(nsCounts map[string]int) []namespaceJSON {
	if len(nsCounts) == 0 {
		return []namespaceJSON{}
	}
	// Collect and sort: core < user < extension:*
	order := func(ns string) int {
		switch ns {
		case nsCore:
			return 0
		case nsUser:
			return 1
		default:
			return 2
		}
	}
	// Gather unique keys.
	keys := make([]string, 0, len(nsCounts))
	for k := range nsCounts {
		keys = append(keys, k)
	}
	// Stable sort: primary by tier, secondary lexicographic.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0; j-- {
			a, b := keys[j-1], keys[j]
			oa, ob := order(a), order(b)
			if oa > ob || (oa == ob && a > b) {
				keys[j-1], keys[j] = keys[j], keys[j-1]
			}
		}
	}
	out := make([]namespaceJSON, 0, len(keys))
	for _, k := range keys {
		out = append(out, namespaceJSON{Namespace: k, KindCount: nsCounts[k]})
	}
	return out
}

// eventTypes lists the registered event-schema kinds, or an empty (non-nil)
// slice when no ontology registry is wired.
func (i *Introspect) eventTypes(ctx context.Context) ([]eventTypeJSON, error) {
	if i.ont == nil {
		return []eventTypeJSON{}, nil
	}
	schemas, err := i.ont.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]eventTypeJSON, 0, len(schemas))
	for _, s := range schemas {
		out = append(out, eventTypeJSON{Kind: s.Kind, Version: s.Version, Provenance: s.Provenance})
	}
	return out, nil
}
