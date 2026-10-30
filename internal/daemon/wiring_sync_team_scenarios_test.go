package daemon

// wiring_sync_team_scenarios_test.go — D1c scenario E2E tests. Each covers a
// distinct two-ordering or multi-device invariant using the helpers in
// teamtest_helpers_test.go. All tests call t.Skip under -short.
//
// D1c.1 — TestTeamScope_PersonalScopeConsume
//   Device A writes personal-scope facts and pushes to a plaintext bare-git
//   remote. Device A2 (different principal UID, same Watchword signing key)
//   consumes and sees A's facts under A's principal UID.
//
// D1c.2 — TestTeamScope_ConvergentFactConflict
//   A and B write to the same (subject, stateKey) slot from independent
//   positions. After full bidirectional sync both devices converge to the same
//   open value (HLC-latest wins).
//
// D1c.5 — NEEDS-PROD-CHANGE: paired-device team E2E via provisionPairedDevice
//   (internal/cli/mellon.go) cannot be imported into daemon package tests.
//   Follow-up: expose a testable adapter in a shared internal/testutil/team pkg.
//
// D1c.6 — TestTeamScope_GenesisPinE2E
//   Genesis trust anchor (TEAM.json) is imported on B's second consume (after
//   the first consume grafts A's genesis signing key). Verifies idempotency of
//   the pin. Tamper-rejection is covered at unit level by TestMaybeImportTeamJSONPin.

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// TestTeamScope_PersonalScopeConsume is D1c.1.
func TestTeamScope_PersonalScopeConsume(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: two device boots + git round-trip")
	}
	ctx := context.Background()
	remote := newBareTeamRemote(t)

	// A and A2 share the same Watchword (same ed25519 signing key). They get
	// different principal UIDs on first boot from empty data dirs because
	// resolvePrincipalUID mints a fresh ULID when no binding exists in state.toml.
	// Personal-scope graft skips team-auth verification so A2 can decode A's events.
	mnemonic, err := watchword.Generate()
	if err != nil {
		t.Fatalf("generate mnemonic: %v", err)
	}
	deviceA := newDeviceWithMnemonic(t, "A", mnemonic)
	deviceA2 := newDeviceWithMnemonic(t, "A2", mnemonic)

	aPrincipal := deviceA.principalUID(t)

	const factKey = "pref.editor"
	const factVal = "neovim"
	// Must specify scope explicitly: facts written without a scope are stored
	// in the JSONL event log with scope="" (raw). sealOnce(ScopePersonal)
	// filters for ev.Scope == "personal" and would skip scope="" events.
	// The SQL projection backfills ""→personal but that happens at read time,
	// not at seal time. Use the explicit value so the sealer finds the event.
	deviceA.invoke(t, ontologyFactToolName, map[string]any{
		"subject":  "self",
		"stateKey": factKey,
		"value":    factVal,
		"scope":    string(kernel.ScopePersonal),
	})

	// A seals + pushes personal scope (nil DEK → plaintext JSONL chunk).
	svA := deviceA.newPlaintextPersonalSupervisor(t, remote)
	svA.sealOnce(ctx, kernel.ScopePersonal)
	svA.pushOnce(ctx, kernel.ScopePersonal)

	// A2 consumes: pulls, grafts A's personal-scope events.
	svA2 := deviceA2.newPlaintextPersonalSupervisor(t, remote)
	svA2.consumeOnce(ctx, kernel.ScopePersonal)

	// A2's dispatcher must contain A's personal fact under aPrincipal.
	dispA2 := deviceA2.dispatcher(t)
	reader, rerr := dispA2.SQLiteReader()
	if rerr != nil {
		t.Fatalf("A2 SQLiteReader: %v", rerr)
	}
	var count int
	if qerr := reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM facts WHERE subject_uid=? AND state_key=? AND value=? AND COALESCE(event_end,'')=''`,
		aPrincipal, factKey, factVal).Scan(&count); qerr != nil {
		t.Fatalf("A2: query personal fact: %v", qerr)
	}
	if count != 1 {
		t.Fatalf("A2: A's personal fact %s=%s not visible after consume (count=%d)", factKey, factVal, count)
	}
}

// TestTeamScope_ConvergentFactConflict is D1c.2.
func TestTeamScope_ConvergentFactConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: two device boots + bidirectional git sync")
	}
	ctx := context.Background()
	remote := newBareTeamRemote(t)

	deviceA := newDevice(t, "A")
	deviceB := newDevice(t, "B")
	aPrincipal := deviceA.principalUID(t)
	bPrincipal := deviceB.principalUID(t)
	bJoinKey := joinKeyBlob(t, deviceB)

	// A creates team, grants B.
	deviceA.invoke(t, "team_create", map[string]any{"remote": remote})
	deviceA.invoke(t, "team_grant", map[string]any{"joinKey": bJoinKey})

	// A writes the first value to the contested slot (subject = aPrincipal).
	const conflictKey = "state.phase"
	deviceA.invoke(t, ontologyFactToolName, map[string]any{
		"subject":  "self",
		"stateKey": conflictKey,
		"value":    "A-value",
		"scope":    string(kernel.ScopeTeam),
	})

	// A seals+pushes; B bootstraps DEK.
	svA := deviceA.newSupervisor(t, remote)
	svA.sealOnce(ctx, kernel.ScopeTeam)
	svA.pushOnce(ctx, kernel.ScopeTeam)

	configureJoinerTeamRemote(t, deviceB, remote)
	svB0 := deviceB.newConsumeSupervisor(t, remote)
	svB0.consumeOnce(ctx, kernel.ScopeTeam)

	// B writes to the SAME slot (aPrincipal, conflictKey) with a different value.
	// B's HLC is strictly after A's → B-value wins in the reducer.
	deviceB.invoke(t, ontologyFactToolName, map[string]any{
		"subject":  aPrincipal,
		"stateKey": conflictKey,
		"value":    "B-value",
		"scope":    string(kernel.ScopeTeam),
	})

	// B seals+pushes; A consumes.
	svB := deviceB.newSupervisor(t, remote)
	svB.sealOnce(ctx, kernel.ScopeTeam)
	svB.pushOnce(ctx, kernel.ScopeTeam)
	svA.consumeOnce(ctx, kernel.ScopeTeam)

	// B idempotently re-consumes A's data.
	svB.consumeOnce(ctx, kernel.ScopeTeam)

	// Both devices must converge to the same open value.
	dispA := deviceA.dispatcher(t)
	dispB := deviceB.dispatcher(t)
	readerA, _ := dispA.SQLiteReader()
	readerB, _ := dispB.SQLiteReader()

	var valA, valB string
	_ = readerA.QueryRowContext(ctx,
		`SELECT value FROM facts WHERE subject_uid=? AND state_key=? AND COALESCE(event_end,'')='' AND COALESCE(retracted_at,'')=''`,
		aPrincipal, conflictKey).Scan(&valA)
	_ = readerB.QueryRowContext(ctx,
		`SELECT value FROM facts WHERE subject_uid=? AND state_key=? AND COALESCE(event_end,'')='' AND COALESCE(retracted_at,'')=''`,
		aPrincipal, conflictKey).Scan(&valB)

	if valA == "" {
		t.Fatalf("A: conflict slot %s for %s is empty after sync", conflictKey, aPrincipal)
	}
	if valA != valB {
		t.Fatalf("convergent fact conflict failed: A=%q B=%q (HLC reducer must agree on same winner)", valA, valB)
	}
	_ = bPrincipal // used in team_grant above
}

// TestTeamScope_GenesisPinE2E is D1c.6.
// TEAM.json is published by A's push and imported into B's state.toml on
// B's second consume (after the first consume grafts A's genesis signing key).
func TestTeamScope_GenesisPinE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short: two device boots + two git round-trips")
	}
	ctx := context.Background()
	remote := newBareTeamRemote(t)

	deviceA := newDevice(t, "A")
	deviceB := newDevice(t, "B")
	aPrincipal := deviceA.principalUID(t)
	bJoinKey := joinKeyBlob(t, deviceB)

	// A creates team (stages TEAM.json) and grants B.
	createResp := deviceA.invoke(t, "team_create", map[string]any{"remote": remote})
	assertJSONField(t, "team_create", createResp, "genesisPrincipalUid", aPrincipal)
	deviceA.invoke(t, "team_grant", map[string]any{"joinKey": bJoinKey})

	// A seals+pushes — TEAM.json goes to the remote repo via reconcileTeamJSON.
	svA := deviceA.newSupervisor(t, remote)
	svA.sealOnce(ctx, kernel.ScopeTeam)
	svA.pushOnce(ctx, kernel.ScopeTeam)

	// B: first consume — bootstrap DEK + graft events (enrolls A's signing key).
	// maybeImportTeamJSONPin runs BEFORE the graft, so A's key is not enrolled
	// yet → pin import is skipped.
	configureJoinerTeamRemote(t, deviceB, remote)
	svB := deviceB.newConsumeSupervisor(t, remote)
	svB.consumeOnce(ctx, kernel.ScopeTeam)

	_, _, pinnedAfterFirst, _ := config.LoadTeamGenesisPin(deviceB.dataDir)
	if pinnedAfterFirst {
		// Implementation calls maybeImportTeamJSONPin AFTER the graft (ahead of
		// spec). Acceptable — we only require pin is set by the second consume.
		t.Logf("pin already set after first consume (implementation ahead of spec — ok)")
	}

	// B: second consume — maybeImportTeamJSONPin now finds A's enrolled key,
	// verifies the TEAM.json signature and writes the genesis pin.
	svB.consumeOnce(ctx, kernel.ScopeTeam)

	pinnedUID, pinnedKID, pinned, perr := config.LoadTeamGenesisPin(deviceB.dataDir)
	if perr != nil {
		t.Fatalf("LoadTeamGenesisPin: %v", perr)
	}
	if !pinned {
		t.Fatalf("genesis pin not set after second consume — maybeImportTeamJSONPin did not fire")
	}
	if pinnedUID != aPrincipal {
		t.Errorf("pinned principal = %q, want %q", pinnedUID, aPrincipal)
	}
	if pinnedKID == "" {
		t.Errorf("pinned sig_key_id is empty — TEAM.json signing key not captured in pin")
	}

	// Idempotency: third consume must not change the already-stored pin.
	svB.consumeOnce(ctx, kernel.ScopeTeam)
	uid2, kid2, pinned2, _ := config.LoadTeamGenesisPin(deviceB.dataDir)
	if !pinned2 || uid2 != pinnedUID || kid2 != pinnedKID {
		t.Errorf("pin changed on third consume: got (%q,%q,%v), want (%q,%q,true)",
			uid2, kid2, pinned2, pinnedUID, pinnedKID)
	}
}
