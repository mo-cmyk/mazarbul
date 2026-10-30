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

// KindEntityIdentifierAdded is the ontology kind under which
// EntityIdentifierAddedEvent is registered. The resolver appends one of
// these events every time it associates a surface form, email, username,
// or external identifier with an Entity, so the dispatcher can populate
// the entity_identifiers table and `maz rebuild` reconstructs the alias
// map deterministically from the JSONL log.
const KindEntityIdentifierAdded = "entityIdentifierAdded"

// VersionEntityIdentifierAdded is the semver-shaped schema version.
//
// Version v0.2.0 opened IdentifierKind from a closed five-value enum to a
// snake_case pattern so domain identifier types (hrb, lei, tax_id,
// github_user_id, …) keep their own lookup lane in entity_identifiers
// instead of collapsing into the generic "id" kind — two entities sharing
// the value "123" under different domain types must never anchor to the
// same (identifier, kind) key. The change is widening-only: every v0.1.0
// payload remains valid.
const VersionEntityIdentifierAdded = "v0.2.0"

// IdentifierKind classifies the shape of the identifier so the resolver
// can dispatch on type and so lookups key on (identifier, kind). The five
// canonical constants below cover the human/machine identifiers the
// resolver extracts itself; any other snake_case type (hrb, lei, tax_id)
// is a domain identifier lane introduced by createEntity or a schema pack.
type IdentifierKind string

// Canonical IdentifierKind values. The schema is an open snake_case
// pattern, not an enum of these — see IdentifierKindPattern.
const (
	IdentifierKindAlias    IdentifierKind = "alias"
	IdentifierKindEmail    IdentifierKind = "email"
	IdentifierKindUsername IdentifierKind = "username"
	IdentifierKindPhone    IdentifierKind = "phone"
	IdentifierKindID       IdentifierKind = "id"
)

// IdentifierKindPattern is the snake_case shape every IdentifierKind must
// match: a lowercase letter followed by lowercase letters, digits, or
// underscores. Shared by the JSON Schema below and write-path validators.
const IdentifierKindPattern = `^[a-z][a-z0-9_]*$`

// JSONSchema lets invopop reflect IdentifierKind as an open
// string-with-pattern constraint (v0.2.0; previously a closed enum).
// Called by invopop during Reflect; do not call directly.
func (IdentifierKind) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:    "string",
		Pattern: IdentifierKindPattern,
	}
}

// IdentifierSource records which surface put the identifier on the
// Entity. "extracted" is the default for resolver-discovered surface
// forms; "user" is set by `maz relate` or the future TUI review queue;
// "inferred" is reserved for future enrichment passes.
type IdentifierSource string

// Allowed IdentifierSource values.
const (
	IdentifierSourceExtracted IdentifierSource = "extracted"
	IdentifierSourceUser      IdentifierSource = "user"
	IdentifierSourceInferred  IdentifierSource = "inferred"
)

// JSONSchema lets invopop reflect IdentifierSource as a string-with-enum
// constraint. Called by invopop during Reflect; do not call directly.
func (IdentifierSource) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "string",
		Enum: []any{
			string(IdentifierSourceExtracted),
			string(IdentifierSourceUser),
			string(IdentifierSourceInferred),
		},
	}
}

// EntityIdentifierAddedEvent is the payload of an entityIdentifierAdded
// event. One identifier per event; the resolver batches them by appending
// multiple events when an entity has more than one surface form.
type EntityIdentifierAddedEvent struct {
	// EntityUID is the target Entity. Required. Must be a ULID.
	EntityUID string `json:"entityUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Entity that owns this identifier. Required."`

	// Identifier is the surface form being attached, e.g. "Eowyn",
	// "rosie@erebor.example", or a github user ID. Required. Non-empty.
	Identifier string `json:"identifier" jsonschema:"required,minLength=1,description=The identifier value being attached to the Entity. Required."`

	// Kind dispatches on the identifier shape. Required. Enforced by
	// IdentifierKind.JSONSchema().
	Kind IdentifierKind `json:"kind" jsonschema:"required,description=Identifier kind: alias, email, username, phone, or id. Required."`

	// Source records which surface added the identifier. Required.
	Source IdentifierSource `json:"source" jsonschema:"required,description=Origin: extracted, user, or inferred. Required."`

	// AddedAt is the instant the identifier was attached, in UTC.
	// Required. Serialized as RFC 3339.
	AddedAt time.Time `json:"addedAt" jsonschema:"required,format=date-time,description=When the identifier was added. RFC 3339 UTC. Required."`
}

//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var entityIdentifierAddedSchema = sync.OnceValue(buildEntityIdentifierAddedSchema)

func buildEntityIdentifierAddedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&EntityIdentifierAddedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal entityIdentifierAdded schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindEntityIdentifierAdded,
		Version:    VersionEntityIdentifierAdded,
		Definition: raw,
		Provenance: "core",
	}
}

// EntityIdentifierAddedSchema returns the cached schema document.
func EntityIdentifierAddedSchema() kernel.Schema {
	return entityIdentifierAddedSchema()
}

// RegisterEntityIdentifierAdded installs the schema with o. Call once at
// startup; "first registration wins".
func RegisterEntityIdentifierAdded(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, entityIdentifierAddedSchema())
}
