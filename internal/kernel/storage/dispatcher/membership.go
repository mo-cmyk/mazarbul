package dispatcher

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// applyTeamMemberGranted records a teamMemberGranted into the raw
// membership OR-set and recomputes the derived ACL. The SIGNED author
// (ev.Author) is the granting authority — the fold, not this handler,
// decides whether the grant is admitted (author reachable from genesis)
// or quarantined. Idempotent under replay via INSERT OR IGNORE keyed by
// the event UID + a full ACL re-fold.
func (d *Dispatcher) applyTeamMemberGranted(ctx context.Context, ev kernel.Event) (retErr error) {
	var g ontology.TeamMemberGrantedEvent
	if err := json.Unmarshal(ev.Payload, &g); err != nil {
		return fmt.Errorf("decode teamMemberGranted payload: %w", err)
	}
	return d.applyMembershipEvent(ctx, membershipRow{
		eventUID:           ev.UID,
		kind:               ontology.KindTeamMemberGranted,
		author:             ev.Author.PrincipalUID,
		authorSigKey:       ev.Author.SigKeyID,
		subject:            g.GranteePrincipalUID,
		wrapPublicKey:      g.GranteeWrapPublicKey,
		wrappedDEK:         g.WrappedDEK,
		dekEpoch:           sql.NullInt64{Int64: int64(g.DEKEpoch), Valid: true},
		occurredAt:         g.GrantedAt,
		createTime:         ev.CreateTime,
		granteeSignKey:     g.GranteeSignPublicKey,
		granteeSigKeyID:    g.GranteeSigKeyID,
		granteeDeviceID:    g.GranteeDeviceID,
		granteeDisplayName: g.GranteeDisplayName,
		role:               g.Role,
	})
}

// applyTeamMemberRevoked records a teamMemberRevoked into the raw OR-set
// and recomputes the ACL. Grant-only columns are NULL. The fold honors
// the revoke only when its author is an admitted owner or admin (forward-only;
// genesis is never revocable).
func (d *Dispatcher) applyTeamMemberRevoked(ctx context.Context, ev kernel.Event) (retErr error) {
	var r ontology.TeamMemberRevokedEvent
	if err := json.Unmarshal(ev.Payload, &r); err != nil {
		return fmt.Errorf("decode teamMemberRevoked payload: %w", err)
	}
	return d.applyMembershipEvent(ctx, membershipRow{
		eventUID:     ev.UID,
		kind:         ontology.KindTeamMemberRevoked,
		author:       ev.Author.PrincipalUID,
		authorSigKey: ev.Author.SigKeyID,
		subject:      r.RevokedPrincipalUID,
		occurredAt:   r.RevokedAt,
		createTime:   ev.CreateTime,
	})
}

// applyTeamRoleChanged records a teamRoleChanged into the raw OR-set
// and recomputes the ACL. The fold enforces authority (author must be
// owner or admin; only owner may assign admin or owner roles).
func (d *Dispatcher) applyTeamRoleChanged(ctx context.Context, ev kernel.Event) (retErr error) {
	var r ontology.TeamRoleChangedEvent
	if err := json.Unmarshal(ev.Payload, &r); err != nil {
		return fmt.Errorf("decode teamRoleChanged payload: %w", err)
	}
	return d.applyMembershipEvent(ctx, membershipRow{
		eventUID:     ev.UID,
		kind:         ontology.KindTeamRoleChanged,
		author:       ev.Author.PrincipalUID,
		authorSigKey: ev.Author.SigKeyID,
		subject:      r.SubjectPrincipalUID,
		role:         r.Role,
		occurredAt:   r.ChangedAt,
		createTime:   ev.CreateTime,
	})
}

