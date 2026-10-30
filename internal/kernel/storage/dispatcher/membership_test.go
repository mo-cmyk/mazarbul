package dispatcher_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

func grantEvent(t *testing.T, uid, author, grantee string, epoch int) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.TeamMemberGrantedEvent{
		GranteePrincipalUID:  grantee,
		GranteeWrapPublicKey: "pub-" + grantee,
		WrappedDEK:           "wrap-" + grantee + "-" + uid,
		DEKEpoch:             epoch,
		GrantedAt:            time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal grant: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindTeamMemberGranted),
		Source:     "team",
		Confidence: 1,
		Scope:      kernel.ScopeTeam,
		Payload:    payload,
		Author:     kernel.EventAuthor{PrincipalUID: author, DeviceID: author + "-dev", SigKeyID: "kid:" + author},
	}
}

func revokeEvent(t *testing.T, uid, author, subject string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.TeamMemberRevokedEvent{
		RevokedPrincipalUID: subject,
		RevokedAt:           time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal revoke: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindTeamMemberRevoked),
		Source:     "team",
		Confidence: 1,
		Scope:      kernel.ScopeTeam,
		Payload:    payload,
		Author:     kernel.EventAuthor{PrincipalUID: author, DeviceID: author + "-dev", SigKeyID: "kid:" + author},
	}
}

func aclMembers(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher) []string {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	rows, err := reader.QueryContext(ctx, `SELECT principal_uid FROM team_membership_acl`)
	if err != nil {
		t.Fatalf("query acl: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(out)
	return out
}

func TestApply_TeamMembership_ChainAdmits(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	events := []kernel.Event{
		grantEvent(t, "01EV0000000000000000000G1", "G", "G", 0), // genesis self-grant
		grantEvent(t, "01EV0000000000000000000A2", "G", "A", 0),
		grantEvent(t, "01EV0000000000000000000B3", "A", "B", 0),
	}
	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := aclMembers(t, ctx, d), []string{"A", "B", "G"}; !equalStrings(got, want) {
		t.Errorf("ACL = %v, want %v", got, want)
	}

	owner, err := d.GenesisOwner(ctx)
	if err != nil || owner != "G" {
		t.Errorf("GenesisOwner = %q, %v; want G, nil", owner, err)
	}
	member, err := d.IsTeamMember(ctx, "B")
	if err != nil || !member {
		t.Errorf("IsTeamMember(B) = %v, %v; want true, nil", member, err)
	}
	stranger, err := d.IsTeamMember(ctx, "Z")
	if err != nil || stranger {
		t.Errorf("IsTeamMember(Z) = %v, %v; want false, nil", stranger, err)
	}
}

// TestApply_TeamMembership_ReplayOrderIndependent is the dispatcher-level
// proof of the acceptance criterion: the same event set applied in any
// order converges on identical ACL on every device.
func TestApply_TeamMembership_ReplayOrderIndependent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: exhaustive permutation replay (~35s)")
	}
	t.Parallel()
	ctx := testutil.TestContext(t)
	base := []kernel.Event{
		grantEvent(t, "01EV0000000000000000000G1", "G", "G", 0),
		grantEvent(t, "01EV0000000000000000000A2", "G", "A", 0),
		grantEvent(t, "01EV0000000000000000000B3", "A", "B", 0),
		revokeEvent(t, "01EV0000000000000000000R4", "G", "A"),
		grantEvent(t, "01EV0000000000000000000X5", "X", "C", 0), // quarantined: X is not a member
	}
	orders := [][]int{
		{0, 1, 2, 3, 4},
		{4, 3, 2, 1, 0},
		{2, 0, 4, 1, 3},
	}
	var want []string
	for i, order := range orders {
		d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
		for _, idx := range order {
			if err := d.Apply(ctx, []kernel.Event{base[idx]}); err != nil {
				t.Fatalf("order %d apply idx %d: %v", i, idx, err)
			}
		}
		got := aclMembers(t, ctx, d)
		if i == 0 {
			want = got
			continue
		}
		if !equalStrings(got, want) {
			t.Fatalf("order %d ACL = %v, want %v (divergent replay)", i, got, want)
		}
	}
	// B retained (forward-only revoke of A), A revoked, C quarantined ⇒ {B, G}.
	if !equalStrings(want, []string{"B", "G"}) {
		t.Errorf("converged ACL = %v, want [B G]", want)
	}
}

