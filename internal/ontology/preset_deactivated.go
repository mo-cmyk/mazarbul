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

// KindPresetDeactivated is the ontology kind under which PresetDeactivatedEvent
// is registered. A presetDeactivated event is appended by `maz preset uninstall`
// to make a clean cut: the dispatcher DELETES the preset's row from the `presets`
// projection (no inactive flag — the body simply disappears). The kinds and
// predicates the preset seeded REMAIN as plain extension ontology and are not
// retracted, so e.g. crm:Account / champion_of stay resolvable.
const KindPresetDeactivated = "presetDeactivated"

// VersionPresetDeactivated is the semver-shaped schema version for
// PresetDeactivatedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionPresetDeactivated = "v0.1.0"

// PresetDeactivatedEvent is the payload of a presetDeactivated event: a
// clean-cut tombstone naming the preset to drop. It carries no instructions and
// no reason — the active row is simply removed. The Go struct is the source of
// truth; the JSON Schema is reflected from it at first access and cached.
type PresetDeactivatedEvent struct {
	// Name is the preset identifier to deactivate. Required.
	Name string `json:"name" jsonschema:"required,minLength=1,pattern=^[a-z][a-z0-9_-]*$,description=Preset identifier to deactivate."`

	// RemovedAt is the instant of uninstall, in UTC. Required. Serialized as RFC 3339.
	RemovedAt time.Time `json:"removedAt" jsonschema:"required,format=date-time,description=When the preset was deactivated. RFC 3339 UTC. Required."`
}

// presetDeactivatedSchema lazily builds the PresetDeactivatedEvent JSON Schema
// document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var presetDeactivatedSchema = sync.OnceValue(buildPresetDeactivatedSchema)

// buildPresetDeactivatedSchema reflects PresetDeactivatedEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildPresetDeactivatedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&PresetDeactivatedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal presetDeactivated schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindPresetDeactivated,
		Version:    VersionPresetDeactivated,
		Definition: raw,
		Provenance: "core",
	}
}

// PresetDeactivatedSchema returns the cached kernel.Schema for
// PresetDeactivatedEvent. Useful for tests and consumers that need to inspect
// the schema without registering it.
func PresetDeactivatedSchema() kernel.Schema {
	return presetDeactivatedSchema()
}

// RegisterPresetDeactivated installs the PresetDeactivatedEvent schema with o.
// Call once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict.
func RegisterPresetDeactivated(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, presetDeactivatedSchema())
}
