package dispatcher

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// selfAnchorUID is the local-only symbolic self anchor seeded by migration
// 0012_self_entity_seed (display name "Me"). It is never emitted as an event,
// so it never syncs or collides across a team. Mirrors resolver.SelfEntityUID
// and the "self" sentinel the Relation schema accepts as subject/object; kept
// as a local constant to avoid importing internal/kernel/resolver, matching the
// pattern in internal/mcp/tools/relate.go (selfEntityUID).
const selfAnchorUID = "self"

// applyPrincipalRegistered upserts one row into principals AND materializes
// the principal as a person entity (so facts/relations/recall reference it
// like any other entity). Both writes share one transaction. Idempotent
// under replay via ON CONFLICT, so a Rebuild reconstructs the projection.
// On the FIRST materialization of the principal entity it also claims any
// pre-genesis self-attributed data (see the repoint below).
func (d *Dispatcher) applyPrincipalRegistered(ctx context.Context, ev kernel.Event) (retErr error) {
	var p ontology.PrincipalRegisteredEvent
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("decode principalRegistered payload: %w", err)
	}

	anchor := p.GitHubAnchor
	if anchor == "" {
		anchor = ontology.GitHubAnchorNone
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO principals
		     (principal_uid, display_name, sign_public_key, wrap_public_key,
		      sig_key_id, create_time, github_login, github_user_id,
		      github_anchor, github_attestation, github_anchor_key)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(principal_uid) DO UPDATE SET
			display_name       = excluded.display_name,
			sign_public_key    = excluded.sign_public_key,
			wrap_public_key    = excluded.wrap_public_key,
			sig_key_id         = excluded.sig_key_id,
			create_time        = excluded.create_time,
			github_login       = excluded.github_login,
			github_user_id     = excluded.github_user_id,
			github_anchor      = excluded.github_anchor,
			github_attestation = excluded.github_attestation,
			github_anchor_key  = excluded.github_anchor_key`,
		p.PrincipalUID,
		p.DisplayName,
		p.SignPublicKey,
		p.WrapPublicKey,
		p.SigKeyID,
		p.CreateTime.UTC().Format(time.RFC3339Nano),
		nullableText(p.SameAs),
		nullableInt64(p.GitHubUserID),
		anchor,
		nullableText(p.GitHubAttestation),
		nullableText(p.GitHubAnchorKey),
	); err != nil {
		return fmt.Errorf("upsert principal: %w", err)
	}

	// Detect whether the principal's person entity already exists BEFORE the
	// upsert below creates it, so the pre-genesis self-data claim can be gated to
	// the first materialization (see the repoint below).
	var entityExisted bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM entities WHERE uid = ?)`, p.PrincipalUID,
	).Scan(&entityExisted); err != nil {
		return fmt.Errorf("check principal entity existence: %w", err)
	}

	// Materialize the principal as a resolved person entity so it is a
	// first-class subject/object for facts and relations. scope tracks the
	// event scope (#206 scope-isolated reads) — personal for identity events —
	// so the principal surfaces in a default read like any other entity.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, scope)
		 VALUES (?, 'person', ?, 1.0, 'resolved', ?)
		 ON CONFLICT(uid) DO UPDATE SET
			display_name = excluded.display_name,
			confidence   = excluded.confidence,
			scope        = excluded.scope`,
		p.PrincipalUID,
		p.DisplayName,
		scopeValue(ev),
	); err != nil {
		return fmt.Errorf("upsert principal entity: %w", err)
	}

	// Claim pre-genesis self-attributed data on the FIRST materialization of the
	// principal entity. Facts/relations written under the local "self" anchor
	// before this principal was bound would otherwise be orphaned: every read
	// rebinds "self" -> this principal (a different row), so the data becomes
	// unreachable. Repoint it onto the principal with the same primitive
	// entityMerged uses. Gated to first-creation so it never runs while the
	// self->same_as->principal identity link (emitted later by
	// kernelboot.EnsureSelfPrincipalLink, subject "self") is present — preserving
	// that link on every replay and on a later principalRegistered revision (a
	// GitHub-anchor upgrade re-applies the event against an existing entity). A
	// no-op when the principal IS the seeded self anchor (degenerate self-merge).
	//
	// Inherited limitation (same as entityMerged): a repointed fact keeps the
	// content_hash it was written with, which still encodes the old "self"
	// subject. Re-asserting that exact fact verbatim against the principal will
	// therefore miss the hash dedup once and emit one spurious supersede. No data
	// is lost; the duplicate is collapsed on the following projection.
	//
	// Scope-safe by the genesis-time no-team invariant: genesis runs before any
	// team join, so every "self"-attributed row is personal scope and there is no
	// team-scoped "self" data the repoint could pull across a scope boundary.
	if !entityExisted && p.PrincipalUID != selfAnchorUID {
		if err := repointEntityFKs(ctx, tx, selfAnchorUID, p.PrincipalUID); err != nil {
			// repointEntityFKs prefixes its errors with "entityMerged:"; wrap so an
			// operator debugging a principalRegistered failure sees the real path.
			return fmt.Errorf("claim pre-genesis self data: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// applyDeviceEnrolled upserts one row into devices, keyed by device_id and
// indexed by sig_key_id (the verifier's lookup key). Idempotent under
// replay.
func (d *Dispatcher) applyDeviceEnrolled(ctx context.Context, ev kernel.Event) (retErr error) {
	var dev ontology.DeviceEnrolledEvent
	if err := json.Unmarshal(ev.Payload, &dev); err != nil {
		return fmt.Errorf("decode deviceEnrolled payload: %w", err)
	}

	// A device enrollment may only be authored by the principal it enrolls.
	// verifyEnrollmentSignature proves the payload is self-consistent (the key
	// signs its own enrollment) but NOT that the event's signed author owns that
	// principal. Without this, an admitted team member could author a team-scope
	// deviceEnrolled naming a co-member's principal_uid but their OWN signing
	// key; the upsert below would bind the victim's principal to the attacker's
	// key, and DevicePublicKey(sigKeyID, victim) would then resolve to it —
	// defeating the principal-scoped verifier. The author is empty only on the
	// unsigned pre-identity path, which never reaches team scope, so require the
	// match whenever an author is present. (#213)
	if ev.Author.PrincipalUID != "" && ev.Author.PrincipalUID != dev.PrincipalUID {
		return fmt.Errorf("deviceEnrolled %s: author %q may not enroll a device for principal %q",
			dev.DeviceID, ev.Author.PrincipalUID, dev.PrincipalUID)
	}

	// Verify the enrollment self-signature before the key is trusted. The
	// team-scope verifier later resolves signing keys from this table by
	// sigKeyID, so an unverified deviceEnrolled would let an event inject an
	// arbitrary verification key. Checking the signature against the device's
	// own public key proves the enroller held the matching private key and
	// rejects a tampered payload at the projection boundary.
	if err := verifyEnrollmentSignature(dev); err != nil {
		return fmt.Errorf("deviceEnrolled %s: %w", dev.DeviceID, err)
	}

	// A vouchless team-scoped enrollment has no legitimate producer: a device-0
	// Watchword key reaches teammates through the grant vouch, never through a
	// deviceEnrolled. Drop it as a benign no-op (no devices row injected — the
	// same refusal requireEnrollmentVouch enforces below) rather than erroring.
	// The boot seeder no longer emits these, but append-only logs written before
	// that fix still carry them; erroring would fail `maz rebuild` for every
	// joined member on replay. Skipping keeps replay deterministic and clean.
	if ev.Scope == kernel.ScopeTeam && dev.VouchSignature == "" {
		return nil
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if err := d.requireEnrollmentVouch(ctx, ev.Scope, &dev); err != nil {
		return err
	}

	// The conflict guard `WHERE devices.principal_uid = excluded.principal_uid`
	// makes a device_id unable to cross principals: a re-enrollment only updates
	// a row that already belongs to the same principal (own-device key rotation),
	// so no event can rebind or clobber another principal's existing device row.
	// team_published latches (MAX, never clears) whether any applied enrollment
	// for this device arrived in team scope — the boot seeder's signal that
	// teammates can already resolve this key.
	teamPublished := 0
	if ev.Scope == kernel.ScopeTeam {
		teamPublished = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO devices
		     (device_id, principal_uid, sign_public_key, sig_key_id,
		      device_label, signature, create_time,
		      vouch_sig_key_id, vouch_signature, team_published)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(device_id) DO UPDATE SET
			principal_uid    = excluded.principal_uid,
			sign_public_key  = excluded.sign_public_key,
			sig_key_id       = excluded.sig_key_id,
			device_label     = excluded.device_label,
			signature        = excluded.signature,
			create_time      = excluded.create_time,
			vouch_sig_key_id = excluded.vouch_sig_key_id,
			vouch_signature  = excluded.vouch_signature,
			team_published   = MAX(devices.team_published, excluded.team_published)
		 WHERE devices.principal_uid = excluded.principal_uid`,
		dev.DeviceID,
		dev.PrincipalUID,
		dev.SignPublicKey,
		dev.SigKeyID,
		nullableText(dev.DeviceLabel),
		dev.Signature,
		dev.CreateTime.UTC().Format(time.RFC3339Nano),
		nullableText(dev.VouchSigKeyID),
		nullableText(dev.VouchSignature),
		teamPublished,
	); err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// requireEnrollmentVouch enforces the vouch policy for a deviceEnrolled
