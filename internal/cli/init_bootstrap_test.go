package cli

// init_bootstrap_test.go pins the contract that `maz init` leaves the
// fresh data-dir with a working git-mirror surface: a local bare repo
// under <data-dir>/local-mirror.git and a `[sync.remotes.personal]`
// entry in the user config pointing at it. Without this, `maz show` /
// `maz blame` / `maz changelog` / `maz diff` all error with "no
// scopes configured" until the user hand-edits config.toml — which is
// the regression the user flagged after PR #144 verification.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

func TestBootstrapLocalScope_CreatesBareRepoAndWiresRemote(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
	}

	if err := bootstrapLocalScope(context.Background(), dataDir, &cfg); err != nil {
		t.Fatalf("bootstrapLocalScope: %v", err)
	}

	bare := filepath.Join(dataDir, "local-mirror.git")
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		t.Fatalf("expected bare repo HEAD at %s, got: %v", bare, err)
	}
	if _, err := os.Stat(filepath.Join(bare, "objects")); err != nil {
		t.Fatalf("expected objects/ inside bare repo, got: %v", err)
	}

	remote, ok := cfg.Sync.Remotes["personal"]
	if !ok {
		t.Fatalf("cfg.Sync.Remotes[personal] not set; have keys: %v", keysOf(cfg.Sync.Remotes))
	}
	want := "file://" + bare
	if remote.URL != want {
		t.Errorf("remote URL = %q, want %q", remote.URL, want)
	}
	if remote.AuthMethod != "none" {
		t.Errorf("remote AuthMethod = %q, want \"none\" (file:// needs no auth)", remote.AuthMethod)
	}
	// Encryption must be explicitly off (not nil) so the daemon's
	// encryption-default migration never flips the local mirror to ciphertext.
	if remote.Encrypted == nil || *remote.Encrypted {
		t.Errorf("remote Encrypted = %v, want explicit false (encryption off by default)", remote.Encrypted)
	}
}

func TestBootstrapLocalScope_IdempotentOnRerun(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.UserConfig{SchemaVersion: config.CurrentUserConfigSchemaVersion}

	if err := bootstrapLocalScope(context.Background(), dataDir, &cfg); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if err := bootstrapLocalScope(context.Background(), dataDir, &cfg); err != nil {
		t.Fatalf("rerun bootstrap (expected no-op): %v", err)
	}
	if len(cfg.Sync.Remotes) != 1 {
		t.Errorf("expected 1 remote after rerun, got %d", len(cfg.Sync.Remotes))
	}
}

func TestBootstrapLocalScope_PreservesExistingRemote(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.UserConfig{
		SchemaVersion: config.CurrentUserConfigSchemaVersion,
		Sync: config.SyncTable{
			Remotes: map[string]config.ScopeRemoteTable{
				"personal": {URL: "git@github.com:user/notes.git", AuthMethod: "ssh"},
			},
		},
	}
	if err := bootstrapLocalScope(context.Background(), dataDir, &cfg); err != nil {
		t.Fatalf("bootstrap with existing remote: %v", err)
	}
	if cfg.Sync.Remotes["personal"].URL != "git@github.com:user/notes.git" {
		t.Errorf("existing remote was overwritten: %q", cfg.Sync.Remotes["personal"].URL)
	}
	// No bare repo should be created either — the user has a real remote.
	if _, err := os.Stat(filepath.Join(dataDir, "local-mirror.git")); !os.IsNotExist(err) {
		t.Errorf("local-mirror.git should not exist when a real remote is configured")
	}
}

func keysOf[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
