// Package team implements the team-scope membership and envelope-key
// lifecycle for issue #213: creating a team (genesis), granting a member
// (wrapping the current data-encryption key to their pasted public key),
// and revoking a member (forward re-key — mint a fresh DEK at the next
// epoch and re-wrap it to every remaining member).
//
// The service is deliberately decoupled from concrete storage: it talks to
// an event Appender, a wrap-key provider, a keychain SecretStore, a
// MembershipReader (the derived ACL), the self->principal binding, and a
// ConfigMutator. The daemon wires the real implementations; tests pass
// fakes. The cryptographic authority on every appended event is its SIGNED
// EventAuthor (stamped + verified by the event log), never a payload field —
// this service only assembles payloads and lets the append boundary enforce
// who may write to the team scope.
package team

import (
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// ErrTeamExists is returned by Create when a team genesis already exists.
var ErrTeamExists = errors.New("team: a team already exists (genesis grant present)")

// ErrNoTeam is returned by Grant/Revoke when no team has been created yet.
var ErrNoTeam = errors.New("team: no team exists yet; run `maz team create` first")

// ErrSelfUnbound is returned when the local self->principal binding is not
// provisioned, so there is no principal to author team events as.
var ErrSelfUnbound = errors.New("team: this device has no bound principal; complete identity genesis first")

// ErrNotMember is returned by Grant/Revoke when the local principal is not an
// admitted team member (a pre-check; the append boundary enforces it too).
var ErrNotMember = errors.New("team: the local principal is not an admitted team member")

// ErrInsufficientRole is returned when the local principal's role does not
// meet the minimum required for the requested operation (e.g. a member
// trying to grant, or a non-owner trying to change an admin's role).
var ErrInsufficientRole = errors.New("team: insufficient role for this operation")

// Appender appends events to the kernel event log. *kernel.FileEventLog
// satisfies it; the team-scope write authority + author signing run inside
// Append, so this service never stamps authors itself.
type Appender interface {
	Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error)
}

// WrapKeyProvider yields the local device's public identity: the x25519
// key-wrapping public key the DEK is sealed to, plus (via Initialize) the
// signing key + ids a grant vouches for so peers can verify this device's team
// events. *identity.Store satisfies it; Initialize is idempotent (provisions
// material if absent, returns the public summary otherwise).
type WrapKeyProvider interface {
	WrappingPublicKey(ctx context.Context) (*ecdh.PublicKey, error)
	Initialize(ctx context.Context) (identity.Identity, error)
}

// SecretStore is the keychain the per-epoch DEK is cached in (base64). The
// secret.Provider chain satisfies it.
type SecretStore interface {
	Get(ctx context.Context, name string) (string, error)
	Set(ctx context.Context, name, value string) error
}

// Member is one admitted ACL row, enough to re-wrap a fresh DEK on revoke.
type Member struct {
	PrincipalUID  string
	WrapPublicKey string // base64 x25519 public key
	DEKEpoch      int
	IsGenesis     bool
	Role          string // effective role: owner | admin | member | reader
}

// MembershipReader reads the derived team ACL. The dispatcher satisfies it
// (via an adapter that maps its rows to team.Member).
type MembershipReader interface {
	GenesisOwner(ctx context.Context) (string, error)
	IsTeamMember(ctx context.Context, principalUID string) (bool, error)
	// MemberRole returns the effective role for principalUID (one of
	// ontology.RoleOwner/Admin/Member/Reader), with ok=false when the
	// principal is not an admitted member.
	MemberRole(ctx context.Context, principalUID string) (role string, ok bool, err error)
	CurrentDEKEpoch(ctx context.Context) (int, error)
	AdmittedMembers(ctx context.Context) ([]Member, error)
	// PrincipalDisplayName returns the local display name for a principal UID
	// (from the principals projection), or "" when unknown. Used to stamp the
	// grantee's name into a grant so peers can materialize the member as a
	// team-scoped person entity. A miss is non-fatal (the entity still resolves
	// by UID, just without a friendly name).
	PrincipalDisplayName(ctx context.Context, principalUID string) (string, error)
}

// SelfResolver resolves the device-local self->principal binding.
// *config.SelfBinder satisfies it.
type SelfResolver interface {
	SelfPrincipal(ctx context.Context) (string, bool)
}

// ConfigMutator flips the team scope into encrypted envelope mode, persists
// the genesis trust anchor, and reads it back. A config-file-backed
// implementation lives in the wiring layer; tests pass a fake.
type ConfigMutator interface {
	// EnableTeamEnvelope sets the team scope to encrypted envelope mode and,
	// when remoteURL is non-empty, pins it as the team remote.
	EnableTeamEnvelope(ctx context.Context, remoteURL string) error
	// PersistGenesisPin writes the genesis trust anchor (principal UID + signing
	// key fingerprint) to state.toml, and on a genesis device also produces the
	// signed TEAM.json staging file so the next sync push copies it to the repo
	// root. Idempotent. The state.toml write always happens first; a returned
	// error means the TEAM.json staging step failed and callers may treat it as
	// non-fatal (the doctor check team.genesis_pin surfaces the gap).
	PersistGenesisPin(ctx context.Context, principalUID, sigKeyID string) error
	// LoadGenesisPin reads the genesis pin from state.toml. Returns ok=false when
	// no pin is stored (unpinned team or fresh state file). A missing state file
	// is not an error.
	LoadGenesisPin(ctx context.Context) (principalUID, sigKeyID string, ok bool, err error)
}

// DeviceInfo is a projected view of a device enrollment with optional
// revocation status, returned by DevicesList.
type DeviceInfo struct {
	DeviceID      string
	PrincipalUID  string
	SigKeyID      string
	DeviceLabel   string
	VouchSigKeyID string // non-empty for subkey devices
	CreateTime    time.Time
	RevokedAt     *time.Time // nil when not revoked
	RevokedReason string
}

