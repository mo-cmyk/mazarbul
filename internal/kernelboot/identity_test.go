package kernelboot_test

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/tyler-smith/go-bip39"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// identityMapProvider is an in-memory secret.Provider for the genesis test.
type identityMapProvider struct {
	mu sync.Mutex
	m  map[string]string
}

func (p *identityMapProvider) Get(_ context.Context, name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.m[name]; ok {
		return v, nil
	}
	return "", secret.ErrNotFound
}

func (p *identityMapProvider) Set(_ context.Context, name, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[name] = value
	return nil
}

func (p *identityMapProvider) Delete(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, name)
	return nil
}

// providerWithWatchword returns a provider seeded with the canonical all-zero
// BIP-39 test mnemonic so identity keys derive deterministically.
func providerWithWatchword(t *testing.T) *identityMapProvider {
	t.Helper()
	m, err := bip39.NewMnemonic(make([]byte, 32))
	if err != nil {
		t.Fatalf("build test mnemonic: %v", err)
	}
	p := &identityMapProvider{m: map[string]string{}}
	if serr := p.Set(context.Background(), watchword.KeychainKey, m); serr != nil {
		t.Fatalf("seed watchword: %v", serr)
	}
	return p
}

// TestSeedIdentity_NoWatchwordIsDeferred confirms genesis is a no-op (not an
// error) before the wizard stores the Watchword: a pre-wizard daemon boot must
// not fail, it defers identity provisioning to a later boot.
func TestSeedIdentity_NoWatchwordIsDeferred(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	idStore := identity.New(&identityMapProvider{m: map[string]string{}})

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	if serr := kernelboot.SeedIdentity(ctx, log, nil, idStore, nil, dataDir); serr != nil {
		t.Fatalf("SeedIdentity with no Watchword should defer (nil), got: %v", serr)
	}
	if uid, _ := config.SelfPrincipalUID(dataDir); uid != "" {
		t.Errorf("self bound to %q before Watchword set; want unbound", uid)
	}
}

// TestSeedIdentity_GenesisRoundTrip is the headline acceptance: a first boot
// provisions identity, appends signed principalRegistered + deviceEnrolled
// events, and a from-log Rebuild reproduces the authorship projection
// deterministically (principals row + person entity + devices row). A second
// boot is idempotent (no duplicate genesis events).
func TestSeedIdentity_GenesisRoundTrip(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == "darwin" {
		t.Skip("macOS race detector collides with sqlite-vec cgo mmap + Argon2id 64MiB; Linux CI runs this under race")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	idStore := identity.New(providerWithWatchword(t))

	// A real dispatcher provides the principals/devices/entities projection
	// (migration 0028) plus the principalRegistered gate query.
	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = disp.Close() })
	db, err := disp.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	// First boot: provision + append genesis (anchorer nil → skip GitHub).
	if serr := kernelboot.SeedIdentity(ctx, log, db, idStore, nil, dataDir); serr != nil {
		t.Fatalf("SeedIdentity (first boot): %v", serr)
	}

	principalUID, err := config.SelfPrincipalUID(dataDir)
	if err != nil || principalUID == "" {
		t.Fatalf("self binding after genesis = (%q, %v), want a bound principal", principalUID, err)
	}

	// From-log Rebuild reproduces the authorship projection.
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		t.Fatalf("Rebuild: %v", rerr)
	}

	// principals row exists for the bound principal, materialized as a person.
	assertProjectedPrincipal(t, ctx, db, principalUID, idStore)

	// devices row enables the verifier lookup.
	id, err := idStore.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	pub, err := disp.DevicePublicKey(ctx, id.SigKeyID, principalUID)
	if err != nil {
		t.Fatalf("DevicePublicKey: %v", err)
	}
	if !pub.Equal(id.SignPublic) {
		t.Error("enrolled device key does not match the derived signing key")
	}

	// The lookup is principal-scoped: the same sig key id under a different
	// principal must not resolve, so an enrolled key cannot speak for another.
	if _, werr := disp.DevicePublicKey(ctx, id.SigKeyID, "01PRINCIPAL000000000000999"); werr == nil {
		t.Error("DevicePublicKey resolved a key for a mismatched principal; want not-found")
	}

	// Second boot: the principals row now exists, so genesis is skipped.
	if serr := kernelboot.SeedIdentity(ctx, log, db, idStore, nil, dataDir); serr != nil {
		t.Fatalf("SeedIdentity (second boot): %v", serr)
	}
	assertPrincipalCount(t, ctx, db, principalUID, 1)
}

