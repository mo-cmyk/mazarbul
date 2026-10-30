// Package watchword implements Mazarbul's BIP-39-based recovery
// mnemonic + per-scope symmetric-key derivation.
//
// The Watchword is the master credential the user types back when
// pairing a new device or recovering from a lost keychain. It is
// derived from 256 bits of CSPRNG entropy and encoded as a BIP-39
// English mnemonic; the standard's checksum bit catches the most
// common transcription errors (one-bit flips, single-letter typos).
//
// Per the architecture spec privacy.md (M4 commitments closed in this
// PR): 24 words (BIP-39 max-entropy slot), Argon2id with OWASP-2025
// parameters as the root-key stretcher, HKDF-SHA-256 with scope-bound
// info for per-scope subkey derivation, XChaCha20-Poly1305 as the
// sealed-chunk AEAD.
//
// Note on word count: the original spec said "20-word Watchword" but
// BIP-39 only supports word counts of 12/15/18/21/24. 24 (256-bit
// entropy) is the safest choice — the spec's open-question hint
// ("wordlist choice (BIP-39 vs custom themed)") leaves the count open
// and the closest standards-compliant alternative wins on
// auditability.
package watchword

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// EntropyBits is the BIP-39 entropy size this package produces. 256 bits
// is the max-entropy BIP-39 slot and maps to a 24-word mnemonic.
const EntropyBits = 256

// EntropyBytes is EntropyBits / 8.
const EntropyBytes = EntropyBits / 8

// Words is the canonical mnemonic length (24 BIP-39 English words).
const Words = 24

// Argon2id parameters. OWASP 2025 baseline for general-purpose KDF:
// m=64 MiB, t=3, p=1. Tuned against the typical laptop: ~200 ms
// derivation cost, fast enough for unattended daemon boot, slow
// enough to defeat realistic offline brute-force against a stolen
// mnemonic.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
)

// rootSalt is the Argon2id salt for the root-key derivation. Domain-
// separated by the explicit v1 suffix so a future KDF migration can
// land alongside v1 without breaking recovery for existing devices.
const rootSalt = "mazarbul:root:v1"

// KeychainKey is the secret-provider name under which the user's
// Watchword mnemonic is stored. Daemon wiring reads this through the
// secret.Provider chain to derive scope keys on boot; the wizard
// (and any future restore flow) writes the same key on first-run.
// Single constant so the contract is checked at compile time.
const KeychainKey = "mazarbul:watchword"

// KeychainTeamDEKPrefix namespaces the per-epoch team data-encryption
// keys cached in the secret provider. The full key is
// KeychainTeamDEKPrefix + strconv.Itoa(epoch) (e.g. "mazarbul:team:dek:0"),
// storing the base64 DEK for that epoch. Epoch-keyed (not a single
// entry) so a forward re-key on revoke can mint a new epoch while old
// epochs stay available to decrypt historical chunks.
const KeychainTeamDEKPrefix = "mazarbul:team:dek:"

// scopeInfo returns the HKDF info string for a scope subkey. The
// domain separation is the scope name plus a version suffix.
func scopeInfo(scope string) string {
	return "mazarbul:scope:" + scope + ":v1"
}

// ErrInvalidMnemonic signals a malformed or checksum-failing mnemonic.
// Errors.Is hands the caller a typed sentinel without leaking which
// validation step rejected the input (defence in depth against
// timing-side-channel guesses on the wordlist).
var ErrInvalidMnemonic = errors.New("watchword: invalid mnemonic")

// Generate produces a new 24-word BIP-39 English mnemonic from 256
// bits of crypto/rand entropy. Callers should display the result
// exactly once and force the user to type it back before continuing —
// the mnemonic is the only path back from a lost keychain.
func Generate() (string, error) {
	return generateWithReader(rand.Reader)
}

// generateWithReader is the test seam for Generate so a deterministic
// reader can pin the test-vector output.
func generateWithReader(r io.Reader) (string, error) {
	entropy := make([]byte, EntropyBytes)
	if _, err := io.ReadFull(r, entropy); err != nil {
		return "", fmt.Errorf("watchword: read entropy: %w", err)
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return "", fmt.Errorf("watchword: encode mnemonic: %w", err)
	}
	return mnemonic, nil
}

