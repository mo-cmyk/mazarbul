package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

type enrolledDevice struct {
	principal string
	key       ed25519.PublicKey
}

type fakeDeviceLookup struct {
	keys     map[string]enrolledDevice
	forceErr error // when set, every lookup returns this (simulates a storage fault)
}

// DevicePublicKey returns the enrolled key only when sigKeyID is enrolled AND
// it belongs to principalUID — mirroring the dispatcher's principal-scoped
// lookup, so a sig key id cannot verify events that claim a different principal.
// A clean miss returns kernel.ErrDeviceNotEnrolled (the dispatcher's contract);
// forceErr injects a non-sentinel fault to exercise the fail-closed path.
func (f fakeDeviceLookup) DevicePublicKey(_ context.Context, sigKeyID, principalUID string) (ed25519.PublicKey, error) {
	if f.forceErr != nil {
		return nil, f.forceErr
	}
	if d, ok := f.keys[sigKeyID]; ok && d.principal == principalUID {
		return d.key, nil
	}
	return nil, kernel.ErrDeviceNotEnrolled
}

// signedSelfGrant builds a teamMemberGranted whose grantee IS its author (the
// self-vouch shape), embedding embedKey as the vouched signing key and signing
// the canonical bytes with signWith. A real bootstrap passes signWith's public
// half as embedKey; an attacker passes their own key as both.
func signedSelfGrant(t *testing.T, principal string, signWith ed25519.PrivateKey, embedKey ed25519.PublicKey, sigKeyID string) *kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.TeamMemberGrantedEvent{
		GranteePrincipalUID:  principal,
		GranteeSignPublicKey: base64.StdEncoding.EncodeToString(embedKey),
		GranteeSigKeyID:      sigKeyID,
		GranteeDeviceID:      principal + "-dev",
	})
	if err != nil {
		t.Fatalf("marshal self-grant: %v", err)
	}
	ev := &kernel.Event{
		UID:     "01EVSELFGRANT000000000001",
		Kind:    kernel.EventKind(ontology.KindTeamMemberGranted),
		Scope:   kernel.ScopeTeam,
		Payload: payload,
		Author:  kernel.EventAuthor{PrincipalUID: principal, DeviceID: principal + "-dev", SigKeyID: sigKeyID},
	}
	ev.Author.Sig = ed25519.Sign(signWith, ev.CanonicalBytes())
	return ev
}

