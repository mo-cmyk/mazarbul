package kernel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

// stubRoleChecker implements kernel.TeamRoleChecker for graft tests. It
// records the position each verdict was requested at and reports one fixed
// role for every principal (role "" means not admitted at that position).
type stubRoleChecker struct {
	role      string
	askedUIDs []string
}

func (s *stubRoleChecker) TeamMemberRoleAt(_ context.Context, _, beforeEventUID string) (string, bool, error) {
	s.askedUIDs = append(s.askedUIDs, beforeEventUID)
	if s.role == "" {
		return "", false, nil
	}
	return s.role, true, nil
}

// stubRevokeChecker implements kernel.DeviceRevokeChecker for graft tests. It
// records the position each verdict was requested at and reports one fixed
// verdict for every device.
type stubRevokeChecker struct {
	revoked   bool
	askedUIDs []string
}

func (s *stubRevokeChecker) DeviceRevokedAt(_ context.Context, _, _, beforeEventUID string) (bool, error) {
	s.askedUIDs = append(s.askedUIDs, beforeEventUID)
	return s.revoked, nil
}

// signedTeamEvents appends count team-scope events through a producer log (which
// signs them with the fixed test key) and reads them back fully signed — the
// on-wire shape a sealed chunk would carry to a peer.
func signedTeamEvents(t *testing.T, count int) ([]kernel.Event, []byte) {
	t.Helper()
	ctx := testutil.TestContext(t)
	priv, pub := fixedTestKey(t)
	producer := newTestWriter(t, kernel.WithSigner(
		kernel.NewEd25519Signer(priv, "01PRINCIPAL000000000000001", "01DEVICE0000000000000000001", "kid:test")))

	batch := make([]kernel.Event, count)
	for i := range batch {
		batch[i] = sampleEventWithScope(kernel.ScopeTeam)
	}
	receipts, err := producer.Append(ctx, batch)
	if err != nil {
		t.Fatalf("producer Append: %v", err)
	}
	signed := make([]kernel.Event, len(receipts))
	for i, r := range receipts {
		ev, aerr := producer.At(ctx, r.UID)
		if aerr != nil {
			t.Fatalf("producer At %s: %v", r.UID, aerr)
		}
		signed[i] = ev
	}
	return signed, pub
}

func TestGraftSealed_PreservesAuthorAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	signed, pub := signedTeamEvents(t, 2)

	// Consumer is a SEPARATE log with its own (local-default) signer plus the
	// producer's verifier. The graft must persist the events verbatim without
	// re-signing them under the consumer's identity.
	consumer := newTestWriter(t, kernel.WithVerifier(testVerifier{pub: pub}))

	res, err := consumer.GraftSealed(ctx, signed)
	if err != nil {
		t.Fatalf("GraftSealed: %v", err)
	}
	if res.Grafted != 2 || res.Skipped != 0 || len(res.Rejections) != 0 {
		t.Fatalf("first graft: got grafted=%d skipped=%d rejected=%d, want 2/0/0",
			res.Grafted, res.Skipped, len(res.Rejections))
	}

	// Verbatim preservation: UID + the producer's signature survive unchanged.
	for _, want := range signed {
		got, aerr := consumer.At(ctx, want.UID)
		if aerr != nil {
			t.Fatalf("consumer At %s: %v", want.UID, aerr)
		}
		if got.UID != want.UID {
			t.Errorf("UID changed: got %q want %q", got.UID, want.UID)
		}
		if got.Author.PrincipalUID != "01PRINCIPAL000000000000001" {
			t.Errorf("PrincipalUID re-stamped: got %q", got.Author.PrincipalUID)
		}
		if !bytes.Equal(got.Author.Sig, want.Author.Sig) {
			t.Errorf("signature re-stamped for %s: graft must preserve the original sig", want.UID)
		}
	}

	// Idempotency: re-grafting the same batch persists nothing new.
	res2, err := consumer.GraftSealed(ctx, signed)
	if err != nil {
		t.Fatalf("second GraftSealed: %v", err)
	}
	if res2.Grafted != 0 || res2.Skipped != 2 {
		t.Fatalf("second graft: got grafted=%d skipped=%d, want 0/2 (idempotent)",
			res2.Grafted, res2.Skipped)
	}
}

