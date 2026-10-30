package cli

// init_sync.go owns the `maz init` wizard's remote-sync step. When the
// gh CLI surfaces an authenticated GitHub login, the wizard offers to
// create a private repo for the "personal" scope and point sync at it
// over SSH; otherwise (or on any failure) it falls back to the local
// mirror that bootstrapLocalScope creates. Encryption is left OFF by
// default for convenience — the operator opts in later with
// `maz config set sync.remotes.personal.encrypted true`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// Sync-target choices offered by the init wizard's remote step.
const (
	syncTargetLocal  = "local"
	syncTargetGitHub = "github"
)

// defaultPersonalRepoName is the GitHub repository name the wizard
// pre-fills for the personal scope. Private by default — memory is private.
const defaultPersonalRepoName = "maz-personal"

// detectGitHubLoginFn resolves the authenticated GitHub login via the gh
// CLI. A package var so tests can stub detection without invoking gh.
var detectGitHubLoginFn = detectGitHubLogin //nolint:gochecknoglobals // test seam

// createGitHubScopeRepoFn creates the private repo and returns its SSH
// clone URL. A package var so tests can stub repo creation (no live gh).
var createGitHubScopeRepoFn = createGitHubScopeRepo //nolint:gochecknoglobals // test seam

// detectGitHubLogin returns the authenticated GitHub login from
// `gh api user`, or "" when gh is absent, unauthenticated, or slow. Only an
// authenticated login is returned: repo creation needs gh to be signed in,
// so offering GitHub on a bare `git config github.user` would just fail.
func detectGitHubLogin(ctx context.Context) string {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, gh, "api", "user").Output()
	if err != nil {
		return ""
	}
	var u struct {
		Login string `json:"login"`
	}
	if jerr := json.Unmarshal(out, &u); jerr != nil {
		return ""
	}
	return strings.TrimSpace(u.Login)
}

// createGitHubScopeRepo creates a private GitHub repo named repo under the
// authenticated account via the gh CLI and returns its SSH clone URL.
func createGitHubScopeRepo(ctx context.Context, login, repo string) (string, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "", fmt.Errorf("gh CLI not found: %w", err)
	}
	// --clone=false: only the remote needs to exist; the daemon manages the
	// local per-scope mirror under <dataDir>/sync/<scope>/.
	cmd := exec.CommandContext(ctx, gh, "repo", "create", repo,
		"--private", "--clone=false",
		"--description", "Mazarbul personal memory scope")
	if out, cerr := cmd.CombinedOutput(); cerr != nil {
		return "", fmt.Errorf("gh repo create %s: %w (%s)", repo, cerr, strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("git@github.com:%s/%s.git", login, repo), nil
}

// applyInitSyncRemote wires the personal scope based on the wizard choices.
// GitHub: creates the repo via gh and points the scope at it over SSH. Local
// (or any GitHub failure): falls back to the local mirror. The encrypt flag
// is recorded explicitly either way so the scope's privacy posture is exactly
// what the operator chose (and the daemon's encryption-default migration never
// silently overrides it). The fallback keeps init non-fatal — a flaky network
// or a name collision must not abort an otherwise-complete init.
func applyInitSyncRemote(ctx context.Context, w io.Writer, dataDir string, cfg *config.UserConfig, target, login, repo string, encrypt bool) error {
	if target == syncTargetGitHub && login != "" {
		name := strings.TrimSpace(repo)
		if name == "" {
			name = defaultPersonalRepoName
		}
		url, err := createGitHubScopeRepoFn(ctx, login, name)
		if err != nil {
			//nolint:errcheck // best-effort warning; the local-mirror fallback follows
			_, _ = fmt.Fprintf(w, "warning: GitHub repo setup failed (%v); using a local mirror instead\n", err)
		} else {
			enc := encrypt
			if cfg.Sync.Remotes == nil {
				cfg.Sync.Remotes = make(map[string]config.ScopeRemoteTable, 1)
			}
			cfg.Sync.Remotes[defaultLocalScopeName] = config.ScopeRemoteTable{
				URL:        url,
				AuthMethod: "ssh",
				Encrypted:  &enc,
			}
			return nil
		}
	}
	if err := bootstrapLocalScope(ctx, dataDir, cfg); err != nil {
		return err
	}
	// bootstrapLocalScope defaults to plaintext; honour an explicit encrypt
	// choice by overriding the freshly-wired local-mirror entry.
	if encrypt {
		if r, ok := cfg.Sync.Remotes[defaultLocalScopeName]; ok {
			enc := true
			r.Encrypted = &enc
			cfg.Sync.Remotes[defaultLocalScopeName] = r
		}
	}
	return nil
}

// validateRepoName rejects empty names and characters GitHub disallows in a
// repository slug (letters, digits, '-', '_', '.').
func validateRepoName(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("repository name is required")
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("invalid character %q in repository name", r)
		}
	}
	return nil
}

// syncSummaryLine describes the personal scope's resolved remote + privacy
// posture for the init summary block.
func syncSummaryLine(cfg *config.UserConfig) string {
	r, ok := cfg.Sync.Remotes[defaultLocalScopeName]
	if !ok || r.URL == "" {
		return "not configured"
	}
	target := r.URL
	if strings.HasPrefix(r.URL, "file://") {
		target = "local mirror (this machine)"
	}
	privacy := "encryption off"
	if r.Encrypted != nil && *r.Encrypted {
		privacy = "encrypted"
	}
	return fmt.Sprintf("%s · %s", target, privacy)
}
