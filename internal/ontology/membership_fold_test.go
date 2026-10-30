package ontology_test

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// grant/revoke build MembershipEvents with monotonic-ish ULIDs derived
// from a sequence number so tests control the canonical total order
// without minting real ULIDs.
func grant(uid, author, subject string, epoch int) ontology.MembershipEvent {
	return ontology.MembershipEvent{
		EventUID:        uid,
		Kind:            ontology.KindTeamMemberGranted,
		AuthorPrincipal: author,
		Subject:         subject,
		OccurredAt:      time.Unix(0, 0).UTC(),
		WrappedDEK:      "wrap-" + subject + "-" + uid,
		GranteeWrapKey:  "pub-" + subject,
		DEKEpoch:        epoch,
	}
}

func revoke(uid, author, subject string) ontology.MembershipEvent {
	return ontology.MembershipEvent{
		EventUID:        uid,
		Kind:            ontology.KindTeamMemberRevoked,
		AuthorPrincipal: author,
		Subject:         subject,
		OccurredAt:      time.Unix(0, 0).UTC(),
	}
}

func admittedUIDs(s ontology.MembershipState) []string {
	out := make([]string, 0, len(s.Admitted))
	for _, m := range s.Admitted {
		out = append(out, m.PrincipalUID)
	}
	return out
}

func TestReduceMembership_GenesisAndChain(t *testing.T) {
	t.Parallel()
	// G self-grants (genesis), grants A; A grants B.
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		grant("03", "A", "B", 0),
	}
	got := ontology.ReduceMembership(events)
	if got.GenesisUID != "G" {
		t.Errorf("genesis = %q, want G", got.GenesisUID)
	}
	if want := []string{"A", "B", "G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v", admittedUIDs(got), want)
	}
	if len(got.Quarantined) != 0 {
		t.Errorf("quarantined = %v, want none", got.Quarantined)
	}
}

func TestReduceMembership_NoGenesisQuarantinesAll(t *testing.T) {
	t.Parallel()
	// No self-grant ⇒ no TOFU root ⇒ empty ACL, everything quarantined.
	events := []ontology.MembershipEvent{
		grant("01", "G", "A", 0),
		grant("02", "A", "B", 0),
	}
	got := ontology.ReduceMembership(events)
	if got.GenesisUID != "" || len(got.Admitted) != 0 {
		t.Errorf("expected no genesis + empty ACL, got genesis=%q admitted=%v", got.GenesisUID, admittedUIDs(got))
	}
	if len(got.Quarantined) != 2 {
		t.Errorf("quarantined = %v, want both events", got.Quarantined)
	}
}

func TestReduceMembership_GrantByNonMemberQuarantined(t *testing.T) {
	t.Parallel()
	// X is not reachable from genesis; X's grant of B is quarantined.
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("05", "X", "B", 0), // X never admitted
	}
	got := ontology.ReduceMembership(events)
	if want := []string{"G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v", admittedUIDs(got), want)
	}
	if want := []string{"05"}; !reflect.DeepEqual(got.Quarantined, want) {
		t.Errorf("quarantined = %v, want %v", got.Quarantined, want)
	}
}

func TestReduceMembership_RevokeByMemberDropsSubject(t *testing.T) {
	t.Parallel()
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		revoke("03", "G", "A"),
	}
	got := ontology.ReduceMembership(events)
	if want := []string{"G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v (A revoked)", admittedUIDs(got), want)
	}
}

func TestReduceMembership_ForwardOnlyRevoke(t *testing.T) {
	t.Parallel()
	// G→A, A→B, then revoke A. B was admitted by A BEFORE A's revoke and
	// stays admitted (forward-only: revoking A does not un-admit B).
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		grant("03", "A", "B", 0),
		revoke("04", "G", "A"),
	}
	got := ontology.ReduceMembership(events)
	if want := []string{"B", "G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v (A revoked, B retained)", admittedUIDs(got), want)
	}
}

func TestReduceMembership_RevokeByNonMemberNoOp(t *testing.T) {
	t.Parallel()
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		revoke("03", "X", "A"), // X is not a member ⇒ revoke ignored + quarantined
	}
	got := ontology.ReduceMembership(events)
	if want := []string{"A", "G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v (X's revoke is a no-op)", admittedUIDs(got), want)
	}
	if want := []string{"03"}; !reflect.DeepEqual(got.Quarantined, want) {
		t.Errorf("quarantined = %v, want %v", got.Quarantined, want)
	}
}

