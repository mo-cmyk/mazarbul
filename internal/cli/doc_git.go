package cli

// doc_git.go — git-tree surface for vault documents: diff, show, blame, and
// github link generation against rendered/documents/<slug>.md.
//
// This is the CROSS-DEVICE history surface: it reads rendered body files from
// the scope's local git mirror, complementing `maz doc log` / `maz doc diff`
// which read the event-log revision chain inside a single device.
//
// Target syntax: `document:<needle>` where <needle> is any of:
//   - frozen slug (e.g. my-notes__GAEN6P)
//   - vault-relative path (e.g. notes/foo.md)
//   - UID prefix (≥ 6 chars)
//   - display-name fragment (matched case-insensitively)
//
// The resolver reads rendered/entities.jsonl at the target revision, collects
// all kind=document rows, and identifies which rendered/documents/*.md file
// belongs to each candidate by matching the last-6-chars suffix of the UID
// against the "__<suffix>" component of the filenames listed by
// `git ls-tree --name-only <rev> rendered/documents/`.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	udiff "github.com/aymanbagabas/go-udiff"
	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// docTargetPrefix is the CLI prefix for document targets.
const docTargetPrefix = "document:"

// entityKindDocument is the canonical entity kind value for vault documents.
const entityKindDocument = "document"

// docResolution is the result of resolving a document:<needle> target.
type docResolution struct {
	// UID is the document entity ULID.
	UID string
	// Slug is the frozen slug (last component of the body file, without .md).
	Slug string
	// BodyPath is the rendered body file path: rendered/documents/<slug>.md.
	BodyPath string
	// DisplayName is the document title from entities.jsonl.
	DisplayName string
}

// resolveDocumentTarget resolves a `document:<needle>` target string into
// a docResolution for the given revision. Returns a clear, actionable error
// on ambiguity or not-found.
//
// Resolution order:
//  1. Filter entities.jsonl to kind=document rows.
//  2. Match the needle against uid-prefix, displayName fragment, vault-path
//     segment, and slug suffix.
//  3. List rendered/documents/ at rev to find the actual file for each
//     matching UID (suffix match on last 6 chars of ULID).
//  4. Exact-one → return; zero → not-found; many → list candidates.
func resolveDocumentTarget(ctx context.Context, repoDir, rev, target string) (docResolution, error) {
	needle := strings.ToLower(strings.TrimPrefix(target, docTargetPrefix))
	if needle == "" {
		return docResolution{}, errors.New("document target: empty needle")
	}

	rows := readJSONLAtRev(ctx, repoDir, rev, "rendered/entities.jsonl")
	docFiles, listErr := listDocumentFiles(ctx, repoDir, rev)
	// listErr is non-fatal: we degrade to suffix-only matching if the tree
	// has no rendered/documents/ entry at this revision.

	candidates := make([]docResolution, 0, len(rows))
	for _, row := range rows {
		if mapStr(row, "kind") != entityKindDocument {
			continue
		}
		uid := mapStr(row, "uid")
		if uid == "" {
			continue
		}
		dn := mapStr(row, "displayName")
		// Resolve the rendered body file (and thus the frozen slug) first so the
		// matcher can compare the needle against the real filename, not just the
		// entity-index fields. The slug encodes path.Base(docPath) at render time,
		// so a "document:notes/foo.md" path target reduces to its basename "foo"
		// and matches the slug base; a mistyped slug whose base does not match the
		// filename is rejected here rather than silently resolving by UID suffix.
		bodyPath, slug := findDocBodyPath(uid, docFiles, listErr)
		if !docNeedleMatches(needle, uid, dn, slug) {
			continue
		}
		if bodyPath == "" {
			// Document exists in the entity index but its body file is absent at
			// this revision (pre-render epoch or encrypted push that pruned rendered/).
			// Surface it with a placeholder so the ambiguity check still works.
			bodyPath = "rendered/documents/<not-rendered-at-" + rev + ">"
			slug = "<not-rendered>"
		}
		candidates = append(candidates, docResolution{
			UID:         uid,
			Slug:        slug,
			BodyPath:    bodyPath,
			DisplayName: dn,
		})
	}

	switch len(candidates) {
	case 0:
		return docResolution{}, fmt.Errorf(
			"document %q not found in rendered/entities.jsonl at %s (kind=document rows checked: %d)",
			target, rev, countDocumentRows(rows))
	case 1:
		if strings.Contains(candidates[0].BodyPath, "<not-rendered") {
			return docResolution{}, fmt.Errorf(
				"document %q (uid %s) exists in the entity index at %s but its body file "+
					"rendered/documents/*.md is absent — the push may predate body rendering "+
					"or rendered/ was pruned by an encrypted-scope push",
				target, candidates[0].UID, rev)
		}
		return candidates[0], nil
	default:
		parts := make([]string, len(candidates))
		for i, c := range candidates {
			parts[i] = fmt.Sprintf("  %s  %q  (%s)", c.UID, c.DisplayName, c.BodyPath)
		}
		return docResolution{}, fmt.Errorf(
			"document target %q is ambiguous (%d matches); be more specific:\n%s",
			target, len(candidates), strings.Join(parts, "\n"))
	}
}

