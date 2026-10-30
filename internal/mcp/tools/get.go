package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// getInputSchema is the JSON Schema for the tools/call arguments. The single
// `name` is an AIP resource name; its collection prefix routes the lookup, so
// one verb fetches any record kind returned by recall. The pattern is permissive
// on the uid because entity uids include the non-ULID "self" sentinel.
const getInputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^[^/]+(/[^/]+)?$", "description": "What to fetch: an AIP resource name whose collection prefix routes the lookup (entities/{uid}, episodes/{uid}, relations/{uid}, facts/{uid}, insights/{uid}, or consolidatedMemories/{uid}) — feed one straight from a recall hit's name field — OR, for an entity, a bare uid or a human display name (resolved case-insensitively, like relations/neighbors). A non-entity record must use its collection prefix."},
    "kind":   {"type": "string", "description": "Optional entity kind hint (e.g. 'document', 'person', 'product'). When supplied alongside a display name, narrows the resolution to entities of that kind only. Useful when a name is shared between a document and a domain entity and you want the document specifically. Ignored for resource-name lookups (entities/{uid}) and bare uid lookups."},
    "scopes": {"type": "array", "items": {"type": "string"}, "description": "Scopes to read from: project, personal, team. Defaults to project+personal. Unknown values are rejected."},
    "at":              {"type": "string", "format": "date-time", "description": "Valid-time anchor (RFC 3339 UTC) for an entity record's fact rows: return each slot's value live at this instant. Defaults to now. Does not move the entity identity row; ignored for non-entity records."},
    "asOf":            {"type": "string", "format": "date-time", "description": "Transaction-time anchor (RFC 3339 UTC) for an entity record's fact rows: hide facts recorded after this instant. Ignored for non-entity records."},
    "includeInactive": {"type": "boolean", "description": "Include superseded / expired facts (the full slot history) on an entity record. Off by default. Ignored for non-entity records."}
  },
  "required": ["name"],
  "additionalProperties": false
}`

// getOutputSchema documents the discriminated-union response. `type` names
// which record kind is present; exactly one non-nil record arm is populated.
const getOutputSchema = `{
  "type": "object",
  "properties": {
    "type":               {"type": "string", "enum": ["entity", "episode", "relation", "fact", "insight", "consolidatedMemory"], "description": "Which record kind this response carries."},
    "entity":             {"type": "object", "description": "Present when type=entity. The canonical node: uid, kind, displayName, confidence, resolutionStatus, and (when createEntity stored them) a 'properties' object of typed attributes validated against the kind's propertySchema. Its scalar facts (status, first name, preferences) arrive in the sibling 'facts' array; its external identifiers (hrb, email, …) in the sibling 'identifiers' array; its edges (who it connects to) live on relations — call relations or neighbors for those."},
    "episode":            {"type": "object", "description": "Present when type=episode."},
    "relation":           {"type": "object", "description": "Present when type=relation. Carries subject/object resource names for onward traversal, plus a 'status' trust posture: 'asserted' (authoritative) or 'proposed' (extracted from an untrusted connector source — excluded from supersession until promoted via 'maz promote' / the promoteRelation tool)."},
    "fact":               {"type": "object", "description": "Present when type=fact. A scalar state slot: subjectUid, stateKey, value, validity window."},
    "insight":            {"type": "object", "description": "Present when type=insight. A synthesized reflection: statement, insightType, currentConfidence, state (hot|demoted), synthesizedAt, and sourceUids typed as entities/{uid} citations."},
    "consolidatedMemory": {"type": "object", "description": "Present when type=consolidatedMemory. A compressed episode summary: summary, memoryType, confidence, consolidatedAt, and sourceUids typed as episodes/{uid} citations."},
    "facts": {
      "type": "array",
      "description": "Present when type=entity: the entity's currently-open scalar facts (its attributes), newest slot value per state_key. Empty when the entity has none.",
      "items": {"type": "object"}
    },
    "identifiers": {
      "type": "array",
      "description": "Present when type=entity and the entity has external identifiers: the strong/weak identifiers (type + value, e.g. {type:\"hrb\",value:\"HRB 12345 B\"}) that anchor it for the resolver. Omitted when none.",
      "items": {
        "type": "object",
        "properties": {
          "type":  {"type": "string", "description": "Identifier type, e.g. hrb, lei, email, alias."},
          "value": {"type": "string", "description": "Identifier value."}
        },
        "required": ["type", "value"]
      }
    },
    "provenance": {
      "type": "array",
      "description": "Source/confidence/decided-at lineage walked back to the record's origin episode(s): direct for an episode, via source_event_uid for a relation, aggregated across citing episodes for an entity. Empty when none recorded.",
      "items": {
        "type": "object",
        "properties": {
          "eventUid":   {"type": "string", "description": "Origin episode event UID. Pass to get(episodes/{uid}) to read the source."},
          "source":     {"type": "string", "description": "Where the origin event came from, e.g. cli:remember, connector:gmail."},
          "confidence": {"type": "number", "description": "Writer confidence in the origin event, 0..1."},
          "decidedAt":  {"type": "string", "description": "When the provenance was recorded (RFC 3339)."}
        },
        "required": ["eventUid", "source", "confidence"]
      }
    },
    "shadowedDocuments": {
      "type": "array",
      "description": "Present when the name argument was resolved by display name AND one or more document entities share that name but were passed over in favour of a higher-priority non-document entity. Each entry gives the document's entities/{uid} resource name (ready to pass to get) and its bare uid. Omitted when the lookup was by resource name or uid, or when no document shares the winner's display name.",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "description": "entities/{uid} resource name of the shadowed document — pass directly to get to fetch it."},
          "uid":  {"type": "string", "description": "Document entity UID."}
        },
        "required": ["name", "uid"]
      }
    }
  },
  "required": ["type"],
  "additionalProperties": false
}`

// Get is the LLM-free fetch-by-resource-name tool. It consolidates
// get-entity/get-episode/get-relation behind one verb, routing on the resource
// name's collection prefix and returning a fully hydrated record.
type Get struct {
	nav    Navigator
	binder selfBinder
	// factNav, when non-nil, is the scope-narrowed navigator used to fetch an
	// entity's facts, independent of g.nav which fetches the node itself. They
	// differ only for a self-identity read (see Invoke): the reader's own
	// principal node always resolves (its home scope, personal, is the
	// device-local private scope — always the reader's own, never a cross-scope
	// leak), while its facts stay filtered to the requested scope so
	// `fact get self --scope team` returns exactly the team-shared facts. Nil →
	// facts use g.nav (the common case, where node and facts share one scope).
	factNav Navigator
	// factFilter is the bi-temporal anchor (at / asOf / includeInactive) narrowing
	// an ENTITY record's fact rows in fetchEntity. The zero value returns each
	// slot's current open value, matching the pre-anchor behavior. It deliberately
	// never touches the entity identity row, its provenance, or its identifiers —
	// `get "X" --at <t>` returns the same node, only its facts reflect the anchor.
	factFilter navigate.FactFilter
}

// NewGet constructs a Get tool over nav.
func NewGet(nav Navigator) *Get { return &Get{nav: nav} }

// SetSelfBinder injects the self->principal binder so get(entities/self)
// resolves the bound principal after binding (mirrors relate/fact). Nil = none.
func (g *Get) SetSelfBinder(b selfBinder) { g.binder = b }

// Name implements mcp.Tool.
func (*Get) Name() string { return "get" }

// Description implements mcp.Tool.
func (*Get) Description() string {
	return "Fetch one record by AIP resource name — entities/{uid}, episodes/{uid}, relations/{uid}, facts/{uid}, insights/{uid}, or consolidatedMemories/{uid} — plus its provenance lineage. For an entity you may also pass a bare uid or a human display name (resolved case-insensitively, the same forms relations/neighbors accept). When a name is shared across kinds, a non-document entity deterministically wins over a same-named document — the passed-over document(s) are surfaced in `shadowedDocuments` as entities/{uid} you can fetch directly; pass `kind` to target a specific kind, or a vault path / entities/{uid} to address a document. A name that is ambiguous among non-document entities (or unknown) returns a pointed error naming the candidates. The structured, LLM-free way to follow any recall hit's name field. Returns a discriminated union tagged by `type`. An entity record returns the canonical node (uid, kind, displayName, confidence, resolutionStatus) AND its typed `properties`, external `identifiers`, and currently-open scalar `facts`. A relation record carries hydrated endpoints + validity. An episode carries its source text. An insight (type=insight) returns the synthesized statement, insightType, currentConfidence, state, synthesizedAt, and its sourceUids as entities/{uid} citations. A consolidated memory (type=consolidatedMemory) returns the compressed summary, memoryType, confidence, consolidatedAt, and its sourceUids as episodes/{uid} citations. Demoted insights (state='demoted') are still fetchable — get is not the hot-recall index."
}

// InputSchema implements mcp.Tool.
func (*Get) InputSchema() json.RawMessage { return json.RawMessage(getInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Get) OutputSchema() json.RawMessage { return json.RawMessage(getOutputSchema) }

type getInput struct {
	Name            string   `json:"name"`
	Kind            string   `json:"kind,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
	At              *string  `json:"at,omitempty"`
	AsOf            *string  `json:"asOf,omitempty"`
	IncludeInactive *bool    `json:"includeInactive,omitempty"`
}

