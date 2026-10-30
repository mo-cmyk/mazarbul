package cli

// team.go — `maz team` parent + create/grant/revoke/members subcommands.
//
// Phase E of #213 ("Team scopes"). Each subcommand is a thin client over
// one of the four already-registered MCP tools (team_create, team_grant,
// team_revoke, team_members), dispatched through the shared
// runToolViaDaemonOrInProcess seam so it works whether a daemon owns the
// store or the call runs in-process. Output honors --output (text|json|
// ndjson) exactly like remember.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	gittransport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// errTeamDisabledMsg is emitted when a team_* tool is absent from the
// wired tool slice, indicating the identity / team layer is not
// provisioned. Mirrors the remember.go "run maz init" wording.
const errTeamDisabledMsg = `team tools not available; run "maz init" to provision your identity`

func newTeamCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "team",
		Short: "Manage a shared, end-to-end-encrypted team scope",
		Long: `maz team manages the team scope: a shared brain encrypted under a
per-team data-encryption key (DEK) that is wrapped to each member's
public key and distributed via a publishable recipients.jsonl artifact.

A team scope is ALWAYS encrypted (envelope mode). There is intentionally
no plaintext team scope — a plaintext shared brain is exactly what the
"project" scope already provides. Publishing the wrapped DEKs is safe:
only a holder of the matching wrap private key can recover the DEK.

Subcommands:
  create  — bootstrap a NEW team scope on this device (becomes genesis)
  join    — point this device at an EXISTING team's remote (no genesis)
  key     — print this device's join key (blob a granter consumes)
  grant   — admit a member by wrapping the DEK history to their key
  revoke  — remove a member and rotate the DEK forward (re-key)
  members — list admitted principals + DEK epoch (no PII)
  role    — change an admitted member's role (owner|admin|member|reader)
  devices — list or revoke a member's enrolled signing devices
  pin     — inspect or clear the genesis trust anchor (TEAM.json)

Examples:
  # Bootstrap a NEW team and push it to a shared git remote
  maz team create --remote git@github.com:acme/brain-team.git

  # Join an EXISTING team someone else created
  maz team join --remote git@github.com:acme/brain-team.git

  # Admit a member from the join key they produced with 'maz team key'
  maz team grant --from-file alice.joinkey --label "Alice (laptop)"

  # Change a member's role
  maz team role set 01ARZ3NDEKTSV4RRFFQ69G5FAV --role admin

  # List a member's enrolled devices, then revoke one
  maz team devices list
  maz team devices revoke <deviceId>

  # Inspect the genesis trust anchor
  maz team pin

  # Revoke a member and rotate forward
  maz team revoke --principal 01ARZ3NDEKTSV4RRFFQ69G5FAV --reason "left company"

  # List members as JSON for a script
  maz team members --output json`,
	}
	cmd.AddCommand(newTeamCreateCmd())
	cmd.AddCommand(newTeamJoinCmd())
	cmd.AddCommand(newTeamGrantCmd())
	cmd.AddCommand(newTeamRevokeCmd())
	cmd.AddCommand(newTeamMembersCmd())
	cmd.AddCommand(newTeamKeyCmd())
	cmd.AddCommand(newTeamRoleCmd())
	cmd.AddCommand(newTeamDevicesCmd())
	cmd.AddCommand(newTeamPinCmd())
	return cmd
}

// teamDataDir resolves the active data dir the same way remember.go does:
// the persistent --data-dir flag, falling back to the config default.
func teamDataDir(cmd *cobra.Command) string {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	return dataDir
}

// dispatchTeamTool marshals nothing (reqBody is pre-marshalled by the
// caller) and routes a team_* tool call through the shared
// daemon-or-in-process seam. A tool absent from wired.Tools surfaces as a
// silentError(1) carrying the actionable "run maz init" hint.
func dispatchTeamTool(cmd *cobra.Command, toolName string, reqBody json.RawMessage) (json.RawMessage, error) {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)
	dataDir := teamDataDir(cmd)
	return runToolViaDaemonOrInProcess(ctx, cmd, dataDir, logger, toolName, reqBody,
		func(ctx context.Context, cmd *cobra.Command, wired *daemon.Wired, body json.RawMessage) (json.RawMessage, error) {
			return invokeTeamTool(ctx, cmd, wired, toolName, body)
		})
}

// invokeTeamTool locates toolName in wired.Tools and calls it. When the
// tool is absent it prints the actionable hint and returns silentError(1)
// so the user learns the identity layer is not provisioned rather than
// hitting a cryptic wiring failure.
func invokeTeamTool(ctx context.Context, cmd *cobra.Command, wired *daemon.Wired, toolName string, reqBody json.RawMessage) (json.RawMessage, error) {
	for _, t := range wired.Tools {
		if t.Name() != toolName {
			continue
		}
		raw, err := t.Invoke(ctx, reqBody)
		if err != nil {
			//nolint:errcheck // best-effort stderr write on the tool-error path
			fmt.Fprintf(cmd.ErrOrStderr(), "team: %v\n", err)
			return nil, silentError{code: 1, err: err}
		}
		return raw, nil
	}
	if _, werr := fmt.Fprintln(cmd.ErrOrStderr(), errTeamDisabledMsg); werr != nil {
		return nil, werr
	}
	return nil, silentError{code: 1, err: errors.New(errTeamDisabledMsg)}
}

// emitRawTeamResult writes the verbatim tool result to stdout, used in
// JSON / NDJSON mode where the raw, documented tool shape is the contract.
func emitRawTeamResult(cmd *cobra.Command, raw json.RawMessage) error {
	_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
	return err
}

