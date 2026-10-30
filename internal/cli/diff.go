package cli

// diff.go — `maz diff [revspec]` — structural diff of the entity graph
// across git revisions in a scope repo.
//
// The command reads the rendered/ tree at two revisions in the scope's
// local mirror (entities.jsonl, relations.jsonl) and emits a human-readable
// or JSON diff. No daemon required; the mirror's git history is the source
// of truth.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/config"
)

// writerIsTTY reports whether w is connected to a terminal. Used to suppress
// ANSI colour codes when diff output is piped.
func writerIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// diffFlagTrue is the canonical string value used in the active_filters map
// for boolean diff flags (forgotten, conflicts).
const diffFlagTrue = "true"

// diff action constants — used in both diffEntry.Action and glyphFor switch.
const (
	diffActionAdded      = "added"
	diffActionChanged    = "changed"
	diffActionSuperseded = "superseded"
	diffActionForgotten  = "forgotten"
	diffActionConflict   = "conflict"
	diffActionOrphaned   = "orphaned"
	diffFormatJSON       = "json"
	diffFormatMarkdown   = "markdown"

	diffKindEntity    = "entity"
	diffKindRelation  = "relation"
	diffKindPredicate = "predicate"
	diffKindEpisode   = "episode"

	// DefaultRevspec used when the caller supplies no revision argument.
	diffDefaultRevspec = "HEAD~1..HEAD"
)

// diffEntry is one semantic change in the entity graph.
type diffEntry struct {
	// Action is the action string: see diffAction* constants.
	Action string `json:"action"`
	// Kind is "entity", "relation", or "predicate".
	Kind string `json:"kind"`
	// Target is the display name or UID slug of the affected record.
	Target string `json:"target"`
	// Scope is the scope name.
	Scope string `json:"scope"`
	// Refs holds related UIDs (e.g. successor for superseded).
	Refs []string `json:"refs,omitempty"`
	// Details holds free-text clarification (e.g. "alias added: X").
	Details string `json:"details,omitempty"`
}

// diffFlags holds the parsed flags for `maz diff`.
type diffFlags struct {
	scope     string
	format    string // "text" or "json"
	stat      bool
	ascii     bool
	noColor   bool
	remote    bool
	noFetch   bool
	entity    string
	kind      string
	predicate string
	operation string // post-filter by diffEntry.Action (added, changed, superseded, forgotten, conflict, orphaned)
	forgotten bool
	conflicts bool
}

// diffOutput is the JSON envelope emitted by `maz diff --format json`.
// Entries carries the post-filtered set. Filtered reports how many entries
// were removed by active flags. ActiveFilters lists the flags that narrowed
// the result set (omitempty — absent when no filters were applied).
type diffOutput struct {
	Entries       []diffEntry       `json:"entries"`
	Filtered      int               `json:"filtered"`
	ActiveFilters map[string]string `json:"active_filters,omitempty"`
}

func newDiffCmd() *cobra.Command {
	flags := &diffFlags{}
	cmd := &cobra.Command{
		Use:   "diff [revspec]",
		Short: "Show structural diff of the entity graph, or a document body diff",
		Long: `diff compares two revisions of the rendered entity graph stored in the
scope's git mirror and prints the semantic changes: added/changed entities,
new/superseded/forgotten relations, and unresolved conflicts.

The revspec argument follows git syntax. Defaults to HEAD~1..HEAD.

Examples:
  maz diff                                # last commit vs its parent
  maz diff HEAD~5..HEAD                  # five-commit window
  maz diff pending                       # what will arrive on next sync pull
  maz diff unpushed                      # local commits not yet pushed
  maz diff origin/main..HEAD --remote    # fetch first, then compare
  maz diff --scope personal --stat       # compact summary for one scope
  maz diff --format json | jq '.entries[] | select(.action=="conflict")'
  maz diff --operation added             # only additions
  maz diff --kind person --operation changed --format json  # changed person entities as JSON`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			revspec := diffDefaultRevspec
			if len(args) == 1 {
				revspec = args[0]
			}
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			return runDiff(cmd.Context(), cmd.OutOrStdout(), dataDir, revspec, flags)
		},
	}
	cmd.Flags().StringVar(&flags.scope, "scope", "", "only this scope; empty = every configured scope")
	cmd.Flags().StringVar(&flags.format, "format", "text", "output format: text|json")
	cmd.Flags().BoolVar(&flags.stat, "stat", false, "compact summary: counts only, no per-row detail")
	cmd.Flags().BoolVar(&flags.ascii, "ascii", false, "ASCII glyphs (+/~/>>/ X/!!/o/=) instead of unicode")
	cmd.Flags().BoolVar(&flags.noColor, "no-color", false, "disable ANSI even on TTY")
	cmd.Flags().BoolVar(&flags.remote, "remote", false, "fetch the remote first (alias: --auto-fetch)")
	cmd.Flags().BoolVar(&flags.remote, "auto-fetch", false, "alias for --remote")
	cmd.Flags().BoolVar(&flags.noFetch, "no-fetch", false, "skip fetch even if default changes")
	cmd.Flags().StringVar(&flags.entity, "entity", "", "focus on one entity (uid or slug)")
	cmd.Flags().StringVar(&flags.kind, "kind", "", "filter by entity kind (person, organization, ...)")
	cmd.Flags().StringVar(&flags.predicate, "predicate", "", "filter by relation predicate")
	cmd.Flags().StringVar(&flags.operation, "operation", "", "post-filter by action: added|changed|superseded|forgotten|conflict|orphaned")
	cmd.Flags().BoolVar(&flags.forgotten, "forgotten", false, "show only forgotten relations/entities")
	cmd.Flags().BoolVar(&flags.conflicts, "conflicts", false, "show only unresolved conflicts")
	cmd.AddCommand(newDiffDocumentCmd())
	return cmd
}

