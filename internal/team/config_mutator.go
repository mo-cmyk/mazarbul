package team

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	gitxport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
)

// ConfigFileMutator implements ConfigMutator against the user config.toml in a
// data dir. It flips the team scope into encrypted envelope mode (and pins the
// remote URL when one is supplied) by a load-modify-write of the [sync.remotes.team]
// table. Idempotent: re-enabling an already-enabled team leaves the config
// equivalent.
//
// When idStore is non-nil (wired by the daemon, which holds the signing
// keychain), PersistGenesisPin also stages the signed TEAM.json trust anchor
// file at <dataDir>/sync/team/TEAM.json so the next push copies it to the
// repo root. Tests and non-genesis devices pass nil idStore; the staging step
// is then skipped silently.
type ConfigFileMutator struct {
	dataDir string
	idStore genesisKeySigner
}

// genesisKeySigner is the minimal interface ConfigFileMutator needs from
// the identity store to sign the genesis TEAM.json anchor. *identity.Store
// satisfies it.
type genesisKeySigner interface {
	SigningKey(ctx context.Context) (ed25519.PrivateKey, error)
}

// NewConfigFileMutator returns a mutator that reads and writes the user config
// under dataDir. The identity store is nil, so TEAM.json staging is skipped
// (used in tests and for non-genesis devices that call ConfigMutator methods
// but do not hold a signing key).
func NewConfigFileMutator(dataDir string) *ConfigFileMutator {
	return &ConfigFileMutator{dataDir: dataDir}
}

// NewConfigFileMutatorWithIdentity returns a mutator that also stages the
// signed TEAM.json on PersistGenesisPin, using idStore to obtain the genesis
// signing private key. Pass this to the team.Service in the daemon wiring on
// the genesis device; non-genesis devices use NewConfigFileMutator.
func NewConfigFileMutatorWithIdentity(dataDir string, idStore genesisKeySigner) *ConfigFileMutator {
	return &ConfigFileMutator{dataDir: dataDir, idStore: idStore}
}

// EnableTeamEnvelope sets the team scope to encrypted + KeyModeEnvelope and,
// when remoteURL is non-empty, pins it as the team remote. Other team-remote
// fields (auth method) are preserved.
func (m *ConfigFileMutator) EnableTeamEnvelope(_ context.Context, remoteURL string) error {
	cfg, err := config.LoadUserConfig(m.dataDir)
	if err != nil {
		return fmt.Errorf("team: load user config: %w", err)
	}
	if cfg.Sync.Remotes == nil {
		cfg.Sync.Remotes = map[string]config.ScopeRemoteTable{}
	}
	scope := string(kernel.ScopeTeam)
	remote := cfg.Sync.Remotes[scope]
	encrypted := true
	remote.Encrypted = &encrypted
	remote.KeyMode = config.KeyModeEnvelope
	if remoteURL != "" {
		remote.URL = remoteURL
	}
	cfg.Sync.Remotes[scope] = remote
	if err := config.WriteUserConfig(m.dataDir, cfg); err != nil {
		return fmt.Errorf("team: persist team envelope config: %w", err)
	}
	return nil
}

// PersistGenesisPin writes the genesis trust anchor to state.toml.
// When the mutator was constructed with an identity store (daemon/genesis-
// device wiring), it also produces the signed TEAM.json staging file at
// <dataDir>/sync/team/TEAM.json. TEAM.json staging errors are returned to
// the caller but do NOT prevent the state.toml write — the pin is always
// stored first and the staging step is best-effort.
func (m *ConfigFileMutator) PersistGenesisPin(ctx context.Context, principalUID, sigKeyID string) error {
	if err := config.SetTeamGenesisPin(m.dataDir, principalUID, sigKeyID); err != nil {
		return fmt.Errorf("team: persist genesis pin: %w", err)
	}
	if m.idStore == nil {
		// No signing key available (test or non-genesis device); skip staging.
		return nil
	}
	privKey, err := m.idStore.SigningKey(ctx)
	if err != nil {
		// Non-fatal: state.toml already written; log at the call site.
		return fmt.Errorf("team: load genesis signing key for TEAM.json: %w", err)
	}
	if err := gitxport.StageTeamJSON(m.dataDir, principalUID, sigKeyID, privKey); err != nil {
		return fmt.Errorf("team: stage TEAM.json: %w", err)
	}
	return nil
}

// LoadGenesisPin reads the genesis trust anchor from state.toml. Returns
// ok=false when no pin is stored (unpinned team or fresh state file).
// A missing state file is not an error.
func (m *ConfigFileMutator) LoadGenesisPin(_ context.Context) (principalUID, sigKeyID string, ok bool, err error) {
	uid, kid, found, lerr := config.LoadTeamGenesisPin(m.dataDir)
	if lerr != nil {
		return "", "", false, fmt.Errorf("team: load genesis pin: %w", lerr)
	}
	return uid, kid, found, nil
}
