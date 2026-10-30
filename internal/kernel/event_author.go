package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"time"
)

// canonicalEventV1 is the domain-separation tag for events that carry no HLC
// field (written before the HLC wave, or legacy events read from JSONL). It is
// FROZEN: the byte layout of v1 CanonicalBytes must never change, because
// grafted peer events signed under v1 cannot be re-signed locally.
const canonicalEventV1 = "mazarbul-event-v1"

// canonicalEventV2 is the domain-separation tag for events that carry a
// non-empty HLC field. The v2 layout appends lenpfx(hlc) after lenpfx(payload)
// so the HLC is part of the signed material and tampering with it breaks
// verification. All locally-written events are stamped with an HLC in
// prepare() before signing, so they use v2. Legacy events (HLC == "") keep v1
// verbatim — no retro-stamp, as peer-signed events cannot be re-signed.
const canonicalEventV2 = "mazarbul-event-v2"

// EventAuthor is the mandatory, signed authorship block carried by every
// Event. It binds an event to a concrete principal + device and proves,
// via an ed25519 signature over CanonicalBytes, that the holder of the
// device's signing key produced this exact event body.
//
// The block is stamped by the injected Signer inside FileEventLog.prepare
// (after UID/CreateTime/Scope are final) and verified for team-scope
// events during Rebuild. Personal/project events carry an Author but are
// never verified (the verification cost is gated to the team boundary,
// where cross-writer authenticity actually matters).
//
// Author is mandatory: there is no omitempty. Events written before this
// field existed are re-stamped once by the sign-events migration
// (see migrate_authors.go) so the on-disk shape is uniform.
type EventAuthor struct {
	// PrincipalUID is the ULID of the principal (a person entity) on whose
	// behalf the event was written. Resolved from the local self binding
	// at the write boundary; see internal/config self->principal map.
	PrincipalUID string `json:"principalUid"`

	// DeviceID is the enrolled device that produced the event.
	DeviceID string `json:"deviceId"`

	// Sig is the ed25519 signature over CanonicalBytes. Marshals as
	// base64 in JSON. Excluded from the canonical bytes it covers.
	Sig []byte `json:"sig"`

	// SigKeyID selects the device's enrolled public key for verification
	// (a fingerprint of that key; see identity.SigKeyID). Excluded from
	// the canonical bytes but travels in the clear so a verifier knows
	// which key to fetch.
	SigKeyID string `json:"sigKeyId"`
}

// CanonicalBytes returns the deterministic byte string an event signature
// covers. It is built from the typed fields plus the verbatim Payload
// bytes, NOT from json.Marshal(e): the envelope's JSON encoding can drift
// across Go versions, whitespace, and omitempty boundaries, but the
// canonical bytes must reproduce identically from write through JSONL
// read-back. Payload is a json.RawMessage that round-trips verbatim, so
// it is the one field safe to include as-is.
//
// Excluded: Name (derived from UID), Author.Sig and Author.SigKeyID (a
// signature cannot cover itself; SigKeyID only selects the verifying
// key). PrincipalUID and DeviceID ARE included so the signature binds the
// claimed authorship identity to the payload.
//
// Two layout versions exist, selected by the presence of e.HLC, where
// lenpfx(b) = uint32-big-endian(len(b)) || b (length-prefixing removes
// field-boundary ambiguity between adjacent variable-length fields):
//
//   - v1 (HLC == ""): tag "mazarbul-event-v1" NUL || lenpfx(uid) ||
//     lenpfx(createTime RFC3339Nano UTC) || lenpfx(kind) || lenpfx(source)
//     || lenpfx(confidence) || lenpfx(scope) || lenpfx(principalUid) ||
//     lenpfx(deviceId) || lenpfx(payload). Byte-for-byte identical to the
//     pre-wave layout; peer-signed v1 events verify without any re-stamp.
//
//   - v2 (HLC != ""): tag "mazarbul-event-v2" NUL || (all v1 fields) ||
//     lenpfx(hlc). The HLC is the last field so v2 extends v1 without
//     shifting any field offsets; a verifier sees a different tag and an
//     extra trailing field, making v1/v2 signatures non-interchangeable.
//
// All locally-written events are stamped with an HLC in prepare() before
// signing and therefore use v2. Grafted peer events keep their original HLC
// (and their original v1 or v2 signature) verbatim.
func (e Event) CanonicalBytes() []byte {
	tag := canonicalEventV1
	if e.HLC != "" {
		tag = canonicalEventV2
	}
	buf := make([]byte, 0, 256+len(e.Payload)+len(e.HLC))
	buf = append(buf, tag...)
	buf = append(buf, 0) // NUL terminator after the domain-separation tag

	buf = appendLenPrefixed(buf, []byte(e.UID))
	buf = appendLenPrefixed(buf, []byte(e.CreateTime.UTC().Format(time.RFC3339Nano)))
	buf = appendLenPrefixed(buf, []byte(e.Kind))
	buf = appendLenPrefixed(buf, []byte(e.Source))
	buf = appendLenPrefixed(buf, []byte(strconv.FormatFloat(e.Confidence, 'g', -1, 64)))
	buf = appendLenPrefixed(buf, []byte(e.Scope))
	buf = appendLenPrefixed(buf, []byte(e.Author.PrincipalUID))
	buf = appendLenPrefixed(buf, []byte(e.Author.DeviceID))
	buf = appendLenPrefixed(buf, e.Payload)
	if e.HLC != "" {
		buf = appendLenPrefixed(buf, []byte(e.HLC))
	}
	return buf
}