func TestReduceMembership_RegrantAfterRevokeWins(t *testing.T) {
	t.Parallel()
	// Revoke A (03) then re-grant A (04, higher ULID) ⇒ A re-admitted.
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		revoke("03", "G", "A"),
		grant("04", "G", "A", 1),
	}
	got := ontology.ReduceMembership(events)
	if want := []string{"A", "G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v (re-grant wins)", admittedUIDs(got), want)
	}
}

func TestReduceMembership_GenesisNeverRevocable(t *testing.T) {
	t.Parallel()
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		revoke("03", "A", "G"), // A (a member) tries to revoke genesis
	}
	got := ontology.ReduceMembership(events)
	if want := []string{"A", "G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v (genesis is not revocable)", admittedUIDs(got), want)
	}
}

func TestReduceMembership_DEKRowPicksHighestEpoch(t *testing.T) {
	t.Parallel()
	// A granted at epoch 0 then re-keyed to epoch 1; the ACL row carries
	// the highest epoch's wrapped DEK.
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		grant("05", "G", "A", 1),
	}
	got := ontology.ReduceMembership(events)
	if got.CurrentEpoch != 1 {
		t.Errorf("CurrentEpoch = %d, want 1", got.CurrentEpoch)
	}
	for _, m := range got.Admitted {
		if m.PrincipalUID == "A" {
			if m.DEKEpoch != 1 || m.WrappedDEK != "wrap-A-05" {
				t.Errorf("A row = epoch %d dek %q, want epoch 1 dek wrap-A-05", m.DEKEpoch, m.WrappedDEK)
			}
			if m.AdmittedByUID != "02" {
				t.Errorf("A admittedBy = %q, want 02 (lowest grant)", m.AdmittedByUID)
			}
		}
	}
}

// TestReduceMembership_OrderIndependent is the proof-of-record: the fold
// must produce byte-identical state across every permutation of the same
// event set — "rejected/accepted IDENTICALLY on every device on replay".
func TestReduceMembership_OrderIndependent(t *testing.T) {
	t.Parallel()
	base := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		grant("03", "A", "B", 0),
		grant("04", "B", "C", 0),
		revoke("05", "G", "A"),
		grant("06", "X", "D", 0), // quarantined (X not a member)
		grant("07", "B", "C", 1), // re-key C
		revoke("08", "Z", "B"),   // quarantined (Z not a member)
	}
	want := ontology.ReduceMembership(base)

	rng := rand.New(rand.NewPCG(42, 99))
	for trial := range 50 {
		shuffled := make([]ontology.MembershipEvent, len(base))
		copy(shuffled, base)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := ontology.ReduceMembership(shuffled)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d diverged:\n got = %s\nwant = %s",
				trial, fmt.Sprintf("%+v", got), fmt.Sprintf("%+v", want))
		}
	}
}

// ---------------------------------------------------------------------------
// Role RBAC tests
// ---------------------------------------------------------------------------

// grantWithRole builds a MembershipEvent with an explicit role.
func grantWithRole(uid, author, subject string, epoch int, role string) ontology.MembershipEvent {
	e := grant(uid, author, subject, epoch)
	e.Role = role
	return e
}

// roleChanged builds a teamRoleChanged MembershipEvent.
func roleChanged(uid, author, subject, role string) ontology.MembershipEvent {
	return ontology.MembershipEvent{
		EventUID:        uid,
		Kind:            ontology.KindTeamRoleChanged,
		AuthorPrincipal: author,
		Subject:         subject,
		OccurredAt:      time.Unix(0, 0).UTC(),
		Role:            role,
	}
}

// memberOf returns the AdmittedMember for uid, or (zero, false) when absent.
func memberOf(s ontology.MembershipState, uid string) (ontology.AdmittedMember, bool) {
	for _, m := range s.Admitted {
		if m.PrincipalUID == uid {
			return m, true
		}
	}
	return ontology.AdmittedMember{}, false
}

// roleFor extracts the effective role of a principal from the fold result.
func roleFor(s ontology.MembershipState, uid string) string {
	for _, m := range s.Admitted {
		if m.PrincipalUID == uid {
			return m.Role
		}
	}
	return ""
}

func TestRole_GrantDefaultIsMember(t *testing.T) {
	t.Parallel()
	// A grant with no Role field defaults to "member".
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0), // no Role field
	}
	got := ontology.ReduceMembership(events)
	if r := roleFor(got, "A"); r != ontology.RoleMember {
		t.Errorf("A role = %q, want member", r)
	}
	if r := roleFor(got, "G"); r != ontology.RoleOwner {
		t.Errorf("G role = %q, want owner", r)
	}
}

