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

// KindContextPinned is the ontology kind under which ContextPinnedEvent is
// registered. A contextPinned event records a user pin: a piece of text the
// user wants surfaced in the cold-start context brief (e.g. "I'm mid-migration
// off Postgres — don't suggest new Postgres features"). Pins are durable,
// user-authored steering that the brief assembler reads back on session start.
//
// The dispatcher projects each event into the context_pins table (migration
// 0042) keyed by PinUID with INSERT OR IGNORE idempotency, so `maz rebuild`
// reconstructs the projection deterministically from the JSONL log. A pin's
// live/retired state is the convergent FOLD over its contextUnpinned tombstone
// set (see KindContextUnpinned), not a value the pin event carries, so two
// devices that pin-then-unpin in either order converge to the same active bit.
//
// Scope is carried on the event ENVELOPE (kernel.Event.Scope), exactly like
// every other projected kind, and materialized via scopeValue — there is no
// scope field in the payload.
const KindContextPinned = "contextPinned"

// VersionContextPinned is the semver-shaped schema version for
// ContextPinnedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionContextPinned = "v0.1.0"

// ContextPinnedEvent is the payload of a contextPinned event — a user pin for
// the cold-start context brief. The Go struct is the source of truth; the JSON
// Schema is reflected from it at first access and cached.
type ContextPinnedEvent struct {
	// PinUID is the pin's own ULID identity. Required. It is the context_pins
	// row primary key and the contextPins/{uid} resource name, and the handle
	// a later contextUnpinned event references to retire the pin. Like an
	// insight's uid it is a fresh random ULID minted by the emitter (pins are
	// not deduped by a key); the dispatcher upserts via INSERT OR IGNORE keyed
	// on this uid so a Rebuild replay of the same event is a no-op.
	PinUID ULID `json:"pinUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identity of this pin. Required. Fresh random ULID minted by the emitter; referenced by contextUnpinned."`

	// Text is the natural-language steering note to surface in the brief.
	// Required, non-empty.
	Text string `json:"text" jsonschema:"required,minLength=1,description=The natural-language steering note to surface in the cold-start brief. Required and non-empty."`

	// TargetRef is an optional resource the pin is about — an entity resource
	// name (e.g. "entities/01HZ...") or any opaque reference the surfacing layer
	// understands. Optional: a free-floating pin (general steering not tied to
	// one record) omits it.
	TargetRef string `json:"targetRef,omitempty" jsonschema:"description=Optional resource the pin is about e.g. an entity resource name. Omit for a free-floating pin. Optional."`

	// PinnedAt is the instant the user created the pin, in UTC. Required.
	// Serialized as RFC 3339. (AIP-142 *Time field, never *At.)
	PinnedAt time.Time `json:"pinnedAt" jsonschema:"required,format=date-time,description=When the user created the pin. RFC 3339 UTC. Required."`
}

// contextPinnedSchema lazily builds the ContextPinnedEvent JSON Schema document
// once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var contextPinnedSchema = sync.OnceValue(buildContextPinnedSchema)

// buildContextPinnedSchema reflects ContextPinnedEvent into a kernel.Schema.
// Runs once at first call; panics on failure because invopop cannot fail on a
// well-formed in-tree struct.
func buildContextPinnedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&ContextPinnedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal contextPinned schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindContextPinned,
		Version:    VersionContextPinned,
		Definition: raw,
		Provenance: "core",
	}
}

// ContextPinnedSchema returns the cached kernel.Schema for ContextPinnedEvent.
// Useful for tests and consumers that need to inspect the schema without
// registering it.
func ContextPinnedSchema() kernel.Schema {
	return contextPinnedSchema()
}

// RegisterContextPinned installs the ContextPinnedEvent schema with o. Call once
// at process startup (or once per test). The kernel.Ontology contract specifies
// "first registration wins": a second call without a higher Version returns
// kernel.ErrSchemaConflict.
func RegisterContextPinned(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, contextPinnedSchema())
}