// toFactFilter projects the bi-temporal knobs into a navigate.FactFilter, reusing
// the shared relationFilterInput time-anchor parsing so a malformed 'at' / 'asOf'
// is the same E_VALIDATION the relations/neighbors tools return. The anchor
// narrows only an entity record's fact rows (fetchEntity), never the identity row.
func (in getInput) toFactFilter() (navigate.FactFilter, error) {
	rf := relationFilterInput{At: in.At, AsOf: in.AsOf, IncludeInactive: in.IncludeInactive}
	f, err := rf.toFilter()
	if err != nil {
		return navigate.FactFilter{}, err
	}
	return navigate.FactFilter{At: f.At, AsOf: f.AsOf, IncludeInactive: f.IncludeInactive}, nil
}

// getOutput is the discriminated union. Exactly one record pointer is non-nil,
// selected by Type. Pointers + omitempty keep the absent arms off the wire.
//
// Facts is a *pointer* to a slice so the entity arm can advertise
// present-and-empty ("facts": []) while every other arm omits the key
// entirely. A bare []navigate.Fact + omitempty would drop the field for an
// entity with zero open facts, leaving clients unable to distinguish "server
// supports the facts fold-in, this entity has none" from an old server that
// never emits facts at all. With a pointer: nil → field omitted (episode /
// relation / fact arms); non-nil pointer to an empty slice → "facts": []
// (entity arm), without ever emitting "facts": null.
type getOutput struct {
	Type               string                      `json:"type"`
	Entity             *entityWire                 `json:"entity,omitempty"`
	Episode            *episodeWire                `json:"episode,omitempty"`
	Relation           *relationWire               `json:"relation,omitempty"`
	Fact               *navigate.Fact              `json:"fact,omitempty"`
	Insight            *insightWire                `json:"insight,omitempty"`
	ConsolidatedMemory *consolidatedMemoryWire     `json:"consolidatedMemory,omitempty"`
	Facts              *[]navigate.Fact            `json:"facts,omitempty"`
	Identifiers        []navigate.EntityIdentifier `json:"identifiers,omitempty"`
	Provenance         []navigate.Provenance       `json:"provenance,omitempty"`
	// ShadowedDocuments is populated when the name argument resolved by display
	// name and one or more document entities share that name but were passed over
	// in favour of a higher-priority non-document winner. Each entry carries the
	// document's entities/{uid} resource name so the agent can call get(name) to
	// fetch it directly. Absent (omitted) for resource-name/uid lookups and when
	// no document shares the winner's display name.
	ShadowedDocuments []shadowedDocRef `json:"shadowedDocuments,omitempty"`
}