// membershipRow is the column set for one raw team_membership_events row.
type membershipRow struct {
	eventUID      string
	kind          string
	author        string
	authorSigKey  string
	subject       string
	wrapPublicKey string
	wrappedDEK    string
	dekEpoch      sql.NullInt64
	occurredAt    time.Time
	createTime    time.Time

	// role is the access role for grant and role-change events; empty for
	// revokes (the fold ignores the column for revoke rows).
	role string

	// Grant-vouched verification identity (grant only; empty for revoke and for
	// re-key grants of an already-enrolled member). When granteeSignKey and
	// granteeDeviceID are both set, the grant publishes the grantee's signing
	// identity; it is PERSISTED into team_membership_events and consumed AFTER
	// the fold (recomputeTeamMembership → projectAdmittedMember) so only an
	// admitted member's device is enrolled — the channel by which a member's
	// signing key reaches the rest of the team so the verifier can resolve their
	// signatures, without a quarantined grant ever touching the devices table.
	granteeSignKey  string
	granteeSigKeyID string
	granteeDeviceID string

	// granteeDisplayName is the grantee's self-reported name (grant only),
	// persisted alongside the identity above and used post-fold to materialize an
	// admitted member as a team-scoped person entity on a peer so self-scoped
	// team facts resolve their subject. May be empty.
	granteeDisplayName string
}