func TestRole_GrantExplicitReader(t *testing.T) {
	t.Parallel()
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grantWithRole("02", "G", "A", 0, ontology.RoleReader),
	}
	got := ontology.ReduceMembership(events)
	if r := roleFor(got, "A"); r != ontology.RoleReader {
		t.Errorf("A role = %q, want reader", r)
	}
}

func TestRole_GenesisAlwaysOwnerRegardlessOfGrant(t *testing.T) {
	t.Parallel()
	// A re-key grant for genesis with role "member" must NOT demote genesis.
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grantWithRole("02", "G", "G", 1, ontology.RoleMember), // re-key self with wrong role
	}
	got := ontology.ReduceMembership(events)
	if r := roleFor(got, "G"); r != ontology.RoleOwner {
		t.Errorf("genesis role = %q, want owner (immutable)", r)
	}
}

func TestRole_RoleChangedSupersedes(t *testing.T) {
	t.Parallel()
	// Grant A as member (02), then owner G promotes A to admin (03).
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grantWithRole("02", "G", "A", 0, ontology.RoleMember),
		roleChanged("03", "G", "A", ontology.RoleAdmin),
	}
	got := ontology.ReduceMembership(events)
	if r := roleFor(got, "A"); r != ontology.RoleAdmin {
		t.Errorf("A role after promote = %q, want admin", r)
	}
}

func TestRole_MemberCannotRevoke(t *testing.T) {
	t.Parallel()
	// G admits A (admin) and B (member). B tries to revoke A — ignored.
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grantWithRole("02", "G", "A", 0, ontology.RoleAdmin),
		grantWithRole("03", "G", "B", 0, ontology.RoleMember),
		revoke("04", "B", "A"), // B has member role — must be ignored
	}
	got := ontology.ReduceMembership(events)
	if want := []string{"A", "B", "G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v (B's revoke of A must be a no-op)", admittedUIDs(got), want)
	}
}

func TestRole_AdminCannotPromoteToOwner(t *testing.T) {
	t.Parallel()
	// G admits A (admin) and B (member). A tries to set B to owner — ignored.
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grantWithRole("02", "G", "A", 0, ontology.RoleAdmin),
		grantWithRole("03", "G", "B", 0, ontology.RoleMember),
		roleChanged("04", "A", "B", ontology.RoleOwner), // admin cannot assign owner
	}
	got := ontology.ReduceMembership(events)
	if r := roleFor(got, "B"); r != ontology.RoleMember {
		t.Errorf("B role = %q, want member (admin cannot promote to owner)", r)
	}
}

func TestRole_ReaderCannotChangeRoles(t *testing.T) {
	t.Parallel()
	// G admits A (reader) and B (member). A (reader) tries to change B to admin.
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grantWithRole("02", "G", "A", 0, ontology.RoleReader),
		grantWithRole("03", "G", "B", 0, ontology.RoleMember),
		roleChanged("04", "A", "B", ontology.RoleAdmin), // reader has no authority
	}
	got := ontology.ReduceMembership(events)
	if r := roleFor(got, "B"); r != ontology.RoleMember {
		t.Errorf("B role = %q, want member (reader cannot change roles)", r)
	}
}

func TestRole_LastOwnerProtectionRevoke(t *testing.T) {
	t.Parallel()
	// G is the only owner. G tries to revoke itself — ignored (last-owner protection).
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		revoke("03", "G", "G"), // last-owner self-revoke — ignored
	}
	got := ontology.ReduceMembership(events)
	if want := []string{"A", "G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v (last-owner revoke must be ignored)", admittedUIDs(got), want)
	}
	if r := roleFor(got, "G"); r != ontology.RoleOwner {
		t.Errorf("G role = %q, want owner", r)
	}
}

func TestRole_LastOwnerProtectionDemote(t *testing.T) {
	t.Parallel()
	// G is the only owner. G tries to demote itself to member — ignored.
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		roleChanged("03", "G", "G", ontology.RoleMember), // targets genesis — always ignored
	}
	got := ontology.ReduceMembership(events)
	if r := roleFor(got, "G"); r != ontology.RoleOwner {
		t.Errorf("G role = %q, want owner (genesis cannot be demoted)", r)
	}
}

