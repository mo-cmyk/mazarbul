package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestFile_RoundTrip drives FileProvider through Set, Get, Delete against a
// real temp directory.
func TestFile_RoundTrip(t *testing.T) {
	p := fileProviderForTest(t)
	ctx := context.Background()
	// Colon-separated names are the identity/team key shape; they must be
	// storable even though they are invalid environment-variable names.
	const name = "mazarbul:identity:ed25519"
	const value = "seed-bytes-1234"

	if err := p.Set(ctx, name, value); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := p.Get(ctx, name)
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if got != value {
		t.Errorf("Get returned %q, want %q", got, value)
	}
	if err := p.Delete(ctx, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete returned %v, want ErrNotFound", err)
	}
}

// TestFile_GetMissingReturnsErrNotFound verifies the not-found translation.
func TestFile_GetMissingReturnsErrNotFound(t *testing.T) {
	p := fileProviderForTest(t)
	if _, err := p.Get(context.Background(), "never.set"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on missing key returned %v, want ErrNotFound", err)
	}
}

// TestFile_DeleteMissingReturnsErrNotFound mirrors the Get test for Delete.
func TestFile_DeleteMissingReturnsErrNotFound(t *testing.T) {
	p := fileProviderForTest(t)
	if err := p.Delete(context.Background(), "never.set"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete on missing key returned %v, want ErrNotFound", err)
	}
}

// TestFile_OverwriteIsAtomicLastWriterWins verifies Set overwrites an
// existing entry rather than appending or erroring.
func TestFile_OverwriteIsAtomicLastWriterWins(t *testing.T) {
	p := fileProviderForTest(t)
	ctx := context.Background()
	for _, v := range []string{"first", "second", "third"} {
		if err := p.Set(ctx, "k", v); err != nil {
			t.Fatalf("Set %q: %v", v, err)
		}
	}
	got, err := p.Get(ctx, "k")
	if err != nil || got != "third" {
		t.Errorf("Get after overwrites: got (%q, %v), want (\"third\", nil)", got, err)
	}
}

// TestFile_DistinctNamesNeverCollide checks the injective name encoding: two
// names that would sanitize to the same on-disk token under a naive scheme
// must remain separate entries.
func TestFile_DistinctNamesNeverCollide(t *testing.T) {
	p := fileProviderForTest(t)
	ctx := context.Background()
	if err := p.Set(ctx, "a:b", "x"); err != nil {
		t.Fatalf("Set a:b: %v", err)
	}
	if err := p.Set(ctx, "a_b", "y"); err != nil {
		t.Fatalf("Set a_b: %v", err)
	}
	if got, _ := p.Get(ctx, "a:b"); got != "x" {
		t.Errorf("a:b = %q, want x", got)
	}
	if got, _ := p.Get(ctx, "a_b"); got != "y" {
		t.Errorf("a_b = %q, want y", got)
	}
}

// TestFile_SecretFilesAreOwnerOnly verifies stored secrets land with 0600
// permissions so other local users cannot read them.
func TestFile_SecretFilesAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	p, err := NewFileProvider(dir)
	if err != nil {
		t.Fatalf("NewFileProvider: %v", err)
	}
	if err := p.Set(context.Background(), "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var checked int
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("Stat %s: %v", e.Name(), err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s perm = %o, want 600", e.Name(), perm)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no secret file written")
	}
}

// TestNewFileProvider_RejectsEmptyDir guards the constructor.
func TestNewFileProvider_RejectsEmptyDir(t *testing.T) {
	if _, err := NewFileProvider(""); err == nil {
		t.Error("NewFileProvider(\"\") returned nil error, want failure")
	}
}

// TestDefaultChain_UsesFileProviderWhenEnvSet verifies the chain selection:
// with EnvSecretsDir set, a written secret lands in that directory and is
// read back, with no environment override shadowing it.
func TestDefaultChain_UsesFileProviderWhenEnvSet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvSecretsDir, dir)
	ctx := context.Background()

	if err := DefaultChain().Set(ctx, "mazarbul:watchword", "correct horse"); err != nil {
		t.Fatalf("Set via DefaultChain: %v", err)
	}
	// A fresh chain reading the same dir must see it.
	got, err := DefaultChain().Get(ctx, "mazarbul:watchword")
	if err != nil || got != "correct horse" {
		t.Errorf("Get via DefaultChain: got (%q, %v), want (\"correct horse\", nil)", got, err)
	}
	// The value must physically exist under dir, not the keychain.
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		t.Error("EnvSecretsDir is empty; secret did not route to FileProvider")
	}
}

// TestDefaultChain_HeadlessFallsBackToStateDir verifies that when the OS
// keychain is unreachable (headless Linux) and EnvSecretsDir is unset, secrets
// land in $XDG_STATE_HOME/mazarbul/secrets instead of failing.
func TestDefaultChain_HeadlessFallsBackToStateDir(t *testing.T) {
	state := t.TempDir()
	t.Setenv(EnvSecretsDir, "")
	t.Setenv("XDG_STATE_HOME", state)
	orig := keychainReachable
	keychainReachable = func() bool { return false }
	t.Cleanup(func() { keychainReachable = orig })
	ctx := context.Background()

	if err := DefaultChain().Set(ctx, "mazarbul:watchword", "correct horse"); err != nil {
		t.Fatalf("Set via DefaultChain: %v", err)
	}
	got, err := PersistentChain().Get(ctx, "mazarbul:watchword")
	if err != nil || got != "correct horse" {
		t.Errorf("Get via PersistentChain: got (%q, %v), want (\"correct horse\", nil)", got, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(state, "mazarbul", "secrets")); len(entries) == 0 {
		t.Error("headless secrets dir is empty; secret did not route to FileProvider")
	}
}

// TestPersistentStore_KeychainWhenReachable verifies the fallback never
// triggers on a host whose keychain works.
func TestPersistentStore_KeychainWhenReachable(t *testing.T) {
	t.Setenv(EnvSecretsDir, "")
	orig := keychainReachable
	keychainReachable = func() bool { return true }
	t.Cleanup(func() { keychainReachable = orig })

	if _, ok := persistentStore().(*KeychainProvider); !ok {
		t.Errorf("persistentStore() = %T, want *KeychainProvider", persistentStore())
	}
}

func fileProviderForTest(t *testing.T) *FileProvider {
	t.Helper()
	p, err := NewFileProvider(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileProvider: %v", err)
	}
	return p
}
