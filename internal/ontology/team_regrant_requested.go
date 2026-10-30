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

// KindTeamRegrantRequested is the ontology kind under which
// TeamRegrantRequestedEvent is registered. A teamRegrantRequested signals
// that a team member's device is missing one or more DEK epochs and is
// requesting that an admitted admin re-wrap those epochs to their key.
//
// This event is purely advisory: it does not change the ACL or rotate the
// DEK. It creates a durable, auditable signal in the team event log that
// flows to all members on the next sync pull. An admin who sees it runs
// `maz team grant` to re-wrap the missing epoch(s), which then flows back
// to the requesting device.
//
// The requesting authority is the signed EventAuthor (ev.Author.PrincipalUID),
// which is signature-verified for the team scope on replay.
const KindTeamRegrantRequested = "teamRegrantRequested"

// VersionTeamRegrantRequested is the semver-shaped schema version.
const VersionTeamRegrantRequested = "v0.1.0"

// TeamRegrantRequestedEvent is the payload of a teamRegrantRequested event.
// It names the requesting principal and the DEK epochs they are missing, so
// the admin knows exactly which epochs need to be re-wrapped without having
// to interrogate the requesting device directly.
type TeamRegrantRequestedEvent struct {
	// RequestingPrincipalUID is the ULID of the principal requesting the
	// re-grant. Carried explicitly so a relay device (that received the event
	// via sync but is not the author) can surface it. Required.
	RequestingPrincipalUID string `json:"requestingPrincipalUid" jsonschema:"required,minLength=1,description=ULID of the requesting principal. Required."`

	// MissingEpochs is the sorted list of DEK epochs the requesting device
	// cannot decrypt (their key is absent from the local keychain). The admin
	// should re-wrap exactly these epochs in the teamMemberGranted events they
	// publish. Required; must have at least one entry.
	MissingEpochs []int `json:"missingEpochs" jsonschema:"required,minItems=1,description=DEK epochs the requesting device is missing. Required."`

	// RequestedAt is the instant the regrant was requested, UTC. Required.
	RequestedAt time.Time `json:"requestedAt" jsonschema:"required,format=date-time,description=When the regrant was requested. RFC 3339 UTC. Required."`
}

// teamRegrantRequestedSchema lazily builds the schema once per process.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var teamRegrantRequestedSchema = sync.OnceValue(buildTeamRegrantRequestedSchema)

func buildTeamRegrantRequestedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&TeamRegrantRequestedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal teamRegrantRequested schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindTeamRegrantRequested,
		Version:    VersionTeamRegrantRequested,
		Definition: raw,
		Provenance: "core",
	}
}

// TeamRegrantRequestedSchema returns the cached kernel.Schema.
func TeamRegrantRequestedSchema() kernel.Schema {
	return teamRegrantRequestedSchema()
}

// RegisterTeamRegrantRequested installs the TeamRegrantRequestedEvent schema
// with o. Call once at process startup.
func RegisterTeamRegrantRequested(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, teamRegrantRequestedSchema())
}
