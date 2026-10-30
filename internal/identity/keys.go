// Package identity owns Mazarbul's per-writer cryptographic identity: the
// ed25519 signing keypair and x25519 wrapping keypair that sit beside the
// Watchword in the OS keychain, the stable device id, and the GitHub
// trust anchor. Both keypairs are derived deterministically from the
// Watchword root (see internal/watchword) so they survive keychain loss —
// a restore-from-mnemonic reproduces the same identity.
//
// This file holds the key material and accessors and intentionally does
// NOT import internal/kernel: the kernel depends only on the small
// Signer/Verifier interfaces it defines, and the glue that adapts these
// keys into those interfaces lives in event_signing.go.
package identity

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// Keychain account names for the identity material. Colon-namespaced,
// consistent with watchword.KeychainKey ("mazarbul:watchword") and the
// sync PAT names. The keychain is a derivation CACHE: on a miss the
// material is re-derived from the Watchword mnemonic and written back, so
// per-boot key access never re-runs Argon2id once warmed.
const (
	KeychainEd25519Seed = "mazarbul:identity:ed25519" // base64 32-byte ed25519 seed
	KeychainX25519Seed  = "mazarbul:identity:x25519"  // base64 32-byte x25519 scalar
	KeychainDeviceID    = "mazarbul:identity:device"  // device ULID, stable across reboots

	// KeychainDeviceSigSeed is the keychain name for a per-device RANDOM
	// ed25519 seed (distinct from the Watchword-derived identity seed). When
	// this entry is present the local device is a subkey device — it signed its
	// enrollment with this random key rather than the principal's
	// Watchword-derived key. Absent on the original "device 0" (the device that
	// initially provisioned the Watchword), which continues to use the
	// Watchword-derived key and remains valid without any migration.
	KeychainDeviceSigSeed = "mazarbul:device:sigseed" // base64 32-byte random ed25519 seed
)

// seedLen is the byte length of both ed25519 seeds and x25519 scalars.
const seedLen = 32

// ErrNoWatchword is returned when identity material is absent from the
// keychain and cannot be derived because no Watchword mnemonic is stored.
// Callers treat this as "identity not provisioned yet" (e.g. genesis runs
// after the wizard stores the Watchword).
var ErrNoWatchword = errors.New("identity: no watchword available to derive keys")

// errSeedMiss signals that a cached seed is absent or unusable and must be
// re-derived. Internal sentinel; never escapes the package.
var errSeedMiss = errors.New("identity: seed cache miss")

// Store reads and provisions the device's identity material through a
// secret.Provider (the keychain chain). It is safe for concurrent use:
// every method is stateless beyond the provider it wraps.
type Store struct {
	secrets secret.Provider
}

// New returns a Store backed by the given secret provider chain.
func New(secrets secret.Provider) *Store {
	return &Store{secrets: secrets}
}

// Identity is the public-facing summary the enrollment ceremony needs:
// the stable device id, the signing-key fingerprint, and the two public
// keys that get enrolled / published. Private material never appears here.
type Identity struct {
	DeviceID   string
	SigKeyID   string
	SignPublic ed25519.PublicKey
	WrapPublic *ecdh.PublicKey
}

// SigKeyID derives the stable key id from an ed25519 public key: a
// truncated, lowercased base32 of its SHA-256. Self-certifying — a
// verifier can recompute it from a looked-up key and compare — and used
// as the lookup key into the enrolled-device table.
func SigKeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return "kid:" + strings.ToLower(enc[:32])
}

// SigningKey returns the device ed25519 signing private key, reading the
// cached seed from the keychain and re-deriving from the Watchword on a
// miss (writing the seed back so the next call is cheap).
func (s *Store) SigningKey(ctx context.Context) (ed25519.PrivateKey, error) {
	if seed, err := s.cachedSeed(ctx, KeychainEd25519Seed); err == nil {
		return ed25519.NewKeyFromSeed(seed), nil
	} else if !errors.Is(err, errSeedMiss) {
		return nil, err
	}
	root, err := s.watchwordRoot(ctx)
	if err != nil {
		return nil, err
	}
	key, err := watchword.DeriveSigningKey(root)
	if err != nil {
		return nil, err
	}
	if err := s.storeSeed(ctx, KeychainEd25519Seed, key.Seed()); err != nil {
		return nil, err
	}
	return key, nil
}

