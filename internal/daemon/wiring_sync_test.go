package daemon

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// stubSecretProvider returns the same value for every name it knows
// about, mimicking the env+keychain chain at the resolution boundary
// without actually touching the OS keychain (which would hang CI).
type stubSecretProvider struct {
	values map[string]string
}

func (s stubSecretProvider) Get(_ context.Context, name string) (string, error) {
	if v, ok := s.values[name]; ok {
		return v, nil
	}
	return "", secret.ErrNotFound
}

func (stubSecretProvider) Set(_ context.Context, _ string, _ string) error {
	return secret.ErrReadOnly
}

func (stubSecretProvider) Delete(_ context.Context, _ string) error {
	return secret.ErrReadOnly
}

// canonicalMnemonic builds the BIP-39 24-word "abandon … art" mnemonic
// programmatically. Matches the chunk + cli + mellon tests.
func canonicalMnemonic() string {
	parts := make([]string, 24)
	for i := range parts {
		parts[i] = "abandon"
	}
	parts[len(parts)-1] = "art"
	return parts[0] + " " + parts[1] + " " + parts[2] + " " + parts[3] +
		" " + parts[4] + " " + parts[5] + " " + parts[6] + " " + parts[7] +
		" " + parts[8] + " " + parts[9] + " " + parts[10] + " " + parts[11] +
		" " + parts[12] + " " + parts[13] + " " + parts[14] + " " + parts[15] +
		" " + parts[16] + " " + parts[17] + " " + parts[18] + " " + parts[19] +
		" " + parts[20] + " " + parts[21] + " " + parts[22] + " " + parts[23]
}

func TestNewSyncSupervisor_NoWatchwordNoRemoteIsNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sv, err := newSyncSupervisor(context.Background(), &syncSupervisorConfig{
		DataDir:     dir,
		DeviceID:    "test-device",
		SecretChain: stubSecretProvider{},
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("newSyncSupervisor: %v", err)
	}
	if sv != nil {
		t.Errorf("expected nil supervisor (no watchword + no remotes), got %T", sv)
	}
}

// truePtr is a tiny helper for the *bool fields in ScopeRemoteTable.
// Keeps the encrypted-opt-in cases readable.
func truePtr() *bool { v := true; return &v }

func TestNewSyncSupervisor_EncryptedScopeDerivesKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sv, err := newSyncSupervisor(context.Background(), &syncSupervisorConfig{
		DataDir:  dir,
		DeviceID: "test-device",
		Sync: config.SyncTable{
			Remotes: map[string]config.ScopeRemoteTable{
				"personal": {URL: "/tmp/maz-fake.git", Encrypted: truePtr()},
			},
		},
		SecretChain: stubSecretProvider{values: map[string]string{
			watchword.KeychainKey: canonicalMnemonic(),
		}},
		Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("newSyncSupervisor: %v", err)
	}
	if sv == nil {
		t.Fatalf("expected supervisor for encrypted+keyed scope, got nil")
	}
	key, ok := sv.scopeKeys["personal"]
	if !ok {
		t.Fatalf("expected personal scope in keys map")
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte derived key, got %d", len(key))
	}
}

// TestNewSyncSupervisor_PlaintextSetupBootsWithoutWatchword pins the
// plaintext-default flow: an operator with a remote but no Watchword has
// chosen plaintext (the default). The supervisor must boot and
// register a nil-keyed scope so the sealer emits JSONL chunks.
func TestNewSyncSupervisor_PlaintextSetupBootsWithoutWatchword(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sv, err := newSyncSupervisor(context.Background(), &syncSupervisorConfig{
		DataDir:  dir,
		DeviceID: "test-device",
		Sync: config.SyncTable{
			Remotes: map[string]config.ScopeRemoteTable{
				"personal": {URL: "/tmp/maz-fake.git"}, // Encrypted nil = plaintext default
			},
		},
		SecretChain: stubSecretProvider{},
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("newSyncSupervisor: %v", err)
	}
	if sv == nil {
		t.Fatalf("expected supervisor for plaintext setup, got nil")
	}
	key, ok := sv.scopeKeys["personal"]
	if !ok {
		t.Fatalf("expected personal scope in keys map")
	}
	if key != nil {
		t.Errorf("expected nil key (plaintext scope), got %d bytes", len(key))
	}
}

// TestNewSyncSupervisor_EncryptedScopeMissingWatchwordSkipsScope:
// the operator explicitly opted into encryption but has no
// Watchword. We can't seal that scope safely; the supervisor must
// warn + omit that scope from the key map. Other plaintext-opted
// scopes continue to seal.
func TestNewSyncSupervisor_EncryptedScopeMissingWatchwordSkipsScope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sv, err := newSyncSupervisor(context.Background(), &syncSupervisorConfig{
		DataDir:  dir,
		DeviceID: "test-device",
		Sync: config.SyncTable{
			Remotes: map[string]config.ScopeRemoteTable{
				"personal": {URL: "/tmp/maz-fake.git", Encrypted: truePtr()},
				"project":  {URL: "/tmp/maz-proj.git"}, // plaintext default
			},
		},
		SecretChain: stubSecretProvider{}, // no watchword
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("newSyncSupervisor: %v", err)
	}
	if sv == nil {
		t.Fatalf("expected supervisor (one plaintext scope still seal-able)")
	}
	if _, found := sv.scopeKeys["personal"]; found {
		t.Errorf("encrypted scope without watchword must be omitted")
	}
	if _, found := sv.scopeKeys["project"]; !found {
		t.Errorf("plaintext scope must still be sealed even when sibling scope misconfigured")
	}
}

func TestSyncSupervisor_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sv, err := newSyncSupervisor(context.Background(), &syncSupervisorConfig{
		DataDir:  dir,
		DeviceID: "test-device",
		Sync: config.SyncTable{
			Remotes: map[string]config.ScopeRemoteTable{
				"personal": {URL: "/tmp/maz-fake.git"},
			},
		},
		SecretChain: stubSecretProvider{},
		Logger:      zap.NewNop(),
	})
	if err != nil || sv == nil {
		t.Fatalf("newSyncSupervisor: sv=%v err=%v", sv, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sv.Start(ctx)
	if err := sv.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := sv.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestSyncSupervisor_NilCloseIsSafe(t *testing.T) {
	t.Parallel()
	var sv *syncSupervisor
	if err := sv.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
	sv.Start(context.Background()) // also safe
}
