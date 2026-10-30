package dispatcher_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

const (
	testPrincipalUID = "01PRINCIPAL000000000000001"
	testDeviceUID    = "01DEVICE0000000000000000001"
	testSigKeyID     = "kid:dispatcher-identity-test"
)

// identityTestKey returns a deterministic ed25519 public key + its base64 form
// for use as a device/principal signing key in the projection tests.
func identityTestKey(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	seed := sha256.Sum256([]byte("mazarbul:dispatcher:identity-test"))
	pub := ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
	return pub, base64.StdEncoding.EncodeToString(pub)
}

func principalEvent(t *testing.T, signPubB64 string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.PrincipalRegisteredEvent{
		PrincipalUID:  testPrincipalUID,
		DisplayName:   "octocat",
		SignPublicKey: signPubB64,
		WrapPublicKey: "d3JhcC1rZXk=", // arbitrary base64; not validated by the projection
		SigKeyID:      testSigKeyID,
		CreateTime:    time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		SameAs:        "github:octocat",
		GitHubUserID:  583231,
		GitHubAnchor:  ontology.GitHubAnchorNone,
	})
	if err != nil {
		t.Fatalf("marshal principal: %v", err)
	}
	return kernel.Event{
		UID:        "01EVPRIN00000000000000001",
		Kind:       kernel.EventKind(ontology.KindPrincipalRegistered),
		Source:     "genesis",
		Confidence: 1,
		Payload:    payload,
	}
}

