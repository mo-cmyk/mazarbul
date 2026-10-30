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

// KindFactForgotten is the ontology kind under which FactForgottenEvent is
// registered. The forget MCP tool appends an event of this kind when the caller
// targets a FactUID; the dispatcher's apply path hard-deletes the facts row and
// reconciles the slot it occupied so the supersede chain re-derives without it.
// Unlike factRetracted (which preserves the row, bounded), forget drops the
// derived row entirely. The JSONL log retains the original Fact event and this
// tombstone so `maz rebuild` reaches the same end-state deterministically.
const KindFactForgotten = "factForgotten"

// VersionFactForgotten is the semver-shaped schema version.
const VersionFactForgotten = "v0.1.0"

// FactForgottenEvent is the payload of a factForgotten event. Carries the
// target fact UID and an optional human-readable reason so the audit trail
// captures why the fact was forgotten.
type FactForgottenEvent struct {
	// FactUID is the target fact to forget. Required. Must be a ULID.
	FactUID string `json:"factUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Fact being forgotten. Required."`

	// Reason is an optional free-text explanation. Empty is allowed.
	Reason string `json:"reason,omitempty" jsonschema:"description=Optional free-text reason for the forget. Surfaced in audit/log views."`

	// ForgottenAt is the instant the forget was invoked, in UTC. Required.
	// Serialized as RFC 3339.
	ForgottenAt time.Time `json:"forgottenAt" jsonschema:"required,format=date-time,description=When the forget was invoked. RFC 3339 UTC. Required."`
}

//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var factForgottenSchema = sync.OnceValue(buildFactForgottenSchema)

func buildFactForgottenSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&FactForgottenEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal factForgotten schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindFactForgotten,
		Version:    VersionFactForgotten,
		Definition: raw,
		Provenance: "core",
	}
}

// FactForgottenSchema returns the cached schema document.
func FactForgottenSchema() kernel.Schema {
	return factForgottenSchema()
}

// RegisterFactForgotten installs the schema with o. Call once at startup;
// "first registration wins".
func RegisterFactForgotten(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, factForgottenSchema())
}
