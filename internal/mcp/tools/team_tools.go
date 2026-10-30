package tools

// team_tools.go exposes the team-scope membership lifecycle (issue #213) as
// MCP tools: team_create (genesis), team_grant (admit a member by wrapping the
// current DEK to their pasted x25519 key), and team_revoke (forward re-key).
// Each is a thin adapter over team.Service — the cryptographic authority is the
// signed EventAuthor stamped + verified at the event-log append boundary, so
// these tools only assemble inputs and map service sentinels to error codes.
// Read-only membership listing is CLI-local (reads the ACL), not an MCP tool.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/team"
)

// TeamOps is the team lifecycle surface the tools depend on. *team.Service
// satisfies it; tests pass a fake.
type TeamOps interface {
	Create(ctx context.Context, remoteURL string) (team.CreateResult, error)
	Join(ctx context.Context, remoteURL string) (team.JoinResult, error)
	Grant(ctx context.Context, key *team.JoinKey, role string) (team.GrantResult, error)
	Revoke(ctx context.Context, principalUID, reason string) (team.RevokeResult, error)
	Members(ctx context.Context) ([]team.Member, error)
	LocalJoinKey(ctx context.Context) (team.JoinKey, error)
	RequestRegrant(ctx context.Context, missingEpochs []int) (team.RequestRegrantResult, error)
	RoleSet(ctx context.Context, subjectUID, role string) (team.RoleSetResult, error)
}

// mapTeamError maps a team.Service sentinel to a stable MCP error code.
func mapTeamError(action string, err error) error {
	switch {
	case errors.Is(err, team.ErrNotMember), errors.Is(err, kernel.ErrPermission):
		// ErrPermission surfaces from the append boundary (enforceTeamWriteAuthority)
		// — e.g. a concurrent revoke lands between this op's membership check and its
		// write. It is an authorization decision, not an internal fault: map it to
		// E_PERMISSION (matching scope_write.go's mapAppendError) so agents don't
		// blind-retry and the real reason stays visible.
		return mcp.NewPermissionError(fmt.Sprintf("%s: %v", action, err), err)
	case errors.Is(err, team.ErrInsufficientRole):
		return mcp.NewPermissionError(fmt.Sprintf("%s: %v", action, err), err)
	case errors.Is(err, team.ErrNoTeam):
		return mcp.NewNotFoundError(fmt.Sprintf("%s: %v", action, err))
	case errors.Is(err, team.ErrTeamExists), errors.Is(err, team.ErrSelfUnbound):
		return mcp.NewValidationError(fmt.Sprintf("%s: %v", action, err), nil)
	case errors.Is(err, config.ErrUserConfigNotFound):
		// The data directory has not been initialised yet (maz init was never run).
		// This is a stable precondition failure, not a transient bug: map it to
		// E_VALIDATION so agents do not blind-retry it as E_INTERNAL.
		return mcp.NewValidationError(
			fmt.Sprintf("%s: maz is not initialised — run `maz init` first (%v)", action, err),
			nil,
		)
	default:
		return mcp.NewInternalError(fmt.Errorf("%s: %w", action, err))
	}
}

// --- team_create ---

const teamCreateInputSchema = `{
  "type": "object",
  "properties": {
    "remote": {"type": "string", "description": "Optional git remote URL to pin for the team scope (e.g. git@github.com:org/team-brain.git)."}
  },
  "additionalProperties": false
}`

const teamCreateOutputSchema = `{
  "type": "object",
  "properties": {
    "genesisPrincipalUid": {"type": "string", "description": "Principal UID admitted as the team's genesis owner (this device's bound principal)."},
    "dekEpoch":            {"type": "integer", "description": "Epoch of the freshly-minted data-encryption key (always 0 at genesis)."},
    "status":              {"type": "string", "enum": ["created"], "description": "Always \"created\" on success."}
  },
  "required": ["genesisPrincipalUid", "dekEpoch", "status"],
  "additionalProperties": false
}`

// TeamCreate is the MCP tool that provisions team genesis.
type TeamCreate struct{ ops TeamOps }