// isJSONMode reports whether the active output writer is JSON or NDJSON.
func isJSONMode(cmd *cobra.Command) bool {
	f := output.From(cmd).Format()
	return f == output.FormatJSON || f == output.FormatNDJSON
}

// ---------------------------------------------------------------------------
// team create
// ---------------------------------------------------------------------------

func newTeamCreateCmd() *cobra.Command {
	var remote string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Bootstrap an encrypted team scope on this device",
		Long: `create bootstraps the team scope on this device. The caller becomes
the genesis member: a fresh data-encryption key (DEK) is minted at epoch
0 and wrapped to the caller's own wrap key.

The team scope is ALWAYS encrypted (envelope mode) — there is no
--plaintext flag by design. A plaintext shared brain is what the
"project" scope already is; the team scope exists to give a shared brain
end-to-end encryption with per-member key wrapping. Publishing the
wrapped DEKs (in recipients.jsonl) is safe.

Pass --remote to record the shared git URL the team syncs through;
omit it to configure the remote later.

Exit codes:
  0   team created
  1   expected failure: a team already exists, identity not provisioned
  2   unexpected failure: storage or wiring error

Examples:
  maz team create
  maz team create --remote git@github.com:acme/brain-team.git
  maz team create --output json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTeamCreate(cmd, remote)
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "shared git remote URL the team syncs through (optional)")
	return cmd
}

func runTeamCreate(cmd *cobra.Command, remote string) error {
	body, err := json.Marshal(map[string]string{"remote": remote})
	if err != nil {
		return wrapTeamFatal(cmd, fmt.Errorf("marshal request: %w", err))
	}
	raw, err := dispatchTeamTool(cmd, "team_create", body)
	if err != nil {
		return err
	}
	if isJSONMode(cmd) {
		return emitRawTeamResult(cmd, raw)
	}
	var out struct {
		GenesisPrincipalUID string `json:"genesisPrincipalUid"`
		DEKEpoch            int    `json:"dekEpoch"`
		Status              string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return fmt.Errorf("team create: parse response: %w", uerr)
	}
	w := cmd.OutOrStdout()
	if _, werr := fmt.Fprintf(w, "Genesis principal: %s\nDEK epoch:         %d\n", out.GenesisPrincipalUID, out.DEKEpoch); werr != nil {
		return werr
	}
	_, werr := fmt.Fprintln(w,
		"Team scope is now encrypted (envelope mode). Grant members with `maz team grant`.")
	return werr
}

// ---------------------------------------------------------------------------
// team join
// ---------------------------------------------------------------------------

func newTeamJoinCmd() *cobra.Command {
	var remote string
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Point this device at an existing team's remote (no genesis)",
		Long: `join configures this device's team scope to sync with an existing
team's encrypted remote, WITHOUT creating a new team. Use this (not
'maz team create') to join a team someone else created — create would
mint a competing genesis.

After joining:
  1. maz team key            # print this device's join-key blob
  2. (send the blob to an admitted member; they run 'maz team grant')
  3. maz sync pull           # bootstrap the team key + decrypt the brain

Refuses if this device is already the team's genesis owner (it created
the team and does not join it).

Exit codes:
  0   remote configured
  1   expected failure: missing remote, or this device created the team
  2   unexpected failure: storage or wiring error

Examples:
  maz team join --remote git@github.com:acme/brain-team.git
  maz team join --remote git@github.com:acme/brain-team.git --output json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTeamJoin(cmd, remote)
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "the team's shared git remote URL (required)")
	//nolint:errcheck // MarkFlagRequired only fails for an unregistered flag; "remote" is registered above
	_ = cmd.MarkFlagRequired("remote")
	return cmd
}

func runTeamJoin(cmd *cobra.Command, remote string) error {
	body, err := json.Marshal(map[string]string{"remote": remote})
	if err != nil {
		return wrapTeamFatal(cmd, fmt.Errorf("marshal request: %w", err))
	}
	raw, err := dispatchTeamTool(cmd, "team_join", body)
	if err != nil {
		return err
	}
	if isJSONMode(cmd) {
		return emitRawTeamResult(cmd, raw)
	}
	var out struct {
		RemoteURL string   `json:"remoteUrl"`
		NextSteps []string `json:"nextSteps"`
		Status    string   `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return fmt.Errorf("team join: parse response: %w", uerr)
	}
	w := cmd.OutOrStdout()
	if _, werr := fmt.Fprintf(w, "Team remote configured: %s\n\nNext steps:\n", out.RemoteURL); werr != nil {
		return werr
	}
	for i, step := range out.NextSteps {
		if _, werr := fmt.Fprintf(w, "  %d. %s\n", i+1, step); werr != nil {
			return werr
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// team grant
// ---------------------------------------------------------------------------

func newTeamGrantCmd() *cobra.Command {
	var (
		joinKey  string
		fromFile string
		label    string
		role     string
	)
	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Admit a member by wrapping the current DEK to their join key",
		Long: `grant admits a member to the team scope: the current DEK is wrapped to
the member's x25519 wrap public key (carried in their join key) and a
recipients.jsonl row is published so their device can recover the DEK on
its next sync pull. The grant also vouches the member's signing identity
into the team so peers can verify the member's own team-scope events.

The member produces their join key with ` + "`maz team key`" + ` and sends you
the printed blob out of band. Provide it with exactly one of:
  --join-key <blob>   the blob inline
  --from-file <path>  read (and trim) the blob from a file

--role sets the access level (owner|admin|member|reader). Defaults to
"member". Only owners may grant admin or owner roles.

--label is local context only; it is not written to the synced artifact
but is passed through for parity.

Exit codes:
  0   member admitted
  1   expected failure: bad join key, caller not a member, insufficient role
  2   unexpected failure: storage or wiring error

Examples:
  maz team grant --join-key "eyJwcmluY2lwYWxVaWQiOi..."
  maz team grant --from-file alice.joinkey --label "Alice"
  maz team grant --from-file alice.joinkey --role admin
  maz team grant --from-file alice.joinkey --output json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTeamGrant(cmd, joinKey, fromFile, label, role)
		},
	}
	cmd.Flags().StringVar(&joinKey, "join-key", "", "the member's join-key blob (from 'maz team key')")
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read the member's join-key blob from this file")
	cmd.Flags().StringVar(&label, "label", "", "local-only label for the member (not synced)")
	cmd.Flags().StringVar(&role, "role", "", "access role to assign: owner|admin|member|reader (default: member)")
	return cmd
}