// resolveDocumentInScopes selects the scope mirror that holds the target
// document and resolves it at rev. When scopeFlag is set, only that scope is
// tried. When it is empty, every configured scope is tried in name order and
// the first mirror that resolves the document wins — so a document living in a
// non-first scope is still found without requiring an explicit --scope.
//
// Returns the chosen scope's repoDir plus the docResolution. The error reports
// either "no scopes configured" or that the target was not found in any mirror.
func resolveDocumentInScopes(ctx context.Context, dataDir string, syncCfg config.SyncTable, scopeFlag, rev, target string, fetch bool) (repoDir, scopeName string, res docResolution, err error) {
	scopes := sortedScopeNames(syncCfg)
	if len(scopes) == 0 {
		return "", "", docResolution{}, errNoScopesConfigured
	}

	var lastErr error
	for _, name := range scopes {
		if scopeFlag != "" && name != scopeFlag {
			continue
		}
		dir := filepath.Join(dataDir, "sync", name, "repo")
		if _, serr := os.Stat(filepath.Join(dir, ".git")); serr != nil {
			lastErr = fmt.Errorf("scope %q has no git mirror yet", name)
			continue // no mirror cloned for this scope
		}
		if fetch {
			fetchErrIgnored(ctx, dir)
		}
		r, rerr := resolveDocumentTarget(ctx, dir, rev, target)
		if rerr != nil {
			lastErr = rerr
			continue
		}
		return dir, name, r, nil
	}
	if lastErr == nil {
		// scopeFlag named a scope that is not configured at all.
		lastErr = fmt.Errorf("scope %q is not configured", scopeFlag)
	}
	return "", "", docResolution{}, lastErr
}

// errNoScopesConfigured is returned when no sync scopes are configured; callers
// print a friendly line rather than treating it as a hard failure.
var errNoScopesConfigured = errors.New("no scopes configured")

