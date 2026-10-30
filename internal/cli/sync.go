package cli

// sync.go — `maz sync` parent + status/push/pull/inspect/diff/log subcommands.
//
// The manual-flush surface promised in agent-docs/sync.md, for operators
// who don't want to wait for the supervisor's coalesce tick. `push` seals
// and pushes immediately; `status` reports per-scope drift; `pull`
// downloads remote chunks (the daemon applies them on its next tick);
// `inspect`, `diff`, and `log` are read-only views over the synced repo.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/colorizer"
	"github.com/mo-cmyk/mazarbul/internal/cli/interactive"
	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	clistatus "github.com/mo-cmyk/mazarbul/internal/cli/status"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/identity"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/recipients"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
	gittransport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Inspect + manually trigger Moria sync between paired devices",
		Long: `maz sync is the operator-facing surface for the sync (Moria) layer.

The daemon's supervisor handles sealing + pushing on a coalesce cadence;
this command lets you inspect the current state and trigger a push without
waiting for the next tick (useful before closing the laptop, when finishing
a working session, or when debugging).

Subcommands:
  status  — per-scope report (mode, local chunk count, remote chunk count)
  push    — immediate push of any local chunks the remote does not have
  inspect — privacy audit: list plaintext exposure + recent commits

All commands operate on the user config + state.toml the daemon would
read. They do not require the daemon to be running.`,
	}
	cmd.AddCommand(newSyncStatusCmd())
	cmd.AddCommand(newSyncPushCmd())
	cmd.AddCommand(newSyncInspectCmd())
	cmd.AddCommand(newSyncPullCmd())
	cmd.AddCommand(newSyncDiffCmd())
	cmd.AddCommand(newSyncLogCmd())
	cmd.AddCommand(newSyncRemoteCmd())
	cmd.AddCommand(newSyncRepairCmd())
	return cmd
}

func newSyncPullCmd() *cobra.Command {
	var scope string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Fetch chunks the local mirror is missing (dispatcher-less; daemon picks them up on next start)",
		Long: `pull contacts each configured remote, compares its chunk list against
the local FSStore, and downloads any chunks the local store does not
have. Does NOT boot the dispatcher — the daemon's next start picks up
the new chunks via its normal replay path.

After a successful pull it prints a "what changed" digest of the graph changes
the pulled chunks brought in (added/changed entities, new/superseded/closed
relations, documents), and nudges 'maz review proposed' when newly pulled
relations are awaiting review. Re-print the last digest any time with
'maz changed'. With --json (or off a TTY) the per-scope outcomes and the
structured digest are emitted as one JSON object.

Pass --dry-run to list what would be pulled (chunkIDs, byte counts,
originating devices from MANIFEST) without writing anything to local.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkSyncScope(cmd, "sync pull", scope); err != nil {
				return err
			}
			return runPullWithDigest(cmd, scope, dryRun)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "only this scope; empty = every configured remote")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be pulled without writing to local")
	return cmd
}

func newSyncInspectCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Privacy audit: per-scope plaintext exposure + recent commits + scope-rotation advisory",
		Long: `inspect walks every chunk in the local mirror and reports what is
plaintext (and therefore readable by anyone with read access to the remote)
versus what is encrypted. It also lists the five most recent plaintext
commits — useful for "is something sensitive in there that I want to rotate
away from?" — and prints the scope-rotation procedure as a trailing
advisory.

Non-destructive. Does not modify any files, does not contact the remote.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			return runSyncInspect(cmd.Context(), cmd.OutOrStdout(), dataDir, scope)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "only this scope; empty = every configured scope")
	return cmd
}

func newSyncStatusCmd() *cobra.Command {
	var (
		scope string
		fetch bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report per-scope sync posture (mode, chunk counts, drift)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			w := cmd.OutOrStdout()
			if err := checkSyncScope(cmd, "sync status", scope); err != nil {
				return err
			}
			enabled := interactive.ShouldStyle(cmd, w)
			format := output.From(cmd).Format()
			return runSyncStatus(cmd.Context(), w, dataDir, scope, fetch, enabled, format)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "only this scope (project|personal|team); empty = all configured")
	cmd.Flags().BoolVar(&fetch, "fetch", false, "run `git fetch` for each scope's mirror before reporting drift")
	return cmd
}

func newSyncPushCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Push every local chunk the remote is missing (no wait for coalesce tick)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			if err := checkSyncScope(cmd, "sync push", scope); err != nil {
				return err
			}
			return runSyncPush(cmd.Context(), cmd.OutOrStdout(), dataDir, scope)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "only this scope; empty = every configured remote")
	return cmd
}