// validDiffOperations is the set of valid --operation values.
//
//nolint:gochecknoglobals // read-only lookup table; package-level const-map is idiomatic Go
var validDiffOperations = map[string]struct{}{
	diffActionAdded:      {},
	diffActionChanged:    {},
	diffActionSuperseded: {},
	diffActionForgotten:  {},
	diffActionConflict:   {},
	diffActionOrphaned:   {},
}

// validDiffFormats is the set of valid --format values for `maz diff`.
//
//nolint:gochecknoglobals // read-only lookup table; package-level const-map is idiomatic Go
var validDiffFormats = map[string]struct{}{
	"text":             {},
	diffFormatJSON:     {},
	diffFormatMarkdown: {},
}

// postFilterDiff applies the --predicate and --operation flags as a post-filter
// pass over the already-computed diffEntry list. It returns the filtered entries
// and the count of entries that were removed.
//
// --kind (entity kind: "person", "organization", ...) Is handled exclusively by
// the deep-pass filters in entityMatchesFilter and is NOT re-applied here.
// Re-applying it here would filter out everything because diffEntry.Kind holds
// the structural type ("entity", "relation"), never the entity kind.
//
// --predicate and --operation are pure string matches on diffEntry fields.
func postFilterDiff(entries []diffEntry, flags *diffFlags) (filtered []diffEntry, removedCount int) {
	if flags.operation == "" && flags.predicate == "" {
		return entries, 0
	}
	filtered = make([]diffEntry, 0, len(entries))
	for _, e := range entries {
		if flags.operation != "" && !strings.EqualFold(e.Action, flags.operation) {
			continue
		}
		if flags.predicate != "" && !strings.Contains(strings.ToLower(e.Target), strings.ToLower(flags.predicate)) {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered, len(entries) - len(filtered)
}

// activeFilters builds the active_filters map for the JSON envelope from the
// flags that narrow the result set. Returns nil when no filter is active.
func activeFilters(flags *diffFlags) map[string]string {
	m := make(map[string]string)
	if flags.kind != "" {
		m["kind"] = flags.kind
	}
	if flags.predicate != "" {
		m["predicate"] = flags.predicate
	}
	if flags.operation != "" {
		m["operation"] = flags.operation
	}
	if flags.forgotten {
		m["forgotten"] = diffFlagTrue
	}
	if flags.conflicts {
		m["conflicts"] = diffFlagTrue
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// runDiff is the testable core of `maz diff`.
func runDiff(ctx context.Context, w io.Writer, dataDir, revspec string, flags *diffFlags) error {
	if flags.format != "" {
		if _, ok := validDiffFormats[strings.ToLower(flags.format)]; !ok {
			return fmt.Errorf("diff: --format %q: unknown format (want text|json|markdown)", flags.format)
		}
	}
	if flags.operation != "" {
		if _, ok := validDiffOperations[strings.ToLower(flags.operation)]; !ok {
			return fmt.Errorf("diff: --operation %q: unknown operation (want added|changed|superseded|forgotten|conflict|orphaned)", flags.operation)
		}
	}
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	_, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return fmt.Errorf("diff: load sync config: %w", err)
	}
	scopes := sortedScopeNames(syncCfg)
	if len(scopes) == 0 {
		if _, werr := fmt.Fprintln(w, "(no scopes configured)"); werr != nil {
			return fmt.Errorf("diff: write: %w", werr)
		}
		return nil
	}
	resolvedRevspec := resolveSymbolicRevspec(revspec)
	allEntries, err := runDiffScopes(ctx, w, dataDir, resolvedRevspec, scopes, flags)
	if err != nil {
		return err
	}
	postFiltered, removedCount := postFilterDiff(allEntries, flags)
	if flags.format == diffFormatJSON {
		return writeDiffJSON(w, postFiltered, removedCount, activeFilters(flags))
	}
	return nil
}

// runDiffScopes iterates over scopes and collects diff entries. Extracted from
// runDiff to keep cognitive complexity within the gocognit limit.
func runDiffScopes(ctx context.Context, w io.Writer, dataDir, revspec string, scopes []string, flags *diffFlags) ([]diffEntry, error) {
	var allEntries []diffEntry
	for _, scopeName := range scopes {
		if flags.scope != "" && scopeName != flags.scope {
			continue
		}
		repoDir := filepath.Join(dataDir, "sync", scopeName, "repo")
		if _, serr := os.Stat(filepath.Join(repoDir, ".git")); serr != nil {
			continue // no mirror yet; skip silently
		}
		if flags.remote && !flags.noFetch {
			fetchErrIgnored(ctx, repoDir) // best-effort; stale diff still useful
		}
		entries, derr := diffScope(ctx, repoDir, scopeName, revspec, flags)
		if derr != nil {
			if _, werr := fmt.Fprintf(w, "scope: %s  diff failed: %v\n", scopeName, derr); werr != nil {
				return nil, fmt.Errorf("diff: write: %w", werr)
			}
			continue
		}
		allEntries = append(allEntries, entries...)
		if flags.format != diffFormatJSON {
			// Apply the same post-filter (--operation, --predicate) before
			// rendering the text section so both paths show identical results.
			// --kind is already applied in the deep pass (entityMatchesFilter).
			visibleEntries, _ := postFilterDiff(entries, flags)
			if err := writeDiffSection(w, scopeName, revspec, visibleEntries, flags); err != nil {
				return nil, err
			}
		}
	}
	return allEntries, nil
}

// fetchErrIgnored runs git fetch and silently drops any error. This is
// intentional: a fetch failure leaves the diff working against cached state,
// which is still useful. Named (not inlined) so the caller site is readable
// without a nolint comment.
func fetchErrIgnored(ctx context.Context, repoDir string) {
	_ = runGitFetch(ctx, repoDir) //nolint:errcheck // intentional best-effort
}

// resolveSymbolicRevspec maps convenience aliases to git-native revspecs.
func resolveSymbolicRevspec(revspec string) string {
	switch revspec {
	case "pending":
		return "HEAD..@{u}"
	case "unpushed":
		return "@{u}..HEAD"
	case "last-push":
		return diffDefaultRevspec
	}
	if strings.HasPrefix(revspec, "last-") && strings.HasSuffix(revspec, "-pushes") {
		mid := revspec[len("last-") : len(revspec)-len("-pushes")]
		return "HEAD~" + mid + "..HEAD"
	}
	return revspec
}

// diffScope runs the diff for one scope and returns the entries.
//
// The renderer moves a relation OUT of relations.jsonl on supersede or
// forget, into superseded-relations.jsonl / forgotten-relations.jsonl.
// We therefore load all four JSONL streams at both revisions so the
// diff can surface ⤳ and ⊗ events explicitly rather than reporting
// them as silent drops.
func diffScope(ctx context.Context, repoDir, scopeName, revspec string, flags *diffFlags) ([]diffEntry, error) {
	revA, revB := splitRevspec(revspec)
	entitiesA := readJSONLAtRev(ctx, repoDir, revA, "rendered/entities.jsonl")
	entitiesB := readJSONLAtRev(ctx, repoDir, revB, "rendered/entities.jsonl")
	relationsA := readJSONLAtRev(ctx, repoDir, revA, "rendered/relations.jsonl")
	relationsB := readJSONLAtRev(ctx, repoDir, revB, "rendered/relations.jsonl")
	supersededA := readJSONLAtRev(ctx, repoDir, revA, "rendered/superseded-relations.jsonl")
	supersededB := readJSONLAtRev(ctx, repoDir, revB, "rendered/superseded-relations.jsonl")
	forgottenA := readJSONLAtRev(ctx, repoDir, revA, "rendered/forgotten-relations.jsonl")
	forgottenB := readJSONLAtRev(ctx, repoDir, revB, "rendered/forgotten-relations.jsonl")
	return computeDiff(scopeName, entitiesA, entitiesB,
		relationsA, relationsB,
		supersededA, supersededB,
		forgottenA, forgottenB,
		flags), nil
}

// splitRevspec splits "A..B" or "A...B" into (A, B). A single ref uses its parent as A.
func splitRevspec(revspec string) (string, string) {
	if strings.Contains(revspec, "...") {
		parts := strings.SplitN(revspec, "...", 2)
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	if strings.Contains(revspec, "..") {
		parts := strings.SplitN(revspec, "..", 2)
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	return revspec + "~1", revspec
}

// readJSONLAtRev shells `git show <rev>:<path>` and returns parsed JSON objects.
// Returns nil silently when the file does not exist at that rev.
func readJSONLAtRev(ctx context.Context, repoDir, rev, path string) []map[string]any {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, gitBin, "-C", repoDir, "show", rev+":"+path)
	cmd.Env = cleanGitEnv(repoDir)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil // missing file at rev is normal
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		var obj map[string]any
		if jerr := json.Unmarshal([]byte(line), &obj); jerr == nil {
			rows = append(rows, obj)
		}
	}
	return rows
}

// mapStr extracts a string value from a raw JSON map.
func mapStr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// mapBool extracts a bool value from a raw JSON map.
func mapBool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		return false
	}
	return b
}

// mapStrSlice extracts a []string from a JSON-decoded []any field.
func mapStrSlice(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, ok := x.(string)
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// computeDiff diffs two entity/relation JSONL snapshots and returns sorted entries.
//
// SupersededA/B and forgottenA/B carry the rows that the renderer moved
// out of relations.jsonl on supersede or forget. Diff entries marked
// ⤳ / ⊗ are produced by walking the new-in-B set of those streams,
// not by inferring from holes in relations.jsonl.
func computeDiff(scopeName string,
	entA, entB,
	relA, relB,
	supersededA, supersededB,
	forgottenA, forgottenB []map[string]any,
	flags *diffFlags,
) []diffEntry {
	entries := diffEntities(scopeName, entA, entB, flags)
	entries = append(entries, diffRelations(scopeName, relA, relB, flags)...)
	entries = append(entries, diffSupersededRelations(scopeName, supersededA, supersededB, flags)...)
	entries = append(entries, diffForgottenRelations(scopeName, forgottenA, forgottenB, flags)...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Action == diffActionConflict && entries[j].Action != diffActionConflict {
			return true
		}
		if entries[i].Action != diffActionConflict && entries[j].Action == diffActionConflict {
			return false
		}
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind < entries[j].Kind
		}
		return entries[i].Target < entries[j].Target
	})
	return entries
}