// DeviceLister reads device enrollments from the projection store. The
// dispatcher satisfies it; the team package does not import the dispatcher
// directly.
type DeviceLister interface {
	// ListDevices returns all devices enrolled under principalUID, annotated
	// with revocation status (earliest revocation by UID).
	ListDevices(ctx context.Context, principalUID string) ([]DeviceInfo, error)
	// GetDeviceByID returns a device by its ULID across all principals.
	// Returns (DeviceInfo{}, false, nil) when no device is found.
	GetDeviceByID(ctx context.Context, deviceID string) (DeviceInfo, bool, error)
}

// Service orchestrates the team membership + envelope-key lifecycle.
type Service struct {
	appender Appender
	wrap     WrapKeyProvider
	secrets  SecretStore
	members  MembershipReader
	self     SelfResolver
	config   ConfigMutator
	devices  DeviceLister // optional; nil disables device management
	now      func() time.Time
}

// Deps bundles the Service's collaborators.
type Deps struct {
	Appender   Appender
	WrapKeys   WrapKeyProvider
	Secrets    SecretStore
	Membership MembershipReader
	Self       SelfResolver
	Config     ConfigMutator
	// Devices is the device projection reader. Optional; when nil the
	// DevicesList and DeviceRevoke methods return ErrNoTeam.
	Devices DeviceLister
	// Now is the clock; nil defaults to time.Now. Tests pin it.
	Now func() time.Time
}

// New constructs a Service. All deps except Now and Devices are required.
func New(d *Deps) (*Service, error) {
	if d == nil || d.Appender == nil || d.WrapKeys == nil || d.Secrets == nil ||
		d.Membership == nil || d.Self == nil || d.Config == nil {
		return nil, errors.New("team: all service dependencies are required")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		appender: d.Appender,
		wrap:     d.WrapKeys,
		secrets:  d.Secrets,
		members:  d.Membership,
		self:     d.Self,
		config:   d.Config,
		devices:  d.Devices,
		now:      now,
	}, nil
}

// dekKeychainKey is the keychain entry name for a given epoch's DEK.
func dekKeychainKey(epoch int) string {
	return watchword.KeychainTeamDEKPrefix + strconv.Itoa(epoch)
}

// CreateResult summarizes a team creation.
type CreateResult struct {
	GenesisPrincipalUID string
	DEKEpoch            int
	GrantEventUID       string
}

// Create provisions team genesis: it mints a fresh epoch-0 DEK, caches it in
// the keychain, seals it to the local principal's wrap key, appends the
// genesis self-grant (teamMemberGranted with grantee == author), and flips the
// team scope into encrypted envelope mode. It refuses if a team already exists.
func (s *Service) Create(ctx context.Context, remoteURL string) (CreateResult, error) {
	selfUID, ok := s.self.SelfPrincipal(ctx)
	if !ok || selfUID == "" {
		return CreateResult{}, ErrSelfUnbound
	}
	genesis, err := s.members.GenesisOwner(ctx)
	if err != nil {
		return CreateResult{}, fmt.Errorf("team: read genesis owner: %w", err)
	}
	if genesis != "" {
		return CreateResult{}, ErrTeamExists
	}

	// The genesis self-grant publishes the team's TOFU root signing key: it is
	// grantee==author, so the verifier self-vouches it (no prior devices row),
	// and its projection enrolls the owner's key so every later event verifies.
	// Provision + read the full local identity here so the grant carries it.
	id, err := s.wrap.Initialize(ctx)
	if err != nil {
		return CreateResult{}, fmt.Errorf("team: initialize local identity: %w", err)
	}
	dek, err := watchword.GenerateDEK()
	if err != nil {
		return CreateResult{}, fmt.Errorf("team: generate DEK: %w", err)
	}
	const epoch0 = 0
	if err := s.storeDEK(ctx, epoch0, dek); err != nil {
		return CreateResult{}, err
	}
	// Name is best-effort: a miss leaves the projected team entity nameless but
	// still resolvable by UID. The genesis self-grant carries the owner's name so
	// every peer can name the team's founder entity.
	selfName, nameErr := s.members.PrincipalDisplayName(ctx, selfUID)
	if nameErr != nil {
		selfName = ""
	}
	selfVouch := granteeVouch{
		signPublicKey: base64.StdEncoding.EncodeToString(id.SignPublic),
		sigKeyID:      id.SigKeyID,
		deviceID:      id.DeviceID,
		displayName:   selfName,
	}
	// Flip the local config to envelope mode BEFORE the durable genesis grant.
	// The signed grant is the team's source of truth (GenesisOwner gates Create's
	// idempotency above); the config flip is local and re-derivable. Enabling
	// first makes a partial Create recoverable: if the grant append fails,
	// GenesisOwner stays empty so Create can simply be re-run, and no team-scope
	// write can succeed in the meantime (enforceTeamWriteAuthority sees no admitted
	// member) so nothing leaks. The reverse order would strand a durable genesis
	// grant with the store NOT in envelope mode AND block retry with ErrTeamExists.
	if err := s.config.EnableTeamEnvelope(ctx, remoteURL); err != nil {
		return CreateResult{}, fmt.Errorf("team: enable envelope mode: %w", err)
	}
	uid, err := s.appendGrant(ctx, selfUID, id.WrapPublic.Bytes(), dek, epoch0, selfVouch, ontology.RoleOwner)
	if err != nil {
		return CreateResult{}, err
	}
	// Persist the genesis trust anchor to state.toml and stage the signed
	// TEAM.json file. Non-fatal: a failure here does not roll back the durable
	// genesis grant; the doctor check will WARN and the operator can re-pin.
	_ = s.config.PersistGenesisPin(ctx, selfUID, id.SigKeyID) //nolint:errcheck // non-fatal: doctor check will WARN if pin is absent
	return CreateResult{GenesisPrincipalUID: selfUID, DEKEpoch: epoch0, GrantEventUID: uid}, nil
}

