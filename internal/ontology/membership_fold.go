package ontology

import (
	"sort"
	"time"
)

// Role constants for team membership RBAC. The hierarchy is:
// owner > admin > member > reader. Use RoleAtLeast for comparisons.
const (
	// RoleOwner is the top-level role. The genesis principal always holds this
	// role; it cannot be demoted by any event. Only an owner may assign or
	// change admin or owner roles.
	RoleOwner = "owner"

	// RoleAdmin may grant members, revoke members, and change member/reader
	// roles. An admin cannot assign or change admin or owner roles (only an
	// owner may do that).
	RoleAdmin = "admin"

	// RoleMember is the default role for newly admitted principals. A member
	// may write team-scope content but cannot change membership or roles.
	RoleMember = "member"

	// RoleReader may read team-scope content but cannot write any content
	// or perform membership operations.
	RoleReader = "reader"
)

// roleRank maps role strings to a numeric rank for comparison. Higher = more
// authority.
//
//nolint:gochecknoglobals // package-level constant table; no mutable state
var roleRank = map[string]int{
	RoleOwner:  4,
	RoleAdmin:  3,
	RoleMember: 2,
	RoleReader: 1,
}

// RoleAtLeast reports whether role r has at least the authority of min.
// An unrecognised or empty role is treated as below reader (rank 0).
// Callers outside this package (the dispatcher, service layer) use this
// for role-based access checks without importing the rank table directly.
func RoleAtLeast(r, min string) bool {
	return roleRank[r] >= roleRank[min] && roleRank[min] > 0
}

// MembershipEvent is one raw element of a team's append-only membership
// OR-set: a teamMemberGranted, teamMemberRevoked, or teamRoleChanged event
// reduced to the fields the authority fold needs. AuthorPrincipal is the
// SIGNED author (ev.Author.PrincipalUID) — the granting/revoking authority —
// not a payload field.
type MembershipEvent struct {
	EventUID        string // the event's own ULID — the canonical total order
	Kind            string // KindTeamMemberGranted | KindTeamMemberRevoked | KindTeamRoleChanged
	AuthorPrincipal string // signed author = the authority
	AuthorSigKeyID  string // signing-key fingerprint of the author ("kid:…"); used by pinned genesis selection
	Subject         string // grantee (grant) | revoked principal (revoke) | role-change target
	OccurredAt      time.Time

	// Grant-only fields; zero/empty for revokes and role-change events.
	WrappedDEK     string
	GranteeWrapKey string
	DEKEpoch       int

	// Role is the access role. Populated for grants (initial role at
	// admission; absent means "member") and for teamRoleChanged events (the
	// new target role). Not meaningful for revokes.
	Role string
}

// AdmittedMember is one row of the materialized ACL: a principal that is
// currently an admitted, non-revoked member, with the (highest-epoch)
// wrapped DEK in force for them, and their effective access role.
type AdmittedMember struct {
	PrincipalUID  string
	AdmittedByUID string // event UID of the lowest-ULID grant that admitted them (deterministic)
	DEKEpoch      int
	WrappedDEK    string
	WrapPublicKey string
	IsGenesis     bool
	Role          string // effective role: owner | admin | member | reader
}

// MembershipState is the deterministic result of folding a team's full
// membership event set: the current ACL, the quarantined events, the
// current (highest) DEK epoch, and the genesis owner.
type MembershipState struct {
	Admitted     []AdmittedMember // current ACL, sorted by PrincipalUID
	Quarantined  []string         // event UIDs whose author is not (yet) reachable from genesis
	CurrentEpoch int
	GenesisUID   string // genesis owner principal UID; "" when no genesis self-grant exists
}

