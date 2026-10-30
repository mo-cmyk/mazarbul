package cli

// changed.go — the "what changed since pull" digest.
//
// After a `maz pull` / `maz sync pull`, instead of a bare "pulled N chunks"
// line we render a human digest of the GRAPH changes those chunks brought in:
// how many entities, relations, and documents were added / changed / closed.
// `maz changed` re-prints the most recent digest.
//
// The digest is computed from the rendered/ JSONL tree in each scope's local
// git mirror, diffed between the pre-pull HEAD (captured before the pull's
// fast-forward) and the post-pull HEAD. The orchestration that captures those
// HEADs lives in sync.go (the pull command's post-pull hook); the diffing,
// grouping, rendering, and persistence live here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// gitEmptyTree is git's well-known empty-tree object. Used as the left side of
// a diff for a scope's FIRST pull, where there is no prior HEAD: every record
// in the freshly pulled rendered/ tree then reads as "added".
const gitEmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// pullDigest is the grouped, human-facing summary of the graph changes a
// `maz pull` brought in. The same struct is the stable `--json` shape and the
// cached artefact `maz changed` re-renders.
type pullDigest struct {
	Entities  digestEntityCounts   `json:"entities"`
	Relations digestRelationCounts `json:"relations"`
	Documents digestDocCounts      `json:"documents"`
	// Scopes lists the scopes whose mirror advanced in this pull.
	Scopes []string `json:"scopes,omitempty"`
	// Entries is the flat diff-entry list (the structured diff) for scripts
	// and agents that want per-record detail, not just counts.
	Entries []diffEntry `json:"entries"`
	// AddedRelationUIDs is the set of relation UIDs new in this pull. Cached so
	// `maz changed` can recompute the proposed-review count live against the
	// current projection (which lags the pull until the daemon applies chunks).
	AddedRelationUIDs []string `json:"addedRelationUids,omitempty"`
	// PullTime is when the digest was produced (RFC 3339).
	PullTime string `json:"pullTime,omitempty"`
}

// digestEntityCounts groups entity-level deltas.
type digestEntityCounts struct {
	Added    int `json:"added"`
	Changed  int `json:"changed"`
	Orphaned int `json:"orphaned"`
}

// digestRelationCounts groups relation-level deltas. Proposed is the subset of
// Added relations still awaiting human review (status=proposed).
type digestRelationCounts struct {
	Added      int `json:"added"`
	Superseded int `json:"superseded"`
	Forgotten  int `json:"forgotten"`
	Proposed   int `json:"proposed"`
}

// digestDocCounts groups document-level deltas, computed from the
// rendered/documents/ tree via git name-status.
type digestDocCounts struct {
	Added   int `json:"added"`
	Changed int `json:"changed"`
	Removed int `json:"removed"`
}

// plus returns the element-wise sum of two doc-count sets.
func (d digestDocCounts) plus(o digestDocCounts) digestDocCounts {
	return digestDocCounts{
		Added:   d.Added + o.Added,
		Changed: d.Changed + o.Changed,
		Removed: d.Removed + o.Removed,
	}
}

// hasChanges reports whether the digest carries any structural change. A pull
// that advances the mirror but touches no entity/relation/document (e.g. an
// episode-only chunk) has none, so the digest stays silent.
func (d *pullDigest) hasChanges() bool {
	e, r, doc := d.Entities, d.Relations, d.Documents
	return e.Added+e.Changed+e.Orphaned+
		r.Added+r.Superseded+r.Forgotten+
		doc.Added+doc.Changed+doc.Removed > 0
}

// computePullDigest folds the aggregated diff entries + document counts into the
// grouped digest. Pure: no git, no I/O, so it is the unit-testable core.
func computePullDigest(scopes []string, entries []diffEntry, docs digestDocCounts) pullDigest {
	s := summarizeDiff(entries)
	if entries == nil {
		entries = []diffEntry{}
	}
	return pullDigest{
		Entities:  digestEntityCounts{Added: s.addedEntities, Changed: s.changedEntities, Orphaned: s.orphanedEntities},
		Relations: digestRelationCounts{Added: s.addedRelations, Superseded: s.superseded, Forgotten: s.forgotten},
		Documents: docs,
		Scopes:    scopes,
		Entries:   entries,
	}
}