func diffEntities(scopeName string, entA, entB []map[string]any, flags *diffFlags) []diffEntry {
	aByUID := indexByUID(entA)
	bByUID := indexByUID(entB)
	uids := make(map[string]struct{})
	for k := range aByUID {
		uids[k] = struct{}{}
	}
	for k := range bByUID {
		uids[k] = struct{}{}
	}
	var entries []diffEntry
	for _, uid := range sortedMapKeys(uids) {
		a, inA := aByUID[uid]
		b, inB := bByUID[uid]
		if !entityMatchesFilter(uid, a, b, inA, inB, flags) {
			continue
		}
		entries = append(entries, entityDiffEntries(scopeName, a, b, inA, inB, flags)...)
	}
	return entries
}

func entityMatchesFilter(uid string, a, b map[string]any, inA, inB bool, flags *diffFlags) bool {
	if flags.entity != "" {
		target := uid
		if inB {
			target = mapStr(b, "displayName")
		} else if inA {
			target = mapStr(a, "displayName")
		}
		if !strings.Contains(strings.ToLower(target), strings.ToLower(flags.entity)) &&
			!strings.Contains(strings.ToLower(uid), strings.ToLower(flags.entity)) {
			return false
		}
	}
	if flags.kind != "" {
		k := ""
		if inB {
			k = mapStr(b, "kind")
		} else if inA {
			k = mapStr(a, "kind")
		}
		if !strings.EqualFold(k, flags.kind) {
			return false
		}
	}
	return true
}