// event: verified whenever present, and REQUIRED in team scope. A subkey
// device carries a VouchSignature signed by the principal's Watchword-derived
// key over the vouch bytes, proving the principal authorized this specific
// random subkey. Absent vouch fields mean a legacy device-0 enrollment —
// still valid in personal scope, but never in team scope: a device-0 key
// reaches teammates through the grant vouch, not through a deviceEnrolled,
// so a vouchless team-scoped enrollment has no legitimate producer and would
// let any admitted member inject a fresh devices row (the self-signature
// alone proves only key possession, not principal authorization).
func (d *Dispatcher) requireEnrollmentVouch(ctx context.Context, scope kernel.Scope, dev *ontology.DeviceEnrolledEvent) error {
	if dev.VouchSignature == "" {
		if scope == kernel.ScopeTeam {
			return fmt.Errorf("deviceEnrolled %s: team-scope enrollment requires a principal vouch", dev.DeviceID)
		}
		return nil
	}
	if err := verifyEnrollmentVouch(ctx, d.sqlite.Reader(), *dev); err != nil {
		return fmt.Errorf("deviceEnrolled %s: vouch verification: %w", dev.DeviceID, err)
	}
	return nil
}

// verifyEnrollmentSignature checks dev.Signature is a valid ed25519 signature
// over the canonical enrollment bytes, made by the device's own signing key —
// the TOFU self-certification the genesis ceremony produces. It rejects a
// tampered or malformed deviceEnrolled payload before applyDeviceEnrolled
// trusts the key, since the team-scope verifier later resolves keys from the
// devices table by sigKeyID without re-checking this signature.
func verifyEnrollmentSignature(dev ontology.DeviceEnrolledEvent) error {
	pub, err := base64.StdEncoding.DecodeString(dev.SignPublicKey)
	if err != nil {
		return fmt.Errorf("decode signPublicKey: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("signPublicKey is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	sig, err := base64.StdEncoding.DecodeString(dev.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	msg := ontology.DeviceEnrollmentBytes(dev.DeviceID, dev.PrincipalUID, dev.SignPublicKey, dev.SigKeyID)
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return errors.New("enrollment signature does not verify against signPublicKey")
	}
	return nil
}

// DevicePublicKey resolves an enrolled device's ed25519 public key by its
// sigKeyID, scoped to principalUID. It satisfies identity.DeviceKeyLookup,
// backing the team-scope signature verifier. The principal_uid predicate is
// security-load-bearing: it ensures a sig key id can only verify events that
// claim authorship by the principal that key is enrolled under, so an enrolled
// member cannot author events impersonating a different principal. Returns an
// error when no device is enrolled under (sigKeyID, principalUID) or the stored
// key is malformed.
func (d *Dispatcher) DevicePublicKey(ctx context.Context, sigKeyID, principalUID string) (ed25519.PublicKey, error) {
	var b64 string
	err := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT sign_public_key FROM devices WHERE sig_key_id = ? AND principal_uid = ?`,
		sigKeyID, principalUID).Scan(&b64)
	if errors.Is(err, sql.ErrNoRows) {
		// Wrap the typed sentinel so the team verifier can distinguish this
		// clean "not enrolled yet" miss (its TOFU bootstrap case) from a real
		// storage fault via errors.Is(err, kernel.ErrDeviceNotEnrolled).
		return nil, fmt.Errorf("%w: sigKeyID %q principal %q", kernel.ErrDeviceNotEnrolled, sigKeyID, principalUID)
	}
	if err != nil {
		return nil, fmt.Errorf("query device key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode device key %q: %w", sigKeyID, err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("device key %q has wrong size %d", sigKeyID, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// GenesisAnchorKey resolves the signing public key enrolled for the fold's
// genesis principal under sigKeyID. It backs the consume loop's TEAM.json
// import verification: a genesis anchor file is only trusted when it is
// signed by a key that the grafted event log itself enrolled for the fold's
// genesis principal, so an adversary with bare repo-write access cannot
// plant a foreign anchor (the pin would otherwise fail-close the whole team
// until a manual re-pin). A false ok with a nil error means the fold has no
// genesis yet or the key is not enrolled for the genesis principal — callers
// retry after a later graft delivers the enrollment.
func (d *Dispatcher) GenesisAnchorKey(ctx context.Context, sigKeyID string) (principalUID string, signPub ed25519.PublicKey, ok bool, err error) {
	if sigKeyID == "" {
		return "", nil, false, nil
	}
	var b64 string
	qerr := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT a.principal_uid, d.sign_public_key
		 FROM team_membership_acl a
		 JOIN devices d ON d.principal_uid = a.principal_uid AND d.sig_key_id = ?
		 WHERE a.is_genesis = 1
		 LIMIT 1`, sigKeyID).Scan(&principalUID, &b64)
	if errors.Is(qerr, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if qerr != nil {
		return "", nil, false, fmt.Errorf("query genesis anchor key: %w", qerr)
	}
	raw, derr := base64.StdEncoding.DecodeString(b64)
	if derr != nil {
		return "", nil, false, fmt.Errorf("decode genesis anchor key %q: %w", sigKeyID, derr)
	}
	if len(raw) != ed25519.PublicKeySize {
		return "", nil, false, fmt.Errorf("genesis anchor key %q has wrong size %d", sigKeyID, len(raw))
	}
	return principalUID, ed25519.PublicKey(raw), true, nil
}

// nullableInt64 maps a zero id to SQL NULL so an unconfigured GitHub user
// id is stored as NULL rather than 0.
func nullableInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

// verifyEnrollmentVouch checks that the DeviceEnrolledEvent's VouchSignature
// is a valid ed25519 signature by the principal's Watchword-derived key over
// DeviceVouchBytes(deviceID, signPublicKey). The vouching key is resolved
// from the principals table (the local/own-machine case) with a fallback to
// the devices table scoped to the SAME principal (the teammate case: a
// member's root key reaches teammates via the grant vouch into devices, not
// via a principals row). Both lookups are principal-scoped, so only a key
// already trusted FOR that principal can vouch a new subkey into it. The
// VouchSigKeyID is validated against the resolved key id as an additional
// consistency guard.
func verifyEnrollmentVouch(ctx context.Context, db *sql.DB, dev ontology.DeviceEnrolledEvent) error {
	var principalPubKeyB64, storedSigKeyID string
	err := db.QueryRowContext(ctx,
		`SELECT sign_public_key, sig_key_id FROM principals WHERE principal_uid = ?`,
		dev.PrincipalUID,
	).Scan(&principalPubKeyB64, &storedSigKeyID)
	if errors.Is(err, sql.ErrNoRows) && dev.VouchSigKeyID != "" {
		// Only a ROOT enrollment (no vouch fields — the Watchword-derived key
		// by construction) may vouch: a subkey can never vouch further
		// subkeys, so compromising one device cannot mint replacement keys
		// that would survive that device's revocation.
		err = db.QueryRowContext(ctx,
			`SELECT sign_public_key, sig_key_id FROM devices
			 WHERE sig_key_id = ? AND principal_uid = ? AND vouch_signature IS NULL`,
			dev.VouchSigKeyID, dev.PrincipalUID,
		).Scan(&principalPubKeyB64, &storedSigKeyID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("principal %q not found; cannot verify vouch", dev.PrincipalUID)
	}
	if err != nil {
		return fmt.Errorf("query principal for vouch verification: %w", err)
	}

	// Confirm the VouchSigKeyID matches what is on record — a mismatch means
	// the event was tampered or the wrong key was referenced.
	if dev.VouchSigKeyID != "" && dev.VouchSigKeyID != storedSigKeyID {
		return fmt.Errorf("vouchSigKeyId %q does not match principal's stored sig_key_id %q",
			dev.VouchSigKeyID, storedSigKeyID)
	}

	principalPub, err := base64.StdEncoding.DecodeString(principalPubKeyB64)
	if err != nil {
		return fmt.Errorf("decode principal sign_public_key: %w", err)
	}
	if len(principalPub) != ed25519.PublicKeySize {
		return fmt.Errorf("principal sign_public_key is %d bytes, want %d", len(principalPub), ed25519.PublicKeySize)
	}
	vouchSig, err := base64.StdEncoding.DecodeString(dev.VouchSignature)
	if err != nil {
		return fmt.Errorf("decode vouchSignature: %w", err)
	}
	msg := ontology.DeviceVouchBytes(dev.DeviceID, dev.SignPublicKey)
	if !ed25519.Verify(ed25519.PublicKey(principalPub), msg, vouchSig) {
		return errors.New("vouch signature does not verify against principal's signing key")
	}
	return nil
}

// applyDeviceRevoked projects a deviceRevoked event into the device_revocations
// table. The revocation record is keyed by event_uid (ULID) so the
// position-restricted fold can compare against any event's UID.
//
// The row is inserted unconditionally — it is a CANDIDATE, not a verdict.
// Authority is NOT decided here: grafted events reach this applier without
// passing the live-append gate, so trusting the row's existence would let any
// validly-signed member revoke arbitrary devices. DeviceRevokedAt evaluates
// authority (self-or-admin at the revocation's own position) per row at
// verdict time, which keeps the outcome a pure function of the event set.
func (d *Dispatcher) applyDeviceRevoked(ctx context.Context, ev kernel.Event) (retErr error) {
	var rev ontology.DeviceRevokedEvent
	if err := json.Unmarshal(ev.Payload, &rev); err != nil {
		return fmt.Errorf("decode deviceRevoked payload: %w", err)
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_revocations
		     (event_uid, device_id, sig_key_id, principal_uid,
		      author_principal, revoked_at, create_time, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(event_uid) DO NOTHING`,
		ev.UID,
		rev.DeviceID,
		rev.SigKeyID,
		rev.PrincipalUID,
		ev.Author.PrincipalUID,
		rev.RevokedAt.UTC().Format(time.RFC3339Nano),
		ev.CreateTime.UTC().Format(time.RFC3339Nano),
		nullableText(rev.Reason),
	); err != nil {
		return fmt.Errorf("insert device_revocations: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// GetDeviceByID returns the DeviceInfo for the given deviceID across all
// principals. Returns (DeviceInfo{}, false, nil) when no device is found.
// Used by the revoke flow when only the deviceID is known.
func (d *Dispatcher) GetDeviceByID(ctx context.Context, deviceID string) (DeviceInfo, bool, error) {
	var di DeviceInfo
	var createTimeStr string
	var deviceLabel, vouchSigKeyID sql.NullString
	err := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT device_id, principal_uid, sig_key_id,
		        COALESCE(device_label, ''),
		        COALESCE(vouch_sig_key_id, ''),
		        create_time
		 FROM devices WHERE device_id = ?`,
		deviceID,
	).Scan(&di.DeviceID, &di.PrincipalUID, &di.SigKeyID, &deviceLabel, &vouchSigKeyID, &createTimeStr)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceInfo{}, false, nil
	}
	if err != nil {
		return DeviceInfo{}, false, fmt.Errorf("query device by id: %w", err)
	}
	if deviceLabel.Valid {
		di.DeviceLabel = deviceLabel.String
	}
	if vouchSigKeyID.Valid {
		di.VouchSigKeyID = vouchSigKeyID.String
	}
	ct, err := time.Parse(time.RFC3339Nano, createTimeStr)
	if err != nil {
		return DeviceInfo{}, false, fmt.Errorf("parse create_time %q: %w", createTimeStr, err)
	}
	di.CreateTime = ct.UTC()
	return di, true, nil
}

// DeviceRevokedAt reports whether the device identified by either deviceID or
// sigKeyID has been revoked by an AUTHORITY-APPROVED revocation event whose
// UID is strictly less than beforeEventUID. This is the position-pure verdict
// used by the convergent revocation fold: every peer sees the same result for
// the same event set regardless of arrival order.
//
// Authority is evaluated per revocation row, inside the fold — the live
// append gate alone is NOT sufficient, because a grafted revocation crafted
// by any validly-signed member would otherwise be honored unconditionally
// (letting a member revoke the owner's device). A revocation is honored only
// when its author is the target device's own principal — resolved from the
// devices projection, never from the revocation payload, which an attacker
// controls — or held owner/admin at the revocation's own position
// (TeamMemberRoleAt). A revocation whose target device is not (yet) enrolled
// resolves to not-honored, fail-closed: verdicts recompute on every pull and
// on rebuild, so it converges once the enrollment arrives.
func (d *Dispatcher) DeviceRevokedAt(ctx context.Context, deviceID, sigKeyID, beforeEventUID string) (bool, error) {
	rows, err := d.sqlite.Reader().QueryContext(ctx,
		`SELECT r.event_uid, r.author_principal, COALESCE(dev.principal_uid, '')
		 FROM device_revocations r
		 LEFT JOIN devices dev
		   ON (r.device_id != '' AND dev.device_id = r.device_id)
		   OR (r.device_id = '' AND r.sig_key_id != '' AND dev.sig_key_id = r.sig_key_id)
		 WHERE (r.device_id = ? OR r.sig_key_id = ?) AND r.event_uid < ?`,
		deviceID, sigKeyID, beforeEventUID,
	)
	if err != nil {
		return false, fmt.Errorf("query device_revocations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type revRow struct{ eventUID, author, targetPrincipal string }
	var candidates []revRow
	for rows.Next() {
		var r revRow
		if err := rows.Scan(&r.eventUID, &r.author, &r.targetPrincipal); err != nil {
			return false, fmt.Errorf("scan device_revocation: %w", err)
		}
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate device_revocations: %w", err)
	}

	for _, r := range candidates {
		// Self-revocation: the device's own principal per the enrollment
		// projection. Empty targetPrincipal = device not enrolled (yet) —
		// fail closed.
		if r.targetPrincipal != "" && r.author == r.targetPrincipal {
			return true, nil
		}
		// Admin revocation: the author held owner/admin at the revocation's
		// own position in the canonical order.
		role, ok, err := d.TeamMemberRoleAt(ctx, r.author, r.eventUID)
		if err != nil {
			return false, err
		}
		if ok && ontology.RoleAtLeast(role, ontology.RoleAdmin) {
			return true, nil
		}
	}
	return false, nil
}

// DeviceInfo is the projected view of a device returned by ListDevices.
type DeviceInfo struct {
	DeviceID      string
	PrincipalUID  string
	SigKeyID      string
	DeviceLabel   string
	VouchSigKeyID string
	CreateTime    time.Time
	RevokedAt     *time.Time
	RevokedReason string
}

// ListDevices returns all devices enrolled under principalUID, annotated with
// revocation status. When a device has been revoked, the earliest revocation
// record by event_uid is surfaced.
func (d *Dispatcher) ListDevices(ctx context.Context, principalUID string) ([]DeviceInfo, error) {
	// The inner SELECT picks the earliest (lowest event_uid) revocation record
	// per device_id using ROW_NUMBER so we get consistent revoked_at and reason
	// from the same row, not a SQLite arbitrary-row mix under GROUP BY.
	rows, err := d.sqlite.Reader().QueryContext(ctx,
		`SELECT d.device_id, d.principal_uid, d.sig_key_id,
		        COALESCE(d.device_label, ''),
		        COALESCE(d.vouch_sig_key_id, ''),
		        d.create_time,
		        dr.revoked_at,
		        COALESCE(dr.reason, '')
		 FROM devices d
		 LEFT JOIN (
		     SELECT device_id, sig_key_id, revoked_at, reason
		     FROM (
		         SELECT device_id, sig_key_id, revoked_at, reason,
		                ROW_NUMBER() OVER (PARTITION BY device_id ORDER BY event_uid ASC) AS rn
		         FROM device_revocations
		     ) WHERE rn = 1
		 ) dr ON (dr.device_id = d.device_id OR dr.sig_key_id = d.sig_key_id)
		 WHERE d.principal_uid = ?
		 ORDER BY d.create_time ASC`,
		principalUID,
	)
	if err != nil {
		return nil, fmt.Errorf("query devices: %w", err)
	}
	defer rows.Close()

	var out []DeviceInfo
	for rows.Next() {
		var di DeviceInfo
		var createTimeStr string
		var revokedAtStr, reason sql.NullString
		if err := rows.Scan(
			&di.DeviceID, &di.PrincipalUID, &di.SigKeyID,
			&di.DeviceLabel, &di.VouchSigKeyID,
			&createTimeStr,
			&revokedAtStr,
			&reason,
		); err != nil {
			return nil, fmt.Errorf("scan device row: %w", err)
		}
		ct, err := time.Parse(time.RFC3339Nano, createTimeStr)
		if err != nil {
			return nil, fmt.Errorf("parse create_time %q: %w", createTimeStr, err)
		}
		di.CreateTime = ct.UTC()
		if revokedAtStr.Valid && revokedAtStr.String != "" {
			rt, err := time.Parse(time.RFC3339Nano, revokedAtStr.String)
			if err != nil {
				return nil, fmt.Errorf("parse revoked_at %q: %w", revokedAtStr.String, err)
			}
			t := rt.UTC()
			di.RevokedAt = &t
		}
		if reason.Valid {
			di.RevokedReason = reason.String
		}
		out = append(out, di)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate devices: %w", err)
	}
	return out, nil
}
