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

// KindTeamMemberGranted is the ontology kind under which
// TeamMemberGrantedEvent is registered. A teamMemberGranted admits a
// principal to the team scope and carries that principal's copy of the
// team data-encryption key (the DEK), wrapped to their x25519 key.
//
// The granting AUTHORITY is the event's signed EventAuthor, never a
// payload field: ev.Author.PrincipalUID is inside CanonicalBytes and is
// signature-verified for team scope on replay, so it cannot be forged.
// The genesis grant is the self-grant (grantee == author) with the
// lowest ULID; the membership fold identifies it structurally, so there
// is deliberately no "genesis" flag in the payload to forge.
const KindTeamMemberGranted = "teamMemberGranted"

// VersionTeamMemberGranted is the semver-shaped schema version.
// The v0.2.0 revision adds the optional Role field (absent ⇒ "member"; all
// v0.1.0 grants remain valid because the fold defaults the absent field).
const VersionTeamMemberGranted = "v0.2.0"

// TeamMemberGrantedEvent is the payload of a teamMemberGranted event —
// a wrapped-DEK ACL row. It names the admitted principal, pins the
// x25519 key the DEK was wrapped to, and carries the sealed DEK for one
// epoch. A re-key (revoke → fresh DEK) emits new grants at the next
// epoch to every remaining member.
type TeamMemberGrantedEvent struct {
	// GranteePrincipalUID is the ULID of the admitted principal (also its
	// person-entity UID). Required.
	GranteePrincipalUID string `json:"granteePrincipalUid" jsonschema:"required,minLength=1,description=ULID of the admitted principal. Required."`

	// GranteeWrapPublicKey is the base64 x25519 public key the DEK was
	// sealed to. Pinned in the payload so a later key change cannot
	// retroactively alter which key a historical grant sealed to. Required.
	GranteeWrapPublicKey string `json:"granteeWrapPublicKey" jsonschema:"required,minLength=1,description=Base64 x25519 public key the DEK was wrapped to. Required."`

	// WrappedDEK is the NaCl anonymous-sealed-box ciphertext of the team
	// DEK for DEKEpoch, sealed to GranteeWrapPublicKey. Base64. Safe to
	// publish in plaintext (only the matching private key opens it). Required.
	WrappedDEK string `json:"wrappedDek" jsonschema:"required,minLength=1,description=Base64 NaCl sealed-box of the team DEK. Required."`

	// DEKEpoch is the epoch of the wrapped DEK. Genesis is epoch 0; a
	// forward re-key on revoke increments it. Required.
	DEKEpoch int `json:"dekEpoch" jsonschema:"minimum=0,description=Epoch of the wrapped DEK (genesis=0; bumped on re-key)."`

	// GranteeSignPublicKey, GranteeSigKeyID, and GranteeDeviceID vouch for the
	// grantee's VERIFICATION identity — the ed25519 key a peer needs to verify
	// the grantee's own team-scope events. A signed grant is how a member's
	// public key reaches the rest of the team: the dispatcher enrolls this
	// triple into the devices table on apply, and the team verifier then
	// resolves the grantee's signature against it. The genesis self-grant
	// (grantee == author) self-vouches its own key — the team's TOFU root,
	// trusted at the same level as the genesis grant itself. Optional: a
	// forward re-key grant for an ALREADY-enrolled member may omit them (the
	// member's key is already in the devices table from their first grant).
	GranteeSignPublicKey string `json:"granteeSignPublicKey,omitempty" jsonschema:"description=Base64 ed25519 public key the grantee signs their team events with. Vouched-for; enrolled on apply."`
	GranteeSigKeyID      string `json:"granteeSigKeyId,omitempty" jsonschema:"description=Stable key id of GranteeSignPublicKey; the verifier resolves grantee signatures by this id."`
	GranteeDeviceID      string `json:"granteeDeviceId,omitempty" jsonschema:"description=The grantee's device id, enrolled alongside the signing key."`

	// GranteeDisplayName is the grantee's self-reported person name, carried so a
	// peer can materialize the member as a TEAM-scoped person entity on apply —
	// without it a self-scoped team fact (subject == a member's principal) lands
	// on the peer with no resolvable subject entity (the member's personal-scope
	// principalRegistered never syncs to the team). It rides inside the encrypted
	// grant event (members already see each other's events); it is NEVER written
	// to the plaintext recipients.jsonl artifact (recipientRow has no name field).
	// Optional: a re-key grant for an already-projected member may omit it.
	GranteeDisplayName string `json:"granteeDisplayName,omitempty" jsonschema:"description=The grantee's self-reported display name; materializes the member as a team-scoped person entity on peers."`

	// Role is the access role assigned to the grantee at admission time.
	// Valid values: "owner", "admin", "member", "reader". Optional: absent
	// means "member" (the historical default before RBAC). Re-key grants for
	// ALREADY-admitted members may omit it; the fold uses the admitting
	// grant's role as the baseline, superseded by explicit teamRoleChanged
	// events. The genesis self-grant always produces an effective role of
	// "owner" regardless of this field.
	Role string `json:"role,omitempty" jsonschema:"enum=owner,enum=admin,enum=member,enum=reader,description=Access role at admission. Absent means member (backwards-compatible default)."`

	// GrantedAt is the instant the grant was authored, UTC. Required.
	GrantedAt time.Time `json:"grantedAt" jsonschema:"required,format=date-time,description=When the grant was authored. RFC 3339 UTC. Required."`
}

// teamMemberGrantedSchema lazily builds the schema once per process.
//
//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue
var teamMemberGrantedSchema = sync.OnceValue(buildTeamMemberGrantedSchema)

func buildTeamMemberGrantedSchema() kernel.Schema {
	r := &jsonschema.Reflector{
		ExpandedStruct:             true,
		RequiredFromJSONSchemaTags: true,
		DoNotReference:             true,
	}
	def := r.Reflect(&TeamMemberGrantedEvent{})
	raw, err := json.Marshal(def)
	if err != nil {
		panic(fmt.Sprintf("ontology: marshal teamMemberGranted schema: %v", err))
	}
	return kernel.Schema{
		Kind:       KindTeamMemberGranted,
		Version:    VersionTeamMemberGranted,
		Definition: raw,
		Provenance: "core",
	}
}

// TeamMemberGrantedSchema returns the cached kernel.Schema.
func TeamMemberGrantedSchema() kernel.Schema {
	return teamMemberGrantedSchema()
}

// RegisterTeamMemberGranted installs the TeamMemberGrantedEvent schema
// with o. Call once at process startup.
func RegisterTeamMemberGranted(ctx context.Context, o kernel.Ontology) error {
	return o.Register(ctx, teamMemberGrantedSchema())
}
