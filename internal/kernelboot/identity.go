package kernelboot

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// githubAnchorTimeout bounds the first-boot GitHub cross-anchor step (gh/git
// subprocesses plus two HTTPS round trips) so a wedged subprocess or an
// unreachable github.com degrades to GitHubAnchorNone instead of stalling boot.
const githubAnchorTimeout = 20 * time.Second

// signerSettableLog is the subset of *kernel.FileEventLog the genesis
// ceremony needs to install the real (Watchword-derived) signer once the
// principal UID is known. Tests using a memory log that does not implement
// it simply keep the log's existing signer.
type signerSettableLog interface {
	SetSigner(kernel.Signer)
}

// SeedIdentity provisions the device's signed identity on first boot and
// installs the real event signer on every boot. It is idempotent and
// resilient: a missing Watchword (pre-wizard) or an unreachable GitHub is
// non-fatal — the binding is simply deferred or downgraded.
//
// Flow:
//  1. Initialize keys + device id (deriving from the Watchword on a cold
//     keychain). No Watchword yet -> return nil (genesis runs after the
//     wizard stores it).
//  2. Resolve the principal UID: the local binding, else a recovered one
//     from the devices table (state.toml lost but log replayed), else a
//     freshly minted ULID (true first boot).
//  3. Install the real signer so genesis events and all later writes are
//     self-authored.
//  4. On true first boot only: resolve + cross-anchor the GitHub identity
//     (best-effort), append principalRegistered + self-signed
//     deviceEnrolled, and persist the local self->principal binding.
//
// db is the SQLite writer pool (same dataDir as log); anchorer may be nil
// to skip the GitHub anchor entirely.
func SeedIdentity(
	ctx context.Context,
	log kernel.EventLog,
	db *sql.DB,
	idStore *identity.Store,
	anchorer *identity.GitHubAnchorer,
	dataDir string,
) error {
	id, err := idStore.Initialize(ctx)
	if err != nil {
		if errors.Is(err, identity.ErrNoWatchword) {
			return nil // no Watchword yet; genesis identity is deferred to a later boot
		}
		return fmt.Errorf("kernelboot: initialize identity: %w", err)
	}

	principalUID, err := resolvePrincipalUID(ctx, db, dataDir, idStore, id)
	if err != nil {
		return err
	}

	// Install the real signer for this principal so genesis events (and
	// every subsequent write) carry a concrete authorship block.
	if settable, ok := log.(signerSettableLog); ok {
		signer, serr := idStore.EventSigner(ctx, principalUID)
		if serr != nil {
			return fmt.Errorf("kernelboot: build event signer: %w", serr)
		}
		settable.SetSigner(signer)
	}

	// Append genesis events only when this principal has never been registered
	// in the log. Keying off the persisted principals projection (not the local
	// binding) keeps re-stamp migration consistent: `maz migrate sign-events`
	// binds + re-stamps the legacy log under this principal but does NOT append
	// genesis, so the daemon appends it here under the SAME principal UID.
	if principalRegistered(ctx, db, principalUID) {
		// The principal exists but this device's ACTIVE key may not be
		// enrolled yet (a Mellon-paired subkey device on its first boot after
		// the log synced) or may not have reached teammates (team membership
		// established after enrollment). Both are repaired here, every boot,
		// idempotently.
		return ensureDeviceEnrolled(ctx, log, db, idStore, principalUID)
	}
	return appendGenesisIdentity(ctx, log, idStore, anchorer, id, principalUID)
}