func TestGraftSealed_RejectsTamperedTeamEvent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	signed, pub := signedTeamEvents(t, 1)

	// Tamper the payload after signing: the canonical bytes change, so the
	// preserved signature no longer verifies.
	tampered := signed[0]
	tampered.Payload = json.RawMessage(`{"text":"forged"}`)

	consumer := newTestWriter(t, kernel.WithVerifier(testVerifier{pub: pub}))
	res, err := consumer.GraftSealed(ctx, []kernel.Event{tampered})
	if err != nil {
		t.Fatalf("GraftSealed: %v", err)
	}
	if res.Grafted != 0 || len(res.Rejections) != 1 {
		t.Fatalf("got grafted=%d rejected=%d, want 0/1 (bad sig must be rejected)",
			res.Grafted, len(res.Rejections))
	}
	if _, aerr := consumer.At(ctx, tampered.UID); aerr == nil {
		t.Errorf("a rejected event must not be persisted, but At found %s", tampered.UID)
	}
}

func TestGraftSealed_NonTeamSkipsVerification(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	// A consumer with NO verifier still grafts personal-scope events: the
	// signature gate is team-scope only (the cross-device trust boundary).
	consumer := newTestWriter(t)

	ev := sampleEventWithScope(kernel.ScopePersonal)
	ev.UID = "01HZX0000000000000000GRAF1"
	ev.CreateTime = authorClock

	res, err := consumer.GraftSealed(ctx, []kernel.Event{ev})
	if err != nil {
		t.Fatalf("GraftSealed: %v", err)
	}
	if res.Grafted != 1 {
		t.Fatalf("got grafted=%d, want 1 (personal scope grafts without a verifier)", res.Grafted)
	}
	if _, aerr := consumer.At(ctx, ev.UID); aerr != nil {
		t.Errorf("grafted personal event not found: %v", aerr)
	}
}

func TestGraftSealed_ReaderContentRejectedAtItsOwnPosition(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	signed, pub := signedTeamEvents(t, 1)

	// A reader-authored content event is dropped, and the role verdict must
	// be requested at the event's OWN position (beforeEventUID == ev.UID) —
	// that restriction is what keeps graft and replay verdicts identical when
	// a demote or revoke post-dates the event.
	checker := &stubRoleChecker{role: "reader"}
	consumer := newTestWriter(t, kernel.WithVerifier(testVerifier{pub: pub}))
	consumer.SetTeamRoleChecker(checker)

	res, err := consumer.GraftSealed(ctx, signed)
	if err != nil {
		t.Fatalf("GraftSealed: %v", err)
	}
	if res.Grafted != 0 || len(res.Rejections) != 1 {
		t.Fatalf("got grafted=%d rejected=%d, want 0/1 (reader content must be rejected)",
			res.Grafted, len(res.Rejections))
	}
	if !errors.Is(res.Rejections[0].Err, kernel.ErrPermission) {
		t.Errorf("rejection error = %v, want kernel.ErrPermission", res.Rejections[0].Err)
	}
	if len(checker.askedUIDs) != 1 || checker.askedUIDs[0] != signed[0].UID {
		t.Errorf("role verdict positions = %v, want exactly [%s] (the event's own UID)",
			checker.askedUIDs, signed[0].UID)
	}
	if _, aerr := consumer.At(ctx, signed[0].UID); aerr == nil {
		t.Errorf("a rejected event must not be persisted, but At found %s", signed[0].UID)
	}
}

