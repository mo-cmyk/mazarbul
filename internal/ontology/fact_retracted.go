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

// KindFactRetracted is the ontology kind under which FactRetractedEvent is
// registered. A factRetracted event is appended by the retractFact tool /
// `maz fact retract` when an agent or user closes the current value of a Fact
// slot (e.g. a status that is no longer true, a preference that changed with no
// replacement). It is the scalar-slot counterpart to relationRetracted: history
// is preserved (the fact row survives, bounded), unlike factForgotten which
// drops the row entirely.
//
// Retraction is modeled as a convergent tombstone keyed by FactUID. The
// dispatcher stamps the targeted fact's retracted_at column and re-derives the
// slot's validity window via reconcileFactSlot, which force-closes a retracted
// row at RetractedAt. Retracting the slot's current value leaves the slot EMPTY
// (it reads as absent) until a newer write reopens it — retraction records "this
// value stopped being true", not "undo", so it does NOT silently fall back to an
// earlier value (re-assert with fact to set a new value). Being a pure fold over
// (subject_uid, state_key, scope) + retracted_at, the close converges across
// paired devices regardless of replay order, so `maz rebuild` reaches the same
// end-state deterministically.
const KindFactRetracted = "factRetracted"

// VersionFactRetracted is the semver-shaped schema version for
// FactRetractedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionFactRetracted = "v0.1.0"

// FactRetractedEvent is the payload of a factRetracted event. It records which
// Fact was closed and the instant the retraction was applied. The Go struct is
// the source of truth; the JSON Schema is reflected from it at first access and
// cached.
type FactRetractedEvent struct {
	// FactUID is the ULID of the Fact row being retracted (closed). Required.
	// This is the specific value the retraction targets — the tool resolves a
	// (subject, stateKey) slot to its open fact uid before appending — so the
	// tombstone is deterministic and uid-keyed.
	FactUID string `json:"factUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Fact being retracted (closed). Required."`

	// RetractedAt is the instant the retraction was applied, in UTC. Required.
	// Serialized as RFC 3339. The fold closes the fact at this instant; it is
	// stamped server-side at invoke time (never in the future), so a retracted
	// value is always excluded from current reads.
	RetractedAt time.Time `json:"retractedAt" jsonschema:"required,format=date-time,description=When the retraction was applied. RFC 3339 UTC. Required."`
}

// factRetractedSchema lazily builds the FactRetractedEvent JSON Schema document
// once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var factRetractedSchema = sync.OnceValue(buildFactRetractedSchema)

// buildFactRetractedSchema reflects FactRetractedEvent into a kernel.Schema.
// Runs once at first call; panics on failure because invopop cannot fail on a
// well-formed in-tree struct.
func buildFactRetractedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&FactRetractedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal factRetracted schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindFactRetracted,
		Version:    VersionFactRetracted,
		Definition: raw,
		Provenance: "core",
	}
}

// FactRetractedSchema returns the cached kernel.Schema for FactRetractedEvent.
func FactRetractedSchema() kernel.Schema {
	return factRetractedSchema()
}

// RegisterFactRetracted installs the FactRetractedEvent schema with o. Call
// once at process startup (or once per test). "First registration wins".
func RegisterFactRetracted(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, factRetractedSchema())
}