// resolvePrincipalUID determines the principal UID for this device and ensures
// the local binding is established: the existing binding, else a UID recovered
// from the devices table (binding lost but log replayed), else a freshly
// minted ULID. Mint and recover both persist the binding before returning, so
// the principal UID is decided exactly once per device regardless of whether
// genesis events have been appended yet.
func resolvePrincipalUID(ctx context.Context, db *sql.DB, dataDir string, idStore *identity.Store, id identity.Identity) (string, error) {
	bound, err := config.SelfPrincipalUID(dataDir)
	if err != nil {
		return "", fmt.Errorf("kernelboot: read self binding: %w", err)
	}
	if bound != "" {
		return bound, nil
	}
	if recovered, ok := lookupPrincipalByDevice(ctx, db, id.SigKeyID); ok {
		// Binding was lost (e.g. state.toml deleted) but the genesis events
		// replayed into the devices table: recover rather than mint a new
		// principal. Re-persist the binding so the next boot is fast.
		//
		// id.SigKeyID is always the Watchword-derived fingerprint, so on a
		// SUBKEY device the matched row is the principal's device-0
		// enrollment, not this device: recover the PRINCIPAL from it but keep
		// the local device id — a Mellon-paired device is a distinct device,
		// not a resurrection of device 0.
		hasSub, herr := idStore.HasDeviceSubkey(ctx)
		if herr != nil {
			return "", fmt.Errorf("kernelboot: check device subkey: %w", herr)
		}
		boundDevice := recovered.deviceID
		if hasSub {
			boundDevice = id.DeviceID
		}
		if berr := config.BindSelfPrincipal(dataDir, recovered.principalUID, boundDevice, id.SigKeyID); berr != nil {
			return "", fmt.Errorf("kernelboot: rebind recovered principal: %w", berr)
		}
		return recovered.principalUID, nil
	}
	minted, err := newGenesisULID()
	if err != nil {
		return "", err
	}
	// Bind the freshly minted principal immediately so the decision survives a
	// failure to append genesis (the next boot reuses it and retries the append
	// because the principals row is still absent).
	if berr := config.BindSelfPrincipal(dataDir, minted, id.DeviceID, id.SigKeyID); berr != nil {
		return "", fmt.Errorf("kernelboot: bind minted principal: %w", berr)
	}
	return minted, nil
}

// principalRegistered reports whether the principals projection already holds a
// row for principalUID, i.e. its genesis principalRegistered event is in the
// log. A missing table (pre-migration) or query error yields false so genesis
// proceeds rather than being silently skipped.
func principalRegistered(ctx context.Context, db *sql.DB, principalUID string) bool {
	if db == nil {
		return false
	}
	var got string
	err := db.QueryRowContext(ctx,
		`SELECT principal_uid FROM principals WHERE principal_uid = ?`, principalUID).Scan(&got)
	return err == nil && got != ""
}

// recoveredPrincipal carries the device row fields used to recover a lost
// local binding.
type recoveredPrincipal struct {
	principalUID string
	deviceID     string
}

// lookupPrincipalByDevice returns the principal + device bound to sigKeyID
// in the devices table, if any. A missing table (pre-migration) or absent
// row yields ok=false; query errors are swallowed to ok=false because this
// is a best-effort recovery probe.
func lookupPrincipalByDevice(ctx context.Context, db *sql.DB, sigKeyID string) (recoveredPrincipal, bool) {
	if db == nil {
		return recoveredPrincipal{}, false
	}
	var r recoveredPrincipal
	err := db.QueryRowContext(ctx,
		`SELECT principal_uid, device_id FROM devices WHERE sig_key_id = ?`, sigKeyID).
		Scan(&r.principalUID, &r.deviceID)
	if err != nil || r.principalUID == "" {
		return recoveredPrincipal{}, false
	}
	return r, true
}

// appendGenesisIdentity builds and appends the principalRegistered +
// self-signed deviceEnrolled events. The local binding is already persisted by
// resolvePrincipalUID, so this only appends. The GitHub anchor is best-effort:
// failure downgrades to GitHubAnchorNone.
func appendGenesisIdentity(
	ctx context.Context,
	log kernel.EventLog,
	idStore *identity.Store,
	anchorer *identity.GitHubAnchorer,
	id identity.Identity,
	principalUID string,
) error {
	now := time.Now().UTC()
	signPubB64 := base64.StdEncoding.EncodeToString(id.SignPublic)
	wrapPubB64 := base64.StdEncoding.EncodeToString(id.WrapPublic.Bytes())

	principal := ontology.PrincipalRegisteredEvent{
		PrincipalUID:  principalUID,
		DisplayName:   "Me",
		SignPublicKey: signPubB64,
		WrapPublicKey: wrapPubB64,
		SigKeyID:      id.SigKeyID,
		CreateTime:    now,
		GitHubAnchor:  ontology.GitHubAnchorNone,
	}
	applyGitHubAnchor(ctx, anchorer, id, &principal)

	// Self-signed device enrollment (TOFU root): the principal's own key
	// signs the canonical enrollment bytes.
	signKey, err := idStore.SigningKey(ctx)
	if err != nil {
		return fmt.Errorf("kernelboot: load signing key: %w", err)
	}
	enrollSig := ed25519.Sign(signKey, ontology.DeviceEnrollmentBytes(id.DeviceID, principalUID, signPubB64, id.SigKeyID))
	device := ontology.DeviceEnrolledEvent{
		DeviceID:      id.DeviceID,
		PrincipalUID:  principalUID,
		SignPublicKey: signPubB64,
		SigKeyID:      id.SigKeyID,
		DeviceLabel:   hostnameOrEmpty(),
		Signature:     base64.StdEncoding.EncodeToString(enrollSig),
		CreateTime:    now,
	}

	events, err := genesisIdentityEvents(&principal, &device)
	if err != nil {
		return err
	}
	if _, err := log.Append(ctx, events); err != nil {
		return fmt.Errorf("kernelboot: append genesis identity events: %w", err)
	}
	return nil
}

