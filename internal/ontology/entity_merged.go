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

// KindEntityMerged is the ontology kind under which EntityMergedEvent is
// registered. The entityMerged MCP tool appends an event of this kind; the
// dispatcher's apply path repoints all relations, facts, and mentions from
// source to target entity within a single SQLite transaction, then removes the
// source entity row. The JSONL log retains both the original Entity events and
// the EntityMerged event so maz rebuild reproduces the same end-state
// deterministically.
const KindEntityMerged = "entityMerged"

// VersionEntityMerged is the semver-shaped schema version.
const VersionEntityMerged = "v0.1.0"

// EntityMergedEvent is the payload of an entityMerged event.
type EntityMergedEvent struct {
	// SourceEntityUID is the entity being absorbed. Required. Must be a ULID.
	// After the merge the source entity row is deleted; all relations, facts,
	// and mentions that referenced it are repointed to TargetEntityUID.
	SourceEntityUID string `json:"sourceEntityUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Entity being absorbed. Required."`

	// TargetEntityUID is the surviving entity. Required. Must be a ULID.
	// Must be distinct from SourceEntityUID.
	TargetEntityUID string `json:"targetEntityUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Entity that survives the merge. Required."`

	// MergedAt is the instant the merge was applied, in UTC. Required.
	// Serialised as RFC 3339.
	MergedAt time.Time `json:"mergedAt" jsonschema:"required,format=date-time,description=When the merge was applied. RFC 3339 UTC. Required."`
}

//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var entityMergedSchema = sync.OnceValue(buildEntityMergedSchema)

func buildEntityMergedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&EntityMergedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal entityMerged schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindEntityMerged,
		Version:    VersionEntityMerged,
		Definition: raw,
		Provenance: "core",
	}
}

// EntityMergedSchema returns the cached schema document.
func EntityMergedSchema() kernel.Schema {
	return entityMergedSchema()
}

// RegisterEntityMerged installs the schema with o. Call once at startup;
// "first registration wins".
func RegisterEntityMerged(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, entityMergedSchema())
}
