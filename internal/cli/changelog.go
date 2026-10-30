package cli

// changelog.go — `maz changelog [revspec]` — chronological per-push
// CHANGELOG-fragment browser.
//
// Read-only counterpart to `maz log` (which dumps the local event log).
// The two commands answer different questions: `log` shows what
// events were appended; `changelog` shows what pushes shipped (one
// row per CHANGELOG/<YYYY>/<MM>/<ULID>__<device>.md fragment), in
// reverse-chronological order. Together they correspond to git's
// own split between `git log` (commits) and `git reflog` (HEAD
// movements). No daemon required — reads the local mirror's git tree.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

func newChangelogCmd() *cobra.Command {
	var (
		scope string
		limit int
	)
	cmd := &cobra.Command{
		Use:   "changelog [revspec]",
		Short: "Browse per-push CHANGELOG fragments in reverse chronological order",
		Long: `changelog walks the scope mirror's CHANGELOG/ tree and prints one
summary row per push fragment, newest first. Use it to scan recent
pushes without leaving the terminal — the GitHub web UI is the same
information for hosted scopes.

Each fragment lives at CHANGELOG/<YYYY>/<MM>/<headULID>__<device>.md
inside the scope mirror at <dataDir>/sync/<scope>/repo/. The fragment
filename's ULID encodes the push time, so listing newest-first reduces
to a reverse path sort.

Examples:
  maz changelog                             # all configured scopes, last 50
  maz changelog --scope personal            # one scope
  maz changelog --limit 5                   # show the 5 most-recent pushes
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			return runChangelog(cmd.Context(), cmd.OutOrStdout(), dataDir, scope, limit)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "only this scope (project|personal|team); empty = every configured")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum fragments to show per scope")
	return cmd
}

// runChangelog walks CHANGELOG/ inside each scope's local mirror,
// collects fragment paths, sorts them reverse-lex (ULID-encoded
// filenames are chronologically ordered), reads the first body line
// of each, and prints one row per fragment. Limit is per-scope.
func runChangelog(ctx context.Context, w io.Writer, dataDir, scopeFilter string, limit int) error {
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	_, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return fmt.Errorf("changelog: load sync config: %w", err)
	}
	scopes := sortedScopeNames(syncCfg)
	if len(scopes) == 0 {
		// No configured remotes still permits a single-scope walk if
		// the caller passed --scope explicitly and a mirror exists.
		if scopeFilter == "" {
			if _, werr := fmt.Fprintln(w, "(no scopes configured)"); werr != nil {
				return fmt.Errorf("changelog: write: %w", werr)
			}
			return nil
		}
		scopes = []string{scopeFilter}
	}
	var buf bytes.Buffer
	rendered := 0
	for _, scopeName := range scopes {
		if scopeFilter != "" && scopeName != scopeFilter {
			continue
		}
		fragments, ferr := collectChangelogFragments(ctx, dataDir, scopeName, limit)
		if ferr != nil {
			fmt.Fprintf(&buf, "scope: %s  changelog read failed: %v\n", scopeName, ferr)
			continue
		}
		if len(fragments) == 0 {
			continue
		}
		fmt.Fprintf(&buf, "scope: %s  (%d push(es) shown)\n", scopeName, len(fragments))
		for _, frag := range fragments {
			fmt.Fprintf(&buf, "  %s  %s  %s\n", frag.ULID, frag.Device, frag.Summary)
		}
		buf.WriteString("\n")
		rendered++
	}
	if rendered == 0 {
		buf.WriteString("(no CHANGELOG fragments found in any scope mirror)\n")
	}
	if _, werr := io.WriteString(w, buf.String()); werr != nil {
		return fmt.Errorf("changelog: write: %w", werr)
	}
	return nil
}

// changelogFragment is one parsed CHANGELOG row. ULID + device come
// from the filename; Summary is the first non-blank, non-heading line
// of the body (kept under ~80 chars so a long row stays scannable).
type changelogFragment struct {
	ULID    string
	Device  string
	Summary string
	Path    string
}

// collectChangelogFragments walks CHANGELOG/ inside the scope mirror
// at <dataDir>/sync/<scope>/repo, parses each *.md filename into its
// ULID + device, reads the first body line as the summary, and
// returns the newest-first slice capped at limit. A missing mirror
// or missing CHANGELOG/ is not an error (fresh install) — the caller
// just sees zero rows.
func collectChangelogFragments(ctx context.Context, dataDir, scope string, limit int) ([]changelogFragment, error) {
	_ = ctx // reserved for future revspec-bounded reads via `git show`
	root := filepath.Join(dataDir, "sync", scope, "repo", "CHANGELOG")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	var paths []string
	walkErr := filepath.Walk(root, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".md") {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk %s: %w", root, walkErr)
	}
	// Reverse-lex sort puts the newest fragment first. ULID
	// lexicographic order matches chronological order by design.
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	if limit > 0 && len(paths) > limit {
		paths = paths[:limit]
	}
	out := make([]changelogFragment, 0, len(paths))
	for _, p := range paths {
		out = append(out, parseChangelogFragment(p))
	}
	return out, nil
}

// parseChangelogFragment turns a path like
// <repo>/CHANGELOG/2026/06/01HZAA...__device-a.md into a
// changelogFragment with the ULID + device extracted from the
// filename and the summary line plucked from the body.
func parseChangelogFragment(path string) changelogFragment {
	frag := changelogFragment{Path: path}
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, ".md")
	if i := strings.Index(name, "__"); i >= 0 {
		frag.ULID = name[:i]
		frag.Device = name[i+2:]
	} else {
		frag.ULID = name
	}
	body, err := os.ReadFile(path)
	if err != nil {
		frag.Summary = "(read failed: " + err.Error() + ")"
		return frag
	}
	frag.Summary = pickChangelogSummary(string(body))
	return frag
}

// pickChangelogSummary returns the first content line of a CHANGELOG
// fragment. Skips headings, blank lines, and metadata key:value lines
// like `Device: laptop-A` — operators looking at `maz changelog` want
// to see WHAT changed, not WHO wrote it (the changelog row already
// has a Device column).
func pickChangelogSummary(body string) string {
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "---") {
			continue
		}
		// Metadata header (e.g. "Device: laptop-A"). Bullet rows start
		// with "- " so they survive this check.
		if !strings.HasPrefix(s, "-") && !strings.HasPrefix(s, "+") && !strings.HasPrefix(s, "*") {
			if i := strings.Index(s, ":"); i > 0 && i < 24 {
				continue
			}
		}
		if len(s) > 80 {
			s = s[:77] + "..."
		}
		return s
	}
	return ""
}

// exec import retained for future revspec-bounded reads (e.g. `git
// show <rev>:CHANGELOG/...`) — kept here so the import does not
// disappear under goimports.
var _ = exec.Cmd{}