// Validate returns nil iff mnemonic is a syntactically valid BIP-39
// English mnemonic of the canonical length whose checksum verifies.
// Wraps the typed sentinel ErrInvalidMnemonic so callers can branch
// without parsing the underlying message.
func Validate(mnemonic string) error {
	if !bip39.IsMnemonicValid(mnemonic) {
		return fmt.Errorf("%w: checksum or word-list mismatch", ErrInvalidMnemonic)
	}
	if got := wordCount(mnemonic); got != Words {
		return fmt.Errorf("%w: got %d words, want %d", ErrInvalidMnemonic, got, Words)
	}
	return nil
}

// DeriveRoot maps a mnemonic to a 32-byte root key via Argon2id. The
// returned key is the input to per-scope HKDF subkey derivation. The
// mnemonic must validate first; callers normally pair this with a
// preceding Validate.
func DeriveRoot(mnemonic string) ([]byte, error) {
	if err := Validate(mnemonic); err != nil {
		return nil, err
	}
	entropy, err := bip39.EntropyFromMnemonic(mnemonic)
	if err != nil {
		return nil, fmt.Errorf("watchword: extract entropy: %w", err)
	}
	root := argon2.IDKey(
		entropy,
		[]byte(rootSalt),
		argonTime,
		argonMemory,
		argonThreads,
		argonKeyLen,
	)
	return root, nil
}

// DeriveScopeKey returns a 32-byte symmetric key for a given scope
// name. HKDF-SHA-256 with info bound to the scope name + version,
// producing a hard domain separation: a leaked scope key cannot be
// used to forge events in a different scope.
//
// Scope must be a non-empty string; the kernel's Scope enum (project,
// personal, team) is the canonical caller, but the API is open to
// future custom scopes for plugin work.
func DeriveScopeKey(root []byte, scope string) ([]byte, error) {
	if len(root) != argonKeyLen {
		return nil, fmt.Errorf("watchword: root key must be %d bytes, got %d", argonKeyLen, len(root))
	}
	if scope == "" {
		return nil, errors.New("watchword: scope is required")
	}
	r := hkdf.New(sha256.New, root, nil, []byte(scopeInfo(scope)))
	out := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("watchword: hkdf read: %w", err)
	}
	return out, nil
}

// Identity-key HKDF info strings. Domain-separated from scope keys (and
// from each other) by a distinct purpose tag + version suffix, mirroring
// scopeInfo: a leaked signing key cannot be repurposed as a wrapping key
// or a scope key, and a v2 KDF can land beside v1 without breaking
// recovery for existing devices.
const (
	infoEd25519 = "mazarbul:identity:ed25519:v1"
	infoX25519  = "mazarbul:identity:x25519:v1"
)

// seedLen is the byte length both ed25519 seeds and x25519 scalars
// require. HKDF emits exactly this many bytes per identity key.
const seedLen = 32

// deriveSeed reads a 32-byte key seed from HKDF-SHA-256 over the root,
// domain-separated by info. Shared by DeriveSigningKey and DeriveWrapKey.
func deriveSeed(root []byte, info string) ([]byte, error) {
	if len(root) != argonKeyLen {
		return nil, fmt.Errorf("watchword: root key must be %d bytes, got %d", argonKeyLen, len(root))
	}
	r := hkdf.New(sha256.New, root, nil, []byte(info))
	seed := make([]byte, seedLen)
	if _, err := io.ReadFull(r, seed); err != nil {
		return nil, fmt.Errorf("watchword: hkdf read: %w", err)
	}
	return seed, nil
}