// newPushCmd promotes `maz sync push` to the top level as `maz push` — git
// muscle memory: users (and agents) reach for `maz push`/`maz pull` first. It
// is a thin alias over the same runSyncPush logic, identical flags. Without it
// `maz push` returned a bare "unknown command".
func newPushCmd() *cobra.Command {
	var scope string
	cmd := &cobra.Command{
		Use:     "push",
		Short:   "Push local memory to its configured remote (alias for `maz sync push`)",
		Aliases: []string{"sync-push"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			if err := checkSyncScope(cmd, "push", scope); err != nil {
				return err
			}
			return runSyncPush(cmd.Context(), cmd.OutOrStdout(), dataDir, scope)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "only this scope; empty = every configured remote")
	return cmd
}

// newPullCmd promotes `maz sync pull` to the top level as `maz pull`, the
// counterpart to newPushCmd. Thin alias over runSyncPull with identical flags.
func newPullCmd() *cobra.Command {
	var (
		scope  string
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:     "pull",
		Short:   "Fetch memory chunks from the configured remote (alias for `maz sync pull`)",
		Aliases: []string{"sync-pull"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkSyncScope(cmd, "pull", scope); err != nil {
				return err
			}
			return runPullWithDigest(cmd, scope, dryRun)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "only this scope; empty = every configured remote")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be pulled without writing to local")
	return cmd
}

// syncScopeReport is the per-scope row rendered by `maz sync status`.
// Ahead / Behind / LastFetch are derived from the local git mirror,
// not the FSStore: the mirror is the source of truth for "what's
// pushable" and "what's pulled but not replayed", and reading it lets
// us answer the drift question without a network round-trip when the
// caller did not request --fetch.
type syncScopeReport struct {
	Scope      string `json:"scope"`
	Mode       string `json:"mode"` // "plaintext" or "encrypted"
	RemoteURL  string `json:"remoteUrl,omitempty"`
	LocalCount int    `json:"localCount"`
	// RemoteCount is the count of chunks on the remote. No omitempty: a real
	// zero ("remote has no chunks") must be distinguishable from "field absent".
	RemoteCount int    `json:"remoteCount"`
	RemoteErr   string `json:"remoteError,omitempty"`
	Ahead       int    `json:"ahead"`
	Behind      int    `json:"behind"`
	// MirrorBehind is how many commits origin/main is ahead of the local mirror
	// main by GIT ANCESTRY, distinct from the chunk-set Behind. A losing
	// push-contention race can leave the mirror behind while the FSStore still
	// holds every chunk (so Ahead/Behind read 0) — this field is the honest
	// split-brain signal that the chunk-set counts miss.
	MirrorBehind int `json:"mirrorBehind"`
	// LastFetch is a pointer so "never fetched" serialises as an absent field
	// (or null) rather than the zero time "0001-01-01T00:00:00Z". omitempty
	// works on a pointer (unlike on a time.Time value, where it is a no-op).
	LastFetch *time.Time `json:"lastFetch,omitempty"`
	DriftErr  string     `json:"driftError,omitempty"`
}

func runSyncStatus(ctx context.Context, w io.Writer, dataDir, scopeFilter string, fetch bool, enabled bool, format output.Format) error {
	_ = fetch // List() always fetches; --fetch is retained as a no-op for parity with git's mental model.
	if !scopeFilterValid(scopeFilter) {
		return fmt.Errorf("sync status: --scope %q: unknown scope (want project|personal|team)", scopeFilter)
	}
	dataDir, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := chunk.NewFSStore(dataDir)
	if err != nil {
		return fmt.Errorf("open chunk store: %w", err)
	}
	rows := make([]syncScopeReport, 0, len(syncCfg.Remotes))
	for _, scopeName := range sortedScopeNames(syncCfg) {
		if scopeFilter != "" && scopeName != scopeFilter {
			continue
		}
		row, rerr := buildSyncScopeReport(ctx, dataDir, scopeName, store, syncCfg.Remotes[scopeName])
		if rerr != nil {
			return rerr
		}
		rows = append(rows, row)
	}
	return renderSyncStatus(w, rows, scopeFilter, enabled, format)
}

// buildSyncScopeReport assembles the status row for a single sync scope: the
// local chunk count from the FSStore, the remote chunk count plus ahead/behind
// drift from the remote MANIFEST (when a remote is configured), the git-ancestry
// mirror-behind honest-health signal, and the last-fetch timestamp.
func buildSyncScopeReport(ctx context.Context, dataDir, scopeName string, store *chunk.FSStore, remote config.ScopeRemoteTable) (syncScopeReport, error) {
	row := syncScopeReport{
		Scope:     scopeName,
		Mode:      modeLabel(remote.EncryptedOrDefault()),
		RemoteURL: remote.URL,
	}
	local, lerr := store.List(ctx, kernel.Scope(scopeName))
	if lerr != nil {
		return syncScopeReport{}, fmt.Errorf("list local %s: %w", scopeName, lerr)
	}
	row.LocalCount = len(local)
	var remoteIDs map[string]struct{}
	if remote.URL != "" {
		ids, rerr := remoteChunkIDs(ctx, dataDir, scopeName, remote.URL)
		if rerr != nil {
			row.RemoteErr = rerr.Error()
		} else {
			remoteIDs = ids
			row.RemoteCount = len(ids)
		}
	}
	if remoteIDs != nil {
		row.Ahead, row.Behind = chunkSetDrift(local, remoteIDs)
	}
	if remote.URL != "" {
		// Honest-health signal (F7c): the chunk-set drift above reads the
		// FSStore, which stays "up to date" even when a lost push-contention
		// race leaves the git mirror behind origin/main. Read the mirror's
		// git ancestry (cached refs, no fetch) so a split-brain surfaces
		// instead of silently reporting ahead=0 behind=0.
		row.MirrorBehind = mirrorBehindByAncestry(ctx, dataDir, scopeName)
	}
	if t := readLastFetch(dataDir, scopeName); !t.IsZero() {
		row.LastFetch = &t
	}
	return row, nil
}

// chunkSetDrift compares the local FSStore against the remote MANIFEST
// and returns the asymmetric chunk-id differences.
//
//	ahead  = chunks present locally but not in remote MANIFEST
//	          → these are sitting in your FSStore waiting to be pushed.
//	behind = chunks present in remote MANIFEST but not locally
//	          → these are pulled-but-not-yet-replayed (or never pulled).
//
// Chunk IDs are ULIDs assigned by the sealer; the same chunk on two
// devices has the same ID, so set-difference is the right semantic
// (unlike git rev-list, which always reads zero because the mirror
// auto-fast-forwards on every List() call).
func chunkSetDrift(local []chunk.Ref, remote map[string]struct{}) (int, int) {
	localIDs := make(map[string]struct{}, len(local))
	for i := range local {
		localIDs[local[i].ChunkID] = struct{}{}
	}
	ahead := 0
	for id := range localIDs {
		if _, ok := remote[id]; !ok {
			ahead++
		}
	}
	behind := 0
	for id := range remote {
		if _, ok := localIDs[id]; !ok {
			behind++
		}
	}
	return ahead, behind
}

// readLastFetch returns the mtime of .git/FETCH_HEAD inside the scope
// mirror. Zero time means the mirror has never fetched (first-run or
// no remote configured).
func readLastFetch(dataDir, scope string) time.Time {
	info, err := os.Stat(filepath.Join(dataDir, "sync", scope, "repo", ".git", "FETCH_HEAD"))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// formatDriftCount renders the ahead/behind cell. Zero stays "0"; any
// non-zero prints with the sign prefix so a quick scan tells you which
// side the drift is on without consulting the column header.
func formatDriftCount(n int, sign string) string {
	if n == 0 {
		return "0"
	}
	return sign + strconv.Itoa(n)
}

// formatLastFetch renders the .git/FETCH_HEAD mtime in a glance-friendly
// way. The zero time (no fetch ever) renders as "never"; recent fetches
// use coarse human units so the column stays narrow.
func formatLastFetch(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// syncInspectReport is the per-scope row rendered by `maz sync inspect`.
// JSON-tagged so future --format json can serialise it without an
// adapter.
type syncInspectReport struct {
	Scope          string   `json:"scope"`
	RemoteURL      string   `json:"remoteUrl,omitempty"`
	TotalChunks    int      `json:"totalChunks"`
	PlaintextCount int      `json:"plaintextCount"`
	EncryptedCount int      `json:"encryptedCount"`
	PlaintextFirst string   `json:"plaintextFirstUlid,omitempty"`
	PlaintextLast  string   `json:"plaintextLastUlid,omitempty"`
	PlaintextBytes int64    `json:"plaintextBytes"`
	RecentCommits  []string `json:"recentCommits,omitempty"`

	// KeyMode is the configured key-distribution mode for the scope
	// ("envelope" for a team scope, "watchword"/"" otherwise). Populated
	// best-effort for the team scope only; absent config leaves it empty.
	KeyMode string `json:"keyMode,omitempty"`
	// RecipientCount is the number of admitted-member rows staged in the
	// scope's local recipients.jsonl. Zero when no team is provisioned.
	RecipientCount int `json:"recipientCount,omitempty"`
	// DEKEpoch is the highest data-encryption-key epoch across the staged
	// recipient rows. Zero when no recipients are staged.
	DEKEpoch int `json:"dekEpoch,omitempty"`
}

func runSyncInspect(ctx context.Context, w io.Writer, dataDir, scopeFilter string) error {
	dataDir, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return err
	}
	scopes := sortedScopeNames(syncCfg)
	var buf strings.Builder
	any := false
	for _, scopeName := range scopes {
		if scopeFilter != "" && scopeName != scopeFilter {
			continue
		}
		any = true
		report, ierr := inspectScope(ctx, dataDir, scopeName, syncCfg.Remotes[scopeName].URL)
		if ierr != nil {
			fmt.Fprintf(&buf, "scope: %s  inspect failed: %v\n\n", scopeName, ierr)
			continue
		}
		writeInspectReport(&buf, &report)
	}
	if !any && scopeFilter != "" {
		fmt.Fprintf(&buf, "scope %q is not configured\n", scopeFilter)
	}
	if !any && scopeFilter == "" {
		buf.WriteString("(no scopes configured)\n")
	}
	if any {
		writeInspectAdvisory(&buf)
	}
	if _, err := io.WriteString(w, buf.String()); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// inspectScope walks the local chunks dir for a single scope, peeks
// every chunk header, accumulates plaintext / encrypted counts, then
// shells `git log` against the local mirror for the recent plaintext
// commit history. All sub-steps are best-effort — a missing mirror
// or a non-git remote degrades to "no recent commits available"
// rather than failing the whole inspect.
func inspectScope(ctx context.Context, dataDir, scopeName, remoteURL string) (syncInspectReport, error) {
	report := syncInspectReport{Scope: scopeName, RemoteURL: remoteURL}
	chunksDir := chunk.DirFor(dataDir, kernel.Scope(scopeName))
	entries, err := os.ReadDir(chunksDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return report, fmt.Errorf("read chunks dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !isChunkFilename(e.Name()) {
			continue
		}
		if err := accumulateChunk(&report, chunksDir, e); err != nil {
			return report, err
		}
	}
	report.RecentCommits = recentPlaintextCommits(ctx, dataDir, scopeName)
	populateTeamPosture(&report, dataDir, scopeName)
	return report, nil
}

// populateTeamPosture fills the team-only posture fields (KeyMode,
// RecipientCount, DEKEpoch) on report. It is a no-op for non-team scopes
// and strictly best-effort: a missing config, missing recipients file, or
// read error leaves the fields at their zero values rather than failing
// the whole inspect. Only the team scope distributes a per-epoch DEK via
// recipients.jsonl, so populating these for project/personal would be
// noise.
func populateTeamPosture(report *syncInspectReport, dataDir, scopeName string) {
	if scopeName != string(kernel.ScopeTeam) {
		return
	}
	if cfg, err := config.LoadUserConfig(dataDir); err == nil {
		report.KeyMode = cfg.Sync.Remotes[scopeName].KeyMode
	}
	rows, err := recipients.Read(filepath.Join(dataDir, "sync", scopeName, recipients.Filename))
	if err != nil {
		return
	}
	report.RecipientCount = len(rows)
	for i := range rows {
		if rows[i].DEKEpoch > report.DEKEpoch {
			report.DEKEpoch = rows[i].DEKEpoch
		}
	}
}

// accumulateChunk reads one chunk file, peeks its header, and merges
// the result into report. Split out so inspectScope stays under the
// gocognit cap.
func accumulateChunk(report *syncInspectReport, dir string, entry os.DirEntry) error {
	path := filepath.Join(dir, entry.Name())
	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("stat chunk %s: %w", path, err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read chunk %s: %w", path, err)
	}
	hdr, err := chunk.PeekHeader(blob)
	if err != nil {
		// Consistent with FSStore.List: a stale pre-0x02 chunk (ErrBadVersion)
		// or a stray non-chunk file (ErrBadMagic) is skipped, not fatal — it
		// carries no data the privacy audit needs and must not abort inspect.
		// Real corruption of a valid chunk still surfaces.
		if errors.Is(err, chunk.ErrBadVersion) || errors.Is(err, chunk.ErrBadMagic) {
			return nil
		}
		return fmt.Errorf("peek chunk %s: %w", path, err)
	}
	report.TotalChunks++
	if hdr.Encrypted {
		report.EncryptedCount++
		return nil
	}
	report.PlaintextCount++
	report.PlaintextBytes += info.Size()
	if report.PlaintextFirst == "" || hdr.FirstULID < report.PlaintextFirst {
		report.PlaintextFirst = hdr.FirstULID
	}
	if hdr.LastULID > report.PlaintextLast {
		report.PlaintextLast = hdr.LastULID
	}
	return nil
}

// isChunkFilename reports whether name has a known chunk extension.
// Local to sync.go (the transport package has its own private copy);
// keeping a sibling here avoids leaking an internal helper.
func isChunkFilename(name string) bool {
	return strings.HasSuffix(name, chunk.Extension) || strings.HasSuffix(name, chunk.ExtensionJSONL)
}

// recentPlaintextCommits returns up to 5 commit summary lines from the
// scope's local mirror that touched a `.jsonl` chunk path. Returns nil
// if the mirror does not exist yet, the git binary is missing, or any
// step fails — `inspect` is a best-effort summary, not an audit log.
func recentPlaintextCommits(ctx context.Context, dataDir, scopeName string) []string {
	repoDir := filepath.Join(dataDir, "sync", scopeName, "repo")
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		return nil
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, gitBin, "-C", repoDir, "log", "-n", "5",
		"--pretty=format:%h %ci %s", "--", "chunks/*.jsonl")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func writeInspectReport(buf *strings.Builder, r *syncInspectReport) {
	fmt.Fprintf(buf, "scope: %s", r.Scope)
	if r.RemoteURL != "" {
		fmt.Fprintf(buf, "  remote: %s", r.RemoteURL)
	}
	buf.WriteString("\n")
	fmt.Fprintf(buf, "  total chunks: %d  (%d plaintext, %d encrypted)\n",
		r.TotalChunks, r.PlaintextCount, r.EncryptedCount)
	if r.PlaintextCount > 0 {
		fmt.Fprintf(buf, "  plaintext range: %s → %s\n", r.PlaintextFirst, r.PlaintextLast)
		fmt.Fprintf(buf, "  plaintext disk:  %s\n", humanBytes(r.PlaintextBytes))
	}
	if r.KeyMode != "" {
		fmt.Fprintf(buf, "  key mode:        %s\n", r.KeyMode)
	}
	if r.RecipientCount > 0 {
		fmt.Fprintf(buf, "  recipients:      %d (DEK epoch %d)\n", r.RecipientCount, r.DEKEpoch)
	}
	if len(r.RecentCommits) > 0 {
		fmt.Fprintf(buf, "  recent plaintext commits (last %d):\n", len(r.RecentCommits))
		for _, line := range r.RecentCommits {
			fmt.Fprintf(buf, "    %s\n", line)
		}
	}
	buf.WriteString("\n")
}

func writeInspectAdvisory(buf *strings.Builder) {
	buf.WriteString("⚠ Plaintext chunks are visible to anyone with read access to the remote.\n")
	buf.WriteString("   To stop adding new plaintext: flip `encrypted = true` under [sync.<scope>] in state.toml.\n")
	buf.WriteString("   Force-push does NOT un-publish historical data — third-party hosts cache, clones persist.\n")
	buf.WriteString("   To get privacy going forward: rotate to a fresh remote URL.\n")
}

// humanBytes renders a byte count in the smallest unit ≥ 1. Kept
// inline rather than pulling in a units package — the output is for
// human eyes, not arithmetic.
func humanBytes(n int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// scopeFilterValid reports whether scopeFilter is empty (no filter) or names a
// valid kernel scope. A non-empty unknown scope is the only invalid case. The
// single source of truth for the three scope-validation sites.
func scopeFilterValid(scopeFilter string) bool {
	return scopeFilter == "" || kernel.Scope(scopeFilter).Valid()
}

// validateScopeFilter returns an error when scopeFilter is non-empty and does
// not name a valid kernel scope (project|personal|team). Used by the inner
// runSync* functions (incl. Direct-call tests) so a typo like --scope
// bogusscope errors instead of silently succeeding with no output.
func validateScopeFilter(cmd, scopeFilter string) error {
	if !scopeFilterValid(scopeFilter) {
		return fmt.Errorf("%s: --scope %q: unknown scope (want project|personal|team)", cmd, scopeFilter)
	}
	return nil
}

// checkSyncScope validates --scope at the RunE boundary, emitting the shared
// {error,code} envelope (E_VALIDATION, exit 1) on an unknown scope so a JSON/
// NDJSON consumer gets a parseable error on stdout — matching the one error
// contract used by every other command. The inner runSync* functions keep
// their own checks as defense-in-depth (and for direct-call tests); on the CLI
// path this RunE-level check fires first.
func checkSyncScope(cmd *cobra.Command, label, scopeFilter string) error {
	if scopeFilterValid(scopeFilter) {
		return nil
	}
	msg := fmt.Sprintf("--scope %q: unknown scope (want project|personal|team)", scopeFilter)
	return respondError(cmd, label, mcp.NewValidationError(msg, nil))
}

// scopePullOutcome is the structured result of one scope's pull pass. It is the
// JSON-serialisable form of the per-scope text line and feeds both the text
// renderer (writePullText) and the `--json` pull result.
type scopePullOutcome struct {
	Scope    string         `json:"scope"`
	NoRemote bool           `json:"noRemote,omitempty"`
	Error    string         `json:"error,omitempty"`
	Fetched  int            `json:"fetched"`
	Already  int            `json:"already"`
	Devices  map[string]int `json:"devices,omitempty"`
}

// runSyncPull contacts each remote, lists chunks, downloads any the
// local FSStore is missing, and prints the per-scope text summary. No
// dispatcher boot — daemon next-start replays into the four-layer storage
// stack. With dryRun=true, lists what would be pulled without writing.
//
// This is the text-mode core. The pull commands route through
// runPullWithDigest, which captures the pre-pull HEADs, calls this for the
// chunk summary, then appends the "what changed" graph digest.
func runSyncPull(ctx context.Context, w io.Writer, dataDir, scopeFilter string, dryRun bool) error {
	_, outcomes, err := collectPull(ctx, dataDir, scopeFilter, dryRun)
	if err != nil {
		return err
	}
	return writePullText(w, outcomes, scopeFilter, dryRun)
}

// collectPull does the chunk-pull work for each configured scope and returns the
// resolved data dir plus a structured per-scope outcome list. Rendering (text or
// JSON) is the caller's job, so the same work backs both output modes.
func collectPull(ctx context.Context, dataDir, scopeFilter string, dryRun bool) (string, []scopePullOutcome, error) {
	if err := validateScopeFilter("sync pull", scopeFilter); err != nil {
		return "", nil, err
	}
	dataDir, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return "", nil, err
	}
	store, err := chunk.NewFSStore(dataDir)
	if err != nil {
		return "", nil, fmt.Errorf("open chunk store: %w", err)
	}
	scopeNames := sortedScopeNames(syncCfg)
	outcomes := make([]scopePullOutcome, 0, len(scopeNames))
	for _, scopeName := range scopeNames {
		if scopeFilter != "" && scopeName != scopeFilter {
			continue
		}
		oc := scopePullOutcome{Scope: scopeName}
		remote := syncCfg.Remotes[scopeName]
		switch remote.URL {
		case "":
			oc.NoRemote = true
		default:
			summary, perr := pullScope(ctx, dataDir, scopeName, remote.URL, store, dryRun)
			if perr != nil {
				oc.Error = perr.Error()
			} else {
				oc.Fetched, oc.Already, oc.Devices = summary.fetched, summary.already, summary.devices
			}
		}
		outcomes = append(outcomes, oc)
	}
	return dataDir, outcomes, nil
}

// writePullText renders the per-scope outcomes as the human chunk summary,
// preserving the exact line format the command shipped with.
func writePullText(w io.Writer, outcomes []scopePullOutcome, scopeFilter string, dryRun bool) error {
	var buf strings.Builder
	pulledAny := false
	for i := range outcomes {
		oc := &outcomes[i]
		switch {
		case oc.NoRemote:
			fmt.Fprintf(&buf, "%-10s no remote configured (skip)\n", oc.Scope)
		case oc.Error != "":
			fmt.Fprintf(&buf, "%-10s pull failed: %s\n", oc.Scope, oc.Error)
		default:
			writePullSummary(&buf, oc.Scope, pullSummary{fetched: oc.Fetched, already: oc.Already, devices: oc.Devices}, dryRun)
			if oc.Fetched > 0 {
				pulledAny = true
			}
		}
	}
	if !pulledAny && scopeFilter == "" {
		if dryRun {
			buf.WriteString("(nothing to pull)\n")
		} else {
			buf.WriteString("(everything up to date)\n")
		}
	}
	if _, err := io.WriteString(w, buf.String()); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// pullJSONResult is the `--json` shape of a pull: the per-scope outcomes plus
// the graph digest. Digest is nil on a dry-run.
type pullJSONResult struct {
	Scopes []scopePullOutcome `json:"scopes"`
	Digest *pullDigest        `json:"digest,omitempty"`
	DryRun bool               `json:"dryRun,omitempty"`
}

// runPullWithDigest is the pull command's post-pull hook. It captures each
// scope mirror's HEAD before the pull's fast-forward, runs the pull, then
// renders the "what changed" graph digest. In text mode it appends the digest
// under the chunk summary; in JSON mode it emits one combined object.
func runPullWithDigest(cmd *cobra.Command, scopeFilter string, dryRun bool) error {
	ctx := cmd.Context()
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	wr := output.From(cmd)
	jsonMode := wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON

	// Capture pre-pull HEADs BEFORE the pull fast-forwards each mirror. The
	// reflog (HEAD@{1}) is not a stable anchor, so we record the SHAs ourselves.
	preHEADs := capturePrePullHEADs(ctx, dataDir, scopeFilter, dryRun)

	if jsonMode {
		return runPullJSON(ctx, cmd, dataDir, scopeFilter, dryRun, preHEADs)
	}

	w := cmd.OutOrStdout()
	if err := runSyncPull(ctx, w, dataDir, scopeFilter, dryRun); err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	digest := buildPullDigest(ctx, dataDir, scopeFilter, preHEADs)
	digest.Relations.Proposed = countProposedAmong(ctx, cmd, digest.AddedRelationUIDs)
	if !digest.hasChanges() {
		return nil
	}
	savePullDigestQuietly(dataDir, &digest)
	if _, err := fmt.Fprintln(w); err != nil {
		return fmt.Errorf("pull: write: %w", err)
	}
	return writePullDigestText(w, &digest)
}

// capturePrePullHEADs records every in-scope mirror's HEAD before a pull. Dry
// runs do not produce a digest, so they skip the capture.
func capturePrePullHEADs(ctx context.Context, dataDir, scopeFilter string, dryRun bool) map[string]string {
	if dryRun {
		return nil
	}
	_, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return nil
	}
	pre := map[string]string{}
	for _, scopeName := range sortedScopeNames(syncCfg) {
		if scopeFilter != "" && scopeName != scopeFilter {
			continue
		}
		pre[scopeName] = captureHEAD(ctx, scopeRepoDir(dataDir, scopeName))
	}
	return pre
}

// runPullJSON pulls and emits one combined JSON object: the per-scope outcomes
// and (for a real pull) the graph digest.
func runPullJSON(ctx context.Context, cmd *cobra.Command, dataDir, scopeFilter string, dryRun bool, preHEADs map[string]string) error {
	resolvedDir, outcomes, err := collectPull(ctx, dataDir, scopeFilter, dryRun)
	if err != nil {
		return respondFatal(cmd, "pull", err)
	}
	res := pullJSONResult{Scopes: outcomes, DryRun: dryRun}
	if !dryRun {
		digest := buildPullDigest(ctx, resolvedDir, scopeFilter, preHEADs)
		digest.Relations.Proposed = countProposedAmong(ctx, cmd, digest.AddedRelationUIDs)
		if digest.hasChanges() {
			savePullDigestQuietly(resolvedDir, &digest)
		}
		res.Digest = &digest
	}
	if err := json.NewEncoder(cmd.OutOrStdout()).Encode(res); err != nil {
		return respondFatal(cmd, "pull", err)
	}
	return nil
}

// pullSummary records the result of one scope's pull pass.
type pullSummary struct {
	fetched int
	already int
	devices map[string]int // device → chunk count, for dry-run reporting
}

// pullScope lists the remote, computes the missing-locally set, and
// (unless dryRun) downloads each missing chunk via the transport.
func pullScope(ctx context.Context, dataDir, scopeName, remoteURL string, store *chunk.FSStore, dryRun bool) (pullSummary, error) {
	scope := kernel.Scope(scopeName)
	tr, err := gittransport.New(gittransport.Options{
		DataDir:    dataDir,
		RemoteURLs: map[kernel.Scope]string{scope: remoteURL},
		DeviceID:   "maz-sync-cli",
	})
	if err != nil {
		return pullSummary{}, fmt.Errorf("transport: %w", err)
	}
	remoteListing, err := tr.List(ctx, scope)
	if err != nil {
		return pullSummary{}, fmt.Errorf("list remote: %w", err)
	}
	localRefs, err := store.List(ctx, scope)
	if err != nil {
		return pullSummary{}, fmt.Errorf("list local: %w", err)
	}
	have := make(map[string]struct{}, len(localRefs))
	for i := range localRefs {
		have[localRefs[i].ChunkID] = struct{}{}
	}
	summary := pullSummary{devices: make(map[string]int)}
	for i := range remoteListing.Chunks {
		hdr := &remoteListing.Chunks[i]
		if _, dup := have[hdr.ChunkID]; dup {
			summary.already++
			continue
		}
		summary.devices[hdr.Device]++
		summary.fetched++
		if dryRun {
			continue
		}
		if err := fetchAndStoreChunk(ctx, tr, store, scope, hdr); err != nil {
			return summary, err
		}
	}
	return summary, nil
}

// fetchAndStoreChunk pulls one chunk from the transport and writes it
// to the local FSStore. Body close errors are surfaced after the Put
// because a close failure does not invalidate the chunk we just wrote.
func fetchAndStoreChunk(ctx context.Context, tr transport.Transport, store *chunk.FSStore, scope kernel.Scope, hdr *chunk.Header) error {
	blob, err := tr.Get(ctx, scope, hdr.ChunkID)
	if err != nil {
		return fmt.Errorf("get chunk %s: %w", hdr.ChunkID, err)
	}
	body, readErr := io.ReadAll(blob.Body)
	closeErr := blob.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read chunk %s: %w", hdr.ChunkID, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close chunk %s body: %w", hdr.ChunkID, closeErr)
	}
	if err := store.Put(ctx, scope, *hdr, body); err != nil {
		return fmt.Errorf("put chunk %s: %w", hdr.ChunkID, err)
	}
	return nil
}

func writePullSummary(buf *strings.Builder, scopeName string, s pullSummary, dryRun bool) {
	verb := "pulled"
	if dryRun {
		verb = "would pull"
	}
	switch s.fetched {
	case 0:
		fmt.Fprintf(buf, "%-10s already up to date (%d chunks present)\n", scopeName, s.already)
	default:
		fmt.Fprintf(buf, "%-10s %s %d chunk(s)", scopeName, verb, s.fetched)
		if len(s.devices) > 0 {
			fmt.Fprintf(buf, " — from %s", formatDeviceBreakdown(s.devices))
		}
		buf.WriteString("\n")
	}
}

// formatDeviceBreakdown returns a stable "deviceA=3, deviceB=1"
// rendering. Sorted by device name so output is deterministic.
func formatDeviceBreakdown(d map[string]int) string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		label := k
		if label == "" {
			label = "(unknown)"
		}
		parts = append(parts, fmt.Sprintf("%s=%d", label, d[k]))
	}
	return strings.Join(parts, ", ")
}