func runTeamGrant(cmd *cobra.Command, joinKey, fromFile, label, role string) error {
	blob, err := resolveJoinKey(cmd, joinKey, fromFile)
	if err != nil {
		return err
	}
	in := map[string]string{"joinKey": blob}
	if label != "" {
		in["label"] = label
	}
	if role != "" {
		in["role"] = role
	}
	body, merr := json.Marshal(in)
	if merr != nil {
		return wrapTeamFatal(cmd, fmt.Errorf("marshal request: %w", merr))
	}
	raw, derr := dispatchTeamTool(cmd, "team_grant", body)
	if derr != nil {
		return derr
	}
	if isJSONMode(cmd) {
		return emitRawTeamResult(cmd, raw)
	}
	var out struct {
		GranteePrincipalUID string `json:"granteePrincipalUid"`
		Role                string `json:"role"`
		DEKEpoch            int    `json:"dekEpoch"`
		GrantedEpochs       []int  `json:"grantedEpochs"`
		Status              string `json:"status"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return fmt.Errorf("team grant: parse response: %w", uerr)
	}
	w := cmd.OutOrStdout()
	if _, werr := fmt.Fprintf(w, "Granted: %s as %s (epoch %d)\n", out.GranteePrincipalUID, out.Role, out.DEKEpoch); werr != nil {
		return werr
	}
	_, werr := fmt.Fprintf(w,
		"The grantee can read %s after they run `maz sync pull`.\n",
		describeGrantedEpochs(out.GrantedEpochs, out.DEKEpoch))
	return werr
}

// describeGrantedEpochs renders the slice of DEK epochs wrapped to a grantee for
// the grant success message. A contiguous 0..current set is the full-history
// case (the user-chosen default — a new member reads the entire shared brain,
// not just data sealed after they joined). A gap means the granting device
// lacked a prior epoch's DEK and could not share it, so we list the actual
// epochs rather than overstate the coverage.
func describeGrantedEpochs(epochs []int, current int) string {
	if len(epochs) == 0 {
		return "no team history (no DEK epoch was wrapped)"
	}
	full := len(epochs) == current+1 && epochs[0] == 0 && epochs[len(epochs)-1] == current
	if full {
		if current == 0 {
			return "the full team history (epoch 0)"
		}
		return fmt.Sprintf("the full team history (epochs 0–%d)", current)
	}
	parts := make([]string, len(epochs))
	for i, e := range epochs {
		parts[i] = strconv.Itoa(e)
	}
	return fmt.Sprintf("team data for epochs %s (a prior epoch's key was unavailable on the granting device)",
		strings.Join(parts, ", "))
}

// resolveJoinKey enforces the exactly-one-of (--join-key | --from-file)
// constraint and returns the resolved, trimmed blob. Neither set or both
// set is a bad-input error (silentError code 1). Reading --from-file
// trims surrounding whitespace so a trailing newline does not corrupt the
// blob.
func resolveJoinKey(cmd *cobra.Command, joinKey, fromFile string) (string, error) {
	switch {
	case joinKey == "" && fromFile == "":
		return "", teamBadInput(cmd, "exactly one of --join-key or --from-file is required")
	case joinKey != "" && fromFile != "":
		return "", teamBadInput(cmd, "--join-key and --from-file are mutually exclusive")
	case fromFile != "":
		blob, err := os.ReadFile(fromFile)
		if err != nil {
			return "", teamBadInput(cmd, fmt.Sprintf("read --from-file %q: %v", fromFile, err))
		}
		key := strings.TrimSpace(string(blob))
		if key == "" {
			return "", teamBadInput(cmd, fmt.Sprintf("--from-file %q is empty", fromFile))
		}
		return key, nil
	default:
		return strings.TrimSpace(joinKey), nil
	}
}

// ---------------------------------------------------------------------------
// team revoke
// ---------------------------------------------------------------------------

func newTeamRevokeCmd() *cobra.Command {
	var (
		principal string
		reason    string
	)
	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Remove a member and rotate the DEK forward",
		Long: `revoke removes a member from the team scope and rotates the DEK to a
new epoch (forward re-key). The remaining members are re-wrapped to the
new DEK; the revoked member can no longer read data sealed at the new
epoch.

revoke prints a checklist of the dual-boundary actions: the forward
re-key is automatic, but you MUST still remove the member's git-host
access yourself, and any chunks they pulled before revocation remain
readable to them (forward secrecy only).

Exit codes:
  0   member revoked, DEK rotated
  1   expected failure: no team, caller not a member
  2   unexpected failure: storage or wiring error

Examples:
  maz team revoke --principal 01ARZ...
  maz team revoke --principal 01ARZ... --reason "left company"
  maz team revoke --principal 01ARZ... --output json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTeamRevoke(cmd, principal, reason)
		},
	}
	cmd.Flags().StringVar(&principal, "principal", "", "ULID of the principal to revoke (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "free-form reason recorded with the revocation (optional)")
	//nolint:errcheck // MarkFlagRequired only fails for an unregistered flag; "principal" is registered above
	_ = cmd.MarkFlagRequired("principal")
	return cmd
}

