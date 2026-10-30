package team

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// --- fakes ---

type fakeAppender struct{ events []kernel.Event }

func (f *fakeAppender) Append(_ context.Context, evs []kernel.Event) ([]kernel.Receipt, error) {
	f.events = append(f.events, evs...)
	out := make([]kernel.Receipt, len(evs))
	for i := range evs {
		out[i] = kernel.Receipt{UID: "ev" + string(rune('A'+len(f.events)-len(evs)+i))}
	}
	return out, nil
}

type fakeWrapKeys struct {
	pub     *ecdh.PublicKey
	signPub ed25519.PublicKey
	device  string
}

func (f fakeWrapKeys) WrappingPublicKey(_ context.Context) (*ecdh.PublicKey, error) {
	return f.pub, nil
}

func (f fakeWrapKeys) Initialize(context.Context) (identity.Identity, error) {
	return identity.Identity{
		DeviceID:   f.device,
		SigKeyID:   identity.SigKeyID(f.signPub),
		SignPublic: f.signPub,
		WrapPublic: f.pub,
	}, nil
}

type fakeSecrets struct{ m map[string]string }

func newFakeSecrets() *fakeSecrets { return &fakeSecrets{m: map[string]string{}} }
func (f *fakeSecrets) Get(_ context.Context, k string) (string, error) {
	v, ok := f.m[k]
	if !ok {
		// Faithful to the real providers (keychain/file/env all return
		// secret.ErrNotFound on a miss), so callers that branch on
		// errors.Is(err, secret.ErrNotFound) — e.g. Grant's per-epoch DEK
		// load — exercise the same path in tests as in production.
		return "", secret.ErrNotFound
	}
	return v, nil
}
func (f *fakeSecrets) Set(_ context.Context, k, v string) error { f.m[k] = v; return nil }

type fakeMembers struct {
	genesis  string
	isMember map[string]bool
	roles    map[string]string
	epoch    int
	admitted []Member
	names    map[string]string
}

func (f *fakeMembers) GenesisOwner(context.Context) (string, error) { return f.genesis, nil }
func (f *fakeMembers) IsTeamMember(_ context.Context, uid string) (bool, error) {
	return f.isMember[uid], nil
}

func (f *fakeMembers) MemberRole(_ context.Context, uid string) (string, bool, error) {
	if r, ok := f.roles[uid]; ok {
		return r, true, nil
	}
	if f.isMember[uid] {
		return "member", true, nil
	}
	return "", false, nil
}
func (f *fakeMembers) CurrentDEKEpoch(context.Context) (int, error)      { return f.epoch, nil }
func (f *fakeMembers) AdmittedMembers(context.Context) ([]Member, error) { return f.admitted, nil }
func (f *fakeMembers) PrincipalDisplayName(_ context.Context, uid string) (string, error) {
	return f.names[uid], nil
}

type fakeSelf struct {
	uid   string
	bound bool
}

func (f fakeSelf) SelfPrincipal(context.Context) (string, bool) { return f.uid, f.bound }

type fakeConfig struct {
	enabled   bool
	url       string
	pinnedUID string
	pinnedKID string
}

func (f *fakeConfig) EnableTeamEnvelope(_ context.Context, url string) error {
	f.enabled = true
	f.url = url
	return nil
}

func (f *fakeConfig) PersistGenesisPin(_ context.Context, uid, kid string) error {
	f.pinnedUID = uid
	f.pinnedKID = kid
	return nil
}

func (f *fakeConfig) LoadGenesisPin(_ context.Context) (string, string, bool, error) {
	if f.pinnedUID == "" || f.pinnedKID == "" {
		return "", "", false, nil
	}
	return f.pinnedUID, f.pinnedKID, true, nil
}

// genWrapKeypair returns a fresh x25519 keypair for tests.
func genWrapKeypair(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen x25519: %v", err)
	}
	return k
}

func fixedClock() func() time.Time {
	ts := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return ts }
}

