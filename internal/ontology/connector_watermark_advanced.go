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

// KindConnectorWatermarkAdvanced is the ontology kind under which
// ConnectorWatermarkAdvancedEvent is registered. A connectorWatermarkAdvanced
// event records that an ingestion source (issue #211) has durably consumed up
// to an opaque, source-defined cursor for one (connector, stream) pair. It is
// the resume primitive: a later ingest loads the highest-sequence watermark and
// pulls only the delta after it, so re-ingests are cheap.
//
// The cursor is OPAQUE to the orchestrator and to the dispatcher — only the
// owning Source interprets its bytes. Both layers VALIDATE it on every advance
// (size cap, well-formed JSON object, and a strictly-monotonic Sequence) so a
// forged, replayed, or regressing cursor can never silently reset the watermark
// or over-pull: the orchestrator FAILs the job, and the dispatcher refuses the
// projection write. The dispatcher projects each event into the
// connector_watermarks table (migration 0035) keyed by (connector, stream) as a
// head-state projection: last-writer-wins by Sequence, so `maz rebuild`
// reconstructs the projection deterministically (it always converges to the
// highest-sequence cursor regardless of replay order).
const KindConnectorWatermarkAdvanced = "connectorWatermarkAdvanced"

// VersionConnectorWatermarkAdvanced is the semver-shaped schema version for
// ConnectorWatermarkAdvancedEvent. Must be a full MAJOR.MINOR.PATCH triplet.
const VersionConnectorWatermarkAdvanced = "v0.1.0"

// MaxCursorBytes is the hard size cap on an ingestion source's opaque cursor.
// A cursor larger than this FAILs cursor validation (orchestrator-side it fails
// the job; dispatcher-side it refuses the projection write). The cap bounds the
// event-log and projection footprint of an untrusted, source-defined blob.
const MaxCursorBytes = 4096

// ConnectorWatermarkAdvancedEvent is the payload of a connectorWatermarkAdvanced
// event: the durable resume position for one (connector, stream) pair. The Go
// struct is the source of truth; the JSON Schema is reflected from it at first
// access and cached.
type ConnectorWatermarkAdvancedEvent struct {
	// Connector is the ingestion source identifier, e.g. "local-git". Required.
	// It is the projection key together with Stream and matches the Source.Name()
	// the orchestrator tags the carrying episodes with ("connector:<Connector>").
	Connector string `json:"connector" jsonschema:"required,minLength=1,description=Ingestion source identifier e.g. local-git. Required. Projection key with stream."`

	// Stream is the sub-channel within the connector, e.g. "commits" or
	// "pull-requests". Optional (empty = the source's single default stream); it
	// is the second half of the (connector, stream) projection key.
	Stream string `json:"stream,omitempty" jsonschema:"description=Sub-channel within the connector e.g. commits|pull-requests. Optional; empty = the single default stream."`

	// Cursor is the source-defined opaque resume position, a JSON object. Required.
	// The orchestrator and dispatcher never interpret it; they only validate its
	// size (<= MaxCursorBytes) and that it is a well-formed JSON object.
	Cursor json.RawMessage `json:"cursor" jsonschema:"required,type=object,description=Opaque source-defined resume position. A JSON object <= 4096 bytes. Required. Never interpreted outside the owning Source."`

	// Sequence is the strictly-monotonic advance counter for this (connector,
	// stream). Required. Each advance must carry a Sequence strictly greater than
	// the persisted watermark's; a regression FAILs validation (no silent reset).
	Sequence uint64 `json:"sequence" jsonschema:"required,description=Strictly-monotonic advance counter for this (connector,stream). Required. A regression fails validation."`

	// AdvanceTime is the instant the watermark advanced, in UTC. Required.
	// Serialized as RFC 3339. (AIP-142 *Time field, never *At.)
	AdvanceTime time.Time `json:"advanceTime" jsonschema:"required,format=date-time,description=When the watermark advanced. RFC 3339 UTC. Required."`
}

// connectorWatermarkAdvancedSchema lazily builds the
// ConnectorWatermarkAdvancedEvent JSON Schema document once and caches it for
// the process lifetime.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var connectorWatermarkAdvancedSchema = sync.OnceValue(buildConnectorWatermarkAdvancedSchema)

// buildConnectorWatermarkAdvancedSchema reflects ConnectorWatermarkAdvancedEvent
// into a kernel.Schema. Runs once at first call; panics on failure because
// invopop cannot fail on a well-formed in-tree struct.
func buildConnectorWatermarkAdvancedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&ConnectorWatermarkAdvancedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal connectorWatermarkAdvanced schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindConnectorWatermarkAdvanced,
		Version:    VersionConnectorWatermarkAdvanced,
		Definition: raw,
		Provenance: "core",
	}
}

// ConnectorWatermarkAdvancedSchema returns the cached kernel.Schema for
// ConnectorWatermarkAdvancedEvent. Useful for tests and consumers that need to
// inspect the schema without registering it.
func ConnectorWatermarkAdvancedSchema() kernel.Schema {
	return connectorWatermarkAdvancedSchema()
}

// RegisterConnectorWatermarkAdvanced installs the ConnectorWatermarkAdvancedEvent
// schema with o. Call once at process startup (or once per test). The
// kernel.Ontology contract specifies "first registration wins": a second call
// without a higher Version returns kernel.ErrSchemaConflict.
func RegisterConnectorWatermarkAdvanced(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, connectorWatermarkAdvancedSchema())
}

// ValidateCursorShape enforces the two structural guards an opaque ingestion
// cursor must always satisfy, independent of sequencing: a size cap
// (<= MaxCursorBytes) and a well-formed JSON object. Both the live orchestrator
// path and the dispatcher apply path call this; it is replay-safe (no sequence
// state), so the dispatcher can hard-fail a malformed cursor without breaking
// `maz rebuild` idempotency.
func ValidateCursorShape(cursor json.RawMessage) error {
	if len(cursor) > MaxCursorBytes {
		return fmt.Errorf("connector cursor: size %d exceeds cap %d", len(cursor), MaxCursorBytes)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(cursor, &probe); err != nil {
		return fmt.Errorf("connector cursor: not a well-formed JSON object: %w", err)
	}
	return nil
}

// ValidateCursor enforces the three guards an opaque ingestion cursor must
// satisfy on every LIVE advance: the structural guards (ValidateCursorShape)
// plus strict Sequence monotonicity relative to prevSequence. A nil error means
// next is a legal advance; any non-nil error must FAIL the job (no silent reset,
// no over-pull). PrevSequence is 0 for a first connect.
//
// This is the orchestrator-side check. The dispatcher does NOT re-run the
// monotonicity arm (a Rebuild replays events whose sequence equals the persisted
// one, which is legitimate); it relies on a sequence-gated UPSERT so a replayed
// or regressing event is a silent no-op rather than a poisoned projection.
//
// Done relaxes monotonicity to allow a terminal re-advance at the same Sequence
// (a source that reports Done having produced no new cursor): when done is true
// nextSequence == prevSequence is accepted, but a regression never is.
func ValidateCursor(prevSequence, nextSequence uint64, cursor json.RawMessage, done bool) error {
	if err := ValidateCursorShape(cursor); err != nil {
		return err
	}
	switch {
	case nextSequence > prevSequence:
		return nil
	case done && nextSequence == prevSequence:
		return nil
	default:
		return fmt.Errorf("connector cursor: sequence %d not strictly greater than %d (non-monotonic)", nextSequence, prevSequence)
	}
}
