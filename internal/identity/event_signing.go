package identity

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// EventSigner builds a kernel.Signer over the device's active signing key.
// When a random device subkey exists (KeychainDeviceSigSeed) it is used; on
// the original "device 0" that provisioned the Watchword the Watchword-derived
// principal key is used instead. Both paths derive sig_key_id from the active
// public key, so the devices table lookup in the Verifier resolves correctly.
//
// PrincipalUID is the concrete person the local self binding resolves to
// (see the self->principalUID map). At genesis the freshly-minted UID is
// passed in before the binding is persisted.
//
// Concrete signer type is intentionally internal to package kernel.
//
//nolint:ireturn // returns the kernel.Signer the event log consumes; the interface is opaque by design.
func (s *Store) EventSigner(ctx context.Context, principalUID string) (kernel.Signer, error) {
	priv, err := s.DeviceSigningKey(ctx)
	if err != nil {
		return nil, err
	}
	pub, err := s.DeviceSigningPublicKey(ctx)
	if err != nil {
		return nil, err
	}
	deviceID, err := s.DeviceID(ctx)
	if err != nil {
		return nil, err
	}
	return kernel.NewEd25519Signer(priv, principalUID, deviceID, SigKeyID(pub)), nil
}

// DeviceEnroll builds a DeviceEnrolledEvent for the local device. When the
// device has a random subkey (HasDeviceSubkey is true) the event carries a
// principal-key VOUCH — a signature by the Watchword-derived principal key
// over the canonical vouch bytes — so peers can verify that the principal
// authorized this specific subkey. When running as the original "device 0"
// (no subkey) the vouch fields are left empty for backward compatibility;
// the event still self-signs with the device key per the existing protocol.
//
// The principalUID argument is the ULID of the owning principal. The caller
// is responsible for appending the returned event.
func (s *Store) DeviceEnroll(ctx context.Context, principalUID, deviceLabel string) (ontology.DeviceEnrolledEvent, error) {
	deviceID, err := s.DeviceID(ctx)
	if err != nil {
		return ontology.DeviceEnrolledEvent{}, fmt.Errorf("identity: enroll: device id: %w", err)
	}
	devPriv, err := s.DeviceSigningKey(ctx)
	if err != nil {
		return ontology.DeviceEnrolledEvent{}, fmt.Errorf("identity: enroll: device signing key: %w", err)
	}
	devPub, ok := devPriv.Public().(ed25519.PublicKey)
	if !ok {
		return ontology.DeviceEnrolledEvent{}, errors.New("identity: enroll: unexpected public key type")
	}
	devPubB64 := base64.StdEncoding.EncodeToString(devPub)
	sigKeyID := SigKeyID(devPub)

	// Self-signature: the device signs its own enrollment with its active key.
	selfSig := ed25519.Sign(devPriv, ontology.DeviceEnrollmentBytes(deviceID, principalUID, devPubB64, sigKeyID))

	ev := ontology.DeviceEnrolledEvent{
		DeviceID:      deviceID,
		PrincipalUID:  principalUID,
		SignPublicKey: devPubB64,
		SigKeyID:      sigKeyID,
		DeviceLabel:   deviceLabel,
		Signature:     base64.StdEncoding.EncodeToString(selfSig),
		CreateTime:    time.Now().UTC(),
	}

	// Principal vouch: only for subkey devices. Device 0 uses the same key for
	// both the principal identity and the device identity, so a separate vouch
	// would be self-redundant (and absent vouch fields = legacy device is valid).
	hasSubkey, err := s.HasDeviceSubkey(ctx)
	if err != nil {
		return ontology.DeviceEnrolledEvent{}, fmt.Errorf("identity: enroll: check subkey: %w", err)
	}
	if hasSubkey {
		principalPriv, err := s.SigningKey(ctx)
		if err != nil {
			return ontology.DeviceEnrolledEvent{}, fmt.Errorf("identity: enroll: principal signing key: %w", err)
		}
		principalPub, ok := principalPriv.Public().(ed25519.PublicKey)
		if !ok {
			return ontology.DeviceEnrolledEvent{}, errors.New("identity: enroll: unexpected principal public key type")
		}
		vouch := ed25519.Sign(principalPriv, ontology.DeviceVouchBytes(deviceID, devPubB64))
		ev.VouchSigKeyID = SigKeyID(principalPub)
		ev.VouchSignature = base64.StdEncoding.EncodeToString(vouch)
	}
	return ev, nil
}