func deviceEvent(t *testing.T, signPubB64 string) kernel.Event {
	t.Helper()
	// Self-sign the enrollment with the device's own key (the TOFU root the
	// genesis ceremony produces) so applyDeviceEnrolled's signature check passes.
	seed := sha256.Sum256([]byte("mazarbul:dispatcher:identity-test"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	sig := ed25519.Sign(priv,
		ontology.DeviceEnrollmentBytes(testDeviceUID, testPrincipalUID, signPubB64, testSigKeyID))
	payload, err := json.Marshal(ontology.DeviceEnrolledEvent{
		DeviceID:      testDeviceUID,
		PrincipalUID:  testPrincipalUID,
		SignPublicKey: signPubB64,
		SigKeyID:      testSigKeyID,
		DeviceLabel:   "laptop",
		Signature:     base64.StdEncoding.EncodeToString(sig),
		CreateTime:    time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal device: %v", err)
	}
	return kernel.Event{
		UID:        "01EVDEV000000000000000001",
		Kind:       kernel.EventKind(ontology.KindDeviceEnrolled),
		Source:     "genesis",
		Confidence: 1,
		Payload:    payload,
		// A real deviceEnrolled is authored by the principal it enrolls; the
		// apply handler enforces that match.
		Author: kernel.EventAuthor{PrincipalUID: testPrincipalUID, DeviceID: testDeviceUID, SigKeyID: testSigKeyID},
	}
}

// TestApply_PrincipalRegistered_MaterializesRowAndEntity verifies the
// dispatcher writer for principalRegistered upserts BOTH the principals row
// (with the GitHub anchor columns) AND a resolved person entity, so the
// principal is a first-class subject for facts/relations.
func TestApply_PrincipalRegistered_MaterializesRowAndEntity(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	_, signPubB64 := identityTestKey(t)

	if err := d.Apply(ctx, []kernel.Event{principalEvent(t, signPubB64)}); err != nil {
		t.Fatalf("Apply principalRegistered: %v", err)
	}

	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}

	var (
		displayName  string
		sameAs       string
		githubUserID int64
		anchor       string
	)
	if qerr := reader.QueryRowContext(ctx,
		`SELECT display_name, github_login, github_user_id, github_anchor
		   FROM principals WHERE principal_uid = ?`, testPrincipalUID).
		Scan(&displayName, &sameAs, &githubUserID, &anchor); qerr != nil {
		t.Fatalf("query principals: %v", qerr)
	}
	if displayName != "octocat" || sameAs != "github:octocat" || githubUserID != 583231 {
		t.Errorf("principals row = (%q, %q, %d), want (octocat, github:octocat, 583231)", displayName, sameAs, githubUserID)
	}
	if anchor != ontology.GitHubAnchorNone {
		t.Errorf("github_anchor = %q, want %q", anchor, ontology.GitHubAnchorNone)
	}

	// The principal must also exist as a resolved person entity.
	var (
		kind   string
		status string
	)
	if qerr := reader.QueryRowContext(ctx,
		`SELECT kind, resolution_status FROM entities WHERE uid = ?`, testPrincipalUID).
		Scan(&kind, &status); qerr != nil {
		t.Fatalf("query entities: %v", qerr)
	}
	if kind != "person" || status != "resolved" {
		t.Errorf("principal entity = (%q, %q), want (person, resolved)", kind, status)
	}
}

// TestPrincipalGitIdentity_StripsGitHubAnchorPrefix is the regression for the
// no-reply-email corruption: github_login stores the anchor "github:<login>",
// so PrincipalGitIdentity must hand back the BARE login. Synthesizing the email
// from the raw anchor would yield "<id>+githuboctocat@..." (the ':' dropped by
// SanitizeGitHubLogin), which does not attribute on GitHub.
func TestPrincipalGitIdentity_StripsGitHubAnchorPrefix(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	_, signPubB64 := identityTestKey(t)
	if err := d.Apply(ctx, []kernel.Event{principalEvent(t, signPubB64)}); err != nil {
		t.Fatalf("Apply principalRegistered: %v", err)
	}

	login, userID, displayName, ok, err := d.PrincipalGitIdentity(ctx, testPrincipalUID)
	if err != nil || !ok {
		t.Fatalf("PrincipalGitIdentity: ok=%v err=%v", ok, err)
	}
	if login != "octocat" {
		t.Errorf("login = %q, want bare %q (anchor prefix must be stripped)", login, "octocat")
	}
	if userID != 583231 || displayName != "octocat" {
		t.Errorf("identity = (%q, %d), want (octocat, 583231)", displayName, userID)
	}
	// The whole point: the synthesized no-reply email must be the real one.
	if got := identity.NoreplyEmail(login, userID); got != "583231+octocat@users.noreply.github.com" {
		t.Errorf("NoreplyEmail = %q, want 583231+octocat@users.noreply.github.com", got)
	}
}

// TestApply_PrincipalRegistered_ClaimsPreGenesisSelfData verifies the orphan
// fix: facts/relations written under the local "self" anchor BEFORE a principal
// is bound are repointed onto the principal's person entity the first time
// principalRegistered materializes it. Without this, first-person data written
// pre-genesis is stranded once every read rebinds "self" -> the principal (a
// different row), as the resolver/MCP self-rebind does.
func TestApply_PrincipalRegistered_ClaimsPreGenesisSelfData(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	_, signPubB64 := identityTestKey(t)

	const (
		otherUID   = "01HZZZZZZZZZZZZZZZZZSELFOTH1"
		relSelfSub = "01HZZZZZZZZZZZZZZZZZSELFRS01"
		relSelfObj = "01HZZZZZZZZZZZZZZZZZSELFRO01"
		factUID    = "01HZZZZZZZZZZZZZZZZZSELFFC01"
	)

	// Pre-genesis state: an org to relate to, plus self-attributed data written
	// while the binder was unbound (subject/object == the "self" sentinel).
	retractTestEntity(t, ctx, d, otherUID, "Erebor")
	retractTestRelation(t, ctx, d, relSelfSub, "self", otherUID) // self -> works_at -> Erebor
	retractTestRelation(t, ctx, d, relSelfObj, otherUID, "self") // Erebor -> works_at -> self

	db, dbErr := d.OpenSQLite(ctx)
	if dbErr != nil {
		t.Fatalf("OpenSQLite: %v", dbErr)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO facts (uid, subject_uid, state_key, value, create_time, source_event_uid)
		      VALUES (?, 'self', 'state.role', 'founder', ?, ?)`,
		factUID, time.Now().UTC().Format(time.RFC3339), "01HZZZZZZZZZZZZZZZZZSELFFEV1",
	); err != nil {
		t.Fatalf("seed pre-genesis self fact: %v", err)
	}

	// Genesis: first materialization of the principal entity claims the data.
	if err := d.Apply(ctx, []kernel.Event{principalEvent(t, signPubB64)}); err != nil {
		t.Fatalf("Apply principalRegistered: %v", err)
	}

	for _, c := range []struct {
		what string
		q    string
	}{
		{"relations.subject_uid", `SELECT subject_uid FROM relations WHERE uid = '` + relSelfSub + `'`},
		{"relations.object_uid", `SELECT object_uid FROM relations WHERE uid = '` + relSelfObj + `'`},
		{"facts.subject_uid", `SELECT subject_uid FROM facts WHERE uid = '` + factUID + `'`},
	} {
		var got string
		if err := db.QueryRowContext(ctx, c.q).Scan(&got); err != nil {
			t.Fatalf("scan %s: %v", c.what, err)
		}
		if got != testPrincipalUID {
			t.Errorf("%s = %q after genesis, want %q (pre-genesis self data must move to the principal)", c.what, got, testPrincipalUID)
		}
	}

	var stranded int
	if err := db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM relations WHERE subject_uid='self' OR object_uid='self')
		      + (SELECT COUNT(*) FROM facts WHERE subject_uid='self')`).Scan(&stranded); err != nil {
		t.Fatalf("count stranded self rows: %v", err)
	}
	if stranded != 0 {
		t.Errorf("rows still attributed to 'self' after genesis = %d, want 0", stranded)
	}
}

// TestApply_PrincipalRegistered_PreservesSameAsLinkOnRevision verifies the
// first-materialization guard: the self->same_as->principal identity link
// (written AFTER genesis by EnsureSelfPrincipalLink) must NOT be repointed when
// a later principalRegistered revision (e.g. a GitHub-anchor upgrade) re-applies
// the event. A repoint that ran on every apply would move the link's subject
// from "self" to the principal, collapsing it into a degenerate self-loop.
func TestApply_PrincipalRegistered_PreservesSameAsLinkOnRevision(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	_, signPubB64 := identityTestKey(t)

	const linkUID = "01HZZZZZZZZZZZZZZZZZSAMEAS01"

	// First genesis materializes the principal entity (no self data to claim).
	if err := d.Apply(ctx, []kernel.Event{principalEvent(t, signPubB64)}); err != nil {
		t.Fatalf("Apply principalRegistered (first): %v", err)
	}

	// The identity link is emitted AFTER genesis with subject = the "self" sentinel.
	linkPayload, err := json.Marshal(ontology.Relation{
		Name:           "relations/" + linkUID,
		UID:            linkUID,
		SubjectUID:     "self",
		Predicate:      "same_as",
		ObjectUID:      testPrincipalUID,
		Confidence:     1.0,
		SourceEventUID: linkUID,
		Status:         ontology.RelationStatusAsserted,
		CreateTime:     time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal same_as link: %v", err)
	}
	if err := d.Apply(ctx, []kernel.Event{{
		UID:        linkUID,
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     "genesis",
		Confidence: 1.0,
		CreateTime: time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC),
		Payload:    linkPayload,
	}}); err != nil {
		t.Fatalf("Apply same_as link: %v", err)
	}

	// A later principalRegistered revision re-applies; the entity already exists,
	// so the repoint must be skipped and the link's subject left as "self".
	if err := d.Apply(ctx, []kernel.Event{principalEvent(t, signPubB64)}); err != nil {
		t.Fatalf("Apply principalRegistered (revision): %v", err)
	}

	db, dbErr := d.OpenSQLite(ctx)
	if dbErr != nil {
		t.Fatalf("OpenSQLite: %v", dbErr)
	}
	var subj string
	if err := db.QueryRowContext(ctx,
		`SELECT subject_uid FROM relations WHERE uid = ?`, linkUID).Scan(&subj); err != nil {
		t.Fatalf("scan same_as link subject: %v", err)
	}
	if subj != "self" {
		t.Errorf("same_as link subject_uid = %q after revision, want \"self\" (the guard must prevent clobbering the link)", subj)
	}
}

// TestApply_DeviceEnrolled_EnablesDevicePublicKeyLookup verifies the device
// projection plus the verifier's hot path: DevicePublicKey resolves the
// enrolled ed25519 key by sigKeyID.
func TestApply_DeviceEnrolled_EnablesDevicePublicKeyLookup(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	wantPub, signPubB64 := identityTestKey(t)

	if err := d.Apply(ctx, []kernel.Event{deviceEvent(t, signPubB64)}); err != nil {
		t.Fatalf("Apply deviceEnrolled: %v", err)
	}

	got, err := d.DevicePublicKey(ctx, testSigKeyID, testPrincipalUID)
	if err != nil {
		t.Fatalf("DevicePublicKey: %v", err)
	}
	if !got.Equal(wantPub) {
		t.Error("DevicePublicKey returned a key that does not match the enrolled key")
	}

	// An unknown sigKeyID is an error, not a silent empty key.
	if _, err := d.DevicePublicKey(ctx, "kid:does-not-exist", testPrincipalUID); err == nil {
		t.Error("DevicePublicKey for an unknown sigKeyID returned nil error; want not-found")
	}

	// A known sigKeyID under the WRONG principal must not resolve — the lookup is
	// principal-scoped so an enrolled key cannot speak for another principal.
	if _, err := d.DevicePublicKey(ctx, testSigKeyID, "01PRINCIPAL000000000000999"); err == nil {
		t.Error("DevicePublicKey resolved a key for a mismatched principal; want not-found")
	}
}

// TestApply_DeviceEnrolled_RejectsInvalidSignature verifies the projection
// refuses a deviceEnrolled event whose enrollment signature does not verify
// against its own signing key. Without this check a forged event could inject
// an arbitrary key the team-scope verifier would later trust by sigKeyID.
func TestApply_DeviceEnrolled_RejectsInvalidSignature(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	_, signPubB64 := identityTestKey(t)

	// A well-formed payload but with a bogus signature (valid base64, wrong bytes).
	payload, err := json.Marshal(ontology.DeviceEnrolledEvent{
		DeviceID:      testDeviceUID,
		PrincipalUID:  testPrincipalUID,
		SignPublicKey: signPubB64,
		SigKeyID:      testSigKeyID,
		DeviceLabel:   "laptop",
		Signature:     base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
		CreateTime:    time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal device: %v", err)
	}
	ev := kernel.Event{
		UID:        "01EVDEV000000000000000002",
		Kind:       kernel.EventKind(ontology.KindDeviceEnrolled),
		Source:     "genesis",
		Confidence: 1,
		Payload:    payload,
	}

	if err := d.Apply(ctx, []kernel.Event{ev}); err == nil {
		t.Fatal("Apply deviceEnrolled with an invalid signature returned nil error; want rejection")
	}

	// The key must NOT have been trusted into the devices table.
	if _, err := d.DevicePublicKey(ctx, testSigKeyID, testPrincipalUID); err == nil {
		t.Error("DevicePublicKey resolved a key from a rejected enrollment; want not-found")
	}
}

// TestApply_DeviceEnrolled_RejectsCrossPrincipalAuthor verifies the apply
// handler refuses a deviceEnrolled whose signed author is not the principal it
// enrolls. This closes a cross-principal key-injection path: an admitted team
// member could otherwise author a team-scope deviceEnrolled naming a co-member's
// principal_uid but their own signing key, binding the victim's principal to the
// attacker's key (the self-signature only proves the key signs its own payload,
// not who authored the event). The malicious key must NOT enter the devices
// table under the victim's principal.
func TestApply_DeviceEnrolled_RejectsCrossPrincipalAuthor(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// The attacker holds their own key and crafts a self-consistent enrollment
	// (the key signs its own bytes) naming the VICTIM's principal_uid.
	attackerSeed := sha256.Sum256([]byte("mazarbul:dispatcher:attacker-key"))
	attackerPriv := ed25519.NewKeyFromSeed(attackerSeed[:])
	attackerPub, _ := attackerPriv.Public().(ed25519.PublicKey)
	attackerPubB64 := base64.StdEncoding.EncodeToString(attackerPub)
	const (
		victimPrincipal = testPrincipalUID
		attackerKID     = "kid:attacker-injected"
	)
	enrollSig := ed25519.Sign(attackerPriv,
		ontology.DeviceEnrollmentBytes(testDeviceUID, victimPrincipal, attackerPubB64, attackerKID))
	payload, err := json.Marshal(ontology.DeviceEnrolledEvent{
		DeviceID:      testDeviceUID,
		PrincipalUID:  victimPrincipal, // names the victim
		SignPublicKey: attackerPubB64,  // but carries the attacker's key
		SigKeyID:      attackerKID,
		DeviceLabel:   "laptop",
		Signature:     base64.StdEncoding.EncodeToString(enrollSig),
		CreateTime:    time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal device: %v", err)
	}
	ev := kernel.Event{
		UID:        "01EVDEV000000000000000003",
		Kind:       kernel.EventKind(ontology.KindDeviceEnrolled),
		Source:     "team",
		Confidence: 1,
		Payload:    payload,
		// Signed by the attacker (a different principal than the one enrolled).
		Author: kernel.EventAuthor{PrincipalUID: "01ATTACKER0000000000000001", DeviceID: "01ATTACKERDEV0000000000001", SigKeyID: attackerKID},
	}

	if aerr := d.Apply(ctx, []kernel.Event{ev}); aerr == nil {
		t.Fatal("Apply deviceEnrolled authored by a different principal returned nil error; want rejection")
	}
	// The attacker's key must NOT be resolvable under the victim's principal.
	if _, lerr := d.DevicePublicKey(ctx, attackerKID, victimPrincipal); lerr == nil {
		t.Error("DevicePublicKey resolved an injected cross-principal key; want not-found")
	}
}

// ---------------------------------------------------------------------------
// B2: vouch verification tests
// ---------------------------------------------------------------------------

// subkeyForTest returns a random ed25519 subkey (simulating a per-device
// MintDeviceSubkey) distinct from identityTestKey.
func subkeyForTest(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey, string) {
	t.Helper()
	seed := sha256.Sum256([]byte("mazarbul:dispatcher:device-subkey-b2"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	pub := priv.Public().(ed25519.PublicKey)
	return priv, pub, identity.SigKeyID(pub)
}

// deviceEventWithVouch builds a deviceEnrolled event that carries a
// principal-key vouch signature. principalPriv signs DeviceVouchBytes and
// principalSigKeyID is its fingerprint (stored in VouchSigKeyID). The device
// signs DeviceEnrollmentBytes with subPriv (the random device subkey).
func deviceEventWithVouch(
	t *testing.T,
	deviceUID, principalUID string,
	subPriv ed25519.PrivateKey, subPub ed25519.PublicKey, devSigKeyID string,
	principalPriv ed25519.PrivateKey, principalSigKeyID string,
) kernel.Event {
	t.Helper()
	subPubB64 := base64.StdEncoding.EncodeToString(subPub)
	// Self-signature of the device subkey.
	enrollSig := ed25519.Sign(subPriv,
		ontology.DeviceEnrollmentBytes(deviceUID, principalUID, subPubB64, devSigKeyID))
	// Vouch: principal key signs DeviceVouchBytes.
	vouchSig := ed25519.Sign(principalPriv,
		ontology.DeviceVouchBytes(deviceUID, subPubB64))
	payload, err := json.Marshal(ontology.DeviceEnrolledEvent{
		DeviceID:       deviceUID,
		PrincipalUID:   principalUID,
		SignPublicKey:  subPubB64,
		SigKeyID:       devSigKeyID,
		DeviceLabel:    "subkey-device",
		Signature:      base64.StdEncoding.EncodeToString(enrollSig),
		CreateTime:     time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		VouchSigKeyID:  principalSigKeyID,
		VouchSignature: base64.StdEncoding.EncodeToString(vouchSig),
	})
	if err != nil {
		t.Fatalf("marshal vouched device: %v", err)
	}
	return kernel.Event{
		UID:        "01EVDEV000000000000000010",
		Kind:       kernel.EventKind(ontology.KindDeviceEnrolled),
		Source:     "device-enroll",
		Confidence: 1,
		Payload:    payload,
		Author:     kernel.EventAuthor{PrincipalUID: principalUID, DeviceID: deviceUID, SigKeyID: devSigKeyID},
	}
}

// TestApply_DeviceEnrolled_AcceptsValidVouch verifies that a deviceEnrolled
// event with a correct principal-key VouchSignature is accepted and the device
// row is stored with vouch columns populated.
func TestApply_DeviceEnrolled_AcceptsValidVouch(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	// identityTestKey uses the same seed as principalEvent/deviceEvent helpers,
	// so principalPriv here IS the key whose public half principalEvent stores.
	// testSigKeyID is the sig_key_id stored by principalEvent — use it as the
	// VouchSigKeyID so verifyEnrollmentVouch's consistency check passes.
	principalPriv := func() ed25519.PrivateKey {
		seed := sha256.Sum256([]byte("mazarbul:dispatcher:identity-test"))
		return ed25519.NewKeyFromSeed(seed[:])
	}()
	principalPubB64 := base64.StdEncoding.EncodeToString(principalPriv.Public().(ed25519.PublicKey))
	principalSigKeyIDStr := testSigKeyID // matches what principalEvent stores in sig_key_id

	// The principal must be registered first so verifyEnrollmentVouch can look up
	// their signing key from the principals table.
	if err := d.Apply(ctx, []kernel.Event{principalEvent(t, principalPubB64)}); err != nil {
		t.Fatalf("Apply principalRegistered: %v", err)
	}

	subPriv, subPub, subSigKeyID := subkeyForTest(t)
	ev := deviceEventWithVouch(t,
		testDeviceUID, testPrincipalUID,
		subPriv, subPub, subSigKeyID,
		principalPriv, principalSigKeyIDStr,
	)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply deviceEnrolled with valid vouch: %v", err)
	}

	// The subkey must be resolvable by its own SigKeyID.
	got, err := d.DevicePublicKey(ctx, subSigKeyID, testPrincipalUID)
	if err != nil {
		t.Fatalf("DevicePublicKey after vouched enrollment: %v", err)
	}
	if !got.Equal(subPub) {
		t.Error("DevicePublicKey returned wrong key after vouched enrollment")
	}

	// The vouch columns must be stored.
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var storedVouchSigKeyID string
	if qerr := reader.QueryRowContext(ctx,
		`SELECT COALESCE(vouch_sig_key_id, '') FROM devices WHERE device_id = ?`,
		testDeviceUID,
	).Scan(&storedVouchSigKeyID); qerr != nil {
		t.Fatalf("query vouch_sig_key_id: %v", qerr)
	}
	if storedVouchSigKeyID != principalSigKeyIDStr {
		t.Errorf("vouch_sig_key_id = %q, want %q", storedVouchSigKeyID, principalSigKeyIDStr)
	}
}

// TestApply_DeviceEnrolled_RejectsInvalidVouch verifies that a deviceEnrolled
// event with a corrupted VouchSignature is rejected, protecting the principal
// against a bad-actor enrolling an unauthorized subkey.
func TestApply_DeviceEnrolled_RejectsInvalidVouch(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	principalPriv := func() ed25519.PrivateKey {
		seed := sha256.Sum256([]byte("mazarbul:dispatcher:identity-test"))
		return ed25519.NewKeyFromSeed(seed[:])
	}()
	principalPubB64 := base64.StdEncoding.EncodeToString(principalPriv.Public().(ed25519.PublicKey))
	// Use testSigKeyID — the value principalEvent stores in sig_key_id — so the
	// VouchSigKeyID consistency check in verifyEnrollmentVouch passes. The test
	// is about the vouch SIGNATURE being invalid, not the key-id mismatch path.
	principalSigKeyIDStr := testSigKeyID

	if err := d.Apply(ctx, []kernel.Event{principalEvent(t, principalPubB64)}); err != nil {
		t.Fatalf("Apply principalRegistered: %v", err)
	}

	subPriv, subPub, subSigKeyID := subkeyForTest(t)
	subPubB64 := base64.StdEncoding.EncodeToString(subPub)

	// Build a valid enrollment but swap in a zero vouch signature.
	enrollSig := ed25519.Sign(subPriv,
		ontology.DeviceEnrollmentBytes(testDeviceUID, testPrincipalUID, subPubB64, subSigKeyID))
	payload, err := json.Marshal(ontology.DeviceEnrolledEvent{
		DeviceID:       testDeviceUID,
		PrincipalUID:   testPrincipalUID,
		SignPublicKey:  subPubB64,
		SigKeyID:       subSigKeyID,
		Signature:      base64.StdEncoding.EncodeToString(enrollSig),
		CreateTime:     time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		VouchSigKeyID:  principalSigKeyIDStr,
		VouchSignature: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)), // zeroes
	})
	if err != nil {
		t.Fatalf("marshal invalid-vouch device: %v", err)
	}
	ev := kernel.Event{
		UID:        "01EVDEV000000000000000011",
		Kind:       kernel.EventKind(ontology.KindDeviceEnrolled),
		Source:     "device-enroll",
		Confidence: 1,
		Payload:    payload,
		Author:     kernel.EventAuthor{PrincipalUID: testPrincipalUID, DeviceID: testDeviceUID, SigKeyID: subSigKeyID},
	}
	if err := d.Apply(ctx, []kernel.Event{ev}); err == nil {
		t.Fatal("Apply deviceEnrolled with invalid vouch returned nil error; want rejection")
	}
	// The subkey must NOT be trusted.
	if _, err := d.DevicePublicKey(ctx, subSigKeyID, testPrincipalUID); err == nil {
		t.Error("DevicePublicKey resolved a key from an invalid-vouch enrollment; want not-found")
	}
}

// TestApply_DeviceEnrolled_TeamScopeVouchlessIsSkipped pins the rule that a
// team-scoped deviceEnrolled without a principal vouch is DROPPED as a benign
// no-op at apply: a device-0 key reaches teammates through the grant vouch,
// never through a deviceEnrolled, so a vouchless team enrollment has no
// legitimate producer. The projection must NOT inject a devices row for it (the
// security property), but applying it must NOT error either — an append-only log
// written before the boot-seeder fix still carries these, and erroring would
// fail `maz rebuild` for every joined member on replay (bug F8).
func TestApply_DeviceEnrolled_TeamScopeVouchlessIsSkipped(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	_, signPubB64 := identityTestKey(t)

	ev := deviceEvent(t, signPubB64) // valid self-signed, vouchless enrollment
	ev.Scope = kernel.ScopeTeam
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply of a vouchless team-scoped deviceEnrolled must be a no-op, got: %v", err)
	}
	// The security property is preserved: no devices row was injected, so the
	// key does not resolve.
	if _, err := d.DevicePublicKey(ctx, testSigKeyID, testPrincipalUID); err == nil {
		t.Error("a skipped vouchless team enrollment must NOT inject a devices row")
	}

	// The same enrollment in personal scope (the legacy device-0 shape) applies.
	ev2 := deviceEvent(t, signPubB64)
	if err := d.Apply(ctx, []kernel.Event{ev2}); err != nil {
		t.Fatalf("Apply of a vouchless personal-scope deviceEnrolled: %v", err)
	}
}

// TestApply_DeviceEnrolled_VouchResolvesFromDevicesTable covers the teammate
// case: the enrolling principal has NO principals row locally (teammates
// never see each other's personal-scope principalRegistered) — their root key
// exists only as a devices row seeded by the grant vouch. The vouch must
// resolve through that row, and only through a ROOT row: a subkey can never
// vouch further subkeys, so compromising one device cannot mint replacement
// keys that would survive its own revocation.
func TestApply_DeviceEnrolled_VouchResolvesFromDevicesTable(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	_, signPubB64 := identityTestKey(t)

	// Seed ONLY the root devices row (no principals row) — the shape a
	// teammate's projection has after applying the member's grant.
	if err := d.Apply(ctx, []kernel.Event{deviceEvent(t, signPubB64)}); err != nil {
		t.Fatalf("seed root device row: %v", err)
	}

	rootPriv := func() ed25519.PrivateKey {
		seed := sha256.Sum256([]byte("mazarbul:dispatcher:identity-test"))
		return ed25519.NewKeyFromSeed(seed[:])
	}()
	subPriv, subPub, subSigKeyID := subkeyForTest(t)

	// Root-vouched subkey enrollment applies via the devices-table fallback.
	ev := deviceEventWithVouch(t,
		"01DEVICE0000000000000000002", testPrincipalUID,
		subPriv, subPub, subSigKeyID,
		rootPriv, testSigKeyID,
	)
	if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
		t.Fatalf("Apply subkey enrollment vouched via devices row: %v", err)
	}
	if _, err := d.DevicePublicKey(ctx, subSigKeyID, testPrincipalUID); err != nil {
		t.Fatalf("subkey not resolvable after devices-fallback vouch: %v", err)
	}

	// A subkey-vouched enrollment must NOT apply: the vouching row carries
	// vouch fields, so the root-only fallback ignores it.
	sub2Seed := sha256.Sum256([]byte("mazarbul:dispatcher:identity-test:sub2"))
	sub2Priv := ed25519.NewKeyFromSeed(sub2Seed[:])
	sub2Pub := sub2Priv.Public().(ed25519.PublicKey)
	ev2 := deviceEventWithVouch(t,
		"01DEVICE0000000000000000003", testPrincipalUID,
		sub2Priv, sub2Pub, identity.SigKeyID(sub2Pub),
		subPriv, subSigKeyID, // vouched by the SUBKEY, not the root
	)
	ev2.UID = "01EVDEV000000000000000011"
	if err := d.Apply(ctx, []kernel.Event{ev2}); err == nil {
		t.Fatal("Apply of a subkey-vouched enrollment must fail (only a root key may vouch)")
	}
}

// TestApply_DeviceEnrolled_TeamPublishedLatch pins the team_published latch:
// it starts unset for a personal-scope enrollment, latches when the same
// enrollment applies from a TEAM-scoped event, and never clears.
func TestApply_DeviceEnrolled_TeamPublishedLatch(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	principalPriv := func() ed25519.PrivateKey {
		seed := sha256.Sum256([]byte("mazarbul:dispatcher:identity-test"))
		return ed25519.NewKeyFromSeed(seed[:])
	}()
	principalPubB64 := base64.StdEncoding.EncodeToString(principalPriv.Public().(ed25519.PublicKey))
	if err := d.Apply(ctx, []kernel.Event{principalEvent(t, principalPubB64)}); err != nil {
		t.Fatalf("Apply principalRegistered: %v", err)
	}
	subPriv, subPub, subSigKeyID := subkeyForTest(t)

	teamPublished := func() int {
		t.Helper()
		reader, err := d.SQLiteReader()
		if err != nil {
			t.Fatalf("SQLiteReader: %v", err)
		}
		var tp int
		if qerr := reader.QueryRowContext(ctx,
			`SELECT team_published FROM devices WHERE sig_key_id = ?`, subSigKeyID).Scan(&tp); qerr != nil {
			t.Fatalf("query team_published: %v", qerr)
		}
		return tp
	}

	personal := deviceEventWithVouch(t, testDeviceUID, testPrincipalUID,
		subPriv, subPub, subSigKeyID, principalPriv, testSigKeyID)
	if err := d.Apply(ctx, []kernel.Event{personal}); err != nil {
		t.Fatalf("Apply personal enrollment: %v", err)
	}
	if got := teamPublished(); got != 0 {
		t.Errorf("team_published after personal enrollment = %d, want 0", got)
	}

	team := deviceEventWithVouch(t, testDeviceUID, testPrincipalUID,
		subPriv, subPub, subSigKeyID, principalPriv, testSigKeyID)
	team.UID = "01EVDEV000000000000000012"
	team.Scope = kernel.ScopeTeam
	if err := d.Apply(ctx, []kernel.Event{team}); err != nil {
		t.Fatalf("Apply team enrollment: %v", err)
	}
	if got := teamPublished(); got != 1 {
		t.Errorf("team_published after team enrollment = %d, want 1", got)
	}

	// A later personal-scope re-apply (rebuild replay order) never clears it.
	again := deviceEventWithVouch(t, testDeviceUID, testPrincipalUID,
		subPriv, subPub, subSigKeyID, principalPriv, testSigKeyID)
	again.UID = "01EVDEV000000000000000013"
	if err := d.Apply(ctx, []kernel.Event{again}); err != nil {
		t.Fatalf("re-apply personal enrollment: %v", err)
	}
	if got := teamPublished(); got != 1 {
		t.Errorf("team_published after personal re-apply = %d, want 1 (latch never clears)", got)
	}
}

// ---------------------------------------------------------------------------
// B2: deviceRevoked projection + convergent DeviceRevokedAt tests
// ---------------------------------------------------------------------------

// deviceRevokedEvent builds a self-authored deviceRevoked kernel.Event: the
// event author IS the principal named in the payload.
func deviceRevokedEvent(t *testing.T, uid, deviceUID, sigKeyID, principalUID, reason string) kernel.Event {
	t.Helper()
	return deviceRevokedEventBy(t, uid, principalUID, deviceUID, sigKeyID, principalUID, reason)
}

// deviceRevokedEventBy builds a deviceRevoked kernel.Event whose author may
// differ from the payload's target principal — the shape a grafted
// admin-revocation (or a forged member-revocation) arrives in.
func deviceRevokedEventBy(t *testing.T, uid, author, deviceUID, sigKeyID, targetPrincipalUID, reason string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.DeviceRevokedEvent{
		DeviceID:     deviceUID,
		SigKeyID:     sigKeyID,
		PrincipalUID: targetPrincipalUID,
		RevokedAt:    time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC),
		Reason:       reason,
	})
	if err != nil {
		t.Fatalf("marshal deviceRevoked: %v", err)
	}
	return kernel.Event{
		UID:        uid,
		Kind:       kernel.EventKind(ontology.KindDeviceRevoked),
		Source:     "team",
		Confidence: 1,
		Scope:      kernel.ScopeTeam,
		Payload:    payload,
		Author:     kernel.EventAuthor{PrincipalUID: author},
	}
}

