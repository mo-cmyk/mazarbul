package cli

// sync_remote.go owns `maz sync remote` — the post-init surface for
// managing where a scope syncs. `maz init` wires a remote once (a GitHub
// repo via gh, or a local mirror); this group lets an operator connect an
// existing remote (`set`), create a fresh private GitHub repo and wire it
// (`create`), or inspect the current targets (`show`) at any later time —
// the gap the user hit ("setting a remote should be possible after init,
// via the CLI"). All three are daemon-less config edits: they persist into
// config.toml's [sync.remotes.<scope>] block, the same surface `maz config
// set sync.remotes.<scope>.url` writes and `maz sync status/push/pull` read.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/cli/uiform"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

func newSyncRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Connect, create, or inspect the git remote a scope syncs to",
		Long:  "maz sync remote manages where each scope (personal|project|team) syncs.\n\nA \"remote\" is just a git repository URL — a private GitHub repo, a repo on\nany git host, or a local bare mirror. The daemon seals your memory into\nchunks and pushes them there; another device pulls them back. Encryption is\nper-scope and off by default (so github.com renders your history); opt in\nwith --encrypted (requires a Watchword in the keychain).\n\nSubcommands:\n  show     report the configured remote for each scope\n  set      point a scope at an existing repo URL (GitHub, any host, or a path)\n  create   a fresh private GitHub repo (via the gh CLI) and wire it\n\nExamples:\n  maz sync remote show\n  maz sync remote set git@github.com:me/my-memory.git\n  maz sync remote set https://github.com/me/my-memory.git --auth https_pat\n  maz sync remote set ~/Backups/maz-mirror.git           # local bare mirror\n  maz sync remote create --name my-memory                # needs: gh auth login\n\nAfter wiring a remote, run \"maz sync push\" to upload, or set\n\"maz config set sync.auto_push true\" to let the daemon push on a cadence.",
	}
	cmd.AddCommand(newSyncRemoteShowCmd())
	cmd.AddCommand(newSyncRemoteSetCmd())
	cmd.AddCommand(newSyncRemoteCreateCmd())
	cmd.AddCommand(newSyncRemoteRecreateCmd())
	return cmd
}

// syncRemoteRow is the per-scope record `show` emits.
type syncRemoteRow struct {
	Scope      string `json:"scope"`
	URL        string `json:"url,omitempty"`
	AuthMethod string `json:"authMethod,omitempty"`
	Encrypted  bool   `json:"encrypted"`
	Configured bool   `json:"configured"`
}

// RenderText prints a clean human line so text mode does not fall back to a
// raw Go struct dump (%+v). The output.Writer appends the newline.
func (r syncRemoteRow) RenderText(w io.Writer) error {
	if !r.Configured {
		_, err := fmt.Fprintf(w, "%-9s (no remote configured)", r.Scope)
		return err
	}
	_, err := fmt.Fprintf(w, "%-9s %s  (auth: %s, %s)", r.Scope, r.URL, r.AuthMethod, modeLabel(r.Encrypted))
	return err
}

func newSyncRemoteShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Report the configured remote for each scope",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			_, syncCfg, err := loadResolvedSyncConfig(dataDir)
			if err != nil {
				return err
			}
			w := output.From(cmd)
			names := sortedScopeNames(syncCfg)
			if len(names) == 0 {
				w.Header("no remotes configured — run `maz sync remote set <url>` or `maz sync remote create`")
				return w.Flush()
			}
			for _, name := range names {
				r := syncCfg.Remotes[name]
				if err := w.Emit(syncRemoteRow{
					Scope:      name,
					URL:        r.URL,
					AuthMethod: r.AuthMethod,
					Encrypted:  r.EncryptedOrDefault(),
					Configured: r.URL != "",
				}); err != nil {
					return err
				}
			}
			return w.Flush()
		},
	}
}