func runSyncPush(ctx context.Context, w io.Writer, dataDir, scopeFilter string) error {
	if err := validateScopeFilter("sync push", scopeFilter); err != nil {
		return err
	}
	dataDir, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return err
	}
	store, err := chunk.NewFSStore(dataDir)
	if err != nil {
		return fmt.Errorf("open chunk store: %w", err)
	}
	ident, closeIdent := resolvePushIdentity(ctx, dataDir, syncCfg)
	defer closeIdent()
	var buf strings.Builder
	// Seal once before listing local chunks so a `maz remember` issued
	// moments before this command lands in a chunk on this push, not
	// the next daemon coalesce tick. Failure is informational — when
	// the daemon owns the event-log lock, the supervisor's own sealer
	// already covers this beat; we surface the hint and proceed.
	if hint := sealBeforePush(ctx, dataDir, syncCfg, scopeFilter, store); hint != "" {
		fmt.Fprint(&buf, hint)
	}
	scopes := sortedScopeNames(syncCfg)
	pushedAny := false
	for _, scopeName := range scopes {
		if scopeFilter != "" && scopeName != scopeFilter {
			continue
		}
		remote := syncCfg.Remotes[scopeName]
		if remote.URL == "" {
			fmt.Fprintf(&buf, "%-10s no remote configured (skip)\n", scopeName)
			continue
		}
		accepted, err := pushScope(ctx, dataDir, scopeName, remote.URL, store, ident)
		if err != nil {
			fmt.Fprintf(&buf, "%-10s push failed: %v\n", scopeName, err)
			continue
		}
		if accepted == 0 {
			fmt.Fprintf(&buf, "%-10s already up to date\n", scopeName)
		} else {
			fmt.Fprintf(&buf, "%-10s pushed %d new chunk(s)\n", scopeName, accepted)
			pushedAny = true
		}
	}
	if !pushedAny && scopeFilter == "" {
		buf.WriteString("(nothing to push)\n")
	}
	if _, err := io.WriteString(w, buf.String()); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// sealBeforePush runs one synchronous SealOnceWith per scope so a
