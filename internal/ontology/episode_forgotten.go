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

// KindEpisodeForgotten is the ontology kind under which
// EpisodeForgottenEvent is registered. The forget MCP tool appends an
// event of this kind; the dispatcher's apply path responds by deleting
// derived state (episodes, mentions, vec0 rows) keyed by EpisodeUID. The
// JSONL log retains both the original Episode and the Forgotten event so
// `maz rebuild` reproduces the same end-state deterministically.
const KindEpisodeForgotten = "episodeForgotten"

// VersionEpisodeForgotten is the semver-shaped schema version.
const VersionEpisodeForgotten = "v0.1.0"

// EpisodeForgottenEvent is the payload of an episodeForgotten event.
// Carries the target episode UID and an optional human-readable reason
// so the audit trail captures why the fact was forgotten (e.g. "user
// invocation via maz forget").
type EpisodeForgottenEvent struct {
	// EpisodeUID is the target episode to forget. Required. Must be a ULID.
	EpisodeUID string `json:"episodeUid" jsonschema:"required,pattern=^[0-9A-HJKMNP-TV-Z]{26}$,description=ULID of the Episode being forgotten. Required."`

	// Reason is an optional free-text explanation. Empty is allowed.
	Reason string `json:"reason,omitempty" jsonschema:"description=Optional free-text reason for the forget. Surfaced in audit/log views."`

	// ForgottenAt is the instant the forget was invoked, in UTC. Required.
	// Serialized as RFC 3339.
	ForgottenAt time.Time `json:"forgottenAt" jsonschema:"required,format=date-time,description=When the forget was invoked. RFC 3339 UTC. Required."`
}

//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var episodeForgottenSchema = sync.OnceValue(buildEpisodeForgottenSchema)

func buildEpisodeForgottenSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&EpisodeForgottenEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal episodeForgotten schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindEpisodeForgotten,
		Version:    VersionEpisodeForgotten,
		Definition: raw,
		Provenance: "core",
	}
}

// EpisodeForgottenSchema returns the cached schema document.
func EpisodeForgottenSchema() kernel.Schema {
	return episodeForgottenSchema()
}

// RegisterEpisodeForgotten installs the schema with o. Call once at
// startup; "first registration wins".
func RegisterEpisodeForgotten(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, episodeForgottenSchema())
}