func TestApply_TeamMembership_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	events := []kernel.Event{
		grantEvent(t, "01EV0000000000000000000G1", "G", "G", 0),
		grantEvent(t, "01EV0000000000000000000A2", "G", "A", 0),
	}
	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("Apply 1: %v", err)
	}
	if err := d.Apply(ctx, events); err != nil {
		t.Fatalf("Apply 2 (replay): %v", err)
	}
	if got, want := aclMembers(t, ctx, d), []string{"A", "G"}; !equalStrings(got, want) {
		t.Errorf("ACL after replay = %v, want %v", got, want)
	}
}

// TestAdmittedRecipientRows_AllEpochsForAdmittedMembers proves the per-epoch
// recipients render: every granted epoch of every ADMITTED member is returned
// (so a full-history grant publishes the whole DEK ring), while a quarantined
// member's rows are excluded.
func TestAdmittedRecipientRows_AllEpochsForAdmittedMembers(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// G genesis @0; A granted full history (@0 and @1 — a post-revoke joiner);
	// Z granted by stranger X → quarantined (never admitted).
	if err := d.Apply(ctx, []kernel.Event{
		grantEvent(t, "01EV0000000000000000000G1", "G", "G", 0),
		grantEvent(t, "01EV0000000000000000000A2", "G", "A", 0),
		grantEvent(t, "01EV0000000000000000000A3", "G", "A", 1),
		grantEvent(t, "01EV0000000000000000000Z9", "X", "Z", 0),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	rows, err := d.AdmittedRecipientRows(ctx)
	if err != nil {
		t.Fatalf("AdmittedRecipientRows: %v", err)
	}
	// Expect G@0, A@0, A@1 — Z excluded (quarantined). Key by principal+epoch.
	type pe struct {
		p string
		e int
	}
	got := map[pe]string{}
	for _, r := range rows {
		got[pe{r.PrincipalUID, r.DEKEpoch}] = r.WrappedDEK
		if r.PrincipalUID == "Z" {
			t.Errorf("quarantined member Z leaked into recipients: %+v", r)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (G@0, A@0, A@1); got %v", len(rows), rows)
	}
	for _, want := range []pe{{"G", 0}, {"A", 0}, {"A", 1}} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing recipient row for %+v", want)
		}
	}
	// A's two epochs carry distinct wrapped DEKs (the full ring, not one epoch).
	if got[pe{"A", 0}] == got[pe{"A", 1}] {
		t.Error("A's epoch-0 and epoch-1 wrapped DEKs should differ (per-epoch ring)")
	}
}

// grantEventVouched builds a grant that carries the grantee's verification
// identity AND display name — the shape a real first grant has, which triggers
// the team-scoped person-entity projection. (The bare grantEvent helper omits
// these, so it deliberately does NOT project.)
func grantEventVouched(t *testing.T, uid, author, grantee, displayName string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.TeamMemberGrantedEvent{
		GranteePrincipalUID:  grantee,
		GranteeWrapPublicKey: "pub-" + grantee,
		WrappedDEK:           "wrap-" + grantee + "-" + uid,
		DEKEpoch:             0,
		GranteeSignPublicKey: "sign-" + grantee,
		GranteeSigKeyID:      "kid:" + grantee,
		GranteeDeviceID:      grantee + "-dev",
		GranteeDisplayName:   displayName,
		GrantedAt:            time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal vouched grant: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindTeamMemberGranted),
		Source:     "team",
		Confidence: 1,
		Scope:      kernel.ScopeTeam,
		Payload:    payload,
		Author:     kernel.EventAuthor{PrincipalUID: author, DeviceID: author + "-dev", SigKeyID: "kid:" + author},
	}
}