// TestVerifier_SelfVouchIsBootstrapOnly pins the hardening that closes the
// self-grant impersonation vector: a self-grant's embedded key is trusted via
// TOFU ONLY while no key is yet enrolled under its sig key id (genesis
// bootstrap). Once a key IS enrolled there, the self-grant must verify against
// that enrolled key — an actor cannot author events under an enrolled member's
// sig key id by embedding their own key in a self-grant.
func TestVerifier_SelfVouchIsBootstrapOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	realPub, realPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	kid := SigKeyID(realPub)

	t.Run("self-vouch trusted when not yet enrolled", func(t *testing.T) {
		t.Parallel()
		v := NewVerifier(fakeDeviceLookup{keys: map[string]enrolledDevice{}})
		ev := signedSelfGrant(t, "G", realPriv, realPub, kid)
		if verr := v.Verify(ctx, ev); verr != nil {
			t.Errorf("genesis bootstrap self-vouch must verify: %v", verr)
		}
	})

	t.Run("enrolled key wins so a foreign-key self-grant is rejected", func(t *testing.T) {
		t.Parallel()
		v := NewVerifier(fakeDeviceLookup{keys: map[string]enrolledDevice{kid: {principal: "G", key: realPub}}})
		_, attackerPriv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			t.Fatalf("genkey: %v", gerr)
		}
		attackerPub, _ := attackerPriv.Public().(ed25519.PublicKey)
		// Attacker claims the enrolled kid but embeds + signs with their own key.
		ev := signedSelfGrant(t, "G", attackerPriv, attackerPub, kid)
		if verr := v.Verify(ctx, ev); !errors.Is(verr, kernel.ErrSignatureInvalid) {
			t.Errorf("self-grant under an enrolled id with a foreign key must fail: %v", verr)
		}
	})

	t.Run("enrolled key verifies a legit same-key self-grant", func(t *testing.T) {
		t.Parallel()
		v := NewVerifier(fakeDeviceLookup{keys: map[string]enrolledDevice{kid: {principal: "G", key: realPub}}})
		ev := signedSelfGrant(t, "G", realPriv, realPub, kid)
		if verr := v.Verify(ctx, ev); verr != nil {
			t.Errorf("legit self-grant signed with the enrolled key must verify: %v", verr)
		}
	})

	t.Run("a sig key id cannot author as a different principal", func(t *testing.T) {
		t.Parallel()
		// kid is enrolled for principal "G". An event signed with G's key but
		// CLAIMING authorship by "Alice" must not verify — the lookup is scoped
		// to the claimed principal, so (kid, Alice) resolves to nothing.
		v := NewVerifier(fakeDeviceLookup{keys: map[string]enrolledDevice{kid: {principal: "G", key: realPub}}})
		ev := &kernel.Event{
			UID:     "01EVIMPERSONATE0000000001",
			Kind:    "fact",
			Scope:   kernel.ScopeTeam,
			Payload: []byte(`{}`),
			Author:  kernel.EventAuthor{PrincipalUID: "Alice", DeviceID: "G-dev", SigKeyID: kid},
		}
		ev.Author.Sig = ed25519.Sign(realPriv, ev.CanonicalBytes())
		if verr := v.Verify(ctx, ev); !errors.Is(verr, kernel.ErrSignatureInvalid) {
			t.Errorf("G's key authoring as Alice must fail: %v", verr)
		}
	})

	t.Run("a storage fault during self-vouch lookup fails closed", func(t *testing.T) {
		t.Parallel()
		// The lookup errors transiently (NOT the not-enrolled sentinel). The TOFU
		// fallback must NOT fire: a self-grant embedding a foreign key under an id
		// that may well be enrolled cannot be trusted just because the store is
		// degraded. Without the sentinel discrimination this silently downgrades
		// trust to the embedded key — the vulnerability this guards.
		v := NewVerifier(fakeDeviceLookup{forceErr: errors.New("disk on fire")})
		_, attackerPriv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			t.Fatalf("genkey: %v", gerr)
		}
		attackerPub, _ := attackerPriv.Public().(ed25519.PublicKey)
		ev := signedSelfGrant(t, "G", attackerPriv, attackerPub, kid)
		if verr := v.Verify(ctx, ev); !errors.Is(verr, kernel.ErrSignatureInvalid) {
			t.Errorf("a storage fault must fail closed with ErrSignatureInvalid, got: %v", verr)
		}
	})
}

// signedEnrollment builds a deviceEnrolled event signed by signWith and
// embedding embedKey, claiming sigKeyID as the enrolled key id. withVouch
// controls the VouchSignature field: the verifier only checks its PRESENCE
// (validity is the apply layer's job), so any non-empty value exercises the
// self-vouch arm.
func signedEnrollment(t *testing.T, principal string, signWith ed25519.PrivateKey, embedKey ed25519.PublicKey, sigKeyID string, withVouch bool) *kernel.Event {
	t.Helper()
	dev := ontology.DeviceEnrolledEvent{
		DeviceID:      principal + "-dev2",
		PrincipalUID:  principal,
		SignPublicKey: base64.StdEncoding.EncodeToString(embedKey),
		SigKeyID:      sigKeyID,
		Signature:     base64.StdEncoding.EncodeToString(ed25519.Sign(signWith, []byte("self-sig-placeholder"))),
	}
	if withVouch {
		dev.VouchSigKeyID = "kid:vouching-root"
		dev.VouchSignature = base64.StdEncoding.EncodeToString(ed25519.Sign(signWith, []byte("vouch-placeholder")))
	}
	payload, err := json.Marshal(dev)
	if err != nil {
		t.Fatalf("marshal enrollment: %v", err)
	}
	ev := &kernel.Event{
		UID:     "01EVENROLLINTRO0000000001",
		Kind:    kernel.EventKind(ontology.KindDeviceEnrolled),
		Scope:   kernel.ScopeTeam,
		Payload: payload,
		Author:  kernel.EventAuthor{PrincipalUID: principal, DeviceID: principal + "-dev2", SigKeyID: sigKeyID},
	}
	ev.Author.Sig = ed25519.Sign(signWith, ev.CanonicalBytes())
	return ev
}