// TestApplyDeviceRevoked_ProjectsToRevocationsTable verifies that applyDeviceRevoked
// inserts a candidate row into device_revocations. Whether a candidate is
// HONORED is decided by DeviceRevokedAt's authority fold at verdict time,
// never at projection time.
func TestApplyDeviceRevoked_ProjectsToRevocationsTable(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	revokeUID := "01EVREV000000000000000005"
	if err := d.Apply(ctx, []kernel.Event{
		deviceRevokedEvent(t, revokeUID, testDeviceUID, testSigKeyID, testPrincipalUID, "lost device"),
	}); err != nil {
		t.Fatalf("Apply deviceRevoked: %v", err)
	}

	// Query directly to confirm the row was inserted.
	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var count int
	if qerr := reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM device_revocations WHERE event_uid = ?`, revokeUID,
	).Scan(&count); qerr != nil {
		t.Fatalf("count device_revocations: %v", qerr)
	}
	if count != 1 {
		t.Errorf("device_revocations row count = %d, want 1", count)
	}
}

// TestDeviceRevokedAt_PositionRestricted pins the convergence contract of the
// DeviceRevokedAt fold: the verdict is computed ONLY from revocation records
// whose event_uid is strictly less than beforeEventUID, so a revocation at U5
// cannot retroactively drop events with UID ≤ U5.
func TestDeviceRevokedAt_PositionRestricted(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	_, signPubB64 := identityTestKey(t)

	// Device enrolled, then self-revoked by its own principal at U5; the
	// revocation is applied before any of the probed positions exist (the
	// arrival order that tests convergence: the local store already knows
	// about the revocation when the probed events arrive). Enrollment is
	// required — the authority fold resolves the target device's principal
	// from the devices projection and fails closed without it.
	const revokeUID = "01EV0000000000000000000005"
	if err := d.Apply(ctx, []kernel.Event{
		deviceEvent(t, signPubB64),
		deviceRevokedEvent(t, revokeUID, testDeviceUID, testSigKeyID, testPrincipalUID, "lost"),
	}); err != nil {
		t.Fatalf("seed enrollment+revocation: %v", err)
	}

	for _, tc := range []struct {
		name        string
		beforeUID   string
		wantRevoked bool
	}{
		{
			name: "strictly before revocation not revoked",
			// An event at U4 asks: was the device revoked before U4? Revocation is at
			// U5 > U4 → no.
			beforeUID: "01EV0000000000000000000004", wantRevoked: false,
		},
		{
			name: "at revocation UID not revoked (strictly less than)",
			// U5 itself is not < U5.
			beforeUID: revokeUID, wantRevoked: false,
		},
		{
			name: "strictly after revocation is revoked",
			// An event at U6 asks: was the device revoked before U6? Revocation is at
			// U5 < U6 → yes.
			beforeUID: "01EV0000000000000000000006", wantRevoked: true,
		},
		{
			name:      "well after revocation is revoked",
			beforeUID: "01EV0000000000000000000009", wantRevoked: true,
		},
		{
			name:        "lookup by deviceID only (no sigKeyID match)",
			beforeUID:   "01EV0000000000000000000009",
			wantRevoked: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			revoked, err := d.DeviceRevokedAt(ctx, testDeviceUID, testSigKeyID, tc.beforeUID)
			if err != nil {
				t.Fatalf("DeviceRevokedAt: %v", err)
			}
			if revoked != tc.wantRevoked {
				t.Errorf("DeviceRevokedAt(before=%s) = %v, want %v", tc.beforeUID, revoked, tc.wantRevoked)
			}
		})
	}
}

// TestDeviceRevokedAt_AuthorityFold pins the authority rule of the revocation
// fold: a device_revocations row is a candidate, honored only when its author
// is the target device's own principal (resolved from the devices projection,
// never from the attacker-controlled payload) or held owner/admin at the
// revocation's own position. Grafted revocations never pass the live-append
// gate, so without this fold any validly-signed member could convergently
// kill the owner's device.
func TestDeviceRevokedAt_AuthorityFold(t *testing.T) {
	t.Parallel()

	const (
		ownerUID  = "01OWNER0000000000000000001"
		memberUID = "01MEMBER000000000000000001"
		revokeUID = "01EV0000000000000000000005"
		probeUID  = "01EV0000000000000000000009"
	)

	for _, tc := range []struct {
		name        string
		events      func(t *testing.T, signPubB64 string) []kernel.Event
		wantRevoked bool
	}{
		{
			// Single-user installs: no membership events exist, so
			// TeamMemberRoleAt returns ok=false and only the SELF path
			// can honor the revocation.
			name: "self author honored without any team membership",
			events: func(t *testing.T, signPubB64 string) []kernel.Event {
				t.Helper()
				return []kernel.Event{
					deviceEvent(t, signPubB64),
					deviceRevokedEvent(t, revokeUID, testDeviceUID, testSigKeyID, testPrincipalUID, "lost"),
				}
			},
			wantRevoked: true,
		},
		{
			// No devices row: the payload's principal_uid claims "self" but
			// the fold resolves the binding from the projection, not the
			// payload — fail closed until the enrollment arrives.
			name: "unenrolled device fails closed even when payload claims self",
			events: func(t *testing.T, signPubB64 string) []kernel.Event {
				t.Helper()
				return []kernel.Event{
					deviceRevokedEvent(t, revokeUID, testDeviceUID, testSigKeyID, testPrincipalUID, "lost"),
				}
			},
			wantRevoked: false,
		},
		{
			// The hole this fold closes: an admitted plain member crafts a
			// validly-signed revocation for another principal's device.
			name: "member author cannot revoke another principals device",
			events: func(t *testing.T, signPubB64 string) []kernel.Event {
				t.Helper()
				return []kernel.Event{
					deviceEvent(t, signPubB64),
					grantEvent(t, "01EV0000000000000000000001", ownerUID, ownerUID, 1),
					grantEvent(t, "01EV0000000000000000000002", ownerUID, memberUID, 1),
					deviceRevokedEventBy(t, revokeUID, memberUID, testDeviceUID, testSigKeyID, testPrincipalUID, "forged"),
				}
			},
			wantRevoked: false,
		},
		{
			name: "admin author honored",
			events: func(t *testing.T, signPubB64 string) []kernel.Event {
				t.Helper()
				return []kernel.Event{
					deviceEvent(t, signPubB64),
					grantEvent(t, "01EV0000000000000000000001", ownerUID, ownerUID, 1),
					grantEvent(t, "01EV0000000000000000000002", ownerUID, memberUID, 1),
					roleChangeEvent(t, "01EV0000000000000000000003", ownerUID, memberUID, ontology.RoleAdmin),
					deviceRevokedEventBy(t, revokeUID, memberUID, testDeviceUID, testSigKeyID, testPrincipalUID, "compromised"),
				}
			},
			wantRevoked: true,
		},
		{
			name: "genesis owner author honored",
			events: func(t *testing.T, signPubB64 string) []kernel.Event {
				t.Helper()
				return []kernel.Event{
					deviceEvent(t, signPubB64),
					grantEvent(t, "01EV0000000000000000000001", ownerUID, ownerUID, 1),
					deviceRevokedEventBy(t, revokeUID, ownerUID, testDeviceUID, testSigKeyID, testPrincipalUID, "compromised"),
				}
			},
			wantRevoked: true,
		},
		{
			// Position purity of the ADMIN path: a promotion that post-dates
			// the revocation can never retroactively authorize it.
			name: "promotion after the revocation does not authorize it",
			events: func(t *testing.T, signPubB64 string) []kernel.Event {
				t.Helper()
				return []kernel.Event{
					deviceEvent(t, signPubB64),
					grantEvent(t, "01EV0000000000000000000001", ownerUID, ownerUID, 1),
					grantEvent(t, "01EV0000000000000000000002", ownerUID, memberUID, 1),
					deviceRevokedEventBy(t, revokeUID, memberUID, testDeviceUID, testSigKeyID, testPrincipalUID, "premature"),
					roleChangeEvent(t, "01EV0000000000000000000006", ownerUID, memberUID, ontology.RoleAdmin),
				}
			},
			wantRevoked: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := testutil.TestContext(t)
			d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
			_, signPubB64 := identityTestKey(t)

			if err := d.Apply(ctx, tc.events(t, signPubB64)); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			revoked, err := d.DeviceRevokedAt(ctx, testDeviceUID, testSigKeyID, probeUID)
			if err != nil {
				t.Fatalf("DeviceRevokedAt: %v", err)
			}
			if revoked != tc.wantRevoked {
				t.Errorf("DeviceRevokedAt = %v, want %v", revoked, tc.wantRevoked)
			}
		})
	}
}

// TestDeviceRevokedAt_IdempotentUnderReplay confirms that applying the same
// deviceRevoked event twice (ON CONFLICT DO NOTHING) does not error and does
// not add a duplicate row — the rebuild reproducibility contract.
func TestDeviceRevokedAt_IdempotentUnderReplay(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})

	ev := deviceRevokedEvent(t, "01EVREV000000000000000005", testDeviceUID, testSigKeyID, testPrincipalUID, "")
	for i := range 2 {
		if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("Apply pass %d: %v", i, err)
		}
	}

	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var count int
	if qerr := reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM device_revocations WHERE device_id = ?`, testDeviceUID,
	).Scan(&count); qerr != nil {
		t.Fatalf("count: %v", qerr)
	}
	if count != 1 {
		t.Errorf("device_revocations count after replay = %d, want 1", count)
	}
}