// applyMembershipEvent inserts one raw membership row and recomputes the
// ACL in a single transaction.
func (d *Dispatcher) applyMembershipEvent(ctx context.Context, row membershipRow) (retErr error) {
	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO team_membership_events
			(event_uid, event_kind, author_principal, author_sig_key_id,
			 subject_principal, grantee_wrap_public_key, wrapped_dek, dek_epoch,
			 grantee_sign_public_key, grantee_sig_key_id, grantee_device_id,
			 grantee_display_name, role, occurred_at, create_time, scope)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'team')`,
		row.eventUID,
		row.kind,
		row.author,
		row.authorSigKey,
		row.subject,
		nullableText(row.wrapPublicKey),
		nullableText(row.wrappedDEK),
		row.dekEpoch,
		row.granteeSignKey,
		row.granteeSigKeyID,
		row.granteeDeviceID,
		row.granteeDisplayName,
		row.role,
		row.occurredAt.UTC().Format(time.RFC3339Nano),
		row.createTime.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("insert team membership event: %w", err)
	}

	// Device enrollment + entity projection are deferred into the recompute
	// below so they run over the FOLD-ADMITTED set only — never per raw event.
	// A quarantined grant therefore cannot enroll or overwrite a signing key.
	if err := recomputeTeamMembership(ctx, tx, d.genesisPin); err != nil {
		return fmt.Errorf("recompute team membership: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// vouchedIdentity is a member's grant-vouched verification identity, read back
// from the persisted membership OR-set for post-fold enrollment.
type vouchedIdentity struct {
	signPublicKey string
	sigKeyID      string
	deviceID      string
	displayName   string
	createTime    string // RFC3339Nano, copied onto the devices row
}

// loadVouchedIdentity returns the verification identity a member is enrolled
// with. The primary source is the member's ADMITTING grant — the single event
// the fold credits with admitting them (AdmittedMember.AdmittedByUID).
// Anchoring on the admitting grant is the security-load-bearing choice: the
// fold only ever selects an admitting grant whose AUTHOR is itself admitted
// (see dekRowFor), so a grant authored by a quarantined/non-member principal
// can never define a member's identity.
//
// Fallback: when the admitting grant carries no device identity (a
// legacy/identity-less grant), this function searches for the EARLIEST
// subsequent grant (event_uid > admittingGrantUID) for the same subject whose
// AUTHOR is currently fold-admitted (joined against team_membership_acl, which
// is fully populated before this function is called inside
// recomputeTeamMembership). This closes the F8 scenario where a member is
// first admitted by a vouchless grant and their device identity arrives only
// in a later re-key grant — without relaxing the "author must be admitted"
// invariant that prevents a quarantined grant from defining a member's identity.
//
// Ok=false when neither the admitting grant nor any later admitted-authored
// grant vouches a device — the caller then enrolls nothing.
//
// (A residual remains for a malicious ADMITTED member who back-dates a low-ULID
// grant for a peer to win the AdmittedByUID tiebreak — the same git-remote-write
// / malicious-insider trust boundary as genesis pinning, closed only by the
// out-of-band identity-anchor follow-up. See membership_fold.go.)
func loadVouchedIdentity(ctx context.Context, tx *sql.Tx, principalUID, admittingGrantUID string) (vouchedIdentity, bool, error) {
	if admittingGrantUID == "" {
		return vouchedIdentity{}, false, nil
	}
	// Primary: the admitting grant itself.
	var v vouchedIdentity
	err := tx.QueryRowContext(ctx,
		`SELECT grantee_sign_public_key, grantee_sig_key_id, grantee_device_id,
		        grantee_display_name, create_time
		   FROM team_membership_events
		  WHERE event_uid = ? AND event_kind = ?
		    AND grantee_device_id <> '' AND grantee_sign_public_key <> ''`,
		admittingGrantUID, string(ontology.KindTeamMemberGranted),
	).Scan(&v.signPublicKey, &v.sigKeyID, &v.deviceID, &v.displayName, &v.createTime)
	if err == nil {
		return v, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return vouchedIdentity{}, false, fmt.Errorf("load vouched identity from grant %s: %w", admittingGrantUID, err)
	}

	// Fallback: admitting grant has no device identity. Search for the earliest
	// later grant (event_uid > admittingGrantUID) for this subject whose author
	// is currently fold-admitted. The JOIN to team_membership_acl enforces the
	// same "author must be admitted" invariant as the primary path.
	err = tx.QueryRowContext(ctx,
		`SELECT tme.grantee_sign_public_key, tme.grantee_sig_key_id, tme.grantee_device_id,
		        tme.grantee_display_name, tme.create_time
		   FROM team_membership_events tme
		   JOIN team_membership_acl acl ON acl.principal_uid = tme.author_principal
		  WHERE tme.subject_principal = ?
		    AND tme.event_kind = ?
		    AND tme.event_uid > ?
		    AND tme.grantee_device_id <> ''
		    AND tme.grantee_sign_public_key <> ''
		  ORDER BY tme.event_uid ASC
		  LIMIT 1`,
		principalUID, string(ontology.KindTeamMemberGranted), admittingGrantUID,
	).Scan(&v.signPublicKey, &v.sigKeyID, &v.deviceID, &v.displayName, &v.createTime)
	if errors.Is(err, sql.ErrNoRows) {
		return vouchedIdentity{}, false, nil
	}
	if err != nil {
		return vouchedIdentity{}, false, fmt.Errorf("load vouched identity fallback for %s: %w", principalUID, err)
	}
	return v, true, nil
}

// projectAdmittedMember enrolls a fold-admitted member's grant-vouched device
// into the devices table (so the team verifier can resolve their signatures)
// and materializes their team-scoped person entity (so peers can resolve
// self-scoped team statements whose subject is that principal).
//
// SECURITY: this runs ONLY from recomputeTeamMembership, over the FOLD-ADMITTED
// set — never per raw event. A quarantined (non-admitted) grant therefore
// projects nothing: it cannot reach this code before the fold's quarantine
// verdict. Both writes are ON CONFLICT DO NOTHING, which is not merely an
// idempotency convenience but the fail-safe: an existing device->key binding
// (a member's own self-enrolled row, or an earlier vouch) is NEVER silently
// overwritten, so no actor with remote-push access can hijack a co-member's
// signing identity by crafting a grant that names that member's device id.
//
// The member's own personal-scope principal entity is likewise left untouched
// (DO NOTHING on uid); on a peer the team-scope person row is created fresh.
// Unlike a deviceEnrolled event the grant-vouched device row carries no
// self-signature (signature ""): the SIGNED grant verified at the team gate IS
// the vouch. The identity comes from the member's admitting grant when it
// carries device fields; when it does not (legacy/vouchless admission), the
// earliest later grant for the same subject whose AUTHOR is fold-admitted is
// used instead (see loadVouchedIdentity). In both paths the author-must-be-
// admitted invariant holds, so an unrelated or quarantined grant cannot
// redefine a member's identity.
func projectAdmittedMember(ctx context.Context, tx *sql.Tx, principalUID, admittingGrantUID string) error {
	v, ok, err := loadVouchedIdentity(ctx, tx, principalUID, admittingGrantUID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO devices
		     (device_id, principal_uid, sign_public_key, sig_key_id,
		      device_label, signature, create_time)
		 VALUES (?, ?, ?, ?, NULL, '', ?)
		 ON CONFLICT(device_id) DO NOTHING`,
		v.deviceID, principalUID, v.signPublicKey, v.sigKeyID, v.createTime,
	); err != nil {
		return fmt.Errorf("enroll admitted device %s: %w", v.deviceID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, scope)
		 VALUES (?, 'person', ?, 1.0, 'resolved', 'team')
		 ON CONFLICT(uid) DO NOTHING`,
		principalUID, v.displayName,
	); err != nil {
		return fmt.Errorf("project admitted member entity %s: %w", principalUID, err)
	}
	return nil
}

// recomputeTeamMembership folds the FULL raw membership set into the
// canonical ACL and replaces team_membership_acl wholesale. A full
// DELETE + re-INSERT makes the result a pure function of the event set,
// so a Rebuild converges on identical membership regardless of replay
// order — the convergence guarantee lives in ontology.ReduceMembershipPinned.
// When pin is non-nil, the fold anchors genesis to the pinned principal's
// self-grant instead of the TOFU lowest-ULID root.
func recomputeTeamMembership(ctx context.Context, tx *sql.Tx, pin *ontology.GenesisPin) error {
	events, err := loadMembershipEvents(ctx, tx)
	if err != nil {
		return err
	}
	state := ontology.ReduceMembershipPinned(events, pin)

	if _, err := tx.ExecContext(ctx, `DELETE FROM team_membership_acl`); err != nil {
		return fmt.Errorf("clear team ACL: %w", err)
	}
	for _, m := range state.Admitted {
		genesis := 0
		if m.IsGenesis {
			genesis = 1
		}
		role := m.Role
		if role == "" {
			role = ontology.RoleMember
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO team_membership_acl
				(principal_uid, admitted_by_uid, dek_epoch, wrapped_dek, wrap_public_key, is_genesis, role)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.PrincipalUID, m.AdmittedByUID, m.DEKEpoch, m.WrappedDEK, m.WrapPublicKey, genesis, role,
		); err != nil {
			return fmt.Errorf("insert team ACL row %s: %w", m.PrincipalUID, err)
		}
	}
	// Device enrollment + entity projection run here, over the ADMITTED set
	// only and AFTER the fold — so a quarantined grant never injects or
	// overwrites a signing key. Idempotent (DO NOTHING), so re-folds on every
	// membership event and re-pulls are cheap no-ops.
	for _, m := range state.Admitted {
		if err := projectAdmittedMember(ctx, tx, m.PrincipalUID, m.AdmittedByUID); err != nil {
			return err
		}
	}
	return nil
}

// loadMembershipEvents reads the full raw membership OR-set inside tx.
// Order is irrelevant: ReduceMembershipPinned sorts by event UID internally.
func loadMembershipEvents(ctx context.Context, tx *sql.Tx) ([]ontology.MembershipEvent, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT event_uid, event_kind, author_principal, author_sig_key_id,
			subject_principal, grantee_wrap_public_key, wrapped_dek, dek_epoch, role
		 FROM team_membership_events`)
	if err != nil {
		return nil, fmt.Errorf("query team membership events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ontology.MembershipEvent
	for rows.Next() {
		var (
			e          ontology.MembershipEvent
			authorSKey sql.NullString
			wrapPub    sql.NullString
			dek        sql.NullString
			epoch      sql.NullInt64
			role       sql.NullString
		)
		if err := rows.Scan(&e.EventUID, &e.Kind, &e.AuthorPrincipal, &authorSKey,
			&e.Subject, &wrapPub, &dek, &epoch, &role); err != nil {
			return nil, fmt.Errorf("scan team membership event: %w", err)
		}
		e.AuthorSigKeyID = authorSKey.String
		e.GranteeWrapKey = wrapPub.String
		e.WrappedDEK = dek.String
		e.DEKEpoch = int(epoch.Int64)
		e.Role = role.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate team membership events: %w", err)
	}
	return out, nil
}

// IsTeamMember reports whether principalUID is in the current team ACL.
// Fail-closed: an unknown principal (or empty UID) is not a member. It
// backs the team-scope write authority check (kernel.MembershipAuthority).
func (d *Dispatcher) IsTeamMember(ctx context.Context, principalUID string) (bool, error) {
	if principalUID == "" {
		return false, nil
	}
	var one int
	err := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT 1 FROM team_membership_acl WHERE principal_uid = ?`, principalUID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query team membership: %w", err)
	}
	return true, nil
}

// TeamMemberRole returns the effective role for principalUID from the derived
// ACL, with ok=false when the principal is not an admitted member. Used by
// the service layer (requireLocalRole) and the rebuild verify path.
func (d *Dispatcher) TeamMemberRole(ctx context.Context, principalUID string) (role string, ok bool, err error) {
	if principalUID == "" {
		return "", false, nil
	}
	var r string
	qerr := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT role FROM team_membership_acl WHERE principal_uid = ?`, principalUID).Scan(&r)
	if errors.Is(qerr, sql.ErrNoRows) {
		return "", false, nil
	}
	if qerr != nil {
		return "", false, fmt.Errorf("query team member role: %w", qerr)
	}
	return r, true, nil
}