// docNeedleMatches returns true when the (already lower-cased) needle matches a
// document identified by uid, displayName, and slug. The slug is the frozen
// rendered filename without ".md" (e.g. "alpha-doc__qqgaen"); it is "" when the
// document has no rendered body file at this revision, in which case slug-based
// matchers are skipped.
//
// Matchers, in order:
//   - UID prefix (case-insensitive).
//   - Full frozen slug: needle equals the rendered slug exactly. This validates
//     the slug BASE against the real filename, so "wrong-title__qqgaen" no longer
//     resolves "alpha-doc__qqgaen" purely on the shared UID suffix.
//   - Display-name fragment (case-insensitive substring).
//   - Vault-path target: the needle's basename, run through the SAME slug
//     normalization the renderer uses (strip ".md"/".markdown", lower-case,
//     non-alphanumerics → "-", collapse/trim "-"), equals the slug's base name
//     (the part before "__<suffix>"). So "notes/Project Plan.md" normalizes to
//     "project-plan" and matches "project-plan__<suffix>.md", and ".markdown"
//     paths resolve too. The renderer flattens documents to <base>__<suffix>.md,
//     so the directory is not preserved in the tree and cannot disambiguate;
//     same-basename collisions surface as an ambiguity error listing UIDs.
func docNeedleMatches(needle, uid, displayName, slug string) bool {
	lUID := strings.ToLower(uid)
	lDN := strings.ToLower(displayName)
	lSlug := strings.ToLower(slug)

	// UID prefix match.
	if strings.HasPrefix(lUID, needle) {
		return true
	}
	// Exact frozen-slug match (validates the base, not just the UID suffix).
	if lSlug != "" && needle == lSlug {
		return true
	}
	// Display-name fragment.
	if strings.Contains(lDN, needle) {
		return true
	}
	// Vault-path / basename match against the slug base, normalizing the needle
	// the way the renderer slugifies a document's path basename. Accept
	// "notes/foo.md", "foo.markdown", "Project Plan.md", or a bare "foo".
	needleSlugBase := slugifyDocBase(needle)
	slugBase := strings.ToLower(slugBaseOf(slug))
	if needleSlugBase != "" && slugBase != "" && needleSlugBase == slugBase {
		return true
	}
	return false
}

// slugifyDocBase normalizes a vault-path needle into the slug base the renderer
// would produce for that document. It mirrors internal/sync/render.docSlugBase
// (strip the ".md"/".markdown" extension from the path basename) followed by
// render.slugBase (lower-case, non-alphanumerics → "-", collapse and trim "-").
// Returns "" when the needle has no usable basename. Kept as a private copy so
// the CLI does not import the render package just to read history.
func slugifyDocBase(needle string) string {
	base := filepath.Base(needle)
	base = strings.TrimSuffix(base, ".md")
	base = strings.TrimSuffix(base, ".markdown")
	// A path with no usable basename (renderer would fall back to the document
	// title, which a path target cannot supply) is treated as no-match.
	if b := strings.TrimSpace(base); b == "" || b == "." || b == "/" {
		return ""
	}
	mapped := strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII {
			return '-'
		}
		lr := unicode.ToLower(r)
		if (lr >= 'a' && lr <= 'z') || (lr >= '0' && lr <= '9') {
			return lr
		}
		return '-'
	}, base)
	result := strings.Trim(collapseSlugHyphens(mapped), "-")
	// Mirror render.slugBase: an empty sanitized base becomes "unnamed" (e.g.
	// "🔥🔥.md"), and the base is truncated to slugBaseMaxRunes with any
	// truncation-exposed trailing hyphen trimmed.
	if result == "" {
		return slugUnnamedBase
	}
	return truncateSlugBase(result)
}

// collapseSlugHyphens replaces runs of "-" with a single "-".
func collapseSlugHyphens(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, ch := range s {
		if ch == '-' {
			if !prevHyphen {
				b.WriteRune(ch)
			}
			prevHyphen = true
			continue
		}
		b.WriteRune(ch)
		prevHyphen = false
	}
	return b.String()
}

// truncateSlugBase caps base at slugBaseMaxRunes runes, trims any trailing
// hyphen the cut exposed, and falls back to "unnamed" if nothing remains.
func truncateSlugBase(base string) string {
	runes := []rune(base)
	if len(runes) <= slugBaseMaxRunes {
		return base
	}
	runes = runes[:slugBaseMaxRunes]
	for len(runes) > 0 && runes[len(runes)-1] == '-' {
		runes = runes[:len(runes)-1]
	}
	if len(runes) == 0 {
		return slugUnnamedBase
	}
	return string(runes)
}

// slugBaseMaxRunes and slugUnnamedBase mirror internal/sync/render.slugMaxLen and
// render.slugUnnamed so path-target normalization matches the rendered filename
// for over-long and fully-sanitized-away basenames.
const (
	slugBaseMaxRunes = 60
	slugUnnamedBase  = "unnamed"
)

