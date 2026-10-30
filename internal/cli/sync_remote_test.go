package cli

// sync_remote_test.go pins `maz sync remote` — the post-init surface for
// connecting (set), creating (create), and inspecting (show) a scope's git
// remote. GitHub creation is stubbed (no live gh); the set path is exercised
// against a local file:// mirror so no network is touched.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

func TestNormalizeRemoteURL(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"git@github.com:me/repo.git", "git@github.com:me/repo.git"},
		{"https://github.com/me/repo.git", "https://github.com/me/repo.git"},
		{"  ssh://host/x.git  ", "ssh://host/x.git"},
		{"/abs/path/mirror.git", "file:///abs/path/mirror.git"},
		{"file:///already/file.git", "file:///already/file.git"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeRemoteURL(c.in); got != c.want {
			t.Errorf("normalizeRemoteURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInferAuthMethod(t *testing.T) {
	t.Parallel()
	cases := []struct{ url, want string }{
		{"git@github.com:me/r.git", authMethodSSH},
		{"ssh://git@host/r.git", authMethodSSH},
		{"https://github.com/me/r.git", authMethodNone},
		{"file:///x/mirror.git", authMethodNone},
	}
	for _, c := range cases {
		if got := inferAuthMethod(c.url); got != c.want {
			t.Errorf("inferAuthMethod(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestValidateAuthMethodAndScope(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{authMethodSSH, authMethodHTTPS, authMethodNone} {
		if err := validateAuthMethod(ok); err != nil {
			t.Errorf("validateAuthMethod(%q) unexpected error: %v", ok, err)
		}
	}
	if err := validateAuthMethod("bogus"); err == nil {
		t.Error("validateAuthMethod(bogus) should error")
	}
	for _, ok := range []string{"personal", "project", "team"} {
		if err := validateSyncScope(ok); err != nil {
			t.Errorf("validateSyncScope(%q) unexpected error: %v", ok, err)
		}
	}
	if err := validateSyncScope("nope"); err == nil {
		t.Error("validateSyncScope(nope) should error")
	}
}

func TestPersistScopeRemote_RoundTrips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	enc := true
	if err := persistScopeRemote(dir, "personal", func(r *config.ScopeRemoteTable) {
		r.URL = "git@github.com:me/notes.git"
		r.AuthMethod = authMethodSSH
		r.Encrypted = &enc
	}); err != nil {
		t.Fatalf("persistScopeRemote: %v", err)
	}
	cfg, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	r := cfg.Sync.Remotes["personal"]
	if r.URL != "git@github.com:me/notes.git" || r.AuthMethod != authMethodSSH {
		t.Errorf("remote = %+v, want the github ssh url", r)
	}
	if r.Encrypted == nil || !*r.Encrypted {
		t.Errorf("Encrypted = %v, want explicit true", r.Encrypted)
	}
	if cfg.SchemaVersion == 0 {
		t.Error("SchemaVersion should be stamped on write")
	}
}

func TestSyncRemoteSet_WiresLocalMirror(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(t.TempDir(), "mirror.git")

	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	// --no-verify so the absent mirror does not trigger a `git ls-remote` warning.
	root.SetArgs([]string{"sync", "remote", "set", mirror, "--scope", "personal", "--no-verify", "--data-dir", dir})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (stderr=%q)", err, errBuf.String())
	}

	cfg, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	r := cfg.Sync.Remotes["personal"]
	if r.URL != "file://"+mirror {
		t.Errorf("URL = %q, want file://%s", r.URL, mirror)
	}
	if r.AuthMethod != authMethodNone {
		t.Errorf("AuthMethod = %q, want %q (inferred for file://)", r.AuthMethod, authMethodNone)
	}
}

func TestSyncRemoteCreate_StubbedGitHubWiresRemote(t *testing.T) {
	origLogin := detectGitHubLoginFn
	origCreate := createGitHubScopeRepoFn
	t.Cleanup(func() {
		detectGitHubLoginFn = origLogin
		createGitHubScopeRepoFn = origCreate
	})
	detectGitHubLoginFn = func(context.Context) string { return "octo" }
	createGitHubScopeRepoFn = func(_ context.Context, login, repo string) (string, error) {
		return "git@github.com:" + login + "/" + repo + ".git", nil
	}

	dir := t.TempDir()
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"sync", "remote", "create", "--name", "my-memory", "--scope", "personal", "--data-dir", dir})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (stderr=%q)", err, errBuf.String())
	}

	cfg, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	r := cfg.Sync.Remotes["personal"]
	if r.URL != "git@github.com:octo/my-memory.git" {
		t.Errorf("URL = %q, want the stubbed github ssh url", r.URL)
	}
	if r.AuthMethod != authMethodSSH {
		t.Errorf("AuthMethod = %q, want ssh", r.AuthMethod)
	}
}