// TeamMemberRoleAt returns the effective role for principalUID as of the
// position of beforeEventUID in the canonical ULID order: it re-folds only
// the membership events with event_uid strictly below beforeEventUID. The
// verdict is therefore a pure function of the membership-event SET the
// device currently holds — identical on live graft and on replay — which is
// what makes the reader-content gate in verifyTeamEvent convergent: a
// role change or revoke that post-dates a content event can never alter
// that event's accept/drop verdict, no matter the arrival order.
//
// The membership table is small (one row per grant/revoke/role change for a
// small trusted team), so the per-call load + re-fold is sub-millisecond;
// no memoization until rebuild profiling says otherwise.
func (d *Dispatcher) TeamMemberRoleAt(ctx context.Context, principalUID, beforeEventUID string) (role string, ok bool, err error) {
	if principalUID == "" || beforeEventUID == "" {
		return "", false, nil
	}
	events, err := loadMembershipEventsBefore(ctx, d.sqlite.Reader(), beforeEventUID)
	if err != nil {
		return "", false, err
	}
	state := ontology.ReduceMembershipPinned(events, d.genesisPin)
	for _, m := range state.Admitted {
		if m.PrincipalUID == principalUID {
			return m.Role, true, nil
		}
	}
	return "", false, nil
}

// membershipQueryer is the minimal query surface loadMembershipEventsBefore
// needs (satisfied by *sql.DB and *sql.Tx).
type membershipQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadMembershipEventsBefore loads the membership events with event_uid
// strictly below beforeEventUID, for the position-restricted fold in
// TeamMemberRoleAt. Same row mapping as loadMembershipEvents.
func loadMembershipEventsBefore(ctx context.Context, q membershipQueryer, beforeEventUID string) ([]ontology.MembershipEvent, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT event_uid, event_kind, author_principal, author_sig_key_id,
			subject_principal, grantee_wrap_public_key, wrapped_dek, dek_epoch, role
		 FROM team_membership_events
		 WHERE event_uid < ?`, beforeEventUID)
	if err != nil {
		return nil, fmt.Errorf("query team membership events before %s: %w", beforeEventUID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ontology.MembershipEvent
	for rows.Next() {
		var (
			e          ontology.MembershipEvent
			authorSKey sql.NullString
			wrapPub    sql.NullString
			dek        sql.NullString
			epoch      sql.NullInt64
			role       sql.NullString
		)
		if err := rows.Scan(&e.EventUID, &e.Kind, &e.AuthorPrincipal, &authorSKey,
			&e.Subject, &wrapPub, &dek, &epoch, &role); err != nil {
			return nil, fmt.Errorf("scan team membership event: %w", err)
		}
		e.AuthorSigKeyID = authorSKey.String
		e.GranteeWrapKey = wrapPub.String
		e.WrappedDEK = dek.String
		e.DEKEpoch = int(epoch.Int64)
		e.Role = role.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate team membership events: %w", err)
	}
	return out, nil
}

// AuthorizeTeamWrite implements kernel.TeamWriteAuthority: it gates a live
// team-scope write at the append boundary. An admitted member with role ≥
// member is allowed; a reader is denied (ErrPermission) because readers may
// not write any team-scope content. A non-member is denied EXCEPT for a
// genesis self-grant — a teamMemberGranted naming the author themselves —
// but ONLY when no genesis exists yet (a virgin team) or the author already
// IS the genesis owner (a key-rotation self-regrant). This closes the
// local-path half of the genesis-hijack surface: a non-member can no longer
// author a self-grant into an *established* team. (The remote-replay half —
// a crafted low-ULID self-grant pushed to the shared remote — is the
// documented genesis-pinning limitation; see membership_fold.go.) This runs
// only on LIVE Append; Rebuild never consults it.
func (d *Dispatcher) AuthorizeTeamWrite(ctx context.Context, ev *kernel.Event, authorPrincipalUID string) error {
	role, ok, err := d.TeamMemberRole(ctx, authorPrincipalUID)
	if err != nil {
		return fmt.Errorf("team write authority: %w", err)
	}
	if ok {
		// Admitted member: require role ≥ member (readers may not write).
		if ontology.RoleAtLeast(role, ontology.RoleMember) {
			return nil
		}
		return fmt.Errorf("%w: principal %q is an admitted reader and may not write team-scope content",
			kernel.ErrPermission, authorPrincipalUID)
	}
	// Not an admitted member: only the genesis self-grant is allowed (virgin
	// team bootstrap or genesis key-rotation).
	if ev != nil && ev.Kind == kernel.EventKind(ontology.KindTeamMemberGranted) {
		var g ontology.TeamMemberGrantedEvent
		if uerr := json.Unmarshal(ev.Payload, &g); uerr == nil && g.GranteePrincipalUID == authorPrincipalUID {
			genesis, gerr := d.GenesisOwner(ctx)
			if gerr != nil {
				return fmt.Errorf("team write authority: %w", gerr)
			}
			if genesis == "" || genesis == authorPrincipalUID {
				return nil // virgin-team bootstrap or genesis key-rotation
			}
		}
	}
	return fmt.Errorf("%w: principal %q is not an admitted team member", kernel.ErrPermission, authorPrincipalUID)
}

// TeamACLMember is one admitted row of the derived team ACL, enough to
// re-wrap a fresh DEK on revoke and to render recipients.jsonl. No PII.
type TeamACLMember struct {
	PrincipalUID  string
	WrapPublicKey string // base64 x25519 public key
	DEKEpoch      int
	WrappedDEK    string // base64 sealed box
	IsGenesis     bool
	Role          string // effective access role
}

// CurrentDEKEpoch returns the highest DEK epoch present in the team ACL, or 0
// when no team exists. The team service wraps grants at this epoch and bumps
// past it on a revoke re-key.
func (d *Dispatcher) CurrentDEKEpoch(ctx context.Context) (int, error) {
	var epoch sql.NullInt64
	err := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT MAX(dek_epoch) FROM team_membership_acl`).Scan(&epoch)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("query current DEK epoch: %w", err)
	}
	if !epoch.Valid {
		return 0, nil
	}
	return int(epoch.Int64), nil
}