// insightWire is a navigate.Insight with its source citations typed as
// entities/{uid} resource names (insight sources are entities seeded from a
// reflection pass over the entity graph).
type insightWire struct {
	navigate.Insight
	// SourceCitations replaces the raw SourceUIDs with typed entities/{uid}
	// names so agents can pipe them straight into get/neighbors/relations.
	SourceCitations []string `json:"sourceCitations,omitempty"`
}

// consolidatedMemoryWire is a navigate.ConsolidatedMemory with its source
// citations typed as episodes/{uid} resource names (consolidated-memory
// sources are the episodes that were compressed into the summary).
type consolidatedMemoryWire struct {
	navigate.ConsolidatedMemory
	// SourceCitations replaces the raw SourceUIDs with typed episodes/{uid}
	// names so agents can pipe them straight into get(episodes/{uid}).
	SourceCitations []string `json:"sourceCitations,omitempty"`
}

// Invoke implements mcp.Tool. A malformed name is E_VALIDATION; a well-formed
// name whose uid does not exist is E_NOT_FOUND.
func (g *Get) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in getInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}

	// Validate the bi-temporal anchor up front (fail fast on a bad timestamp,
	// before the entity-resolution round-trip) — mirrors the relations tool.
	factFilter, ferr := in.toFactFilter()
	if ferr != nil {
		return nil, ferr
	}

	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}

	var collection, uid string
	var shadowedDocs []shadowedDocRef
	var err error
	if looksLikeAIPResourceName(in.Name) {
		// Resource name (entities/{uid}, episodes/{uid}, …): the collection prefix
		// routes the lookup, so a single get fetches any record kind. A slashed
		// display name (prefix is not a known collection, e.g. "Ring Mk 4000 / RFC
		// 1149") is NOT a resource name and resolves as an entity name below.
		collection, uid, err = parseResourceName(in.Name)
		if err != nil {
			return nil, err
		}
	} else {
		// Bare token — a uid or a human display name. Resolve it as an entity so
		// `get "Mira Stonebrook"` and `get <uid>` work like show/relations/
		// neighbors; get was the lone read verb that demanded entities/{uid}, which
		// every usability pass flagged as the surprising odd-one-out. Non-entity
		// records (episodes/relations/facts/…) have no display name and so still
		// require their collection prefix; resolveEntityArg also handles `self`.
		// resolveEntityArgKindShadow is used (instead of resolveEntityArgKind) so
		// that a display-name hit can carry the shadowed-document signal: when a
		// non-document entity wins over a same-named document, shadowedDocs is
		// populated and threaded into the output for the agent's awareness.
		uid, shadowedDocs, err = resolveEntityArgKindShadow(ctx, navWithScopes(g.nav, scopeSet), g.binder, in.Name, in.Kind)
		if err != nil {
			return nil, err
		}
		collection = collectionEntities
	}
	// Self-identity reads always resolve the reader's own principal node, even
	// when the requested scope set excludes the scope the principal lives in
	// (personal). `fact get self --scope team` must surface the reader's
	// team-shared facts, but the principal node is personal-scoped, so widen JUST
	// the node lookup to include personal — facts stay filtered to the requested
	// scope (factNav). Gated on the entity being the reader's own principal, so
	// no other entity's resolution widens and the fail-closed boundary holds.
	nodeScopes := scopeSet
	if collection == collectionEntities && !scopeSet.Contains(kernel.ScopePersonal) &&
		isOwnPrincipalUID(ctx, g.binder, rebindSelf(ctx, g.binder, uid)) {
		nodeScopes = kernel.NewScopeSet(append(scopeSet.Scopes(), kernel.ScopePersonal)...)
	}
	n := navWithScopes(g.nav, nodeScopes)
	factNav := navWithScopes(g.nav, scopeSet)
	out, err := (&Get{nav: n, binder: g.binder, factNav: factNav, factFilter: factFilter}).fetch(ctx, collection, uid)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return nil, mcp.NewNotFoundError(in.Name + " not found")
		}
		return nil, mcp.NewInternalError(fmt.Errorf("get %s: %w", in.Name, err))
	}
	// Annotate with the shadow signal gathered during display-name resolution.
	// Non-nil only when a bare display name resolved to a non-document winner
	// while one or more same-named documents exist; nil for resource-name/uid
	// lookups (where shadowedDocs was never set).
	out.ShadowedDocuments = shadowedDocs

	body, err := json.Marshal(out)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// fetch routes a parsed resource name to the matching Navigator getter,