// `maz remember` issued moments earlier lands in a chunk before this
// push lists the store. Best-effort: when the daemon owns the
// event-log lock (the common case on a running install) the open
// fails fast and we return a short hint instead of blocking the
// push — the supervisor's own sealer covers that beat. Returns "" on
// the no-op path so callers can unconditionally prepend to the
// output buffer.
func sealBeforePush(ctx context.Context, dataDir string, syncCfg config.SyncTable, scopeFilter string, store *chunk.FSStore) string {
	// Interprocess lock first: if the daemon's supervisor is mid-seal
	// we must skip — without this both processes can read the same
	// sealed.jsonl, decide on the same event tail, and write divergent
	// chunks with different device tags (Greptile finding on PR #142).
	// On success we hold the lock for the whole seal pass so the
	// supervisor's next tick sees ErrSealBusy and yields back.
	lock, err := chunk.TryAcquireSealLock(dataDir)
	if err != nil {
		if errors.Is(err, chunk.ErrSealBusy) {
			return "note: skipping pre-push seal (daemon supervisor is sealing concurrently; this push uses chunks already on disk)\n"
		}
		return fmt.Sprintf("note: skipping pre-push seal (acquire seal lock: %v)\n", err)
	}
	defer func() {
		if cerr := lock.Close(); cerr != nil {
			_ = cerr
		}
	}()
	log, closeLog, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		return fmt.Sprintf("note: skipping pre-push seal (open event log: %v)\n", err)
	}
	defer func() {
		if cerr := closeLog(); cerr != nil {
			_ = cerr
		}
	}()
	cursor, err := chunk.NewFileCursor(dataDir)
	if err != nil {
		return fmt.Sprintf("note: skipping pre-push seal (open cursor: %v)\n", err)
	}
	mnemonic := loadWatchwordForSeal(ctx)
	// Resolve THIS device's stable identity id so a CLI-sealed chunk's
	// Header.Device matches the device that signed its events — consumers
	// enforce that binding via chunk.Header.AuthorsMatchDevice, and the
	// sealer's own-device filter (chunk ownsEvent) only seals events this
	// device authored. Without an established identity there is nothing a
	// paired device could accept, so skip and let the daemon supervisor seal
	// under the correct tag once identity exists.
	deviceTag := resolveSealDeviceTag(ctx)
	if deviceTag == "" {
		return "note: skipping pre-push seal (no device identity yet; the daemon seals once identity is established)\n"
	}
	for scopeName, remote := range syncCfg.Remotes {
		sealOneScopeBeforePush(ctx, log, cursor, store, syncCfg, mnemonic, deviceTag, scopeFilter, scopeName, remote)
	}
	return ""
}