// validateRevokeePrincipal fetches the current team membership and returns an
// error when principalUID is not an admitted member. It is called before the
// revoke dispatch so an unknown principal is rejected with E_VALIDATION exit 1
// rather than silently rotating the DEK to a new epoch — the silent behaviour
// was a B20.0 bug: the tool never validated the principal against the ACL.
func validateRevokeePrincipal(cmd *cobra.Command, principalUID string) error {
	membersRaw, merr := dispatchTeamTool(cmd, "team_members", json.RawMessage(`{}`))
	if merr != nil {
		return merr
	}
	var out struct {
		Members []teamMember `json:"members"`
	}
	if uerr := json.Unmarshal(membersRaw, &out); uerr != nil {
		return fmt.Errorf("team revoke: parse members response: %w", uerr)
	}
	for _, m := range out.Members {
		if m.PrincipalUID == principalUID {
			return nil
		}
	}
	return teamBadInput(cmd, fmt.Sprintf("principal %q is not an admitted team member; use `maz team members` to list admitted principals", principalUID))
}

func runTeamRevoke(cmd *cobra.Command, principal, reason string) error {
	// Validate the principal is an admitted team member before rotating the DEK.
	// Without this check the tool silently accepts any string and rotates, which
	// is a no-op revoke that wastes a DEK epoch (B20.0).
	if verr := validateRevokeePrincipal(cmd, principal); verr != nil {
		return verr
	}

	in := map[string]string{"principalUid": principal}
	if reason != "" {
		in["reason"] = reason
	}
	body, merr := json.Marshal(in)
	if merr != nil {
		return wrapTeamFatal(cmd, fmt.Errorf("marshal request: %w", merr))
	}
	raw, derr := dispatchTeamTool(cmd, "team_revoke", body)
	if derr != nil {
		return derr
	}
	if isJSONMode(cmd) {
		return emitRawTeamResult(cmd, raw)
	}
	var out struct {
		RevokedPrincipalUID string   `json:"revokedPrincipalUid"`
		NewDEKEpoch         int      `json:"newDekEpoch"`
		RewrappedTo         []string `json:"rewrappedTo"`
		Checklist           []string `json:"checklist"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return fmt.Errorf("team revoke: parse response: %w", uerr)
	}
	return renderTeamRevoke(cmd, &out)
}

func renderTeamRevoke(cmd *cobra.Command, out *struct {
	RevokedPrincipalUID string   `json:"revokedPrincipalUid"`
	NewDEKEpoch         int      `json:"newDekEpoch"`
	RewrappedTo         []string `json:"rewrappedTo"`
	Checklist           []string `json:"checklist"`
},
) error {
	w := cmd.OutOrStdout()
	if _, werr := fmt.Fprintf(w, "Revoked:   %s\nNew epoch: %d\n", out.RevokedPrincipalUID, out.NewDEKEpoch); werr != nil {
		return werr
	}
	if len(out.RewrappedTo) > 0 {
		if _, werr := fmt.Fprintf(w, "Re-keyed:  %s\n", strings.Join(out.RewrappedTo, ", ")); werr != nil {
			return werr
		}
	}
	for _, line := range out.Checklist {
		if _, werr := fmt.Fprintf(w, "  • %s\n", line); werr != nil {
			return werr
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// team members
// ---------------------------------------------------------------------------

func newTeamMembersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "members",
		Short: "List admitted team members (no PII)",
		Long: `members lists the admitted principals on the team scope: their opaque
principal UID, the DEK epoch they hold, whether they are the genesis
member, and a truncated view of their wrap public key. No identifying
information (name, email, login) is exposed.

Text mode prints an aligned table; --output json|ndjson emits the raw
tool result (json) or one member object per line (ndjson) for scripts.

Exit codes:
  0   listed
  1   expected failure: no team, identity not provisioned
  2   unexpected failure: storage or wiring error

Examples:
  maz team members
  maz team members --output json
  maz team members --output ndjson`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTeamMembers(cmd)
		},
	}
	return cmd
}

func newTeamKeyCmd() *cobra.Command {
	var requestRegrant bool
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Print this device's team join key",
		Long: `key prints this device's join key: a single opaque blob a prospective
member sends to an admitted member out of band so they can be granted. It
carries the device's principal UID, x25519 wrap public key, and signing
identity. Nothing in it is secret.

The admitted member then runs:
  maz team grant --join-key <printed-blob>

Works before this device has joined any team (it only needs a provisioned
local identity).

Text mode prints the principal UID, wrap key, and the join-key blob;
--output json emits the raw tool result {"principalUid","wrapPublicKey",
"joinKey"} for scripts.

With --request-regrant: scan the local team-scope mirror for DEK epochs not
yet held in the local keychain, and append a teamRegrantRequested event to
the team log. An admin who pulls and sees this event can re-wrap the missing
epochs via maz team grant. Use this when maz doctor reports a
sync.dek_coverage WARN.

Exit codes:
  0   printed
  1   expected failure: identity not provisioned, no missing epochs found (--request-regrant)
  2   unexpected failure: storage or wiring error

Examples:
  maz team key
  maz team key --output json
  maz team key --request-regrant`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if requestRegrant {
				return runTeamKeyRequestRegrant(cmd)
			}
			return runTeamKey(cmd)
		},
	}
	cmd.Flags().BoolVar(&requestRegrant, "request-regrant", false,
		"signal to team admins that this device is missing DEK epochs (appends a teamRegrantRequested event)")
	return cmd
}

func runTeamKey(cmd *cobra.Command) error {
	raw, err := dispatchTeamTool(cmd, "team_key", json.RawMessage(`{}`))
	if err != nil {
		return err
	}
	if output.From(cmd).Format() == output.FormatJSON || output.From(cmd).Format() == output.FormatNDJSON {
		return emitRawTeamResult(cmd, raw)
	}
	var out struct {
		PrincipalUID  string `json:"principalUid"`
		WrapPublicKey string `json:"wrapPublicKey"`
		JoinKey       string `json:"joinKey"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return fmt.Errorf("team key: parse response: %w", uerr)
	}
	_, werr := fmt.Fprintf(cmd.OutOrStdout(),
		"Principal:  %s\nWrap key:   %s\n\nJoin key (send to an admitted member: maz team grant --join-key <blob>):\n%s\n",
		out.PrincipalUID, out.WrapPublicKey, out.JoinKey)
	return werr
}