func TestRole_TwoOwners_LastOwnerProtectionAllowsDemote(t *testing.T) {
	t.Parallel()
	// G and A are both owners. G demotes A to member — allowed (G still owner).
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grantWithRole("02", "G", "A", 0, ontology.RoleOwner),
		roleChanged("03", "G", "A", ontology.RoleMember),
	}
	got := ontology.ReduceMembership(events)
	if r := roleFor(got, "A"); r != ontology.RoleMember {
		t.Errorf("A role = %q, want member (demote allowed when G still owner)", r)
	}
}

func TestRole_RoleChangeTargetingGenesisIgnored(t *testing.T) {
	t.Parallel()
	// An owner (A) tries to change genesis G's role — always ignored.
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grantWithRole("02", "G", "A", 0, ontology.RoleOwner),
		roleChanged("03", "A", "G", ontology.RoleMember), // targeting genesis
	}
	got := ontology.ReduceMembership(events)
	if r := roleFor(got, "G"); r != ontology.RoleOwner {
		t.Errorf("G role = %q, want owner (genesis immutable)", r)
	}
}

// TestRole_OrderIndependent_WithRoles extends the shuffle proof to role events:
// 50 random permutations of a set that includes grants-with-roles,
// teamRoleChanged events, and revokes must all produce identical output.
func TestRole_OrderIndependent_WithRoles(t *testing.T) {
	t.Parallel()
	base := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),                              // genesis G = owner
		grantWithRole("02", "G", "A", 0, ontology.RoleAdmin),  // A = admin
		grantWithRole("03", "G", "B", 0, ontology.RoleMember), // B = member
		grantWithRole("04", "G", "C", 0, ontology.RoleReader), // C = reader
		roleChanged("05", "G", "B", ontology.RoleAdmin),       // G promotes B to admin
		roleChanged("06", "A", "C", ontology.RoleMember),      // A promotes C to member
		revoke("07", "G", "A"),                                // G revokes A
		grant("08", "X", "D", 0),                              // quarantined (X unknown)
		roleChanged("09", "C", "B", ontology.RoleOwner),       // C (member after 06) tries owner — ignored
	}
	want := ontology.ReduceMembership(base)

	rng := rand.New(rand.NewPCG(77, 13))
	for trial := range 50 {
		shuffled := make([]ontology.MembershipEvent, len(base))
		copy(shuffled, base)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := ontology.ReduceMembership(shuffled)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d diverged:\n got = %+v\nwant = %+v", trial, got, want)
		}
	}

	// Spot-check the expected output of the canonical order.
	if roleFor(want, "B") != ontology.RoleAdmin {
		t.Errorf("B expected admin (promoted by G at 05), got %q", roleFor(want, "B"))
	}
	if roleFor(want, "C") != ontology.RoleMember {
		t.Errorf("C expected member (promoted by A at 06), got %q", roleFor(want, "C"))
	}
	if _, ok := memberOf(want, "A"); ok {
		t.Error("A expected revoked at 07, still in ACL")
	}
}

// ---------------------------------------------------------------------------
// Genesis-pinning tests (ReduceMembershipPinned)
// ---------------------------------------------------------------------------

// grantWithSig builds a MembershipEvent with an explicit AuthorSigKeyID,
// used to exercise key-id matching in pinnedGenesisUID.
func grantWithSig(uid, author, subject, sigKeyID string, epoch int) ontology.MembershipEvent {
	e := grant(uid, author, subject, epoch)
	e.AuthorSigKeyID = sigKeyID
	return e
}

// TestPinned_GenesisWinsOverLowerULID is the core security property:
// a malicious self-grant with a LOWER ULID than the real genesis must NOT
// become genesis when a pin is present. The malicious principal's events are
// quarantined; only the pinned genesis chain is admitted.
func TestPinned_GenesisWinsOverLowerULID(t *testing.T) {
	t.Parallel()
	// "00" < "01" — the malicious event sorts first by ULID.
	events := []ontology.MembershipEvent{
		grantWithSig("00", "EVIL", "EVIL", "kid:evil", 0), // malicious low-ULID self-grant
		grantWithSig("01", "G", "G", "kid:genesis", 0),    // real genesis (higher ULID)
		grant("02", "G", "A", 0),                          // G → A (admitted)
		grant("03", "EVIL", "B", 0),                       // EVIL → B (quarantined)
	}
	pin := &ontology.GenesisPin{PrincipalUID: "G", SigKeyID: "kid:genesis"}
	got := ontology.ReduceMembershipPinned(events, pin)

	if got.GenesisUID != "G" {
		t.Errorf("genesis = %q, want G (pin overrides lower ULID)", got.GenesisUID)
	}
	if want := []string{"A", "G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v (EVIL chain quarantined)", admittedUIDs(got), want)
	}
	// "00" (EVIL self-grant) and "03" (EVIL→B) must be quarantined.
	if len(got.Quarantined) != 2 {
		t.Errorf("quarantined = %v (len %d), want 2 events", got.Quarantined, len(got.Quarantined))
	}
}

