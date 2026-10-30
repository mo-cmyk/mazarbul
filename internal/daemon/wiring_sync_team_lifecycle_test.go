//go:build integration

package daemon

// wiring_sync_team_lifecycle_test.go — D1b: full two-user lifecycle proof.
//
// Build with -tags integration (this test is too slow for the regular gate:
// three device boots + four git transport round-trips + epoch rotation).
//
// Phase 1 — Genesis + grant B (writer).
// Phase 2 — Bidirectional writes → seal+push → consume → byte-identical
//            canonical team-scope dump.
// Phase 3 — Grant C (reader) → reads OK → write rejected (ErrPermission).
// Phase 4 — Revoke B → epoch rotation → B cannot decode epoch+1 chunks.
//
// All helpers (device, remote, sync drivers, assertions, canonicalDump) live
// in teamtest_helpers_test.go and are shared with the other E2E test files.

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// TestTeamScope_FullLifecycleTwoUser is the D1b full-lifecycle E2E proof.
func TestTeamScope_FullLifecycleTwoUser(t *testing.T) {
	// NOT parallel: three sequential device boots are deliberately
	// single-threaded to avoid go-ladybug concurrent-open panics and to keep
	// the test deterministic.
	ctx := context.Background()
	remote := newBareTeamRemote(t)

	// ── Devices ──────────────────────────────────────────────────────────────
	deviceA := newDevice(t, "A") // genesis owner
	deviceB := newDevice(t, "B") // member (writer)
	deviceC := newDevice(t, "C") // reader

	aPrincipal := deviceA.principalUID(t)
	bPrincipal := deviceB.principalUID(t)
	cPrincipal := deviceC.principalUID(t)

	if aPrincipal == bPrincipal || aPrincipal == cPrincipal || bPrincipal == cPrincipal {
		t.Fatalf("devices share a principal — Watchwords must yield distinct identities; A=%s B=%s C=%s", aPrincipal, bPrincipal, cPrincipal)
	}

	// ── Phase 1: Genesis + grant B ────────────────────────────────────────────
	bJoinKey := joinKeyBlob(t, deviceB)
	cJoinKey := joinKeyBlob(t, deviceC)

	createResp := deviceA.invoke(t, "team_create", map[string]any{"remote": remote})
	assertJSONField(t, "team_create", createResp, "status", "created")
	assertJSONField(t, "team_create", createResp, "genesisPrincipalUid", aPrincipal)

	grantBResp := deviceA.invoke(t, "team_grant", map[string]any{"joinKey": bJoinKey})
	assertJSONField(t, "team_grant", grantBResp, "granteePrincipalUid", bPrincipal)
	assertJSONField(t, "team_grant", grantBResp, "status", "granted")

	dispA := deviceA.dispatcher(t)
	requireMember(t, dispA, aPrincipal, true, "A: genesis self")
	requireMember(t, dispA, bPrincipal, true, "A: after grant B")

	// ── Phase 2a: A writes team fact ─────────────────────────────────────────
	const aFactKey = "pref.coffee.type"
	const aFactVal = "espresso"
	deviceA.invoke(t, ontologyFactToolName, map[string]any{
		"subject":  "self",
		"stateKey": aFactKey,
		"value":    aFactVal,
		"scope":    string(kernel.ScopeTeam),
	})
	navA := deviceA.teamNavigator(t)
	if !hasFact(factsForSubject(t, navA, aPrincipal), aFactKey, aFactVal) {
		t.Fatalf("A: team fact %s=%s not visible to A before sync", aFactKey, aFactVal)
	}

	// ── Phase 2b: A seals+pushes; B bootstraps DEK + consumes ────────────────
	svA := deviceA.newSupervisor(t, remote)
	svA.sealOnce(ctx, kernel.ScopeTeam)
	svA.pushOnce(ctx, kernel.ScopeTeam)
	assertRemoteHasTeamChunk(ctx, t, svA)
	assertRecipientsRoster(t, deviceA.dataDir, aPrincipal, bPrincipal)

	configureJoinerTeamRemote(t, deviceB, remote)
	svB0 := deviceB.newConsumeSupervisor(t, remote)
	svB0.consumeOnce(ctx, kernel.ScopeTeam)

	dispB := deviceB.dispatcher(t)
	if owner, err := dispB.GenesisOwner(ctx); err != nil || owner != aPrincipal {
		t.Fatalf("B genesis owner = %q (err %v), want %q", owner, err, aPrincipal)
	}
	requireMember(t, dispB, aPrincipal, true, "B: A enrolled via graft")
	requireMember(t, dispB, bPrincipal, true, "B: B admitted via graft")
	navB := deviceB.teamNavigator(t)
	if !hasFact(factsForSubject(t, navB, aPrincipal), aFactKey, aFactVal) {
		t.Fatalf("B: A's team fact %s=%s not readable after consume", aFactKey, aFactVal)
	}

	// ── Phase 2c: B writes team fact + seals+pushes ──────────────────────────
	const bFactKey = "pref.coffee.type"
	const bFactVal = "latte"
	deviceB.invoke(t, ontologyFactToolName, map[string]any{
		"subject":  "self",
		"stateKey": bFactKey,
		"value":    bFactVal,
		"scope":    string(kernel.ScopeTeam),
	})

	svB := deviceB.newSupervisor(t, remote)
	svB.sealOnce(ctx, kernel.ScopeTeam)
	svB.pushOnce(ctx, kernel.ScopeTeam)

	// ── Phase 2d: A consumes B's chunk ────────────────────────────────────────
	svA.consumeOnce(ctx, kernel.ScopeTeam)
	if !hasFact(factsForSubject(t, deviceA.teamNavigator(t), bPrincipal), bFactKey, bFactVal) {
		t.Fatalf("A: B's team fact %s=%s not readable after A consumes B's chunk", bFactKey, bFactVal)
	}

	// ── Phase 2e: Canonical dump must match byte-for-byte ────────────────────
	// B idempotently re-consumes (GraftSealed skips known UIDs).
	svB.consumeOnce(ctx, kernel.ScopeTeam)

	dumpA := canonicalDump(t, ctx, dispA, string(kernel.ScopeTeam))
	dumpB := canonicalDump(t, ctx, dispB, string(kernel.ScopeTeam))
	if dumpA != dumpB {
		t.Fatalf("canonical team dump diverged after bidirectional sync\nA:\n%s\nB:\n%s", dumpA, dumpB)
	}

	// ── Phase 3: Grant C as reader → reads OK, write rejected ────────────────
	grantCResp := deviceA.invoke(t, "team_grant", map[string]any{
		"joinKey": cJoinKey,
		"role":    "reader",
	})
	assertJSONField(t, "team_grant", grantCResp, "granteePrincipalUid", cPrincipal)
	assertJSONField(t, "team_grant", grantCResp, "role", "reader")

	svA.sealOnce(ctx, kernel.ScopeTeam)
	svA.pushOnce(ctx, kernel.ScopeTeam)

	configureJoinerTeamRemote(t, deviceC, remote)
	svC := deviceC.newConsumeSupervisor(t, remote)
	svC.consumeOnce(ctx, kernel.ScopeTeam)

	dispC := deviceC.dispatcher(t)
	requireMember(t, dispC, aPrincipal, true, "C: A enrolled via graft")
	requireMember(t, dispC, cPrincipal, true, "C: C admitted via graft")

	navC := deviceC.teamNavigator(t)
	if !hasFact(factsForSubject(t, navC, bPrincipal), bFactKey, bFactVal) {
		t.Fatalf("C (reader): B's fact %s=%s not visible after consume", bFactKey, bFactVal)
	}

	_, writeErr := deviceC.invokeMayError(t, ontologyFactToolName, map[string]any{
		"subject":  "self",
		"stateKey": "state.intruder",
		"value":    "yes",
		"scope":    string(kernel.ScopeTeam),
	})
	if !isErrPermission(writeErr) {
		t.Fatalf("C (reader) team write: got %v, want ErrPermission", writeErr)
	}

	// ── Phase 4: Revoke B → epoch rotation → B cannot read epoch+1 chunks ────
	revokeResp := deviceA.invoke(t, "team_revoke", map[string]any{
		"principalUid": bPrincipal,
		"reason":       "lifecycle test revoke",
	})
	assertJSONField(t, "team_revoke", revokeResp, "revokedPrincipalUid", bPrincipal)
	newEpoch := jsonIntField(t, revokeResp, "newDekEpoch")
	if newEpoch < 1 {
		t.Fatalf("team_revoke: newDekEpoch = %d, want ≥1", newEpoch)
	}
	requireMember(t, dispA, bPrincipal, false, "A: B removed after revoke")

	const postRevokeVal = "ristretto"
	deviceA.invoke(t, ontologyFactToolName, map[string]any{
		"subject":  "self",
		"stateKey": aFactKey,
		"value":    postRevokeVal,
		"scope":    string(kernel.ScopeTeam),
	})

	// New supervisor picks up the epoch+1 DEK from A's keychain.
	svA2 := deviceA.newSupervisor(t, remote)
	svA2.sealOnce(ctx, kernel.ScopeTeam)
	svA2.pushOnce(ctx, kernel.ScopeTeam)

	// B tries to consume the epoch+1 chunk — its DEK ring has only epoch-0.
	svB.consumeOnce(ctx, kernel.ScopeTeam)

	navBAfterRevoke := deviceB.teamNavigator(t)
	bViewFacts := factsForSubject(t, navBAfterRevoke, aPrincipal)
	if hasFact(bViewFacts, aFactKey, postRevokeVal) {
		t.Errorf("B: post-revoke fact %s=%s visible to revoked member — forward secrecy broken", aFactKey, postRevokeVal)
	}
	// Pre-revoke epoch-0 history must remain readable by B.
	if !hasFact(bViewFacts, aFactKey, aFactVal) {
		t.Errorf("B: pre-revoke fact %s=%s missing — revoke must not erase epoch-0 history", aFactKey, aFactVal)
	}
}