// NewTeamCreate constructs the tool bound to ops.
func NewTeamCreate(ops TeamOps) *TeamCreate { return &TeamCreate{ops: ops} }

// Name implements mcp.Tool.
func (*TeamCreate) Name() string { return "team_create" }

// Description implements mcp.Tool.
func (*TeamCreate) Description() string {
	return "Create the team scope (genesis): mints a fresh per-team data-encryption key, admits this device's principal as the genesis owner, and switches the team scope to encrypted envelope mode. Fails if a team already exists."
}

// InputSchema implements mcp.Tool.
func (*TeamCreate) InputSchema() json.RawMessage { return json.RawMessage(teamCreateInputSchema) }

// OutputSchema implements mcp.Tool.
func (*TeamCreate) OutputSchema() json.RawMessage { return json.RawMessage(teamCreateOutputSchema) }

type teamCreateInput struct {
	Remote string `json:"remote,omitempty"`
}

type teamCreateOutput struct {
	GenesisPrincipalUID string `json:"genesisPrincipalUid"`
	DEKEpoch            int    `json:"dekEpoch"`
	Status              string `json:"status"`
}

// Invoke implements mcp.Tool.
func (t *TeamCreate) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in teamCreateInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	res, err := t.ops.Create(ctx, in.Remote)
	if err != nil {
		return nil, mapTeamError("team_create", err)
	}
	return mustMarshalOutput(teamCreateOutput{
		GenesisPrincipalUID: res.GenesisPrincipalUID,
		DEKEpoch:            res.DEKEpoch,
		Status:              "created",
	})
}

// --- team_join ---

const teamJoinInputSchema = `{
  "type": "object",
  "properties": {
    "remote": {"type": "string", "minLength": 1, "description": "The team's shared git remote URL to sync through (e.g. git@github.com:org/team-brain.git)."}
  },
  "required": ["remote"],
  "additionalProperties": false
}`

const teamJoinOutputSchema = `{
  "type": "object",
  "properties": {
    "remoteUrl":  {"type": "string", "description": "The team remote this device is now configured to sync with."},
    "nextSteps":  {"type": "array", "items": {"type": "string"}, "description": "What to do next to finish joining."},
    "status":     {"type": "string", "enum": ["joined"], "description": "Always \"joined\" on success."}
  },
  "required": ["remoteUrl", "status"],
  "additionalProperties": false
}`

// TeamJoin is the MCP tool that points this device at an existing team's remote
// in envelope mode WITHOUT minting a genesis grant — the join-side counterpart
// to team_create.
type TeamJoin struct{ ops TeamOps }

// NewTeamJoin constructs the tool bound to ops.
func NewTeamJoin(ops TeamOps) *TeamJoin { return &TeamJoin{ops: ops} }

// Name implements mcp.Tool.
func (*TeamJoin) Name() string { return "team_join" }

// Description implements mcp.Tool.
func (*TeamJoin) Description() string {
	return "Point this device at an existing team's encrypted remote so it can sync the shared brain, WITHOUT creating a new team. Use this (not team_create) to join a team someone else created. After joining, run team_key and send the blob to an admitted member to be granted, then sync pull to bootstrap the team key."
}

// InputSchema implements mcp.Tool.
func (*TeamJoin) InputSchema() json.RawMessage { return json.RawMessage(teamJoinInputSchema) }

// OutputSchema implements mcp.Tool.
func (*TeamJoin) OutputSchema() json.RawMessage { return json.RawMessage(teamJoinOutputSchema) }

type teamJoinInput struct {
	Remote string `json:"remote"`
}

type teamJoinOutput struct {
	RemoteURL string   `json:"remoteUrl"`
	NextSteps []string `json:"nextSteps,omitempty"`
	Status    string   `json:"status"`
}