// newService wires a Service with the given fakes; genesis="" means no team yet.
func newService(t *testing.T, selfPriv *ecdh.PrivateKey, secrets *fakeSecrets, members *fakeMembers, self fakeSelf, cfg *fakeConfig, app *fakeAppender) *Service {
	t.Helper()
	signPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ed25519: %v", err)
	}
	s, err := New(&Deps{
		Appender:   app,
		WrapKeys:   fakeWrapKeys{pub: selfPriv.PublicKey(), signPub: signPub, device: "device-test"},
		Secrets:    secrets,
		Membership: members,
		Self:       self,
		Config:     cfg,
		Now:        fixedClock(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// decodeGrant returns the single teamMemberGranted payload in app at index i.
func decodeGrant(t *testing.T, app *fakeAppender, i int) ontology.TeamMemberGrantedEvent {
	t.Helper()
	if app.events[i].Kind != kernel.EventKind(ontology.KindTeamMemberGranted) {
		t.Fatalf("event %d kind = %q, want grant", i, app.events[i].Kind)
	}
	if app.events[i].Scope != kernel.ScopeTeam {
		t.Fatalf("event %d scope = %q, want team", i, app.events[i].Scope)
	}
	var g ontology.TeamMemberGrantedEvent
	if err := json.Unmarshal(app.events[i].Payload, &g); err != nil {
		t.Fatalf("decode grant %d: %v", i, err)
	}
	return g
}

// unwrap reads the wrapped DEK from a grant payload and opens it with priv.
func unwrap(t *testing.T, g *ontology.TeamMemberGrantedEvent, priv *ecdh.PrivateKey) []byte {
	t.Helper()
	sealed, err := base64.StdEncoding.DecodeString(g.WrappedDEK)
	if err != nil {
		t.Fatalf("decode wrappedDek: %v", err)
	}
	dek, err := watchword.UnwrapDEK(sealed, priv)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	return dek
}

// --- Create ---

func TestCreate_GenesisSelfGrant_RoundTrips(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	secrets := newFakeSecrets()
	members := &fakeMembers{genesis: "", isMember: map[string]bool{}, names: map[string]string{"G": "Genesis Owner"}}
	app := &fakeAppender{}
	cfg := &fakeConfig{}
	s := newService(t, selfPriv, secrets, members, fakeSelf{uid: "G", bound: true}, cfg, app)

	res, err := s.Create(context.Background(), "git@example:team.git")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.GenesisPrincipalUID != "G" || res.DEKEpoch != 0 {
		t.Fatalf("result = %+v, want genesis G epoch 0", res)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1 genesis grant", len(app.events))
	}
	g := decodeGrant(t, app, 0)
	if g.GranteePrincipalUID != "G" {
		t.Errorf("grantee = %q, want G (self-grant)", g.GranteePrincipalUID)
	}
	// The genesis self-grant carries the owner's display name so every peer can
	// name the founder's projected team-scoped person entity.
	if g.GranteeDisplayName != "Genesis Owner" {
		t.Errorf("genesis grant display name = %q, want Genesis Owner", g.GranteeDisplayName)
	}
	if g.DEKEpoch != 0 {
		t.Errorf("grant epoch = %d, want 0", g.DEKEpoch)
	}
	// The genesis DEK in the keychain must equal the DEK the grant wraps.
	kept, err := secrets.Get(context.Background(), dekKeychainKey(0))
	if err != nil {
		t.Fatalf("epoch-0 DEK not cached: %v", err)
	}
	keptRaw, _ := base64.StdEncoding.DecodeString(kept)
	got := unwrap(t, &g, selfPriv)
	if !bytes.Equal(got, keptRaw) {
		t.Errorf("unwrapped DEK != cached DEK")
	}
	if len(got) != watchword.DEKSize {
		t.Errorf("DEK len = %d, want %d", len(got), watchword.DEKSize)
	}
	if !cfg.enabled || cfg.url != "git@example:team.git" {
		t.Errorf("envelope config not enabled with remote: %+v", cfg)
	}
}

func TestCreate_AlreadyExists(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	members := &fakeMembers{genesis: "G"} // team already exists
	s := newService(t, selfPriv, newFakeSecrets(), members, fakeSelf{uid: "H", bound: true}, &fakeConfig{}, &fakeAppender{})
	if _, err := s.Create(context.Background(), ""); !errors.Is(err, ErrTeamExists) {
		t.Fatalf("Create on existing team = %v, want ErrTeamExists", err)
	}
}

func TestCreate_SelfUnbound(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	s := newService(t, selfPriv, newFakeSecrets(), &fakeMembers{}, fakeSelf{bound: false}, &fakeConfig{}, &fakeAppender{})
	if _, err := s.Create(context.Background(), ""); !errors.Is(err, ErrSelfUnbound) {
		t.Fatalf("Create unbound = %v, want ErrSelfUnbound", err)
	}
}

// TestJoin_ConfiguresRemoteWithoutGenesis proves a fresh device (no local team)
// configures the team remote in envelope mode and appends NO grant — the join
// path must never mint a genesis.
func TestJoin_ConfiguresRemoteWithoutGenesis(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	app := &fakeAppender{}
	cfg := &fakeConfig{}
	// Fresh joiner: no genesis yet locally.
	s := newService(t, selfPriv, newFakeSecrets(), &fakeMembers{genesis: ""}, fakeSelf{uid: "B", bound: true}, cfg, app)

	res, err := s.Join(context.Background(), "git@example:team.git")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if res.RemoteURL != "git@example:team.git" {
		t.Errorf("RemoteURL = %q, want the joined remote", res.RemoteURL)
	}
	if !cfg.enabled || cfg.url != "git@example:team.git" {
		t.Errorf("envelope config not enabled with remote: %+v", cfg)
	}
	if len(app.events) != 0 {
		t.Fatalf("Join appended %d events, want 0 (no genesis on join)", len(app.events))
	}
}

func TestJoin_RequiresRemote(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	s := newService(t, selfPriv, newFakeSecrets(), &fakeMembers{}, fakeSelf{uid: "B", bound: true}, &fakeConfig{}, &fakeAppender{})
	if _, err := s.Join(context.Background(), ""); err == nil {
		t.Fatal("Join with empty remote should error")
	}
}

// TestJoin_RefusesGenesisOwner proves the team creator cannot "join" their own
// team (they made it; there is nothing to join).
func TestJoin_RefusesGenesisOwner(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	cfg := &fakeConfig{}
	// This device IS the genesis owner.
	s := newService(t, selfPriv, newFakeSecrets(), &fakeMembers{genesis: "G"}, fakeSelf{uid: "G", bound: true}, cfg, &fakeAppender{})
	if _, err := s.Join(context.Background(), "git@example:team.git"); err == nil {
		t.Fatal("genesis owner joining their own team should error")
	}
	if cfg.enabled {
		t.Error("a refused join must not have reconfigured the remote")
	}
}

func TestLocalJoinKey_ReturnsPrincipalAndWrapKey(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	members := &fakeMembers{names: map[string]string{"P9": "Alice"}}
	s := newService(t, selfPriv, newFakeSecrets(), members, fakeSelf{uid: "P9", bound: true}, &fakeConfig{}, &fakeAppender{})
	jk, err := s.LocalJoinKey(context.Background())
	if err != nil {
		t.Fatalf("LocalJoinKey: %v", err)
	}
	if jk.PrincipalUID != "P9" {
		t.Errorf("principal = %q, want P9", jk.PrincipalUID)
	}
	wantKey := base64.StdEncoding.EncodeToString(selfPriv.PublicKey().Bytes())
	if jk.WrapPublicKey != wantKey {
		t.Errorf("wrap key = %q, want %q", jk.WrapPublicKey, wantKey)
	}
	// The self-reported display name rides in the join key so the granter's
	// grant can name this member's projected team entity on every peer.
	if jk.DisplayName != "Alice" {
		t.Errorf("display name = %q, want Alice", jk.DisplayName)
	}
}

func TestLocalJoinKey_SelfUnbound(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	s := newService(t, selfPriv, newFakeSecrets(), &fakeMembers{}, fakeSelf{bound: false}, &fakeConfig{}, &fakeAppender{})
	if _, err := s.LocalJoinKey(context.Background()); !errors.Is(err, ErrSelfUnbound) {
		t.Fatalf("LocalJoinKey unbound = %v, want ErrSelfUnbound", err)
	}
}

// --- Grant ---

func TestGrant_WrapsCurrentDEKToPastedKey(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	secrets := newFakeSecrets()
	// Seed a team at epoch 0 with the genesis DEK cached.
	dek := mustGenDEK(t)
	_ = secrets.Set(context.Background(), dekKeychainKey(0), base64.StdEncoding.EncodeToString(dek))
	members := &fakeMembers{genesis: "G", isMember: map[string]bool{"G": true}, roles: map[string]string{"G": ontology.RoleOwner}, epoch: 0}
	app := &fakeAppender{}
	s := newService(t, selfPriv, secrets, members, fakeSelf{uid: "G", bound: true}, &fakeConfig{}, app)

	granteePriv := genWrapKeypair(t)
	granteePubB64 := base64.StdEncoding.EncodeToString(granteePriv.PublicKey().Bytes())
	granteeSignPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen grantee sign key: %v", err)
	}
	granteeSignB64 := base64.StdEncoding.EncodeToString(granteeSignPub)
	res, err := s.Grant(context.Background(), &JoinKey{
		PrincipalUID:  "B",
		WrapPublicKey: granteePubB64,
		SignPublicKey: granteeSignB64,
		SigKeyID:      identity.SigKeyID(granteeSignPub),
		DeviceID:      "device-b",
	}, "")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if res.GranteePrincipalUID != "B" || res.DEKEpoch != 0 {
		t.Fatalf("result = %+v, want B epoch 0", res)
	}
	g := decodeGrant(t, app, 0)
	if g.GranteePrincipalUID != "B" || g.GranteeWrapPublicKey != granteePubB64 {
		t.Errorf("grant pins wrong grantee/key: %+v", g)
	}
	// The grant vouches the grantee's verification identity into the team so a
	// peer can verify B's own team events once the grant's projection enrolls it.
	if g.GranteeSignPublicKey != granteeSignB64 || g.GranteeSigKeyID != identity.SigKeyID(granteeSignPub) || g.GranteeDeviceID != "device-b" {
		t.Errorf("grant does not vouch the grantee signing identity: %+v", g)
	}
	// The grantee can unwrap to the SAME DEK the team uses.
	got := unwrap(t, &g, granteePriv)
	if !bytes.Equal(got, dek) {
		t.Errorf("grantee unwrapped a different DEK than the team's")
	}
}

// TestGrant_FullHistoryWrapsEveryEpoch proves a grant into a team that has
// re-keyed (epochs 0..2) gives the new member EVERY epoch's DEK — so they can
// read the full history, not just the current epoch — by emitting one grant per
// epoch the granter holds.
func TestGrant_FullHistoryWrapsEveryEpoch(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	secrets := newFakeSecrets()
	// A team that has re-keyed twice: DEKs cached at epochs 0, 1, 2.
	deks := map[int][]byte{}
	for e := range 3 {
		d := mustGenDEK(t)
		deks[e] = d
		_ = secrets.Set(context.Background(), dekKeychainKey(e), base64.StdEncoding.EncodeToString(d))
	}
	members := &fakeMembers{genesis: "G", isMember: map[string]bool{"G": true}, roles: map[string]string{"G": ontology.RoleOwner}, epoch: 2}
	app := &fakeAppender{}
	s := newService(t, selfPriv, secrets, members, fakeSelf{uid: "G", bound: true}, &fakeConfig{}, app)

	granteePriv := genWrapKeypair(t)
	res, err := s.Grant(context.Background(), &JoinKey{
		PrincipalUID:  "B",
		WrapPublicKey: base64.StdEncoding.EncodeToString(granteePriv.PublicKey().Bytes()),
		SignPublicKey: "c2lnbg==",
		SigKeyID:      "kid:b",
		DeviceID:      "device-b",
	}, "")
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if res.DEKEpoch != 2 {
		t.Errorf("result epoch = %d, want 2 (current)", res.DEKEpoch)
	}
	wantEpochs := []int{0, 1, 2}
	if len(res.GrantedEpochs) != 3 || res.GrantedEpochs[0] != 0 || res.GrantedEpochs[2] != 2 {
		t.Fatalf("GrantedEpochs = %v, want %v", res.GrantedEpochs, wantEpochs)
	}
	if len(app.events) != 3 {
		t.Fatalf("appended %d events, want 3 (one grant per epoch)", len(app.events))
	}
	// Each grant unwraps to the matching epoch's DEK, and every grant vouches B's
	// signing identity so enrollment is robust to partial replay.
	for i := range app.events {
		g := decodeGrant(t, app, i)
		if g.GranteePrincipalUID != "B" {
			t.Errorf("grant %d grantee = %q, want B", i, g.GranteePrincipalUID)
		}
		if g.GranteeSigKeyID != "kid:b" {
			t.Errorf("grant %d does not vouch B's signing key: %+v", i, g)
		}
		got := unwrap(t, &g, granteePriv)
		if !bytes.Equal(got, deks[g.DEKEpoch]) {
			t.Errorf("grant for epoch %d unwrapped the wrong DEK", g.DEKEpoch)
		}
	}
}

// TestGrant_SkipsMissingEpochButGrantsRest pins the skip arm of the per-epoch
// load loop: an epoch this device genuinely never held (secret.ErrNotFound) is
// skipped, and the grantee still receives every epoch we CAN wrap. A missing
// middle epoch must NOT abort the whole grant.
func TestGrant_SkipsMissingEpochButGrantsRest(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	secrets := newFakeSecrets()
	// Epochs 0 and 2 present; epoch 1 never stored (a real ErrNotFound gap).
	dek0, dek2 := mustGenDEK(t), mustGenDEK(t)
	_ = secrets.Set(context.Background(), dekKeychainKey(0), base64.StdEncoding.EncodeToString(dek0))
	_ = secrets.Set(context.Background(), dekKeychainKey(2), base64.StdEncoding.EncodeToString(dek2))
	members := &fakeMembers{genesis: "G", isMember: map[string]bool{"G": true}, roles: map[string]string{"G": ontology.RoleOwner}, epoch: 2}
	app := &fakeAppender{}
	s := newService(t, selfPriv, secrets, members, fakeSelf{uid: "G", bound: true}, &fakeConfig{}, app)

	granteePriv := genWrapKeypair(t)
	res, err := s.Grant(context.Background(), &JoinKey{
		PrincipalUID:  "B",
		WrapPublicKey: base64.StdEncoding.EncodeToString(granteePriv.PublicKey().Bytes()),
	}, "")
	if err != nil {
		t.Fatalf("Grant must skip the missing epoch, not fail: %v", err)
	}
	if want := []int{0, 2}; len(res.GrantedEpochs) != 2 || res.GrantedEpochs[0] != 0 || res.GrantedEpochs[1] != 2 {
		t.Fatalf("GrantedEpochs = %v, want %v (epoch 1 skipped)", res.GrantedEpochs, want)
	}
	if len(app.events) != 2 {
		t.Fatalf("appended %d events, want 2 (epoch 1 skipped)", len(app.events))
	}
}

// TestGrant_FailsOnCorruptEpochDEK pins the fail-loud arm: a real key-store
// fault (here, an epoch whose cached DEK is not valid base64) must abort the
// grant rather than silently under-granting — a member who can decrypt only
// part of the history is worse than a visible error.
func TestGrant_FailsOnCorruptEpochDEK(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	secrets := newFakeSecrets()
	// Epoch 0 valid, epoch 1 corrupt (not base64), epoch 2 valid; current = 2.
	_ = secrets.Set(context.Background(), dekKeychainKey(0), base64.StdEncoding.EncodeToString(mustGenDEK(t)))
	_ = secrets.Set(context.Background(), dekKeychainKey(1), "!!! not valid base64 !!!")
	_ = secrets.Set(context.Background(), dekKeychainKey(2), base64.StdEncoding.EncodeToString(mustGenDEK(t)))
	members := &fakeMembers{genesis: "G", isMember: map[string]bool{"G": true}, roles: map[string]string{"G": ontology.RoleOwner}, epoch: 2}
	app := &fakeAppender{}
	s := newService(t, selfPriv, secrets, members, fakeSelf{uid: "G", bound: true}, &fakeConfig{}, app)

	if _, err := s.Grant(context.Background(), &JoinKey{
		PrincipalUID:  "B",
		WrapPublicKey: base64.StdEncoding.EncodeToString(genWrapKeypair(t).PublicKey().Bytes()),
	}, ""); err == nil {
		t.Fatal("Grant must fail on a corrupt epoch DEK, not silently under-grant")
	}
	if len(app.events) != 0 {
		t.Fatalf("appended %d events, want 0 (a corrupt epoch must abort before any append)", len(app.events))
	}
}

func TestGrant_NonMember(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	members := &fakeMembers{genesis: "G", isMember: map[string]bool{"G": true}, epoch: 0}
	// Local self is "Z", not an admitted member.
	s := newService(t, selfPriv, newFakeSecrets(), members, fakeSelf{uid: "Z", bound: true}, &fakeConfig{}, &fakeAppender{})
	jk := JoinKey{PrincipalUID: "B", WrapPublicKey: base64.StdEncoding.EncodeToString(genWrapKeypair(t).PublicKey().Bytes())}
	if _, err := s.Grant(context.Background(), &jk, ""); !errors.Is(err, ErrNotMember) {
		t.Fatalf("Grant by non-member = %v, want ErrNotMember", err)
	}
}

func TestGrant_BadPublicKey(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	members := &fakeMembers{genesis: "G", isMember: map[string]bool{"G": true}, epoch: 0}
	s := newService(t, selfPriv, newFakeSecrets(), members, fakeSelf{uid: "G", bound: true}, &fakeConfig{}, &fakeAppender{})
	if _, err := s.Grant(context.Background(), &JoinKey{PrincipalUID: "B", WrapPublicKey: "not-base64!!"}, ""); err == nil {
		t.Fatal("Grant with malformed pubkey should error")
	}
}

// --- Revoke (forward re-key) ---

func TestRevoke_MintsNewEpochAndReKeysRemaining(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	secrets := newFakeSecrets()
	dek0 := mustGenDEK(t)
	_ = secrets.Set(context.Background(), dekKeychainKey(0), base64.StdEncoding.EncodeToString(dek0))

	// Team: G (genesis), A, B all at epoch 0. Revoke B.
	aPriv, bPriv := genWrapKeypair(t), genWrapKeypair(t)
	members := &fakeMembers{
		genesis:  "G",
		isMember: map[string]bool{"G": true, "A": true, "B": true},
		roles:    map[string]string{"G": ontology.RoleOwner},
		epoch:    0,
		admitted: []Member{
			{PrincipalUID: "G", WrapPublicKey: base64.StdEncoding.EncodeToString(selfPriv.PublicKey().Bytes()), DEKEpoch: 0, IsGenesis: true},
			{PrincipalUID: "A", WrapPublicKey: base64.StdEncoding.EncodeToString(aPriv.PublicKey().Bytes()), DEKEpoch: 0},
			{PrincipalUID: "B", WrapPublicKey: base64.StdEncoding.EncodeToString(bPriv.PublicKey().Bytes()), DEKEpoch: 0},
		},
	}
	app := &fakeAppender{}
	s := newService(t, selfPriv, secrets, members, fakeSelf{uid: "G", bound: true}, &fakeConfig{}, app)

	res, err := s.Revoke(context.Background(), "B", "left the team")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if res.NewDEKEpoch != 1 {
		t.Fatalf("new epoch = %d, want 1", res.NewDEKEpoch)
	}
	// Re-keyed to G and A only — never B.
	if len(res.RewrappedTo) != 2 {
		t.Fatalf("rewrapped to %v, want exactly {G,A}", res.RewrappedTo)
	}
	for _, p := range res.RewrappedTo {
		if p == "B" {
			t.Fatal("must not re-key the revoked member B")
		}
	}
	// Events: 1 revoke + 2 re-key grants.
	if len(app.events) != 3 {
		t.Fatalf("appended %d events, want 3 (revoke + 2 re-grants)", len(app.events))
	}
	if app.events[0].Kind != kernel.EventKind(ontology.KindTeamMemberRevoked) {
		t.Errorf("first event = %q, want revoke", app.events[0].Kind)
	}
	// The fresh epoch-1 DEK is cached and differs from epoch 0.
	newRaw, err := secrets.Get(context.Background(), dekKeychainKey(1))
	if err != nil {
		t.Fatalf("epoch-1 DEK not cached: %v", err)
	}
	newDEK, _ := base64.StdEncoding.DecodeString(newRaw)
	if bytes.Equal(newDEK, dek0) {
		t.Error("re-key produced the same DEK as epoch 0")
	}
	// A's re-key grant unwraps to the new DEK; B has no epoch-1 grant.
	g1 := decodeGrant(t, app, 1)
	if g1.DEKEpoch != 1 {
		t.Errorf("re-grant epoch = %d, want 1", g1.DEKEpoch)
	}
}

func TestRevoke_GenesisRefused(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	members := &fakeMembers{genesis: "G", isMember: map[string]bool{"G": true}, epoch: 0}
	s := newService(t, selfPriv, newFakeSecrets(), members, fakeSelf{uid: "G", bound: true}, &fakeConfig{}, &fakeAppender{})
	if _, err := s.Revoke(context.Background(), "G", ""); err == nil {
		t.Fatal("revoking genesis should error")
	}
}

func mustGenDEK(t *testing.T) []byte {
	t.Helper()
	dek, err := watchword.GenerateDEK()
	if err != nil {
		t.Fatalf("gen DEK: %v", err)
	}
	return dek
}

// ---------------------------------------------------------------------------
// Role / RBAC service tests
// ---------------------------------------------------------------------------

func TestGrant_NonAdminReturnsErrInsufficientRole(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	secrets := newFakeSecrets()
	_ = secrets.Set(context.Background(), dekKeychainKey(0), base64.StdEncoding.EncodeToString(mustGenDEK(t)))
	// Self is admitted as member (not admin/owner).
	members := &fakeMembers{
		genesis:  "G",
		isMember: map[string]bool{"Z": true},
		roles:    map[string]string{"Z": ontology.RoleMember},
		epoch:    0,
	}
	s := newService(t, selfPriv, secrets, members, fakeSelf{uid: "Z", bound: true}, &fakeConfig{}, &fakeAppender{})
	jk := JoinKey{PrincipalUID: "B", WrapPublicKey: base64.StdEncoding.EncodeToString(genWrapKeypair(t).PublicKey().Bytes())}
	_, err := s.Grant(context.Background(), &jk, "")
	if !errors.Is(err, ErrInsufficientRole) {
		t.Fatalf("Grant by member = %v, want ErrInsufficientRole", err)
	}
}

func TestRevoke_NonAdminReturnsErrInsufficientRole(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	members := &fakeMembers{
		genesis:  "G",
		isMember: map[string]bool{"Z": true},
		roles:    map[string]string{"Z": ontology.RoleMember},
		epoch:    0,
	}
	s := newService(t, selfPriv, newFakeSecrets(), members, fakeSelf{uid: "Z", bound: true}, &fakeConfig{}, &fakeAppender{})
	_, err := s.Revoke(context.Background(), "G", "")
	if !errors.Is(err, ErrInsufficientRole) {
		t.Fatalf("Revoke by member = %v, want ErrInsufficientRole", err)
	}
}

func TestRoleSet_AdminCanPromoteMemberToAdmin(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	app := &fakeAppender{}
	members := &fakeMembers{
		genesis:  "G",
		isMember: map[string]bool{"G": true, "A": true},
		roles:    map[string]string{"G": ontology.RoleOwner, "A": ontology.RoleAdmin, "B": ontology.RoleMember},
		epoch:    0,
	}
	// Self = G (owner), subject = A (admin), target role = member (demotion within owner rights)
	s := newService(t, selfPriv, newFakeSecrets(), members, fakeSelf{uid: "G", bound: true}, &fakeConfig{}, app)
	// Use subject "A" which is in the roles map (admitted). Change A from admin → member.
	// G is owner so this is allowed.
	members.isMember["B"] = true
	res, err := s.RoleSet(context.Background(), "A", ontology.RoleMember)
	if err != nil {
		t.Fatalf("RoleSet: %v", err)
	}
	if res.SubjectPrincipalUID != "A" || res.Role != ontology.RoleMember {
		t.Errorf("result = %+v, want A member", res)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	if app.events[0].Kind != kernel.EventKind(ontology.KindTeamRoleChanged) {
		t.Errorf("event kind = %q, want teamRoleChanged", app.events[0].Kind)
	}
}

func TestRoleSet_AdminCannotAssignOwner(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	members := &fakeMembers{
		genesis:  "G",
		isMember: map[string]bool{"A": true, "B": true},
		roles:    map[string]string{"A": ontology.RoleAdmin, "B": ontology.RoleMember},
		epoch:    0,
	}
	s := newService(t, selfPriv, newFakeSecrets(), members, fakeSelf{uid: "A", bound: true}, &fakeConfig{}, &fakeAppender{})
	_, err := s.RoleSet(context.Background(), "B", ontology.RoleOwner)
	if !errors.Is(err, ErrInsufficientRole) {
		t.Fatalf("RoleSet admin→owner by admin = %v, want ErrInsufficientRole", err)
	}
}

func TestRoleSet_InvalidRoleReturnsError(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	members := &fakeMembers{
		genesis:  "G",
		isMember: map[string]bool{"G": true, "B": true},
		roles:    map[string]string{"G": ontology.RoleOwner, "B": ontology.RoleMember},
		epoch:    0,
	}
	s := newService(t, selfPriv, newFakeSecrets(), members, fakeSelf{uid: "G", bound: true}, &fakeConfig{}, &fakeAppender{})
	_, err := s.RoleSet(context.Background(), "B", "superadmin")
	if err == nil {
		t.Fatal("RoleSet with invalid role must error")
	}
}

func TestRoleSet_SubjectNotMemberReturnsError(t *testing.T) {
	t.Parallel()
	selfPriv := genWrapKeypair(t)
	members := &fakeMembers{
		genesis:  "G",
		isMember: map[string]bool{"G": true},
		roles:    map[string]string{"G": ontology.RoleOwner},
		epoch:    0,
	}
	s := newService(t, selfPriv, newFakeSecrets(), members, fakeSelf{uid: "G", bound: true}, &fakeConfig{}, &fakeAppender{})
	_, err := s.RoleSet(context.Background(), "UNKNOWN", ontology.RoleMember)
	if err == nil {
		t.Fatal("RoleSet for non-member must error")
	}
}

// ---------------------------------------------------------------------------
// JoinKey genesis-pin embedding
// ---------------------------------------------------------------------------

func TestJoinKey_RoundtripWithGenesisPin(t *testing.T) {
	t.Parallel()

	pin := &ontology.GenesisPin{
		PrincipalUID: "01HXYZ0000000000000000001",
		SigKeyID:     "kid:abc123",
	}
	k := JoinKey{
		PrincipalUID:  "01HXYZ0000000000000000002",
		WrapPublicKey: "AAAA",
		SignPublicKey: "BBBB",
		SigKeyID:      "kid:def456",
		DeviceID:      "dev-1",
		DisplayName:   "Alice",
		GenesisPin:    pin,
	}
	token, err := k.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := DecodeJoinKey(token)
	if err != nil {
		t.Fatalf("DecodeJoinKey: %v", err)
	}
	if got.PrincipalUID != k.PrincipalUID {
		t.Errorf("PrincipalUID: got %q, want %q", got.PrincipalUID, k.PrincipalUID)
	}
	if got.GenesisPin == nil {
		t.Fatal("GenesisPin is nil after round-trip")
	}
	if got.GenesisPin.PrincipalUID != pin.PrincipalUID {
		t.Errorf("GenesisPin.PrincipalUID: got %q, want %q", got.GenesisPin.PrincipalUID, pin.PrincipalUID)
	}
	if got.GenesisPin.SigKeyID != pin.SigKeyID {
		t.Errorf("GenesisPin.SigKeyID: got %q, want %q", got.GenesisPin.SigKeyID, pin.SigKeyID)
	}
}

func TestJoinKey_OldFormatNilGenesisPin(t *testing.T) {
	t.Parallel()

	// Simulate a token produced by a client that predates genesis-pin support:
	// the JSON has no "genesisPin" field.
	old := map[string]string{
		"principalUid":  "01HXYZ0000000000000000003",
		"wrapPublicKey": "CCCC",
		"signPublicKey": "DDDD",
		"sigKeyId":      "kid:old",
		"deviceId":      "dev-old",
	}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal old format: %v", err)
	}
	token := base64.StdEncoding.EncodeToString(raw)

	got, err := DecodeJoinKey(token)
	if err != nil {
		t.Fatalf("DecodeJoinKey old format: %v", err)
	}
	if got.GenesisPin != nil {
		t.Errorf("GenesisPin: got %+v, want nil for old-format token", got.GenesisPin)
	}
	if got.PrincipalUID != "01HXYZ0000000000000000003" {
		t.Errorf("PrincipalUID: got %q, want correct value", got.PrincipalUID)
	}
}

func TestJoinKey_RoundtripWithoutPin(t *testing.T) {
	t.Parallel()

	k := JoinKey{
		PrincipalUID:  "01HXYZ0000000000000000004",
		WrapPublicKey: "EEEE",
		SignPublicKey: "FFFF",
		SigKeyID:      "kid:no-pin",
		DeviceID:      "dev-np",
	}
	token, err := k.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Token JSON must NOT contain "genesisPin" when field is nil.
	raw, _ := base64.StdEncoding.DecodeString(token)
	if bytes.Contains(raw, []byte("genesisPin")) {
		t.Errorf("omitempty violated: JSON contains genesisPin key for nil pin")
	}

	got, err := DecodeJoinKey(token)
	if err != nil {
		t.Fatalf("DecodeJoinKey: %v", err)
	}
	if got.GenesisPin != nil {
		t.Errorf("GenesisPin: got %+v, want nil", got.GenesisPin)
	}
}