// appendLenPrefixed appends a 4-byte big-endian length prefix followed by
// b. Event fields are far smaller than 4 GiB; the clamp is defensive so
// the conversion can never wrap.
func appendLenPrefixed(buf, b []byte) []byte {
	n := len(b)
	if n > math.MaxUint32 {
		n = math.MaxUint32
	}
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(n))
	buf = append(buf, lp[:]...)
	buf = append(buf, b[:n]...)
	return buf
}

// Signer stamps an Event's authorship block. It is two-phase because the
// principalUID and deviceID it returns are themselves part of the
// canonical bytes: the kernel calls Author() to learn the identity,
// populates EventAuthor with it (Sig empty), computes CanonicalBytes, and
// only then calls SignCanonical over those bytes.
//
// The production implementation (internal/identity.KeychainSigner) signs
// with the Watchword-derived ed25519 key; tests and single-user local
// mode use the deterministic local default installed by NewFileEventLog.
type Signer interface {
	// Author returns the identity fields stamped into every event:
	// principalUID, deviceID, and the sigKeyID that selects the
	// verifying public key.
	Author() (principalUID, deviceID, sigKeyID string)

	// SignCanonical returns the ed25519 signature over the event's
	// canonical bytes.
	SignCanonical(canonical []byte) (sig []byte, err error)
}

// Verifier checks an Event's signature against the enrolled public key
// selected by ev.Author.SigKeyID. It is invoked only on team-scope replay
// (the verification gate in Rebuild); personal/project events skip it.
// A nil Verifier on the log means "no team verification configured",
// which is the single-user default. The event is passed by pointer to
// avoid copying the full Event per verification.
type Verifier interface {
	Verify(ctx context.Context, ev *Event) error
}

// ErrSignatureInvalid is returned by a Verifier when an event's signature
// does not verify against the claimed signing key (wrong key, tampered
// payload, or truncation). Rebuild records it as an ApplyError.
var ErrSignatureInvalid = errors.New("kernel: event signature invalid")

// ErrDeviceNotEnrolled reports that no device key is enrolled under a given
// (sigKeyID, principalUID). A device-key lookup MUST return this (not a bare
// formatted error) for the clean not-found case so the team verifier can tell
// "not bootstrapped yet" — the ONLY case its TOFU self-vouch fallback may trust
// an event's embedded key — from a transient backing-store fault, which must
// fail closed rather than silently downgrade trust to the embedded key.
var ErrDeviceNotEnrolled = errors.New("kernel: no device enrolled for sig key id")

// ErrPermission is returned when a principal attempts a team-scope write
// without authority — they are not an admitted, non-revoked team member
// (and the write is not the one allowed bootstrap, a genesis self-grant).
// The MCP tool layer maps this to the E_PERMISSION error code.
var ErrPermission = errors.New("kernel: principal not authorized for team scope")

// TeamWriteAuthority gates a LIVE team-scope write at the append boundary.
// AuthorizeTeamWrite returns nil when authorPrincipalUID may author ev
// into the team scope, or an error wrapping ErrPermission otherwise. It is
// consulted only for team-scope events on live Append (never during
// Rebuild, where admission/quarantine is the membership fold's job).
//
// The implementation lives in the dispatcher (which holds the ACL and can
// read the event payload): a member is authorized; a non-member is denied
// EXCEPT for a genesis self-grant (a teamMemberGranted naming the author
// themselves), the one bootstrap that creates the first member. The event
// is passed before its Author block is stamped, so the principal is passed
// explicitly.
type TeamWriteAuthority interface {
	AuthorizeTeamWrite(ctx context.Context, ev *Event, authorPrincipalUID string) error
}

