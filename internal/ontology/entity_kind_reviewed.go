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

// KindEntityKindReviewed is the ontology kind under which
// EntityKindReviewedEvent is registered. An entityKindReviewed event is
// appended by the `maz schema review approve` / `maz schema review reject`
// commands (and programmatically by tests) to disposition a proposed entity
// kind.
//
// The event carries the kind name, the review decision, who performed the
// review, and when. The dispatcher materialises it into entity_kinds by
// updating the status column: approved → active, rejected → rejected. Core
// rows are write-protected and cannot be reviewed (their status is always
// active). Unknown kind names are silently skipped so replays stay idempotent
// even when the underlying kind row was already removed.
const KindEntityKindReviewed = "entityKindReviewed"

// VersionEntityKindReviewed is the semver-shaped schema version for
// EntityKindReviewedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionEntityKindReviewed = "v0.1.0"

// EntityKindReviewDecision is the outcome of an entity-kind governance review.
type EntityKindReviewDecision string

// Known EntityKindReviewDecision values.
const (
	// EntityKindReviewApproved marks the reviewed kind as approved: its status
	// transitions from proposed to active and it becomes part of the live
	// taxonomy.
	EntityKindReviewApproved EntityKindReviewDecision = "approved"

	// EntityKindReviewRejected marks the reviewed kind as rejected: its status
	// transitions to rejected and it is excluded from the active taxonomy.
	// The kind row and its events remain in the log for audit purposes.
	EntityKindReviewRejected EntityKindReviewDecision = "rejected"
)

// JSONSchema lets invopop reflect EntityKindReviewDecision as a string-with-enum
// constraint. Called by invopop during Reflect; do not call directly.
func (EntityKindReviewDecision) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "string",
		Enum: []any{
			string(EntityKindReviewApproved),
			string(EntityKindReviewRejected),
		},
	}
}

// EntityKindReviewedEvent is the payload of an entityKindReviewed event.
// It records the reviewer's decision for a proposed entity kind. The Go struct
// is the source of truth; the JSON Schema is reflected from it at first access
// and cached.
type EntityKindReviewedEvent struct {
	// Name is the kind identifier being reviewed, e.g. "user:vehicle".
	// Required. Must match an existing entity_kinds row.
	Name string `json:"name" jsonschema:"required,minLength=1,description=Kind identifier being reviewed. Must match an existing entity_kinds row."`

	// Decision is the review outcome: approved or rejected. Required.
	Decision EntityKindReviewDecision `json:"decision" jsonschema:"required,description=Review outcome: approved (kind becomes active) or rejected (kind is excluded from taxonomy)."`

	// ReviewedBy identifies the agent that performed the review.
	// "cli" for `maz schema review` commands; any string for
	// programmatic use. Optional; empty means the reviewer is unknown.
	ReviewedBy string `json:"reviewedBy,omitempty" jsonschema:"description=Agent that performed the review, e.g. cli. Optional."`

	// ReviewedAt is the instant the decision was made, in UTC. Required.
	// Serialized as RFC 3339.
	ReviewedAt time.Time `json:"reviewedAt" jsonschema:"required,format=date-time,description=When the review decision was made. RFC 3339 UTC."`

	// Note is an optional free-text comment documenting the rationale for
	// the decision. May be empty.
	Note string `json:"note,omitempty" jsonschema:"description=Optional human-readable rationale for the decision."`
}

// entityKindReviewedSchema lazily builds the EntityKindReviewedEvent JSON
// Schema document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var entityKindReviewedSchema = sync.OnceValue(buildEntityKindReviewedSchema)

// buildEntityKindReviewedSchema reflects EntityKindReviewedEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildEntityKindReviewedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&EntityKindReviewedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal entityKindReviewed schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindEntityKindReviewed,
		Version:    VersionEntityKindReviewed,
		Definition: raw,
		Provenance: "core",
	}
}

// EntityKindReviewedSchema returns the cached kernel.Schema for
// EntityKindReviewedEvent. Useful for tests and consumers that need to inspect
// the schema without registering it.
func EntityKindReviewedSchema() kernel.Schema {
	return entityKindReviewedSchema()
}

// RegisterEntityKindReviewed installs the EntityKindReviewedEvent schema with
// o. Call once at process startup (or once per test). The kernel.Ontology
// contract specifies "first registration wins": a second call without a higher
// Version returns kernel.ErrSchemaConflict.
func RegisterEntityKindReviewed(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, entityKindReviewedSchema())
}
