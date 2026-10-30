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

// KindEntityDeleted is the ontology kind under which EntityDeletedEvent is
// registered. The entityDeleted MCP tool appends an event of this kind; the
// dispatcher's apply path removes the entity row, its identifiers, and its
// name/summary vectors, then detaches the graph node. Unlike entityMerged it
// has no target — the entity is gone, not absorbed — so callers must first
// detach or cascade its relations (the tool refuses to delete an entity that
// still has active edges unless cascade is requested). The JSONL log retains
// both the original Entity event and the EntityDeleted tombstone so
// maz rebuild reproduces the same end-state deterministically.
const KindEntityDeleted = "entityDeleted"

// VersionEntityDeleted is the semver-shaped schema version.
const VersionEntityDeleted = "v0.1.0"

// EntityDeletedEvent is the payload of an entityDeleted event.
type EntityDeletedEvent struct {
	// EntityUID is the entity being deleted. Required. Must be a ULID. After the
	// delete the entity row, its identifiers, and its vectors are removed and the
	// graph node is detached.
	EntityUID string `json:"entityUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Entity being deleted. Required."`

	// Reason is an optional free-text justification, surfaced in audit views.
	Reason string `json:"reason,omitempty" jsonschema:"description=Optional free-text reason for the deletion. Surfaced in audit/log views."`

	// DeletedAt is the instant the delete was applied, in UTC. Required.
	// Serialised as RFC 3339.
	DeletedAt time.Time `json:"deletedAt" jsonschema:"required,format=date-time,description=When the delete was applied. RFC 3339 UTC. Required."`
}

//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var entityDeletedSchema = sync.OnceValue(buildEntityDeletedSchema)

func buildEntityDeletedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&EntityDeletedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal entityDeleted schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindEntityDeleted,
		Version:    VersionEntityDeleted,
		Definition: raw,
		Provenance: "core",
	}
}

// EntityDeletedSchema returns the cached schema document.
func EntityDeletedSchema() kernel.Schema {
	return entityDeletedSchema()
}

// RegisterEntityDeleted installs the schema with o. Call once at startup;
// "first registration wins".
func RegisterEntityDeleted(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, entityDeletedSchema())
}