// resolveSealDeviceTag reads this device's stable identity id from the keychain
// WITHOUT minting one (unlike identity.Store.DeviceID). It is the tag a
// CLI-sealed chunk must carry so its Header.Device matches the authoring
// device recorded in every event's signed EventAuthor. Returns "" when no
// identity is established yet.
func resolveSealDeviceTag(ctx context.Context) string {
	id, err := newSecretChain().Get(ctx, identity.KeychainDeviceID)
	if err != nil || id == "" {
		return ""
	}
	return id
}

// sealOneScopeBeforePush runs the seal step for a single configured
// remote. Split out of sealBeforePush so the parent function stays
// under the gocognit budget; every error path here is intentionally
// best-effort (silent continue) — a per-scope failure must not block
// the push, and the daemon supervisor's own sealer covers any gap.
func sealOneScopeBeforePush(
	ctx context.Context,
	log kernel.EventLog,
	cursor *chunk.FileCursor,
	store *chunk.FSStore,
	syncCfg config.SyncTable,
	mnemonic, deviceTag, scopeFilter, scopeName string,
	remote config.ScopeRemoteTable,
) {
	if scopeFilter != "" && scopeName != scopeFilter {
		return
	}
	if remote.URL == "" {
		return
	}
	scope := kernel.Scope(scopeName)
	if !scope.Valid() {
		return
	}
	// Envelope-mode (team) scopes are daemon-only: sealing them needs the
	// per-epoch DEK ring from the keychain AND the device's signed identity
	// as the chunk tag (so peers' AuthorsMatchDevice accepts the chunk).
	// The CLI has neither in this best-effort pre-push pass — and
	// resolveScopeKeyForSeal would wrongly hand back the Watchword-derived
	// "team" key, sealing a chunk no DEK holder could open. Skip; the
	// daemon supervisor seals team chunks under the correct key + identity.
	if remote.Envelope() {
		return
	}
	scopeKey, kerr := resolveScopeKeyForSeal(mnemonic, scope, remote)
	if kerr != nil {
		// Encrypted scope with no watchword: the supervisor would
		// also skip — surface the same posture from the CLI.
		return
	}
	maxEvents := pickSealMaxForScope(scopeKey, syncCfg)
	if _, _, serr := chunk.SealOnceWith(ctx, log, scope, scopeKey, store, cursor, maxEvents, deviceTag); serr != nil {
		// Best-effort: a per-scope failure is noisy in the buffer
		// but doesn't block the push.
		return
	}
}