// SigningPublicKey returns the ed25519 public key (the verifying key
// enrolled in deviceEnrolled and fingerprinted into the sigKeyID).
func (s *Store) SigningPublicKey(ctx context.Context) (ed25519.PublicKey, error) {
	priv, err := s.SigningKey(ctx)
	if err != nil {
		return nil, err
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok { // unreachable: ed25519.PrivateKey.Public always returns ed25519.PublicKey
		return nil, errors.New("identity: unexpected public key type")
	}
	return pub, nil
}

// WrappingKey returns the device x25519 key-wrapping private key.
func (s *Store) WrappingKey(ctx context.Context) (*ecdh.PrivateKey, error) {
	if seed, err := s.cachedSeed(ctx, KeychainX25519Seed); err == nil {
		priv, perr := ecdh.X25519().NewPrivateKey(seed)
		if perr != nil {
			return nil, fmt.Errorf("identity: x25519 private key: %w", perr)
		}
		return priv, nil
	} else if !errors.Is(err, errSeedMiss) {
		return nil, err
	}
	root, err := s.watchwordRoot(ctx)
	if err != nil {
		return nil, err
	}
	key, err := watchword.DeriveWrapKey(root)
	if err != nil {
		return nil, err
	}
	if err := s.storeSeed(ctx, KeychainX25519Seed, key.Bytes()); err != nil {
		return nil, err
	}
	return key, nil
}

// WrappingPublicKey returns the x25519 public key.
func (s *Store) WrappingPublicKey(ctx context.Context) (*ecdh.PublicKey, error) {
	priv, err := s.WrappingKey(ctx)
	if err != nil {
		return nil, err
	}
	return priv.PublicKey(), nil
}

// MintDeviceSubkey generates a fresh random ed25519 seed and stores it
// under KeychainDeviceSigSeed, returning the private key. The operation is
// idempotent at the caller's discretion — if a seed already exists it is
// OVERWRITTEN; callers should check HasDeviceSubkey first when the device
// may already be enrolled. The returned key is the subkey the device uses
// to sign events and the enrollment vouch covers.
func (s *Store) MintDeviceSubkey(ctx context.Context) (ed25519.PrivateKey, error) {
	seed := make([]byte, seedLen)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("identity: mint device subkey seed: %w", err)
	}
	if err := s.storeSeed(ctx, KeychainDeviceSigSeed, seed); err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// HasDeviceSubkey reports whether this device has a random device subkey
// stored in the keychain. False means the device uses the Watchword-derived
// principal key (device 0).
func (s *Store) HasDeviceSubkey(ctx context.Context) (bool, error) {
	_, err := s.cachedSeed(ctx, KeychainDeviceSigSeed)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, errSeedMiss) {
		return false, nil
	}
	return false, err
}

// DeviceSigningKey returns the ed25519 private key this device signs events
// with. When a random device subkey exists (KeychainDeviceSigSeed), it is
// returned — this is the subkey-device path (enrolled via Mellon pairing or
// explicit enrollment). Otherwise the Watchword-derived principal key is
// returned, preserving backward compatibility for the original "device 0"
// that provisioned the Watchword.
func (s *Store) DeviceSigningKey(ctx context.Context) (ed25519.PrivateKey, error) {
	if seed, err := s.cachedSeed(ctx, KeychainDeviceSigSeed); err == nil {
		return ed25519.NewKeyFromSeed(seed), nil
	} else if !errors.Is(err, errSeedMiss) {
		return nil, err
	}
	return s.SigningKey(ctx)
}

// DeviceSigningPublicKey returns the ed25519 public key paired with
// DeviceSigningKey: the subkey public key when a subkey exists, otherwise
// the Watchword-derived public key.
func (s *Store) DeviceSigningPublicKey(ctx context.Context) (ed25519.PublicKey, error) {
	priv, err := s.DeviceSigningKey(ctx)
	if err != nil {
		return nil, err
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("identity: unexpected public key type")
	}
	return pub, nil
}