// DeviceKeyLookup resolves an enrolled device's ed25519 public key by its
// sigKeyID, scoped to the principal that key id must belong to. The dispatcher's
// enrolled-device projection (the `devices` table populated by deviceEnrolled
// events and grant vouches) satisfies it. Binding the lookup to principalUID is
// security-load-bearing: it forbids one principal from signing events that claim
// authorship by another, even if that signer's own key is enrolled.
type DeviceKeyLookup interface {
	DevicePublicKey(ctx context.Context, sigKeyID, principalUID string) (ed25519.PublicKey, error)
}

// Verifier implements kernel.Verifier: it resolves the enrolled public key
// for an event's sigKeyID and checks the ed25519 signature over the
// event's canonical bytes. Installed on the event log via SetVerifier and
// consulted only for team-scope events during Rebuild.
type Verifier struct {
	lookup DeviceKeyLookup
}

// NewVerifier returns a kernel.Verifier backed by the given device-key
// lookup.
func NewVerifier(lookup DeviceKeyLookup) *Verifier {
	return &Verifier{lookup: lookup}
}

// Verify checks ev's signature against the enrolled key selected by
// ev.Author.SigKeyID. It returns an error wrapping kernel.ErrSignatureInvalid
// on any failure (missing key, malformed key, or a signature that does not
// verify) so Rebuild records a single, branchable ApplyError.
func (v *Verifier) Verify(ctx context.Context, ev *kernel.Event) error {
	if v == nil || v.lookup == nil {
		return fmt.Errorf("%w: no device-key lookup configured", kernel.ErrSignatureInvalid)
	}
	// A genesis-style self-grant carries the signing key it is signed with —
	// the team's TOFU root, which no prior devices row vouches for. Verify it
	// against the embedded key; every other event resolves its key from the
	// devices table (populated as those self-grants and grants apply).
	//
	// TOFU is strictly BOOTSTRAP-only: the embedded key is trusted only while
	// no key is yet enrolled under ev.Author.SigKeyID. Once a key IS enrolled
	// there, a self-grant must verify against that enrolled key — a self-grant
	// can never silently rotate the key out from under an already-known id, so
	// no actor can author events impersonating an enrolled member by embedding
	// their own key in a self-grant under the victim's sig key id.
	if pub, ok, err := selfVouchKey(ev); err != nil {
		return err
	} else if ok {
		enrolled, lerr := v.lookup.DevicePublicKey(ctx, ev.Author.SigKeyID, ev.Author.PrincipalUID)
		switch {
		case lerr == nil:
			// A key IS enrolled under this sig key id: the enrolled key wins, so
			// a self-grant can never rotate the key out from under an existing id.
			return verifyAuthorSig(enrolled, ev)
		case errors.Is(lerr, kernel.ErrDeviceNotEnrolled):
			// Genesis bootstrap: nothing enrolled yet under this sig key id, so
			// trust the embedded TOFU key. This is the ONLY branch that may use
			// the embedded key.
			return verifyAuthorSig(pub, ev)
		default:
			// A storage fault must fail closed — never silently downgrade trust
			// to the embedded key just because the lookup transiently errored.
			return fmt.Errorf("%w: self-vouch lookup %q for %q: %w",
				kernel.ErrSignatureInvalid, ev.Author.SigKeyID, ev.Author.PrincipalUID, lerr)
		}
	}
	pub, err := v.lookup.DevicePublicKey(ctx, ev.Author.SigKeyID, ev.Author.PrincipalUID)
	if err != nil {
		return fmt.Errorf("%w: lookup key %q for %q: %w", kernel.ErrSignatureInvalid, ev.Author.SigKeyID, ev.Author.PrincipalUID, err)
	}
	return verifyAuthorSig(pub, ev)
}

// verifyAuthorSig checks ev.Author.Sig against pub over the canonical bytes.
// Guards the key size first: ed25519.Verify panics on a wrong-sized key, so a
// corrupt enrolled row or a malformed self-vouched key surfaces as a
// verification failure rather than a crash.
func verifyAuthorSig(pub ed25519.PublicKey, ev *kernel.Event) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: key for %q has wrong size %d", kernel.ErrSignatureInvalid, ev.Author.SigKeyID, len(pub))
	}
	if !ed25519.Verify(pub, ev.CanonicalBytes(), ev.Author.Sig) {
		return fmt.Errorf("%w: event %s", kernel.ErrSignatureInvalid, ev.UID)
	}
	return nil
}