func newSyncRemoteSetCmd() *cobra.Command {
	var (
		scope    string
		auth     string
		noVerify bool
	)
	cmd := &cobra.Command{
		Use:   "set <url>",
		Short: "Point a scope at an existing git repo (GitHub, any host, or a local path)",
		Long: `set connects a scope to an existing git repository you already have.

The URL can be an SSH remote (git@github.com:me/repo.git), an HTTPS remote
(https://github.com/me/repo.git), or a local path / file:// URL for a bare
mirror. The repo must already exist — use "maz sync remote create" to make a
fresh GitHub repo instead.

Auth is inferred from the URL (ssh for git@/ssh://, none for file://) unless
you pass --auth. HTTPS remotes that need a token use --auth https_pat; store
the token with the keychain entry the docs describe.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			return runSyncRemoteSet(cmd, dataDir, args[0], scope, auth, noVerify)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", defaultLocalScopeName, "scope to configure (personal|project|team)")
	cmd.Flags().StringVar(&auth, "auth", "", "auth method: ssh|https_pat|none (default: inferred from the URL)")
	cmd.Flags().Bool("encrypted", false, "seal this scope's chunks (XChaCha20-Poly1305; needs a Watchword)")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "skip the `git ls-remote` reachability check")
	return cmd
}

func runSyncRemoteSet(cmd *cobra.Command, dataDir, rawURL, scope, auth string, noVerify bool) error {
	if err := validateSyncScope(scope); err != nil {
		return err
	}
	url := normalizeRemoteURL(rawURL)
	if url == "" {
		return errors.New("sync remote set: a non-empty url is required")
	}
	if auth == "" {
		auth = inferAuthMethod(url)
	} else if err := validateAuthMethod(auth); err != nil {
		return err
	}

	if !noVerify {
		if verr := verifyRemoteReachable(cmd.Context(), url); verr != nil {
			//nolint:errcheck // best-effort advisory; the config is still written below
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: could not reach %s (%v). Saving anyway — create the repo / fix auth, then `maz sync push`. Pass --no-verify to silence.\n",
				url, verr)
		}
	}

	encrypted, encChanged := boolFlag(cmd, "encrypted")
	if err := persistScopeRemote(dataDir, scope, func(r *config.ScopeRemoteTable) {
		r.URL = url
		r.AuthMethod = auth
		if encChanged {
			r.Encrypted = &encrypted
		}
	}); err != nil {
		return err
	}

	w := output.From(cmd)
	if w.Format() == output.FormatJSON || w.Format() == output.FormatNDJSON {
		return w.Emit(map[string]any{"ok": true, "scope": scope, "url": url, "authMethod": auth})
	}
	//nolint:errcheck // best-effort stdout
	fmt.Fprintf(cmd.OutOrStdout(), "connected scope %q to %s (auth: %s)\nnext: `maz sync push` to upload, or `maz config set sync.auto_push true` for background pushes\n", scope, url, auth)
	return nil
}

func newSyncRemoteCreateCmd() *cobra.Command {
	var (
		scope string
		name  string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a fresh private GitHub repo (via the gh CLI) and wire it as the remote",
		Long: `create makes a new PRIVATE GitHub repository under your authenticated
account (using the gh CLI) and points the scope at it over SSH.

Memory is private, so the repo is always created private. To use an existing
or public repo instead, create it yourself and run "maz sync remote set <url>".

Requires the GitHub CLI signed in:  gh auth login`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			return runSyncRemoteCreate(cmd, dataDir, scope, name)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", defaultLocalScopeName, "scope to configure (personal|project|team)")
	cmd.Flags().StringVar(&name, "name", defaultPersonalRepoName, "GitHub repository name to create")
	cmd.Flags().Bool("encrypted", false, "seal this scope's chunks (XChaCha20-Poly1305; needs a Watchword)")
	return cmd
}

func runSyncRemoteCreate(cmd *cobra.Command, dataDir, scope, name string) error {
	if err := validateSyncScope(scope); err != nil {
		return err
	}
	if err := validateRepoName(name); err != nil {
		return err
	}
	login := detectGitHubLoginFn(cmd.Context())
	if login == "" {
		return errors.New("sync remote create: no authenticated GitHub login found. Run `gh auth login` first, or create the repo yourself and run `maz sync remote set <url>`")
	}
	url, err := createGitHubScopeRepoFn(cmd.Context(), login, strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("sync remote create: %w", err)
	}

	encrypted, encChanged := boolFlag(cmd, "encrypted")
	if err := persistScopeRemote(dataDir, scope, func(r *config.ScopeRemoteTable) {
		r.URL = url
		r.AuthMethod = authMethodSSH
		if encChanged {
			r.Encrypted = &encrypted
		}
	}); err != nil {
		return err
	}

	w := output.From(cmd)
	if w.Format() == output.FormatJSON || w.Format() == output.FormatNDJSON {
		return w.Emit(map[string]any{"ok": true, "scope": scope, "url": url, "repo": name, "login": login})
	}
	//nolint:errcheck // best-effort stdout
	fmt.Fprintf(cmd.OutOrStdout(), "created private repo %s/%s and connected scope %q to %s\nnext: `maz sync push` to upload\n", login, name, scope, url)
	return nil
}

func newSyncRemoteRecreateCmd() *cobra.Command {
	var (
		scope    string
		url      string
		auth     string
		noVerify bool
		yes      bool
	)
	cmd := &cobra.Command{
		Use:   "recreate",
		Short: "Rebuild a scope's remote from a clean slate (fresh commit history under your identity)",
		Long: `recreate gives a scope a clean-slate remote: it drops the local git mirror
so the next push re-clones the (empty) remote and re-commits every chunk as
fresh git history — authored under your current git identity, with the
polished README, .gitattributes, and DASHBOARD written on the first push.

This is the supported way to adopt new authorship/structure on an existing
memory repo, or to start a remote over for testing.

What it touches and what it does NOT:
  • removes only the local mirror at <data-dir>/sync/<scope>/repo
  • your event log and sealed chunks (the source of truth) are UNTOUCHED —
    nothing is re-sealed and no ULIDs change; only git commit history is new

For a truly clean slate the remote itself must be empty. Either:
  • point at a fresh repo:   maz sync remote recreate --scope personal --url <new>
  • or empty the current one on the host first (e.g. gh repo delete <repo> &&
    gh repo create <repo> --private), then run recreate to reset the mirror.

Stop the daemon first (maz daemon stop) — it holds the mirror open and would
race a concurrent push. This rewrites a remote, so it requires --yes (or an
interactive confirm).

Examples:
  maz daemon stop
  maz sync remote recreate --scope personal --yes
  maz sync remote recreate --scope personal --url git@github.com:me/fresh.git --yes
  maz sync push                                  # re-commits everything fresh`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			return runSyncRemoteRecreate(cmd, dataDir, scope, url, auth, noVerify, yes)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", defaultLocalScopeName, "scope to recreate (personal|project|team)")
	cmd.Flags().StringVar(&url, "url", "", "repoint the scope at a new remote URL before recreating (optional)")
	cmd.Flags().StringVar(&auth, "auth", "", "auth method for --url: ssh|https_pat|none (default: inferred)")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "skip the `git ls-remote` reachability check on --url")
	cmd.Flags().BoolVar(&yes, "yes", false, "proceed without the interactive confirmation prompt")
	return cmd
}