// TestPinned_UnpinnedBehaviorUnchanged verifies that a nil pin produces
// output identical to the unpinned ReduceMembership — no regression.
func TestPinned_UnpinnedBehaviorUnchanged(t *testing.T) {
	t.Parallel()
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
		grant("03", "A", "B", 0),
		revoke("04", "G", "A"),
	}
	want := ontology.ReduceMembership(events)
	got := ontology.ReduceMembershipPinned(events, nil)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nil-pin diverged from ReduceMembership:\ngot  = %+v\nwant = %+v", got, want)
	}
}

// TestPinned_PermutationDeterminism proves that 50 random permutations of the
// same event set (including a malicious low-ULID self-grant) all produce the
// same result with a pin.
func TestPinned_PermutationDeterminism(t *testing.T) {
	t.Parallel()
	base := []ontology.MembershipEvent{
		grantWithSig("00", "EVIL", "EVIL", "kid:evil", 0),
		grantWithSig("01", "G", "G", "kid:genesis", 0),
		grant("02", "G", "A", 0),
		grant("03", "A", "B", 0),
		grant("04", "EVIL", "C", 0),
		revoke("05", "G", "A"),
	}
	pin := &ontology.GenesisPin{PrincipalUID: "G", SigKeyID: "kid:genesis"}
	want := ontology.ReduceMembershipPinned(base, pin)

	rng := rand.New(rand.NewPCG(17, 42))
	for trial := range 50 {
		shuffled := make([]ontology.MembershipEvent, len(base))
		copy(shuffled, base)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := ontology.ReduceMembershipPinned(shuffled, pin)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d diverged:\n got = %+v\nwant = %+v", trial, got, want)
		}
	}
}

// TestPinned_NoMatchingGenesisFailsClosed verifies that a pin naming a
// principal with no matching self-grant results in empty genesis (fail-closed)
// and all events quarantined — not a panic or a fallback to a different genesis.
func TestPinned_NoMatchingGenesisFailsClosed(t *testing.T) {
	t.Parallel()
	events := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grant("02", "G", "A", 0),
	}
	pin := &ontology.GenesisPin{PrincipalUID: "MISSING", SigKeyID: "kid:missing"}
	got := ontology.ReduceMembershipPinned(events, pin)

	if got.GenesisUID != "" {
		t.Errorf("genesis = %q, want empty (no matching self-grant)", got.GenesisUID)
	}
	if len(got.Admitted) != 0 {
		t.Errorf("admitted = %v, want empty (fail-closed)", admittedUIDs(got))
	}
	if len(got.Quarantined) != 2 {
		t.Errorf("quarantined len=%d, want 2", len(got.Quarantined))
	}
}

// TestPinned_SigKeyIDMatchRequired verifies that when pin.SigKeyID is set, a
// self-grant with a different sig key id for the same principal is NOT accepted.
// Only the grant with the correct SigKeyID qualifies as genesis.
func TestPinned_SigKeyIDMatchRequired(t *testing.T) {
	t.Parallel()
	events := []ontology.MembershipEvent{
		grantWithSig("01", "G", "G", "kid:wrong", 0),   // G self-grant, wrong key
		grantWithSig("02", "G", "G", "kid:correct", 0), // G self-grant, correct key
		grant("03", "G", "A", 0),
	}
	pin := &ontology.GenesisPin{PrincipalUID: "G", SigKeyID: "kid:correct"}
	got := ontology.ReduceMembershipPinned(events, pin)

	if got.GenesisUID != "G" {
		t.Errorf("genesis = %q, want G", got.GenesisUID)
	}
	if want := []string{"A", "G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v", admittedUIDs(got), want)
	}
}

