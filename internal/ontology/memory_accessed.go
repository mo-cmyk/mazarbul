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

// KindMemoryAccessed is the ontology kind under which MemoryAccessedEvent is
// registered. A memoryAccessed event is emitted on the recall hot path
// whenever a memory (episode, entity, insight, or consolidated memory) is
// surfaced to a caller. It is the passive-reinforcement signal of the
// cognitive memory lifecycle (issue #231).
//
// Like relationRetracted, memoryAccessed is modeled as a commutative,
// monotonic SET keyed by the access event's own UID: a target's
// memory_strength (access_count / first_accessed_at / last_accessed_at) is a
// deterministic FOLD over the full set of accesses targeting it (see
// ReduceAccesses). Set cardinality, MIN, and MAX are commutative, associative,
// and idempotent, so two paired devices replaying the same access events in
// different orders CONVERGE to byte-identical memory_strength state. The
// time-dependent decay term (e^(-lambda*age)) is NOT stored — it is recomputed
// at query time from last_accessed_at, exactly like the recency boost — which
// keeps the stored projection replay-stable.
const KindMemoryAccessed = "memoryAccessed"

// VersionMemoryAccessed is the semver-shaped schema version for
// MemoryAccessedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionMemoryAccessed = "v0.1.0"

// MemoryAccessedEvent is the payload of a memoryAccessed event. It records that
// a single memory was surfaced by recall, and when. The payload is
// intentionally minimal: TargetUID is the fold's grouping key and AccessedAt is
// the only time input, so the two fields are exactly the reorder-invariant
// inputs to the convergent memory_strength fold. Salience is a fixed kernel
// constant (the salience_base column default), not carried per-event, to keep
// the fold one-dimensional. The Go struct is the source of truth; the JSON
// Schema is reflected from it at first access and cached.
type MemoryAccessedEvent struct {
	// TargetUID is the ULID of the surfaced memory (episode, entity, insight,
	// or consolidated memory) that recall returned. Required. This is the
	// GROUPING KEY of the access set: every memoryAccessed sharing a TargetUID
	// reinforces the same memory, and that memory's strength columns are a
	// deterministic fold over the set.
	TargetUID ULID `json:"targetUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the surfaced memory that recall returned. Required. Grouping key for the convergent access set."`

	// AccessedAt is the instant the memory was surfaced by recall, in UTC.
	// Required. Serialized as RFC 3339. MAX across the set becomes
	// last_accessed_at and MIN becomes first_accessed_at, so both fold to
	// order-independent state.
	AccessedAt time.Time `json:"accessedAt" jsonschema:"required,format=date-time,description=When the memory was surfaced by recall. RFC 3339 UTC. Required. MAX over the set is last_accessed_at; MIN is first_accessed_at."`
}

// memoryAccessedSchema lazily builds the MemoryAccessedEvent JSON Schema
// document once and caches it for the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var memoryAccessedSchema = sync.OnceValue(buildMemoryAccessedSchema)

// buildMemoryAccessedSchema reflects MemoryAccessedEvent into a kernel.Schema.
// Runs once at first call; panics on failure because invopop cannot fail on a
// well-formed in-tree struct.
func buildMemoryAccessedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&MemoryAccessedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal memoryAccessed schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindMemoryAccessed,
		Version:    VersionMemoryAccessed,
		Definition: raw,
		Provenance: "core",
	}
}

// MemoryAccessedSchema returns the cached kernel.Schema for MemoryAccessedEvent.
// Useful for tests and consumers that need to inspect the schema without
// registering it.
func MemoryAccessedSchema() kernel.Schema {
	return memoryAccessedSchema()
}

// RegisterMemoryAccessed installs the MemoryAccessedEvent schema with o. Call
// once at process startup (or once per test). The kernel.Ontology contract
// specifies "first registration wins": a second call without a higher Version
// returns kernel.ErrSchemaConflict.
func RegisterMemoryAccessed(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, memoryAccessedSchema())
}
