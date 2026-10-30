// Package mellon is Mazarbul's device-pairing layer (the "speak
// friend and enter" gate, from project lore). It bundles the secrets
// a freshly paired device needs to participate in sync — the
// Watchword, the remote URLs per scope, and the operator's chosen
// device identity — into a single sealed payload guarded by a short
// numeric code.
//
// Pairing is air-gapped on purpose: the existing device produces a
// (code, bundle) pair; the new device types the code and pastes the
// bundle. There is no network listener, no TCP socket, no firewall
// hole. Bring-your-own-transport: AirDrop, signal, scan-the-QR,
// scribble-on-paper — any channel that can move ~1 KB of text and
// six digits will work.
//
// Cryptography:
//   - Pair code is a 6-digit random number (0–999_999), constant-
//     time padded so a leading zero does not leak.
//   - Pairing key = Argon2id(code, salt=BLAKE3("mellon:v1:" + bundleID),
//     m=64MiB, t=3, p=1) — the same OWASP 2025 parameters Watchword
//     uses, so a brute-forcer needs a ~64 MiB / 3-pass cost per
//     guess. Six digits is 10^6 attempts → ~3 minutes per million
//     attempts on a fast laptop, which is short but not free, and
//     codes are one-shot (the producer deletes the bundle file
//     after a successful pair).
//   - The bundle itself is XChaCha20-Poly1305 AEAD-sealed via
//     watchword.Seal under the derived pairing key. Tampering
//     surfaces as ErrAuthFailed at Open time.
//
// Wire shape (base64-encoded, copy-paste friendly):
//
//	[16 bytes] BundleID (random; binds the Argon2id salt to one bundle)
//	[remainder] sealed payload: Seal(pairingKey, gzip-free JSON-serialised PairBundle)
package mellon

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// PairBundle is the plaintext payload one device hands to another
// during pairing. The receiving device absorbs each field into local
// state: Watchword → keychain (and derives the scope keys on first
// load); RemoteURLs → state.toml's sync section; DeviceID → the
// transport's commit-identity. Comments per field describe absorption
// semantics so future maintainers can grow the schema safely.
type PairBundle struct {
	// SchemaVersion lets the receiving device reject a bundle from a
	// future-incompatible producer. Bumped when the field set
	// changes; producers always set this to the constant defined in
	// this package. Required.
	SchemaVersion int `json:"schemaVersion"`

	// Watchword is the BIP-39 mnemonic the operator established at
	// init time. Used to derive every scope key. Required.
	Watchword string `json:"watchword"`

	// RemoteURLs maps a scope to its upstream git remote. Personal
	// is mandatory in practice; team / project keys are optional and
	// only present when the operator has configured them. Required
	// (the map itself; values are scope-specific).
	RemoteURLs map[kernel.Scope]string `json:"remoteUrls"`

	// DeviceID is a stable identifier for this NEW device — chosen
	// by the operator on the receiving side (the bundle producer
	// supplies a default, the receiver may override). Required.
	DeviceID string `json:"deviceId"`

	// Note is free-text intended for operator-to-operator context
	// ("MacBook, work, 2026"). Never used by the daemon. Optional.
	Note string `json:"note,omitempty"`
}

// schemaVersion is the on-the-wire version stamp every newly
// generated bundle carries.
const schemaVersion = 1

// codeMin and codeMax bracket the inclusive range of valid pair
// codes. Six digits, zero-padded so "012345" is acceptable.
const (
	codeMin = 0
	codeMax = 999_999
)

// CodeWidth is the canonical length of a pairing code string.
const CodeWidth = 6

// bundleIDLen is the random-prefix length on the wire. 128 bits is
// enough to make every bundle's Argon2id salt unique.
const bundleIDLen = 16

// salt prefix bound into the Argon2id input so a leaked pairing code
// cannot be replayed against an unrelated bundle.
const saltPrefix = "mellon:v1:"

// ErrInvalidCode is returned by Pair when the supplied code is not
// six digits in 0–999_999.
var ErrInvalidCode = errors.New("mellon: pair code must be a 6-digit number")

// ErrCorruptBundle is returned by Pair when the wire format does not
// parse — too-short payload, malformed base64, JSON syntax error.
var ErrCorruptBundle = errors.New("mellon: bundle is corrupt")

// ErrWrongCode is returned by Pair when the supplied code did not
// decrypt the bundle (AEAD authentication failed). Disambiguates
// from ErrCorruptBundle so the TUI can show "wrong code, try again"
// vs "this bundle is broken".
var ErrWrongCode = errors.New("mellon: code did not decrypt bundle")

