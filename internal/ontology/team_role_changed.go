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

// KindTeamRoleChanged is the ontology kind under which
// TeamRoleChangedEvent is registered. A teamRoleChanged updates the
// access role of an already-admitted team member WITHOUT re-wrapping key
// material — it is a pure authorization event, not a DEK lifecycle event.
//
// The changing AUTHORITY is the signed EventAuthor (ev.Author.PrincipalUID).
// The fold enforces authority at each event's position in ULID order: only
// an owner or admin may author a role change, and only an owner may assign
// or change admin or owner roles. Role changes targeting the genesis
// principal are ignored (the genesis owner is immutably "owner").
const KindTeamRoleChanged = "teamRoleChanged"

// VersionTeamRoleChanged is the semver-shaped schema version.
const VersionTeamRoleChanged = "v0.1.0"

// TeamRoleChangedEvent is the payload of a teamRoleChanged event. It
// names the subject principal and the new role they are assigned. The
// authority constraints (author must be owner/admin; only owner may
// assign admin or owner) are enforced by the membership fold, not here.
type TeamRoleChangedEvent struct {
	// SubjectPrincipalUID is the ULID of the principal whose role is
	// being changed. Required.
	SubjectPrincipalUID string `json:"subjectPrincipalUid" jsonschema:"required,minLength=1,description=ULID of the principal whose role is being changed. Required."`

	// Role is the new role for the subject. Required.
	// Valid values: owner | admin | member | reader.
	Role string `json:"role" jsonschema:"required,enum=owner,enum=admin,enum=member,enum=reader,description=New access role for the subject. Required."`

	// ChangedAt is the instant the role change was authored, UTC. Required.
	ChangedAt time.Time `json:"changedAt" jsonschema:"required,format=date-time,description=When the role change was authored. RFC 3339 UTC. Required."`
}

// teamRoleChangedSchema lazily builds the schema once per process.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var teamRoleChangedSchema = sync.OnceValue(buildTeamRoleChangedSchema)

func buildTeamRoleChangedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&TeamRoleChangedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal teamRoleChanged schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindTeamRoleChanged,
		Version:    VersionTeamRoleChanged,
		Definition: raw,
		Provenance: "core",
	}
}

// TeamRoleChangedSchema returns the cached kernel.Schema.
func TeamRoleChangedSchema() kernel.Schema {
	return teamRoleChangedSchema()
}

// RegisterTeamRoleChanged installs the TeamRoleChangedEvent schema with o.
// Call once at process startup.
func RegisterTeamRoleChanged(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, teamRoleChangedSchema())
}
