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

// KindRelationRetracted is the ontology kind under which
// RelationRetractedEvent is registered. A relationRetracted event is
// appended either (a) by the resolver whenever a newer Relation invalidates
// an older one via the supersede-on-contradict logic (functional predicates)
// or the LLM contradiction check (non-functional predicates) — these carry a
// SupersededByUID; or (b) by the unrelate tool / `maz unrelate` when an agent
// or user explicitly closes a relation that ended (e.g. someone left a job) —
// a pure temporal close with an empty SupersededByUID, closed by valid_until.
// Both preserve history: the relation row survives with a bounded valid_until,
// unlike relationForgotten which drops the derived row entirely.
//
// Unlike the session-serial relationSuperseded event it replaces, retraction
// is modeled as a commutative, monotonic tombstone set: each retraction is
// one element of the set keyed by the retraction event's own UID, and a
// relation's valid_until / superseded_by_uid columns are a deterministic
// FOLD over the full set of tombstones targeting it. Set-union is
// commutative, associative, and idempotent, so two paired devices that close
// the same relation arm CONVERGE to identical state regardless of the order
// the events replay in. The dispatcher persists each tombstone in the
// relation_retractions table (one row per retraction event) and recomputes
// the materialized columns; the JSONL log + that table let `maz rebuild`
// reach the same end-state deterministically.
const KindRelationRetracted = "relationRetracted"

// VersionRelationRetracted is the semver-shaped schema version for
// RelationRetractedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionRelationRetracted = "v0.1.0"

// RelationRetractedEvent is the payload of a relationRetracted event. It
// records which Relation was invalidated and which newer Relation replaces
// it, plus the instant the retraction was applied. The Go struct is the
// source of truth; the JSON Schema is reflected from it at first access and
// cached.
type RelationRetractedEvent struct {
	// RelationUID is the ULID of the Relation being retracted (closed).
	// Required. This is the GROUPING KEY of the tombstone set: every
	// relationRetracted sharing a RelationUID closes the same arm, and the
	// relation's materialized columns are a deterministic fold over that set.
	RelationUID ULID `json:"relationUid" jsonschema:"required,description=ULID of the Relation being retracted (closed). Required. Grouping key for the convergent tombstone set."`

	// SupersededByUID is the ULID of the newer Relation that replaces the
	// retracted one, when there is one. OPTIONAL: a pure temporal close — a
	// relation that simply ENDED with no replacement (e.g. someone left a job
	// on a date) — carries an empty superseder and is closed by valid_until
	// alone, the fold leaving superseded_by_uid NULL (ReduceRetractions counts
	// only non-empty superseders; valid_until still folds from RetractedAt).
	// This is the explicit-close path the unrelate tool emits; the resolver's
	// supersede-on-contradict path always sets it. The lowest ULID across the
	// tombstone set that carries a superseder becomes the canonical
	// superseded_by_uid, so the pointer converges.
	SupersededByUID ULID `json:"supersededByUid,omitempty" jsonschema:"description=ULID of the newer Relation that replaces the retracted one. Optional: omit for a pure temporal close (the relation simply ended on a date with no replacement). Lowest ULID across the tombstone set is the canonical superseded_by_uid."`

	// RetractedAt is the instant the retraction was applied, in UTC.
	// Required. Serialized as RFC 3339. The EARLIEST RetractedAt across the
	// tombstone set (folded against any extractor-set validity bound)
	// becomes the canonical valid_until, so the closing instant converges.
	RetractedAt time.Time `json:"retractedAt" jsonschema:"required,format=date-time,description=When the retraction was applied. The earliest across the tombstone set becomes valid_until. RFC 3339 UTC. Required."`
}

// relationRetractedSchema lazily builds the RelationRetractedEvent JSON
// Schema document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var relationRetractedSchema = sync.OnceValue(buildRelationRetractedSchema)

// buildRelationRetractedSchema reflects RelationRetractedEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildRelationRetractedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&RelationRetractedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal relationRetracted schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindRelationRetracted,
		Version:    VersionRelationRetracted,
		Definition: raw,
		Provenance: "core",
	}
}

// RelationRetractedSchema returns the cached kernel.Schema for
// RelationRetractedEvent. Useful for tests and consumers that need to
// inspect the schema without registering it.
func RelationRetractedSchema() kernel.Schema {
	return relationRetractedSchema()
}

// RegisterRelationRetracted installs the RelationRetractedEvent schema with
// o. Call once at process startup (or once per test). The kernel.Ontology
// contract specifies "first registration wins": a second call without a
// higher Version returns kernel.ErrSchemaConflict.
func RegisterRelationRetracted(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, relationRetractedSchema())
}
