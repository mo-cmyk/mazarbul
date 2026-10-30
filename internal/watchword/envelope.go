package watchword

import (
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/nacl/box"
)

// Envelope encryption for team scopes.
//
// A team scope is sealed under a random per-epoch data-encryption key
// (the DEK) fed to the existing Seal/Open AEAD — there is no new cipher.
// The DEK is distributed to each member by WRAPPING it to that member's
// x25519 wrapping public key with a NaCl anonymous sealed box
// (curve25519 + XSalsa20-Poly1305). An anonymous sealed box is the right
// primitive for grant-by-pasted-pubkey: the granter needs only the
// grantee's public key (no shared session, no granter keypair in the
// wrap), and the grantee unwraps with their wrapping private key. The
// wrapped DEK is therefore safe to publish in the plaintext
// recipients.jsonl artifact — only the holder of the matching private
// key can recover it.
//
// The wrapping keypair is the principal's x25519 key (watchword.DeriveWrapKey
// / identity.Store.WrappingKey); its public half is recorded in the
// principals projection at registration, so a granter resolves it without
// contacting the grantee.

// DEKSize is the byte length of a team data-encryption key. It matches
// the XChaCha20-Poly1305 key size the Seal/Open AEAD consumes.
const DEKSize = 32

// wrapKeySize is the byte length of an x25519 public key / scalar, the
// fixed-size arrays NaCl box operates over.
const wrapKeySize = 32

// ErrUnwrapFailed indicates a wrapped DEK could not be opened with the
// given wrapping private key: the recipient is not in the sealed box's
// recipient set, the ciphertext was tampered, or the key is wrong.
var ErrUnwrapFailed = errors.New("watchword: unwrap DEK failed")

// GenerateDEK returns a fresh 32-byte data-encryption key from
// crypto/rand. It is the symmetric key a team scope's chunks are sealed
// under for one epoch.
func GenerateDEK() ([]byte, error) {
	return generateDEKWithReader(rand.Reader)
}

// generateDEKWithReader is the test seam for GenerateDEK.
func generateDEKWithReader(r io.Reader) ([]byte, error) {
	dek := make([]byte, DEKSize)
	if _, err := io.ReadFull(r, dek); err != nil {
		return nil, fmt.Errorf("watchword: read DEK entropy: %w", err)
	}
	return dek, nil
}

// WrapDEK seals dek to recipientWrapPub (a 32-byte x25519 public key)
// with a NaCl anonymous sealed box. The result carries an ephemeral
// public key + ciphertext; only the holder of the matching private key
// can open it, so the output is safe to publish in plaintext. The
// granter needs nothing but the recipient's public key.
func WrapDEK(dek, recipientWrapPub []byte) ([]byte, error) {
	if len(dek) != DEKSize {
		return nil, fmt.Errorf("watchword: DEK must be %d bytes, got %d", DEKSize, len(dek))
	}
	if len(recipientWrapPub) != wrapKeySize {
		return nil, fmt.Errorf("watchword: wrap public key must be %d bytes, got %d", wrapKeySize, len(recipientWrapPub))
	}
	var recip [wrapKeySize]byte
	copy(recip[:], recipientWrapPub)
	sealed, err := box.SealAnonymous(nil, dek, &recip, rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("watchword: seal anonymous box: %w", err)
	}
	return sealed, nil
}

// UnwrapDEK opens a WrapDEK-produced sealed box with the recipient's
// x25519 wrapping private key, returning the 32-byte DEK. Returns
// ErrUnwrapFailed if the box does not open for this key (wrong
// recipient, tamper, or truncation).
func UnwrapDEK(sealed []byte, wrapPriv *ecdh.PrivateKey) ([]byte, error) {
	if wrapPriv == nil {
		return nil, fmt.Errorf("%w: nil wrapping key", ErrUnwrapFailed)
	}
	var pub, priv [wrapKeySize]byte
	pubBytes := wrapPriv.PublicKey().Bytes()
	privBytes := wrapPriv.Bytes()
	if len(pubBytes) != wrapKeySize || len(privBytes) != wrapKeySize {
		return nil, fmt.Errorf("%w: wrapping key has wrong size", ErrUnwrapFailed)
	}
	copy(pub[:], pubBytes)
	copy(priv[:], privBytes)
	dek, ok := box.OpenAnonymous(nil, sealed, &pub, &priv)
	if !ok {
		return nil, ErrUnwrapFailed
	}
	if len(dek) != DEKSize {
		return nil, fmt.Errorf("%w: unwrapped %d bytes, want %d", ErrUnwrapFailed, len(dek), DEKSize)
	}
	return dek, nil
}
