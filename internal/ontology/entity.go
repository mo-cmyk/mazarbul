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

// KindEntity is the ontology kind under which Entity is registered.
// Entity is the canonical resolved form of a person, organization,
// place, project, event, or document: the consensus view materialized
// from one or more Mentions by the resolver. The resolver itself lands
// in M2; the schema lands here so downstream kinds (#35 Mention, #36
// Relation) have a target to point at.
const KindEntity = "entity"

// VersionEntity is the semver-shaped schema version for Entity. Bumped
// to v0.4.0 to add the optional Properties field carrying per-kind typed
// attribute values (P3 dynamic-ontology typed properties). V0.3.0
// replaced the closed EntityKind enum with an open pattern-based
// constraint (P2). V0.2.0 added summary, summaryUpdateTime, coordinates,
// and coordinatesProvenance (P0). V0.x.y signals that breaking changes are
// allowed before v1.0; the M2 epic introduces the migration story (#13).
// Must be a full MAJOR.MINOR.PATCH triplet because
// kernel.MemoryOntology.Register validates with golang.org/x/mod/semver.IsValid,
// which rejects shorter forms like "v0.1" (Greptile #82 P1).
const VersionEntity = "v0.4.0"

// Coordinates holds the geographic position of a place Entity in WGS84
// decimal degrees. Both fields are required when the struct is present;
// the Coordinates field on Entity is a pointer so omitempty fires when
// absent, matching the *time.Time pattern used for ValidFrom/ValidUntil
// on Relation.
type Coordinates struct {
	// Latitude is the WGS84 latitude in decimal degrees. Required.
	// Must be in the closed interval [-90, 90].
	Latitude float64 `json:"latitude" jsonschema:"required,minimum=-90,maximum=90,description=Latitude in WGS84 decimal degrees. Range -90..90 inclusive. Required."`

	// Longitude is the WGS84 longitude in decimal degrees. Required.
	// Must be in the closed interval [-180, 180].
	Longitude float64 `json:"longitude" jsonschema:"required,minimum=-180,maximum=180,description=Longitude in WGS84 decimal degrees. Range -180..180 inclusive. Required."`
}

// EntityKind names the high-level taxon of an Entity. The seven built-in
// core kinds (person, organization, place, project, event, document,
// external_ref) are the canonical taxa shipped with Mazarbul. The ontology
// registry allows
// extensions to add namespaced custom kinds such as "user:deal" or
// "extension:schema_org:Person". The Go constants give callers a typed
// handle without changing serialization.
//
// Mirrors the ULID custom-type pattern (see episode.go): we attach the
// pattern constraint to the type via JSONSchema() instead of packing it
// into every struct-tag literal, so future fields of kind EntityKind
// inherit the constraint automatically.
type EntityKind string

// Core EntityKind values. Extension kinds follow the namespacing
// convention: "namespace:localname", e.g. "user:deal" or
// "extension:schema_org:Product". The ontology registry validates
// membership at write time via a warning-accept path (P2).
const (
	EntityKindPerson       EntityKind = "person"
	EntityKindOrganization EntityKind = "organization"
	EntityKindPlace        EntityKind = "place"
	EntityKindProject      EntityKind = "project"
	EntityKindEvent        EntityKind = "event"
	EntityKindDocument     EntityKind = "document"
	// EntityKindExternalRef models a pointer to information held in another
	// system (a URI). Its canonical identity is a {type:"uri"} Identifier; it
	// is a first-class local entity so relate/recall/traverse work unchanged.
	EntityKindExternalRef EntityKind = "external_ref"
)

// entityKindPattern is the regular expression that EntityKind.JSONSchema
// compiles into the JSON Schema pattern constraint. It accepts the seven
// core snake_case kinds and namespaced extension kinds of the form
// "namespace:localname" (one or more colon-separated segments, each a
// non-empty C-identifier-safe token). Examples that match:
//
//	person, organization, user:deal, extension:schema_org:Person
//
// Examples that do NOT match: "Fish", "123abc", "", "user:", ":deal".
const entityKindPattern = `^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$`

// JSONSchema lets invopop reflect EntityKind as an open string constrained
// by a pattern. The pattern accepts the seven core built-in kinds (bare
// snake_case: person, organization, place, project, event, document,
// external_ref) and namespaced extension kinds registered via the ontology
// registry (e.g. "user:deal", "extension:schema_org:Person"). Called by
// invopop during Reflect; do not call directly.
func (EntityKind) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Pattern:     entityKindPattern,
		Description: "Entity kind. Core kinds: person, organization, place, project, event, document, external_ref. Extension kinds follow the namespace:localname convention, e.g. user:deal.",
	}
}

