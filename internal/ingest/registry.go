package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// githubTokenSecretKey is the secret-chain key for the optional GitHub token
// that lifts REST rate limits and unlocks private pull requests. Its absence is
// not an error: public repositories ingest PR prose unauthenticated.
const githubTokenSecretKey = "github.token"

// SecretReader is the minimal secret-chain reader the registry needs to fetch
// an optional GitHub token. Secret.Provider satisfies it; tests pass a fake.
type SecretReader interface {
	Get(ctx context.Context, name string) (string, error)
}

// Config carries the per-run inputs a connector factory needs. GitHubBaseURL /
// GranolaBaseURL override the respective REST roots for tests (empty => public
// API). RepoPath doubles as the local locator for file-based connectors (the
// claude-chats export path).
type Config struct {
	RepoPath       string
	Secrets        SecretReader
	GitHubBaseURL  string
	GranolaBaseURL string
}

// Connectors returns the registered connector names. Add a connector here and
// in NewSource's switch.
func Connectors() []string {
	return []string{connectorLocalGit, connectorClaudeChats, connectorGranola}
}

// NewSource resolves a connector name to a constructed Source. An unknown name
// is an error naming the known connectors.
func NewSource(ctx context.Context, name string, cfg Config) (Source, error) { //nolint:ireturn // factory dispatches by connector name; the Source interface IS the abstraction
	switch name {
	case connectorLocalGit:
		return buildLocalGitSource(ctx, cfg)
	case connectorClaudeChats:
		return buildClaudeChatsSource(cfg)
	case connectorGranola:
		return buildGranolaSource(ctx, cfg)
	default:
		return nil, fmt.Errorf("ingest: unknown connector %q (known: %v)", name, Connectors())
	}
}

// buildGranolaSource constructs a GranolaSource. The Granola API key is REQUIRED
// (unlike the optional GitHub token): a missing key is an actionable error
// pointing at `maz source add granola`, not a silent unauthenticated client.
func buildGranolaSource(ctx context.Context, cfg Config) (*GranolaSource, error) {
	if cfg.Secrets == nil {
		return nil, fmt.Errorf("ingest: connector %q requires a secret provider for the API key", connectorGranola)
	}
	key, err := cfg.Secrets.Get(ctx, secret.NameGranolaAPIKey)
	if err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			return nil, fmt.Errorf("ingest: no %s in the keychain — run `maz source add granola` first", secret.NameGranolaAPIKey)
		}
		return nil, fmt.Errorf("ingest: read granola api key: %w", err)
	}
	return NewGranolaSource(newGranolaClient(cfg.GranolaBaseURL, key, nil), nil), nil
}

// buildClaudeChatsSource constructs a ClaudeChatsSource from the export path
// carried in cfg.RepoPath (reused as the local locator: a .zip bundle or a bare
// conversations.json). No secret is required — this is offline file import.
func buildClaudeChatsSource(cfg Config) (*ClaudeChatsSource, error) {
	if cfg.RepoPath == "" {
		return nil, fmt.Errorf("ingest: connector %q requires a path to the export (.zip or conversations.json)", connectorClaudeChats)
	}
	return NewClaudeChatsSource(cfg.RepoPath, nil), nil
}

// buildLocalGitSource constructs a LocalGitSource. The pull-requests stream is
// always wired: public PRs ingest unauthenticated and a non-GitHub origin soft-
// skips, so no token is required for commits-only repos. An optional
// github.token secret enriches the GitHub client.
func buildLocalGitSource(ctx context.Context, cfg Config) (*LocalGitSource, error) {
	if cfg.RepoPath == "" {
		return nil, fmt.Errorf("ingest: connector %q requires a repo path", connectorLocalGit)
	}
	token, err := lookupGitHubToken(ctx, cfg.Secrets)
	if err != nil {
		return nil, err
	}
	gh := newGitHubClient(cfg.GitHubBaseURL, token, nil)
	return NewLocalGitSource(cfg.RepoPath, gh, nil), nil
}

// lookupGitHubToken reads the optional GitHub token. A nil provider or a missing
// secret yields an empty token and no error (unauthenticated access is fine); a
// real provider error propagates.
func lookupGitHubToken(ctx context.Context, secrets SecretReader) (string, error) {
	if secrets == nil {
		return "", nil
	}
	token, err := secrets.Get(ctx, githubTokenSecretKey)
	switch {
	case err == nil:
		return token, nil
	case errors.Is(err, secret.ErrNotFound):
		return "", nil
	default:
		return "", fmt.Errorf("ingest: read github token: %w", err)
	}
}