// slugBaseOf returns the human-readable base of a frozen slug by stripping the
// trailing "__<suffix>" component that slugify() appends. Returns the whole slug
// when no "__" separator is present (synthetic or legacy slugs).
func slugBaseOf(slug string) string {
	if i := strings.LastIndex(slug, "__"); i >= 0 {
		return slug[:i]
	}
	return slug
}

// slugSuffixOf returns the last 6 characters of a ULID (the slug suffix
// produced by internal/sync/render.slugSuffix). Returns "" when uid is too
// short (only happens in synthetic test IDs).
func slugSuffixOf(uid string) string {
	const suffixLen = 6
	if len(uid) < suffixLen {
		return uid
	}
	return uid[len(uid)-suffixLen:]
}

// listDocumentFiles returns the filenames (base names, no directory) in
// rendered/documents/ at the given revision, using `git ls-tree --name-only`.
// Returns nil + error when the path does not exist at rev.
func listDocumentFiles(ctx context.Context, repoDir, rev string) ([]string, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git not in PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, gitBin,
		"-C", repoDir, "ls-tree", "--name-only", rev, "rendered/documents/")
	cmd.Env = cleanGitEnv(repoDir)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err // path not in tree at this rev — caller treats as nil list
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	names := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// git ls-tree returns full path; we want the base name.
		names = append(names, filepath.Base(line))
	}
	return names, nil
}

// findDocBodyPath finds the rendered/documents/ file for a given UID and
// returns (bodyPath, slug). When docFiles is nil (tree has no documents dir)
// or no match exists, returns ("", "").
func findDocBodyPath(uid string, docFiles []string, listErr error) (bodyPath, slug string) {
	if listErr != nil || len(docFiles) == 0 {
		return "", ""
	}
	suffix := "__" + strings.ToLower(slugSuffixOf(uid))
	suffixUpper := "__" + strings.ToUpper(slugSuffixOf(uid))
	for _, name := range docFiles {
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		base := strings.TrimSuffix(name, ".md")
		// Match both lowercase and uppercase ULID suffixes (Crockford Base32 is
		// uppercase but the slug code lowercases via slugBase; the suffix itself
		// comes from uid[len-6:] which is uppercase for real ULIDs). Check both.
		if strings.HasSuffix(strings.ToLower(base), suffix) ||
			strings.HasSuffix(base, suffixUpper) ||
			strings.HasSuffix(strings.ToUpper(base), strings.ToUpper(suffix)) {
			return "rendered/documents/" + name, base
		}
	}
	return "", ""
}

// countDocumentRows returns the number of rows with kind=document in rows.
func countDocumentRows(rows []map[string]any) int {
	n := 0
	for _, r := range rows {
		if mapStr(r, "kind") == entityKindDocument {
			n++
		}
	}
	return n
}