// DeriveSigningKey returns the principal's ed25519 signing private key,
// derived deterministically from the Watchword root via HKDF. Because the
// key is a pure function of the mnemonic, it survives keychain loss: a
// restore-from-Watchword reproduces the same identity. The public half
// (priv.Public()) is the verifying key enrolled in deviceEnrolled and
// fingerprinted into the sigKeyID.
//
// The key is PRINCIPAL-scoped, not per-device: every device that loads the
// same Watchword derives the same key and sigKeyID, so an event signature
// authenticates the principal, and the deviceId in EventAuthor is attribution
// metadata rather than a second cryptographic identity. That is the deliberate
// single-device MVP model (recoverability over per-device isolation); binding
// derivation to a stable per-device secret is deferred to the teams work
// (#213), where revoking one device without rotating the principal matters.
func DeriveSigningKey(root []byte) (ed25519.PrivateKey, error) {
	seed, err := deriveSeed(root, infoEd25519)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// DeriveWrapKey returns the device's x25519 key-wrapping private key,
// derived deterministically from the Watchword root via HKDF. Reserved
// for sealing per-scope DEKs to a principal's public key (the teams
// envelope-encryption path); derived here so the key material exists and
// is recoverable from genesis, even though MVP does not yet wrap.
func DeriveWrapKey(root []byte) (*ecdh.PrivateKey, error) {
	seed, err := deriveSeed(root, infoX25519)
	if err != nil {
		return nil, err
	}
	priv, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return nil, fmt.Errorf("watchword: x25519 private key: %w", err)
	}
	return priv, nil
}

// Seal encrypts plaintext under a 32-byte scope key with
// XChaCha20-Poly1305 and no additional authenticated data. The output
// is the 24-byte nonce concatenated with the ciphertext + 16-byte auth
// tag. Thin wrapper over SealWithAAD(…, nil); see there for details.
func Seal(scopeKey, plaintext []byte) ([]byte, error) {
	return SealWithAAD(scopeKey, plaintext, nil)
}

// SealWithAAD encrypts plaintext under a 32-byte scope key with
// XChaCha20-Poly1305, binding aad as additional authenticated data.
// The aad is authenticated but NOT encrypted: Open succeeds only when
// called with byte-identical aad, so callers bind plaintext metadata
// (e.g. a chunk's plaintext header bytes) to the ciphertext, making any
// post-seal tamper of that metadata fail the auth tag. The output is
// the 24-byte nonce concatenated with the ciphertext + 16-byte auth
// tag; aad is NOT stored (the caller reproduces it on Open).
//
// SealWithAAD returns an error on nonce-RNG failure — a CSPRNG failure
// is unrecoverable and writing a deterministic-nonce ciphertext is
// worse than failing the call.
func SealWithAAD(scopeKey, plaintext, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(scopeKey)
	if err != nil {
		return nil, fmt.Errorf("watchword: aead init: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("watchword: nonce: %w", err)
	}
	// Pre-allocate dst so Seal does not grow nonce mid-call.
	dst := make([]byte, 0, aead.NonceSize()+len(plaintext)+aead.Overhead())
	dst = append(dst, nonce...)
	return aead.Seal(dst, nonce, plaintext, aad), nil
}

// Open verifies and decrypts a Seal-produced ciphertext under the
// same scope key and no additional authenticated data. Returns
// ErrAuthFailed if the auth tag does not verify. Thin wrapper over
// OpenWithAAD(…, nil).
func Open(scopeKey, sealed []byte) ([]byte, error) {
	return OpenWithAAD(scopeKey, sealed, nil)
}

// OpenWithAAD verifies and decrypts a SealWithAAD-produced ciphertext
// under the same scope key and the same additional authenticated data.
// Returns ErrAuthFailed if the auth tag does not verify (wrong key,
// tampering, truncation, or aad that differs from the bytes bound at
// seal time).
func OpenWithAAD(scopeKey, sealed, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(scopeKey)
	if err != nil {
		return nil, fmt.Errorf("watchword: aead init: %w", err)
	}
	if len(sealed) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrAuthFailed
	}
	nonce := sealed[:aead.NonceSize()]
	ct := sealed[aead.NonceSize():]
	out, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuthFailed, err)
	}
	return out, nil
}

// ErrAuthFailed is returned by Open when the auth tag does not verify.
// Wraps with %w so callers can errors.Is.
var ErrAuthFailed = errors.New("watchword: auth failed")

func wordCount(s string) int {
	n := 0
	inWord := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			if inWord {
				n++
				inWord = false
			}
			continue
		}
		inWord = true
	}
	if inWord {
		n++
	}
	return n
}