// runTeamKeyRequestRegrant scans the local team-scope mirror MANIFEST for DEK
// epochs not held in the local keychain, then dispatches team_request_regrant
// to append a teamRegrantRequested event. This signals admitted team admins
// that a re-wrap is needed.
func runTeamKeyRequestRegrant(cmd *cobra.Command) error {
	ctx := cmd.Context()
	dataDir := teamDataDir(cmd)

	chain := secret.NewChain(secret.NewEnvProvider(), secret.NewKeychainProvider())
	_, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return wrapTeamFatal(cmd, fmt.Errorf("load sync config: %w", err))
	}

	missing := collectMissingTeamDEKEpochs(ctx, dataDir, syncCfg, chain)
	if len(missing) == 0 {
		_, werr := fmt.Fprintln(cmd.OutOrStdout(),
			"All DEK epochs present in local keychain — no regrant needed.")
		return werr
	}

	body, merr := json.Marshal(map[string]any{"missingEpochs": missing})
	if merr != nil {
		return wrapTeamFatal(cmd, fmt.Errorf("marshal request: %w", merr))
	}
	raw, derr := dispatchTeamTool(cmd, "team_request_regrant", body)
	if derr != nil {
		return derr
	}
	if isJSONMode(cmd) {
		return emitRawTeamResult(cmd, raw)
	}
	var out struct {
		EventUID      string `json:"eventUid"`
		MissingEpochs []int  `json:"missingEpochs"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return fmt.Errorf("team key --request-regrant: parse response: %w", uerr)
	}
	w := cmd.OutOrStdout()
	_, werr := fmt.Fprintf(w,
		"Regrant requested (event: %s)\nMissing epochs: %v\n\nAn admitted admin must re-wrap epoch(s) %v via maz team grant,\nthen run maz sync pull to recover the missing keys.\n",
		out.EventUID, out.MissingEpochs, out.MissingEpochs)
	return werr
}

// collectMissingTeamDEKEpochs scans each encrypted remote scope's local mirror
// MANIFEST and returns a sorted list of DEK epochs that are not present in the
// local keychain. Used by --request-regrant to build the missingEpochs payload.
func collectMissingTeamDEKEpochs(ctx context.Context, dataDir string, syncCfg config.SyncTable, chain secret.Provider) []int {
	epochsSeen := make(map[int]struct{})
	for name, r := range syncCfg.Remotes {
		if !r.EncryptedOrDefault() {
			continue
		}
		repoDir := filepath.Join(dataDir, "sync", name, "repo")
		for epoch := range readTeamManifestEpochs(repoDir) {
			epochsSeen[epoch] = struct{}{}
		}
	}
	missing := make([]int, 0, len(epochsSeen))
	for epoch := range epochsSeen {
		key := watchword.KeychainTeamDEKPrefix + strconv.Itoa(epoch)
		if _, gErr := chain.Get(ctx, key); gErr != nil {
			missing = append(missing, epoch)
		}
	}
	sort.Ints(missing)
	return missing
}

// readTeamManifestEpochs reads MANIFEST.json at repoDir and returns the
// set of distinct DEK epochs used by encrypted chunks. Returns an empty map
// when the manifest is absent or unreadable.
func readTeamManifestEpochs(repoDir string) map[int]struct{} {
	epochs := make(map[int]struct{})
	raw, err := os.ReadFile(filepath.Join(repoDir, "MANIFEST.json"))
	if err != nil {
		return epochs
	}
	var doc struct {
		Chunks []struct {
			Encrypted bool `json:"encrypted"`
			DEKEpoch  int  `json:"dekEpoch"`
		} `json:"chunks"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return epochs
	}
	for _, c := range doc.Chunks {
		if c.Encrypted {
			epochs[c.DEKEpoch] = struct{}{}
		}
	}
	return epochs
}

// teamMember mirrors one row of the team_members tool result.
type teamMember struct {
	PrincipalUID  string `json:"principalUid"`
	Role          string `json:"role"`
	DEKEpoch      int    `json:"dekEpoch"`
	WrapPublicKey string `json:"wrapPublicKey"`
	IsGenesis     bool   `json:"isGenesis"`
}