// loadWatchwordForSeal pulls the user's mnemonic from the secret chain
// the same way the daemon's loadWatchword does. Returns "" on every
// error path: encrypted scopes will fail at resolveScopeKey, plaintext
// scopes never needed it.
func loadWatchwordForSeal(ctx context.Context) string {
	chain := newSecretChain()
	v, err := chain.Get(ctx, watchword.KeychainKey)
	if err != nil {
		if errors.Is(err, secret.ErrNotFound) {
			return ""
		}
		return ""
	}
	return v
}

// resolveScopeKeyForSeal mirrors buildScopeKeyMap's per-scope branch:
// encrypted scope + watchword present → 32-byte derived key; plaintext
// scope → nil (sealer takes the plaintext path); encrypted with no
// watchword → error so the caller can skip.
func resolveScopeKeyForSeal(mnemonic string, scope kernel.Scope, remote config.ScopeRemoteTable) ([]byte, error) {
	if !remote.EncryptedOrDefault() {
		return nil, nil
	}
	if mnemonic == "" {
		return nil, errors.New("encrypted scope requires watchword")
	}
	root, err := watchword.DeriveRoot(mnemonic)
	if err != nil {
		return nil, fmt.Errorf("derive root: %w", err)
	}
	key, err := watchword.DeriveScopeKey(root, string(scope))
	if err != nil {
		return nil, fmt.Errorf("derive scope key: %w", err)
	}
	return key, nil
}

// pickSealMaxForScope picks the per-chunk event cap the same way the
// daemon supervisor does: plaintext defaults 2_000 (so .jsonl files
// stay under github.com's ~1 MiB web-UI render ceiling), encrypted
// defaults 10_000 (gzip amortises the larger batch).
func pickSealMaxForScope(scopeKey []byte, syncCfg config.SyncTable) int {
	if scopeKey == nil {
		if syncCfg.SealMaxEventsPlaintext > 0 {
			return syncCfg.SealMaxEventsPlaintext
		}
		return 2_000
	}
	if syncCfg.SealMaxEvents > 0 {
		return syncCfg.SealMaxEvents
	}
	return 10_000
}

// pushScope replicates the supervisor's pushOnce flow against a
// fresh git transport. Idempotent: the transport's commit-only-on-
// new-chunks contract means repeated runs are no-ops once the remote
// has caught up.
func pushScope(ctx context.Context, dataDir, scopeName, remoteURL string, store *chunk.FSStore, ident *pushIdentity) (int, error) {
	scope := kernel.Scope(scopeName)
	opts := gittransport.Options{
		DataDir:    dataDir,
		RemoteURLs: map[kernel.Scope]string{scope: remoteURL},
		DeviceID:   syncCLIDeviceID,
	}
	// Author the commit as the resolved user (bring-your-own-git) instead of the
	// Mazarbul sentinel the transport defaults to. Resolved once by the caller so
	// every scope in this push shares the one identity.
	if ident != nil {
		opts.AuthorName = ident.resolved.Name
		opts.AuthorMail = ident.resolved.Email
		opts.CommitIdentityMode = ident.resolved.Mode
		opts.SelfPrincipalUID = ident.selfPrincipalUID
		opts.CoAuthors = ident.coAuthors
	}
	tr, err := gittransport.New(opts)
	if err != nil {
		return 0, fmt.Errorf("transport: %w", err)
	}
	local, err := store.List(ctx, scope)
	if err != nil {
		return 0, fmt.Errorf("list local: %w", err)
	}
	if len(local) == 0 {
		return 0, nil
	}
	remoteListing, err := tr.List(ctx, scope)
	if err != nil {
		return 0, fmt.Errorf("list remote: %w", err)
	}
	seen := make(map[string]struct{}, len(remoteListing.Chunks))
	for i := range remoteListing.Chunks {
		seen[remoteListing.Chunks[i].ChunkID] = struct{}{}
	}
	blobs := make([]transport.Blob, 0, len(local))
	for i := range local {
		if _, dup := seen[local[i].ChunkID]; dup {
			continue
		}
		body, gerr := store.Get(ctx, scope, local[i].ChunkID)
		if gerr != nil {
			return 0, fmt.Errorf("get chunk %s: %w", local[i].ChunkID, gerr)
		}
		blobs = append(blobs, transport.Blob{
			Header: local[i].Header,
			Body:   io.NopCloser(bytesReader(body)),
		})
	}
	if len(blobs) == 0 {
		return 0, nil
	}
	return tr.Push(ctx, scope, blobs)
}