// entityDiffEntries returns the diff entries for one entity, split from
// entityDiffEntries to keep gocyclo below 15.
func entityDiffEntries(scopeName string, a, b map[string]any, inA, inB bool, flags *diffFlags) []diffEntry {
	switch {
	case !inA && inB:
		return entityAddedEntries(scopeName, b, flags)
	case inA && !inB:
		return entityRemovedEntries(scopeName, a, flags)
	case inA && inB:
		return entityChangedEntries(scopeName, a, b, flags)
	}
	return nil
}

func entityAddedEntries(scopeName string, b map[string]any, flags *diffFlags) []diffEntry {
	if flags.forgotten || flags.conflicts {
		return nil
	}
	return []diffEntry{entityDiffEntry(scopeName, diffActionAdded, b)}
}

func entityRemovedEntries(scopeName string, a map[string]any, flags *diffFlags) []diffEntry {
	if flags.conflicts {
		return nil
	}
	return []diffEntry{entityDiffEntry(scopeName, diffActionOrphaned, a)}
}

func entityChangedEntries(scopeName string, a, b map[string]any, flags *diffFlags) []diffEntry {
	var entries []diffEntry
	if !flags.forgotten && !flags.conflicts {
		if diff := entityChanges(a, b); diff != "" {
			e := entityDiffEntry(scopeName, diffActionChanged, b)
			e.Details = diff
			entries = append(entries, e)
		}
	}
	if !mapBool(a, "orphaned") && mapBool(b, "orphaned") && !flags.conflicts {
		entries = append(entries, entityDiffEntry(scopeName, diffActionOrphaned, b))
	}
	return entries
}