// projects the result into the discriminated-union wire shape, and hydrates the
// record's provenance. Provenance is walked back to the origin episode(s):
// directly for an episode, via source_event_uid for a relation, and aggregated
// across citing episodes for an entity (entities have no provenance row of
// their own). An *empty* provenance result is normal and returns the record as
// is; a query *error* fails the call, since it signals a real fault in the same
// reader pool the record lookup just used.
func (g *Get) fetch(ctx context.Context, collection, uid string) (getOutput, error) {
	switch collection {
	case collectionEntities:
		// Read-side self-rebind: after a principal is bound, entities/self must
		// follow the binding to the principal node (the seeded "self" entity is
		// left behind, factless). nil binder / unbound device → uid unchanged.
		return g.fetchEntity(ctx, rebindSelf(ctx, g.binder, uid))
	case collectionEpisodes:
		ep, err := g.nav.GetEpisode(ctx, uid)
		if err != nil {
			return getOutput{}, err
		}
		w := projectEpisode(ep)
		prov, err := g.nav.EventProvenance(ctx, ep.UID)
		if err != nil {
			return getOutput{}, err
		}
		return getOutput{Type: "episode", Episode: &w, Provenance: prov}, nil
	case collectionRelations:
		r, err := g.nav.GetRelation(ctx, uid)
		if err != nil {
			return getOutput{}, err
		}
		w := projectRelation(r)
		prov, err := g.provenanceFor(ctx, r.SourceEventUID)
		if err != nil {
			return getOutput{}, err
		}
		return getOutput{Type: "relation", Relation: &w, Provenance: prov}, nil
	case collectionInsights:
		return g.fetchInsight(ctx, uid)
	case collectionConsolidatedMemories:
		return g.fetchConsolidatedMemory(ctx, uid)
	default: // collectionFacts (parseResourceName already validated the set)
		return g.fetchFact(ctx, uid)
	}
}

