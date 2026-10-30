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

// KindTeamMemberRevoked is the ontology kind under which
// TeamMemberRevokedEvent is registered. A teamMemberRevoked removes a
// principal from the team's current ACL. As with teamMemberGranted, the
// revoking AUTHORITY is the signed EventAuthor (ev.Author.PrincipalUID),
// not a payload field.
//
// Revocation is forward-only: it removes the subject from the current
// membership but does not retroactively un-admit members the subject
// previously granted, nor invalidate events the subject authored before
// their own revoke (the threat model excludes a malicious insider with
// push + key). The operator pairs a revoke with a forward re-key (a
// fresh DEK epoch wrapped only to remaining members) so the revoked
// principal cannot read NEW chunks.
const KindTeamMemberRevoked = "teamMemberRevoked"

// VersionTeamMemberRevoked is the semver-shaped schema version.
const VersionTeamMemberRevoked = "v0.1.0"

// TeamMemberRevokedEvent is the payload of a teamMemberRevoked event.
type TeamMemberRevokedEvent struct {
	// RevokedPrincipalUID is the ULID of the principal being removed.
	// Required.
	RevokedPrincipalUID string `json:"revokedPrincipalUid" jsonschema:"required,minLength=1,description=ULID of the principal being revoked. Required."`

	// Reason is an optional human note for the audit trail. Optional.
	Reason string `json:"reason,omitempty" jsonschema:"description=Optional human-readable revocation reason."`

	// RevokedAt is the instant the revoke was authored, UTC. Required.
	RevokedAt time.Time `json:"revokedAt" jsonschema:"required,format=date-time,description=When the revoke was authored. RFC 3339 UTC. Required."`
}

// teamMemberRevokedSchema lazily builds the schema once per process.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var teamMemberRevokedSchema = sync.OnceValue(buildTeamMemberRevokedSchema)

func buildTeamMemberRevokedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&TeamMemberRevokedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal teamMemberRevoked schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindTeamMemberRevoked,
		Version:    VersionTeamMemberRevoked,
		Definition: raw,
		Provenance: "core",
	}
}

// TeamMemberRevokedSchema returns the cached kernel.Schema.
func TeamMemberRevokedSchema() kernel.Schema {
	return teamMemberRevokedSchema()
}

// RegisterTeamMemberRevoked installs the TeamMemberRevokedEvent schema
// with o. Call once at process startup.
func RegisterTeamMemberRevoked(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, teamMemberRevokedSchema())
}