// ResolutionStatus names the resolver's verdict on an Entity. Confirmed
// means a strong-id or user-approved merge; tentative means an automatic
// merge in the gray zone; split_candidate means the resolver flagged the
// merge for human review.
//
// The wire form uses split_candidate (underscore) per the issue AC,
// even though the architecture spec writes split-candidate (hyphen);
// the issue body is authoritative here.
type ResolutionStatus string

// Allowed ResolutionStatus values.
const (
	ResolutionConfirmed      ResolutionStatus = "confirmed"
	ResolutionTentative      ResolutionStatus = "tentative"
	ResolutionSplitCandidate ResolutionStatus = "split_candidate"
)

// JSONSchema lets invopop reflect ResolutionStatus as a string-with-enum
// constraint. Called by invopop during Reflect; do not call directly.
func (ResolutionStatus) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "string",
		Enum: []any{
			string(ResolutionConfirmed),
			string(ResolutionTentative),
			string(ResolutionSplitCandidate),
		},
	}
}

// Identifier is one typed external identifier attached to an Entity,
// e.g. {type: "email", value: "frodo@erebor.example"} or
// {type: "github_user_id", value: "12345678"}. The taxonomy of Type
// values is intentionally open in M1; M2 introduces strong-id awareness
// in the resolver (architecture spec section 05, "strong vs weak
// identifiers").
type Identifier struct {
	// Type is the identifier taxonomy tag. Required. Free-form in M1;
	// canonicalized in M2 alongside the resolver's strong-id table.
	Type string `json:"type" jsonschema:"required,minLength=1,description=Identifier type, e.g. email or github_user_id."`

	// Value is the identifier value. Required. Opaque to this schema;
	// per-type format validation lands with the resolver in M2.
	Value string `json:"value" jsonschema:"required,minLength=1,description=Identifier value. Opaque to this schema in v0.1."`
}

