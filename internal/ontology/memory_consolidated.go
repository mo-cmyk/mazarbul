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

// KindMemoryConsolidated is the ontology kind under which
// MemoryConsolidatedEvent is registered. A memoryConsolidated event is appended
// by the autonomous consolidation job: it groups recent episodes by their
// cited entity, LLM-compresses each group into a typed consolidated memory, and
// dedups against existing consolidated memories before appending (issue #231).
//
// Like insightSynthesized, the event MUST carry a non-empty SourceUids list —
// the episode ULIDs that were compressed — so every consolidated memory
// cryptographically cites its sources (auditable provenance, contrast the
// competitor's opaque LLM blobs). The dispatcher rejects an empty list as a
// hard error.
//
// The dispatcher projects each event into the consolidated_memories table
// (migration 0039) keyed by MemoryUID with INSERT OR IGNORE idempotency, so
// `maz rebuild` reconstructs the projection deterministically from the JSONL
// log.
const KindMemoryConsolidated = "memoryConsolidated"

// VersionMemoryConsolidated is the semver-shaped schema version for
// MemoryConsolidatedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionMemoryConsolidated = "v0.1.0"

// MemoryConsolidatedEvent is the payload of a memoryConsolidated event — a
// consolidation-job-derived compression of a group of related episodes. The Go
// struct is the source of truth; the JSON Schema is reflected from it at first
// access and cached.
type MemoryConsolidatedEvent struct {
	// MemoryUID is the consolidated memory's own ULID identity. Required. It is
	// the consolidated_memories row primary key; the dispatcher upserts via
	// INSERT OR IGNORE keyed on it so a Rebuild replay of the same event is a
	// no-op.
	MemoryUID ULID `json:"memoryUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID identity of this consolidated memory. Required."`

	// Summary is the LLM-compressed prose of the consolidated group. Required,
	// non-empty.
	Summary string `json:"summary" jsonschema:"required,minLength=1,description=The compressed consolidated-memory prose. Required and non-empty."`

	// MemoryType is the kind of consolidated memory, drawn from an OPEN taxonomy
	// (free-text, intentionally not an enum). Required. Examples: "semantic",
	// "procedural", "summary".
	MemoryType string `json:"memoryType" jsonschema:"required,minLength=1,description=Kind of consolidated memory. Open taxonomy free-text e.g. semantic|procedural|summary. Required."`

	// SourceUids is the set of episode ULIDs that were compressed into this
	// memory (auditable provenance). Required and NON-EMPTY: the dispatcher
	// rejects an empty list as a hard error, and the consolidation job rejects
	// any uid the LLM did not draw from the offered group.
	SourceUids []ULID `json:"sourceUids" jsonschema:"required,minItems=1,description=Episode ULIDs compressed into this memory. Required and non-empty (auditable provenance)."`

	// CitedEntityUID is the entity the group was clustered around. Required.
	CitedEntityUID ULID `json:"citedEntityUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=Entity the consolidated group was clustered around. Required."`

	// Confidence is the writer's confidence in the consolidation, in the closed
	// interval 0..1. Required.
	Confidence float64 `json:"confidence" jsonschema:"required,minimum=0,maximum=1,description=Confidence in the consolidation. Range 0..1. Required."`

	// ConsolidatedAt is the instant the memory was consolidated, in UTC.
	// Required. Serialized as RFC 3339. (AIP-142 *Time field, never *At.)
	ConsolidatedAt time.Time `json:"consolidatedAt" jsonschema:"required,format=date-time,description=When the memory was consolidated. RFC 3339 UTC. Required."`
}

// memoryConsolidatedSchema lazily builds the MemoryConsolidatedEvent JSON
// Schema document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var memoryConsolidatedSchema = sync.OnceValue(buildMemoryConsolidatedSchema)

// buildMemoryConsolidatedSchema reflects MemoryConsolidatedEvent into a
// kernel.Schema. Runs once at first call; panics on failure because invopop
// cannot fail on a well-formed in-tree struct.
func buildMemoryConsolidatedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&MemoryConsolidatedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal memoryConsolidated schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindMemoryConsolidated,
		Version:    VersionMemoryConsolidated,
		Definition: raw,
		Provenance: "core",
	}
}

// MemoryConsolidatedSchema returns the cached kernel.Schema for
// MemoryConsolidatedEvent. Useful for tests and consumers that need to inspect
// the schema without registering it.
func MemoryConsolidatedSchema() kernel.Schema {
	return memoryConsolidatedSchema()
}

// RegisterMemoryConsolidated installs the MemoryConsolidatedEvent schema with o.
// Call once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict.
func RegisterMemoryConsolidated(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, memoryConsolidatedSchema())
}