// ensureDeviceEnrolled appends deviceEnrolled events for this device's ACTIVE
// signing key wherever they are still missing — the second half of Mellon
// pairing. A paired device mints its random subkey at pair time but cannot
// enroll it until the principal is known, which happens on the first boot
// after the personal-scope log has synced (resolvePrincipalUID recovers the
// principal via the Watchword-key row). Two publications, each latched
// independently:
//
//   - PERSONAL scope, when the devices projection has no row for the active
//     key: the user's own devices learn the subkey (attribution, revocation).
//   - TEAM scope, when the principal is an admitted non-reader member and no
//     team-scoped enrollment has applied yet (the team_published latch):
//     teammates can only verify this key's events once the enrollment reaches
//     them through the team log.
//
// Device 0's Watchword key is enrolled by genesis and reaches teammates via
// the grant vouch, so both publications are no-ops there, as they are on
// every later boot once the latches are set.
func ensureDeviceEnrolled(ctx context.Context, log kernel.EventLog, db *sql.DB, idStore *identity.Store, principalUID string) error {
	pub, err := idStore.DeviceSigningPublicKey(ctx)
	if err != nil {
		return fmt.Errorf("kernelboot: device signing key: %w", err)
	}
	kid := identity.SigKeyID(pub)
	enrolled, teamPublished := deviceEnrollmentState(ctx, db, kid)
	needTeam := !teamPublished && teamMemberCanWrite(ctx, db, principalUID)
	if enrolled && !needTeam {
		return nil
	}

	dev, err := idStore.DeviceEnroll(ctx, principalUID, hostnameOrEmpty())
	if err != nil {
		return fmt.Errorf("kernelboot: build device enrollment: %w", err)
	}
	// Only a vouched subkey may be published to team scope. Device 0's Watchword
	// key is vouchless; it reaches teammates through the grant vouch, and
	// applyDeviceEnrolled drops a vouchless team-scoped enrollment. Publishing
	// one would never latch team_published, so the seeder would re-emit it on
	// every boot and `maz rebuild` would fail to replay it (bug F8).
	if dev.VouchSignature == "" {
		needTeam = false
	}
	if enrolled && !needTeam {
		return nil
	}

	var events []kernel.Event
	if !enrolled {
		ev, merr := makeGenesisEvent(ontology.KindDeviceEnrolled, &dev)
		if merr != nil {
			return fmt.Errorf("kernelboot: marshal deviceEnrolled: %w", merr)
		}
		events = append(events, ev)
	}
	if needTeam {
		ev, merr := makeGenesisEvent(ontology.KindDeviceEnrolled, &dev)
		if merr != nil {
			return fmt.Errorf("kernelboot: marshal team deviceEnrolled: %w", merr)
		}
		ev.Scope = kernel.ScopeTeam
		events = append(events, ev)
	}
	if _, aerr := log.Append(ctx, events); aerr != nil {
		return fmt.Errorf("kernelboot: append deviceEnrolled: %w", aerr)
	}
	return nil
}

// deviceEnrollmentState reports whether the active key has a devices row and
// whether any applied enrollment for it arrived in team scope. Best-effort
// like lookupPrincipalByDevice: a missing table or query error reads as
// not-enrolled so the seeder appends (the apply layer is idempotent).
func deviceEnrollmentState(ctx context.Context, db *sql.DB, sigKeyID string) (enrolled, teamPublished bool) {
	if db == nil {
		return false, false
	}
	var tp int
	err := db.QueryRowContext(ctx,
		`SELECT team_published FROM devices WHERE sig_key_id = ?`, sigKeyID).Scan(&tp)
	if err != nil {
		return false, false
	}
	return true, tp != 0
}

