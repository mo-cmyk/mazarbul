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

// KindPredicateRegistered is the ontology kind under which
// PredicateRegisteredEvent is registered. A predicateRegistered event is
// appended by the resolver the first time it encounters a predicate that
// is not yet in the predicates table. The event records the predicate
// string, its functional/non-functional classification (determined by an
// LLM call for unknown predicates, or hard-coded for the seed list), and
// the agent that added it.
const KindPredicateRegistered = "predicateRegistered"

// VersionPredicateRegistered is the semver-shaped schema version for
// PredicateRegisteredEvent. Must be a full MAJOR.MINOR.PATCH triplet.
//
// Version v0.2.0 adds optional SubjectKinds, ObjectKinds, SameAs, and
// PropertySchema fields for domain/range declarations and linked-data alignment
// (P5 typed relations). All new fields are omitempty so existing events remain
// valid.
//
// Version v0.2.1 adds "extension" to the PredicateAddedBy enum so schema packs
// can record their provenance accurately. Additive enum — v0.2.0 events without
// this value remain valid.
const VersionPredicateRegistered = "v0.2.1"

// PredicateKind classifies a predicate as functional (at most one current
// value per subject) or non-functional (multiple values permitted).
// Functional predicates trigger unconditional supersession when a new
// conflicting Relation is appended; non-functional predicates go through
// the LLM contradiction check first.
type PredicateKind string

// Known PredicateKind values.
const (
	// PredicateKindFunctional identifies predicates where only one
	// current value is valid per subject Entity, e.g. works_at or
	// lives_in. A new works_at Relation unconditionally supersedes
	// the prior one without an LLM contradiction check.
	PredicateKindFunctional PredicateKind = "functional"

	// PredicateKindNonFunctional identifies predicates where multiple
	// current values are valid simultaneously, e.g. member_of or
	// knows. A new conflicting Relation triggers an LLM contradiction
	// check before supersession.
	PredicateKindNonFunctional PredicateKind = "non-functional"
)

// JSONSchema lets invopop reflect PredicateKind as a string-with-enum
// constraint. Called by invopop during Reflect; do not call directly.
func (PredicateKind) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "string",
		Enum: []any{
			string(PredicateKindFunctional),
			string(PredicateKindNonFunctional),
		},
	}
}

// PredicateAddedBy records which agent registered the predicate. The
// resolver seeds the predicates table with a hard-coded list on first
// boot (seed); subsequent additions come from the user or the LLM
// predicate-classification call.
type PredicateAddedBy string

// Known PredicateAddedBy values.
const (
	// PredicateAddedByUser indicates the predicate was added explicitly
	// by the human user, e.g. via a future `maz schema predicates add`
	// command.
	PredicateAddedByUser PredicateAddedBy = "user"

	// PredicateAddedByLLM indicates the predicate was classified and
	// registered automatically by the resolver's LLM
	// predicate-canonicalization call (LLM leverage point #6).
	PredicateAddedByLLM PredicateAddedBy = "llm"

	// PredicateAddedBySeed indicates the predicate is part of the
	// hard-coded seed list installed by the resolver on first boot
	// (works_at, lives_in, born_in, etc.).
	PredicateAddedBySeed PredicateAddedBy = "seed"

	// PredicateAddedByExtension indicates the predicate was registered by a
	// schema pack or other plugin extension (provenance "extension:<name>").
	// Added in v0.2.1.
	PredicateAddedByExtension PredicateAddedBy = "extension"
)

// JSONSchema lets invopop reflect PredicateAddedBy as a string-with-enum
// constraint. Called by invopop during Reflect; do not call directly.
func (PredicateAddedBy) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "string",
		Enum: []any{
			string(PredicateAddedByUser),
			string(PredicateAddedByLLM),
			string(PredicateAddedBySeed),
			string(PredicateAddedByExtension),
		},
	}
}