// ErrUnsupportedVersion is returned by Pair when the bundle's
// schemaVersion is not one this build understands.
var ErrUnsupportedVersion = errors.New("mellon: bundle schema version is not supported")

// GenerateCode returns a fresh random 6-digit pair code as a string
// (zero-padded to CodeWidth digits). Suitable for printing in the
// TUI and reading aloud across a meeting room.
func GenerateCode() (string, error) {
	max := uint64(codeMax - codeMin + 1)
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("mellon: read rng: %w", err)
	}
	v := binary.BigEndian.Uint64(buf[:]) % max
	return fmt.Sprintf("%0*d", CodeWidth, v), nil
}

// Generate seals bundle under a fresh pairing key derived from code.
// Returns the base64-encoded wire payload. The caller is expected to
// transmit (code, wire) to the receiving device via independent
// channels — typing code into a TUI prompt while pasting wire is the
// canonical flow.
func Generate(code string, bundle PairBundle) (string, error) {
	if err := validateCode(code); err != nil {
		return "", err
	}
	if bundle.Watchword == "" {
		return "", errors.New("mellon: bundle is missing the watchword")
	}
	if bundle.DeviceID == "" {
		return "", errors.New("mellon: bundle is missing a device ID")
	}
	bundle.SchemaVersion = schemaVersion

	bundleID := make([]byte, bundleIDLen)
	if _, err := rand.Read(bundleID); err != nil {
		return "", fmt.Errorf("mellon: read rng: %w", err)
	}
	key := deriveKey(code, bundleID)
	plain, err := json.Marshal(&bundle)
	if err != nil {
		return "", fmt.Errorf("mellon: marshal bundle: %w", err)
	}
	sealed, err := watchword.Seal(key, plain)
	if err != nil {
		return "", fmt.Errorf("mellon: seal bundle: %w", err)
	}
	var out bytes.Buffer
	out.Write(bundleID)
	out.Write(sealed)
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// Pair decrypts wire using code and returns the embedded PairBundle.
// Distinguishes between three failure modes:
//   - ErrInvalidCode: code is not 6 digits
//   - ErrCorruptBundle: wire is not the expected framing
//   - ErrWrongCode: framing is fine but AEAD auth failed (wrong code
//     OR tampered ciphertext)
func Pair(code, wire string) (PairBundle, error) {
	if err := validateCode(code); err != nil {
		return PairBundle{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(wire)
	if err != nil {
		return PairBundle{}, fmt.Errorf("%w: %w", ErrCorruptBundle, err)
	}
	if len(raw) < bundleIDLen+1 {
		return PairBundle{}, fmt.Errorf("%w: %d bytes, want >= %d", ErrCorruptBundle, len(raw), bundleIDLen+1)
	}
	bundleID := raw[:bundleIDLen]
	sealed := raw[bundleIDLen:]
	key := deriveKey(code, bundleID)
	plain, err := watchword.Open(key, sealed)
	if err != nil {
		if errors.Is(err, watchword.ErrAuthFailed) {
			return PairBundle{}, ErrWrongCode
		}
		return PairBundle{}, fmt.Errorf("%w: %w", ErrCorruptBundle, err)
	}
	var bundle PairBundle
	if err := json.Unmarshal(plain, &bundle); err != nil {
		return PairBundle{}, fmt.Errorf("%w: %w", ErrCorruptBundle, err)
	}
	if bundle.SchemaVersion != schemaVersion {
		return PairBundle{}, fmt.Errorf("%w: bundle is v%d, this build understands v%d",
			ErrUnsupportedVersion, bundle.SchemaVersion, schemaVersion)
	}
	return bundle, nil
}

// validateCode rejects anything that is not exactly CodeWidth decimal
// digits. Tighter than strconv.Atoi to keep tokens like "0x123abc"
// or " 12345" from sneaking through with a forgiving parse.
func validateCode(code string) error {
	if len(code) != CodeWidth {
		return fmt.Errorf("%w: got %d chars", ErrInvalidCode, len(code))
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return fmt.Errorf("%w: non-digit %q", ErrInvalidCode, c)
		}
	}
	return nil
}

// deriveKey runs Argon2id over (code, salt=saltPrefix+bundleID) with
// OWASP-2025 parameters. Output is a 32-byte XChaCha20-Poly1305 key
// suitable for watchword.Seal/Open.
func deriveKey(code string, bundleID []byte) []byte {
	salt := make([]byte, 0, len(saltPrefix)+len(bundleID))
	salt = append(salt, []byte(saltPrefix)...)
	salt = append(salt, bundleID...)
	return argon2.IDKey([]byte(code), salt, 3, 64*1024, 1, 32)
}
