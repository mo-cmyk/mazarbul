package daemon

// wiring_sync_team_e2e_test.go is the load-bearing end-to-end proof of the
// #213 team-scope multi-writer envelope-encryption chain across TWO genuinely
// separate device identities (distinct keychains + distinct dispatchers) and
// one shared bare git remote.
//
// The unit layers are proven elsewhere (chunk encode/decode + AAD header auth,
// GraftSealed idempotency/tamper-reject, DEK bootstrap + ring, membership fold
// convergence, grant-vouching unit pieces). This test COMPOSES them through the
// real daemon wiring (OpenWiring + the package-private syncSupervisor) driven
// SYNCHRONOUSLY — sealOnce / pushOnce / consumeOnce, never the background loops
// (which are timer-driven and non-deterministic).
//
// The chain it exercises, in order:
//
//	A: team_create  → mint epoch-0 DEK, genesis self-grant (vouches A's signing
//	                   key), enable envelope mode + pin the team remote.
//	B: team_key     → produce B's join-key blob (B's principal + wrap pubkey +
//	                   signing identity).
//	A: team_grant   → wrap epoch-0 DEK to B, append a grant vouching B's key.
//	A: fact(team)   → a signed, team-scoped event authored by A.
//	A: sealOnce + pushOnce → envelope-encrypted chunk(s) + recipients.jsonl to
//	                   the shared remote.
//	B: consumeOnce  → pull, bootstrap DEK from recipients.jsonl, decode, graft.
//	                   The genesis self-grant grafts FIRST (lowest ULID) and its
//	                   projection enrolls A's signing key into B's devices table
//	                   BEFORE A's later events verify. That is the crux.
//
// Assertions (the proof):
//  1. B's dispatcher admits BOTH A and B; GenesisOwner == A.
//  2. B can read A's team fact under a {team}-scoped reader.
//  3. A never-granted device C (its own keychain, no DEK) that pulls + consumes
//     cannot decode the chunks — empty ring, zero team events, IsTeamMember(A)
//     false — proving envelope encryption actually gates access.
//
// All helpers (device, remote, sync drivers, assertions) live in
// teamtest_helpers_test.go so every new E2E below can compose them.

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// TestTeamScope_TwoDeviceEnvelopeChain is the composed E2E proof.
func TestTeamScope_TwoDeviceEnvelopeChain(t *testing.T) {
	// NOT parallel: go-ladybug OpenDatabase is not concurrency-safe under
	// parallel opens, and the two wirings open it sequentially within this one
	// function by design.
	ctx := context.Background()
	remote := newBareTeamRemote(t)

	// --- Device A: genesis owner ---
	deviceA := newDevice(t, "A")
	aPrincipal := deviceA.principalUID(t)

	// --- Device B: joiner (opened BEFORE the grant so it can produce a join key) ---
	deviceB := newDevice(t, "B")
	bPrincipal := deviceB.principalUID(t)

	if aPrincipal == bPrincipal {
		t.Fatalf("devices A and B share a principal UID %q — distinct Watchwords must yield distinct identities", aPrincipal)
	}

	// B's join key carries B's principal + wrap pubkey + signing identity.
	bJoinKey := joinKeyBlob(t, deviceB)

	// --- A: create team genesis (mint DEK, self-grant vouching A's key, envelope mode + remote) ---
	createResp := deviceA.invoke(t, "team_create", map[string]any{"remote": remote})
	assertJSONField(t, "team_create", createResp, "genesisPrincipalUid", aPrincipal)
	assertJSONField(t, "team_create", createResp, "status", "created")

	// A is admitted as genesis immediately (local projection).
	dispA := deviceA.dispatcher(t)
	if owner, err := dispA.GenesisOwner(ctx); err != nil || owner != aPrincipal {
		t.Fatalf("A genesis owner = %q (err %v), want %q", owner, err, aPrincipal)
	}

	// --- A: grant B (wrap epoch-0 DEK to B, vouch B's signing key) ---
	grantResp := deviceA.invoke(t, "team_grant", map[string]any{"joinKey": bJoinKey})
	assertJSONField(t, "team_grant", grantResp, "granteePrincipalUid", bPrincipal)
	assertJSONField(t, "team_grant", grantResp, "status", "granted")

	// A now sees both members locally.
	requireMember(t, dispA, aPrincipal, true, "A: A self")
	requireMember(t, dispA, bPrincipal, true, "A: B after grant")

	// --- A: write a team-scope fact (signed by A) ---
	const factKey = "preference.coffee.milk"
	const factVal = "oat"
	factResp := deviceA.invoke(t, ontologyFactToolName, map[string]any{
		"subject":  "self",
		"stateKey": factKey,
		"value":    factVal,
		"scope":    string(kernel.ScopeTeam),
	})
	factName := jsonField(t, factResp, "name")
	if factName == "" {
		t.Fatalf("A: fact returned empty name: %s", factResp)
	}

	// The fact must be visible to A under a {team}-scoped read (sanity: it was
	// written to the team scope, not personal).
	navA := deviceA.teamNavigator(t)
	if f := factsForSubject(t, navA, aPrincipal); !hasFact(f, factKey, factVal) {
		t.Fatalf("A: team fact %s=%s not found under {team} scope: %+v", factKey, factVal, f)
	}

	// --- A: seal + push the team scope synchronously ---
	svA := deviceA.newSupervisor(t, remote)
	svA.sealOnce(ctx, kernel.ScopeTeam)
	svA.pushOnce(ctx, kernel.ScopeTeam)

	// The remote must now carry at least one team chunk + a recipients.jsonl
	// with A and B rows. We read the recipients off A's local staging file
	// (rendered during sealOnce) and the pushed chunk count off the transport.
	assertRemoteHasTeamChunk(ctx, t, svA)
	assertRecipientsRoster(t, deviceA.dataDir, aPrincipal, bPrincipal)

	// =====================================================================
	// Device B: pull → bootstrap DEK → decode → graft. The genesis self-grant
	// enrolls A's signing key BEFORE A's grant-of-B and fact verify.
	// =====================================================================
	configureJoinerTeamRemote(t, deviceB, remote)
	svB := deviceB.newConsumeSupervisor(t, remote)
	// Twice: consumeOnce is idempotent (GraftSealed skips known UIDs); the
	// second pass proves no double-graft and stable convergence.
	svB.consumeOnce(ctx, kernel.ScopeTeam)
	svB.consumeOnce(ctx, kernel.ScopeTeam)

	// --- Assertion 1: B's membership fold admitted BOTH A and B; genesis is A. ---
	dispB := deviceB.dispatcher(t)
	if owner, err := dispB.GenesisOwner(ctx); err != nil || owner != aPrincipal {
		t.Fatalf("B genesis owner = %q (err %v), want A %q — grant-vouching + graft did not enroll the genesis", owner, err, aPrincipal)
	}
	requireMember(t, dispB, aPrincipal, true, "B: A enrolled via genesis self-grant graft")
	requireMember(t, dispB, bPrincipal, true, "B: B admitted via A's grant graft")

	// --- Assertion 2: B can read A's team fact (signed by A, verified on graft). ---
	navB := deviceB.teamNavigator(t)
	bFacts := factsForSubject(t, navB, aPrincipal)
	if !hasFact(bFacts, factKey, factVal) {
		t.Fatalf("B: A's team fact %s=%s NOT readable after consume — the sign→graft→verify→enroll→project→read chain is broken: %+v", factKey, factVal, bFacts)
	}

	// =====================================================================
	// Device C: never granted, own keychain, no DEK. Envelope encryption must
	// gate it out entirely.
	// =====================================================================
	deviceC := newDevice(t, "C")
	cPrincipal := deviceC.principalUID(t)
	if cPrincipal == aPrincipal || cPrincipal == bPrincipal {
		t.Fatalf("C shares a principal with A or B; distinct Watchwords must yield distinct identities")
	}
	configureJoinerTeamRemote(t, deviceC, remote)
	svC := deviceC.newConsumeSupervisor(t, remote)
	svC.consumeOnce(ctx, kernel.ScopeTeam)
	svC.consumeOnce(ctx, kernel.ScopeTeam)

	// C DID pull the encrypted chunks to its local store — the gate is the
	// missing DEK, NOT a failed pull. Asserting C mirrored the same opaque
	// chunks the remote holds makes the negative load-bearing: C has the
	// ciphertext on disk and still cannot read it.
	cLocal, err := svC.store.List(ctx, kernel.ScopeTeam)
	if err != nil {
		t.Fatalf("C list local chunks: %v", err)
	}
	if len(cLocal) == 0 {
		t.Fatalf("C pulled zero chunks — the negative test is vacuous unless C actually mirrored the ciphertext")
	}
	// C holds NO DEK: the recipients roster has no row for C, so bootstrap
	// unwraps nothing and the ring is empty. The ciphertext on disk is undecodable.
	if ring := resolveTeamDEKRing(ctx, deviceC.secrets, zap.NewNop()); len(ring) != 0 {
		t.Fatalf("C resolved a non-empty DEK ring (%d epochs) — a never-granted device must hold no team DEK", len(ring))
	}
	// With no DEK, no chunk decodes, so no team event grafts: C sees no team.
	dispC := deviceC.dispatcher(t)
	if owner, err := dispC.GenesisOwner(ctx); err != nil || owner != "" {
		t.Fatalf("C genesis owner = %q (err %v), want empty — C must not have grafted any team membership event", owner, err)
	}
	requireMember(t, dispC, aPrincipal, false, "C: A must be invisible (no DEK, no graft)")
	requireMember(t, dispC, cPrincipal, false, "C: C is not a member")
	cFacts := factsForSubject(t, deviceC.teamNavigator(t), aPrincipal)
	if len(cFacts) != 0 {
		t.Fatalf("C read %d team facts for A — envelope encryption failed to gate a never-granted device: %+v", len(cFacts), cFacts)
	}
}