// GrantResult summarizes a grant.
type GrantResult struct {
	GranteePrincipalUID string
	// DEKEpoch is the current (highest) epoch the grantee was admitted at.
	DEKEpoch int
	// GrantedEpochs lists every epoch whose DEK was wrapped to the grantee
	// (ascending). A full-history grant covers 0..DEKEpoch; it is shorter only
	// when this device is missing a prior epoch's DEK (cannot share what it
	// lacks).
	GrantedEpochs []int
	// GrantEventUID is the first grant event's UID (the batch lands atomically).
	GrantEventUID string
}

// Grant admits the principal named by key and gives them FULL HISTORY: it wraps
// every DEK epoch this device holds (0..current) to the grantee's x25519 wrap
// key and appends one teamMemberGranted per epoch — so the new member can read
// the entire shared brain, including data sealed before they joined and across
// past revoke re-keys, not just from the current epoch forward. Every per-epoch
// grant also vouches the grantee's ed25519 signing identity into the team (so
// peers can verify the new member's own team events once a grant's projection
// enrolls it; enrollment is idempotent, so vouching on each is safe and robust
// to partial replay). The join key is the trust input — the granter vouches for
// it out of band.
//
// If this device is missing a prior epoch's DEK (a legacy gap, or it joined
// before full-history grants existed), that epoch is skipped: you cannot share
// a key you do not hold, and the grantee simply does not get that slice of
// history. The grants land as ONE atomic batch.
// Grant admits the principal named by key at role. An empty role defaults to
// "member". Callers obtain the role from user input (CLI flag / MCP param).
func (s *Service) Grant(ctx context.Context, key *JoinKey, role string) (GrantResult, error) {
	if key == nil {
		return GrantResult{}, errors.New("team: join key is required")
	}
	if err := s.requireLocalRole(ctx, ontology.RoleAdmin); err != nil {
		return GrantResult{}, err
	}
	if key.PrincipalUID == "" {
		return GrantResult{}, errors.New("team: grantee principal UID is required")
	}
	pub, err := base64.StdEncoding.DecodeString(key.WrapPublicKey)
	if err != nil {
		return GrantResult{}, fmt.Errorf("team: grantee wrap public key is not valid base64: %w", err)
	}
	if _, err := ecdh.X25519().NewPublicKey(pub); err != nil {
		return GrantResult{}, fmt.Errorf("team: grantee wrap public key is not a valid x25519 key: %w", err)
	}
	if role == "" {
		role = ontology.RoleMember
	}
	current, err := s.members.CurrentDEKEpoch(ctx)
	if err != nil {
		return GrantResult{}, fmt.Errorf("team: read current epoch: %w", err)
	}
	vouch := vouchFromJoinKey(key)
	events := make([]kernel.Event, 0, current+1)
	granted := make([]int, 0, current+1)
	for epoch := 0; epoch <= current; epoch++ {
		dek, derr := s.loadDEK(ctx, epoch)
		if derr != nil {
			if errors.Is(derr, secret.ErrNotFound) {
				// This device genuinely does not hold epoch's DEK — skip it (cannot
				// share what we lack). The grantee gets every epoch we can wrap.
				continue
			}
			// A real key-store fault (I/O, base64 corruption, wrong size) must NOT
			// silently under-grant — fail loudly so the operator sees the broken
			// keychain rather than a member who can't decrypt part of the history.
			return GrantResult{}, fmt.Errorf("team: load epoch-%d DEK to grant: %w", epoch, derr)
		}
		ev, gerr := s.buildGrantEvent(key.PrincipalUID, pub, dek, epoch, vouch, role)
		if gerr != nil {
			return GrantResult{}, gerr
		}
		events = append(events, ev)
		granted = append(granted, epoch)
	}
	if len(events) == 0 {
		return GrantResult{}, errors.New("team: this device holds no team DEK to grant; run `maz sync pull` to bootstrap one first")
	}
	receipts, aerr := s.appender.Append(ctx, events)
	if aerr != nil {
		return GrantResult{}, fmt.Errorf("team: append grants: %w", aerr)
	}
	return GrantResult{
		GranteePrincipalUID: key.PrincipalUID,
		DEKEpoch:            current,
		GrantedEpochs:       granted,
		GrantEventUID:       receiptUID(receipts),
	}, nil
}

// RevokeResult summarizes a revoke + forward re-key.
type RevokeResult struct {
	RevokedPrincipalUID string
	// NewDEKEpoch is the freshly-minted epoch the remaining members were
	// re-keyed to. The revoked principal has no grant at this epoch.
	NewDEKEpoch int
	// RewrappedTo lists the principal UIDs the new DEK was re-wrapped to.
	RewrappedTo []string
}