func runSyncRemoteRecreate(cmd *cobra.Command, dataDir, scope, rawURL, auth string, noVerify, yes bool) error {
	if err := validateSyncScope(scope); err != nil {
		return err
	}
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// Refuse while the daemon owns the mirror: removing it under a live push
	// would race git and leave a half-written tree. This is a hard gate even
	// under --yes — a clean slate is the whole point, and a concurrent push
	// defeats it.
	if st := readDaemonState(cmd.Context(), dataDir); st.Status == daemonStatusRunning {
		return fmt.Errorf("sync remote recreate: the daemon is running (pid %d) and holds the mirror open; run `maz daemon stop` first, then retry", st.PID)
	}

	mirror := filepath.Join(dataDir, "sync", scope, "repo")
	proceed, err := confirmSyncRemoteRecreate(cmd, scope, mirror, yes)
	if err != nil {
		return err
	}
	if !proceed {
		//nolint:errcheck // best-effort stdout
		fmt.Fprintln(cmd.OutOrStdout(), "sync remote recreate cancelled")
		return nil
	}

	// Optionally repoint the remote first, so the recreated mirror clones the
	// new target. Mirrors `set`: infer auth, advisory reachability check.
	if u := normalizeRemoteURL(rawURL); u != "" {
		method := auth
		if method == "" {
			method = inferAuthMethod(u)
		} else if verr := validateAuthMethod(method); verr != nil {
			return verr
		}
		if !noVerify {
			if verr := verifyRemoteReachable(cmd.Context(), u); verr != nil {
				//nolint:errcheck // best-effort advisory; config is still written below
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: could not reach %s (%v). Saving anyway — create the repo / fix auth, then `maz sync push`. Pass --no-verify to silence.\n",
					u, verr)
			}
		}
		if perr := persistScopeRemote(dataDir, scope, func(r *config.ScopeRemoteTable) {
			r.URL = u
			r.AuthMethod = method
		}); perr != nil {
			return perr
		}
	}

	// Drop the local mirror. Its absence makes the next push re-clone the
	// (empty) remote and re-commit every chunk from the source-of-truth store
	// as fresh history. RemoveAll is idempotent: a missing mirror is success.
	if rmErr := os.RemoveAll(mirror); rmErr != nil {
		return fmt.Errorf("sync remote recreate: remove mirror %s: %w", mirror, rmErr)
	}

	w := output.From(cmd)
	if w.Format() == output.FormatJSON || w.Format() == output.FormatNDJSON {
		return w.Emit(map[string]any{"ok": true, "scope": scope, "mirrorRemoved": mirror, "repointed": normalizeRemoteURL(rawURL) != ""})
	}
	//nolint:errcheck // best-effort stdout
	fmt.Fprintf(cmd.OutOrStdout(),
		"recreated scope %q: removed local mirror %s\nensure the remote is empty (fresh --url, or re-create it on the host), then run `maz sync push` to re-commit everything under your identity\n",
		scope, mirror)
	return nil
}

