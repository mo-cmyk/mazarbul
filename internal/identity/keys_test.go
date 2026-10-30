package identity

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"

	"github.com/tyler-smith/go-bip39"

	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// testMnemonic returns the canonical BIP-39 24-word vector for all-zero
// 256-bit entropy. Derived at runtime (not hardcoded) so the checksum word
// is always correct; deterministic, so derived keys are stable across runs.
func testMnemonic(t *testing.T) string {
	t.Helper()
	m, err := bip39.NewMnemonic(make([]byte, 32))
	if err != nil {
		t.Fatalf("build test mnemonic: %v", err)
	}
	return m
}

// mapProvider is an in-memory secret.Provider for tests.
type mapProvider struct {
	mu sync.Mutex
	m  map[string]string
}

func newMapProvider() *mapProvider { return &mapProvider{m: map[string]string{}} }

func (p *mapProvider) Get(_ context.Context, name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.m[name]; ok {
		return v, nil
	}
	return "", secret.ErrNotFound
}

func (p *mapProvider) Set(_ context.Context, name, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[name] = value
	return nil
}

func (p *mapProvider) Delete(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, name)
	return nil
}

func providerWithWatchword(t *testing.T) *mapProvider {
	t.Helper()
	p := newMapProvider()
	if err := p.Set(context.Background(), watchword.KeychainKey, testMnemonic(t)); err != nil {
		t.Fatalf("seed watchword: %v", err)
	}
	return p
}

func TestSigningKeyIsDeterministicFromWatchword(t *testing.T) {
	ctx := context.Background()

	k1, err := New(providerWithWatchword(t)).SigningKey(ctx)
	if err != nil {
		t.Fatalf("derive key 1: %v", err)
	}
	// A fresh provider (empty keychain cache) with the same mnemonic must
	// re-derive an identical key: this is the keychain-loss recovery story.
	k2, err := New(providerWithWatchword(t)).SigningKey(ctx)
	if err != nil {
		t.Fatalf("derive key 2: %v", err)
	}
	if !k1.Equal(k2) {
		t.Fatal("signing keys derived from the same mnemonic differ")
	}
}

func TestSigningKeyCachesSeedAfterFirstDerive(t *testing.T) {
	ctx := context.Background()
	p := providerWithWatchword(t)
	s := New(p)

	if _, err := s.SigningKey(ctx); err != nil {
		t.Fatalf("first derive: %v", err)
	}
	if _, err := p.Get(ctx, KeychainEd25519Seed); err != nil {
		t.Fatalf("expected ed25519 seed cached after first derive, got: %v", err)
	}

	// Drop the watchword: a cached seed must still resolve the key.
	if err := p.Delete(ctx, watchword.KeychainKey); err != nil {
		t.Fatalf("delete watchword: %v", err)
	}
	if _, err := s.SigningKey(ctx); err != nil {
		t.Fatalf("derive from cached seed: %v", err)
	}
}

func TestSigningKeyWithoutWatchwordReturnsErrNoWatchword(t *testing.T) {
	_, err := New(newMapProvider()).SigningKey(context.Background())
	if !errors.Is(err, ErrNoWatchword) {
		t.Fatalf("want ErrNoWatchword, got %v", err)
	}
}

func TestInitializeReturnsStableIdentity(t *testing.T) {
	ctx := context.Background()
	p := providerWithWatchword(t)
	s := New(p)

	id1, err := s.Initialize(ctx)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if id1.DeviceID == "" || id1.SigKeyID == "" {
		t.Fatalf("incomplete identity: %+v", id1)
	}
	if len(id1.SignPublic) != ed25519.PublicKeySize {
		t.Fatalf("sign public key wrong size: %d", len(id1.SignPublic))
	}

	// A second Initialize on the same store is idempotent: same device id
	// (persisted) and same sigKeyID (derived).
	id2, err := s.Initialize(ctx)
	if err != nil {
		t.Fatalf("re-initialize: %v", err)
	}
	if id1.DeviceID != id2.DeviceID {
		t.Fatalf("device id changed across Initialize: %q vs %q", id1.DeviceID, id2.DeviceID)
	}
	if id1.SigKeyID != id2.SigKeyID {
		t.Fatalf("sig key id changed: %q vs %q", id1.SigKeyID, id2.SigKeyID)
	}
}