// plural returns one when n==1, else many. Taking both forms explicitly keeps
// the irregular "entity" → "entities" inflection at the call site and out of a
// branch on a bare noun literal.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// digestSegments renders the ordered, non-empty digest segments. Each segment
// is glyph + count + noun, e.g. "+3 entities", "⤳1 relation superseded".
func digestSegments(d *pullDigest) []string {
	var segs []string
	if n := d.Entities.Added; n > 0 {
		segs = append(segs, fmt.Sprintf("+%d %s", n, plural(n, "entity", "entities")))
	}
	if n := d.Entities.Changed; n > 0 {
		segs = append(segs, fmt.Sprintf("~%d %s", n, plural(n, "entity", "entities")))
	}
	if n := d.Entities.Orphaned; n > 0 {
		segs = append(segs, fmt.Sprintf("↺%d %s orphaned", n, plural(n, "entity", "entities")))
	}
	if n := d.Relations.Added; n > 0 {
		seg := fmt.Sprintf("+%d %s", n, plural(n, "relation", "relations"))
		if p := d.Relations.Proposed; p > 0 {
			seg += fmt.Sprintf(" (%d proposed → run maz review proposed)", p)
		}
		segs = append(segs, seg)
	}
	if n := d.Relations.Superseded; n > 0 {
		segs = append(segs, fmt.Sprintf("⤳%d %s superseded", n, plural(n, "relation", "relations")))
	}
	if n := d.Relations.Forgotten; n > 0 {
		segs = append(segs, fmt.Sprintf("⊗%d %s closed", n, plural(n, "relation", "relations")))
	}
	if n := d.Documents.Added; n > 0 {
		segs = append(segs, fmt.Sprintf("+%d %s", n, plural(n, "document", "documents")))
	}
	if n := d.Documents.Changed; n > 0 {
		segs = append(segs, fmt.Sprintf("~%d %s", n, plural(n, "document", "documents")))
	}
	if n := d.Documents.Removed; n > 0 {
		segs = append(segs, fmt.Sprintf("-%d %s", n, plural(n, "document", "documents")))
	}
	return segs
}

// writePullDigestText writes the one-line human digest. Writes nothing when
// there are no segments, so a no-op pull stays quiet.
func writePullDigestText(w io.Writer, d *pullDigest) error {
	segs := digestSegments(d)
	if len(segs) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "what changed: %s\n", strings.Join(segs, " · ")); err != nil {
		return fmt.Errorf("changed: write: %w", err)
	}
	return nil
}

// scopeRepoDir returns the path to a scope's local git mirror.
func scopeRepoDir(dataDir, scope string) string {
	return filepath.Join(dataDir, "sync", scope, "repo")
}

