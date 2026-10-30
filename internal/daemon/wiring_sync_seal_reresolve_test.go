package daemon

// wiring_sync_seal_reresolve_test.go pins FIX C — the daemon seal path must
// re-resolve the scope key + DEK epoch on EACH pass rather than reading a boot
// snapshot. Covers F5 (forward secrecy: seal under the rotated epoch after a
// revoke) and F4 (a joiner whose team DEK only bootstraps after boot still gets
// a seal loop and seals once the DEK lands — no restart).

import (
	"bytes"
	"context"
	"encoding/base64"
	"slices"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// TestTeamScope_SealReResolvesDEKEpoch is the F5 regression (forward secrecy).
// The supervisor snapshotted the team DEK + epoch at boot and sealed every
// subsequent chunk under that frozen epoch. After a revoke rotates the DEK to a
// new epoch, a running daemon MUST seal new team writes under the NEW epoch —
// otherwise a revoked member (who still holds the old DEK) could decrypt content
// authored after their removal. This drives the rotation directly on the
// keychain (the effect a revoke re-key has) and asserts the next seal pass picks
// it up with no restart.
func TestTeamScope_SealReResolvesDEKEpoch(t *testing.T) {
	// NOT parallel: go-ladybug OpenDatabase is not concurrency-safe under
	// parallel opens (matches the other team E2E tests in this package).
	ctx := context.Background()
	remote := newBareTeamRemote(t)

	deviceA := newDevice(t, "A")
	// A creates the team: mints the epoch-0 DEK, envelope mode, pins the remote.
	deviceA.invoke(t, "team_create", map[string]any{"remote": remote})

	sv := deviceA.newSupervisor(t, remote)

	// First team write → sealed under the boot epoch (0).
	deviceA.invoke(t, ontologyFactToolName, map[string]any{
		"subject":  "self",
		"stateKey": "preference.editor",
		"value":    "vim",
		"scope":    string(kernel.ScopeTeam),
	})
	sv.sealOnce(ctx, kernel.ScopeTeam)
	if got := maxTeamChunkEpoch(ctx, t, sv); got != 0 {
		t.Fatalf("pre-rotation newest team chunk epoch = %d, want 0", got)
	}

	// Simulate a revoke re-key: a fresh epoch-1 DEK lands in the keychain — the
	// exact effect `maz team revoke` has (mint the next-epoch DEK, rewrap to the
	// remaining members). A running daemon must seal subsequent writes under it.
	newDEK := bytes.Repeat([]byte{0x7}, watchword.DEKSize)
	if err := deviceA.secrets.Set(ctx, watchword.KeychainTeamDEKPrefix+"1",
		base64.StdEncoding.EncodeToString(newDEK)); err != nil {
		t.Fatalf("rotate DEK to epoch 1: %v", err)
	}

	// Second team write AFTER the rotation — must seal under epoch 1, not 0.
	deviceA.invoke(t, ontologyFactToolName, map[string]any{
		"subject":  "self",
		"stateKey": "preference.shell",
		"value":    "zsh",
		"scope":    string(kernel.ScopeTeam),
	})
	sv.sealOnce(ctx, kernel.ScopeTeam)

	if got := maxTeamChunkEpoch(ctx, t, sv); got != 1 {
		t.Fatalf("post-rotation newest team chunk epoch = %d, want 1 — the seal path ignored the DEK rotation (forward-secrecy break)", got)
	}
}

// TestSealScopes_IncludesConfiguredTeamWithoutBootDEK is the F4 regression. A
// fresh joiner boots with the team remote configured but holds no DEK yet, so
// the team scope is absent from the boot scopeKeys snapshot. The seal-loop set
// must still include it (launched from the configured scopes), and the per-pass
// resolver must report not-ready until the consume loop bootstraps the DEK — at
// which point the very next seal pass seals, no restart.
func TestSealScopes_IncludesConfiguredTeamWithoutBootDEK(t *testing.T) {
	t.Parallel()
	sv := &syncSupervisor{
		cfg: &syncSupervisorConfig{
			Sync:        envelopeTeamSync("/tmp/maz-fake-f4.git"),
			SecretChain: stubSecretProvider{}, // no DEK at boot
			Logger:      zap.NewNop(),
		},
		scopeKeys: map[kernel.Scope][]byte{}, // team absent — DEK not yet bootstrapped
		logger:    zap.NewNop(),
	}

	if scopes := sv.sealScopes(); !slices.Contains(scopes, kernel.ScopeTeam) {
		t.Fatalf("sealScopes() = %v, want it to include team (configured remote) even without a boot DEK", scopes)
	}

	// Not ready while the DEK is missing → the seal pass skips.
	if _, _, ok := sv.resolveSealKey(context.Background(), kernel.ScopeTeam); ok {
		t.Fatalf("resolveSealKey returned ready with no DEK — the pass must skip until bootstrap")
	}

	// Once a DEK bootstraps (simulating the consume loop landing epoch 0), the
	// next pass resolves it — the joiner starts sealing with no restart.
	sv.cfg.SecretChain = stubSecretProvider{values: map[string]string{
		watchword.KeychainTeamDEKPrefix + "0": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x3}, watchword.DEKSize)),
	}}
	key, epoch, ok := sv.resolveSealKey(context.Background(), kernel.ScopeTeam)
	if !ok || epoch != 0 || len(key) != watchword.DEKSize {
		t.Fatalf("resolveSealKey after bootstrap = (len=%d, epoch=%d, ok=%v), want (32, 0, true)", len(key), epoch, ok)
	}
}

// maxTeamChunkEpoch returns the highest DEKEpoch across all sealed team chunks.
func maxTeamChunkEpoch(ctx context.Context, t *testing.T, sv *syncSupervisor) int {
	t.Helper()
	refs, err := sv.store.List(ctx, kernel.ScopeTeam)
	if err != nil {
		t.Fatalf("list team chunks: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("no team chunks sealed")
	}
	best := 0
	for i := range refs {
		if refs[i].DEKEpoch > best {
			best = refs[i].DEKEpoch
		}
	}
	return best
}