// ReduceMembership folds a team's FULL membership event set into the
// canonical ACL + quarantine. It is a PURE, replay-order-independent
// function: every device that has replayed the same set of events converges
// on byte-identical output regardless of arrival order — the sort by EventUID
// produces a deterministic total order, so the result is a pure function of
// the event SET.
//
// The model runs in four phases:
//
//  1. Genesis = the lowest-ULID self-grant (granted ∧ author==subject) —
//     the TOFU root. No self-grant ⇒ empty ACL, every event quarantined.
//
//  2. Admission A = the least fixpoint of reachability from genesis through
//     grants authored by already-admitted principals (forward-only;
//     revoking a member does not un-admit members they previously granted).
//
//  3. Role and revoke authority: events are processed in ULID order.
//     The genesis principal is ALWAYS owner (immutable). For each event,
//     the author's role is the effective role computed from all events with
//     a LOWER ULID. Authority rules:
//     - Revokes and role changes are honored only when the author held
//     owner or admin at that position in ULID order.
//     - Only an owner may assign or change admin or owner roles.
//     - A revoke or demote that would leave the team with zero owners is
//     deterministically IGNORED (last-owner protection).
//     - Role changes targeting the genesis principal are IGNORED (genesis
//     is immutably owner).
//
//  4. Final ACL: admitted principals minus those whose highest
//     authority-approved revoke ULID out-ranks their highest admitted grant
//     ULID. DEK fields come from the highest-(epoch,ULID) admitted grant
//     (unchanged from the admission-only fold).
//
// Quarantine is purely derived: an event whose author is not in A is
// quarantined regardless of kind. Events whose author IS in A but fail
// authority checks (member tries to revoke; admin tries to promote to owner)
// are simply ignored — not quarantined — because their author IS admitted;
// only a missing authorizing chain earns quarantine.
//
// ReduceMembership is equivalent to ReduceMembershipPinned(events, nil):
// it uses the TOFU lowest-ULID genesis selection. Call ReduceMembershipPinned
// with a non-nil *GenesisPin to close the remote-replay hijack surface.
func ReduceMembership(events []MembershipEvent) MembershipState {
	return ReduceMembershipPinned(events, nil)
}

// ReduceMembershipPinned is the pinned-genesis variant of ReduceMembership.
// When pin is nil the fold is identical to ReduceMembership (lowest-ULID TOFU).
// When pin is non-nil, genesis is the self-grant that matches the pin:
//
//   - Kind == KindTeamMemberGranted
//   - AuthorPrincipal == Subject == pin.PrincipalUID
//   - If pin.SigKeyID is non-empty: AuthorSigKeyID == pin.SigKeyID
//
// A self-grant with a lower ULID authored by a DIFFERENT principal is not
// selected and its events are quarantined (that principal is not admitted).
// Fail-closed: if the pinned principal has no matching self-grant, the result
// has an empty genesis and every event is quarantined.
//
// The function is PURE: same events + same pin → same state in any order.
func ReduceMembershipPinned(events []MembershipEvent, pin *GenesisPin) MembershipState {
	sorted := sortMembershipByUID(events)
	genesis := selectGenesisUID(sorted, pin)
	if genesis == "" {
		return MembershipState{Quarantined: allEventUIDs(sorted)}
	}

	// Phase 1+2: compute admission set via reachability fixpoint.
	admitted := computeReachable(sorted, genesis)

	// Precompute the admitting grant UID per admitted principal (the lowest-ULID
	// admitted grant — the role baseline and the AdmittedByUID in the DEK row).
	admittedByUID := computeAdmittedByUIDs(sorted, admitted)

	// Phase 3: sequential role/authority evaluation in ULID order.
	roles, _, authorityApprovedRevokes := applyPhase3(sorted, genesis, admitted, admittedByUID)

	// Phase 4: build the final ACL.
	return buildPhase4State(sorted, genesis, admitted, roles, authorityApprovedRevokes)
}

// selectGenesisUID returns the genesis principal UID using the strategy
// appropriate for pin: nil → lowest-ULID TOFU; non-nil → pinned principal.
// Returns "" when no genesis can be determined.
func selectGenesisUID(sorted []MembershipEvent, pin *GenesisPin) string {
	if pin == nil {
		return lowestSelfGrant(sorted)
	}
	return pinnedGenesisUID(sorted, pin)
}