// AdmittedMembers returns every admitted ACL row, ordered by principal UID for
// deterministic rendering. It backs the revoke re-key (re-wrap to remaining
// members) and the recipients.jsonl render.
func (d *Dispatcher) AdmittedMembers(ctx context.Context) ([]TeamACLMember, error) {
	rows, err := d.sqlite.Reader().QueryContext(ctx,
		`SELECT principal_uid, wrap_public_key, dek_epoch, wrapped_dek, is_genesis, role
		 FROM team_membership_acl ORDER BY principal_uid`)
	if err != nil {
		return nil, fmt.Errorf("query admitted members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TeamACLMember
	for rows.Next() {
		var (
			m       TeamACLMember
			wrapPub sql.NullString
			dek     sql.NullString
			epoch   sql.NullInt64
			genesis int
			role    sql.NullString
		)
		if err := rows.Scan(&m.PrincipalUID, &wrapPub, &epoch, &dek, &genesis, &role); err != nil {
			return nil, fmt.Errorf("scan admitted member: %w", err)
		}
		m.WrapPublicKey = wrapPub.String
		m.DEKEpoch = int(epoch.Int64)
		m.WrappedDEK = dek.String
		m.IsGenesis = genesis == 1
		m.Role = role.String
		if m.Role == "" {
			m.Role = ontology.RoleMember
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admitted members: %w", err)
	}
	return out, nil
}

// TeamRecipientRow is one (admitted member, granted epoch) wrapped-DEK row for
// the recipients.jsonl render — the per-epoch DEK ring a joining device unwraps
// to read the shared brain. No PII.
type TeamRecipientRow struct {
	PrincipalUID  string
	WrapPublicKey string // base64 x25519 public key
	DEKEpoch      int
	WrappedDEK    string // base64 sealed box
}

// AdmittedRecipientRows returns one row per (admitted member, granted epoch):
// EVERY teamMemberGranted whose subject is currently admitted, across ALL
// epochs — not the ACL's collapsed single current-epoch row per member. This is
// what lets a full-history grant work end to end: the recipients.jsonl rendered
// from these rows carries every epoch's wrapped DEK, so a joining device's
// bootstrap unwraps the full ring and can decrypt the entire history, not just
// the current epoch. Deduped to the highest-ULID grant per (subject, epoch) so a
// re-grant of the same slot is idempotent. Ordered for deterministic rendering.
//
// Both the subject AND the author of each grant must be currently admitted: the
// author join mirrors the fold's dekRowFor admitted-author filter, so a grant
// authored by a non-member (or a since-revoked member who pre-staged a high-ULID
// grant with a bogus wrapped_dek) can never win the per-(subject, epoch) dedup
// and poison a victim's DEK bootstrap for that epoch.
func (d *Dispatcher) AdmittedRecipientRows(ctx context.Context) ([]TeamRecipientRow, error) {
	rows, err := d.sqlite.Reader().QueryContext(ctx,
		`SELECT e.subject_principal, e.grantee_wrap_public_key, e.dek_epoch, e.wrapped_dek
		   FROM team_membership_events e
		   JOIN team_membership_acl a  ON a.principal_uid  = e.subject_principal
		   JOIN team_membership_acl aa ON aa.principal_uid = e.author_principal
		  WHERE e.event_kind = ?
		    AND e.wrapped_dek IS NOT NULL
		    AND e.grantee_wrap_public_key IS NOT NULL
		  ORDER BY e.subject_principal, e.dek_epoch, e.event_uid`,
		ontology.KindTeamMemberGranted)
	if err != nil {
		return nil, fmt.Errorf("query admitted recipient rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Keyed dedup: ORDER BY ... event_uid ascending means the last row seen for a
	// (subject, epoch) has the highest ULID, so the map keeps the winner. A slice
	// alongside preserves the ordered first-seen sequence for stable output.
	type key struct {
		uid   string
		epoch int
	}
	winner := map[key]TeamRecipientRow{}
	order := make([]key, 0)
	for rows.Next() {
		var (
			r       TeamRecipientRow
			wrapPub sql.NullString
			dek     sql.NullString
			epoch   sql.NullInt64
		)
		if err := rows.Scan(&r.PrincipalUID, &wrapPub, &epoch, &dek); err != nil {
			return nil, fmt.Errorf("scan recipient row: %w", err)
		}
		r.WrapPublicKey = wrapPub.String
		r.DEKEpoch = int(epoch.Int64)
		r.WrappedDEK = dek.String
		k := key{uid: r.PrincipalUID, epoch: r.DEKEpoch}
		if _, seen := winner[k]; !seen {
			order = append(order, k)
		}
		winner[k] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recipient rows: %w", err)
	}
	out := make([]TeamRecipientRow, 0, len(order))
	for _, k := range order {
		out = append(out, winner[k])
	}
	return out, nil
}

// GenesisOwner returns the principal UID of the team's genesis owner (the
// TOFU root), or "" when no team has been created. Used by the authorized
// retraction authority check (only the asserter or genesis owner may
// close a team-scope relation).
func (d *Dispatcher) GenesisOwner(ctx context.Context) (string, error) {
	var uid string
	err := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT principal_uid FROM team_membership_acl WHERE is_genesis = 1 LIMIT 1`).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query genesis owner: %w", err)
	}
	return uid, nil
}

// PrincipalGitIdentity returns the GitHub login, immutable numeric id, and
// display name recorded for a principal UID, with ok=false when the principal
// is unknown locally. It feeds the sync transport's bring-your-own-git
// authorship: the login+id synthesize the no-reply commit email, the display
// name becomes the author name, and the same lookup names peers in the
// Co-authored-by trailer. A miss is non-fatal — the caller falls back to the
// next identity source (git config, then the Mazarbul sentinel).
func (d *Dispatcher) PrincipalGitIdentity(ctx context.Context, principalUID string) (login string, userID int64, displayName string, ok bool, err error) {
	var (
		l  sql.NullString
		id sql.NullInt64
		dn sql.NullString
	)
	qerr := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT github_login, github_user_id, display_name FROM principals WHERE principal_uid = ?`,
		principalUID).Scan(&l, &id, &dn)
	if errors.Is(qerr, sql.ErrNoRows) {
		return "", 0, "", false, nil
	}
	if qerr != nil {
		return "", 0, "", false, fmt.Errorf("query principal git identity: %w", qerr)
	}
	// github_login holds the anchor "github:<login>" (see SameAs); callers want
	// the bare login so the synthesized no-reply email is "<id>+<login>@..." and
	// actually attributes on GitHub. Strip the scheme; a value without it (legacy
	// or a future provider) passes through unchanged.
	return strings.TrimPrefix(l.String, ontology.GitHubAnchorPrefix), id.Int64, dn.String, true, nil
}

// PrincipalDisplayName returns the display name recorded for a principal UID in
// the principals projection, or "" when the principal is unknown locally. The
// team service stamps it into a grant so peers can name the projected member
// entity; a miss is non-fatal (the entity still resolves by UID).
func (d *Dispatcher) PrincipalDisplayName(ctx context.Context, principalUID string) (string, error) {
	var name string
	err := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT display_name FROM principals WHERE principal_uid = ?`, principalUID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query principal display name: %w", err)
	}
	return name, nil
}