// selfVouchKey returns the embedded signing key for a self-vouching event, or
// ok=false for events verified via the normal devices-table lookup. Two
// shapes self-vouch:
//
//   - A team grant whose grantee IS its author: the team has no prior
//     knowledge of that principal's key, so the grant publishes it and is
//     trusted at TOFU level — the same trust the genesis self-grant already
//     roots the whole ACL with.
//   - A principal-vouched deviceEnrolled signed by the key it enrolls: a
//     member's NEW subkey device introduces itself to teammates through the
//     team log, and no devices row can exist yet for a key that is being
//     introduced. The embedded key only bootstraps the signature check; the
//     projection layer independently verifies the principal vouch before any
//     trust attaches, and a vouchless team-scoped enrollment is rejected
//     there outright.
//
// The carried key id is bound to the author's sig key id so an event cannot
// self-vouch under one id while signing under another. Everything else
// returns ok=false and falls through to the lookup path (where the key must
// already be enrolled).
func selfVouchKey(ev *kernel.Event) (ed25519.PublicKey, bool, error) {
	switch string(ev.Kind) {
	case ontology.KindTeamMemberGranted:
		return selfGrantVouchKey(ev)
	case ontology.KindDeviceEnrolled:
		return enrollmentVouchKey(ev)
	default:
		return nil, false, nil
	}
}

// selfGrantVouchKey extracts the embedded key from a self-vouching team grant
// (grantee IS the author); see selfVouchKey for the trust rationale.
func selfGrantVouchKey(ev *kernel.Event) (ed25519.PublicKey, bool, error) {
	var g ontology.TeamMemberGrantedEvent
	if err := json.Unmarshal(ev.Payload, &g); err != nil {
		return nil, false, nil //nolint:nilerr // unparseable grant → let the lookup path reject it
	}
	if g.GranteePrincipalUID == "" || g.GranteePrincipalUID != ev.Author.PrincipalUID || g.GranteeSignPublicKey == "" {
		return nil, false, nil
	}
	if g.GranteeSigKeyID != "" && g.GranteeSigKeyID != ev.Author.SigKeyID {
		return nil, false, fmt.Errorf("%w: self-grant sigKeyId %q disagrees with author %q",
			kernel.ErrSignatureInvalid, g.GranteeSigKeyID, ev.Author.SigKeyID)
	}
	return decodeEmbeddedKey(g.GranteeSignPublicKey, "self-grant")
}

// enrollmentVouchKey extracts the embedded key from a principal-vouched
// deviceEnrolled signed by the key it enrolls; see selfVouchKey for the trust
// rationale. Only a vouched enrollment authored by the principal it enrolls
// may introduce its own key, and the enrolled key id must be the id the
// event is signed under.
func enrollmentVouchKey(ev *kernel.Event) (ed25519.PublicKey, bool, error) {
	var d ontology.DeviceEnrolledEvent
	if err := json.Unmarshal(ev.Payload, &d); err != nil {
		return nil, false, nil //nolint:nilerr // unparseable enrollment → let the lookup path reject it
	}
	if d.VouchSignature == "" || d.SignPublicKey == "" ||
		d.PrincipalUID == "" || d.PrincipalUID != ev.Author.PrincipalUID {
		return nil, false, nil
	}
	if d.SigKeyID != ev.Author.SigKeyID {
		return nil, false, fmt.Errorf("%w: enrollment sigKeyId %q disagrees with author %q",
			kernel.ErrSignatureInvalid, d.SigKeyID, ev.Author.SigKeyID)
	}
	pub, ok, err := decodeEmbeddedKey(d.SignPublicKey, "enrollment")
	if err != nil {
		return nil, false, err
	}
	// Bind the embedded key to the claimed id: the id is what the devices
	// table will index the key under, so a mismatch means the event signs
	// under an id its key cannot own.
	if ok && SigKeyID(pub) != d.SigKeyID {
		return nil, false, fmt.Errorf("%w: enrollment key does not hash to sigKeyId %q",
			kernel.ErrSignatureInvalid, d.SigKeyID)
	}
	return pub, ok, nil
}

// decodeEmbeddedKey decodes a base64 embedded signing key for the self-vouch
// paths, wrapping decode failures as signature errors.
func decodeEmbeddedKey(b64, what string) (ed25519.PublicKey, bool, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, false, fmt.Errorf("%w: decode %s key: %w", kernel.ErrSignatureInvalid, what, err)
	}
	return ed25519.PublicKey(raw), true, nil
}