// Invoke implements mcp.Tool.
func (t *TeamJoin) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in teamJoinInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.Remote == "" {
		return nil, mcp.NewValidationError("team_join: a team remote URL is required", nil)
	}
	res, err := t.ops.Join(ctx, in.Remote)
	if err != nil {
		return nil, mapTeamError("team_join", err)
	}
	return mustMarshalOutput(teamJoinOutput{
		RemoteURL: res.RemoteURL,
		NextSteps: []string{
			"Run team_key and send the printed join-key blob to an admitted team member.",
			"After they grant you, run `maz sync pull` to bootstrap the team key and decrypt the shared brain.",
		},
		Status: "joined",
	})
}

// --- team_grant ---

const teamGrantInputSchema = `{
  "type": "object",
  "properties": {
    "joinKey": {"type": "string", "minLength": 1, "description": "The grantee's join-key blob (the opaque token from team_key / 'maz team key'). It carries their principal UID, wrap public key, and signing identity — this is the trust input; obtain it out of band."},
    "role":    {"type": "string", "enum": ["owner", "admin", "member", "reader"], "description": "Access role to assign the grantee. Defaults to \"member\" when absent."},
    "label":   {"type": "string", "description": "Optional human label for local logging; never serialized into the synced recipients.jsonl."}
  },
  "required": ["joinKey"],
  "additionalProperties": false
}`

const teamGrantOutputSchema = `{
  "type": "object",
  "properties": {
    "granteePrincipalUid": {"type": "string", "description": "Principal UID that was admitted."},
    "role":                {"type": "string", "description": "Effective access role assigned to the grantee."},
    "dekEpoch":            {"type": "integer", "description": "Highest (current) DEK epoch wrapped to the grantee."},
    "grantedEpochs":       {"type": "array", "items": {"type": "integer"}, "description": "Every DEK epoch wrapped to the grantee (ascending). A full-history grant covers 0..dekEpoch; it is a subset only when this device lacks a prior epoch's DEK."},
    "status":              {"type": "string", "enum": ["granted"], "description": "Always \"granted\" on success."}
  },
  "required": ["granteePrincipalUid", "role", "dekEpoch", "grantedEpochs", "status"],
  "additionalProperties": false
}`

// TeamGrant is the MCP tool that admits a member.
type TeamGrant struct{ ops TeamOps }

// NewTeamGrant constructs the tool bound to ops.
func NewTeamGrant(ops TeamOps) *TeamGrant { return &TeamGrant{ops: ops} }

// Name implements mcp.Tool.
func (*TeamGrant) Name() string { return "team_grant" }

// Description implements mcp.Tool.
func (*TeamGrant) Description() string {
	return "Admit a principal to the team by wrapping the current team data-encryption key to their pasted x25519 public key. The grantee unwraps it on their next sync pull. Requires the caller to hold at least admin role. The optional role parameter sets the grantee's access level (default: member)."
}

// InputSchema implements mcp.Tool.
func (*TeamGrant) InputSchema() json.RawMessage { return json.RawMessage(teamGrantInputSchema) }

// OutputSchema implements mcp.Tool.
func (*TeamGrant) OutputSchema() json.RawMessage { return json.RawMessage(teamGrantOutputSchema) }

type teamGrantInput struct {
	JoinKey string `json:"joinKey"`
	Role    string `json:"role,omitempty"`
	Label   string `json:"label,omitempty"`
}

type teamGrantOutput struct {
	GranteePrincipalUID string `json:"granteePrincipalUid"`
	Role                string `json:"role"`
	DEKEpoch            int    `json:"dekEpoch"`
	GrantedEpochs       []int  `json:"grantedEpochs"`
	Status              string `json:"status"`
}

// Invoke implements mcp.Tool.
func (t *TeamGrant) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in teamGrantInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	jk, err := team.DecodeJoinKey(in.JoinKey)
	if err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("team_grant: %v", err), nil)
	}
	res, err := t.ops.Grant(ctx, &jk, in.Role)
	if err != nil {
		return nil, mapTeamError("team_grant", err)
	}
	role := in.Role
	if role == "" {
		role = "member"
	}
	return mustMarshalOutput(teamGrantOutput{
		GranteePrincipalUID: res.GranteePrincipalUID,
		Role:                role,
		DEKEpoch:            res.DEKEpoch,
		GrantedEpochs:       res.GrantedEpochs,
		Status:              "granted",
	})
}

