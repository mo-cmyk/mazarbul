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

// KindContextUnpinned is the ontology kind under which ContextUnpinnedEvent is
// registered. A contextUnpinned event retires a previously pinned context note
// (KindContextPinned) or correction (KindContextCorrected) so it stops
// surfacing in the cold-start brief.
//
// Like relationRetracted, retirement is modeled as a commutative, monotonic
// TOMBSTONE SET rather than an in-place delete: each contextUnpinned is one
// element of the set keyed by the unpin event's own UID, and a pin's materialized
// `active` column is a deterministic FOLD over (the pin exists) AND (no tombstone
// targets it). Set-union is commutative, associative, and idempotent, so two
// paired devices — or a cross-device replay that delivers the unpin before the
// pin — CONVERGE to the same active bit regardless of order. The dispatcher
// persists each tombstone in the context_pin_tombstones table (one row per unpin
// event) and recomputes the pin's active flag; the pin ROW is preserved (audit /
// resurrect), never dropped.
//
// Scope is carried on the event ENVELOPE (kernel.Event.Scope) and materialized
// via scopeValue, like every other projected kind.
const KindContextUnpinned = "contextUnpinned"

// VersionContextUnpinned is the semver-shaped schema version for
// ContextUnpinnedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionContextUnpinned = "v0.1.0"

// ContextUnpinnedEvent is the payload of a contextUnpinned event — a convergent
// tombstone retiring one pin or correction. The Go struct is the source of
// truth; the JSON Schema is reflected from it at first access and cached.
type ContextUnpinnedEvent struct {
	// PinUID is the ULID of the context_pins row being retired (the PinUID of a
	// prior contextPinned, or the CorrectionUID of a prior contextCorrected).
	// Required. This is the GROUPING KEY of the tombstone set: every
	// contextUnpinned sharing a PinUID retires the same row, and the row's
	// active flag is a deterministic fold over that set. A tombstone for a row
	// that does not (yet) exist is retained and applies the moment the pin row
	// lands (order-independent), so unpin-before-pin still converges to inactive.
	PinUID ULID `json:"pinUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the pin or correction being retired. Required. Grouping key for the convergent tombstone set."`

	// UnpinnedAt is the instant the user retired the pin, in UTC. Required.
	// Serialized as RFC 3339. (AIP-142 *Time field, never *At.)
	UnpinnedAt time.Time `json:"unpinnedAt" jsonschema:"required,format=date-time,description=When the user retired the pin. RFC 3339 UTC. Required."`
}

// contextUnpinnedSchema lazily builds the ContextUnpinnedEvent JSON Schema
// document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var contextUnpinnedSchema = sync.OnceValue(buildContextUnpinnedSchema)

// buildContextUnpinnedSchema reflects ContextUnpinnedEvent into a kernel.Schema.
// Runs once at first call; panics on failure because invopop cannot fail on a
// well-formed in-tree struct.
func buildContextUnpinnedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&ContextUnpinnedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal contextUnpinned schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindContextUnpinned,
		Version:    VersionContextUnpinned,
		Definition: raw,
		Provenance: "core",
	}
}

// ContextUnpinnedSchema returns the cached kernel.Schema for
// ContextUnpinnedEvent. Useful for tests and consumers that need to inspect the
// schema without registering it.
func ContextUnpinnedSchema() kernel.Schema {
	return contextUnpinnedSchema()
}

// RegisterContextUnpinned installs the ContextUnpinnedEvent schema with o. Call
// once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict.
func RegisterContextUnpinned(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, contextUnpinnedSchema())
}