// teamMemberCanWrite reports whether principalUID is an admitted team member
// whose role permits team-scope writes (role ≥ member). Readers cannot
// append their enrollment to the team log — and have nothing to verify there
// anyway. Best-effort: a missing ACL table or query error reads as false.
func teamMemberCanWrite(ctx context.Context, db *sql.DB, principalUID string) bool {
	if db == nil {
		return false
	}
	var role string
	err := db.QueryRowContext(ctx,
		`SELECT role FROM team_membership_acl WHERE principal_uid = ?`, principalUID).Scan(&role)
	if err != nil {
		return false
	}
	return ontology.RoleAtLeast(role, ontology.RoleMember)
}

// applyGitHubAnchor resolves the operator's GitHub identity and attaches a
// verified SSHSIG attestation to the principal. Every failure mode
// (anchorer nil, no GitHub identity, no published signing key, offline) is
// non-fatal: the principal keeps GitHubAnchorNone and can be upgraded on a
// later boot.
func applyGitHubAnchor(ctx context.Context, anchorer *identity.GitHubAnchorer, id identity.Identity, principal *ontology.PrincipalRegisteredEvent) {
	if anchorer == nil {
		return
	}
	// Bound the whole anchor step (gh/git subprocesses + two HTTPS round
	// trips) so a wedged subprocess or a slow github.com cannot stall boot.
	ctx, cancel := context.WithTimeout(ctx, githubAnchorTimeout)
	defer cancel()

	ghID, ok := anchorer.ResolveIdentity(ctx)
	if !ok {
		return
	}
	principal.SameAs = "github:" + ghID.Login
	principal.GitHubUserID = ghID.UserID
	principal.DisplayName = ghID.Login

	att, err := anchorer.Anchor(ctx, principal.PrincipalUID, ghID, id.SignPublic)
	if err != nil {
		return // sameAs recorded but unverified; anchor stays "none"
	}
	principal.GitHubAnchor = ontology.GitHubAnchorSSH
	principal.GitHubAttestation = att.Signature
	principal.GitHubAnchorKey = att.SignerKey
}

// genesisIdentityEvents marshals the two payloads into personal-scope
// kernel events. Personal scope keeps the public keys syncing across the
// user's own devices without team verification cost.
func genesisIdentityEvents(principal *ontology.PrincipalRegisteredEvent, device *ontology.DeviceEnrolledEvent) ([]kernel.Event, error) {
	principalEv, err := makeGenesisEvent(ontology.KindPrincipalRegistered, principal)
	if err != nil {
		return nil, fmt.Errorf("kernelboot: marshal principalRegistered: %w", err)
	}
	deviceEv, err := makeGenesisEvent(ontology.KindDeviceEnrolled, device)
	if err != nil {
		return nil, fmt.Errorf("kernelboot: marshal deviceEnrolled: %w", err)
	}
	return []kernel.Event{principalEv, deviceEv}, nil
}

// SignerForReStamp ensures the device identity exists and returns a signer for
// the bound self principal, minting + binding one when the device is unbound.
// `maz migrate sign-events` uses it to sign legacy events under the SAME
// principal the daemon will register at genesis on the next boot (the binding
// is the shared anchor). It performs no DB recovery: a legacy SQLite projection
// has no principals/devices rows yet. Returns identity.ErrNoWatchword when the
// Watchword has not been set (the operator must run `maz init` first).
//
//nolint:ireturn // returns the kernel.Signer interface by design; the concrete type is internal to identity.
func SignerForReStamp(ctx context.Context, idStore *identity.Store, dataDir string) (kernel.Signer, error) {
	id, err := idStore.Initialize(ctx)
	if err != nil {
		return nil, fmt.Errorf("kernelboot: initialize identity: %w", err)
	}
	principalUID, err := config.SelfPrincipalUID(dataDir)
	if err != nil {
		return nil, fmt.Errorf("kernelboot: read self binding: %w", err)
	}
	if principalUID == "" {
		principalUID, err = newGenesisULID()
		if err != nil {
			return nil, err
		}
		if berr := config.BindSelfPrincipal(dataDir, principalUID, id.DeviceID, id.SigKeyID); berr != nil {
			return nil, fmt.Errorf("kernelboot: bind minted principal: %w", berr)
		}
	}
	signer, err := idStore.EventSigner(ctx, principalUID)
	if err != nil {
		return nil, fmt.Errorf("kernelboot: build event signer: %w", err)
	}
	return signer, nil
}