func diffRelations(scopeName string, relA, relB []map[string]any, flags *diffFlags) []diffEntry {
	relAByUID := indexByUID(relA)
	relBByUID := indexByUID(relB)
	relUIDs := make(map[string]struct{})
	for k := range relAByUID {
		relUIDs[k] = struct{}{}
	}
	for k := range relBByUID {
		relUIDs[k] = struct{}{}
	}
	var entries []diffEntry
	for _, uid := range sortedMapKeys(relUIDs) {
		a, inA := relAByUID[uid]
		b, inB := relBByUID[uid]
		pred := mapStr(b, "predicate")
		if pred == "" {
			pred = mapStr(a, "predicate")
		}
		if flags.predicate != "" && !strings.EqualFold(pred, flags.predicate) {
			continue
		}
		entries = append(entries, relationDiffEntries(scopeName, a, b, inA, inB, flags)...)
	}
	return entries
}

func relationDiffEntries(scopeName string, a, b map[string]any, inA, inB bool, flags *diffFlags) []diffEntry {
	var entries []diffEntry
	if !inA && inB && !flags.forgotten && !flags.conflicts {
		entries = append(entries, relationDiffEntry(scopeName, diffActionAdded, b))
		return entries
	}
	if !inA || !inB {
		return entries
	}
	entries = append(entries, relationSupersededEntry(scopeName, a, b, flags)...)
	entries = append(entries, relationForgottenEntry(scopeName, a, b, flags)...)
	entries = append(entries, relationConflictEntry(scopeName, a, b, flags)...)
	return entries
}

func relationSupersededEntry(scopeName string, a, b map[string]any, flags *diffFlags) []diffEntry {
	if mapStr(a, "superseded_by") != "" || mapStr(b, "superseded_by") == "" {
		return nil
	}
	if flags.forgotten || flags.conflicts {
		return nil
	}
	e := relationDiffEntry(scopeName, diffActionSuperseded, b)
	e.Refs = []string{mapStr(b, "superseded_by")}
	return []diffEntry{e}
}

func relationForgottenEntry(scopeName string, a, b map[string]any, flags *diffFlags) []diffEntry {
	if mapBool(a, "forgotten") || !mapBool(b, "forgotten") {
		return nil
	}
	if flags.conflicts {
		return nil
	}
	e := relationDiffEntry(scopeName, diffActionForgotten, b)
	if reason := mapStr(b, "forgottenReason"); reason != "" {
		e.Details = "reason: " + reason
	}
	return []diffEntry{e}
}