// fetchEntity projects the entity node and folds in its currently-open scalar
// facts. An entity's attributes live in the facts table, not on the node row,
// so surfacing the open ones lets a single get answer "what do we know about
// this entity" without a second call; edges still live on relations. The facts
// slice is normalized nil → empty so the entity arm always serializes "facts"
// as present-and-(possibly-)empty, never as null or absent.
func (g *Get) fetchEntity(ctx context.Context, uid string) (getOutput, error) {
	e, err := g.nav.GetEntity(ctx, uid)
	if err != nil {
		return getOutput{}, err
	}
	w := projectEntity(e)
	prov, err := g.nav.EntityProvenance(ctx, uid)
	if err != nil {
		return getOutput{}, err
	}
	// Facts use factNav (the requested scope) when set, so a self-identity read
	// whose node was resolved through a widened scope still filters facts to the
	// scope the caller asked for. Nil factNav → node and facts share g.nav.
	factNav := g.nav
	if g.factNav != nil {
		factNav = g.factNav
	}
	facts, err := factNav.FactsForSubject(ctx, uid, g.factFilter)
	if err != nil {
		return getOutput{}, err
	}
	if facts == nil {
		facts = []navigate.Fact{}
	}
	// Surface the entity's external identifiers (entity_identifiers) alongside
	// its typed properties (carried inside the entity record) so a createEntity
	// write round-trips through get: both the typed attributes and the strong
	// identifiers (hrb, lei, email, …) are readable, not write-only.
	ids, err := g.nav.EntityIdentifiers(ctx, uid)
	if err != nil {
		return getOutput{}, err
	}
	return getOutput{Type: "entity", Entity: &w, Facts: &facts, Identifiers: ids, Provenance: prov}, nil
}

