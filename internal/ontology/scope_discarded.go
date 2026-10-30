package ontology

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/invopop/jsonschema"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// KindScopeDiscarded is the ontology kind under which ScopeDiscardedEvent is
// registered. A scopeDiscarded event removes the draft-scoped projection rows
// for TargetUID, and cascades to provenance-linked derived rows. Only
// draft-scoped rows are affected; the event log itself is append-only.
//
// Rebuild-deterministic: on a full log replay the original draft event projects
// the row, then this discard event replays and removes it — the derived store
// converges to the same state.
const KindScopeDiscarded = "scopeDiscarded"

// VersionScopeDiscarded is the semver-shaped schema version for ScopeDiscardedEvent.
const VersionScopeDiscarded = "v0.1.0"

// ScopeDiscardedEvent is the payload of a scopeDiscarded event. The dispatcher
// DELETEs the draft-scoped projection rows for TargetUID and any derived rows
// (mentions, relations, facts) whose source_event_uid is TargetUID.
type ScopeDiscardedEvent struct {
	// TargetUID is the UID of the draft episode to discard. Required.
	TargetUID string `json:"targetUid" jsonschema:"required,minLength=1,description=UID of the draft episode to discard. Required."`

	// Reason is an optional human-readable explanation for the discard.
	Reason string `json:"reason,omitempty" jsonschema:"description=Optional human-readable reason for the discard."`
}

// scopeDiscardedSchema lazily builds and caches the ScopeDiscardedEvent JSON Schema.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var scopeDiscardedSchema = sync.OnceValue(buildScopeDiscardedSchema)

func buildScopeDiscardedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&ScopeDiscardedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal scopeDiscarded schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindScopeDiscarded,
		Version:    VersionScopeDiscarded,
		Definition: raw,
		Provenance: "core",
	}
}

// ScopeDiscardedSchema returns the cached kernel.Schema for ScopeDiscardedEvent.
func ScopeDiscardedSchema() kernel.Schema { return scopeDiscardedSchema() }

// RegisterScopeDiscarded installs the ScopeDiscardedEvent schema with o.
func RegisterScopeDiscarded(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, scopeDiscardedSchema())
}