// captureHEAD returns the current HEAD commit of a scope mirror, or "" when the
// repo does not exist, has no commits, or git is unavailable. Callers capture
// this BEFORE a pull (the pull's fast-forward moves HEAD) and again after.
func captureHEAD(ctx context.Context, repoDir string) string {
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		return ""
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	cmd := exec.CommandContext(ctx, gitBin, "-C", repoDir, "rev-parse", "HEAD")
	cmd.Env = cleanGitEnv(repoDir)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// scopeDelta is one scope's contribution to the digest.
type scopeDelta struct {
	entries  []diffEntry
	docs     digestDocCounts
	addedRel []string
	changed  bool
}

// scopeDigestDelta diffs a single scope's rendered tree between its pre-pull
// HEAD and current HEAD. Returns changed=false when the mirror did not advance
// or the advance touched nothing the digest reports.
func scopeDigestDelta(ctx context.Context, repoDir, scopeName, pre string) scopeDelta {
	post := captureHEAD(ctx, repoDir)
	if post == "" || post == pre {
		return scopeDelta{}
	}
	revA := pre
	if revA == "" {
		revA = gitEmptyTree
	}
	revspec := revA + ".." + post
	entries, err := diffScope(ctx, repoDir, scopeName, revspec, &diffFlags{})
	if err != nil {
		return scopeDelta{}
	}
	docs := diffDocCounts(ctx, repoDir, revspec)
	if len(entries) == 0 && docs == (digestDocCounts{}) {
		return scopeDelta{}
	}
	return scopeDelta{
		entries:  entries,
		docs:     docs,
		addedRel: addedRelationUIDsForScope(ctx, repoDir, revspec),
		changed:  true,
	}
}

// buildPullDigest aggregates the per-scope deltas into one digest. PreHEADs maps
// scope name to its HEAD captured before the pull's fast-forward.
func buildPullDigest(ctx context.Context, dataDir, scopeFilter string, preHEADs map[string]string) pullDigest {
	_, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return computePullDigest(nil, nil, digestDocCounts{})
	}
	scopeNames := sortedScopeNames(syncCfg)
	var (
		entries  []diffEntry
		docs     digestDocCounts
		addedRel []string
	)
	changed := make([]string, 0, len(scopeNames))
	for _, scopeName := range scopeNames {
		if scopeFilter != "" && scopeName != scopeFilter {
			continue
		}
		d := scopeDigestDelta(ctx, scopeRepoDir(dataDir, scopeName), scopeName, preHEADs[scopeName])
		if !d.changed {
			continue
		}
		entries = append(entries, d.entries...)
		docs = docs.plus(d.docs)
		addedRel = append(addedRel, d.addedRel...)
		changed = append(changed, scopeName)
	}
	out := computePullDigest(changed, entries, docs)
	out.AddedRelationUIDs = addedRel
	out.PullTime = time.Now().UTC().Format(time.RFC3339)
	return out
}

// diffDocCounts tallies added / changed / removed rendered documents between two
// revisions via `git diff --name-status` over rendered/documents/. Best-effort:
// any failure yields a zero tally.
func diffDocCounts(ctx context.Context, repoDir, revspec string) digestDocCounts {
	var dc digestDocCounts
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return dc
	}
	cmd := exec.CommandContext(ctx, gitBin, "-C", repoDir, "diff", "--name-status", revspec, "--", "rendered/documents/")
	cmd.Env = cleanGitEnv(repoDir)
	out, err := cmd.Output()
	if err != nil {
		return dc
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'A':
			dc.Added++
		case 'M', 'R', 'C':
			dc.Changed++
		case 'D':
			dc.Removed++
		}
	}
	return dc
}

// addedRelationUIDsForScope returns the relation UIDs present in revB's
// rendered/relations.jsonl but absent from revA's.
func addedRelationUIDsForScope(ctx context.Context, repoDir, revspec string) []string {
	revA, revB := splitRevspec(revspec)
	prior := indexByUID(readJSONLAtRev(ctx, repoDir, revA, "rendered/relations.jsonl"))
	var out []string
	for _, row := range readJSONLAtRev(ctx, repoDir, revB, "rendered/relations.jsonl") {
		uid := mapStr(row, "uid")
		if uid == "" {
			continue
		}
		if _, ok := prior[uid]; !ok {
			out = append(out, uid)
		}
	}
	return out
}

// countProposedAmong returns how many of the given relation UIDs are currently
// status=proposed in the local projection. Best-effort: a missing index, a
// locked DB, or any read error yields 0 (no false nudge). The projection lags a
// fresh pull until the daemon applies the chunks, so this naturally reports 0 at
// pull time and the accurate count once `maz changed` is run after catch-up.
func countProposedAmong(ctx context.Context, cmd *cobra.Command, uids []string) int {
	if len(uids) == 0 {
		return 0
	}
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	// Do not create a fresh index.db (with its migration cost) on the pull path.
	if _, err := os.Stat(filepath.Join(dataDir, "index.db")); err != nil {
		return 0
	}
	want := make(map[string]struct{}, len(uids))
	for _, u := range uids {
		want[u] = struct{}{}
	}
	store, _, err := openListReader(cmd)
	if err != nil {
		return 0
	}
	defer closeStoreQuietly(store)
	// DefaultReadSet: proposed-relation check spans project+personal, the user-facing default.
	nav, err := navigate.New(store.Reader())
	if err != nil {
		return 0
	}
	rels, err := nav.WithScopes(kernel.DefaultReadSet()).
		ListRelationsByStatus(ctx, relationStatusProposed, "", proposedReviewLimit)
	if err != nil {
		return 0
	}
	n := 0
	for i := range rels {
		if _, ok := want[rels[i].UID]; ok {
			n++
		}
	}
	return n
}