// DeviceID returns the device's stable ULID, minting and persisting one
// to the keychain on first call. The device id is independent of the
// keypairs (it is not derived from the Watchword) so a re-paired device
// keeps a distinct identity even when it shares the recovery mnemonic.
func (s *Store) DeviceID(ctx context.Context) (string, error) {
	got, err := s.secrets.Get(ctx, KeychainDeviceID)
	if err == nil && got != "" {
		return got, nil
	}
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		return "", fmt.Errorf("identity: read device id: %w", err)
	}
	id, err := newULID()
	if err != nil {
		return "", err
	}
	if serr := s.secrets.Set(ctx, KeychainDeviceID, id); serr != nil {
		return "", fmt.Errorf("identity: persist device id: %w", serr)
	}
	return id, nil
}

// AdoptDeviceID persists proposed as this device's stable ULID when no device
// id exists in the keychain yet, and returns the effective id. An
// already-identified device keeps its existing id — pairing is idempotent and
// never re-identifies an established device. Mellon pairing uses this to
// adopt the device id the sending device stamped into the bundle.
func (s *Store) AdoptDeviceID(ctx context.Context, proposed string) (string, error) {
	got, err := s.secrets.Get(ctx, KeychainDeviceID)
	if err == nil && got != "" {
		return got, nil
	}
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		return "", fmt.Errorf("identity: read device id: %w", err)
	}
	if serr := s.secrets.Set(ctx, KeychainDeviceID, proposed); serr != nil {
		return "", fmt.Errorf("identity: persist device id: %w", serr)
	}
	return proposed, nil
}

// Initialize provisions all identity material idempotently: it ensures
// both keypairs and the device id exist in the keychain (deriving the
// keypairs from the Watchword when absent) and returns the public summary
// the enrollment ceremony needs. Safe to call on every boot.
func (s *Store) Initialize(ctx context.Context) (Identity, error) {
	signPub, err := s.SigningPublicKey(ctx)
	if err != nil {
		return Identity{}, err
	}
	wrapPub, err := s.WrappingPublicKey(ctx)
	if err != nil {
		return Identity{}, err
	}
	deviceID, err := s.DeviceID(ctx)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		DeviceID:   deviceID,
		SigKeyID:   SigKeyID(signPub),
		SignPublic: signPub,
		WrapPublic: wrapPub,
	}, nil
}

// cachedSeed returns the decoded 32-byte seed stored under name, or
// errSeedMiss when the entry is absent or unusable (so the caller
// re-derives). A genuine provider failure is returned wrapped.
func (s *Store) cachedSeed(ctx context.Context, name string) ([]byte, error) {
	got, err := s.secrets.Get(ctx, name)
	if err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			return nil, errSeedMiss
		}
		return nil, fmt.Errorf("identity: read %s: %w", name, err)
	}
	if got == "" {
		return nil, errSeedMiss
	}
	seed, derr := base64.StdEncoding.DecodeString(got)
	if derr != nil || len(seed) != seedLen {
		return nil, errSeedMiss // corrupt/short cache: re-derive
	}
	return seed, nil
}

// storeSeed base64-encodes and writes a seed under name.
func (s *Store) storeSeed(ctx context.Context, name string, seed []byte) error {
	if err := s.secrets.Set(ctx, name, base64.StdEncoding.EncodeToString(seed)); err != nil {
		return fmt.Errorf("identity: persist %s: %w", name, err)
	}
	return nil
}

// watchwordRoot loads the Watchword mnemonic from the keychain and
// stretches it to the Argon2id root. Returns ErrNoWatchword when the
// mnemonic is absent so callers can distinguish "not provisioned" from a
// real provider failure.
func (s *Store) watchwordRoot(ctx context.Context) ([]byte, error) {
	mnemonic, err := s.secrets.Get(ctx, watchword.KeychainKey)
	if err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			return nil, ErrNoWatchword
		}
		return nil, fmt.Errorf("identity: read watchword: %w", err)
	}
	if strings.TrimSpace(mnemonic) == "" {
		return nil, ErrNoWatchword
	}
	root, err := watchword.DeriveRoot(mnemonic)
	if err != nil {
		return nil, fmt.Errorf("identity: derive root: %w", err)
	}
	return root, nil
}

// newULID mints a fresh ULID from crypto/rand. Used for the device id.
func newULID() (string, error) {
	id, err := ulid.New(ulid.Now(), ulid.Monotonic(rand.Reader, math.MaxUint32))
	if err != nil {
		return "", fmt.Errorf("identity: mint ulid: %w", err)
	}
	return id.String(), nil
}
