package watchword_test

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

func newWrapKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate wrap key: %v", err)
	}
	return priv
}

func TestWrapUnwrapDEK_Roundtrip(t *testing.T) {
	t.Parallel()
	recip := newWrapKey(t)
	dek, err := watchword.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	if len(dek) != watchword.DEKSize {
		t.Fatalf("DEK len = %d, want %d", len(dek), watchword.DEKSize)
	}
	sealed, err := watchword.WrapDEK(dek, recip.PublicKey().Bytes())
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if bytes.Contains(sealed, dek) {
		t.Errorf("wrapped DEK leaks the plaintext DEK bytes")
	}
	got, err := watchword.UnwrapDEK(sealed, recip)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Errorf("unwrapped DEK mismatch:\n got = %x\nwant = %x", got, dek)
	}
}

func TestUnwrapDEK_WrongKeyFails(t *testing.T) {
	t.Parallel()
	recip := newWrapKey(t)
	other := newWrapKey(t)
	dek, err := watchword.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	sealed, err := watchword.WrapDEK(dek, recip.PublicKey().Bytes())
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if _, err := watchword.UnwrapDEK(sealed, other); !errors.Is(err, watchword.ErrUnwrapFailed) {
		t.Errorf("UnwrapDEK with wrong key: err = %v, want ErrUnwrapFailed", err)
	}
}

func TestUnwrapDEK_TamperedFails(t *testing.T) {
	t.Parallel()
	recip := newWrapKey(t)
	dek, err := watchword.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	sealed, err := watchword.WrapDEK(dek, recip.PublicKey().Bytes())
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	sealed[len(sealed)-1] ^= 0x01
	if _, err := watchword.UnwrapDEK(sealed, recip); !errors.Is(err, watchword.ErrUnwrapFailed) {
		t.Errorf("UnwrapDEK tampered: err = %v, want ErrUnwrapFailed", err)
	}
}

func TestWrapDEK_RejectsBadSizes(t *testing.T) {
	t.Parallel()
	recip := newWrapKey(t)
	if _, err := watchword.WrapDEK(make([]byte, 16), recip.PublicKey().Bytes()); err == nil {
		t.Error("WrapDEK must reject a short DEK")
	}
	dek, _ := watchword.GenerateDEK()
	if _, err := watchword.WrapDEK(dek, make([]byte, 16)); err == nil {
		t.Error("WrapDEK must reject a short wrap public key")
	}
}

// TestWrappedDEKsFedToSealOpen pins the contract that an unwrapped DEK is
// a valid 32-byte key for the existing Seal/Open AEAD — envelope mode
// reuses the cipher, it does not introduce a new one.
func TestWrappedDEKFeedsSealOpen(t *testing.T) {
	t.Parallel()
	recip := newWrapKey(t)
	dek, _ := watchword.GenerateDEK()
	sealed, err := watchword.WrapDEK(dek, recip.PublicKey().Bytes())
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	got, err := watchword.UnwrapDEK(sealed, recip)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	ct, err := watchword.Seal(got, []byte("team chunk payload"))
	if err != nil {
		t.Fatalf("Seal under unwrapped DEK: %v", err)
	}
	out, err := watchword.Open(got, ct)
	if err != nil || string(out) != "team chunk payload" {
		t.Errorf("Open under unwrapped DEK: out=%q err=%v", out, err)
	}
}