func TestSigKeyIDFormat(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	kid := SigKeyID(pub)
	if len(kid) != len("kid:")+32 {
		t.Fatalf("unexpected sigKeyID length: %q", kid)
	}
	// Stable: same key -> same id.
	if SigKeyID(pub) != kid {
		t.Fatal("SigKeyID not deterministic")
	}
}

// ---------------------------------------------------------------------------
// B2 device subkey tests
// ---------------------------------------------------------------------------

// TestMintDeviceSubkey verifies that MintDeviceSubkey generates a random
// ed25519 subkey, stores it under KeychainDeviceSigSeed, and that HasDeviceSubkey
// returns true afterwards. Re-calling MintDeviceSubkey rotates to a fresh key.
func TestMintDeviceSubkey(t *testing.T) {
	ctx := context.Background()
	p := providerWithWatchword(t)
	s := New(p)

	// Before minting, HasDeviceSubkey must report false.
	has, err := s.HasDeviceSubkey(ctx)
	if err != nil {
		t.Fatalf("HasDeviceSubkey before mint: %v", err)
	}
	if has {
		t.Fatal("HasDeviceSubkey: want false before first mint, got true")
	}

	priv, err := s.MintDeviceSubkey(ctx)
	if err != nil {
		t.Fatalf("MintDeviceSubkey: %v", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		t.Fatalf("MintDeviceSubkey returned key of size %d, want %d", len(priv), ed25519.PrivateKeySize)
	}

	// HasDeviceSubkey must now return true.
	has, err = s.HasDeviceSubkey(ctx)
	if err != nil {
		t.Fatalf("HasDeviceSubkey after mint: %v", err)
	}
	if !has {
		t.Fatal("HasDeviceSubkey: want true after mint, got false")
	}

	// KeychainDeviceSigSeed must be stored in the provider.
	if _, err := p.Get(ctx, KeychainDeviceSigSeed); err != nil {
		t.Fatalf("expected %q to be stored in keychain after mint: %v", KeychainDeviceSigSeed, err)
	}

	// A second mint rotates the key: the new key must differ from the first.
	priv2, err := s.MintDeviceSubkey(ctx)
	if err != nil {
		t.Fatalf("second MintDeviceSubkey: %v", err)
	}
	if priv.Equal(priv2) {
		// Probabilistically impossible (128-bit random seed), but guard anyway.
		t.Fatal("two consecutive MintDeviceSubkey calls returned identical keys")
	}
}

// TestAdoptDeviceID verifies the Mellon-pairing device-id handoff: a device
// with no id adopts the proposed one; an already-identified device keeps its
// existing id (pairing never re-identifies an established device).
func TestAdoptDeviceID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := New(newMapProvider())

	got, err := s.AdoptDeviceID(ctx, "01BUNDLE000000000000000001")
	if err != nil {
		t.Fatalf("AdoptDeviceID on a fresh device: %v", err)
	}
	if got != "01BUNDLE000000000000000001" {
		t.Errorf("adopted id = %q, want the proposed bundle id", got)
	}

	// A second adopt with a different proposal keeps the established id.
	got2, err := s.AdoptDeviceID(ctx, "01OTHER0000000000000000001")
	if err != nil {
		t.Fatalf("AdoptDeviceID on an established device: %v", err)
	}
	if got2 != "01BUNDLE000000000000000001" {
		t.Errorf("established id replaced: got %q, want the original", got2)
	}

	// DeviceID must return the adopted id, not mint a fresh one.
	id, err := s.DeviceID(ctx)
	if err != nil {
		t.Fatalf("DeviceID: %v", err)
	}
	if id != "01BUNDLE000000000000000001" {
		t.Errorf("DeviceID = %q, want the adopted id", id)
	}
}