// TestGetDeviceByID_ReturnsDeviceInfo confirms that GetDeviceByID resolves a
// previously enrolled device across all principals and returns not-found for
// an unknown ULID.
func TestGetDeviceByID_ReturnsDeviceInfo(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	_, signPubB64 := identityTestKey(t)

	if err := d.Apply(ctx, []kernel.Event{deviceEvent(t, signPubB64)}); err != nil {
		t.Fatalf("Apply deviceEnrolled: %v", err)
	}

	di, found, err := d.GetDeviceByID(ctx, testDeviceUID)
	if err != nil {
		t.Fatalf("GetDeviceByID: %v", err)
	}
	if !found {
		t.Fatal("GetDeviceByID: expected found=true for enrolled device")
	}
	if di.DeviceID != testDeviceUID {
		t.Errorf("DeviceID = %q, want %q", di.DeviceID, testDeviceUID)
	}
	if di.PrincipalUID != testPrincipalUID {
		t.Errorf("PrincipalUID = %q, want %q", di.PrincipalUID, testPrincipalUID)
	}
	if di.SigKeyID != testSigKeyID {
		t.Errorf("SigKeyID = %q, want %q", di.SigKeyID, testSigKeyID)
	}

	// Unknown device must return not-found.
	_, found2, err2 := d.GetDeviceByID(ctx, "01UNKNOWN0000000000000001")
	if err2 != nil {
		t.Fatalf("GetDeviceByID unknown: %v", err2)
	}
	if found2 {
		t.Error("GetDeviceByID: expected found=false for unknown device")
	}
}

// TestApply_PrincipalRegistered_IdempotentUnderReplay confirms a second Apply
// of the same genesis event (as happens on Rebuild) does not error and does
// not duplicate the row — the from-log reproducibility contract.
func TestApply_PrincipalRegistered_IdempotentUnderReplay(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	d := newDispatcher(t, ctx, &fakeEmbedder{dim: storage.VectorDim})
	_, signPubB64 := identityTestKey(t)
	ev := principalEvent(t, signPubB64)

	for i := range 2 {
		if err := d.Apply(ctx, []kernel.Event{ev}); err != nil {
			t.Fatalf("Apply pass %d: %v", i, err)
		}
	}

	reader, err := d.SQLiteReader()
	if err != nil {
		t.Fatalf("SQLiteReader: %v", err)
	}
	var count int
	if qerr := reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM principals WHERE principal_uid = ?`, testPrincipalUID).Scan(&count); qerr != nil {
		t.Fatalf("count principals: %v", qerr)
	}
	if count != 1 {
		t.Errorf("principals row count after replay = %d, want 1 (idempotent upsert)", count)
	}
}