// cleanGitEnv returns a scrubbed environment for git sub-processes. It drops
// the GIT_DIR / GIT_INDEX_FILE / GIT_WORK_TREE variables that a parent git
// hook injects; leaving them in would redirect git to the developer's real
// repository and corrupt its index — the same class of bug fixed in PR #146.
func cleanGitEnv(_ string) []string {
	// Keys whose presence would redirect git away from the -C repoDir argument.
	drop := map[string]struct{}{
		"GIT_DIR":                          {},
		"GIT_WORK_TREE":                    {},
		"GIT_INDEX_FILE":                   {},
		"GIT_OBJECT_DIRECTORY":             {},
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
		"GIT_COMMON_DIR":                   {},
		"GIT_NAMESPACE":                    {},
		"GIT_PREFIX":                       {},
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		key := kv
		if idx := strings.Index(kv, "="); idx >= 0 {
			key = kv[:idx]
		}
		if _, bad := drop[key]; !bad {
			out = append(out, kv)
		}
	}
	out = append(out, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/true")
	return out
}

// ---- maz diff document:<target> ----

// diffDocumentFlags are the additional flags for `maz diff document:`.
type diffDocumentFlags struct {
	scope   string
	format  string
	ascii   bool
	noColor bool
	remote  bool
	noFetch bool
}

func newDiffDocumentCmd() *cobra.Command {
	flags := &diffDocumentFlags{}
	cmd := &cobra.Command{
		Use:   "document <target> [revspec]",
		Short: "Show a body diff of a vault document between two git revisions",
		Long: `document shows the unified body diff of a vault document's rendered file
(rendered/documents/<slug>.md) between two git revisions in the scope mirror.

<target> is a document identifier with the "document:" prefix:
  document:<slug>         — frozen slug (e.g. document:my-notes__GAEN6P)
  document:<path>         — vault path; matched by basename (document:notes/foo.md)
  document:<uid-prefix>   — ULID prefix (≥ 6 chars)
  document:<name-fragment>— display-name fragment (case-insensitive)

The revspec argument follows git syntax and defaults to HEAD~1..HEAD.

Examples:
  maz diff document document:notes/project.md
  maz diff document document:project HEAD~5..HEAD
  maz diff document document:meeting-notes__GAEN6P --scope personal
  maz diff document document:project --format json | jq '.bodyDiff'`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			revspec := diffDefaultRevspec
			if len(args) == 2 {
				revspec = args[1]
			}
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			return runDiffDocument(cmd.Context(), cmd.OutOrStdout(), dataDir, args[0], revspec, flags)
		},
	}
	cmd.Flags().StringVar(&flags.scope, "scope", "", "scope name; empty = first configured scope")
	cmd.Flags().StringVar(&flags.format, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&flags.ascii, "ascii", false, "ASCII glyphs instead of unicode")
	cmd.Flags().BoolVar(&flags.noColor, "no-color", false, "disable ANSI even on TTY")
	cmd.Flags().BoolVar(&flags.remote, "remote", false, "fetch remote before resolving")
	cmd.Flags().BoolVar(&flags.noFetch, "no-fetch", false, "skip automatic fetch")
	return cmd
}

// runDiffDocument is the testable core of `maz diff document`.
func runDiffDocument(ctx context.Context, w io.Writer, dataDir, target, revspec string, flags *diffDocumentFlags) error {
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	_, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return fmt.Errorf("diff document: load sync config: %w", err)
	}

	resolvedRevspec := resolveSymbolicRevspec(revspec)
	revA, revB := splitRevspec(resolvedRevspec)

	// Resolve the document across scopes at revB first; fall back to revA for
	// documents that were tombstoned between the two revisions.
	doFetch := flags.remote && !flags.noFetch
	repoDir, _, res, err := resolveDocumentInScopes(ctx, dataDir, syncCfg, flags.scope, revB, target, doFetch)
	if err != nil {
		if errors.Is(err, errNoScopesConfigured) {
			_, werr := fmt.Fprintln(w, "(no scopes configured)")
			return werr
		}
		repoDir, _, res, err = resolveDocumentInScopes(ctx, dataDir, syncCfg, flags.scope, revA, target, false)
		if err != nil {
			return fmt.Errorf("diff document: %w", err)
		}
	}

	// Validate both endpoints up front so a typo'd revision is reported as an
	// error rather than silently rendered as a full add/delete (or "no changes").
	if err := assertRevExists(ctx, repoDir, revA); err != nil {
		return fmt.Errorf("diff document: %w", err)
	}
	if err := assertRevExists(ctx, repoDir, revB); err != nil {
		return fmt.Errorf("diff document: %w", err)
	}

	bodyA, err := gitShowFileSilent(ctx, repoDir, revA, res.BodyPath)
	if err != nil {
		return fmt.Errorf("diff document: read %s at %s: %w", res.BodyPath, revA, err)
	}
	bodyB, err := gitShowFileSilent(ctx, repoDir, revB, res.BodyPath)
	if err != nil {
		return fmt.Errorf("diff document: read %s at %s: %w", res.BodyPath, revB, err)
	}

	if flags.format == diffFormatJSON {
		return writeDiffDocumentJSON(w, res, revA, revB, bodyA, bodyB)
	}
	return writeDiffDocumentText(w, res, revA, revB, bodyA, bodyB, flags)
}