// TestDeviceSigningKey_FallsBackToWatchwordDerived verifies that when no
// device subkey has been minted, DeviceSigningKey returns the Watchword-derived
// key (the device-0 / legacy path).
func TestDeviceSigningKey_FallsBackToWatchwordDerived(t *testing.T) {
	ctx := context.Background()
	s := New(providerWithWatchword(t))

	got, err := s.DeviceSigningKey(ctx)
	if err != nil {
		t.Fatalf("DeviceSigningKey (no subkey): %v", err)
	}
	want, err := s.SigningKey(ctx)
	if err != nil {
		t.Fatalf("SigningKey (Watchword-derived): %v", err)
	}
	if !got.Equal(want) {
		t.Fatal("DeviceSigningKey did not fall back to the Watchword-derived key when no subkey is present")
	}
}

// TestDeviceSigningKey_UsesSubkeyWhenPresent verifies that after MintDeviceSubkey
// is called, DeviceSigningKey returns the subkey, NOT the Watchword-derived key.
func TestDeviceSigningKey_UsesSubkeyWhenPresent(t *testing.T) {
	ctx := context.Background()
	s := New(providerWithWatchword(t))

	subkey, err := s.MintDeviceSubkey(ctx)
	if err != nil {
		t.Fatalf("MintDeviceSubkey: %v", err)
	}

	got, err := s.DeviceSigningKey(ctx)
	if err != nil {
		t.Fatalf("DeviceSigningKey (with subkey): %v", err)
	}
	if !got.Equal(subkey) {
		t.Fatal("DeviceSigningKey did not return the minted subkey")
	}

	// It must NOT equal the Watchword-derived key.
	wdKey, err := s.SigningKey(ctx)
	if err != nil {
		t.Fatalf("SigningKey (Watchword-derived): %v", err)
	}
	if got.Equal(wdKey) {
		t.Fatal("DeviceSigningKey returned the Watchword-derived key even though a subkey is present")
	}
}

// TestSigKeyID_UniqueForTwoDevicesSharingWatchword verifies that two devices
// that share the same Watchword-derived principal key but each mint their own
// random subkey produce distinct SigKeyIDs. This is the core B2 guarantee:
// sigKeyID uniquely identifies a device even when two devices share a principal.
func TestSigKeyID_UniqueForTwoDevicesSharingWatchword(t *testing.T) {
	ctx := context.Background()

	// Two stores with the SAME Watchword (same principal) but separate keychains
	// simulate two devices enrolled under one principal.
	s1 := New(providerWithWatchword(t))
	s2 := New(providerWithWatchword(t))

	// Without subkeys: both resolve to the same Watchword-derived key → same SigKeyID.
	pub1, err := s1.DeviceSigningPublicKey(ctx)
	if err != nil {
		t.Fatalf("device 1 DeviceSigningPublicKey (no subkey): %v", err)
	}
	pub2, err := s2.DeviceSigningPublicKey(ctx)
	if err != nil {
		t.Fatalf("device 2 DeviceSigningPublicKey (no subkey): %v", err)
	}
	if SigKeyID(pub1) != SigKeyID(pub2) {
		t.Fatal("pre-subkey: expected same SigKeyID for two devices sharing a Watchword")
	}

	// After minting subkeys, the two devices must have distinct SigKeyIDs.
	if _, err := s1.MintDeviceSubkey(ctx); err != nil {
		t.Fatalf("device 1 MintDeviceSubkey: %v", err)
	}
	if _, err := s2.MintDeviceSubkey(ctx); err != nil {
		t.Fatalf("device 2 MintDeviceSubkey: %v", err)
	}
	pub1, err = s1.DeviceSigningPublicKey(ctx)
	if err != nil {
		t.Fatalf("device 1 DeviceSigningPublicKey (with subkey): %v", err)
	}
	pub2, err = s2.DeviceSigningPublicKey(ctx)
	if err != nil {
		t.Fatalf("device 2 DeviceSigningPublicKey (with subkey): %v", err)
	}
	if SigKeyID(pub1) == SigKeyID(pub2) {
		t.Fatal("post-subkey: two devices sharing a Watchword have the same SigKeyID; want distinct IDs")
	}
}