// diffSupersededRelations emits a ⤳ entry for every relation that
// appears in revB's superseded-relations.jsonl but not in revA's. The
// row carries the historical edge plus the successor UID under
// "superseded_by"; diff exposes that as a Refs entry so callers can
// follow the chain.
func diffSupersededRelations(scopeName string, supersededA, supersededB []map[string]any, flags *diffFlags) []diffEntry {
	if flags.forgotten || flags.conflicts {
		return nil
	}
	priorUIDs := uidSet(supersededA)
	entries := make([]diffEntry, 0, len(supersededB))
	for _, row := range supersededB {
		uid := mapStr(row, "uid")
		if uid == "" {
			continue
		}
		if _, seenBefore := priorUIDs[uid]; seenBefore {
			continue
		}
		if flags.predicate != "" && !strings.EqualFold(mapStr(row, "predicate"), flags.predicate) {
			continue
		}
		entry := relationDiffEntry(scopeName, diffActionSuperseded, row)
		if succ := mapStr(row, "superseded_by"); succ != "" {
			entry.Refs = []string{succ}
		}
		entries = append(entries, entry)
	}
	return entries
}

// diffForgottenRelations emits a ⊗ entry for every relation that
// appears in revB's forgotten-relations.jsonl but not in revA's, with
// the user-supplied reason carried into Details so the operator sees
// WHY the row went away (e.g. "left in 2024, wrong dates").
func diffForgottenRelations(scopeName string, forgottenA, forgottenB []map[string]any, flags *diffFlags) []diffEntry {
	if flags.conflicts {
		return nil
	}
	priorUIDs := uidSet(forgottenA)
	entries := make([]diffEntry, 0, len(forgottenB))
	for _, row := range forgottenB {
		uid := mapStr(row, "uid")
		if uid == "" {
			continue
		}
		if _, seenBefore := priorUIDs[uid]; seenBefore {
			continue
		}
		if flags.predicate != "" && !strings.EqualFold(mapStr(row, "predicate"), flags.predicate) {
			continue
		}
		entry := relationDiffEntry(scopeName, diffActionForgotten, row)
		if reason := mapStr(row, "reason"); reason != "" {
			entry.Details = "reason: " + reason
		}
		entries = append(entries, entry)
	}
	return entries
}

func uidSet(rows []map[string]any) map[string]struct{} {
	m := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		if uid := mapStr(r, "uid"); uid != "" {
			m[uid] = struct{}{}
		}
	}
	return m
}

func relationConflictEntry(scopeName string, a, b map[string]any, flags *diffFlags) []diffEntry {
	if mapStr(a, "conflictWith") != "" || mapStr(b, "conflictWith") == "" {
		return nil
	}
	if flags.forgotten {
		return nil
	}
	e := relationDiffEntry(scopeName, diffActionConflict, b)
	e.Refs = []string{mapStr(b, "conflictWith")}
	return []diffEntry{e}
}

func entityDiffEntry(scope, action string, row map[string]any) diffEntry {
	uid := mapStr(row, "uid")
	dn := mapStr(row, "displayName")
	kind := mapStr(row, "kind")
	target := kind + "/" + dn
	if len(uid) > 8 {
		target += "__" + uid[:8]
	}
	return diffEntry{Action: action, Kind: diffKindEntity, Target: target, Scope: scope}
}

// relationUIDDisplayLen is the number of leading characters shown for a
// relation endpoint UID in the diff text view.  12 chars is long enough to
// disambiguate ULIDs in practice (collision probability negligible for any
// realistic store size) while staying within an 80-column terminal.
const relationUIDDisplayLen = 12

func relationDiffEntry(scope, action string, row map[string]any) diffEntry {
	pred := mapStr(row, "predicate")
	subj := mapStr(row, "subjectUid")
	if subj == "" {
		subj = mapStr(row, "subject")
	}
	obj := mapStr(row, "objectUid")
	if obj == "" {
		obj = mapStr(row, "object")
	}
	if len(subj) > relationUIDDisplayLen {
		subj = subj[:relationUIDDisplayLen]
	}
	if len(obj) > relationUIDDisplayLen {
		obj = obj[:relationUIDDisplayLen]
	}
	return diffEntry{Action: action, Kind: diffKindRelation, Target: subj + " " + pred + " " + obj, Scope: scope}
}

