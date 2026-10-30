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

// KindEntityUpdated is the ontology kind under which EntityUpdatedEvent is
// registered. The updateEntity MCP tool appends an event of this kind; the
// dispatcher's apply path does a targeted UPDATE on the entities row and
// Entity graph node, changing only the supplied fields (kind and/or
// displayName). The JSONL log retains both the original Entity event and every
// EntityUpdated event so maz rebuild reproduces the same end-state
// deterministically.
const KindEntityUpdated = "entityUpdated"

// VersionEntityUpdated is the semver-shaped schema version.
const VersionEntityUpdated = "v0.1.0"

// EntityUpdatedEvent is the payload of an entityUpdated event. At least one of
// Kind or DisplayName must be non-empty (the dispatcher validates this, and the
// MCP tool rejects a no-op at the write boundary).
type EntityUpdatedEvent struct {
	// EntityUID is the target Entity. Required. Must be a ULID.
	EntityUID string `json:"entityUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Entity being updated. Required."`

	// Kind is the new entity kind in snake_case. Optional; omit to leave
	// unchanged.
	Kind string `json:"kind,omitempty" jsonschema:"description=New entity kind in snake_case. Omit to leave unchanged."`

	// DisplayName is the new human-readable label. Optional; omit to leave
	// unchanged.
	DisplayName string `json:"displayName,omitempty" jsonschema:"description=New human-readable label. Omit to leave unchanged."`

	// UpdatedAt is the instant the update was applied, in UTC. Required.
	// Serialised as RFC 3339.
	UpdatedAt time.Time `json:"updatedAt" jsonschema:"required,format=date-time,description=When the update was applied. RFC 3339 UTC. Required."`
}

//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var entityUpdatedSchema = sync.OnceValue(buildEntityUpdatedSchema)

func buildEntityUpdatedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&EntityUpdatedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal entityUpdated schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindEntityUpdated,
		Version:    VersionEntityUpdated,
		Definition: raw,
		Provenance: "core",
	}
}

// EntityUpdatedSchema returns the cached schema document.
func EntityUpdatedSchema() kernel.Schema {
	return entityUpdatedSchema()
}

// RegisterEntityUpdated installs the schema with o. Call once at startup;
// "first registration wins".
func RegisterEntityUpdated(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, entityUpdatedSchema())
}
