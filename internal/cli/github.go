package cli

// github.go — `maz github` — build and open GitHub/Gitea web URLs for
// scope repos, entity stubs, commits, and changelogs.
//
// Degrades gracefully on non-GitHub remotes: prints the URL and a
// "copy this manually" note instead of failing.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

func newGithubCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github",
		Short: "Build and open GitHub web URLs for scope repos and entities",
		Long: `github constructs web URLs for the scope's remote and opens them in the
default browser. Works with GitHub and Gitea remotes; degrades gracefully
on other hosts by printing the URL for manual copy.

Subcommands:
  browse  — open the URL in the default browser
  url     — print the URL without opening`,
	}
	cmd.AddCommand(newGithubBrowseCmd())
	cmd.AddCommand(newGithubURLCmd())
	return cmd
}

// githubFlags holds flags shared by browse and url subcommands.
type githubFlags struct {
	scope     string
	entity    string
	document  string
	commit    string
	changelog bool
}

func newGithubBrowseCmd() *cobra.Command {
	flags := &githubFlags{}
	cmd := &cobra.Command{
		Use:   "browse",
		Short: "Open the scope remote in the default browser",
		Long: `browse constructs the web URL for the scope remote and opens it using
the platform-native open command (macOS: open, Linux: xdg-open, Windows: start).

Examples:
  maz github browse                      # open scope root on github.com
  maz github browse --entity sarah       # open entity stub page
  maz github browse --changelog          # open CHANGELOG directory
  maz github browse --commit abc123      # open specific commit page`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			url, err := resolveGithubURL(cmd.Context(), cmd.OutOrStdout(), dataDir, flags)
			if err != nil {
				return err
			}
			return openBrowser(cmd.Context(), cmd.OutOrStdout(), url)
		},
	}
	addGithubFlags(cmd, flags)
	return cmd
}

func newGithubURLCmd() *cobra.Command {
	flags := &githubFlags{}
	cmd := &cobra.Command{
		Use:   "url",
		Short: "Print the GitHub web URL without opening the browser",
		Long: `url constructs and prints the web URL for the scope remote. Suitable
for scripting, agent calls, or piping into other tools.

Examples:
  maz github url --entity sarah
  maz github url --commit abc123
  maz github url --changelog`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			url, err := resolveGithubURL(cmd.Context(), cmd.OutOrStdout(), dataDir, flags)
			if err != nil {
				return err
			}
			if _, werr := fmt.Fprintln(cmd.OutOrStdout(), url); werr != nil {
				return fmt.Errorf("github url: write: %w", werr)
			}
			return nil
		},
	}
	addGithubFlags(cmd, flags)
	return cmd
}

func addGithubFlags(cmd *cobra.Command, flags *githubFlags) {
	cmd.Flags().StringVar(&flags.scope, "scope", "", "scope name; empty = first configured scope")
	cmd.Flags().StringVar(&flags.entity, "entity", "", "open the entity stub (uid or slug)")
	cmd.Flags().StringVar(&flags.document, "document", "", "open a vault document body file (document:<slug|path|uid|name>)")
	cmd.Flags().StringVar(&flags.commit, "commit", "", "open a specific commit SHA")
	cmd.Flags().BoolVar(&flags.changelog, "changelog", false, "open the CHANGELOG directory")
}

// resolveGithubURL builds the web URL for the scope repo + optional subpath.
func resolveGithubURL(ctx context.Context, w io.Writer, dataDir string, flags *githubFlags) (string, error) {
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	_, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return "", fmt.Errorf("github: load sync config: %w", err)
	}

	scopeName := flags.scope
	if scopeName == "" {
		scopes := sortedScopeNames(syncCfg)
		if len(scopes) == 0 {
			return "", errors.New("github: no scopes configured")
		}
		scopeName = scopes[0]
	}

	remote, ok := syncCfg.Remotes[scopeName]
	if !ok || remote.URL == "" {
		return "", fmt.Errorf("github: scope %q has no remote configured", scopeName)
	}

	baseURL, err := remoteToWebURL(remote.URL)
	if err != nil {
		// Non-GitHub remote: print the raw remote URL with a note.
		msg := "remote is not a GitHub URL — copy this URL manually: " + remote.URL
		if _, werr := fmt.Fprintln(w, msg); werr != nil {
			return "", fmt.Errorf("github: write: %w", werr)
		}
		return remote.URL, nil
	}

	repoDir := filepath.Join(dataDir, "sync", scopeName, "repo")
	return buildGithubSubpath(ctx, baseURL, repoDir, flags), nil
}