// pinnedGenesisUID returns the principal UID of the pinned genesis self-grant,
// or "" (fail-closed) when no self-grant matches the pin. Match criteria:
//
//   - Kind == KindTeamMemberGranted, AuthorPrincipal == Subject == pin.PrincipalUID
//   - If pin.SigKeyID non-empty: AuthorSigKeyID == pin.SigKeyID
func pinnedGenesisUID(sorted []MembershipEvent, pin *GenesisPin) string {
	for i := range sorted {
		e := sorted[i]
		if e.Kind != KindTeamMemberGranted {
			continue
		}
		if e.AuthorPrincipal != pin.PrincipalUID || e.Subject != pin.PrincipalUID {
			continue
		}
		if pin.SigKeyID != "" && e.AuthorSigKeyID != pin.SigKeyID {
			continue
		}
		return e.Subject
	}
	return ""
}

// applyPhase3 processes every event in ULID order to produce the effective
// role per principal (roles, roleSetUID) and the set of authority-approved
// revokes. Genesis starts as owner (immutable invariant).
func applyPhase3(
	sorted []MembershipEvent,
	genesis string,
	admitted map[string]struct{},
	admittedByUID map[string]string,
) (roles, roleSetUID, authorityApprovedRevokes map[string]string) {
	roles = map[string]string{genesis: RoleOwner}
	roleSetUID = map[string]string{genesis: ""} // "" sorts below any real ULID
	authorityApprovedRevokes = map[string]string{}
	for i := range sorted {
		e := sorted[i]
		if _, authorAdmitted := admitted[e.AuthorPrincipal]; !authorAdmitted {
			continue // quarantined author; skip entirely
		}
		authorRole := roles[e.AuthorPrincipal]
		switch e.Kind {
		case KindTeamMemberGranted:
			applyGrantRole(&sorted[i], genesis, admitted, admittedByUID, roles, roleSetUID, authorityApprovedRevokes)
		case KindTeamRoleChanged:
			applyRoleChange(&sorted[i], genesis, admitted, authorRole, roles, roleSetUID, admittedByUID, authorityApprovedRevokes)
		case KindTeamMemberRevoked:
			applyRevokeRecord(&sorted[i], genesis, admitted, authorRole, roles, admittedByUID, authorityApprovedRevokes)
		}
	}
	return
}

// applyGrantRole handles a teamMemberGranted in Phase 3. It sets the role
// baseline in two cases:
//
//  1. Original admission: this event IS the lowest-ULID admitted grant
//     (admittedByUID[subject] == e.EventUID).
//
//  2. Re-admission after revoke: a prior authority-approved revoke exists
//     with a UID strictly less than e.EventUID, AND the currently-tracked
//     admitting grant UID (admittedByUID[subject]) is also less than that
//     revoke — meaning the subject is in a revoked state at this fold
//     position. In this case the grant is treated as a fresh admission:
//     admittedByUID is advanced to e.EventUID and the role latch is reset,
//     overriding any stale teamRoleChanged from before the revoke.
//
// All other grants (re-keys, grants to already-re-admitted principals) are
// skipped. The genesis subject is always owner (immutable).
func applyGrantRole(
	e *MembershipEvent,
	genesis string,
	admitted map[string]struct{},
	admittedByUID map[string]string,
	roles, roleSetUID map[string]string,
	authorityApprovedRevokes map[string]string,
) {
	if _, ok := admitted[e.Subject]; !ok {
		return
	}
	if e.Subject == genesis {
		return // genesis role is immutable; skip self-grants targeting genesis
	}
	if admittedByUID[e.Subject] != e.EventUID {
		// Not the original admitting grant. Check whether this is a
		// re-admission: the grant post-dates an authority-approved revoke AND
		// the current admitting-grant UID pre-dates that revoke (subject is
		// effectively revoked at this fold position).
		revokeUID, wasRevoked := authorityApprovedRevokes[e.Subject]
		if !wasRevoked || e.EventUID <= revokeUID || admittedByUID[e.Subject] >= revokeUID {
			return // re-key or no qualifying revoke; does not affect role baseline
		}
		// Re-admission: advance the admitting-grant pointer and reset the role.
		admittedByUID[e.Subject] = e.EventUID
		r := e.Role
		if r == "" {
			r = RoleMember
		}
		roles[e.Subject] = r
		roleSetUID[e.Subject] = e.EventUID
		return
	}
	r := e.Role
	if r == "" {
		r = RoleMember
	}
	if e.EventUID > roleSetUID[e.Subject] {
		roles[e.Subject] = r
		roleSetUID[e.Subject] = e.EventUID
	}
}