// TestApply_TeamMemberGranted_ProjectsTeamScopedEntity pins the peer-side fix
// for dangling self-scoped team facts: applying a vouched grant materializes the
// grantee as a team-scoped person entity (so a `get`/`fact get --scope team`
// against that principal resolves on a peer that never saw the member's
// personal-scope principalRegistered).
func TestApply_TeamMemberGranted_ProjectsTeamScopedEntity(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	if err := d.Apply(ctx, []kernel.Event{
		grantEventVouched(t, "01EV0000000000000000000G1", "G", "G", "Genesis Owner"),
		grantEventVouched(t, "01EV0000000000000000000A2", "G", "A", "Member A"),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	for _, want := range []struct{ uid, name string }{{"G", "Genesis Owner"}, {"A", "Member A"}} {
		var kind, status, scope, name string
		if qerr := reader.QueryRowContext(ctx,
			`SELECT kind, resolution_status, scope, display_name FROM entities WHERE uid = ?`, want.uid).
			Scan(&kind, &status, &scope, &name); qerr != nil {
			t.Fatalf("query entity %s: %v", want.uid, qerr)
		}
		if kind != "person" || status != "resolved" || scope != "team" || name != want.name {
			t.Errorf("entity %s = (%q,%q,%q,%q), want (person,resolved,team,%q)", want.uid, kind, status, scope, name, want.name)
		}
	}
}

// TestApply_TeamMemberGranted_DoesNotClobberOwnPrincipalScope pins the
// member's-own-device path: the principal already exists (personal scope, from
// principalRegistered), so the projection's ON CONFLICT(uid) DO NOTHING must
// leave its scope untouched — otherwise the owner's principal would vanish from
// the default {project,personal} read.
func TestApply_TeamMemberGranted_DoesNotClobberOwnPrincipalScope(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	writer, err := d.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := writer.ExecContext(ctx,
		`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, scope)
		 VALUES ('G', 'person', 'Local Self', 1.0, 'resolved', 'personal')`); err != nil {
		t.Fatalf("seed personal entity: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{
		grantEventVouched(t, "01EV0000000000000000000G1", "G", "G", "Genesis Owner"),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var scope, name string
	if qerr := reader.QueryRowContext(ctx,
		`SELECT scope, display_name FROM entities WHERE uid = 'G'`).Scan(&scope, &name); qerr != nil {
		t.Fatalf("query entity G: %v", qerr)
	}
	if scope != "personal" || name != "Local Self" {
		t.Errorf("own principal entity = (%q,%q), want (personal,Local Self) — projection must not clobber it", scope, name)
	}
}

// deviceSignKey returns the base64 sign_public_key enrolled for deviceID, or
// "" if no device row exists.
func deviceSignKey(t *testing.T, ctx context.Context, d *dispatcher.Dispatcher, deviceID string) string {
	t.Helper()
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var key string
	err = reader.QueryRowContext(ctx,
		`SELECT sign_public_key FROM devices WHERE device_id = ?`, deviceID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("query device %s: %v", deviceID, err)
	}
	return key
}

// TestApply_TeamMemberGranted_QuarantinedGrantCannotHijackDevice pins the
// security fix for the device-key hijack: device enrollment runs over the
// FOLD-ADMITTED set only, after the fold — so a quarantined (non-admitted)
// self-grant that names an existing member's device id with an attacker key can
// neither overwrite that member's enrolled signing key nor enroll a new one.
// Before the fix, enrollGrantedDevice ran per raw event with ON CONFLICT DO
// UPDATE, so this exact event silently rewrote the victim's key.
func TestApply_TeamMemberGranted_QuarantinedGrantCannotHijackDevice(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// Establish a team: G genesis, B admitted with a vouched device "B-dev"/"sign-B".
	if err := d.Apply(ctx, []kernel.Event{
		grantEventVouched(t, "01EV0000000000000000000G1", "G", "G", "Genesis Owner"),
		grantEventVouched(t, "01EV0000000000000000000B2", "G", "B", "Member B"),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if got := deviceSignKey(t, ctx, d, "B-dev"); got != "sign-B" {
		t.Fatalf("precondition: B-dev key = %q, want sign-B", got)
	}

	// Attacker Z (not reachable from genesis) crafts a SELF-grant (grantee==author)
	// naming the VICTIM's device id with an attacker-controlled key.
	hijack, err := json.Marshal(ontology.TeamMemberGrantedEvent{
		GranteePrincipalUID:  "Z",
		GranteeWrapPublicKey: "pub-Z",
		WrappedDEK:           "wrap-Z",
		GranteeSignPublicKey: "attacker-key",
		GranteeSigKeyID:      "kid:B",
		GranteeDeviceID:      "B-dev", // the victim's device
		GranteeDisplayName:   "Definitely Not An Attacker",
		GrantedAt:            time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal hijack: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        "01EVZHIJACK00000000000001",
		Kind:       kernel.EventKind(ontology.KindTeamMemberGranted),
		Source:     "team",
		Confidence: 1,
		Scope:      kernel.ScopeTeam,
		Payload:    hijack,
		Author:     kernel.EventAuthor{PrincipalUID: "Z", DeviceID: "Z-dev", SigKeyID: "kid:Z"},
	}}); err != nil {
		t.Fatalf("apply hijack: %v", err)
	}

	// Z is quarantined (never admitted): the self-grant is not reachable from genesis.
	for _, p := range aclMembers(t, ctx, d) {
		if p == "Z" {
			t.Fatal("attacker Z must NOT be admitted by a self-grant")
		}
	}
	// The victim's device key is intact — the quarantined grant could not rewrite it.
	if got := deviceSignKey(t, ctx, d, "B-dev"); got != "sign-B" {
		t.Errorf("B-dev key = %q, want sign-B (quarantined grant must not hijack it)", got)
	}
}

func TestAuthorizeTeamWrite(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// Establish a team: G genesis, A admitted.
	if err := d.Apply(ctx, []kernel.Event{
		grantEvent(t, "01EV0000000000000000000G1", "G", "G", 0),
		grantEvent(t, "01EV0000000000000000000A2", "G", "A", 0),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	dataWrite := &kernel.Event{Kind: "fact", Scope: kernel.ScopeTeam, Payload: []byte(`{}`)}
	selfGrant := func(p string) *kernel.Event {
		ev := grantEvent(t, "01EVSELF000000000000000001", p, p, 0)
		return &ev
	}
	otherGrant := func(author, grantee string) *kernel.Event {
		ev := grantEvent(t, "01EVOTHR000000000000000001", author, grantee, 0)
		return &ev
	}

	t.Run("member data write allowed", func(t *testing.T) {
		t.Parallel()
		if err := d.AuthorizeTeamWrite(ctx, dataWrite, "A"); err != nil {
			t.Errorf("member write: %v, want nil", err)
		}
	})
	t.Run("non-member data write denied", func(t *testing.T) {
		t.Parallel()
		if err := d.AuthorizeTeamWrite(ctx, dataWrite, "Z"); !errors.Is(err, kernel.ErrPermission) {
			t.Errorf("non-member write: %v, want ErrPermission", err)
		}
	})
	t.Run("non-member self-grant into established team denied", func(t *testing.T) {
		t.Parallel()
		// The genesis-self-grant carve-out fires ONLY on a virgin team (or for
		// the genesis owner's own key-rotation). A non-member Z cannot author a
		// self-grant into an established team — that would be a genesis-hijack
		// foothold on the local append path.
		if err := d.AuthorizeTeamWrite(ctx, selfGrant("Z"), "Z"); !errors.Is(err, kernel.ErrPermission) {
			t.Errorf("self-grant into established team: %v, want ErrPermission", err)
		}
	})
	t.Run("genesis owner self-regrant allowed", func(t *testing.T) {
		t.Parallel()
		if err := d.AuthorizeTeamWrite(ctx, selfGrant("G"), "G"); err != nil {
			t.Errorf("genesis self-regrant: %v, want nil", err)
		}
	})
	t.Run("non-member granting another denied", func(t *testing.T) {
		t.Parallel()
		if err := d.AuthorizeTeamWrite(ctx, otherGrant("Z", "Q"), "Z"); !errors.Is(err, kernel.ErrPermission) {
			t.Errorf("non-member grant of other: %v, want ErrPermission", err)
		}
	})
}

// TestAuthorizeTeamWrite_VirginBootstrap verifies the one bootstrap that is
// allowed: the FIRST self-grant on a team with no genesis yet.
func TestAuthorizeTeamWrite_VirginBootstrap(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	bootstrap := grantEvent(t, "01EVSELF000000000000000001", "G", "G", 0)
	if err := d.AuthorizeTeamWrite(ctx, &bootstrap, "G"); err != nil {
		t.Errorf("virgin-team self-grant bootstrap: %v, want nil", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func roleChangeEvent(t *testing.T, uid, author, subject, role string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.TeamRoleChangedEvent{
		SubjectPrincipalUID: subject,
		Role:                role,
		ChangedAt:           time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal role change: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindTeamRoleChanged),
		Source:     "team",
		Confidence: 1,
		Scope:      kernel.ScopeTeam,
		Payload:    payload,
		Author:     kernel.EventAuthor{PrincipalUID: author, DeviceID: author + "-dev", SigKeyID: "kid:" + author},
	}
}

// TestTeamMemberRoleAt_PositionRestricted pins the convergence contract of
// the graft/replay role gate: the verdict for a position is computed ONLY
// from membership events with a strictly lower UID, so a demote that
// post-dates a content event can never flip that event's verdict — even
// when the demote is already applied locally (the arrival order that used
// to diverge from replay).
func TestTeamMemberRoleAt_PositionRestricted(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// G genesis at U1; A admitted member at U3; A demoted to reader at U7.
	// The demotion is APPLIED before any RoleAt query — the "device B saw the
	// demote first" arrival order.
	if err := d.Apply(ctx, []kernel.Event{
		grantEvent(t, "01EV0000000000000000000001", "G", "G", 0),
		grantEvent(t, "01EV0000000000000000000003", "G", "A", 0),
		roleChangeEvent(t, "01EV0000000000000000000007", "G", "A", ontology.RoleReader),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	// The current ACL says reader — that is the live "now" answer.
	nowRole, ok, err := d.TeamMemberRole(ctx, "A")
	if err != nil || !ok || nowRole != ontology.RoleReader {
		t.Fatalf("TeamMemberRole(A) = %q, %v, %v; want reader, true, nil", nowRole, ok, err)
	}

	for _, tc := range []struct {
		name      string
		principal string
		before    string
		wantRole  string
		wantOK    bool
	}{
		{name: "between admission and demotion A was member", principal: "A", before: "01EV0000000000000000000005", wantRole: ontology.RoleMember, wantOK: true},
		{name: "after the demotion A is reader", principal: "A", before: "01EV0000000000000000000009", wantRole: ontology.RoleReader, wantOK: true},
		{name: "strictly before the admitting grant A is unknown", principal: "A", before: "01EV0000000000000000000003", wantOK: false},
		{name: "before genesis nobody is admitted", principal: "G", before: "01EV0000000000000000000001", wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			role, ok, err := d.TeamMemberRoleAt(ctx, tc.principal, tc.before)
			if err != nil {
				t.Fatalf("TeamMemberRoleAt: %v", err)
			}
			if ok != tc.wantOK || (ok && role != tc.wantRole) {
				t.Errorf("TeamMemberRoleAt(before=%s) = %q, %v; want %q, %v",
					tc.before, role, ok, tc.wantRole, tc.wantOK)
			}
		})
	}
}

// grantEventVouchedByAuthor builds a vouched grant event where the event Author
// can differ from the grantee, needed to test the loadVouchedIdentity fallback
// with an admitted (or non-admitted) third-party grantor.
func grantEventVouchedByAuthor(t *testing.T, uid, author, grantee, displayName string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.TeamMemberGrantedEvent{
		GranteePrincipalUID:  grantee,
		GranteeWrapPublicKey: "pub-" + grantee,
		WrappedDEK:           "wrap-" + grantee + "-" + uid,
		DEKEpoch:             1,
		GranteeSignPublicKey: "sign-" + grantee + "-later",
		GranteeSigKeyID:      "kid:" + grantee + "-later",
		GranteeDeviceID:      grantee + "-dev",
		GranteeDisplayName:   displayName,
		GrantedAt:            time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal vouched-by-author grant: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindTeamMemberGranted),
		Source:     "team",
		Confidence: 1,
		Scope:      kernel.ScopeTeam,
		Payload:    payload,
		Author:     kernel.EventAuthor{PrincipalUID: author, DeviceID: author + "-dev", SigKeyID: "kid:" + author},
	}
}

// TestApply_TeamMemberGranted_LaterGrantProvidesDeviceWhenAdmittingGrantHasNone
// pins the F8 fix: a member admitted by a vouchless grant (no grantee_sign_public_key)
// must have their device enrolled when a later grant for the same subject — authored
// by an ADMITTED principal — carries the device identity.
func TestApply_TeamMemberGranted_LaterGrantProvidesDeviceWhenAdmittingGrantHasNone(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// G genesis (vouched); A admitted by a vouchless grant (no device identity);
	// then G issues a second grant for A that carries A's device identity.
	if err := d.Apply(ctx, []kernel.Event{
		grantEventVouched(t, "01EV0000000000000000000G1", "G", "G", "Genesis Owner"),
		grantEvent(t, "01EV0000000000000000000A2", "G", "A", 0),                         // vouchless admitting grant
		grantEventVouchedByAuthor(t, "01EV0000000000000000000A3", "G", "A", "Member A"), // later grant carries device
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// A's device must be enrolled via the fallback (the later grant authored by G).
	if got := deviceSignKey(t, ctx, d, "A-dev"); got != "sign-A-later" {
		t.Errorf("A-dev sign key = %q, want sign-A-later (fallback from later admitted-author grant)", got)
	}
}

// TestApply_TeamMemberGranted_LaterGrantByNonAdmittedCannotEnroll pins the
// security boundary of the F8 fallback: a later grant authored by a NON-admitted
// (quarantined) principal must NOT enroll the member's device, even when the
// admitting grant carried no device identity.
func TestApply_TeamMemberGranted_LaterGrantByNonAdmittedCannotEnroll(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// G genesis; A admitted by vouchless grant; X (never admitted) issues a later
	// grant for A carrying a device identity — must be rejected by the fallback JOIN.
	if err := d.Apply(ctx, []kernel.Event{
		grantEventVouched(t, "01EV0000000000000000000G1", "G", "G", "Genesis Owner"),
		grantEvent(t, "01EV0000000000000000000A2", "G", "A", 0),                         // vouchless admitting grant
		grantEventVouchedByAuthor(t, "01EV0000000000000000000A3", "X", "A", "Member A"), // X is not admitted
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// A must not be enrolled: the only later vouching grant was authored by
	// quarantined X, which is filtered by the admitted-author JOIN.
	if got := deviceSignKey(t, ctx, d, "A-dev"); got != "" {
		t.Errorf("A-dev sign key = %q, want empty (non-admitted author must not enroll)", got)
	}
	// A must still be admitted (the vouchless grant is valid).
	member, err := d.IsTeamMember(ctx, "A")
	if err != nil || !member {
		t.Errorf("IsTeamMember(A) = %v, %v; want true, nil", member, err)
	}
}

// TestAuthorizeTeamWrite_ReaderDenied verifies the LIVE append gate: an
// admitted reader may not author team-scope content.
func TestAuthorizeTeamWrite_ReaderDenied(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	if err := d.Apply(ctx, []kernel.Event{
		grantEvent(t, "01EV0000000000000000000001", "G", "G", 0),
		grantEvent(t, "01EV0000000000000000000003", "G", "R", 0),
		roleChangeEvent(t, "01EV0000000000000000000005", "G", "R", ontology.RoleReader),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	dataWrite := &kernel.Event{Kind: "fact", Scope: kernel.ScopeTeam, Payload: []byte(`{}`)}
	if err := d.AuthorizeTeamWrite(ctx, dataWrite, "R"); !errors.Is(err, kernel.ErrPermission) {
		t.Errorf("reader write: %v, want ErrPermission", err)
	}
}
