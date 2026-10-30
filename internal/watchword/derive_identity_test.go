package watchword_test

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// TestDeriveSigningKey_DeterministicAndDistinct pins the contract the identity
// recovery story depends on: the signing key is a pure function of the
// Watchword root (so a keychain wipe + restore reproduces the same identity),
// and a different root yields a different key.
func TestDeriveSigningKey_DeterministicAndDistinct(t *testing.T) {
	t.Parallel()
	rootA, err := watchword.DeriveRoot(zeroEntropyMnemonic())
	if err != nil {
		t.Fatalf("DeriveRoot(A): %v", err)
	}
	rootB, err := watchword.DeriveRoot(allOnesMnemonic())
	if err != nil {
		t.Fatalf("DeriveRoot(B): %v", err)
	}

	keyA1, err := watchword.DeriveSigningKey(rootA)
	if err != nil {
		t.Fatalf("DeriveSigningKey(A): %v", err)
	}
	keyA2, err := watchword.DeriveSigningKey(rootA)
	if err != nil {
		t.Fatalf("DeriveSigningKey(A, again): %v", err)
	}
	keyB, err := watchword.DeriveSigningKey(rootB)
	if err != nil {
		t.Fatalf("DeriveSigningKey(B): %v", err)
	}

	if !bytes.Equal(keyA1, keyA2) {
		t.Error("DeriveSigningKey is not deterministic for a fixed root")
	}
	if bytes.Equal(keyA1, keyB) {
		t.Error("DeriveSigningKey collided across distinct roots")
	}
	if len(keyA1) != ed25519.PrivateKeySize {
		t.Errorf("signing key size = %d, want %d", len(keyA1), ed25519.PrivateKeySize)
	}

	// The derived key actually signs + verifies.
	msg := []byte("anchor statement")
	sig := ed25519.Sign(keyA1, msg)
	if !ed25519.Verify(keyA1.Public().(ed25519.PublicKey), msg, sig) {
		t.Error("derived signing key failed a sign/verify round trip")
	}
}

// TestDeriveWrapKey_DeterministicAndDistinctFromSigning confirms the x25519
// wrap key is deterministic and uses a separate HKDF info string from the
// signing key (no key reuse across primitives).
func TestDeriveWrapKey_DeterministicAndDistinctFromSigning(t *testing.T) {
	t.Parallel()
	root, err := watchword.DeriveRoot(zeroEntropyMnemonic())
	if err != nil {
		t.Fatalf("DeriveRoot: %v", err)
	}

	wrap1, err := watchword.DeriveWrapKey(root)
	if err != nil {
		t.Fatalf("DeriveWrapKey: %v", err)
	}
	wrap2, err := watchword.DeriveWrapKey(root)
	if err != nil {
		t.Fatalf("DeriveWrapKey (again): %v", err)
	}
	if !bytes.Equal(wrap1.Bytes(), wrap2.Bytes()) {
		t.Error("DeriveWrapKey is not deterministic for a fixed root")
	}

	sign, err := watchword.DeriveSigningKey(root)
	if err != nil {
		t.Fatalf("DeriveSigningKey: %v", err)
	}
	// The 32-byte x25519 seed must not equal the ed25519 seed (first 32 bytes
	// of the private key): distinct HKDF info strings guarantee this.
	if bytes.Equal(wrap1.Bytes(), sign.Seed()) {
		t.Error("wrap key seed equals signing key seed; HKDF info strings are not domain-separating")
	}
}