func runTeamMembers(cmd *cobra.Command) error {
	raw, err := dispatchTeamTool(cmd, "team_members", json.RawMessage(`{}`))
	if err != nil {
		return err
	}
	switch output.From(cmd).Format() {
	case output.FormatJSON:
		return emitRawTeamResult(cmd, raw)
	case output.FormatNDJSON:
		return streamTeamMembersNDJSON(cmd, raw)
	default:
		return renderTeamMembersTable(cmd, raw)
	}
}

// streamTeamMembersNDJSON emits one member object per line so a pipeline
// can consume the list without buffering the whole array.
func streamTeamMembersNDJSON(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Members []teamMember `json:"members"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("team members: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	for i := range out.Members {
		line, merr := json.Marshal(out.Members[i])
		if merr != nil {
			return fmt.Errorf("team members: marshal row: %w", merr)
		}
		if _, werr := fmt.Fprintln(w, string(line)); werr != nil {
			return werr
		}
	}
	return nil
}

// renderTeamMembersTable prints an aligned PRINCIPAL/EPOCH/GENESIS/WRAP_KEY
// table. The wrap key is truncated to ~12 chars + "…" so a single member
// does not blow out the row width while still being recognisable.
func renderTeamMembersTable(cmd *cobra.Command, raw json.RawMessage) error {
	var out struct {
		Members []teamMember `json:"members"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("team members: parse response: %w", err)
	}
	w := cmd.OutOrStdout()
	if len(out.Members) == 0 {
		_, werr := fmt.Fprintln(w, "(no members)")
		return werr
	}
	if _, werr := fmt.Fprintf(w, "%-26s  %-7s  %-5s  %-7s  %s\n", "PRINCIPAL", "ROLE", "EPOCH", "GENESIS", "WRAP_KEY"); werr != nil {
		return werr
	}
	for i := range out.Members {
		m := out.Members[i]
		genesis := "no"
		if m.IsGenesis {
			genesis = "yes"
		}
		role := m.Role
		if role == "" {
			role = "member"
		}
		if _, werr := fmt.Fprintf(w, "%-26s  %-7s  %-5d  %-7s  %s\n",
			m.PrincipalUID, role, m.DEKEpoch, genesis, truncateWrapKey(m.WrapPublicKey)); werr != nil {
			return werr
		}
	}
	return nil
}

// truncateWrapKey clips a base64 wrap key to 12 runes + "…" for the table.
func truncateWrapKey(key string) string {
	const max = 12
	r := []rune(key)
	if len(r) <= max {
		return key
	}
	return string(r[:max]) + "…"
}

// ---------------------------------------------------------------------------
// team role
// ---------------------------------------------------------------------------

func newTeamRoleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "role",
		Short: "Manage team member roles",
		Long:  `role manages the access roles of admitted team members.`,
	}
	cmd.AddCommand(newTeamRoleSetCmd())
	return cmd
}

func newTeamRoleSetCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "set <principalUid>",
		Short: "Change an admitted member's access role",
		Long: `set changes the access role of an already-admitted team member by
appending a teamRoleChanged event. The change propagates to every peer
on the next sync pull and is applied in ULID order by the membership fold,
so all peers converge to the same effective role independently.

Roles: owner | admin | member | reader
  owner  — full authority; may grant/revoke/promote any role
  admin  — may grant/revoke members; cannot promote to admin/owner
  member — can author team-scope content events (default)
  reader — read-only; content events authored by readers are dropped on replay

Requires the caller to hold at least admin role. Assigning admin or owner
requires the caller to hold owner role.

Exit codes:
  0   role changed
  1   expected failure: insufficient role, principal not a member
  2   unexpected failure: storage or wiring error

Examples:
  maz team role set 01ARZ3NDEKTSV4RRFFQ69G5FAV --role admin
  maz team role set 01ARZ3NDEKTSV4RRFFQ69G5FAV --role reader`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTeamRoleSet(cmd, args[0], role)
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "new role: owner|admin|member|reader (required)")
	//nolint:errcheck // MarkFlagRequired only fails for an unregistered flag; "role" is registered above
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

