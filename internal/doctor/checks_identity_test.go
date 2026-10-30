package doctor

import (
	"context"
	"runtime"
	"sync"
	"testing"

	"github.com/tyler-smith/go-bip39"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// testPrincipalUID is a syntactically valid 26-char Crockford-base32 ULID used
// as the bound principal in the detection fixtures.
const testPrincipalUID = "01J0PRINCIPAL0000000000000"

// TestSelfPrincipalLinkCheck_Detection exercises Run across the four states the
// check distinguishes, using hand-built SQLite fixtures (no identity ceremony):
// unbound and principal==self short-circuit to OK before touching storage; an
// un-projected principal is OK (deferred); a registered-but-unlinked principal
// is the repairable FAIL.
func TestSelfPrincipalLinkCheck_Detection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name           string
		bind           string // principal UID to persist to state ("" = unbound)
		insertRegister bool   // insert a principals projection row for bind
		want           Status
	}{
		{"unbound returns ok", "", false, StatusOK},
		{"principal is the self anchor returns ok", "self", false, StatusOK},
		{"principal not yet projected returns ok", testPrincipalUID, false, StatusOK},
		{"registered but unlinked returns fail", testPrincipalUID, true, StatusFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			if tc.bind != "" {
				if err := config.BindSelfPrincipal(dataDir, tc.bind, "device-test", "kid:test"); err != nil {
					t.Fatalf("BindSelfPrincipal: %v", err)
				}
			}
			shared := &sharedSQLite{path: sqliteIndexPath(dataDir)}
			if tc.insertRegister {
				insertPrincipalRow(t, ctx, shared, tc.bind)
			}
			check := newSelfPrincipalLinkCheck(dataDir, shared)
			status, detail, err := check.Run(ctx)
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if status != tc.want {
				t.Fatalf("status = %q (detail %q), want %q", status, detail, tc.want)
			}
		})
	}
}

// TestSelfPrincipalLinkCheck_RemediationContract pins the remediation shape the
// fix command relies on: a non-nil, Safe descriptor with a description and an
// Apply func.
func TestSelfPrincipalLinkCheck_RemediationContract(t *testing.T) {
	t.Parallel()
	check := newSelfPrincipalLinkCheck(t.TempDir(), &sharedSQLite{path: "unused"})
	rem := RemediationOf(check)
	if rem == nil {
		t.Fatal("self-principal link check must expose a remediation")
	}
	if !rem.Safe {
		t.Error("remediation must be Safe: it is an idempotent, personal-scope append")
	}
	if rem.Description == "" {
		t.Error("remediation Description must be non-empty for the confirm prompt")
	}
	if rem.Apply == nil {
		t.Error("remediation Apply must be set")
	}
}

// TestRemediationOf_CheckFunc confirms the nilable contract on the function
// adapter: a CheckFunc with no Rem is non-remediable; one with a Rem is
// remediable.
func TestRemediationOf_CheckFunc(t *testing.T) {
	t.Parallel()
	c := CheckFunc{N: "x", Sub: "storage", Fn: func(context.Context) (Status, string, error) {
		return StatusFail, "", nil
	}}
	if rem := RemediationOf(c); rem != nil {
		t.Errorf("CheckFunc with nil Rem must be non-remediable, got %+v", rem)
	}
	c.Rem = &Remediation{Description: "do it", Safe: true, Apply: func(context.Context) error { return nil }}
	if rem := RemediationOf(c); rem == nil {
		t.Error("CheckFunc carrying a Rem must be remediable")
	}
}

// TestSelfPrincipalLinkCheck_RepairEndToEnd is the headline acceptance: a
// genesis'd install with a projected principal but no self link FAILs; applying
// the remediation repairs it; a re-run is OK; a second apply is an idempotent
// no-op leaving exactly one live link.
func TestSelfPrincipalLinkCheck_RepairEndToEnd(t *testing.T) {
	t.Parallel()
	if raceDetectorEnabled && runtime.GOOS == "darwin" {
		t.Skip("macOS race detector collides with sqlite-vec cgo mmap + Argon2id 64MiB; Linux CI runs this under race")
	}
	ctx := context.Background()
	dataDir := t.TempDir()

	// Arrange: genesis a principal and project it, but never link the self
	// anchor. seedPrincipalNoLink releases all single-process locks on return
	// so the remediation can open its own dispatcher + event log.
	principalUID := seedPrincipalNoLink(t, ctx, dataDir)

	shared := &sharedSQLite{path: sqliteIndexPath(dataDir)}
	check := newSelfPrincipalLinkCheck(dataDir, shared)

	// Pre-fix: the link is absent, so the check FAILs.
	if status, detail, err := check.Run(ctx); err != nil || status != StatusFail {
		t.Fatalf("pre-fix Run = (%q, %q, %v), want FAIL", status, detail, err)
	}

	rem := RemediationOf(check)
	if rem == nil {
		t.Fatal("expected a remediation on the failing check")
	}

	// Fix: apply once; the check now passes.
	if err := rem.Apply(ctx); err != nil {
		t.Fatalf("Apply (first run): %v", err)
	}
	if status, detail, err := check.Run(ctx); err != nil || status != StatusOK {
		t.Fatalf("post-fix Run = (%q, %q, %v), want OK", status, detail, err)
	}

	// Idempotent: a second apply is a no-op; still OK, still exactly one link.
	if err := rem.Apply(ctx); err != nil {
		t.Fatalf("Apply (second run): %v", err)
	}
	if status, _, err := check.Run(ctx); err != nil || status != StatusOK {
		t.Fatalf("post-second-fix Run = %q (%v), want OK", status, err)
	}
	if n := liveLinkCount(t, ctx, shared, principalUID); n != 1 {
		t.Fatalf("live self->principal links = %d, want exactly 1 (idempotent)", n)
	}
}