// TeamRoleChecker queries a principal's effective role AT A POSITION in the
// canonical event order. It is consulted by verifyTeamEvent — on live graft
// and on Rebuild replay alike — to enforce role-at-position for content
// events: a reader-authored content event is dropped to Rejections.
//
// TeamMemberRoleAt returns the role string ("owner", "admin", "member",
// "reader") and ok=true when the principal is admitted, both computed by
// re-folding ONLY the membership events with event UID strictly below
// beforeEventUID. That restriction is what makes the verdict convergent: it
// is a pure function of the membership-event set the device holds, so a
// role change or revoke that post-dates the event being verified can never
// flip the verdict, regardless of arrival order. Ok=false means not
// admitted at that position; the caller treats that as a non-content
// verification concern (the verifier handles signature/admission
// separately).
type TeamRoleChecker interface {
	TeamMemberRoleAt(ctx context.Context, principalUID, beforeEventUID string) (role string, ok bool, err error)
}

// DeviceRevokeChecker queries whether a device has been revoked by a
// revocation event whose UID is strictly less than beforeEventUID. It is
// consulted by verifyTeamEvent — on live graft and on Rebuild replay alike —
// to enforce the convergent revocation fold: an event authored by a revoked
// device with a UID strictly greater than the revocation's UID is dropped to
// Rejections. The verdict is position-pure: it re-folds only the
// device_revocations whose event_uid < beforeEventUID, so every peer reaches
// the same conclusion regardless of arrival order.
//
// DeviceRevokedAt matches when EITHER deviceID OR sigKeyID appears in a
// revocation record, covering both forms of revocation (by device ULID and
// by key fingerprint). Returns (false, nil) when no revocation exists.
type DeviceRevokeChecker interface {
	DeviceRevokedAt(ctx context.Context, deviceID, sigKeyID, beforeEventUID string) (bool, error)
}

// ed25519Signer is a Signer over a fixed ed25519 key and identity. It
// backs the local default installed by NewFileEventLog, and is the
// concrete the re-stamp migration and tests construct directly. The
// production daemon wires internal/identity.KeychainSigner instead, which
// loads the key lazily from the keychain.
type ed25519Signer struct {
	priv         ed25519.PrivateKey
	principalUID string
	deviceID     string
	sigKeyID     string
}

// NewEd25519Signer constructs a Signer from a private key and identity.
func NewEd25519Signer(priv ed25519.PrivateKey, principalUID, deviceID, sigKeyID string) Signer {
	return &ed25519Signer{priv: priv, principalUID: principalUID, deviceID: deviceID, sigKeyID: sigKeyID}
}

func (s *ed25519Signer) Author() (string, string, string) {
	return s.principalUID, s.deviceID, s.sigKeyID
}

func (s *ed25519Signer) SignCanonical(canonical []byte) ([]byte, error) {
	return ed25519.Sign(s.priv, canonical), nil
}

// localDefaultPrincipalUID / localDefaultSigKeyID are the sentinel
// identity the local default signer stamps. They are intentionally not a
// real ULID: an event authored "local" was written without a configured
// principal identity (single-user / test / pre-genesis). Production
// overrides the signer via WithSigner before any such event is written.
const (
	localDefaultPrincipalUID = "local"
	localDefaultDeviceID     = "local"
	localDefaultSigKeyID     = "kid:local-default"
)

// PreIdentity reports whether this author is the local-default placeholder:
// an event written before any principal identity was configured (the
// first-boot ontology bootstrap, single-user setups, tests). The placeholder
// trio (PrincipalUID/DeviceID/SigKeyID) is stamped together, so the device
// marker suffices as the discriminator. Such events are signed by the PUBLIC
// deterministic default key and carry no authorship authority: a sealer must
// not bind them to a real device header (chunk.Header.AuthorsMatchDevice
// would reject the whole chunk at the replay boundary), and the team-write
// gate already refuses them.
func (a EventAuthor) PreIdentity() bool {
	return a.DeviceID == localDefaultDeviceID
}

// newLocalDefaultSigner returns the Signer NewFileEventLog installs when no
// WithSigner option is supplied. It derives a fixed ed25519 key from a
// constant seed so the signature is deterministic (stable golden output,
// reproducible tests) and the mandatory Author field is never empty. It
// is NOT a real identity: production wiring (orchestrator, kernelboot)
// overrides it with the Watchword-derived KeychainSigner, and team-scope
// writes (#213) reject the local default. Because verification is gated
// to team scope, the local default is harmless for personal/project.
func newLocalDefaultSigner() Signer {
	seed := sha256.Sum256([]byte("mazarbul:local-default-signer:v1"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return NewEd25519Signer(priv, localDefaultPrincipalUID, localDefaultDeviceID, localDefaultSigKeyID)
}