// TestRole_ReAdmissionAfterRevokeResetsRole verifies that re-granting a previously
// revoked principal restores their effective role to the new grant's role, overriding
// any stale teamRoleChanged event that was recorded before the revoke.
//
// Sequence: genesis G → grant A(member) → roleChanged A→reader → revoke A → grant A(member).
// Expected: A is admitted with role "member", not the stale "reader".
//
// The 50-permutation shuffle proves the fold converges identically regardless of
// event arrival order (pure function of the event set).
func TestRole_ReAdmissionAfterRevokeResetsRole(t *testing.T) {
	t.Parallel()
	base := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),
		grantWithRole("02", "G", "A", 0, ontology.RoleMember),
		roleChanged("03", "G", "A", ontology.RoleReader),
		revoke("04", "G", "A"),
		grantWithRole("05", "G", "A", 1, ontology.RoleMember),
	}
	want := ontology.ReduceMembership(base)

	// Direct role check: must be "member" after re-admission, not the stale "reader".
	if r := roleFor(want, "A"); r != ontology.RoleMember {
		t.Errorf("A role = %q, want member after re-admission (stale reader must not survive)", r)
	}
	if _, ok := memberOf(want, "A"); !ok {
		t.Error("A must be admitted after re-grant")
	}

	// 50-permutation shuffle proof: result is order-independent.
	rng := rand.New(rand.NewPCG(31, 17))
	for trial := range 50 {
		shuffled := make([]ontology.MembershipEvent, len(base))
		copy(shuffled, base)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := ontology.ReduceMembership(shuffled)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d diverged:\n got = %+v\nwant = %+v", trial, got, want)
		}
	}
}

// TestRole_LastOwnerProtection_RevokeThenSelfDemote is the regression test for
// the countSurvivingOwners fix: when owner A authority-approves a revoke of the
// only other owner B, a subsequent self-demote by A must be IGNORED — the fold
// must not count the already-revoked B as a surviving owner and allow the demote.
// After phase 4, A must still be an admitted owner (≥1 owner invariant preserved).
//
// Also verifies the shuffle invariant: 50 permutations of the same event set
// converge on the same result, confirming the fix is order-independent.
func TestRole_LastOwnerProtection_RevokeThenSelfDemote(t *testing.T) {
	t.Parallel()
	// G = genesis (owner); B admitted as owner; G revokes B; G tries to demote itself.
	base := []ontology.MembershipEvent{
		grant("01", "G", "G", 0),                             // genesis G = owner
		grantWithRole("02", "G", "B", 0, ontology.RoleOwner), // B = owner (second owner)
		revoke("03", "G", "B"),                               // G revokes B (authority-approved)
		roleChanged("04", "G", "G", ontology.RoleMember),     // G tries to demote itself — must be IGNORED
	}
	want := ontology.ReduceMembership(base)

	// G must still be owner: the self-demote is blocked by last-owner protection
	// because B is effectively revoked at fold position 04.
	if r := roleFor(want, "G"); r != ontology.RoleOwner {
		t.Errorf("G role = %q, want owner (self-demote after revoking B must be ignored)", r)
	}
	// B must not be in the final ACL (effectively revoked by event 03).
	if _, ok := memberOf(want, "B"); ok {
		t.Error("B must be removed from ACL (revoke at 03 is authority-approved)")
	}
	// Team must have exactly one admitted member: G.
	if got := admittedUIDs(want); len(got) != 1 || got[0] != "G" {
		t.Errorf("admitted = %v, want [G] only", got)
	}

	// 50-permutation shuffle: order-independence proof.
	rng := rand.New(rand.NewPCG(53, 11))
	for trial := range 50 {
		shuffled := make([]ontology.MembershipEvent, len(base))
		copy(shuffled, base)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := ontology.ReduceMembership(shuffled)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d diverged:\n got = %+v\nwant = %+v", trial, got, want)
		}
	}
}

// TestPinned_EmptySigKeyIDMatchesAny verifies that when pin.SigKeyID is empty,
// any self-grant by the pinned principal qualifies as genesis.
func TestPinned_EmptySigKeyIDMatchesAny(t *testing.T) {
	t.Parallel()
	events := []ontology.MembershipEvent{
		grantWithSig("01", "G", "G", "kid:anything", 0),
		grant("02", "G", "A", 0),
	}
	pin := &ontology.GenesisPin{PrincipalUID: "G"} // SigKeyID intentionally empty
	got := ontology.ReduceMembershipPinned(events, pin)

	if got.GenesisUID != "G" {
		t.Errorf("genesis = %q, want G (empty SigKeyID matches any)", got.GenesisUID)
	}
	if want := []string{"A", "G"}; !reflect.DeepEqual(admittedUIDs(got), want) {
		t.Errorf("admitted = %v, want %v", admittedUIDs(got), want)
	}
}