func runTeamRoleSet(cmd *cobra.Command, principalUID, role string) error {
	body, merr := json.Marshal(map[string]string{"principalUid": principalUID, "role": role})
	if merr != nil {
		return wrapTeamFatal(cmd, fmt.Errorf("marshal request: %w", merr))
	}
	raw, derr := dispatchTeamTool(cmd, "team_role_set", body)
	if derr != nil {
		return derr
	}
	if isJSONMode(cmd) {
		return emitRawTeamResult(cmd, raw)
	}
	var out struct {
		SubjectPrincipalUID string `json:"subjectPrincipalUid"`
		Role                string `json:"role"`
		EventUID            string `json:"eventUid"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return fmt.Errorf("team role set: parse response: %w", uerr)
	}
	_, werr := fmt.Fprintf(cmd.OutOrStdout(),
		"Role updated: %s → %s (event: %s)\n", out.SubjectPrincipalUID, out.Role, out.EventUID)
	return werr
}

// ---------------------------------------------------------------------------
// shared error helpers
// ---------------------------------------------------------------------------

// teamBadInput prints msg to stderr and returns silentError(1) — the
// documented exit code for a CLI input error.
func teamBadInput(cmd *cobra.Command, msg string) error {
	//nolint:errcheck // best-effort stderr write on the bad-input path
	fmt.Fprintf(cmd.ErrOrStderr(), "team: %s\n", msg)
	return silentError{code: 1, err: errors.New(msg)}
}

// wrapTeamFatal prints err to stderr and returns silentError(2) for an
// unexpected (bug-class) failure.
func wrapTeamFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "team", err)
}

// _ keeps zap imported for the dispatchTeamTool closure signature parity
// with the rest of the CLI even when the logger is threaded implicitly.
var _ = zap.NewNop

// ---------------------------------------------------------------------------
// team devices
// ---------------------------------------------------------------------------

// newTeamDevicesCmd returns the `maz team devices` parent command with
// `list` and `revoke` subcommands for managing per-device signing keys.
func newTeamDevicesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "Manage enrolled device signing keys",
		Long: `maz team devices manages the per-device ed25519 signing keys enrolled
under the local principal's identity.

Each device has its own random subkey (B2 Keybase model). A revoked device's
events authored after the revocation UID are dropped by all peers via the
convergent revocation fold.

Subcommands:
  list    — list enrolled devices with revocation status
  revoke  — revoke a device by its ULID`,
	}
	cmd.AddCommand(newTeamDevicesListCmd())
	cmd.AddCommand(newTeamDevicesRevokeCmd())
	return cmd
}

func newTeamDevicesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List enrolled devices for the local principal",
		Long: `list shows all devices enrolled under the local principal with their
signing-key fingerprints, optional labels, create times, and revocation
status. Pass --output json for machine-readable output.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTeamDevicesList(cmd)
		},
	}
}

// teamDeviceRow is the JSON shape of one device in the team_device_list
// response.
type teamDeviceRow struct {
	DeviceID      string  `json:"deviceId"`
	SigKeyID      string  `json:"sigKeyId"`
	DeviceLabel   string  `json:"deviceLabel"`
	VouchSigKeyID string  `json:"vouchSigKeyId"`
	CreateTime    string  `json:"createTime"`
	Revoked       bool    `json:"revoked"`
	RevokedAt     *string `json:"revokedAt"`
	RevokedReason string  `json:"revokedReason"`
}

func runTeamDevicesList(cmd *cobra.Command) error {
	raw, err := dispatchTeamTool(cmd, "team_device_list", json.RawMessage(`{}`))
	if err != nil {
		return err
	}
	if isJSONMode(cmd) {
		return emitRawTeamResult(cmd, raw)
	}

	var out struct {
		Devices []teamDeviceRow `json:"devices"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return fmt.Errorf("team devices list: parse response: %w", uerr)
	}

	w := cmd.OutOrStdout()
	if len(out.Devices) == 0 {
		_, werr := fmt.Fprintln(w, "No devices enrolled.")
		return werr
	}
	for i := range out.Devices {
		if _, werr := fmt.Fprintln(w, formatTeamDeviceRow(&out.Devices[i])); werr != nil {
			return werr
		}
	}
	return nil
}

// formatTeamDeviceRow renders one device line: id, truncated key id, label,
// subkey marker, creation time, and revocation status.
func formatTeamDeviceRow(d *teamDeviceRow) string {
	label := d.DeviceLabel
	if label == "" {
		label = "(no label)"
	}
	status := "active"
	if d.Revoked {
		status = "REVOKED"
		if d.RevokedAt != nil {
			status += " at " + *d.RevokedAt
		}
		if d.RevokedReason != "" {
			status += " — " + d.RevokedReason
		}
	}
	subkey := ""
	if d.VouchSigKeyID != "" {
		subkey = " [subkey]"
	}
	return fmt.Sprintf("%s  %s  %s%s  %s  %s",
		d.DeviceID, d.SigKeyID[:16]+"…", label, subkey, d.CreateTime, status)
}

func newTeamDevicesRevokeCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "revoke <deviceId>",
		Short: "Revoke an enrolled device by its ULID",
		Long: `revoke appends a deviceRevoked event to the team scope. After revocation,
any event authored by this device with a UID greater than the revocation
event's UID is dropped on all peers (convergent fold).

The revoker must be the device's own principal (revoking one's own device)
or a team admin/owner (revoking a team member's device).

Exit codes:
  0   device revoked
  1   expected failure: not found, not a member, insufficient role
  2   unexpected failure: storage or wiring error`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTeamDevicesRevoke(cmd, args[0], reason)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "free-form reason recorded with the revocation (optional)")
	return cmd
}

func runTeamDevicesRevoke(cmd *cobra.Command, deviceID, reason string) error {
	in := map[string]string{"deviceId": deviceID}
	if reason != "" {
		in["reason"] = reason
	}
	body, merr := json.Marshal(in)
	if merr != nil {
		return wrapTeamFatal(cmd, fmt.Errorf("marshal request: %w", merr))
	}
	raw, derr := dispatchTeamTool(cmd, "team_device_revoke", body)
	if derr != nil {
		return derr
	}
	if isJSONMode(cmd) {
		return emitRawTeamResult(cmd, raw)
	}
	var out struct {
		EventUID     string `json:"eventUid"`
		DeviceID     string `json:"deviceId"`
		PrincipalUID string `json:"principalUid"`
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return fmt.Errorf("team devices revoke: parse response: %w", uerr)
	}
	w := cmd.OutOrStdout()
	_, werr := fmt.Fprintf(w, "Revoked device: %s\nPrincipal:      %s\nEvent UID:      %s\n",
		out.DeviceID, out.PrincipalUID, out.EventUID)
	return werr
}

// ---------------------------------------------------------------------------
// maz team pin — genesis trust anchor inspection + recovery
// ---------------------------------------------------------------------------

// newTeamPinCmd returns the `maz team pin` subcommand. Deliberately LOCAL:
// it reads and writes state.toml + the sync mirror directly (no daemon
// dispatch), because the primary use case is diagnosing and recovering a
// poisoned or stale pin — exactly the state in which the team fold (and any
// daemon surface built on it) is fail-closed.
func newTeamPinCmd() *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "pin",
		Short: "Show or clear the genesis trust anchor pin",
		Long: `maz team pin shows the genesis trust anchor stored in state.toml and the
TEAM.json anchor published in the team sync repo (when present).

The pin anchors the membership fold to the known genesis principal + signing
key, closing the genesis-hijack replay surface. It is written automatically:
at team CREATE (pin = self) and on a joining device's first team pull (from
the repo's TEAM.json, after verifying its signature against the event log's
own enrolled genesis key).

--clear removes the pin. The fold then falls back to trust-on-first-use
(lowest-ULID genesis) and the daemon re-imports a VERIFIED TEAM.json anchor
on the next team pull. Use this to recover from a pin that doctor reports as
conflicting (team.genesis_pin).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir := resolveHookDataDir(cmd)
			if clear {
				return runTeamPinClear(cmd, dataDir)
			}
			return runTeamPinShow(cmd, dataDir)
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false,
		"remove the stored pin (fold falls back to TOFU; a verified TEAM.json re-pins on the next pull)")
	return cmd
}