// remoteChunkIDs lists the remote MANIFEST through a fresh transport
// and returns the chunk-id set. Sharing this through status lets the
// drift report distinguish "I have chunks not yet pushed" (ahead) from
// "the remote has chunks I haven't pulled" (behind) without a second
// network round-trip.
func remoteChunkIDs(ctx context.Context, dataDir, scopeName, remoteURL string) (map[string]struct{}, error) {
	scope := kernel.Scope(scopeName)
	tr, err := gittransport.New(gittransport.Options{
		DataDir:    dataDir,
		RemoteURLs: map[kernel.Scope]string{scope: remoteURL},
		DeviceID:   "maz-sync-cli",
	})
	if err != nil {
		return nil, err
	}
	listing, err := tr.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(listing.Chunks))
	for i := range listing.Chunks {
		out[listing.Chunks[i].ChunkID] = struct{}{}
	}
	return out, nil
}

// mirrorBehindByAncestry returns how many commits origin/main is ahead of the
// local mirror's main by GIT ANCESTRY, using the mirror's already-cached refs —
// no fetch, no network. This is the honest-health counterpart to chunkSetDrift:
// a losing push-contention race leaves the mirror behind origin/main while the
// FSStore still holds every chunk (so ahead/behind read 0). Returns 0 on any
// error (missing ref, no mirror yet, corrupt repo) — unknown, not behind.
func mirrorBehindByAncestry(ctx context.Context, dataDir, scopeName string) int {
	repoDir := filepath.Join(dataDir, "sync", scopeName, "repo")
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return 0
	}
	// #nosec G204 — gitBin is the resolved path from exec.LookPath("git");
	// repoDir is assembled from dataDir + a scopeFilter-validated scope name.
	cmd := exec.CommandContext(ctx, gitBin, "-C", repoDir,
		"rev-list", "--count", "main..origin/main")
	out, runErr := cmd.Output()
	if runErr != nil {
		return 0
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil {
		return 0
	}
	return n
}

// noRemoteMsg returns the user-facing message for the empty-rows path.
// When scopeFilter is set the message names the requested scope so the
// operator knows which scope is unconfigured; otherwise the generic
// all-scopes wording is used.
func noRemoteMsg(scopeFilter string) string {
	if scopeFilter != "" {
		return fmt.Sprintf("scope %q has no remote configured (edit <data-dir>/config.toml [sync.%s] or run `maz config edit`)", scopeFilter, scopeFilter)
	}
	return "no sync remotes configured (edit <data-dir>/config.toml [sync.<scope>] or run `maz config edit`)"
}

func renderSyncStatus(w io.Writer, rows []syncScopeReport, scopeFilter string, enabled bool, format output.Format) error {
	// JSON and NDJSON: emit the structured report directly; no colour, no table.
	switch format {
	case output.FormatJSON:
		out := rows
		if out == nil {
			out = []syncScopeReport{}
		}
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
		return nil
	case output.FormatNDJSON:
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		for i := range rows {
			if err := enc.Encode(rows[i]); err != nil {
				return fmt.Errorf("encode ndjson: %w", err)
			}
		}
		return nil
	default:
		// FormatText: fall through to the human table below.
	}
	return renderSyncStatusText(w, rows, scopeFilter, enabled)
}

// renderSyncStatusText renders the human-readable ASCII table for `maz sync status`.
// Split from renderSyncStatus to keep each function under the gocognit limit.
func renderSyncStatusText(w io.Writer, rows []syncScopeReport, scopeFilter string, enabled bool) error {
	clr := colorizer.New(enabled, w)
	p := clistatus.New(w, enabled)
	var buf strings.Builder
	if len(rows) == 0 {
		buf.WriteString(p.WarnBullet(noRemoteMsg(scopeFilter)))
		buf.WriteString("\n")
		if _, err := io.WriteString(w, buf.String()); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
		return nil
	}
	fmt.Fprintf(&buf, "%-10s %-10s %-6s %-6s %-6s %-6s %-12s %s\n",
		"scope", "mode", "local", "remote", "ahead", "behind", "last_fetch", "remote_url")
	for i := range rows {
		if err := renderSyncStatusRow(&buf, &rows[i], clr); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, buf.String()); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// renderSyncStatusRow appends one scope row (plus optional error annotations)
// to buf. Split from renderSyncStatusText to keep cognitive complexity low.
func renderSyncStatusRow(buf *strings.Builder, r *syncScopeReport, clr *colorizer.Colorizer) error {
	// Pad each cell to its column width BEFORE colorizing: %-6s counts ANSI
	// escape bytes as visible width, so padding a styled string misaligns the
	// table. Wrapping the already-padded plain cell keeps the visible width at
	// 6 and the colour invisible to the layout.
	remoteCell := fmt.Sprintf("%-6s", strconv.Itoa(r.RemoteCount))
	if r.RemoteErr != "" {
		remoteCell = clr.Err(fmt.Sprintf("%-6s", "ERR"))
	}
	var aheadCell, behindCell string
	if r.DriftErr != "" {
		aheadCell = fmt.Sprintf("%-6s", "?")
		behindCell = fmt.Sprintf("%-6s", "?")
	} else {
		aheadCell = fmt.Sprintf("%-6s", formatDriftCount(r.Ahead, "+"))
		behindCell = fmt.Sprintf("%-6s", formatDriftCount(r.Behind, "-"))
		if r.Ahead > 0 {
			aheadCell = clr.Added(aheadCell)
		}
		if r.Behind > 0 {
			behindCell = clr.Changed(behindCell)
		}
	}
	var lastFetch time.Time
	if r.LastFetch != nil {
		lastFetch = *r.LastFetch
	}
	fmt.Fprintf(buf, "%-10s %-10s %-6d %s %s %s %-12s %s\n",
		r.Scope, r.Mode, r.LocalCount, remoteCell,
		aheadCell, behindCell, formatLastFetch(lastFetch), r.RemoteURL)
	if r.RemoteErr != "" {
		fmt.Fprintf(buf, "             ↳ remote error: %s\n", r.RemoteErr)
	}
	if r.DriftErr != "" {
		fmt.Fprintf(buf, "             ↳ drift error: %s\n", r.DriftErr)
	}
	if r.MirrorBehind > 0 {
		fmt.Fprintf(buf, "             %s\n", clr.Err(fmt.Sprintf(
			"↳ mirror behind origin by %d commit(s) — a push race may have split-brained this scope; run `maz doctor` / `maz sync pull`",
			r.MirrorBehind)))
	}
	return nil
}

// loadResolvedSyncConfig reads UserConfig + UserState and returns
// the merged view of [sync]. Mirrors what the daemon would see at
// boot so the CLI reports + pushes against the same data. DataDir
// inherits the maz-root --data-dir flag; empty falls back to the
// runtime default (XDG / platform path).
func loadResolvedSyncConfig(dataDir string) (string, config.SyncTable, error) {
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	user, err := config.LoadUserConfig(dataDir)
	if err != nil {
		if !errors.Is(err, config.ErrUserConfigNotFound) {
			return "", config.SyncTable{}, fmt.Errorf("load user config: %w", err)
		}
		user = config.UserConfig{}
	}
	state, err := config.LoadUserState(dataDir)
	if err != nil {
		if !errors.Is(err, config.ErrUserStateNotFound) {
			return "", config.SyncTable{}, fmt.Errorf("load user state: %w", err)
		}
		state = config.UserState{}
	}
	merged := config.Merge(user, state)
	return dataDir, merged.Sync, nil
}

func sortedScopeNames(s config.SyncTable) []string {
	out := make([]string, 0, len(s.Remotes))
	for name := range s.Remotes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func modeLabel(encrypted bool) string {
	if encrypted {
		return "encrypted"
	}
	return "plaintext"
}

// bytesReader is io.NopCloser(bytes.NewReader(...)) Factored out so
// callers do not have to import the bytes package alongside io for
// every push.
func bytesReader(b []byte) io.Reader { return &byteReader{b: b} }

type byteReader struct {
	b []byte
	i int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// newSyncDiffCmd returns the `maz sync diff` subcommand — a convenience
// wrapper around `maz diff` that automatically uses the scope's remote ref
// (origin/main...HEAD) so the user sees outgoing + incoming changes without
// having to know the revspec grammar.
func newSyncDiffCmd() *cobra.Command {
	var (
		scope   string
		noFetch bool
		stat    bool
	)
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Show what differs between local state and the remote (outgoing + incoming)",
		Long: `diff fetches the remote (unless --no-fetch), then runs the equivalent of
'maz diff origin/main...HEAD' for each configured scope. The three-dot revspec
shows changes on both sides: outgoing (pushed commits not yet merged upstream)
and incoming (remote commits not yet pulled).

Note: diff compares the pushed git history in the local mirror. Sealed chunks
that have not been pushed yet do not appear here — use 'maz sync status' to
see ahead/behind counts for sealed-but-unpushed chunks.

Examples:
  maz sync diff                          # fetch + full diff for all scopes
  maz sync diff --scope personal         # one scope only
  maz sync diff --no-fetch               # offline, use last-fetched state
  maz sync diff --stat                   # compact counts only`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkSyncScope(cmd, "sync diff", scope); err != nil {
				return err
			}
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			flags := &diffFlags{
				scope:   scope,
				format:  "text",
				stat:    stat,
				remote:  !noFetch,
				noFetch: noFetch,
			}
			// Use the symmetric three-dot revspec to show both sides.
			return runDiff(cmd.Context(), cmd.OutOrStdout(), dataDir, "origin/main...HEAD", flags)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "only this scope; empty = every configured scope")
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "skip fetch; use cached remote state")
	cmd.Flags().BoolVar(&stat, "stat", false, "compact summary: counts only")
	return cmd
}

// newSyncLogCmd returns the `maz sync log` subcommand — lists CHANGELOG
// fragments from the scope's git mirror in reverse chronological order.
// This is the sync-centric log; `maz log` is the event-log tail.
func newSyncLogCmd() *cobra.Command {
	var (
		scope   string
		limit   int
		remote  bool
		revspec string
		format  string
	)
	cmd := &cobra.Command{
		Use:   "log [revspec]",
		Short: "List CHANGELOG fragments from the scope mirror in reverse-chronological order",
		Long: `log lists the per-push CHANGELOG fragments stored under CHANGELOG/ in the
scope's git mirror, one summary line per push, newest first.

revspec restricts the range (default: full history). Accepts the same grammar
as 'maz diff': HEAD~5..HEAD, origin/main..HEAD, last-3-pushes, etc.

Examples:
  maz sync log                           # full history, all scopes
  maz sync log --scope personal -n 10   # last 10 pushes for personal scope
  maz sync log origin/main..HEAD        # unpushed pushes
  maz sync log --remote                  # fetch first
  maz sync log --format json | jq '.[].summary'`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				revspec = args[0]
			}
			dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
			w := cmd.OutOrStdout()
			enabled := format != diffFormatJSON && interactive.ShouldStyle(cmd, w)
			return runSyncLog(cmd.Context(), w, dataDir, scope, revspec, limit, remote, format, enabled)
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "", "only this scope; empty = every configured scope")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "maximum entries to show (0 = no limit)")
	cmd.Flags().BoolVar(&remote, "remote", false, "fetch remote before listing")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	return cmd
}