// Revoke removes principalUID and performs a full forward re-key: it appends a
// teamMemberRevoked, mints a fresh DEK at epoch+1, caches it, and re-wraps it
// to every REMAINING admitted member (emitting a teamMemberGranted per member
// at the new epoch). Prior-epoch chunks stay readable to historical members;
// the revoked principal cannot read epoch+1 onward. The caller is responsible
// for the OTHER boundary — removing the principal's git-host write access —
// which Mazarbul cannot do.
func (s *Service) Revoke(ctx context.Context, principalUID, reason string) (RevokeResult, error) {
	if err := s.requireLocalRole(ctx, ontology.RoleAdmin); err != nil {
		return RevokeResult{}, err
	}
	if principalUID == "" {
		return RevokeResult{}, errors.New("team: revoked principal UID is required")
	}
	// Reject self-revoke: the revoke append would land, then every re-key grant
	// would be refused by the append-boundary authority (the caller is no longer
	// admitted), stranding the team with a tombstone and no forward re-key — and
	// the caller could not recover. Another member must revoke them.
	if selfUID, ok := s.self.SelfPrincipal(ctx); ok && selfUID == principalUID {
		return RevokeResult{}, errors.New("team: cannot self-revoke; ask another admitted member to revoke you")
	}
	genesis, err := s.members.GenesisOwner(ctx)
	if err != nil {
		return RevokeResult{}, fmt.Errorf("team: read genesis owner: %w", err)
	}
	if principalUID == genesis {
		return RevokeResult{}, errors.New("team: the genesis owner cannot be revoked")
	}

	// Snapshot the admitted set BEFORE the revoke so we re-key exactly the
	// members who remain. The fold is order-independent, but the re-key set is
	// a deterministic function of the pre-revoke ACL minus the revoked member.
	members, err := s.members.AdmittedMembers(ctx)
	if err != nil {
		return RevokeResult{}, fmt.Errorf("team: read admitted members: %w", err)
	}
	epoch, err := s.members.CurrentDEKEpoch(ctx)
	if err != nil {
		return RevokeResult{}, fmt.Errorf("team: read current epoch: %w", err)
	}
	newEpoch := epoch + 1
	newDEK, err := watchword.GenerateDEK()
	if err != nil {
		return RevokeResult{}, fmt.Errorf("team: generate re-key DEK: %w", err)
	}

	// Assemble the revoke + every re-key grant, then append as ONE batch so a
	// crash leaves either none or all of the lines — never a tombstone with no
	// forward re-key (which would strand the remaining members at the next push).
	// The revoke sorts below the re-grants by ULID within the batch, so the fold
	// supersedes deterministically. Cache the new DEK BEFORE the append: an
	// orphan keychain entry (append failed) is harmless, but a missing entry
	// after a successful append would leave the local sealer unable to seal the
	// new epoch.
	revokeEv, err := s.buildRevokeEvent(principalUID, reason)
	if err != nil {
		return RevokeResult{}, err
	}
	grantEvs, rewrapped, err := s.buildRekeyGrants(members, principalUID, newDEK, newEpoch)
	if err != nil {
		return RevokeResult{}, err
	}
	if err := s.storeDEK(ctx, newEpoch, newDEK); err != nil {
		return RevokeResult{}, err
	}
	batch := append([]kernel.Event{revokeEv}, grantEvs...)
	if _, err := s.appender.Append(ctx, batch); err != nil {
		return RevokeResult{}, fmt.Errorf("team: append revoke re-key batch: %w", err)
	}
	return RevokeResult{RevokedPrincipalUID: principalUID, NewDEKEpoch: newEpoch, RewrappedTo: rewrapped}, nil
}

// Members returns the current admitted ACL (CLI-local read; no PII).
func (s *Service) Members(ctx context.Context) ([]Member, error) {
	return s.members.AdmittedMembers(ctx)
}

// JoinResult summarizes a join (team-remote configuration).
type JoinResult struct {
	RemoteURL string
}

// Join configures THIS device's team scope to sync with remoteURL in encrypted
// envelope mode WITHOUT minting a genesis grant — the join-side counterpart to
// Create. It is how a prospective member points at an existing team's remote
// before being admitted: after Join, run `maz team key`, hand the printed blob
// to an admitted member to be granted, then `maz sync pull` bootstraps the DEK
// and decrypts the shared brain. Refuses when THIS device is already the team's
// genesis owner (it created the team; there is nothing to join).
func (s *Service) Join(ctx context.Context, remoteURL string) (JoinResult, error) {
	if remoteURL == "" {
		return JoinResult{}, errors.New("team: a remote URL is required to join a team")
	}
	genesis, err := s.members.GenesisOwner(ctx)
	if err != nil {
		return JoinResult{}, fmt.Errorf("team: read genesis owner: %w", err)
	}
	if genesis != "" {
		if selfUID, ok := s.self.SelfPrincipal(ctx); ok && selfUID == genesis {
			return JoinResult{}, errors.New("team: this device is the team's genesis owner; it created the team and does not join it")
		}
	}
	if err := s.config.EnableTeamEnvelope(ctx, remoteURL); err != nil {
		return JoinResult{}, fmt.Errorf("team: configure team remote: %w", err)
	}
	return JoinResult{RemoteURL: remoteURL}, nil
}

// JoinKey is the local device's join request. It carries everything an admitted
// member needs to grant the holder AND to vouch the holder's verification
// identity into the team: the principal UID, the base64 x25519 wrap public key
// the DEK is sealed to, and the holder's ed25519 signing public key + its stable
// key id + device id (so peers can verify the holder's own team-scope events
// once a grant enrolls them). A prospective member produces it with
// `maz team key` and hands the encoded blob to an admitted member out of band;
// nothing here is secret.
type JoinKey struct {
	PrincipalUID  string `json:"principalUid"`
	WrapPublicKey string `json:"wrapPublicKey"` // base64 x25519 public key
	SignPublicKey string `json:"signPublicKey"` // base64 ed25519 public key
	SigKeyID      string `json:"sigKeyId"`
	DeviceID      string `json:"deviceId"`
	// DisplayName is the prospective member's self-reported person name. It is
	// carried into the grant so peers can materialize the member as a team-scoped
	// person entity (so self-scoped team facts resolve their subject). Optional.
	DisplayName string `json:"displayName,omitempty"`
	// GenesisPin is the team genesis trust anchor carried by the join key when
	// the producing device already holds one. NOTE the current invite flow runs
	// joiner→granter (the joiner produces this key and the admin consumes it),
	// so a FRESH joiner's key carries a nil pin and the joiner instead pins from
	// the repo's verified TEAM.json on its first team pull. The field exists so
	// a future granter→joiner invite token can deliver the pin out-of-band, and
	// so re-issued keys from already-pinned devices carry the anchor. Backward
	// compatible: DecodeJoinKey leaves the field nil when the JSON key is
	// absent, so old-format tokens still parse without error.
	GenesisPin *ontology.GenesisPin `json:"genesisPin,omitempty"`
}