// pullDigestPath is where the last pull's digest is cached.
func pullDigestPath(dataDir string) string {
	return filepath.Join(dataDir, "sync", "last-digest.json")
}

// savePullDigest writes the digest cache that `maz changed` re-reads.
func savePullDigest(dataDir string, d *pullDigest) error {
	p := pullDigestPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("changed: mkdir: %w", err)
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("changed: marshal digest: %w", err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return fmt.Errorf("changed: write digest: %w", err)
	}
	return nil
}

// savePullDigestQuietly caches the digest, dropping any write error: the pull
// itself already succeeded, so a cache miss must not fail the command.
func savePullDigestQuietly(dataDir string, d *pullDigest) {
	_ = savePullDigest(dataDir, d) //nolint:errcheck // best-effort cache write
}

// loadPullDigest reads the cached digest. Returns an os.ErrNotExist-wrapped
// error when no pull has produced a digest yet.
func loadPullDigest(dataDir string) (*pullDigest, error) {
	b, err := os.ReadFile(pullDigestPath(dataDir))
	if err != nil {
		return nil, err
	}
	var d pullDigest
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("changed: parse digest cache: %w", err)
	}
	return &d, nil
}

func newChangedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "changed",
		Short: "Re-print the digest of graph changes from the last `maz pull`",
		Long: `changed re-prints the "what changed since pull" digest produced by the most
recent ` + "`maz pull`" + ` (or ` + "`maz sync pull`" + `): how many entities, relations, and
documents arrived, grouped by action.

The digest is cached under <data-dir>/sync/last-digest.json whenever a pull
brings in graph changes; changed reads that cache and re-renders it. The count
of newly pulled relations still awaiting review (status=proposed) is recomputed
live, so once the daemon has applied the pulled chunks the nudge to
` + "`maz review proposed`" + ` reflects the current queue.

Text mode prints the one-line digest; --json emits the structured diff (grouped
counts plus the per-change entries) for scripts and agents.

Exit codes:
  0   re-printed (or nothing recorded yet)
  2   unexpected failure reading the cache

Examples:
  maz changed
  maz changed --json
  maz changed --json | jq '.relations'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runChanged(cmd)
		},
	}
	return cmd
}

// runChanged re-prints the cached pull digest, recomputing the proposed count
// live against the current projection.
func runChanged(cmd *cobra.Command) error {
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON

	d, err := loadPullDigest(dataDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emitNoDigest(cmd, jsonMode)
		}
		return respondFatal(cmd, "changed", err)
	}
	d.Relations.Proposed = countProposedAmong(cmd.Context(), cmd, d.AddedRelationUIDs)

	w := cmd.OutOrStdout()
	if jsonMode {
		if encErr := json.NewEncoder(w).Encode(d); encErr != nil {
			return respondFatal(cmd, "changed", encErr)
		}
		return nil
	}
	if !d.hasChanges() {
		_, werr := fmt.Fprintln(w, "last pull brought no graph changes")
		return werr
	}
	return writePullDigestText(w, d)
}

// emitNoDigest reports the "no pull recorded yet" state in the active mode. JSON
// mode emits an empty-but-parseable digest so a consumer never has to special
// case a missing file.
func emitNoDigest(cmd *cobra.Command, jsonMode bool) error {
	w := cmd.OutOrStdout()
	if jsonMode {
		empty := computePullDigest(nil, nil, digestDocCounts{})
		if err := json.NewEncoder(w).Encode(&empty); err != nil {
			return respondFatal(cmd, "changed", err)
		}
		return nil
	}
	_, err := fmt.Fprintln(w, "no pull digest recorded yet; run `maz pull` first")
	return err
}