// --- team_revoke ---

const teamRevokeInputSchema = `{
  "type": "object",
  "properties": {
    "principalUid": {"type": "string", "minLength": 1, "description": "ULID of the principal to revoke."},
    "reason":       {"type": "string", "description": "Optional human-readable revocation reason recorded in the audit trail."}
  },
  "required": ["principalUid"],
  "additionalProperties": false
}`

const teamRevokeOutputSchema = `{
  "type": "object",
  "properties": {
    "revokedPrincipalUid": {"type": "string", "description": "Principal UID that was revoked."},
    "newDekEpoch":         {"type": "integer", "description": "Epoch of the fresh DEK the remaining members were re-keyed to; the revoked principal has no grant here."},
    "rewrappedTo":         {"type": "array", "items": {"type": "string"}, "description": "Principal UIDs the new DEK was re-wrapped to (the remaining members)."},
    "checklist":           {"type": "array", "items": {"type": "string"}, "description": "Post-revoke operator checklist: what Mazarbul did and what you must still do."}
  },
  "required": ["revokedPrincipalUid", "newDekEpoch", "checklist"],
  "additionalProperties": false
}`

// TeamRevoke is the MCP tool that revokes a member with a forward re-key.
type TeamRevoke struct{ ops TeamOps }

// NewTeamRevoke constructs the tool bound to ops.
func NewTeamRevoke(ops TeamOps) *TeamRevoke { return &TeamRevoke{ops: ops} }

// Name implements mcp.Tool.
func (*TeamRevoke) Name() string { return "team_revoke" }

// Description implements mcp.Tool.
func (*TeamRevoke) Description() string {
	return "Revoke a team member and forward re-key the team: appends the revocation, mints a fresh data-encryption key at the next epoch, and re-wraps it to every remaining member. Returns a checklist — you must separately remove the principal's git-host access, which Mazarbul cannot do."
}

// InputSchema implements mcp.Tool.
func (*TeamRevoke) InputSchema() json.RawMessage { return json.RawMessage(teamRevokeInputSchema) }

// OutputSchema implements mcp.Tool.
func (*TeamRevoke) OutputSchema() json.RawMessage { return json.RawMessage(teamRevokeOutputSchema) }

type teamRevokeInput struct {
	PrincipalUID string `json:"principalUid"`
	Reason       string `json:"reason,omitempty"`
}

type teamRevokeOutput struct {
	RevokedPrincipalUID string   `json:"revokedPrincipalUid"`
	NewDEKEpoch         int      `json:"newDekEpoch"`
	RewrappedTo         []string `json:"rewrappedTo,omitempty"`
	Checklist           []string `json:"checklist"`
}

// revokeChecklist is the dual-boundary disclosure returned after a revoke.
func revokeChecklist(res team.RevokeResult) []string {
	return []string{
		fmt.Sprintf("Forward re-key done: remaining members re-keyed to DEK epoch %d; %q cannot read epoch %d onward.", res.NewDEKEpoch, res.RevokedPrincipalUID, res.NewDEKEpoch),
		"You MUST separately remove the revoked principal's git-host collaborator/write access — Mazarbul cannot revoke remote git access.",
		"Chunks the revoked principal already pulled (prior epochs) remain readable to them; forward secrecy applies only to new (epoch+1) writes.",
	}
}

// Invoke implements mcp.Tool.
func (t *TeamRevoke) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in teamRevokeInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	res, err := t.ops.Revoke(ctx, in.PrincipalUID, in.Reason)
	if err != nil {
		return nil, mapTeamError("team_revoke", err)
	}
	return mustMarshalOutput(teamRevokeOutput{
		RevokedPrincipalUID: res.RevokedPrincipalUID,
		NewDEKEpoch:         res.NewDEKEpoch,
		RewrappedTo:         res.RewrappedTo,
		Checklist:           revokeChecklist(res),
	})
}