// seedPrincipalNoLink runs genesis under dataDir so a principal is bound,
// registered, and projected, but the self->principal same_as link is never
// created. It closes the dispatcher and event log before returning so the
// remediation under test can take the single-process locks itself.
func seedPrincipalNoLink(t *testing.T, ctx context.Context, dataDir string) string {
	t.Helper()
	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: dataDir})
	if err != nil {
		t.Fatalf("dispatcher.Open: %v", err)
	}
	db, err := disp.OpenSQLite(ctx)
	if err != nil {
		_ = disp.Close()
		t.Fatalf("OpenSQLite: %v", err)
	}
	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		_ = disp.Close()
		t.Fatalf("OpenLog: %v", err)
	}

	idStore := identity.New(watchwordProvider(t))
	// nil anchorer mints the principal under a ULID (never "self").
	if serr := kernelboot.SeedIdentity(ctx, log, db, idStore, nil, dataDir); serr != nil {
		t.Fatalf("SeedIdentity: %v", serr)
	}
	principalUID, perr := config.SelfPrincipalUID(dataDir)
	if perr != nil || principalUID == "" || principalUID == selfAnchorUID {
		t.Fatalf("self binding after genesis = (%q, %v); want a real ULID", principalUID, perr)
	}
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		t.Fatalf("Rebuild (post-genesis): %v", rerr)
	}

	if cerr := closeLog(); cerr != nil {
		t.Fatalf("close event log: %v", cerr)
	}
	if cerr := disp.Close(); cerr != nil {
		t.Fatalf("close dispatcher: %v", cerr)
	}
	return principalUID
}

// insertPrincipalRow writes a minimal principals projection row so the check
// observes a registered principal without running the identity ceremony.
func insertPrincipalRow(t *testing.T, ctx context.Context, s *sharedSQLite, principalUID string) {
	t.Helper()
	store, err := s.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire sqlite: %v", err)
	}
	_, err = store.Writer().ExecContext(ctx,
		`INSERT INTO principals
		   (principal_uid, display_name, sign_public_key, wrap_public_key, sig_key_id, create_time)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		principalUID, "Test Principal", "sign-pk", "wrap-pk", "kid:test", "2026-06-24T00:00:00Z")
	if err != nil {
		t.Fatalf("insert principal row: %v", err)
	}
}

// liveLinkCount returns the number of live (not retracted/superseded) same_as
// edges from the self anchor to principalUID.
func liveLinkCount(t *testing.T, ctx context.Context, s *sharedSQLite, principalUID string) int {
	t.Helper()
	store, err := s.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire sqlite: %v", err)
	}
	const q = `
		SELECT COUNT(*) FROM relations
		WHERE subject_uid = 'self' AND predicate = 'same_as' AND object_uid = ?
		  AND (superseded_by_uid IS NULL OR superseded_by_uid = '')`
	var n int
	if err := store.Reader().QueryRowContext(ctx, q, principalUID).Scan(&n); err != nil {
		t.Fatalf("count live links: %v", err)
	}
	return n
}

// mapSecretProvider is an in-memory secret.Provider for the genesis ceremony,
// mirroring kernelboot's test provider.
type mapSecretProvider struct {
	mu sync.Mutex
	m  map[string]string
}

func (p *mapSecretProvider) Get(_ context.Context, name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.m[name]; ok {
		return v, nil
	}
	return "", secret.ErrNotFound
}

func (p *mapSecretProvider) Set(_ context.Context, name, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[name] = value
	return nil
}

func (p *mapSecretProvider) Delete(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m, name)
	return nil
}

// watchwordProvider returns a provider seeded with the canonical all-zero
// BIP-39 test mnemonic so identity keys derive deterministically.
func watchwordProvider(t *testing.T) *mapSecretProvider {
	t.Helper()
	m, err := bip39.NewMnemonic(make([]byte, 32))
	if err != nil {
		t.Fatalf("build test mnemonic: %v", err)
	}
	p := &mapSecretProvider{m: map[string]string{}}
	if serr := p.Set(context.Background(), watchword.KeychainKey, m); serr != nil {
		t.Fatalf("seed watchword: %v", serr)
	}
	return p
}