// PredicateRegisteredEvent is the payload of a predicateRegistered event.
// It records a newly recognised predicate, its functional classification,
// a one-sentence description, and the agent that added it. The Go struct
// is the source of truth; the JSON Schema is reflected from it at first
// access and cached.
type PredicateRegisteredEvent struct {
	// Predicate is the predicate string being registered, e.g.
	// works_at or member_of. Required. Non-empty. The resolver
	// normalises predicates to snake_case before registering; exact
	// normalisation rules land with the resolver implementation.
	Predicate string `json:"predicate" jsonschema:"required,minLength=1,description=The predicate string being registered, e.g. works_at. Required."`

	// Kind classifies the predicate as functional or non-functional.
	// Required. Enforced by PredicateKind.JSONSchema().
	Kind PredicateKind `json:"kind" jsonschema:"required,description=Functional or non-functional classification. Required."`

	// Description is a one-sentence human-readable explanation of what
	// the predicate means, generated by the LLM for llm-added
	// predicates and hand-written for seed predicates. Required.
	// Non-empty.
	Description string `json:"description" jsonschema:"required,minLength=1,description=One-sentence description of the predicate. Required."`

	// AddedBy identifies the agent that registered this predicate.
	// Required. Enforced by PredicateAddedBy.JSONSchema().
	AddedBy PredicateAddedBy `json:"addedBy" jsonschema:"required,description=Agent that registered this predicate: user, llm, or seed. Required."`

	// AddedAt is the instant the predicate was registered, in UTC.
	// Required. Serialized as RFC 3339.
	AddedAt time.Time `json:"addedAt" jsonschema:"required,format=date-time,description=When the predicate was registered. RFC 3339 UTC. Required."`

	// SubjectKinds lists the entity-kind names that are valid subjects for
	// this predicate (domain constraint). Optional; absent means any kind is
	// accepted. E.g. ["person", "organization"] for works_at.
	SubjectKinds []string `json:"subjectKinds,omitempty" jsonschema:"description=Entity-kind names that are valid subjects for this predicate. Absent means any kind is accepted."`

	// ObjectKinds lists the entity-kind names that are valid objects for
	// this predicate (range constraint). Optional; absent means any kind is
	// accepted. E.g. ["organization"] for works_at.
	ObjectKinds []string `json:"objectKinds,omitempty" jsonschema:"description=Entity-kind names that are valid objects for this predicate. Absent means any kind is accepted."`

	// SameAs lists alias/sameAs IRIs mapping this predicate to canonical or
	// external vocabularies: schema.org properties, Wikidata properties, FOAF.
	// Optional. Enables interoperability and cross-vocabulary deduplication.
	SameAs []string `json:"sameAs,omitempty" jsonschema:"description=Alias/sameAs IRIs to canonical or external vocabularies, e.g. schema.org/worksFor, wikidata:P108."`

	// PropertySchema is an optional JSON Schema (Draft 2020-12) describing
	// typed constraints on the relation's Attributes map for this predicate.
	// Absent means the attributes bag is untyped (free-form). Validated
	// against relation instances only when [ontology] strict_domain_range
	// is enabled (P5 typed relations).
	PropertySchema json.RawMessage `json:"propertySchema,omitempty" jsonschema:"description=Optional JSON Schema for the relation's Attributes map. Absent = untyped."`
}

// predicateRegisteredSchema lazily builds the PredicateRegisteredEvent
// JSON Schema document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var predicateRegisteredSchema = sync.OnceValue(buildPredicateRegisteredSchema)

// buildPredicateRegisteredSchema reflects PredicateRegisteredEvent into a
// kernel.Schema. Runs once at first call; panics on failure because
// invopop cannot fail on a well-formed in-tree struct.
func buildPredicateRegisteredSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&PredicateRegisteredEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal predicateRegistered schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindPredicateRegistered,
		Version:    VersionPredicateRegistered,
		Definition: raw,
		Provenance: "core",
	}
}

// PredicateRegisteredSchema returns the cached kernel.Schema for
// PredicateRegisteredEvent. Useful for tests and consumers that need to
// inspect the schema without registering it.
func PredicateRegisteredSchema() kernel.Schema {
	return predicateRegisteredSchema()
}

// RegisterPredicateRegistered installs the PredicateRegisteredEvent
// schema with o. Call once at process startup (or once per test). The
// kernel.Ontology contract specifies "first registration wins": a second
// call without a higher Version returns kernel.ErrSchemaConflict.
func RegisterPredicateRegistered(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, predicateRegisteredSchema())
}
