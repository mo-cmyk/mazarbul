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

// KindRelationSupersedeConflict is the ontology kind for a
// cross-device supersede-conflict event. Emitted by the resolver
// during post-pull replay when two devices independently superseded
// the same Relation along incompatible paths — for example, device A
// closes Rosie-CEO and replaces it with Rosie-CFO while device B
// closes Rosie-CEO and replaces it with Rosie-leaving-the-company.
// Convergent retraction (relationRetracted) makes the CLOSING of
// Rosie-CEO converge by construction, but the two distinct REPLACEMENT
// arms (Rosie-CFO and Rosie-leaving) both land OPEN in the same
// functional slot — a residual conflict the monotonic tombstone set
// deliberately does not resolve. Rather than silently collapse one arm,
// the resolver materialises the conflict as a first-class event for the
// operator to triage via /review conflicts.
//
// The event itself is the durable conflict record: the dispatcher
// applies no derived-store mutation, so a re-replay is naturally
// idempotent. Resolution happens via a follow-up user action that
// emits a normal relationForgotten event closing the losing arm.
const KindRelationSupersedeConflict = "relationSupersedeConflict"

// VersionRelationSupersedeConflict is the semver-shaped schema
// version. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionRelationSupersedeConflict = "v0.1.0"

// RelationSupersedeConflictEvent is the payload of a
// relationSupersedeConflict event. PrimaryRelationUID and
// ConflictingRelationUID identify the two open relations the
// resolver flagged as a concurrent-supersede pair; (Predicate,
// SubjectUID) names the functional slot they share. Reason carries
// a short machine-readable tag the TUI maps to a human label.
type RelationSupersedeConflictEvent struct {
	// PrimaryRelationUID is the ULID of the open Relation the
	// resolver picked as the primary side of the conflict (lowest
	// ULID among the open siblings — purely a deterministic tiebreak,
	// not a quality judgement). Required.
	PrimaryRelationUID ULID `json:"primaryRelationUid" jsonschema:"required,description=ULID of the Relation the resolver picked as the primary arm. Lowest-ULID tiebreak. Required."`

	// ConflictingRelationUID is the ULID of the OTHER open Relation
	// in the same functional slot. Required.
	ConflictingRelationUID ULID `json:"conflictingRelationUid" jsonschema:"required,description=ULID of the conflicting open Relation. Required."`

	// SubjectUID is the entity UID shared by both relations.
	// Required.
	SubjectUID ULID `json:"subjectUid" jsonschema:"required,description=Subject entity UID common to both relations. Required."`

	// Predicate is the functional predicate where both relations
	// landed open simultaneously. Required.
	Predicate string `json:"predicate" jsonschema:"required,description=Functional predicate name. Required."`

	// Reason is a short tag describing the detection trigger.
	// "multiple_open_functional" is the only currently-emitted
	// reason; the field is present so future detection heuristics
	// can disambiguate without bumping the schema version.
	Reason string `json:"reason" jsonschema:"required,description=Detection trigger tag. Required."`

	// DetectedAt is the instant the resolver flagged the conflict,
	// in UTC. Required. Used so the TUI can order conflicts by age.
	DetectedAt time.Time `json:"detectedAt" jsonschema:"required,format=date-time,description=When the conflict was detected. RFC 3339 UTC. Required."`
}

// relationSupersedeConflictSchema lazily builds the
// RelationSupersedeConflictEvent JSON Schema document once and caches
// it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var relationSupersedeConflictSchema = sync.OnceValue(buildRelationSupersedeConflictSchema)

func buildRelationSupersedeConflictSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&RelationSupersedeConflictEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal relationSupersedeConflict schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindRelationSupersedeConflict,
		Version:    VersionRelationSupersedeConflict,
		Definition: raw,
		Provenance: "core",
	}
}

// RelationSupersedeConflictSchema returns the cached kernel.Schema
// for RelationSupersedeConflictEvent.
func RelationSupersedeConflictSchema() kernel.Schema {
	return relationSupersedeConflictSchema()
}

// RegisterRelationSupersedeConflict installs the schema with o.
// Call once at process startup. "First registration wins": a second
// call without a higher Version returns kernel.ErrSchemaConflict.
func RegisterRelationSupersedeConflict(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, relationSupersedeConflictSchema())
}
