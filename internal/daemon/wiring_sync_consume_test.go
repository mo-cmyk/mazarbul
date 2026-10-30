package daemon

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/sync/recipients"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// memSecrets is an in-memory writable secret.Provider for the consume tests.
type memSecrets struct {
	mu     sync.Mutex
	m      map[string]string
	getErr map[string]error // inject a transient (non-ErrNotFound) Get fault per key
}

func newMemSecrets() *memSecrets { return &memSecrets{m: map[string]string{}} }

func (s *memSecrets) Get(_ context.Context, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.getErr[name]; err != nil {
		return "", err
	}
	v, ok := s.m[name]
	if !ok {
		return "", secret.ErrNotFound
	}
	return v, nil
}

func (s *memSecrets) Set(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = value
	return nil
}

func (s *memSecrets) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
	return nil
}

// seedWrapKey installs a deterministic x25519 wrap key into secrets (so
// identity.WrappingKey resolves without a Watchword) and returns its public
// bytes for sealing DEKs to this device.
func seedWrapKey(t *testing.T, secrets *memSecrets) []byte {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate wrap key: %v", err)
	}
	if serr := secrets.Set(context.Background(), identity.KeychainX25519Seed,
		base64.StdEncoding.EncodeToString(priv.Bytes())); serr != nil {
		t.Fatalf("seed wrap key: %v", serr)
	}
	return priv.PublicKey().Bytes()
}

func TestBootstrapTeamDEKs_UnwrapsOwnRowsIntoKeychain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	secrets := newMemSecrets()
	wrapPub := seedWrapKey(t, secrets)

	const self = "01PRINCIPAL0000000000SELF01"
	if err := config.BindSelfPrincipal(dataDir, self, "01DEVICE0000000000000000001", "kid:test"); err != nil {
		t.Fatalf("bind self: %v", err)
	}

	dek, err := watchword.GenerateDEK()
	if err != nil {
		t.Fatalf("generate dek: %v", err)
	}
	wrapped, err := watchword.WrapDEK(dek, wrapPub)
	if err != nil {
		t.Fatalf("wrap dek: %v", err)
	}
	rows := []recipients.Row{
		{PrincipalUID: self, WrapPublicKey: base64.StdEncoding.EncodeToString(wrapPub), DEKEpoch: 0, WrappedDEK: base64.StdEncoding.EncodeToString(wrapped)},
		{PrincipalUID: "01PRINCIPAL0000000000OTHER1", WrapPublicKey: "x", DEKEpoch: 0, WrappedDEK: "y"}, // not us → skipped
	}

	n, err := bootstrapTeamDEKs(ctx, rows, dataDir, secrets, zap.NewNop())
	if err != nil {
		t.Fatalf("bootstrapTeamDEKs: %v", err)
	}
	if n != 1 {
		t.Fatalf("unwrapped %d, want 1 (only our own row)", n)
	}
	got, err := secrets.Get(ctx, watchword.KeychainTeamDEKPrefix+"0")
	if err != nil {
		t.Fatalf("keychain DEK missing after bootstrap: %v", err)
	}
	if got != base64.StdEncoding.EncodeToString(dek) {
		t.Errorf("bootstrapped DEK mismatch: the unwrapped key must equal the original DEK")
	}

	// Idempotent: a second pass unwraps nothing new.
	n2, err := bootstrapTeamDEKs(ctx, rows, dataDir, secrets, zap.NewNop())
	if err != nil {
		t.Fatalf("second bootstrapTeamDEKs: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second bootstrap unwrapped %d, want 0 (epoch already present)", n2)
	}
}

func TestBootstrapTeamDEKs_NoSelfBindingIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	secrets := newMemSecrets()
	seedWrapKey(t, secrets)
	rows := []recipients.Row{{PrincipalUID: "P", DEKEpoch: 0, WrappedDEK: "x"}}
	// No BindSelfPrincipal: pre-genesis device. Must be a quiet no-op, not an error.
	n, err := bootstrapTeamDEKs(ctx, rows, t.TempDir(), secrets, zap.NewNop())
	if err != nil {
		t.Fatalf("bootstrapTeamDEKs: %v", err)
	}
	if n != 0 {
		t.Errorf("unwrapped %d, want 0 (no self binding yet)", n)
	}
}

func TestResolveTeamDEKRing_ReadsEpochs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	secrets := newMemSecrets()
	dek0, _ := watchword.GenerateDEK()
	dek1, _ := watchword.GenerateDEK()
	must(t, secrets.Set(ctx, watchword.KeychainTeamDEKPrefix+"0", base64.StdEncoding.EncodeToString(dek0)))
	must(t, secrets.Set(ctx, watchword.KeychainTeamDEKPrefix+"1", base64.StdEncoding.EncodeToString(dek1)))

	ring := resolveTeamDEKRing(ctx, secrets, zap.NewNop())
	if len(ring) != 2 {
		t.Fatalf("ring size %d, want 2 (epochs 0+1)", len(ring))
	}
	if _, ok := ring[0]; !ok {
		t.Error("epoch 0 missing from ring")
	}
	if _, ok := ring[1]; !ok {
		t.Error("epoch 1 missing from ring")
	}
}