func assertProjectedPrincipal(t *testing.T, ctx context.Context, db *sql.DB, principalUID string, idStore *identity.Store) {
	t.Helper()
	var signPub, anchor string
	if err := db.QueryRowContext(ctx,
		`SELECT sign_public_key, github_anchor FROM principals WHERE principal_uid = ?`, principalUID).
		Scan(&signPub, &anchor); err != nil {
		t.Fatalf("query principals: %v", err)
	}
	// With a nil anchorer the principal is unanchored (offline-resilient path).
	if anchor != ontology.GitHubAnchorNone {
		t.Errorf("github_anchor = %q, want %q (nil anchorer)", anchor, ontology.GitHubAnchorNone)
	}
	id, err := idStore.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if signPub != base64.StdEncoding.EncodeToString(id.SignPublic) {
		t.Error("principals sign_public_key does not match the derived key")
	}

	var kind, status string
	if err := db.QueryRowContext(ctx,
		`SELECT kind, resolution_status FROM entities WHERE uid = ?`, principalUID).
		Scan(&kind, &status); err != nil {
		t.Fatalf("query principal entity: %v", err)
	}
	if kind != "person" || status != "resolved" {
		t.Errorf("principal entity = (%q, %q), want (person, resolved)", kind, status)
	}
}

func assertPrincipalCount(t *testing.T, ctx context.Context, db *sql.DB, principalUID string, want int) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM principals WHERE principal_uid = ?`, principalUID).Scan(&n); err != nil {
		t.Fatalf("count principals: %v", err)
	}
	if n != want {
		t.Errorf("principals count = %d, want %d", n, want)
	}
}

// pairedGenesisEvents synthesizes the synced shape a paired device's
// projection holds before its first boot: device 0's principalRegistered +
// self-signed deviceEnrolled, exactly what the personal-scope consume loop
// materializes after `maz sync pull`.
func pairedGenesisEvents(t *testing.T, rootStore *identity.Store, principalUID string) []kernel.Event {
	t.Helper()
	ctx := context.Background()
	rootID, err := rootStore.Initialize(ctx)
	if err != nil {
		t.Fatalf("root Initialize: %v", err)
	}
	rootPriv, err := rootStore.SigningKey(ctx)
	if err != nil {
		t.Fatalf("root SigningKey: %v", err)
	}
	signPubB64 := base64.StdEncoding.EncodeToString(rootID.SignPublic)
	principalPayload, err := json.Marshal(ontology.PrincipalRegisteredEvent{
		PrincipalUID:  principalUID,
		DisplayName:   "paired-test",
		SignPublicKey: signPubB64,
		WrapPublicKey: base64.StdEncoding.EncodeToString(rootID.WrapPublic.Bytes()),
		SigKeyID:      rootID.SigKeyID,
		CreateTime:    time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		GitHubAnchor:  ontology.GitHubAnchorNone,
	})
	if err != nil {
		t.Fatalf("marshal principal: %v", err)
	}
	enrollSig := ed25519.Sign(rootPriv,
		ontology.DeviceEnrollmentBytes(rootID.DeviceID, principalUID, signPubB64, rootID.SigKeyID))
	devicePayload, err := json.Marshal(ontology.DeviceEnrolledEvent{
		DeviceID:      rootID.DeviceID,
		PrincipalUID:  principalUID,
		SignPublicKey: signPubB64,
		SigKeyID:      rootID.SigKeyID,
		DeviceLabel:   "device-0",
		Signature:     base64.StdEncoding.EncodeToString(enrollSig),
		CreateTime:    time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal device: %v", err)
	}
	author := kernel.EventAuthor{PrincipalUID: principalUID, DeviceID: rootID.DeviceID, SigKeyID: rootID.SigKeyID}
	return []kernel.Event{
		{
			UID: "01EVPAIRPRIN0000000000001", Kind: kernel.EventKind(ontology.KindPrincipalRegistered),
			Source: "genesis", Confidence: 1, Payload: principalPayload, Author: author,
		},
		{
			UID: "01EVPAIRDEV00000000000001", Kind: kernel.EventKind(ontology.KindDeviceEnrolled),
			Source: "genesis", Confidence: 1, Payload: devicePayload, Author: author,
		},
	}
}

// TestSeedIdentity_PairedSubkeyDeviceEnrolls covers the second half of Mellon
// pairing: a paired device (same Watchword, adopted bundle device id, minted
// random subkey, personal log already synced) must recover the principal from
// device 0's enrollment, keep its OWN device identity, and append a
// principal-vouched deviceEnrolled introducing its subkey. A second boot is
// idempotent once the enrollment has applied.
func TestSeedIdentity_PairedSubkeyDeviceEnrolls(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == "darwin" {
		t.Skip("macOS race detector collides with sqlite-vec cgo mmap + Argon2id 64MiB; Linux CI runs this under race")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	const (
		principalUID    = "01PRINCIPALPAIRED00000001"
		pairedDeviceUID = "01PAIREDDEVICE00000000001"
	)

	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	t.Cleanup(func() { _ = disp.Close() })
	db, err := disp.OpenSQLite(ctx)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	// The synced projection: device 0's identity rows.
	rootStore := identity.New(providerWithWatchword(t))
	if aerr := disp.Apply(ctx, pairedGenesisEvents(t, rootStore, principalUID)); aerr != nil {
		t.Fatalf("seed synced genesis rows: %v", aerr)
	}

	// The paired device: what applyPairBundle leaves behind — same Watchword
	// in its own keychain, the bundle's device id, a fresh random subkey.
	pairedStore := identity.New(providerWithWatchword(t))
	if _, aerr := pairedStore.AdoptDeviceID(ctx, pairedDeviceUID); aerr != nil {
		t.Fatalf("AdoptDeviceID: %v", aerr)
	}
	if _, merr := pairedStore.MintDeviceSubkey(ctx); merr != nil {
		t.Fatalf("MintDeviceSubkey: %v", merr)
	}

	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = closeLog() })

	if serr := kernelboot.SeedIdentity(ctx, log, db, pairedStore, nil, dataDir); serr != nil {
		t.Fatalf("SeedIdentity (paired boot): %v", serr)
	}

	// Recovered the existing principal — no second genesis.
	if uid, _ := config.SelfPrincipalUID(dataDir); uid != principalUID {
		t.Fatalf("bound principal = %q, want recovered %q", uid, principalUID)
	}
	assertPrincipalCount(t, ctx, db, principalUID, 1)

	// The binding carries the PAIRED device id, not device 0's.
	st, err := config.LoadUserState(dataDir)
	if err != nil {
		t.Fatalf("LoadUserState: %v", err)
	}
	if st.Identity.DeviceID != pairedDeviceUID {
		t.Errorf("bound device id = %q, want the paired device's own %q", st.Identity.DeviceID, pairedDeviceUID)
	}

	// A principal-vouched deviceEnrolled for the subkey was appended.
	subPub, err := pairedStore.DeviceSigningPublicKey(ctx)
	if err != nil {
		t.Fatalf("DeviceSigningPublicKey: %v", err)
	}
	subKid := identity.SigKeyID(subPub)
	countSubkeyEnrollments := func() (int, kernel.Event) {
		t.Helper()
		var n int
		var last kernel.Event
		for ev, rerr := range log.Read(ctx, kernel.ReadOpts{Kinds: []kernel.EventKind{kernel.EventKind(ontology.KindDeviceEnrolled)}}) {
			if rerr != nil {
				t.Fatalf("read log: %v", rerr)
			}
			var dev ontology.DeviceEnrolledEvent
			if uerr := json.Unmarshal(ev.Payload, &dev); uerr != nil {
				t.Fatalf("decode enrollment: %v", uerr)
			}
			if dev.SigKeyID != subKid {
				continue
			}
			if dev.VouchSignature == "" || dev.VouchSigKeyID == "" {
				t.Error("subkey enrollment missing the principal vouch")
			}
			if dev.DeviceID != pairedDeviceUID {
				t.Errorf("enrollment device id = %q, want %q", dev.DeviceID, pairedDeviceUID)
			}
			if dev.PrincipalUID != principalUID {
				t.Errorf("enrollment principal = %q, want %q", dev.PrincipalUID, principalUID)
			}
			n++
			last = ev
		}
		return n, last
	}
	n, enrollEv := countSubkeyEnrollments()
	if n != 1 {
		t.Fatalf("subkey enrollments in log = %d, want 1", n)
	}

	// Apply the enrollment (what the daemon pipeline does), then boot again:
	// the seeder must be a no-op.
	if aerr := disp.Apply(ctx, []kernel.Event{enrollEv}); aerr != nil {
		t.Fatalf("apply appended enrollment: %v", aerr)
	}
	if serr := kernelboot.SeedIdentity(ctx, log, db, pairedStore, nil, dataDir); serr != nil {
		t.Fatalf("SeedIdentity (second boot): %v", serr)
	}
	if n2, _ := countSubkeyEnrollments(); n2 != 1 {
		t.Errorf("subkey enrollments after second boot = %d, want 1 (idempotent)", n2)
	}
}