// entityChanges returns a description of what changed between two entity rows, or "".
// Detects display-name flips, alias additions, identifier additions, and
// resolution-status transitions. Identifier additions are tracked via
// the "identifiers" array of {type, value} objects — anything in B that
// is not in A is reported as "identifier added: <type>=<value>".
func entityChanges(a, b map[string]any) string {
	var parts []string
	if mapStr(a, "displayName") != mapStr(b, "displayName") {
		parts = append(parts, fmt.Sprintf("display_name: %s -> %s",
			mapStr(a, "displayName"), mapStr(b, "displayName")))
	}
	for _, al := range mapStrSlice(b, "aliases") {
		if !containsStr(mapStrSlice(a, "aliases"), al) {
			parts = append(parts, "alias added: "+al)
		}
	}
	for _, id := range identifierStrings(b) {
		if !containsStr(identifierStrings(a), id) {
			parts = append(parts, "identifier added: "+id)
		}
	}
	if mapStr(a, "resolutionStatus") != mapStr(b, "resolutionStatus") {
		parts = append(parts, fmt.Sprintf("status: %s -> %s",
			mapStr(a, "resolutionStatus"), mapStr(b, "resolutionStatus")))
	}
	return strings.Join(parts, "; ")
}

// identifierStrings flattens the "identifiers" array into stable
// "<type>=<value>" tokens so a set difference between two revisions
// reduces to a string-slice comparison.
func identifierStrings(row map[string]any) []string {
	raw, ok := row["identifiers"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		t := mapStr(obj, "type")
		v := mapStr(obj, "value")
		if t == "" && v == "" {
			continue
		}
		out = append(out, t+"="+v)
	}
	return out
}

func containsStr(slice []string, s string) bool {
	for _, x := range slice {
		if x == s {
			return true
		}
	}
	return false
}

func indexByUID(rows []map[string]any) map[string]map[string]any {
	m := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		uid := mapStr(row, "uid")
		if uid != "" {
			m[uid] = row
		}
	}
	return m
}

func sortedMapKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// diffGlyphs holds the unicode and ASCII glyph sets. Passed by pointer to
// satisfy gocritic's hugeParam check (struct is 96 bytes).
type diffGlyphs struct {
	added      string
	changed    string
	superseded string
	forgotten  string
	conflict   string
	orphaned   string
}

// defaultGlyphs returns the diff glyph set, leaning on git conventions:
//   - `+` added, `~` changed, `-` forgotten (git's deletion line marker)
//   - `!` conflict (git uses `UU` for unmerged paths; `!` is the common
//     short form across linters and reviewers)
//   - `⤳` superseded (kept unicode — "replaced by a successor" has no
//     direct git analogue; the glyph reads as "points to next version")
//   - `↺` orphaned (kept unicode — "lost its anchor, still tracked",
//     conceptually distinct from a deletion)
//
// --ascii downgrades the two remaining unicode glyphs to ASCII for
// pipes, log scrapers, or terminals without UTF-8 support.
func defaultGlyphs() diffGlyphs {
	return diffGlyphs{
		added: "+", changed: "~", superseded: "⤳", forgotten: "-",
		conflict: "!", orphaned: "↺",
	}
}

func asciiOnlyGlyphs() diffGlyphs {
	return diffGlyphs{
		added: "+", changed: "~", superseded: ">", forgotten: "-",
		conflict: "!", orphaned: "?",
	}
}

func glyphFor(action string, g *diffGlyphs) string {
	switch action {
	case diffActionAdded:
		return g.added
	case diffActionChanged:
		return g.changed
	case diffActionSuperseded:
		return g.superseded
	case diffActionForgotten:
		return g.forgotten
	case diffActionConflict:
		return g.conflict
	case diffActionOrphaned:
		return g.orphaned
	default:
		return "="
	}
}

func writeDiffSection(w io.Writer, scopeName, revspec string, entries []diffEntry, flags *diffFlags) error {
	g := defaultGlyphs()
	if flags.ascii {
		g = asciiOnlyGlyphs()
	}
	// Suppress ANSI when: --no-color flag set, NO_COLOR env set, or stdout is
	// not a TTY (piped to a script, CI log, etc.).
	noColor := flags.noColor || os.Getenv("NO_COLOR") != "" || !writerIsTTY(w)

	var buf strings.Builder
	fmt.Fprintf(&buf, "\nscope: %s  (%s)\n", scopeName, revspec)

	if flags.stat {
		writeDiffStat(&buf, entries, &g)
	} else {
		writeDiffEntries(&buf, entries, &g, noColor)
	}

	if _, err := fmt.Fprint(w, buf.String()); err != nil {
		return fmt.Errorf("diff: write: %w", err)
	}
	return nil
}