// remoteToWebURL converts a git remote URL to its https://github.com/... Form.
// Supports SSH (git@github.com:org/repo.git) and HTTPS forms.
// Returns an error for file:// or other non-GitHub remotes.
func remoteToWebURL(remoteURL string) (string, error) {
	if strings.HasPrefix(remoteURL, "file://") {
		return "", errors.New("file:// remote is not a web URL")
	}
	// SSH form: git@github.com:org/repo.git
	if strings.HasPrefix(remoteURL, "git@") {
		// git@github.com:org/repo.git -> https://github.com/org/repo
		rest := strings.TrimPrefix(remoteURL, "git@")
		colon := strings.Index(rest, ":")
		if colon < 0 {
			return "", fmt.Errorf("unrecognised SSH remote format: %s", remoteURL)
		}
		host := rest[:colon]
		path := strings.TrimSuffix(rest[colon+1:], ".git")
		if !isGithubHost(host) {
			return "", fmt.Errorf("remote host %q is not GitHub/Gitea", host)
		}
		return "https://" + host + "/" + path, nil
	}
	// HTTPS form.
	if strings.HasPrefix(remoteURL, "https://") {
		trimmed := strings.TrimSuffix(remoteURL, ".git")
		host := strings.TrimPrefix(trimmed, "https://")
		slash := strings.Index(host, "/")
		if slash < 0 {
			return "", fmt.Errorf("unrecognised HTTPS remote format: %s", remoteURL)
		}
		if !isGithubHost(host[:slash]) {
			return "", fmt.Errorf("remote host %q is not GitHub/Gitea", host[:slash])
		}
		return trimmed, nil
	}
	return "", fmt.Errorf("unrecognised remote URL format: %s", remoteURL)
}

// isGithubHost returns true for github.com, gitea.io, and common self-hosted
// Gitea/GitHub Enterprise patterns.
func isGithubHost(host string) bool {
	return strings.Contains(host, "github") || strings.Contains(host, "gitea")
}

// buildGithubSubpath appends the appropriate subpath to baseURL based on flags.
// Document targets resolve to rendered/documents/<slug>.md; entity targets resolve
// to the entity stub file. Returns baseURL unchanged when no subpath flag is set.
func buildGithubSubpath(ctx context.Context, baseURL, repoDir string, flags *githubFlags) string {
	if flags.commit != "" {
		return baseURL + "/commit/" + flags.commit
	}
	if flags.changelog {
		return baseURL + "/tree/main/CHANGELOG"
	}
	if flags.document != "" {
		// Ensure the document: prefix is present so the resolver understands it.
		target := flags.document
		if !strings.HasPrefix(target, docTargetPrefix) {
			target = docTargetPrefix + target
		}
		if url := resolveDocumentGithubURL(ctx, baseURL, repoDir, target); url != "" {
			return url
		}
	}
	if flags.entity != "" {
		stubPath := resolveEntityStubPathFromRepo(ctx, repoDir, flags.entity)
		if stubPath != "" {
			return baseURL + "/blob/main/" + stubPath
		}
	}
	return baseURL
}

// resolveEntityStubPathFromRepo looks up an entity's stub path using the
// local repo's rendered/entities.jsonl at HEAD. Returns "" when not found.
func resolveEntityStubPathFromRepo(ctx context.Context, repoDir, entity string) string {
	path, err := resolveEntityStubPath(ctx, repoDir, "HEAD", entity)
	if err != nil {
		return ""
	}
	return path
}

// openBrowser opens url in the platform-native browser.
func openBrowser(ctx context.Context, w io.Writer, url string) error {
	var cmdArgs []string
	switch runtime.GOOS {
	case "darwin":
		cmdArgs = []string{"open", url}
	case "windows":
		cmdArgs = []string{"cmd", "/c", "start", url}
	default:
		cmdArgs = []string{"xdg-open", url}
	}
	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	if err := cmd.Start(); err != nil {
		// Degrade gracefully: print the URL so the user can open it manually.
		if _, werr := fmt.Fprintf(w, "could not open browser: %v\nURL: %s\n", err, url); werr != nil {
			return fmt.Errorf("github: write: %w", werr)
		}
		return nil
	}
	return nil
}
