package ontology

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/invopop/jsonschema"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// KindEntityKindRegistered is the ontology kind under which
// EntityKindRegisteredEvent is registered. An entityKindRegistered event is
// the entity-kind analogue of predicateRegistered: it is appended whenever a
// new entity kind enters the taxonomy — the seven core kinds at genesis
// (provenance core, addedBy seed), a novel kind the resolver classifies
// during ingestion (addedBy llm), a kind a human or agent mints explicitly
// (addedBy user), or a kind a schema pack installs (addedBy extension).
//
// The event is the source of truth; the dispatcher materializes it into the
// entity_kinds projection table and the render pipeline writes it to the
// git-synced ontology/ tree, so the taxonomy survives restart, replays under
// maz rebuild, and travels through the git transport. See
// agent-docs/designs/dynamic-ontology.md.
const KindEntityKindRegistered = "entityKindRegistered"

// VersionEntityKindRegistered is the semver-shaped schema version for
// EntityKindRegisteredEvent. Must be a full MAJOR.MINOR.PATCH triplet because
// kernel.MemoryOntology.Register validates with golang.org/x/mod/semver.IsValid,
// which rejects shorter forms like "v0.1".
//
// Version v0.2.0 adds the optional Status field (active|proposed). Old events
// without Status replay as active (zero-value empty string semantics = active).
// The change is backward-compatible: omitempty means v0.1.0 consumers see no
// unexpected field.
const VersionEntityKindRegistered = "v0.2.0"

// Entity-kind governance status values. Stored in the entity_kinds.status
// column and carried on EntityKindRegisteredEvent.Status. An empty Status on a
// registered event means active (backward-compatible with pre-v0.2.0 events).
const (
	// EntityKindStatusActive marks a kind that is part of the live taxonomy.
	EntityKindStatusActive = "active"

	// EntityKindStatusProposed marks a kind minted in governed mode, awaiting
	// disposition via `maz schema review`. Excluded from the active taxonomy
	// but still rendered to git so peers see the proposal.
	EntityKindStatusProposed = "proposed"

	// EntityKindStatusRejected marks a kind a reviewer rejected. Retained for
	// audit; excluded from the active taxonomy.
	EntityKindStatusRejected = "rejected"
)

// EntityKindAddedBy records which agent registered an entity kind. Shared by
// SchemaRegisteredEvent. Mirrors PredicateAddedBy and adds the extension lane
// for schema-pack-supplied kinds (agent-docs/designs/ontology.md anticipates
// the provenance "extension:<name>" lane).
type EntityKindAddedBy string

// Known EntityKindAddedBy values.
const (
	// EntityKindAddedBySeed marks one of the seven core kinds installed by the
	// genesis seed batch on first boot (provenance core).
	EntityKindAddedBySeed EntityKindAddedBy = "seed"

	// EntityKindAddedByLLM marks a novel kind the resolver classified and
	// registered automatically during ingestion (provenance user-namespaced).
	EntityKindAddedByLLM EntityKindAddedBy = "llm"

	// EntityKindAddedByUser marks a kind a human (via `maz schema kinds add`)
	// or an agent (via the defineEntityKind MCP tool) registered explicitly.
	EntityKindAddedByUser EntityKindAddedBy = "user"

	// EntityKindAddedByExtension marks a kind a schema pack installed
	// (provenance extension:<name>).
	EntityKindAddedByExtension EntityKindAddedBy = "extension"
)

// JSONSchema lets invopop reflect EntityKindAddedBy as a string-with-enum
// constraint. Called by invopop during Reflect; do not call directly.
func (EntityKindAddedBy) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "string",
		Enum: []any{
			string(EntityKindAddedBySeed),
			string(EntityKindAddedByLLM),
			string(EntityKindAddedByUser),
			string(EntityKindAddedByExtension),
		},
	}
}