// assertRevExists returns an error when rev does not resolve to a commit in
// repoDir. Used to fail fast on a mistyped revision before a body read silently
// degrades to an empty diff side.
func assertRevExists(ctx context.Context, repoDir, rev string) error {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git not in PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, gitBin,
		"-C", repoDir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	cmd.Env = cleanGitEnv(repoDir)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("revision %q does not resolve to a commit in the scope mirror", rev)
	}
	return nil
}

// gitShowFileSilent reads path at rev, returning ("", nil) only when the file
// is genuinely absent from that revision's tree (normal for an added/removed
// document on one side of a diff). Any other git failure — a bad revision, an
// unreadable repo, or git pointed at the wrong worktree — propagates as an error
// so it is never silently turned into an empty body. Callers should validate the
// revision separately (see assertRevExists); this guards the remaining cases.
func gitShowFileSilent(ctx context.Context, repoDir, rev, path string) (string, error) {
	content, exists, err := gitShowFileMaybe(ctx, repoDir, rev, path)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	return content, nil
}

// gitShowFileMaybe reads path at rev. It returns exists=false (and no error)
// when git reports the path is missing from the tree at that revision, and a
// real error for any other failure. Missing-path detection matches git's
// stderr phrasings ("does not exist", "exists on disk, but not in").
func gitShowFileMaybe(ctx context.Context, repoDir, rev, path string) (content string, exists bool, err error) {
	gitBin, lerr := exec.LookPath("git")
	if lerr != nil {
		return "", false, fmt.Errorf("git not in PATH: %w", lerr)
	}
	cmd := exec.CommandContext(ctx, gitBin, "-C", repoDir, "show", rev+":"+path)
	cmd.Env = cleanGitEnv(repoDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		msg := stderr.String()
		if strings.Contains(msg, "does not exist") ||
			strings.Contains(msg, "exists on disk, but not in") {
			return "", false, nil // path absent at this rev — expected for add/remove
		}
		return "", false, fmt.Errorf("git show %s:%s: %w: %s", rev, path, runErr, strings.TrimSpace(msg))
	}
	return stdout.String(), true, nil
}

// writeDiffDocumentText renders a unified diff between bodyA and bodyB.
func writeDiffDocumentText(w io.Writer, res docResolution, revA, revB, bodyA, bodyB string, flags *diffDocumentFlags) error {
	labelA := fmt.Sprintf("a/%s @ %s", res.BodyPath, shortRev(revA))
	labelB := fmt.Sprintf("b/%s @ %s", res.BodyPath, shortRev(revB))
	unified := udiff.Unified(labelA, labelB, bodyA, bodyB)
	if strings.TrimSpace(unified) == "" {
		_, err := fmt.Fprintf(w, "no changes in %s between %s and %s\n",
			res.BodyPath, shortRev(revA), shortRev(revB))
		return err
	}
	_, err := fmt.Fprint(w, unified)
	return err
}

// diffDocumentJSONResult is the JSON shape emitted by `maz diff document --format json`.
type diffDocumentJSONResult struct {
	// UID is the document entity ULID.
	UID string `json:"uid"`
	// DisplayName is the document title.
	DisplayName string `json:"displayName"`
	// BodyPath is the rendered body file path in the git tree.
	BodyPath string `json:"bodyPath"`
	// RevA is the older revision ref.
	RevA string `json:"revA"`
	// RevB is the newer revision ref.
	RevB string `json:"revB"`
	// BodyDiff is the unified diff text. Empty string when no changes.
	BodyDiff string `json:"bodyDiff"`
}