// changelogEntry is one CHANGELOG fragment summary for `maz sync log`.
type changelogEntry struct {
	// CommitHash is the short git commit hash.
	CommitHash string `json:"commitHash"`
	// Date is the commit date (YYYY-MM-DD HH:MM).
	Date string `json:"date"`
	// Device is the git author (device slug).
	Device string `json:"device"`
	// Scope is the scope name.
	Scope string `json:"scope"`
	// Summary is the commit subject line.
	Summary string `json:"summary"`
}

func runSyncLog(ctx context.Context, w io.Writer, dataDir, scopeFilter, revspec string, limit int, remote bool, format string, enabled bool) error {
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}
	_, syncCfg, err := loadResolvedSyncConfig(dataDir)
	if err != nil {
		return fmt.Errorf("sync log: load config: %w", err)
	}
	scopes := sortedScopeNames(syncCfg)
	var allEntries []changelogEntry
	for _, scopeName := range scopes {
		if scopeFilter != "" && scopeName != scopeFilter {
			continue
		}
		repoDir := filepath.Join(dataDir, "sync", scopeName, "repo")
		if _, serr := os.Stat(filepath.Join(repoDir, ".git")); serr != nil {
			continue
		}
		if remote {
			fetchErrIgnored(ctx, repoDir)
		}
		entries, lerr := listChangelogEntries(ctx, repoDir, scopeName, revspec, limit)
		if lerr != nil {
			if _, werr := fmt.Fprintf(w, "scope: %s  log failed: %v\n", scopeName, lerr); werr != nil {
				return fmt.Errorf("sync log: write: %w", werr)
			}
			continue
		}
		allEntries = append(allEntries, entries...)
	}
	if format == diffFormatJSON {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		if allEntries == nil {
			allEntries = []changelogEntry{}
		}
		return enc.Encode(allEntries)
	}
	clr := colorizer.New(enabled, w)
	return writeChangelogText(w, allEntries, clr)
}

// listChangelogEntries shells `git log` over the CHANGELOG/ directory and
// returns the entries sorted newest-first.
func listChangelogEntries(ctx context.Context, repoDir, scopeName, revspec string, limit int) ([]changelogEntry, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git not in PATH: %w", err)
	}
	args := []string{"-C", repoDir, "log", "--pretty=format:%h|%ci|%an|%s"}
	if revspec != "" {
		args = append(args, revspec)
	}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	args = append(args, "--", "CHANGELOG/")

	cmd := exec.CommandContext(ctx, gitBin, args...)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, nil //nolint:nilerr // no CHANGELOG yet = empty log, not an error
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	entries := make([]changelogEntry, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		e := parseChangelogLine(line, scopeName)
		entries = append(entries, e)
	}
	return entries, nil
}

func parseChangelogLine(line, scopeName string) changelogEntry {
	parts := strings.SplitN(line, "|", 4)
	if len(parts) != 4 {
		return changelogEntry{Scope: scopeName, Summary: line}
	}
	date := parts[1]
	if len(date) > 16 {
		date = date[:16]
	}
	return changelogEntry{
		CommitHash: parts[0],
		Date:       date,
		Device:     parts[2],
		Scope:      scopeName,
		Summary:    parts[3],
	}
}

func writeChangelogText(w io.Writer, entries []changelogEntry, clr *colorizer.Colorizer) error {
	var buf strings.Builder
	for _, e := range entries {
		deviceStr := fmt.Sprintf("%-16s", e.Device)
		scopeStr := fmt.Sprintf("%-10s", e.Scope)
		fmt.Fprintf(&buf, "%s  %s  %s  %s  %s\n",
			clr.Ident(e.CommitHash), clr.Dim(e.Date), clr.Meta(deviceStr), clr.Meta(scopeStr), e.Summary)
	}
	if len(entries) == 0 {
		buf.WriteString("(no changelog entries found)\n")
	}
	if _, err := io.WriteString(w, buf.String()); err != nil {
		return fmt.Errorf("sync log: write: %w", err)
	}
	return nil
}