// runTeamPinShow prints the state.toml pin and the sync-mirror TEAM.json side
// by side so drift between the two is visible at a glance. The full crypto
// verification verdict lives in `maz doctor` (team.genesis_pin); this command
// only reports what is stored where.
func runTeamPinShow(cmd *cobra.Command, dataDir string) error {
	uid, kid, pinned, err := config.LoadTeamGenesisPin(dataDir)
	if err != nil {
		return fmt.Errorf("team pin: load state.toml pin: %w", err)
	}
	tj, tjFound, tjErr := gittransport.ReadTeamJSON(filepath.Join(dataDir, "sync", "team", "repo"))
	if isJSONMode(cmd) {
		out := map[string]any{"pinned": pinned}
		if pinned {
			out["principalUid"] = uid
			out["sigKeyId"] = kid
		}
		if tjErr == nil && tjFound {
			out["teamJson"] = map[string]string{
				"principalUid": tj.PrincipalUID,
				"sigKeyId":     tj.SigKeyID,
			}
		}
		raw, merr := json.Marshal(out)
		if merr != nil {
			return fmt.Errorf("team pin: marshal: %w", merr)
		}
		return emitRawTeamResult(cmd, raw)
	}
	w := cmd.OutOrStdout()
	if pinned {
		if _, werr := fmt.Fprintf(w, "Pinned genesis (state.toml):\n  principal: %s\n  sig key:   %s\n", uid, kid); werr != nil {
			return werr
		}
	} else {
		if _, werr := fmt.Fprintln(w, "No genesis pin stored (fold uses TOFU lowest-ULID root).\nThe daemon pins automatically from a verified TEAM.json on the next team pull."); werr != nil {
			return werr
		}
	}
	return renderTeamPinTeamJSON(w, tj, tjFound, tjErr, uid, kid, pinned)
}

// renderTeamPinTeamJSON prints the TEAM.json half of the pin report,
// flagging drift against the stored pin. Extracted from runTeamPinShow to
// keep the cognitive-complexity gate satisfied.
func renderTeamPinTeamJSON(w io.Writer, tj gittransport.TeamJSON, found bool, readErr error, uid, kid string, pinned bool) error {
	switch {
	case readErr != nil:
		_, werr := fmt.Fprintf(w, "TEAM.json (sync repo): unreadable: %v\n", readErr)
		return werr
	case !found:
		_, werr := fmt.Fprintln(w, "TEAM.json (sync repo): not present")
		return werr
	default:
		if _, werr := fmt.Fprintf(w, "TEAM.json (sync repo):\n  principal: %s\n  sig key:   %s\n", tj.PrincipalUID, tj.SigKeyID); werr != nil {
			return werr
		}
		if pinned && (tj.PrincipalUID != uid || tj.SigKeyID != kid) {
			_, werr := fmt.Fprintln(w, "WARNING: TEAM.json differs from the stored pin — run `maz doctor` (team.genesis_pin).")
			return werr
		}
		return nil
	}
}

// runTeamPinClear removes the pin from state.toml. Idempotent: clearing an
// absent pin succeeds silently.
func runTeamPinClear(cmd *cobra.Command, dataDir string) error {
	uid, _, pinned, err := config.LoadTeamGenesisPin(dataDir)
	if err != nil {
		return fmt.Errorf("team pin: load state.toml pin: %w", err)
	}
	if cerr := config.ClearTeamGenesisPin(dataDir); cerr != nil {
		return fmt.Errorf("team pin: clear: %w", cerr)
	}
	if isJSONMode(cmd) {
		raw, merr := json.Marshal(map[string]any{"cleared": pinned})
		if merr != nil {
			return fmt.Errorf("team pin: marshal: %w", merr)
		}
		return emitRawTeamResult(cmd, raw)
	}
	w := cmd.OutOrStdout()
	if !pinned {
		_, werr := fmt.Fprintln(w, "No pin was stored; nothing to clear.")
		return werr
	}
	_, werr := fmt.Fprintf(w, "Cleared genesis pin (was principal %s).\nThe fold falls back to TOFU; a verified TEAM.json re-pins on the next team pull.\n", uid)
	return werr
}