func writeDiffDocumentJSON(w io.Writer, res docResolution, revA, revB, bodyA, bodyB string) error {
	labelA := "a/" + res.BodyPath
	labelB := "b/" + res.BodyPath
	diff := udiff.Unified(labelA, labelB, bodyA, bodyB)
	out := diffDocumentJSONResult{
		UID:         res.UID,
		DisplayName: res.DisplayName,
		BodyPath:    res.BodyPath,
		RevA:        revA,
		RevB:        revB,
		BodyDiff:    diff,
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// shortRev truncates a git ref for display (first 8 chars of a SHA, or the
// full symbolic ref such as HEAD~1).
func shortRev(rev string) string {
	// Symbolic refs (HEAD, origin/main, HEAD~N) are short enough already.
	if strings.Contains(rev, "/") || strings.Contains(rev, "~") ||
		strings.Contains(rev, "^") || strings.HasPrefix(rev, "HEAD") ||
		len(rev) <= 8 {
		return rev
	}
	return rev[:8]
}

// ---- maz show document:<target> ----

// showDocumentFlags holds flags for `maz show document`.
type showDocumentFlags struct {
	scope  string
	rev    string
	remote bool
	format string
}

func newShowDocumentCmd() *cobra.Command {
	flags := &showDocumentFlags{format: "text", rev: "HEAD"}
	cmd := &cobra.Command{
		Use:   "document <target>",
		Short: "Print the verbatim body of a vault document at a git revision",
		Long: `document prints the rendered body file (rendered/documents/<slug>.md) for a
vault document at the given git revision in the scope mirror.

<target> is a document identifier with the "document:" prefix:
  document:<slug>         — frozen slug
  document:<path>         — vault path; matched by basename (document:notes/foo.md)
  document:<uid-prefix>   — ULID prefix (≥ 6 chars)
  document:<name-fragment>— display-name fragment (case-insensitive)

Examples:
  maz show document document:notes/project.md
  maz show document document:project --rev HEAD~3
  maz show document document:meeting-notes__GAEN6P --scope personal
  maz show document document:project --format json | jq '.body'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			return runShowDocument(cmd.Context(), cmd.OutOrStdout(), dataDir, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.scope, "scope", "", "scope name; empty = first configured scope")
	cmd.Flags().StringVar(&flags.rev, "rev", "HEAD", "revspec (HEAD, origin/main, HEAD~3, ...)")
	cmd.Flags().BoolVar(&flags.remote, "remote", false, "fetch remote before resolving")
	cmd.Flags().StringVar(&flags.format, "format", "text", "output format: text|markdown|json")
	return cmd
}

// runShowDocument is the testable core of `maz show document`.
func runShowDocument(ctx context.Context, w io.Writer, dataDir, target string, flags *showDocumentFlags) error {
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	_, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return fmt.Errorf("show document: load sync config: %w", err)
	}

	repoDir, _, res, err := resolveDocumentInScopes(ctx, dataDir, syncCfg, flags.scope, flags.rev, target, flags.remote)
	if err != nil {
		if errors.Is(err, errNoScopesConfigured) {
			_, werr := fmt.Fprintln(w, "(no scopes configured)")
			return werr
		}
		return fmt.Errorf("show document: %w", err)
	}

	content, err := gitShowFile(ctx, repoDir, flags.rev, res.BodyPath)
	if err != nil {
		return fmt.Errorf("show document: %q not found at %s: %w", target, flags.rev, err)
	}

	switch flags.format {
	case "markdown":
		return writeShowMarkdown(w, content)
	case diffFormatJSON:
		return writeShowDocumentJSON(w, res, flags.rev, content)
	default:
		return writeShowMarkdown(w, content) // body is already markdown
	}
}

// showDocumentJSONResult is the JSON shape for `maz show document --format json`.
type showDocumentJSONResult struct {
	// UID is the document entity ULID.
	UID string `json:"uid"`
	// DisplayName is the document title.
	DisplayName string `json:"displayName"`
	// BodyPath is the rendered body file path.
	BodyPath string `json:"bodyPath"`
	// Rev is the resolved revision.
	Rev string `json:"rev"`
	// Body is the verbatim body content.
	Body string `json:"body"`
}

func writeShowDocumentJSON(w io.Writer, res docResolution, rev, content string) error {
	out := showDocumentJSONResult{
		UID:         res.UID,
		DisplayName: res.DisplayName,
		BodyPath:    res.BodyPath,
		Rev:         rev,
		Body:        content,
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ---- maz blame document:<target> ----

// blameDocumentFlags holds flags for `maz blame document`.
type blameDocumentFlags struct {
	scope  string
	since  string
	rev    string
	remote bool
	format string
}

func newBlameDocumentCmd() *cobra.Command {
	flags := &blameDocumentFlags{format: "text", rev: "HEAD"}
	cmd := &cobra.Command{
		Use:   "document <target>",
		Short: "Show chronological git history of a vault document body file",
		Long: `document walks the git log of the scope mirror for every commit that touched
the document's rendered body file (rendered/documents/<slug>.md) and lists
changes in chronological order — who changed which section, and when.

<target> is a document identifier with the "document:" prefix.

Examples:
  maz blame document document:notes/project.md
  maz blame document document:project --since 30d
  maz blame document document:meeting-notes__GAEN6P --scope personal
  maz blame document document:project --format json | jq '.[] | .action'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			return runBlameDocument(cmd.Context(), cmd.OutOrStdout(), dataDir, args[0], flags)
		},
	}
	cmd.Flags().StringVar(&flags.scope, "scope", "", "scope name; empty = first configured scope")
	cmd.Flags().StringVar(&flags.since, "since", "", "time limit, e.g. 30d or 2026-01-01")
	cmd.Flags().StringVar(&flags.rev, "rev", "HEAD", "revspec to walk from (HEAD, origin/main, ...)")
	cmd.Flags().BoolVar(&flags.remote, "remote", false, "fetch remote before resolving")
	cmd.Flags().StringVar(&flags.format, "format", "text", "output format: text|json")
	return cmd
}

// runBlameDocument is the testable core of `maz blame document`.
func runBlameDocument(ctx context.Context, w io.Writer, dataDir, target string, flags *blameDocumentFlags) error {
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	_, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return fmt.Errorf("blame document: load sync config: %w", err)
	}

	repoDir, _, res, err := resolveDocumentInScopes(ctx, dataDir, syncCfg, flags.scope, flags.rev, target, flags.remote)
	if err != nil {
		if errors.Is(err, errNoScopesConfigured) {
			_, werr := fmt.Fprintln(w, "(no scopes configured)")
			return werr
		}
		return fmt.Errorf("blame document: %w", err)
	}

	// Reuse the blameEntity path but against the body file, not an entity stub.
	// The body filename is a slug (alpha-doc__QQGAEN.md), so uidFromStubPath
	// cannot recover the document UID — pass res.UID explicitly so structured
	// "Documents:" commit lines map to add/update/forget actions.
	blameFlags := &blameFlags{
		since:  flags.since,
		rev:    flags.rev,
		format: flags.format,
	}
	entries, err := blameEntity(ctx, repoDir, res.BodyPath, res.UID, blameFlags)
	if err != nil {
		return fmt.Errorf("blame document: %w", err)
	}

	if flags.format == diffFormatJSON {
		return writeBlameJSON(w, entries)
	}
	return writeBlameText(w, target, res.BodyPath, entries)
}

// ---- github document link ----

// resolveDocumentGithubURL returns the GitHub blob URL for a document's body
// file at HEAD in the scope repo. Falls back to "" + logged note when not
// found, so the caller can still return the repo root URL.
func resolveDocumentGithubURL(ctx context.Context, baseURL, repoDir, docTarget string) string {
	res, err := resolveDocumentTarget(ctx, repoDir, "HEAD", docTarget)
	if err != nil {
		return ""
	}
	return baseURL + "/blob/main/" + res.BodyPath
}