// --- team_members ---

const teamMembersInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const teamMembersOutputSchema = `{
  "type": "object",
  "properties": {
    "members": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "principalUid":  {"type": "string", "description": "Admitted principal UID (opaque; no PII)."},
          "role":          {"type": "string", "description": "Effective access role: owner | admin | member | reader."},
          "dekEpoch":      {"type": "integer", "description": "DEK epoch this member is keyed to."},
          "wrapPublicKey": {"type": "string", "description": "Member's base64 x25519 wrap public key."},
          "isGenesis":     {"type": "boolean", "description": "Whether this is the genesis owner (the TOFU root)."}
        },
        "required": ["principalUid", "role", "dekEpoch", "isGenesis"],
        "additionalProperties": false
      }
    }
  },
  "required": ["members"],
  "additionalProperties": false
}`

// TeamMembers lists the admitted team ACL. Read-only and PII-free (opaque
// principal UIDs + wrap keys + epoch), it routes through the daemon like every
// other read so it never contends for the store lock.
type TeamMembers struct{ ops TeamOps }

// NewTeamMembers constructs the tool bound to ops.
func NewTeamMembers(ops TeamOps) *TeamMembers { return &TeamMembers{ops: ops} }

// Name implements mcp.Tool.
func (*TeamMembers) Name() string { return "team_members" }

// Description implements mcp.Tool.
func (*TeamMembers) Description() string {
	return "List the admitted team members (the derived ACL): opaque principal UID, DEK epoch, wrap public key, and whether each is the genesis owner. Read-only and contains no display names or other PII."
}

// InputSchema implements mcp.Tool.
func (*TeamMembers) InputSchema() json.RawMessage { return json.RawMessage(teamMembersInputSchema) }

// OutputSchema implements mcp.Tool.
func (*TeamMembers) OutputSchema() json.RawMessage { return json.RawMessage(teamMembersOutputSchema) }

type teamMemberRow struct {
	PrincipalUID  string `json:"principalUid"`
	Role          string `json:"role"`
	DEKEpoch      int    `json:"dekEpoch"`
	WrapPublicKey string `json:"wrapPublicKey,omitempty"`
	IsGenesis     bool   `json:"isGenesis"`
}

type teamMembersOutput struct {
	Members []teamMemberRow `json:"members"`
}

// Invoke implements mcp.Tool.
func (t *TeamMembers) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	members, err := t.ops.Members(ctx)
	if err != nil {
		return nil, mapTeamError("team_members", err)
	}
	rows := make([]teamMemberRow, 0, len(members))
	for _, m := range members {
		role := m.Role
		if role == "" {
			role = "member"
		}
		rows = append(rows, teamMemberRow{
			PrincipalUID:  m.PrincipalUID,
			Role:          role,
			DEKEpoch:      m.DEKEpoch,
			WrapPublicKey: m.WrapPublicKey,
			IsGenesis:     m.IsGenesis,
		})
	}
	return mustMarshalOutput(teamMembersOutput{Members: rows})
}

// --- team_key ---

const teamKeyInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const teamKeyOutputSchema = `{
  "type": "object",
  "properties": {
    "principalUid":  {"type": "string", "description": "This device's bound principal UID."},
    "wrapPublicKey": {"type": "string", "description": "This device's base64 x25519 wrap public key."},
    "joinKey":       {"type": "string", "description": "Opaque join-key blob to hand an admitted member (maz team grant --join-key <blob>). Carries the principal UID, wrap public key, and signing identity; nothing in it is secret."}
  },
  "required": ["principalUid", "wrapPublicKey", "joinKey"],
  "additionalProperties": false
}`

// TeamKey returns this device's join request — the principal UID + wrap public
// key a prospective member hands to an admitted member out of band so they can
// be granted. Read-only; nothing it returns is secret.
type TeamKey struct{ ops TeamOps }

// NewTeamKey constructs the tool bound to ops.
func NewTeamKey(ops TeamOps) *TeamKey { return &TeamKey{ops: ops} }