// newGenesisULID mints a fresh ULID for a principal.
func newGenesisULID() (string, error) {
	id, err := ulid.New(ulid.Now(), ulid.Monotonic(rand.Reader, math.MaxUint32))
	if err != nil {
		return "", fmt.Errorf("kernelboot: mint principal ulid: %w", err)
	}
	return id.String(), nil
}

// hostnameOrEmpty returns the OS hostname, or "" when unavailable.
func hostnameOrEmpty() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}

// UpgradeGitHubAnchor re-attempts the GitHub cross-anchor for the bound
// principal when it was registered without one (githubAnchor:"none" — minted
// offline, without `gh`, or without a published signing key). It is idempotent
// and best-effort, meant to run on every daemon boot right after SeedIdentity:
// a no-op when there is no bound principal, the principal is already anchored,
// no anchorer is configured, or the anchor attempt still fails (still offline /
// no key — left for a later boot). On success it appends a NEW principalRegistered
// revision carrying the verified attestation; the dispatcher's idempotent upsert
// folds it onto the principals row on replay.
//
// Db is the SQLite writer pool (same dataDir as log); anchorer may be nil to
// skip the upgrade entirely.
func UpgradeGitHubAnchor(
	ctx context.Context,
	log kernel.EventLog,
	db *sql.DB,
	idStore *identity.Store,
	anchorer *identity.GitHubAnchorer,
	dataDir string,
) error {
	if anchorer == nil || db == nil {
		return nil
	}
	principalUID, err := config.SelfPrincipalUID(dataDir)
	if err != nil {
		return fmt.Errorf("kernelboot: read self binding: %w", err)
	}
	if principalUID == "" {
		return nil // genesis has not run yet; nothing to upgrade
	}

	anchored, exists, err := principalAnchorStatus(ctx, db, principalUID)
	if err != nil {
		return err
	}
	if !exists || anchored {
		return nil // not registered yet, or already anchored — nothing to do
	}

	id, err := idStore.Initialize(ctx)
	if err != nil {
		if errors.Is(err, identity.ErrNoWatchword) {
			return nil // keys not provisioned; cannot anchor
		}
		return fmt.Errorf("kernelboot: initialize identity: %w", err)
	}

	// Rebuild the principal payload from the derived identity (the keys are
	// deterministic, so this reproduces the genesis fields) and attempt the
	// anchor. If it still cannot anchor (offline / no published key), leave the
	// principal as-is rather than appending a redundant "none" revision.
	revision := ontology.PrincipalRegisteredEvent{
		PrincipalUID:  principalUID,
		DisplayName:   "Me",
		SignPublicKey: base64.StdEncoding.EncodeToString(id.SignPublic),
		WrapPublicKey: base64.StdEncoding.EncodeToString(id.WrapPublic.Bytes()),
		SigKeyID:      id.SigKeyID,
		CreateTime:    time.Now().UTC(),
		GitHubAnchor:  ontology.GitHubAnchorNone,
	}
	applyGitHubAnchor(ctx, anchorer, id, &revision)
	if revision.GitHubAnchor != ontology.GitHubAnchorSSH {
		return nil
	}

	ev, err := makeGenesisEvent(ontology.KindPrincipalRegistered, &revision)
	if err != nil {
		return err
	}
	if _, err := log.Append(ctx, []kernel.Event{ev}); err != nil {
		return fmt.Errorf("kernelboot: append github anchor upgrade: %w", err)
	}
	return nil
}

// principalAnchorStatus reports whether a principals row exists for principalUID
// and whether it is already GitHub-anchored. A missing row yields
// (anchored=false, exists=false, nil). A missing table (pre-migration) or query
// error is returned so the caller can surface it.
func principalAnchorStatus(ctx context.Context, db *sql.DB, principalUID string) (anchored, exists bool, err error) {
	var anchor string
	qerr := db.QueryRowContext(ctx,
		`SELECT github_anchor FROM principals WHERE principal_uid = ?`, principalUID).Scan(&anchor)
	if errors.Is(qerr, sql.ErrNoRows) {
		return false, false, nil
	}
	if qerr != nil {
		return false, false, fmt.Errorf("kernelboot: read principal anchor status: %w", qerr)
	}
	return anchor != ontology.GitHubAnchorNone, true, nil
}