// TestResolveTeamDEKRing_ToleratesGaps pins the gap-tolerance fix: a single
// missing middle epoch must NOT silently drop every higher epoch (which would
// leave already-bootstrapped chunks permanently undecryptable). Epoch 3 must
// still resolve despite the gap at epoch 2.
func TestResolveTeamDEKRing_ToleratesGaps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	secrets := newMemSecrets()
	dek, _ := watchword.GenerateDEK()
	enc := base64.StdEncoding.EncodeToString(dek)
	must(t, secrets.Set(ctx, watchword.KeychainTeamDEKPrefix+"0", enc))
	must(t, secrets.Set(ctx, watchword.KeychainTeamDEKPrefix+"1", enc))
	// Gap at epoch 2; epoch 3 present.
	must(t, secrets.Set(ctx, watchword.KeychainTeamDEKPrefix+"3", enc))

	ring := resolveTeamDEKRing(ctx, secrets, zap.NewNop())
	for _, want := range []int{0, 1, 3} {
		if _, ok := ring[want]; !ok {
			t.Errorf("epoch %d missing from ring (gap-tolerant scan must keep higher epochs)", want)
		}
	}
	if _, ok := ring[2]; ok {
		t.Error("epoch 2 was never stored; it must not appear in the ring")
	}
}

// TestResolveTeamDEKRing_NoGenesisShortCircuits pins the non-member fast path:
// absent epoch 0 (genesis) means this device holds no team DEK, so the scan
// returns an empty ring without probing higher epochs.
func TestResolveTeamDEKRing_NoGenesisShortCircuits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	secrets := newMemSecrets()
	dek, _ := watchword.GenerateDEK()
	// Epoch 1 present but epoch 0 absent — a shape a real member never has.
	must(t, secrets.Set(ctx, watchword.KeychainTeamDEKPrefix+"1", base64.StdEncoding.EncodeToString(dek)))

	ring := resolveTeamDEKRing(ctx, secrets, zap.NewNop())
	if len(ring) != 0 {
		t.Fatalf("ring size %d, want 0 (absent genesis epoch 0 → no team DEK)", len(ring))
	}
}

// TestResolveTeamDEKRing_TransientFaultIsGapNotStop verifies a transient
// keychain read fault (not a clean ErrNotFound miss) at one epoch is treated as
// a gap — the scan continues — rather than silently terminating the ring and
// dropping every higher epoch for the process lifetime.
func TestResolveTeamDEKRing_TransientFaultIsGapNotStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	secrets := newMemSecrets()
	dek0, _ := watchword.GenerateDEK()
	dek2, _ := watchword.GenerateDEK()
	must(t, secrets.Set(ctx, watchword.KeychainTeamDEKPrefix+"0", base64.StdEncoding.EncodeToString(dek0)))
	must(t, secrets.Set(ctx, watchword.KeychainTeamDEKPrefix+"2", base64.StdEncoding.EncodeToString(dek2)))
	// Epoch 1 faults transiently (a degrading key store), NOT a clean absence.
	secrets.getErr = map[string]error{watchword.KeychainTeamDEKPrefix + "1": errors.New("keychain temporarily locked")}

	ring := resolveTeamDEKRing(ctx, secrets, zap.NewNop())
	if _, ok := ring[0]; !ok {
		t.Error("epoch 0 must resolve")
	}
	if _, ok := ring[1]; ok {
		t.Error("epoch 1 faulted; it must not be in the ring")
	}
	if _, ok := ring[2]; !ok {
		t.Error("epoch 2 must still resolve past a transient fault at epoch 1 (gap-tolerant, not a hard stop)")
	}
}

func TestEnvelopeScopes_SelectsTeamEnvelopeRemotes(t *testing.T) {
	t.Parallel()
	enc := true
	syncCfg := config.SyncTable{Remotes: map[string]config.ScopeRemoteTable{
		"team":     {URL: "git+file:///tmp/x.git", Encrypted: &enc, KeyMode: config.KeyModeEnvelope},
		"personal": {URL: "git+file:///tmp/p.git"}, // plaintext → excluded
		"project":  {URL: ""},                      // no remote → excluded
	}}
	got := envelopeScopes(syncCfg)
	if len(got) != 1 || got[0] != kernel.ScopeTeam {
		t.Fatalf("envelopeScopes = %v, want [team]", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}
