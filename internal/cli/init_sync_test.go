package cli

// init_sync_test.go pins the init wizard's remote-sync step: GitHub repo
// creation (stubbed — no live gh), the local-mirror fallback, and the
// encryption-off-by-default posture that keeps the interactive and --yes
// paths consistent.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

func TestApplyInitSyncRemote_GitHubSuccessWiresSSHRemote(t *testing.T) {
	orig := createGitHubScopeRepoFn
	t.Cleanup(func() { createGitHubScopeRepoFn = orig })
	createGitHubScopeRepoFn = func(_ context.Context, login, repo string) (string, error) {
		return "git@github.com:" + login + "/" + repo + ".git", nil
	}

	var buf bytes.Buffer
	cfg := config.UserConfig{}
	err := applyInitSyncRemote(context.Background(), &buf, t.TempDir(), &cfg, syncTargetGitHub, "alice", "maz-personal", false)
	if err != nil {
		t.Fatalf("applyInitSyncRemote: %v", err)
	}
	r := cfg.Sync.Remotes[defaultLocalScopeName]
	if r.URL != "git@github.com:alice/maz-personal.git" {
		t.Errorf("URL = %q, want the github ssh url", r.URL)
	}
	if r.AuthMethod != "ssh" {
		t.Errorf("AuthMethod = %q, want ssh", r.AuthMethod)
	}
	if r.Encrypted == nil || *r.Encrypted {
		t.Errorf("Encrypted = %v, want explicit false (encryption off by default)", r.Encrypted)
	}
}

func TestApplyInitSyncRemote_EncryptOnSetsEncryptedTrue(t *testing.T) {
	orig := createGitHubScopeRepoFn
	t.Cleanup(func() { createGitHubScopeRepoFn = orig })
	createGitHubScopeRepoFn = func(_ context.Context, login, repo string) (string, error) {
		return "git@github.com:" + login + "/" + repo + ".git", nil
	}

	t.Run("github target honours encrypt=true", func(t *testing.T) {
		var buf bytes.Buffer
		cfg := config.UserConfig{}
		if err := applyInitSyncRemote(context.Background(), &buf, t.TempDir(), &cfg, syncTargetGitHub, "alice", "notes", true); err != nil {
			t.Fatalf("applyInitSyncRemote: %v", err)
		}
		r := cfg.Sync.Remotes[defaultLocalScopeName]
		if r.Encrypted == nil || !*r.Encrypted {
			t.Errorf("Encrypted = %v, want explicit true", r.Encrypted)
		}
	})

	t.Run("local target honours encrypt=true", func(t *testing.T) {
		var buf bytes.Buffer
		cfg := config.UserConfig{}
		if err := applyInitSyncRemote(context.Background(), &buf, t.TempDir(), &cfg, syncTargetLocal, "alice", "notes", true); err != nil {
			t.Fatalf("applyInitSyncRemote: %v", err)
		}
		r := cfg.Sync.Remotes[defaultLocalScopeName]
		if r.Encrypted == nil || !*r.Encrypted {
			t.Errorf("Encrypted = %v, want explicit true on the local mirror", r.Encrypted)
		}
	})
}

func TestApplyInitSyncRemote_GitHubFailureFallsBackToLocalMirror(t *testing.T) {
	orig := createGitHubScopeRepoFn
	t.Cleanup(func() { createGitHubScopeRepoFn = orig })
	createGitHubScopeRepoFn = func(_ context.Context, _, _ string) (string, error) {
		return "", errors.New("name already exists")
	}

	var buf bytes.Buffer
	cfg := config.UserConfig{}
	err := applyInitSyncRemote(context.Background(), &buf, t.TempDir(), &cfg, syncTargetGitHub, "alice", "maz-personal", false)
	if err != nil {
		t.Fatalf("applyInitSyncRemote: %v", err)
	}
	r := cfg.Sync.Remotes[defaultLocalScopeName]
	if !strings.HasPrefix(r.URL, "file://") {
		t.Errorf("URL = %q, want a file:// local mirror after fallback", r.URL)
	}
	if !strings.Contains(buf.String(), "GitHub repo setup failed") {
		t.Errorf("expected a fallback warning, got %q", buf.String())
	}
	if r.Encrypted == nil || *r.Encrypted {
		t.Errorf("Encrypted = %v, want explicit false", r.Encrypted)
	}
}

func TestApplyInitSyncRemote_LocalTargetUsesMirror(t *testing.T) {
	// The github stub must NOT be called for a local target.
	orig := createGitHubScopeRepoFn
	t.Cleanup(func() { createGitHubScopeRepoFn = orig })
	called := false
	createGitHubScopeRepoFn = func(_ context.Context, _, _ string) (string, error) {
		called = true
		return "", nil
	}

	var buf bytes.Buffer
	cfg := config.UserConfig{}
	err := applyInitSyncRemote(context.Background(), &buf, t.TempDir(), &cfg, syncTargetLocal, "alice", "maz-personal", false)
	if err != nil {
		t.Fatalf("applyInitSyncRemote: %v", err)
	}
	if called {
		t.Error("createGitHubScopeRepoFn was called for a local target")
	}
	if !strings.HasPrefix(cfg.Sync.Remotes[defaultLocalScopeName].URL, "file://") {
		t.Errorf("URL = %q, want a file:// local mirror", cfg.Sync.Remotes[defaultLocalScopeName].URL)
	}
}

func TestValidateRepoName(t *testing.T) {
	t.Run("accepts a normal slug", func(t *testing.T) {
		if err := validateRepoName("maz-personal.notes_1"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("rejects empty", func(t *testing.T) {
		if err := validateRepoName("   "); err == nil {
			t.Error("expected error for empty name")
		}
	})
	t.Run("rejects illegal characters", func(t *testing.T) {
		if err := validateRepoName("bad name/slash"); err == nil {
			t.Error("expected error for space/slash")
		}
	})
}

func TestSyncSummaryLine(t *testing.T) {
	t.Run("local mirror plaintext", func(t *testing.T) {
		f := false
		cfg := config.UserConfig{Sync: config.SyncTable{Remotes: map[string]config.ScopeRemoteTable{
			defaultLocalScopeName: {URL: "file:///x/local-mirror.git", Encrypted: &f},
		}}}
		got := syncSummaryLine(&cfg)
		if !strings.Contains(got, "local mirror") || !strings.Contains(got, "encryption off") {
			t.Errorf("summary = %q", got)
		}
	})
	t.Run("github encrypted", func(t *testing.T) {
		tr := true
		cfg := config.UserConfig{Sync: config.SyncTable{Remotes: map[string]config.ScopeRemoteTable{
			defaultLocalScopeName: {URL: "git@github.com:a/b.git", Encrypted: &tr},
		}}}
		got := syncSummaryLine(&cfg)
		if !strings.Contains(got, "github.com") || !strings.Contains(got, "encrypted") {
			t.Errorf("summary = %q", got)
		}
	})
}