// Entity is the canonical resolved form of a real-world referent in
// Mazarbul's knowledge graph. The Go struct is the source of truth;
// the JSON Schema is reflected from it at first access via
// invopop/jsonschema and cached, so the wire shape and the Go type
// cannot drift.
//
// At M1 an Entity carries identity and a confidence-tagged resolution
// status; merge history (MergeEvent) and world-ontology identifiers
// (Wikidata QID, etc.) are out of scope until M2 with the resolver.
//
// Field naming follows Google AIP: createTime (not createdAt), name as
// the resource name in the form "entities/{uid}", uid as the stable
// ULID identifier. Optional fields carry omitempty so absent fields are
// omitted from JSON rather than serialized as empty.
type Entity struct {
	// Name is the AIP resource name in the form "entities/{uid}".
	// Required. Output-only; assigned by the writer from UID.
	Name string `json:"name" jsonschema:"required,pattern=^entities/[0-9A-HJKMNP-TV-Z]{26}$,description=Resource name in the form entities/{uid}. Output-only."`

	// UID is the ULID identifier for this Entity. Required. Output-only.
	// Sortable lexicographically by creation time, opaque, immutable.
	UID string `json:"uid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identifier. Output-only; assigned by the writer."`

	// Kind is the high-level taxon: one of the seven core kinds (person,
	// organization, place, project, event, document, external_ref) or a
	// namespaced extension kind registered via the ontology registry (e.g.
	// "user:deal"). Required. Pattern-validated by EntityKind.JSONSchema().
	// Membership in the registry is checked at write time with a
	// warning-accept policy (P2 dynamic-ontology).
	Kind EntityKind `json:"kind" jsonschema:"required,description=Entity kind. One of person, organization, place, project, event, document."`

	// DisplayName is the human-readable label picked from the canonical
	// mention or set explicitly by the user. Required. Non-empty.
	DisplayName string `json:"displayName" jsonschema:"required,minLength=1,description=Human-readable name. Picked from the canonical mention or set explicitly."`

	// Confidence is the aggregate confidence in this resolved Entity,
	// in the closed interval 0..1. Required. 1.0 for user-confirmed
	// identities; lower for resolver-only merges in the gray zone.
	Confidence float64 `json:"confidence" jsonschema:"required,minimum=0,maximum=1,description=Aggregate confidence in this resolved Entity. Range 0..1 inclusive."`

	// ResolutionStatus is the resolver's verdict: confirmed, tentative,
	// or split_candidate. Required. Enforced by
	// ResolutionStatus.JSONSchema().
	ResolutionStatus ResolutionStatus `json:"resolutionStatus" jsonschema:"required,description=Resolution status. One of confirmed, tentative, split_candidate."`

	// CreateTime is the instant the Entity was first materialized in
	// UTC. Required. Output-only. Serialized as an RFC 3339 string.
	CreateTime time.Time `json:"createTime" jsonschema:"required,format=date-time,description=Time the Entity was first materialized. RFC 3339 UTC. Output-only."`

	// AlternateNames lists other names observed across mentions for
	// this Entity. Optional. Order is not significant; a future
	// migration may deduplicate, but the schema does not enforce
	// uniqueness today.
	AlternateNames []string `json:"alternateNames,omitempty" jsonschema:"description=Alternate names observed across mentions. Order not significant."`

	// Identifiers holds typed external identifiers, e.g. email or
	// github_user_id. Optional. Per-type format validation is the
	// resolver's job in M2.
	Identifiers []Identifier `json:"identifiers,omitempty" jsonschema:"description=Typed external identifiers, e.g. email or github_user_id."`

	// MentionUIDs lists the Mention UIDs that resolve to this Entity.
	// Optional. Empty until the resolver runs. Element type is ULID so
	// the schema validates each entry against the ULID pattern.
	MentionUIDs []ULID `json:"mentionUids,omitempty" jsonschema:"description=Mention UIDs that resolve to this Entity. Each is a ULID."`

	// Summary is an LLM-generated one-paragraph description of this
	// Entity, synthesised from its accumulated Mentions. Optional.
	// Absent on freshly created Entities; regenerated by the resolver
	// (M1) when N new Mentions accumulate or 24 hours elapse. Output-only:
	// the writer pipeline sets this field; callers must not supply it.
	Summary string `json:"summary,omitempty" jsonschema:"description=LLM-generated one-paragraph summary of this Entity. Regenerated when N new Mentions accumulate or 24h elapse. Output-only. Optional."`

	// SummaryUpdateTime is the instant the Summary was last regenerated,
	// in UTC. Optional. Absent when Summary is absent. Pointer so
	// omitempty fires on absence; a bare time.Time would emit
	// "0001-01-01T00:00:00Z". Output-only.
	SummaryUpdateTime *time.Time `json:"summaryUpdateTime,omitempty" jsonschema:"format=date-time,description=When the Summary was last regenerated. RFC 3339 UTC. Output-only. Optional."`

	// Coordinates holds the geographic position of this Entity in WGS84
	// decimal degrees. Optional. Set when the source supplies coordinates
	// (M1: explicit user input or place-keyed identifier match; M2:
	// geocoder enrichment). Pointer so omitempty fires on absence,
	// mirroring the *time.Time pattern on ValidFrom/ValidUntil.
	Coordinates *Coordinates `json:"coordinates,omitempty" jsonschema:"description=Geographic coordinates of this Entity. Both latitude and longitude required when present. Optional."`

	// CoordinatesProvenance records the origin of the Coordinates value,
	// e.g. user_provided, geocoded:nominatim, or calendar:google.
	// Optional. Present only when Coordinates is set. Rank order for
	// conflict resolution (highest wins): user_provided >
	// calendar:google > geocoded:nominatim. Optional.
	CoordinatesProvenance string `json:"coordinatesProvenance,omitempty" jsonschema:"description=Source of the Coordinates value, e.g. user_provided or geocoded:nominatim. Optional."`

	// Properties holds optional typed attribute values for this Entity,
	// validated against the kind's propertySchema when
	// [ontology] strict_attributes is enabled (P3 typed properties).
	// When absent the entity carries no kind-specific attributes.
	// The shape is a free JSON object; each key is a property name
	// defined in the kind's JSON Schema. Optional (AIP-203).
	Properties json.RawMessage `json:"properties,omitempty" jsonschema:"type=object,description=Optional typed attribute values for this Entity, validated against the kind's propertySchema. Optional."`
}

// entitySchema lazily builds the Entity JSON Schema document once and
// caches it for the process lifetime. Invopop reflection is not free;
// sync.OnceValue makes the cost pay-once regardless of how many callers
// invoke RegisterEntity or EntitySchema.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var entitySchema = sync.OnceValue(buildEntitySchema)

// buildEntitySchema reflects the Entity struct into a kernel.Schema.
// Runs once at first call to EntitySchema or RegisterEntity; panics on
// failure because invopop cannot fail on a well-formed in-tree struct
// and a startup-time encoding error is unrecoverable.
func buildEntitySchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&Entity{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal entity schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindEntity,
		Version:    VersionEntity,
		Definition: raw,
		Provenance: "core",
	}
}

// EntitySchema returns the cached kernel.Schema document for Entity.
// Useful for tests and callers that need to inspect the schema without
// registering it.
func EntitySchema() kernel.Schema {
	return entitySchema()
}

// RegisterEntity installs the Entity schema with o. Call once at
// process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher
// Version returns kernel.ErrSchemaConflict; this function does not
// retry or suppress that error.
//
// Named per-kind (RegisterEntity rather than the generic Register) so
// sibling kinds (#22 Episode, #35 Mention, #36 Relation) coexist in
// this package without a function-name collision.
func RegisterEntity(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, entitySchema())
}