// TestVerifier_DeviceEnrolledSelfVouch pins the subkey-introduction path: a
// principal-vouched deviceEnrolled signed by the key it enrolls verifies via
// its embedded key while that key is unknown (the only way a member's new
// device can introduce itself to teammates through the team log), with the
// same bootstrap-only and key-binding hardening the self-grant path has.
func TestVerifier_DeviceEnrolledSelfVouch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	subPub, subPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	kid := SigKeyID(subPub)

	t.Run("vouched enrollment from an unknown key verifies", func(t *testing.T) {
		t.Parallel()
		v := NewVerifier(fakeDeviceLookup{keys: map[string]enrolledDevice{}})
		ev := signedEnrollment(t, "G", subPriv, subPub, kid, true)
		if verr := v.Verify(ctx, ev); verr != nil {
			t.Errorf("vouched subkey introduction must verify: %v", verr)
		}
	})

	t.Run("vouchless enrollment falls through to the lookup and is rejected", func(t *testing.T) {
		t.Parallel()
		v := NewVerifier(fakeDeviceLookup{keys: map[string]enrolledDevice{}})
		ev := signedEnrollment(t, "G", subPriv, subPub, kid, false)
		if verr := v.Verify(ctx, ev); !errors.Is(verr, kernel.ErrSignatureInvalid) {
			t.Errorf("vouchless enrollment from an unknown key must fail: %v", verr)
		}
	})

	t.Run("embedded key must hash to the claimed sig key id", func(t *testing.T) {
		t.Parallel()
		v := NewVerifier(fakeDeviceLookup{keys: map[string]enrolledDevice{}})
		// Claim a kid the embedded key does not hash to: without the binding an
		// attacker could park their key under an arbitrary id.
		ev := signedEnrollment(t, "G", subPriv, subPub, "kid:someoneelse", true)
		if verr := v.Verify(ctx, ev); !errors.Is(verr, kernel.ErrSignatureInvalid) {
			t.Errorf("kid-mismatched enrollment must fail: %v", verr)
		}
	})

	t.Run("enrolled key wins over the embedded key", func(t *testing.T) {
		t.Parallel()
		// kid already enrolled with the REAL key; an attacker re-introduces the
		// id with their own key. The enrolled key must win, so the attacker's
		// signature fails.
		v := NewVerifier(fakeDeviceLookup{keys: map[string]enrolledDevice{kid: {principal: "G", key: subPub}}})
		_, attackerPriv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			t.Fatalf("genkey: %v", gerr)
		}
		attackerPub, _ := attackerPriv.Public().(ed25519.PublicKey)
		ev := signedEnrollment(t, "G", attackerPriv, attackerPub, kid, true)
		if verr := v.Verify(ctx, ev); !errors.Is(verr, kernel.ErrSignatureInvalid) {
			t.Errorf("enrollment under an enrolled id with a foreign key must fail: %v", verr)
		}
	})

	t.Run("author principal must match the enrolled principal", func(t *testing.T) {
		t.Parallel()
		v := NewVerifier(fakeDeviceLookup{keys: map[string]enrolledDevice{}})
		ev := signedEnrollment(t, "G", subPriv, subPub, kid, true)
		ev.Author.PrincipalUID = "Mallory"
		ev.Author.Sig = ed25519.Sign(subPriv, ev.CanonicalBytes())
		if verr := v.Verify(ctx, ev); !errors.Is(verr, kernel.ErrSignatureInvalid) {
			t.Errorf("cross-principal enrollment introduction must fail: %v", verr)
		}
	})
}
