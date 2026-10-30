package daemon

// wiring_sync_genesis_pin_test.go — verification gate on the consume loop's
// TEAM.json genesis-pin import. The pin fail-closes the whole team fold when
// wrong, so the import must refuse to persist any anchor it cannot verify
// against the projections: right signature by the fold genesis's own enrolled
// key, naming the fold's own genesis principal, and never overwriting an
// existing pin.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	gitxport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
)

// fakeGenesisKeys is a canned genesisAnchorKeySource.
type fakeGenesisKeys struct {
	uid string
	pub ed25519.PublicKey
	ok  bool
	err error
}

func (f fakeGenesisKeys) GenesisAnchorKey(context.Context, string) (string, ed25519.PublicKey, bool, error) {
	return f.uid, f.pub, f.ok, f.err
}

// writeRepoTeamJSON drops a TEAM.json into the team sync repo mirror layout
// under dataDir, exactly where maybeImportTeamJSONPin reads it.
func writeRepoTeamJSON(t *testing.T, dataDir string, tj gitxport.TeamJSON) {
	t.Helper()
	repoDir := filepath.Join(dataDir, "sync", "team", "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo dir: %v", err)
	}
	raw, err := json.Marshal(tj)
	if err != nil {
		t.Fatalf("marshal TEAM.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, gitxport.TeamJSONFilename), raw, 0o600); err != nil {
		t.Fatalf("write TEAM.json: %v", err)
	}
}

// signedTeamJSON builds a TEAM.json whose signature is a real ed25519
// signature by priv over the canonical genesis-pin bytes.
func signedTeamJSON(t *testing.T, uid, kid string, priv ed25519.PrivateKey) gitxport.TeamJSON {
	t.Helper()
	sig := ed25519.Sign(priv, ontology.GenesisPinBytes(uid, kid))
	return gitxport.TeamJSON{
		PrincipalUID: uid,
		SigKeyID:     kid,
		Signature:    base64.StdEncoding.EncodeToString(sig),
	}
}

// pinSupervisor builds the minimal supervisor the import path needs.
func pinSupervisor(dataDir string, keys genesisAnchorKeySource) *syncSupervisor {
	return &syncSupervisor{
		cfg:    &syncSupervisorConfig{DataDir: dataDir, GenesisKeys: keys},
		logger: zap.NewNop(),
	}
}

func TestMaybeImportTeamJSONPin(t *testing.T) {
	t.Parallel()
	genesisUID := "01GENESISPRINCIPAL0000001"
	kid := "kid:genesis-root"
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	t.Run("verified anchor pins", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeRepoTeamJSON(t, dir, signedTeamJSON(t, genesisUID, kid, priv))
		s := pinSupervisor(dir, fakeGenesisKeys{uid: genesisUID, pub: pub, ok: true})
		s.maybeImportTeamJSONPin(context.Background())
		gotUID, gotKid, pinned, lerr := config.LoadTeamGenesisPin(dir)
		if lerr != nil {
			t.Fatalf("load pin: %v", lerr)
		}
		if !pinned || gotUID != genesisUID || gotKid != kid {
			t.Errorf("pin = (%q,%q,%v), want (%q,%q,true)", gotUID, gotKid, pinned, genesisUID, kid)
		}
	})

	t.Run("invalid signature is not pinned", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tj := signedTeamJSON(t, genesisUID, kid, priv)
		tj.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		writeRepoTeamJSON(t, dir, tj)
		s := pinSupervisor(dir, fakeGenesisKeys{uid: genesisUID, pub: pub, ok: true})
		s.maybeImportTeamJSONPin(context.Background())
		if _, _, pinned, _ := config.LoadTeamGenesisPin(dir); pinned {
			t.Error("pin persisted despite invalid signature")
		}
	})

	t.Run("fold genesis mismatch is not pinned", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeRepoTeamJSON(t, dir, signedTeamJSON(t, genesisUID, kid, priv))
		s := pinSupervisor(dir, fakeGenesisKeys{uid: "01DIFFERENTPRINCIPAL00001", pub: pub, ok: true})
		s.maybeImportTeamJSONPin(context.Background())
		if _, _, pinned, _ := config.LoadTeamGenesisPin(dir); pinned {
			t.Error("pin persisted despite fold genesis mismatch")
		}
	})

	t.Run("unresolvable anchor key is not pinned", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeRepoTeamJSON(t, dir, signedTeamJSON(t, genesisUID, kid, priv))
		s := pinSupervisor(dir, fakeGenesisKeys{ok: false})
		s.maybeImportTeamJSONPin(context.Background())
		if _, _, pinned, _ := config.LoadTeamGenesisPin(dir); pinned {
			t.Error("pin persisted despite unresolvable anchor key")
		}
	})

	t.Run("nil key source disables import", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeRepoTeamJSON(t, dir, signedTeamJSON(t, genesisUID, kid, priv))
		s := pinSupervisor(dir, nil)
		s.maybeImportTeamJSONPin(context.Background())
		if _, _, pinned, _ := config.LoadTeamGenesisPin(dir); pinned {
			t.Error("pin persisted with nil GenesisKeys source")
		}
	})

	t.Run("existing pin is never overwritten", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := config.SetTeamGenesisPin(dir, "01EXISTINGPIN000000000001", "kid:existing"); err != nil {
			t.Fatalf("seed existing pin: %v", err)
		}
		writeRepoTeamJSON(t, dir, signedTeamJSON(t, genesisUID, kid, priv))
		s := pinSupervisor(dir, fakeGenesisKeys{uid: genesisUID, pub: pub, ok: true})
		s.maybeImportTeamJSONPin(context.Background())
		gotUID, _, pinned, _ := config.LoadTeamGenesisPin(dir)
		if !pinned || gotUID != "01EXISTINGPIN000000000001" {
			t.Errorf("existing pin overwritten: got (%q, pinned=%v)", gotUID, pinned)
		}
	})
}
