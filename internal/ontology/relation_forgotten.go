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

// KindRelationForgotten is the ontology kind under which
// RelationForgottenEvent is registered. The forget MCP tool appends an
// event of this kind when the caller targets a specific RelationUID
// instead of an EpisodeUID; the dispatcher's apply path deletes the
// relations row and its RELATES_TO graph edge, keyed by RelationUID. The
// JSONL log retains the original RelationCandidatesProposed event and
// this tombstone so `maz rebuild` reaches the same end-state
// deterministically.
const KindRelationForgotten = "relationForgotten"

// VersionRelationForgotten is the semver-shaped schema version.
const VersionRelationForgotten = "v0.1.0"

// RelationForgottenEvent is the payload of a relationForgotten event.
// Carries the target relation UID and an optional human-readable reason
// so the audit trail captures why the fact was forgotten.
type RelationForgottenEvent struct {
	// RelationUID is the target relation to forget. Required. Must be a ULID.
	RelationUID string `json:"relationUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Relation being forgotten. Required."`

	// Reason is an optional free-text explanation. Empty is allowed.
	Reason string `json:"reason,omitempty" jsonschema:"description=Optional free-text reason for the forget. Surfaced in audit/log views."`

	// ForgottenAt is the instant the forget was invoked, in UTC. Required.
	// Serialized as RFC 3339.
	ForgottenAt time.Time `json:"forgottenAt" jsonschema:"required,format=date-time,description=When the forget was invoked. RFC 3339 UTC. Required."`
}

//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var relationForgottenSchema = sync.OnceValue(buildRelationForgottenSchema)

func buildRelationForgottenSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&RelationForgottenEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal relationForgotten schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindRelationForgotten,
		Version:    VersionRelationForgotten,
		Definition: raw,
		Provenance: "core",
	}
}

// RelationForgottenSchema returns the cached schema document.
func RelationForgottenSchema() kernel.Schema {
	return relationForgottenSchema()
}

// RegisterRelationForgotten installs the schema with o. Call once at
// startup; "first registration wins".
func RegisterRelationForgotten(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, relationForgottenSchema())
}