// Name implements mcp.Tool.
func (*TeamKey) Name() string { return "team_key" }

// Description implements mcp.Tool.
func (*TeamKey) Description() string {
	return "Show this device's team join request: its bound principal UID and base64 x25519 wrap public key. Send both to an admitted team member so they can grant you (maz team grant --principal <uid> --wrap-pubkey <key>). Read-only; neither value is secret."
}

// InputSchema implements mcp.Tool.
func (*TeamKey) InputSchema() json.RawMessage { return json.RawMessage(teamKeyInputSchema) }

// OutputSchema implements mcp.Tool.
func (*TeamKey) OutputSchema() json.RawMessage { return json.RawMessage(teamKeyOutputSchema) }

type teamKeyOutput struct {
	PrincipalUID  string `json:"principalUid"`
	WrapPublicKey string `json:"wrapPublicKey"`
	JoinKey       string `json:"joinKey"`
}

// Invoke implements mcp.Tool.
func (t *TeamKey) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	jk, err := t.ops.LocalJoinKey(ctx)
	if err != nil {
		return nil, mapTeamError("team_key", err)
	}
	blob, err := jk.Encode()
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("team_key: encode join key: %w", err))
	}
	return mustMarshalOutput(teamKeyOutput{
		PrincipalUID:  jk.PrincipalUID,
		WrapPublicKey: jk.WrapPublicKey,
		JoinKey:       blob,
	})
}

// --- team_request_regrant ---

const teamRequestRegrantInputSchema = `{
  "type": "object",
  "properties": {
    "missingEpochs": {"type": "array", "items": {"type": "integer", "minimum": 0}, "minItems": 1, "description": "DEK epochs missing from this device's local keychain. Obtain by running maz doctor (sync.dek_coverage row) or maz team key --request-regrant."}
  },
  "required": ["missingEpochs"],
  "additionalProperties": false
}`

const teamRequestRegrantOutputSchema = `{
  "type": "object",
  "properties": {
    "eventUid":      {"type": "string", "description": "UID of the appended teamRegrantRequested event."},
    "missingEpochs": {"type": "array", "items": {"type": "integer"}, "description": "DEK epochs reported as missing in the request."},
    "status":        {"type": "string", "enum": ["requested"], "description": "Always \"requested\" on success."}
  },
  "required": ["eventUid", "missingEpochs", "status"],
  "additionalProperties": false
}`

// TeamRequestRegrant appends a teamRegrantRequested event to the team event
// log. This signals to admitted team admins that this device is missing the
// listed DEK epochs and needs them re-wrapped. The event flows to all members
// on the next sync pull.
type TeamRequestRegrant struct{ ops TeamOps }

// NewTeamRequestRegrant constructs the tool bound to ops.
func NewTeamRequestRegrant(ops TeamOps) *TeamRequestRegrant { return &TeamRequestRegrant{ops: ops} }

// Name implements mcp.Tool.
func (*TeamRequestRegrant) Name() string { return "team_request_regrant" }

// Description implements mcp.Tool.
func (*TeamRequestRegrant) Description() string {
	return "Signal to admitted team admins that this device is missing one or more DEK epochs and needs them re-wrapped. Appends a teamRegrantRequested event to the team log; the event flows to all members on the next sync pull. An admin who sees it runs `maz team grant` to re-wrap the missing epochs."
}

// InputSchema implements mcp.Tool.
func (*TeamRequestRegrant) InputSchema() json.RawMessage {
	return json.RawMessage(teamRequestRegrantInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*TeamRequestRegrant) OutputSchema() json.RawMessage {
	return json.RawMessage(teamRequestRegrantOutputSchema)
}

type teamRequestRegrantOutput struct {
	EventUID      string `json:"eventUid"`
	MissingEpochs []int  `json:"missingEpochs"`
	Status        string `json:"status"`
}

// Invoke implements mcp.Tool.
func (t *TeamRequestRegrant) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in struct {
		MissingEpochs []int `json:"missingEpochs"`
	}
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("team_request_regrant: decode arguments: %v", err), nil)
	}
	if len(in.MissingEpochs) == 0 {
		return nil, mcp.NewValidationError("team_request_regrant: missingEpochs must not be empty", nil)
	}
	res, err := t.ops.RequestRegrant(ctx, in.MissingEpochs)
	if err != nil {
		return nil, mapTeamError("team_request_regrant", err)
	}
	return mustMarshalOutput(teamRequestRegrantOutput{
		EventUID:      res.EventUID,
		MissingEpochs: res.MissingEpochs,
		Status:        "requested",
	})
}