func TestSyncRemoteRecreate_DropsMirrorWithYes(t *testing.T) {
	dir := t.TempDir()
	// A pre-existing remote and a populated local mirror to be dropped.
	if err := persistScopeRemote(dir, "personal", func(r *config.ScopeRemoteTable) {
		r.URL = "file:///x/old.git"
		r.AuthMethod = authMethodNone
	}); err != nil {
		t.Fatalf("persistScopeRemote: %v", err)
	}
	mirror := filepath.Join(dir, "sync", "personal", "repo")
	if err := os.MkdirAll(filepath.Join(mirror, "chunks"), 0o755); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}

	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"sync", "remote", "recreate", "--scope", "personal", "--yes", "--data-dir", dir})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (stderr=%q)", err, errBuf.String())
	}
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Errorf("mirror %s should be removed, stat err = %v", mirror, err)
	}
	// The remote URL must survive a recreate without --url (we only reset the
	// mirror, not the user's configured target).
	cfg, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if cfg.Sync.Remotes["personal"].URL != "file:///x/old.git" {
		t.Errorf("URL = %q, want it preserved when --url is omitted", cfg.Sync.Remotes["personal"].URL)
	}
}

func TestSyncRemoteRecreate_RepointsWithURL(t *testing.T) {
	dir := t.TempDir()
	newMirror := filepath.Join(t.TempDir(), "fresh.git")

	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{
		"sync", "remote", "recreate", "--scope", "personal",
		"--url", newMirror, "--no-verify", "--yes", "--data-dir", dir,
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (stderr=%q)", err, errBuf.String())
	}
	cfg, err := config.LoadUserConfig(dir)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if got := cfg.Sync.Remotes["personal"].URL; got != "file://"+newMirror {
		t.Errorf("URL = %q, want file://%s after repoint", got, newMirror)
	}
}

func TestSyncRemoteRecreate_RefusesScriptedWithoutYes(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(dir, "sync", "personal", "repo")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}

	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	// No TTY (buffers) and no --yes → must refuse and leave the mirror intact.
	root.SetArgs([]string{"sync", "remote", "recreate", "--scope", "personal", "--data-dir", dir})
	if err := root.Execute(); err == nil {
		t.Fatal("expected refusal without --yes in scripted mode")
	}
	if _, err := os.Stat(mirror); err != nil {
		t.Errorf("mirror must survive a refused recreate, stat err = %v", err)
	}
}

func TestSyncRemoteCreate_NoLoginIsActionable(t *testing.T) {
	origLogin := detectGitHubLoginFn
	t.Cleanup(func() { detectGitHubLoginFn = origLogin })
	detectGitHubLoginFn = func(context.Context) string { return "" }

	dir := t.TempDir()
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"sync", "remote", "create", "--data-dir", dir})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error when no GitHub login is available")
	}
	if !strings.Contains(err.Error(), "gh auth login") || !strings.Contains(err.Error(), "maz sync remote set") {
		t.Errorf("error %q should point at both `gh auth login` and `maz sync remote set`", err.Error())
	}
}