// EntityKindRegisteredEvent is the payload of an entityKindRegistered event:
// one entry in the entity-kind taxonomy. The Go struct is the source of
// truth; the JSON Schema is reflected from it at first access and cached.
//
// Naming maps to Palantir Foundry object types and the JSON-LD / RDFS
// vocabulary-reuse convention: Name is the local type, Extends is RDFS
// subClassOf (single parent), SameAs lists the canonical/external IRIs the
// kind aligns to (schema.org, Wikidata QID, FOAF, Dublin Core), and
// PropertySchema is the SHACL-style shape for the kind's typed attributes.
type EntityKindRegisteredEvent struct {
	// Name is the kind's identifier. Required. Core kinds use a bare
	// snake_case token (person). Custom kinds are namespaced:
	// user:deal, extension:schema_org:Person. The namespaced tail may carry
	// source casing (schema.org types are PascalCase).
	Name string `json:"name" jsonschema:"required,minLength=1,pattern=^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$,description=Kind identifier. Bare snake_case for core kinds (person); namespaced for custom (user:deal, extension:schema_org:Person)."`

	// DisplayName is the human-readable label, e.g. "Research Paper".
	// Required. Non-empty.
	DisplayName string `json:"displayName" jsonschema:"required,minLength=1,description=Human-readable display label, e.g. Research Paper."`

	// Description is a one-sentence explanation of what the kind models.
	// Required. Non-empty. Embedded by the dedup-check so near-duplicate
	// kinds can be detected before a synonym is minted.
	Description string `json:"description" jsonschema:"required,minLength=1,description=One-sentence description of the kind. Drives dedup and agent reuse decisions."`

	// Extends names the parent kind for the subtype hierarchy (RDFS
	// subClassOf). Optional; empty for root kinds. Single inheritance;
	// cycles are rejected at registration. A child's PropertySchema is
	// composed with the parent's via JSON Schema allOf. When present, the
	// reference must itself be a valid kind name, so it carries the same
	// pattern as Name (bare snake_case or namespaced).
	Extends string `json:"extends,omitempty" jsonschema:"pattern=^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$,description=Parent kind name for the subtype hierarchy (single inheritance). Empty for root kinds. Must be a valid kind name."`

	// SameAs lists alias/sameAs IRIs mapping this kind to canonical or
	// external vocabularies: schema.org/Person, wikidata:Q5, foaf:Person.
	// Optional. Enables interoperability and cross-vocabulary deduplication.
	SameAs []string `json:"sameAs,omitempty" jsonschema:"description=Alias/sameAs IRIs to canonical or external vocabularies, e.g. schema.org/Person, wikidata:Q5."`

	// PropertySchema is an optional JSON Schema (Draft 2020-12) describing
	// the kind's typed attributes — the Palantir property-type / SHACL-shape
	// layer. Absent means the kind is untyped (free-form attributes).
	// Validated against entity instances only when [ontology] strict_attributes
	// is enabled. Composed with the parent's schema when Extends is set.
	PropertySchema json.RawMessage `json:"propertySchema,omitempty" jsonschema:"description=Optional JSON Schema for the kind's typed attributes (Palantir property types). Absent = untyped."`

	// Provenance identifies the namespace lane that owns this kind:
	// core (shipped, immutable), extension:<name> (schema pack), or user
	// (human/agent/LLM minted). Required.
	Provenance string `json:"provenance" jsonschema:"required,pattern=^(core|user|extension:[a-z][a-z0-9_-]*)$,description=Namespace lane: core, user, or extension:<name>."`

	// AddedBy identifies the agent that registered this kind: seed, llm,
	// user, or extension. Required. Enforced by EntityKindAddedBy.JSONSchema().
	AddedBy EntityKindAddedBy `json:"addedBy" jsonschema:"required,description=Agent that registered the kind: seed, llm, user, or extension."`

	// AddedAt is the instant the kind was registered, in UTC. Required.
	// Serialized as RFC 3339.
	AddedAt time.Time `json:"addedAt" jsonschema:"required,format=date-time,description=When the kind was registered. RFC 3339 UTC."`

	// Status reflects whether the kind has been approved for use in the live
	// taxonomy. Empty (the zero value) and "active" are equivalent — old
	// events that pre-date v0.2.0 replay as active. "proposed" means the
	// kind is queued for human review (governed mode); "rejected" means the
	// review queue rejected it. Optional; omitempty so the common active
	// path produces no extra JSON bytes.
	Status string `json:"status,omitempty" jsonschema:"enum=active,enum=proposed,enum=rejected,description=Governance status: active (default) or proposed (awaiting review in governed mode) or rejected."`
}

// entityKindRegisteredSchema lazily builds the EntityKindRegisteredEvent JSON
// Schema document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var entityKindRegisteredSchema = sync.OnceValue(buildEntityKindRegisteredSchema)

// buildEntityKindRegisteredSchema reflects EntityKindRegisteredEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildEntityKindRegisteredSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&EntityKindRegisteredEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal entityKindRegistered schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindEntityKindRegistered,
		Version:    VersionEntityKindRegistered,
		Definition: raw,
		Provenance: "core",
	}
}

// EntityKindRegisteredSchema returns the cached kernel.Schema for
// EntityKindRegisteredEvent. Useful for tests and consumers that need to
// inspect the schema without registering it.
func EntityKindRegisteredSchema() kernel.Schema {
	return entityKindRegisteredSchema()
}

// RegisterEntityKindRegistered installs the EntityKindRegisteredEvent schema
// with o. Call once at process startup (or once per test). The
// kernel.Ontology contract specifies "first registration wins": a second call
// without a higher Version returns kernel.ErrSchemaConflict.
func RegisterEntityKindRegistered(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, entityKindRegisteredSchema())
}
