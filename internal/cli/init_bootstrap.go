package cli

// init_bootstrap.go owns the post-config step of `maz init` that
// guarantees `maz show` / `maz blame` / `maz changelog` / `maz diff`
// work out of the box. A fresh book has events scoped to "personal"
// the moment the first `maz remember` lands, but those commands all
// read from the per-scope git mirror under <dataDir>/sync/<scope>/
// repo — which the daemon only creates when [sync.remotes.<scope>]
// is set. Hand-editing config.toml just to get history surfaces is
// the kind of paper-cut PR #144's reviewer caught.
//
// bootstrapLocalScope creates <dataDir>/local-mirror.git as a bare
// repo and points the "personal" scope at it via file://. The user
// can swap it for a real remote later — running `maz config set
// sync.remotes.personal.url git@github.com:...` overwrites the URL
// in place. Idempotent: a rerun is a no-op; an existing real remote
// is preserved verbatim.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// defaultLocalScopeName is the scope `maz remember` writes to by
// default; keeping it in one place lets a future scope-aware init
// expand without scattering literals.
const defaultLocalScopeName = "personal"

// localMirrorDirName is the basename of the bare git repo created
// inside the data-dir. Mirrors `git init --bare local-mirror.git`.
const localMirrorDirName = "local-mirror.git"

// bootstrapLocalScope is the surgical fix for the "no scopes
// configured" error users hit after `maz init --yes` when they reach
// for a git-like command. It mutates cfg in place and creates the
// bare repo on disk. If the user already has a real remote wired for
// defaultLocalScopeName, we leave it alone — that's a clear signal
// they want a paired sync target, not the local mirror.
func bootstrapLocalScope(ctx context.Context, dataDir string, cfg *config.UserConfig) error {
	if dataDir == "" {
		return errors.New("bootstrap-local-scope: dataDir is empty")
	}
	if cfg == nil {
		return errors.New("bootstrap-local-scope: cfg is nil")
	}
	if existing, ok := cfg.Sync.Remotes[defaultLocalScopeName]; ok && existing.URL != "" {
		return nil
	}

	barePath := filepath.Join(dataDir, localMirrorDirName)
	if err := ensureBareRepo(ctx, barePath); err != nil {
		return fmt.Errorf("bootstrap-local-scope: %w", err)
	}

	if cfg.Sync.Remotes == nil {
		cfg.Sync.Remotes = make(map[string]config.ScopeRemoteTable, 1)
	}
	// Encrypted is set explicitly to false (not left nil) so the daemon's
	// encryption-default migration never silently flips the scope to
	// ciphertext when a Watchword is present. Encryption stays an opt-in:
	// `maz config set sync.remotes.personal.encrypted true`.
	plaintext := false
	cfg.Sync.Remotes[defaultLocalScopeName] = config.ScopeRemoteTable{
		URL:        "file://" + barePath,
		AuthMethod: "none",
		Encrypted:  &plaintext,
	}
	return nil
}

// ensureBareRepo creates a bare git repo at path if one is not already
// present. Re-running on an existing bare repo is a no-op.
func ensureBareRepo(ctx context.Context, path string) error {
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git not in PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, gitBin, "init", "--bare", "--initial-branch=main", path)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init --bare %s: %w (%s)", path, err, string(out))
	}
	return nil
}