// applyRoleChange handles a teamRoleChanged in Phase 3: honors only changes
// authored by admin/owner at that ULID position, and enforces the owner-only
// guard for admin/owner target roles and last-owner protection.
func applyRoleChange(
	e *MembershipEvent,
	genesis string,
	admitted map[string]struct{},
	authorRole string,
	roles, roleSetUID map[string]string,
	admittedByUID map[string]string,
	authorityApprovedRevokes map[string]string,
) {
	if e.Subject == genesis {
		return // genesis role is immutable
	}
	if _, ok := admitted[e.Subject]; !ok {
		return
	}
	if !RoleAtLeast(authorRole, RoleAdmin) {
		return // author lacks authority
	}
	newRole := e.Role
	if newRole == "" {
		return // malformed payload; ignore
	}
	currentRole := roles[e.Subject]
	// Only an owner may assign or change admin or owner roles.
	if (newRole == RoleAdmin || newRole == RoleOwner ||
		currentRole == RoleAdmin || currentRole == RoleOwner) &&
		authorRole != RoleOwner {
		return
	}
	// Last-owner protection: a demote leaving zero surviving owners is ignored.
	// countSurvivingOwners excludes principals already authority-approved-revoked
	// (without a later re-admission grant), so a revoke-then-self-demote sequence
	// cannot strand the team with zero owners after phase-4 pruning.
	if currentRole == RoleOwner && newRole != RoleOwner &&
		countSurvivingOwners(roles, admitted, admittedByUID, authorityApprovedRevokes) <= 1 {
		return
	}
	if e.EventUID > roleSetUID[e.Subject] {
		roles[e.Subject] = newRole
		roleSetUID[e.Subject] = e.EventUID
	}
}

// applyRevokeRecord handles a teamMemberRevoked in Phase 3: records the
// revoke as authority-approved when the author held admin/owner at that
// ULID position, subject to genesis protection and last-owner protection.
func applyRevokeRecord(
	e *MembershipEvent,
	genesis string,
	admitted map[string]struct{},
	authorRole string,
	roles map[string]string,
	admittedByUID map[string]string,
	authorityApprovedRevokes map[string]string,
) {
	if e.Subject == genesis {
		return // genesis is never revocable
	}
	if _, ok := admitted[e.Subject]; !ok {
		return
	}
	if !RoleAtLeast(authorRole, RoleAdmin) {
		return // author lacks authority
	}
	// Last-owner protection: do not zero out the surviving owner set.
	// countSurvivingOwners excludes principals already authority-approved-revoked
	// so a chain of revokes cannot silently drain every owner before phase 4.
	if roles[e.Subject] == RoleOwner &&
		countSurvivingOwners(roles, admitted, admittedByUID, authorityApprovedRevokes) <= 1 {
		return
	}
	if cur, exists := authorityApprovedRevokes[e.Subject]; !exists || e.EventUID > cur {
		authorityApprovedRevokes[e.Subject] = e.EventUID
	}
}