// Encode renders the join key as a single opaque token the holder pastes into
// `maz team grant --join-key`. It is base64(JSON), not secret — it carries only
// public keys and ids.
func (k *JoinKey) Encode() (string, error) {
	raw, err := json.Marshal(k)
	if err != nil {
		return "", fmt.Errorf("team: marshal join key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// DecodeJoinKey parses a token produced by JoinKey.Encode, rejecting one missing
// the principal UID or wrap public key (the minimum a grant needs).
func DecodeJoinKey(token string) (JoinKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return JoinKey{}, fmt.Errorf("team: join key is not valid base64: %w", err)
	}
	var k JoinKey
	if err := json.Unmarshal(raw, &k); err != nil {
		return JoinKey{}, fmt.Errorf("team: join key is not valid JSON: %w", err)
	}
	if k.PrincipalUID == "" || k.WrapPublicKey == "" {
		return JoinKey{}, errors.New("team: join key is missing the principal UID or wrap public key")
	}
	return k, nil
}

// LocalJoinKey returns this device's join request so the holder can hand it to
// an admitted member to be granted. It does NOT require an existing team — a
// brand-new device calls this before joining — and provisions the local
// identity (idempotent) so the wrap + signing publics are populated.
func (s *Service) LocalJoinKey(ctx context.Context) (JoinKey, error) {
	selfUID, ok := s.self.SelfPrincipal(ctx)
	if !ok || selfUID == "" {
		return JoinKey{}, ErrSelfUnbound
	}
	id, err := s.wrap.Initialize(ctx)
	if err != nil {
		return JoinKey{}, fmt.Errorf("team: initialize local identity: %w", err)
	}
	// Self-report the local display name so the granter's grant can name this
	// member's projected team entity on every peer. Best-effort: a miss yields
	// an empty name (the member still resolves by UID).
	name, nameErr := s.members.PrincipalDisplayName(ctx, selfUID)
	if nameErr != nil {
		name = ""
	}
	// Embed the genesis pin if this device already holds one (i.e. it IS the
	// genesis device, or a member that has already pinned). This lets a granter
	// who calls LocalJoinKey to produce an invite embed the pin so a fresh
	// device can pin immediately on receipt rather than waiting for a sync pull.
	// Best-effort: a pin-load failure is silently ignored.
	var pin *ontology.GenesisPin
	if pUID, pKID, pOK, _ := s.config.LoadGenesisPin(ctx); pOK { //nolint:errcheck // best-effort: pin-load failure silently skips embed
		pin = &ontology.GenesisPin{PrincipalUID: pUID, SigKeyID: pKID}
	}
	return JoinKey{
		PrincipalUID:  selfUID,
		WrapPublicKey: base64.StdEncoding.EncodeToString(id.WrapPublic.Bytes()),
		SignPublicKey: base64.StdEncoding.EncodeToString(id.SignPublic),
		SigKeyID:      id.SigKeyID,
		DeviceID:      id.DeviceID,
		DisplayName:   name,
		GenesisPin:    pin,
	}, nil
}

// requireLocalMember pre-checks that the local principal is admitted, so a
// non-member gets a clean ErrNotMember instead of an opaque append rejection.
// (The append boundary still enforces authority independently.)
func (s *Service) requireLocalMember(ctx context.Context) error {
	selfUID, ok := s.self.SelfPrincipal(ctx)
	if !ok || selfUID == "" {
		return ErrSelfUnbound
	}
	genesis, err := s.members.GenesisOwner(ctx)
	if err != nil {
		return fmt.Errorf("team: read genesis owner: %w", err)
	}
	if genesis == "" {
		return ErrNoTeam
	}
	member, err := s.members.IsTeamMember(ctx, selfUID)
	if err != nil {
		return fmt.Errorf("team: membership check: %w", err)
	}
	if !member {
		return ErrNotMember
	}
	return nil
}

// requireLocalRole pre-checks that the local principal is admitted AND holds
// at least minRole. It short-circuits with clean sentinel errors so the caller
// sees ErrSelfUnbound, ErrNoTeam, ErrNotMember, or ErrInsufficientRole rather
// than an opaque append rejection. The append boundary enforces authority
// independently; this is a UX pre-check.
func (s *Service) requireLocalRole(ctx context.Context, minRole string) error {
	if err := s.requireLocalMember(ctx); err != nil {
		return err
	}
	selfUID, _ := s.self.SelfPrincipal(ctx)
	role, _, err := s.members.MemberRole(ctx, selfUID)
	if err != nil {
		return fmt.Errorf("team: read local role: %w", err)
	}
	if !ontology.RoleAtLeast(role, minRole) {
		return fmt.Errorf("%w: have %q, need %q", ErrInsufficientRole, role, minRole)
	}
	return nil
}

// granteeVouch carries the grantee's VERIFICATION identity, embedded into a
// grant so the dispatcher can enroll it and peers can verify the grantee's own
// team-scope events. It is empty for a forward re-key of an ALREADY-enrolled
// member — their signing key reached the team with their first grant and is
// already in the devices table, so the re-key need not (and does not) re-vouch.
type granteeVouch struct {
	signPublicKey string // base64 ed25519
	sigKeyID      string
	deviceID      string
	displayName   string // self-reported; materializes the member as a team-scoped entity on peers
}

// vouchFromJoinKey lifts the verification identity out of a JoinKey.
func vouchFromJoinKey(k *JoinKey) granteeVouch {
	return granteeVouch{
		signPublicKey: k.SignPublicKey,
		sigKeyID:      k.SigKeyID,
		deviceID:      k.DeviceID,
		displayName:   k.DisplayName,
	}
}

// buildGrantEvent assembles (but does not append) a teamMemberGranted sealing
// dek to granteeWrapPub (raw 32-byte x25519 key) at epoch, embedding vouch so
// the grantee's signing identity is enrolled on apply (empty for a re-key).
// The role argument sets the grantee's access role; an empty string
// becomes "member".
func (s *Service) buildGrantEvent(granteeUID string, granteeWrapPub, dek []byte, epoch int, vouch granteeVouch, role string) (kernel.Event, error) {
	if role == "" {
		role = ontology.RoleMember
	}
	wrapped, err := watchword.WrapDEK(dek, granteeWrapPub)
	if err != nil {
		return kernel.Event{}, fmt.Errorf("team: wrap DEK for %s: %w", granteeUID, err)
	}
	payload, err := json.Marshal(ontology.TeamMemberGrantedEvent{
		GranteePrincipalUID:  granteeUID,
		GranteeWrapPublicKey: base64.StdEncoding.EncodeToString(granteeWrapPub),
		WrappedDEK:           base64.StdEncoding.EncodeToString(wrapped),
		DEKEpoch:             epoch,
		GranteeSignPublicKey: vouch.signPublicKey,
		GranteeSigKeyID:      vouch.sigKeyID,
		GranteeDeviceID:      vouch.deviceID,
		GranteeDisplayName:   vouch.displayName,
		GrantedAt:            s.now().UTC(),
		Role:                 role,
	})
	if err != nil {
		return kernel.Event{}, fmt.Errorf("team: marshal grant: %w", err)
	}
	return kernel.Event{
		Kind:       kernel.EventKind(ontology.KindTeamMemberGranted),
		Source:     "team",
		Confidence: 1.0,
		Scope:      kernel.ScopeTeam,
		Payload:    payload,
	}, nil
}

// appendGrant assembles and appends a single teamMemberGranted, returning the
// event UID. Used by Create/Grant; Revoke batches grants instead (see Revoke).
// The role argument sets the grantee's access role; an empty string
// becomes "member".
func (s *Service) appendGrant(ctx context.Context, granteeUID string, granteeWrapPub, dek []byte, epoch int, vouch granteeVouch, role string) (string, error) {
	ev, err := s.buildGrantEvent(granteeUID, granteeWrapPub, dek, epoch, vouch, role)
	if err != nil {
		return "", err
	}
	receipts, err := s.appender.Append(ctx, []kernel.Event{ev})
	if err != nil {
		return "", fmt.Errorf("team: append grant: %w", err)
	}
	return receiptUID(receipts), nil
}

// buildRevokeEvent assembles (but does not append) a teamMemberRevoked.
func (s *Service) buildRevokeEvent(revokedUID, reason string) (kernel.Event, error) {
	payload, err := json.Marshal(ontology.TeamMemberRevokedEvent{
		RevokedPrincipalUID: revokedUID,
		Reason:              reason,
		RevokedAt:           s.now().UTC(),
	})
	if err != nil {
		return kernel.Event{}, fmt.Errorf("team: marshal revoke: %w", err)
	}
	return kernel.Event{
		Kind:       kernel.EventKind(ontology.KindTeamMemberRevoked),
		Source:     "team",
		Confidence: 1.0,
		Scope:      kernel.ScopeTeam,
		Payload:    payload,
	}, nil
}

// buildRekeyGrants builds a teamMemberGranted at newEpoch for every admitted
// member EXCEPT the revoked principal, returning the events and the list of
// re-keyed UIDs. Pure assembly — the caller appends the batch atomically.
func (s *Service) buildRekeyGrants(members []Member, revokedUID string, dek []byte, newEpoch int) ([]kernel.Event, []string, error) {
	events := make([]kernel.Event, 0, len(members))
	rewrapped := make([]string, 0, len(members))
	for _, m := range members {
		if m.PrincipalUID == revokedUID {
			continue // the revoked member is NOT re-keyed
		}
		pub, derr := base64.StdEncoding.DecodeString(m.WrapPublicKey)
		if derr != nil {
			return nil, nil, fmt.Errorf("team: member %s wrap key not base64: %w", m.PrincipalUID, derr)
		}
		// Empty vouch: a re-key targets an already-admitted member whose signing
		// key is already enrolled (it arrived with their first grant). Re-vouching
		// would be redundant, and the member's wrap pubkey is the only thing the
		// fresh DEK must be sealed to. The member's existing role is preserved.
		ev, gerr := s.buildGrantEvent(m.PrincipalUID, pub, dek, newEpoch, granteeVouch{}, m.Role)
		if gerr != nil {
			return nil, nil, gerr
		}
		events = append(events, ev)
		rewrapped = append(rewrapped, m.PrincipalUID)
	}
	return events, rewrapped, nil
}

// RequestRegrantResult summarizes a teamRegrantRequested event append.
type RequestRegrantResult struct {
	// EventUID is the UID of the appended teamRegrantRequested event.
	EventUID string
	// MissingEpochs lists the DEK epochs that were reported missing.
	MissingEpochs []int
}

// RequestRegrant appends a teamRegrantRequested event to the team event log,
// signalling that this device is missing the given DEK epochs and requesting
// that an admitted admin re-wrap them. The event flows to all members on the
// next sync pull, where the admin can see it via `maz doctor` or
// `maz query --kind teamRegrantRequested` and then run `maz team grant` to
// re-wrap the missing epochs.
//
// The event is signed by this device's EventAuthor (stamped at the append
// boundary), so peers can verify the request came from a real principal.
// ErrSelfUnbound is returned when the device has no bound principal (identity
// genesis was never run).
func (s *Service) RequestRegrant(ctx context.Context, missingEpochs []int) (RequestRegrantResult, error) {
	selfUID, ok := s.self.SelfPrincipal(ctx)
	if !ok || selfUID == "" {
		return RequestRegrantResult{}, ErrSelfUnbound
	}
	if len(missingEpochs) == 0 {
		return RequestRegrantResult{}, errors.New("team: no missing epochs provided; nothing to request")
	}
	payload, err := json.Marshal(ontology.TeamRegrantRequestedEvent{
		RequestingPrincipalUID: selfUID,
		MissingEpochs:          missingEpochs,
		RequestedAt:            s.now(),
	})
	if err != nil {
		return RequestRegrantResult{}, fmt.Errorf("team: marshal regrant event: %w", err)
	}
	ev := kernel.Event{
		Kind:    kernel.EventKind(ontology.KindTeamRegrantRequested),
		Scope:   kernel.ScopeTeam,
		Payload: payload,
	}
	receipts, aerr := s.appender.Append(ctx, []kernel.Event{ev})
	if aerr != nil {
		return RequestRegrantResult{}, fmt.Errorf("team: append regrant request: %w", aerr)
	}
	return RequestRegrantResult{
		EventUID:      receiptUID(receipts),
		MissingEpochs: missingEpochs,
	}, nil
}

// RoleSetResult summarizes a teamRoleChanged event append.
type RoleSetResult struct {
	// EventUID is the UID of the appended teamRoleChanged event.
	EventUID string
	// SubjectPrincipalUID is the principal whose role was changed.
	SubjectPrincipalUID string
	// Role is the new effective role.
	Role string
}

// RoleSet changes the access role of an already-admitted team member by
// appending a teamRoleChanged event. The local principal must hold at
// least admin role to call this; it must hold owner when the target role
// or the subject's current role is admin or owner. The fold enforces these
// constraints on every peer independently; this is a UX pre-check.
//
// ErrInsufficientRole is returned when the local principal's role is too
// low. ErrSelfUnbound / ErrNoTeam / ErrNotMember gate the pre-checks.
func (s *Service) RoleSet(ctx context.Context, subjectUID, role string) (RoleSetResult, error) {
	if err := s.requireLocalRole(ctx, ontology.RoleAdmin); err != nil {
		return RoleSetResult{}, err
	}
	if subjectUID == "" {
		return RoleSetResult{}, errors.New("team: subject principal UID is required")
	}
	selfUID, _ := s.self.SelfPrincipal(ctx)
	if err := s.checkRoleSetAuthority(ctx, selfUID, subjectUID, role); err != nil {
		return RoleSetResult{}, err
	}
	payload, err := json.Marshal(ontology.TeamRoleChangedEvent{
		SubjectPrincipalUID: subjectUID,
		Role:                role,
		ChangedAt:           s.now().UTC(),
	})
	if err != nil {
		return RoleSetResult{}, fmt.Errorf("team: marshal role change: %w", err)
	}
	ev := kernel.Event{
		Kind:       kernel.EventKind(ontology.KindTeamRoleChanged),
		Source:     "team",
		Confidence: 1.0,
		Scope:      kernel.ScopeTeam,
		Payload:    payload,
	}
	receipts, aerr := s.appender.Append(ctx, []kernel.Event{ev})
	if aerr != nil {
		return RoleSetResult{}, fmt.Errorf("team: append role change: %w", aerr)
	}
	return RoleSetResult{
		EventUID:            receiptUID(receipts),
		SubjectPrincipalUID: subjectUID,
		Role:                role,
	}, nil
}

// checkRoleSetAuthority validates the role string and enforces the owner-only
// constraints for assigning or changing admin/owner roles. Extracted to keep
// RoleSet within the cyclomatic complexity gate.
func (s *Service) checkRoleSetAuthority(ctx context.Context, selfUID, subjectUID, role string) error {
	switch role {
	case ontology.RoleOwner, ontology.RoleAdmin, ontology.RoleMember, ontology.RoleReader:
	default:
		return fmt.Errorf("team: invalid role %q; must be owner | admin | member | reader", role)
	}
	localRole, _, err := s.members.MemberRole(ctx, selfUID)
	if err != nil {
		return fmt.Errorf("team: read local role: %w", err)
	}
	// Only an owner may assign admin or owner.
	if (role == ontology.RoleAdmin || role == ontology.RoleOwner) && localRole != ontology.RoleOwner {
		return fmt.Errorf("%w: assigning admin or owner role requires owner; have %q", ErrInsufficientRole, localRole)
	}
	subjectRole, subjectOK, err := s.members.MemberRole(ctx, subjectUID)
	if err != nil {
		return fmt.Errorf("team: read subject role: %w", err)
	}
	if !subjectOK {
		return fmt.Errorf("team: principal %q is not an admitted team member", subjectUID)
	}
	// Only an owner may change an admin's or owner's role.
	if (subjectRole == ontology.RoleAdmin || subjectRole == ontology.RoleOwner) &&
		role != subjectRole && localRole != ontology.RoleOwner {
		return fmt.Errorf("%w: changing an admin or owner's role requires owner; have %q", ErrInsufficientRole, localRole)
	}
	return nil
}

// storeDEK caches a DEK (base64) in the keychain under its epoch key.
func (s *Service) storeDEK(ctx context.Context, epoch int, dek []byte) error {
	if err := s.secrets.Set(ctx, dekKeychainKey(epoch), base64.StdEncoding.EncodeToString(dek)); err != nil {
		return fmt.Errorf("team: cache epoch-%d DEK: %w", epoch, err)
	}
	return nil
}

// loadDEK reads an epoch's DEK from the keychain.
func (s *Service) loadDEK(ctx context.Context, epoch int) ([]byte, error) {
	raw, err := s.secrets.Get(ctx, dekKeychainKey(epoch))
	if err != nil {
		return nil, fmt.Errorf("team: read epoch-%d DEK from keychain: %w", epoch, err)
	}
	dek, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("team: epoch-%d DEK is not valid base64: %w", epoch, err)
	}
	if len(dek) != watchword.DEKSize {
		return nil, fmt.Errorf("team: epoch-%d DEK is %d bytes, want %d", epoch, len(dek), watchword.DEKSize)
	}
	return dek, nil
}

// receiptUID returns the first receipt's UID, or "" if none.
func receiptUID(receipts []kernel.Receipt) string {
	if len(receipts) == 0 {
		return ""
	}
	return receipts[0].UID
}

// DevicesListResult summarizes the result of a DevicesList call.
type DevicesListResult struct {
	Devices []DeviceInfo
}

// DevicesList returns all devices enrolled under the local principal,
// annotated with revocation status. ErrNoTeam when device management is
// not configured, ErrSelfUnbound when the local principal is not resolved.
func (s *Service) DevicesList(ctx context.Context) (DevicesListResult, error) {
	if s.devices == nil {
		return DevicesListResult{}, ErrNoTeam
	}
	selfUID, ok := s.self.SelfPrincipal(ctx)
	if !ok || selfUID == "" {
		return DevicesListResult{}, ErrSelfUnbound
	}
	devs, err := s.devices.ListDevices(ctx, selfUID)
	if err != nil {
		return DevicesListResult{}, fmt.Errorf("team: list devices: %w", err)
	}
	return DevicesListResult{Devices: devs}, nil
}

// DeviceRevokeResult summarizes the result of a DeviceRevoke call.
type DeviceRevokeResult struct {
	// EventUID is the UID of the appended deviceRevoked event.
	EventUID string
	// DeviceID is the ULID of the revoked device.
	DeviceID string
	// PrincipalUID is the ULID of the device's owning principal.
	PrincipalUID string
}

// DeviceRevoke appends a deviceRevoked event for the named device. The
// revoker must be either the device's own principal (revoking one's own
// device) or an admitted owner/admin (revoking a team member's device).
// Returns ErrNoTeam when device management is not configured, ErrSelfUnbound
// when no principal is bound, ErrNotMember / ErrInsufficientRole on authority
// failures, or ErrNoTeam when the device is not enrolled.
func (s *Service) DeviceRevoke(ctx context.Context, deviceID, reason string) (DeviceRevokeResult, error) {
	if s.devices == nil {
		return DeviceRevokeResult{}, ErrNoTeam
	}
	selfUID, ok := s.self.SelfPrincipal(ctx)
	if !ok || selfUID == "" {
		return DeviceRevokeResult{}, ErrSelfUnbound
	}

	dev, found, err := s.devices.GetDeviceByID(ctx, deviceID)
	if err != nil {
		return DeviceRevokeResult{}, fmt.Errorf("team: look up device %q: %w", deviceID, err)
	}
	if !found {
		return DeviceRevokeResult{}, fmt.Errorf("%w: device %q is not enrolled", ErrNoTeam, deviceID)
	}

	// Authority: own principal may revoke any of their own devices without
	// a minimum role requirement. A team admin/owner may revoke any device.
	if dev.PrincipalUID != selfUID {
		if err := s.requireLocalRole(ctx, ontology.RoleAdmin); err != nil {
			return DeviceRevokeResult{}, err
		}
	}

	ev, err := s.buildDeviceRevokeEvent(&dev, reason)
	if err != nil {
		return DeviceRevokeResult{}, err
	}
	receipts, err := s.appender.Append(ctx, []kernel.Event{ev})
	if err != nil {
		return DeviceRevokeResult{}, fmt.Errorf("team: append deviceRevoked: %w", err)
	}
	return DeviceRevokeResult{
		EventUID:     receiptUID(receipts),
		DeviceID:     dev.DeviceID,
		PrincipalUID: dev.PrincipalUID,
	}, nil
}

// buildDeviceRevokeEvent assembles the unsigned kernel.Event; the author is
// stamped by the Append boundary.
func (s *Service) buildDeviceRevokeEvent(dev *DeviceInfo, reason string) (kernel.Event, error) {
	payload, err := json.Marshal(ontology.DeviceRevokedEvent{
		DeviceID:     dev.DeviceID,
		SigKeyID:     dev.SigKeyID,
		PrincipalUID: dev.PrincipalUID,
		RevokedAt:    s.now().UTC(),
		Reason:       reason,
	})
	if err != nil {
		return kernel.Event{}, fmt.Errorf("team: marshal deviceRevoked: %w", err)
	}
	return kernel.Event{
		Kind:       kernel.EventKind(ontology.KindDeviceRevoked),
		Source:     "team",
		Confidence: 1.0,
		Scope:      kernel.ScopeTeam,
		Payload:    payload,
	}, nil
}