// --- team_role_set ---

const teamRoleSetInputSchema = `{
  "type": "object",
  "properties": {
    "principalUid": {"type": "string", "minLength": 1, "description": "ULID of the admitted team member whose role is being changed."},
    "role":         {"type": "string", "enum": ["owner", "admin", "member", "reader"], "description": "New access role. Assigning admin or owner requires the caller to hold owner role."}
  },
  "required": ["principalUid", "role"],
  "additionalProperties": false
}`

const teamRoleSetOutputSchema = `{
  "type": "object",
  "properties": {
    "eventUid":            {"type": "string", "description": "UID of the appended teamRoleChanged event."},
    "subjectPrincipalUid": {"type": "string", "description": "Principal whose role was changed."},
    "role":                {"type": "string", "description": "New effective role."},
    "status":              {"type": "string", "enum": ["role_set"], "description": "Always \"role_set\" on success."}
  },
  "required": ["eventUid", "subjectPrincipalUid", "role", "status"],
  "additionalProperties": false
}`

// TeamRoleSet changes an admitted member's access role without re-wrapping
// key material. The appended teamRoleChanged event is fold-governed: every
// peer applies it in ULID order, so the effective role converges identically.
type TeamRoleSet struct{ ops TeamOps }

// NewTeamRoleSet constructs the tool bound to ops.
func NewTeamRoleSet(ops TeamOps) *TeamRoleSet { return &TeamRoleSet{ops: ops} }

// Name implements mcp.Tool.
func (*TeamRoleSet) Name() string { return "team_role_set" }

// Description implements mcp.Tool.
func (*TeamRoleSet) Description() string {
	return "Change an admitted team member's access role (owner | admin | member | reader) without re-wrapping key material. Requires the caller to hold at least admin role; assigning admin or owner requires owner. The change is recorded as a teamRoleChanged event and converges identically on every peer."
}

// InputSchema implements mcp.Tool.
func (*TeamRoleSet) InputSchema() json.RawMessage { return json.RawMessage(teamRoleSetInputSchema) }

// OutputSchema implements mcp.Tool.
func (*TeamRoleSet) OutputSchema() json.RawMessage { return json.RawMessage(teamRoleSetOutputSchema) }

type teamRoleSetInput struct {
	PrincipalUID string `json:"principalUid"`
	Role         string `json:"role"`
}

type teamRoleSetOutput struct {
	EventUID            string `json:"eventUid"`
	SubjectPrincipalUID string `json:"subjectPrincipalUid"`
	Role                string `json:"role"`
	Status              string `json:"status"`
}

// Invoke implements mcp.Tool.
func (t *TeamRoleSet) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in teamRoleSetInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.PrincipalUID == "" {
		return nil, mcp.NewValidationError("team_role_set: principalUid is required", nil)
	}
	if in.Role == "" {
		return nil, mcp.NewValidationError("team_role_set: role is required", nil)
	}
	res, err := t.ops.RoleSet(ctx, in.PrincipalUID, in.Role)
	if err != nil {
		return nil, mapTeamError("team_role_set", err)
	}
	return mustMarshalOutput(teamRoleSetOutput{
		EventUID:            res.EventUID,
		SubjectPrincipalUID: res.SubjectPrincipalUID,
		Role:                res.Role,
		Status:              "role_set",
	})
}

// mustMarshalOutput marshals a tool response, mapping a (practically
// impossible) marshal failure to E_INTERNAL.
func mustMarshalOutput(v any) (json.RawMessage, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}