// buildPhase4State assembles the final MembershipState from the admitted set,
// authority-approved revokes, effective roles, and DEK rows.
func buildPhase4State(
	sorted []MembershipEvent,
	genesis string,
	admitted map[string]struct{},
	roles map[string]string,
	authorityApprovedRevokes map[string]string,
) MembershipState {
	state := MembershipState{GenesisUID: genesis}
	for _, p := range sortedKeys(admitted) {
		if p != genesis {
			if revUID, wasRevoked := authorityApprovedRevokes[p]; wasRevoked {
				if revUID > newestAdmittedGrantUID(sorted, admitted, p) {
					continue // effectively revoked
				}
			}
		}
		m, ok := dekRowFor(sorted, admitted, p, p == genesis)
		if !ok {
			continue // defensive: no admitted grant materialises a DEK row
		}
		m.Role = roles[p]
		if m.Role == "" {
			m.Role = RoleMember
		}
		if p == genesis {
			m.Role = RoleOwner
		}
		state.Admitted = append(state.Admitted, m)
		if m.DEKEpoch > state.CurrentEpoch {
			state.CurrentEpoch = m.DEKEpoch
		}
	}
	state.Quarantined = quarantinedUIDs(sorted, admitted)
	return state
}

// countSurvivingOwners returns how many admitted principals currently hold the
// owner role AND will not be removed by an authority-approved revoke in phase 4.
// A principal is excluded when its admitting-grant pointer (admittedByUID[p])
// pre-dates its authority-approved revoke UID, meaning phase 4 will drop it.
// This makes last-owner protection forward-looking: a revoke-then-self-demote
// (or revoke-then-revoke) sequence cannot strand the team with zero owners after
// phase-4 pruning.
//
// Genesis always survives: it is blocked from revoke by the genesis guard in
// applyRevokeRecord and is immutably overridden to owner in buildPhase4State.
func countSurvivingOwners(
	roles map[string]string,
	admitted map[string]struct{},
	admittedByUID map[string]string,
	authorityApprovedRevokes map[string]string,
) int {
	n := 0
	for p := range admitted {
		if roles[p] != RoleOwner {
			continue
		}
		if revokeUID, wasRevoked := authorityApprovedRevokes[p]; wasRevoked {
			if admittedByUID[p] < revokeUID {
				continue // effectively revoked: admitting grant pre-dates the revoke
			}
		}
		n++
	}
	return n
}

// computeAdmittedByUIDs returns the lowest-ULID admitted grant UID for each
// principal in the admitted set. This identifies the "admitting grant" —
// the unique event that first admitted the principal — which is the source
// of their initial role assignment.
func computeAdmittedByUIDs(sorted []MembershipEvent, admitted map[string]struct{}) map[string]string {
	out := map[string]string{}
	for i := range sorted {
		e := sorted[i]
		if e.Kind != KindTeamMemberGranted {
			continue
		}
		if _, subjectOK := admitted[e.Subject]; !subjectOK {
			continue
		}
		if _, authorOK := admitted[e.AuthorPrincipal]; !authorOK {
			continue
		}
		cur, exists := out[e.Subject]
		if !exists || e.EventUID < cur {
			out[e.Subject] = e.EventUID
		}
	}
	return out
}

// newestAdmittedGrantUID returns the highest-ULID admitted grant for p —
// used by the revoke check to determine whether a re-grant post-dates the
// authority-approved revoke.
func newestAdmittedGrantUID(sorted []MembershipEvent, admitted map[string]struct{}, p string) string {
	var newest string
	for i := range sorted {
		e := sorted[i]
		if e.Kind != KindTeamMemberGranted || e.Subject != p {
			continue
		}
		if _, authorOK := admitted[e.AuthorPrincipal]; !authorOK {
			continue
		}
		if e.EventUID > newest {
			newest = e.EventUID
		}
	}
	return newest
}

// sortMembershipByUID returns a copy sorted by EventUID ascending, so the
// fold's iteration order is a pure function of set contents, not arrival.
//
// Correctness rests on every EventUID being a well-formed 26-char Crockford
// ULID — guaranteed for the live append path (the kernel mints the UID) and
// for replayed events (signed CanonicalBytes include the UID). The lexical
// ordering here and the ULID comparisons in the fold are only meaningful for
// well-formed inputs; a malformed UID is part of the same remote-bytes trust
// boundary documented on lowestSelfGrant.
func sortMembershipByUID(events []MembershipEvent) []MembershipEvent {
	out := make([]MembershipEvent, len(events))
	copy(out, events)
	sort.SliceStable(out, func(i, j int) bool { return out[i].EventUID < out[j].EventUID })
	return out
}