func writeDiffEntries(buf *strings.Builder, entries []diffEntry, g *diffGlyphs, noColor bool) {
	if len(entries) == 0 {
		buf.WriteString("  (no changes)\n")
		return
	}
	// diff renders into buf before the caller writes it to the real stream;
	// os.Stdout is that stream in the common case and gives the right profile.
	c := colorizer.New(!noColor, os.Stdout)
	for _, e := range entries {
		glyph := glyphFor(e.Action, g)
		line := colorizeEntry(c, fmt.Sprintf("  %s %s %s", glyph, e.Kind, e.Target), e.Action)
		buf.WriteString(line + "\n")
		if e.Details != "" {
			buf.WriteString("       " + e.Details + "\n")
		}
		if len(e.Refs) > 0 {
			buf.WriteString("       refs: " + strings.Join(e.Refs, ", ") + "\n")
		}
	}
}

// colorizeEntry applies the semantic colour for a diff action via the colorizer.
// Added is green, changed amber, superseded purple, forgotten red, conflict
// red+bold, and orphaned dim — matching the git-style vocabulary in the
// colorizer package. When c is disabled the line is returned unchanged.
func colorizeEntry(c *colorizer.Colorizer, line, action string) string {
	switch action {
	case diffActionAdded:
		return c.Added(line)
	case diffActionChanged:
		return c.Changed(line)
	case diffActionSuperseded:
		return c.Superseded(line)
	case diffActionForgotten:
		return c.Removed(line)
	case diffActionConflict:
		return c.Conflict(line)
	case diffActionOrphaned:
		return c.Dim(line)
	default:
		return line
	}
}

// diffSummary holds aggregate counts for the --stat view.
type diffSummary struct {
	totalEntities    int
	addedEntities    int
	changedEntities  int
	orphanedEntities int
	totalRelations   int
	addedRelations   int
	superseded       int
	forgotten        int
	conflicts        int
}

func summarizeDiff(entries []diffEntry) diffSummary {
	var s diffSummary
	for _, e := range entries {
		switch e.Kind {
		case diffKindEntity:
			s.totalEntities++
			switch e.Action {
			case diffActionAdded:
				s.addedEntities++
			case diffActionChanged:
				s.changedEntities++
			case diffActionOrphaned:
				s.orphanedEntities++
			}
		case diffKindRelation:
			s.totalRelations++
			switch e.Action {
			case diffActionAdded:
				s.addedRelations++
			case diffActionSuperseded:
				s.superseded++
			case diffActionForgotten:
				s.forgotten++
			case diffActionConflict:
				s.conflicts++
			}
		}
	}
	return s
}

func writeDiffStat(buf *strings.Builder, entries []diffEntry, g *diffGlyphs) {
	s := summarizeDiff(entries)
	fmt.Fprintf(buf, "  entities  | %3d  %s%d  %s%d  %s%d\n",
		s.totalEntities,
		g.added, s.addedEntities,
		g.changed, s.changedEntities,
		g.orphaned, s.orphanedEntities,
	)
	fmt.Fprintf(buf, "  relations | %3d  %s%d  %s%d  %s%d\n",
		s.totalRelations,
		g.added, s.addedRelations,
		g.superseded, s.superseded,
		g.forgotten, s.forgotten,
	)
	if s.conflicts > 0 {
		fmt.Fprintf(buf, "  conflicts |   %d  %s\n", s.conflicts, g.conflict)
	}
}

func writeDiffJSON(w io.Writer, entries []diffEntry, filtered int, filters map[string]string) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if entries == nil {
		entries = []diffEntry{}
	}
	out := diffOutput{
		Entries:       entries,
		Filtered:      filtered,
		ActiveFilters: filters,
	}
	return enc.Encode(out)
}

// runGitFetch runs `git fetch --quiet origin` in repoDir. Best-effort.
func runGitFetch(ctx context.Context, repoDir string) error {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git not in PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, gitBin, "-C", repoDir, "fetch", "--quiet", "origin")
	cmd.Env = cleanGitEnv(repoDir)
	return cmd.Run()
}