// confirmSyncRemoteRecreate gates the destructive mirror reset. It returns
// (true,nil) to proceed, (false,nil) when the user interactively declines
// (exit 0), and a non-nil error in scripted/non-TTY mode without --yes so the
// caller exits non-zero. Mirrors confirmDocRehome.
func confirmSyncRemoteRecreate(cmd *cobra.Command, scope, mirror string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON
	if jsonMode || !wr.IsTTY() || !interactive.CanPromptInteractively() {
		//nolint:errcheck // best-effort advisory before the error return
		fmt.Fprintf(cmd.ErrOrStderr(),
			"sync remote recreate removes the local mirror %s and rewrites the remote's git history on the next push; pass --yes to proceed\n",
			mirror)
		return false, fmt.Errorf("sync remote recreate: refusing to rewrite scope %q without --yes", scope)
	}
	return uiform.PromptYN(cmd.ErrOrStderr(),
		fmt.Sprintf("Recreate scope %q? This drops the local mirror %s; the next push rewrites the remote's git history (chunks/source of truth are untouched).", scope, mirror),
		false)
}

// persistScopeRemote loads config.toml, applies mutate to the scope's remote
// entry, and writes it back atomically — the same load/mutate/write the
// `maz config set` path uses, so `maz sync status` reflects the change.
func persistScopeRemote(dataDir, scope string, mutate func(*config.ScopeRemoteTable)) error {
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	cfg, err := config.LoadUserConfig(dataDir)
	if err != nil && !errors.Is(err, config.ErrUserConfigNotFound) {
		return fmt.Errorf("sync remote: load config: %w", err)
	}
	if cfg.Sync.Remotes == nil {
		cfg.Sync.Remotes = make(map[string]config.ScopeRemoteTable, 1)
	}
	r := cfg.Sync.Remotes[scope]
	mutate(&r)
	cfg.Sync.Remotes[scope] = r
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = config.CurrentUserConfigSchemaVersion
	}
	if err := config.WriteTOMLFileAtomic(config.UserConfigPath(dataDir), cfg); err != nil {
		return fmt.Errorf("sync remote: write config: %w", err)
	}
	return nil
}

// normalizeRemoteURL turns a bare local absolute path into a file:// URL so a
// caller can point a scope at a local bare mirror without remembering the
// file:// prefix. Everything else (ssh, https, existing file://) passes through
// trimmed.
func normalizeRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			s = filepath.Join(home, s[2:])
		}
	}
	if filepath.IsAbs(s) {
		return "file://" + s
	}
	return s
}

// inferAuthMethod guesses the auth method from the URL shape. Ssh-style
// remotes (git@host:path or ssh://) use the SSH agent/key; file:// + local
// paths need no auth; anything else (https) defaults to "none" so git's own
// credential helper handles it (override with --auth https_pat for a PAT).
func inferAuthMethod(url string) string {
	switch {
	case strings.HasPrefix(url, "git@"), strings.HasPrefix(url, "ssh://"):
		return authMethodSSH
	default:
		// file:// + local paths need no auth; https relies on git's own
		// credential helper (override with --auth https_pat for a PAT).
		return authMethodNone
	}
}

func validateAuthMethod(auth string) error {
	switch auth {
	case authMethodSSH, authMethodHTTPS, authMethodNone:
		return nil
	default:
		return fmt.Errorf("sync remote: invalid --auth %q (want %s|%s|%s)", auth, authMethodSSH, authMethodHTTPS, authMethodNone)
	}
}

func validateSyncScope(scope string) error {
	if !kernel.Scope(scope).Valid() {
		return fmt.Errorf("sync remote: invalid scope %q (want personal|project|team)", scope)
	}
	return nil
}

// boolFlag returns a bool flag's value and whether the user actually set it,
// so a remote's Encrypted pointer is only touched on an explicit --encrypted.
func boolFlag(cmd *cobra.Command, name string) (val, changed bool) {
	v, _ := cmd.Flags().GetBool(name)
	return v, cmd.Flags().Changed(name)
}

// verifyRemoteReachable runs a bounded, non-interactive `git ls-remote` so the
// operator learns immediately if a URL is wrong or auth is missing — without
// ever hanging on an ssh passphrase or https credential prompt.
func verifyRemoteReachable(ctx context.Context, url string) error {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, gitBin, "ls-remote", url)
	c.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oStrictHostKeyChecking=accept-new",
	)
	if out, runErr := c.CombinedOutput(); runErr != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return runErr
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}