func TestGraftSealed_RevokedDeviceRejectedAtItsOwnPosition(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	signed, pub := signedTeamEvents(t, 1)

	// An event authored by a revoked device is dropped, and the revocation
	// verdict must be requested at the event's OWN position (beforeEventUID ==
	// ev.UID) — a revocation that post-dates the event can never flip its
	// verdict, whatever the arrival order.
	checker := &stubRevokeChecker{revoked: true}
	consumer := newTestWriter(t, kernel.WithVerifier(testVerifier{pub: pub}))
	consumer.SetDeviceRevokeChecker(checker)

	res, err := consumer.GraftSealed(ctx, signed)
	if err != nil {
		t.Fatalf("GraftSealed: %v", err)
	}
	if res.Grafted != 0 || len(res.Rejections) != 1 {
		t.Fatalf("got grafted=%d rejected=%d, want 0/1 (revoked-device event must be rejected)",
			res.Grafted, len(res.Rejections))
	}
	if !errors.Is(res.Rejections[0].Err, kernel.ErrPermission) {
		t.Errorf("rejection error = %v, want kernel.ErrPermission", res.Rejections[0].Err)
	}
	if len(checker.askedUIDs) != 1 || checker.askedUIDs[0] != signed[0].UID {
		t.Errorf("revocation verdict positions = %v, want exactly [%s] (the event's own UID)",
			checker.askedUIDs, signed[0].UID)
	}
	if _, aerr := consumer.At(ctx, signed[0].UID); aerr == nil {
		t.Errorf("a rejected event must not be persisted, but At found %s", signed[0].UID)
	}
}

func TestGraftSealed_DeviceRevocationGateCoversMembershipKinds(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	priv, pub := fixedTestKey(t)

	// Membership kinds are exempt from the reader-role gate but NOT from the
	// device-revocation gate: a revoked (stolen) owner/admin device must not
	// be able to author grants, role changes, or revenge-revocations.
	for _, tc := range []struct {
		name        string
		revoked     bool
		wantGrafted int
	}{
		{name: "revoked device grant is rejected", revoked: true, wantGrafted: 0},
		{name: "non revoked device grant grafts", revoked: false, wantGrafted: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			producer := newTestWriter(t, kernel.WithSigner(
				kernel.NewEd25519Signer(priv, "01PRINCIPAL000000000000001", "01DEVICE0000000000000000001", "kid:test")))
			ev := sampleEventWithScope(kernel.ScopeTeam)
			ev.Kind = "teamMemberGranted"
			receipts, err := producer.Append(ctx, []kernel.Event{ev})
			if err != nil {
				t.Fatalf("producer Append: %v", err)
			}
			signed, aerr := producer.At(ctx, receipts[0].UID)
			if aerr != nil {
				t.Fatalf("producer At: %v", aerr)
			}

			checker := &stubRevokeChecker{revoked: tc.revoked}
			consumer := newTestWriter(t, kernel.WithVerifier(testVerifier{pub: pub}))
			consumer.SetDeviceRevokeChecker(checker)

			res, gerr := consumer.GraftSealed(ctx, []kernel.Event{signed})
			if gerr != nil {
				t.Fatalf("GraftSealed: %v", gerr)
			}
			if res.Grafted != tc.wantGrafted {
				t.Fatalf("got grafted=%d rejected=%d, want grafted=%d",
					res.Grafted, len(res.Rejections), tc.wantGrafted)
			}
			if tc.revoked && !errors.Is(res.Rejections[0].Err, kernel.ErrPermission) {
				t.Errorf("rejection error = %v, want kernel.ErrPermission", res.Rejections[0].Err)
			}
		})
	}
}

func TestGraftSealed_MemberContentGraftsWithRoleChecker(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	signed, pub := signedTeamEvents(t, 1)

	// The same event grafts cleanly when the author held member at its
	// position, and an author unknown at that position (ok=false) is left to
	// the signature/admission machinery rather than role-rejected.
	for _, tc := range []struct {
		name string
		role string
	}{
		{name: "member role grafts", role: "member"},
		{name: "unknown principal passes the role gate", role: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := &stubRoleChecker{role: tc.role}
			consumer := newTestWriter(t, kernel.WithVerifier(testVerifier{pub: pub}))
			consumer.SetTeamRoleChecker(checker)

			res, err := consumer.GraftSealed(ctx, signed)
			if err != nil {
				t.Fatalf("GraftSealed: %v", err)
			}
			if res.Grafted != 1 || len(res.Rejections) != 0 {
				t.Fatalf("got grafted=%d rejected=%d, want 1/0", res.Grafted, len(res.Rejections))
			}
		})
	}
}