// fetchFact projects a fact and surfaces provenance. Facts created by direct
// assertion (maz fact set / the fact MCP tool) have no row in the
// episodes-only provenance table, so we first try EventProvenance via
// SourceEventUID; when that returns empty we fall back to synthetic provenance
// built from the source + confidence columns that applyFact writes directly
// onto the facts row (migration 0017). This guarantees every fact — whether
// extracted by an episode or asserted directly — surfaces lineage.
func (g *Get) fetchFact(ctx context.Context, uid string) (getOutput, error) {
	f, err := g.nav.GetFact(ctx, uid)
	if err != nil {
		return getOutput{}, err
	}
	prov, err := g.provenanceFor(ctx, f.SourceEventUID)
	if err != nil {
		return getOutput{}, err
	}
	// Fall back to synthetic provenance for direct-assertion facts (no episode
	// provenance row).  Only synthesise when Source is non-empty so pre-0017
	// rows without the column are not polluted with a zero-confidence entry.
	if len(prov) == 0 && f.Source != "" {
		prov = []navigate.Provenance{{
			EventUID:   f.SourceEventUID,
			Source:     f.Source,
			Confidence: f.Confidence,
		}}
	}
	return getOutput{Type: "fact", Fact: &f, Provenance: prov}, nil
}

// fetchInsight fetches a synthesized insight by uid and projects it into the
// wire shape. SourceUIDs are typed as entities/{uid} citations (insight sources
// are entities from the reflection pass). A demoted insight (state='demoted')
// is still returned — get is not the hot-recall index.
func (g *Get) fetchInsight(ctx context.Context, uid string) (getOutput, error) {
	ins, err := g.nav.GetInsight(ctx, uid)
	if err != nil {
		return getOutput{}, err
	}
	citations := make([]string, 0, len(ins.SourceUIDs))
	for _, s := range ins.SourceUIDs {
		citations = append(citations, collectionEntities+"/"+s)
	}
	w := insightWire{Insight: ins, SourceCitations: citations}
	return getOutput{Type: "insight", Insight: &w}, nil
}

// fetchConsolidatedMemory fetches a consolidated memory by uid and projects it
// into the wire shape. SourceUIDs are typed as episodes/{uid} citations
// (consolidated-memory sources are the episodes compressed into the summary).
func (g *Get) fetchConsolidatedMemory(ctx context.Context, uid string) (getOutput, error) {
	cm, err := g.nav.GetConsolidatedMemory(ctx, uid)
	if err != nil {
		return getOutput{}, err
	}
	citations := make([]string, 0, len(cm.SourceUIDs))
	for _, s := range cm.SourceUIDs {
		citations = append(citations, collectionEpisodes+"/"+s)
	}
	w := consolidatedMemoryWire{ConsolidatedMemory: cm, SourceCitations: citations}
	return getOutput{Type: "consolidatedMemory", ConsolidatedMemory: &w}, nil
}

// provenanceFor walks an origin event UID back to its provenance lineage,
// shared by the relation and fact arms. An empty UID → no lineage (nil, nil):
// the record simply carries no source-event anchor.
func (g *Get) provenanceFor(ctx context.Context, sourceEventUID string) ([]navigate.Provenance, error) {
	if sourceEventUID == "" {
		return nil, nil
	}
	return g.nav.EventProvenance(ctx, sourceEventUID)
}
