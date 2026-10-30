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

// KindContextCorrected is the ontology kind under which ContextCorrectedEvent is
// registered. A contextCorrected event records a correction OVERLAY a user laid
// over a surfaced item: the surfaced item's id plus the corrected text the user
// wants shown in its place (e.g. the brief surfaced "works at Acme" and the user
// corrects it to "left Acme in May"). It is the inbox/brief sibling of a pin.
//
// Corrections share the context_pins projection table (migration 0042) with a
// pin_type='correction' discriminator, so the single Navigator.ContextPins read
// returns pins and corrections together. The dispatcher upserts via INSERT OR
// IGNORE keyed on CorrectionUID, so a Rebuild replay of the same event is a
// no-op. Corrections are append-only: re-correcting the same TargetId mints a
// new CorrectionUID row, and the surfacing layer resolves the overlay as the
// most recent correction per TargetId (latest create_time wins) — a pure read-
// time fold, so the projection stays replay-stable. A correction can be retired
// by a contextUnpinned referencing its CorrectionUID, exactly like a pin.
//
// Scope is carried on the event ENVELOPE (kernel.Event.Scope) and materialized
// via scopeValue, like every other projected kind.
const KindContextCorrected = "contextCorrected"

// VersionContextCorrected is the semver-shaped schema version for
// ContextCorrectedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionContextCorrected = "v0.1.0"

// ContextCorrectedEvent is the payload of a contextCorrected event — a user's
// correction overlay on a surfaced item. The Go struct is the source of truth;
// the JSON Schema is reflected from it at first access and cached.
type ContextCorrectedEvent struct {
	// CorrectionUID is the correction's own ULID identity. Required. It is the
	// context_pins row primary key and the contextPins/{uid} resource name, and
	// the handle a later contextUnpinned references to retire the correction.
	// A fresh random ULID minted by the emitter (corrections are append-only,
	// not deduped by TargetId); the dispatcher upserts via INSERT OR IGNORE
	// keyed on this uid so a Rebuild replay of the same event is a no-op.
	CorrectionUID ULID `json:"correctionUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identity of this correction. Required. Fresh random ULID minted by the emitter."`

	// TargetID is the id of the surfaced item being corrected — whatever opaque
	// handle the surfacing layer assigned to the item the user is overriding
	// (an entity uid, a fact state_key address, a brief line id). Required,
	// non-empty. The surfacing layer groups corrections by this id to resolve
	// the latest overlay.
	TargetID string `json:"targetId" jsonschema:"required,minLength=1,description=Id of the surfaced item being corrected. Required and non-empty. Grouped by the surfacing layer to resolve the latest overlay."`

	// Text is the corrected text to show in place of the surfaced item.
	// Required, non-empty.
	Text string `json:"text" jsonschema:"required,minLength=1,description=The corrected text to show in place of the surfaced item. Required and non-empty."`

	// CorrectedAt is the instant the user made the correction, in UTC. Required.
	// Serialized as RFC 3339. (AIP-142 *Time field, never *At.)
	CorrectedAt time.Time `json:"correctedAt" jsonschema:"required,format=date-time,description=When the user made the correction. RFC 3339 UTC. Required."`
}

// contextCorrectedSchema lazily builds the ContextCorrectedEvent JSON Schema
// document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var contextCorrectedSchema = sync.OnceValue(buildContextCorrectedSchema)

// buildContextCorrectedSchema reflects ContextCorrectedEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildContextCorrectedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&ContextCorrectedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal contextCorrected schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindContextCorrected,
		Version:    VersionContextCorrected,
		Definition: raw,
		Provenance: "core",
	}
}

// ContextCorrectedSchema returns the cached kernel.Schema for
// ContextCorrectedEvent. Useful for tests and consumers that need to inspect the
// schema without registering it.
func ContextCorrectedSchema() kernel.Schema {
	return contextCorrectedSchema()
}

// RegisterContextCorrected installs the ContextCorrectedEvent schema with o.
// Call once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict.
func RegisterContextCorrected(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, contextCorrectedSchema())
}