// lowestSelfGrant returns the subject of the lowest-ULID self-grant
// (granted ∧ author==subject) — the TOFU genesis root — or "" when none
// exists. This is the UNPINNED path: called only from selectGenesisUID when
// pin is nil. When a GenesisPin is available callers should use
// ReduceMembershipPinned to close the remote-replay hijack surface.
//
// KNOWN LIMITATION (unpinned mode only): genesis is chosen by lowest ULID,
// which is fail-open against an adversary who controls bytes on the shared
// remote. A party with git push can craft a self-grant with a synthetically
// low ULID and hijack the genesis on replay. The pinned path
// (ReduceMembershipPinned with a non-nil GenesisPin) closes this by anchoring
// to the principal-UID + signing-key fingerprint stored out of band in
// state.toml and TEAM.json.
func lowestSelfGrant(sorted []MembershipEvent) string {
	for i := range sorted {
		e := sorted[i]
		if e.Kind == KindTeamMemberGranted && e.AuthorPrincipal == e.Subject {
			return e.Subject
		}
	}
	return ""
}

// computeReachable returns the least-fixpoint admission set: principals
// reachable from genesis through grants authored by admitted principals.
func computeReachable(sorted []MembershipEvent, genesis string) map[string]struct{} {
	admitted := map[string]struct{}{genesis: {}}
	for {
		changed := false
		for i := range sorted {
			e := sorted[i]
			if e.Kind != KindTeamMemberGranted {
				continue
			}
			if _, authorOK := admitted[e.AuthorPrincipal]; !authorOK {
				continue
			}
			if _, already := admitted[e.Subject]; already {
				continue
			}
			admitted[e.Subject] = struct{}{}
			changed = true
		}
		if !changed {
			return admitted
		}
	}
}

// dekRowFor builds p's ACL row: admittedBy is the lowest-ULID admitted
// grant; the DEK fields come from the admitted grant with the highest
// (DEKEpoch, EventUID) so a forward re-key supersedes deterministically.
// The returned AdmittedMember has an empty Role field; the caller sets it.
func dekRowFor(sorted []MembershipEvent, admitted map[string]struct{}, p string, isGenesis bool) (AdmittedMember, bool) {
	var admittedBy string
	var best *MembershipEvent
	for i := range sorted {
		e := sorted[i]
		if e.Kind != KindTeamMemberGranted || e.Subject != p {
			continue
		}
		if _, authorOK := admitted[e.AuthorPrincipal]; !authorOK {
			continue
		}
		if admittedBy == "" || e.EventUID < admittedBy {
			admittedBy = e.EventUID
		}
		if best == nil || e.DEKEpoch > best.DEKEpoch || (e.DEKEpoch == best.DEKEpoch && e.EventUID > best.EventUID) {
			best = &sorted[i]
		}
	}
	if best == nil {
		return AdmittedMember{}, false
	}
	return AdmittedMember{
		PrincipalUID:  p,
		AdmittedByUID: admittedBy,
		DEKEpoch:      best.DEKEpoch,
		WrappedDEK:    best.WrappedDEK,
		WrapPublicKey: best.GranteeWrapKey,
		IsGenesis:     isGenesis,
	}, true
}

// quarantinedUIDs returns, sorted, the UIDs of events whose author is not
// in the admission set — their authorizing chain has not (yet) arrived.
func quarantinedUIDs(sorted []MembershipEvent, admitted map[string]struct{}) []string {
	var out []string
	for i := range sorted {
		if _, ok := admitted[sorted[i].AuthorPrincipal]; !ok {
			out = append(out, sorted[i].EventUID)
		}
	}
	return out
}

func allEventUIDs(sorted []MembershipEvent) []string {
	if len(sorted) == 0 {
		return nil
	}
	out := make([]string, len(sorted))
	for i := range sorted {
		out[i] = sorted[i].EventUID
	}
	return out
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
