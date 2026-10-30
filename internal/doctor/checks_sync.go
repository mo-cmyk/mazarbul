package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/secret"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/privacy"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

// subsystemSync groups the Moria-sync probes in `maz doctor` output.
const subsystemSync = "sync"

// SyncChecks returns the rows the Moria sync layer contributes to
// `maz doctor`. Returned as a fresh slice on each call so callers
// can append to DefaultChecks without aliasing.
//
// Rows:
//  1. state.toml parseable              — optional file, must parse if present
//  2. config.sync remotes               — `[sync]` block is sane (URLs valid)
//  3. encryption posture                — per-scope plaintext/encrypted summary
//  4. watchword required-when-encrypted — warns if encrypted scope has no key
//  5. sensitive-payload posture         — FAILS if a plaintext scope holds a sensitive chunk
//  6. sealed chunks readable            — every per-scope sealed.jsonl loads
//  7. git binary in PATH                — git transport prerequisite
//  8. history rewrite                   — WARNs when a mirror is wedged by a force-push
//  9. dek_coverage                      — WARNs when a remote epoch's DEK is absent locally
func SyncChecks(dataDir string) []Check {
	chain := secret.NewChain(secret.NewEnvProvider(), secret.NewKeychainProvider())
	return []Check{
		newStateTOMLCheck(dataDir),
		newSyncRemotesCheck(dataDir),
		newEncryptionPostureCheck(dataDir),
		newWatchwordPostureCheck(dataDir, chain),
		newSensitivePayloadPostureCheck(dataDir),
		newSealedChunksReadableCheck(dataDir),
		newGitBinaryCheck(),
		newChunkIntegrityCheck(dataDir),
		newCommitIdentityCheck(dataDir),
		newRenderSizeCheck(dataDir),
		newRenderTombstoneRatioCheck(dataDir),
		newRenderCursorBehindCheck(dataDir),
		newRelationCandidatesStuckCheck(dataDir),
		newHistoryRewriteCheck(dataDir),
		newMirrorBehindCheck(dataDir),
		newDEKCoverageCheck(dataDir, chain),
	}
}

// newMirrorBehindCheck WARNs when a scope's local mirror is behind origin/main
// by git ancestry — the honest split-brain signal that the chunk-set-based
// `maz sync status` misses. When concurrent pushers contend, a losing device
// can end up with a mirror whose main lags the remote; the FSStore may still
// hold every chunk (so status reports "up to date") while the projection is
// stale. This check compares git commits directly: `git rev-list --count
// main..origin/main` against the cached refs (local-only, no fetch — same
// posture as sync.history_rewrite). A force-push wedge is reported by
// sync.history_rewrite instead, so those scopes are skipped here to avoid
// double-warning.
func newMirrorBehindCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.mirror_behind",
		Sub: subsystemSync,
		Fn: func(ctx context.Context) (Status, string, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			gitBin, lookErr := exec.LookPath("git")
			if lookErr != nil {
				return StatusOK, "git not in PATH (mirror behind: skipped)", nil
			}
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return StatusOK, "no remotes configured (mirror behind: n/a)", nil
			}

			behind := findBehindScopes(ctx, gitBin, dataDir, sortedRemoteNames(resolved.Remotes))
			if len(behind) > 0 {
				return StatusWarn, fmt.Sprintf(
					"scope(s) %v: local mirror is behind origin/main — sync may have split-brained (a push lost a contention race); run `maz sync pull && maz rebuild` to reconcile",
					behind), nil
			}
			return StatusOK, "all mirrors up to date with origin/main", nil
		},
	}
}

// findBehindScopes returns scopes whose local mirror main lags origin/main.
// Force-push-wedged scopes are skipped (sync.history_rewrite reports those).
func findBehindScopes(ctx context.Context, gitBin, dataDir string, names []string) []string {
	behind := make([]string, 0)
	for _, name := range names {
		if !kernel.Scope(name).Valid() {
			continue
		}
		repoDir := filepath.Join(dataDir, "sync", name, "repo")
		if _, statErr := os.Stat(repoDir); os.IsNotExist(statErr) {
			continue // mirror not yet cloned
		}
		if isMirrorWedged(ctx, gitBin, repoDir) {
			continue // force-push wedge — reported by sync.history_rewrite
		}
		if mirrorBehindCount(ctx, gitBin, repoDir) > 0 {
			behind = append(behind, name)
		}
	}
	return behind
}

// mirrorBehindCount returns how many commits origin/main is ahead of the local
// main (`git rev-list --count main..origin/main`) using cached refs, no fetch.
// Returns 0 on any error or when either ref is absent (empty remote, fresh
// mirror) — the check reports divergence, never false-positives on setup state.
func mirrorBehindCount(ctx context.Context, gitBin, repoDir string) int {
	// #nosec G204 — gitBin is the resolved path from exec.LookPath("git");
	// repoDir is assembled from dataDir + a kernel.Scope-validated name.
	cmd := exec.CommandContext(ctx, gitBin, "-C", repoDir,
		"rev-list", "--count", "main..origin/main")
	out, runErr := cmd.Output()
	if runErr != nil {
		return 0 // missing ref / corrupt repo — treat as unknown, not behind
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil {
		return 0
	}
	return n
}

// newHistoryRewriteCheck WARNs when a scope's local mirror is wedged because
// the remote's git history was force-pushed after the last recorded fast-forward.
// The check is intentionally cheap — no fetch, no network: it reads the
// maz-last-remote-head marker written by the transport after each successful
// pull, then runs `git merge-base --is-ancestor <markerSHA> origin/main`
// against the cached refs. Exit 1 from merge-base means the old HEAD is no
// longer an ancestor of the current origin/main, confirming a force-push.
// The check skips silently when:
//   - git is not in PATH (sync.git_binary covers that separately)
//   - no mirror exists for the scope (daemon has not pushed yet)
//   - no marker file (first run, or marker was lost)
//   - any git error other than exit 1 (bad SHA, corrupt repo — don't false-positive)
func newHistoryRewriteCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.history_rewrite",
		Sub: subsystemSync,
		Fn: func(ctx context.Context) (Status, string, error) {
			// Short timeout: local-only git invocation, no network needed.
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			gitBin, lookErr := exec.LookPath("git")
			if lookErr != nil {
				// git not installed; sync.git_binary check covers this separately.
				return StatusOK, "git not in PATH (history rewrite: skipped)", nil
			}

			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return StatusOK, "no remotes configured (history rewrite: n/a)", nil
			}

			names := sortedRemoteNames(resolved.Remotes)
			wedged := findWedgedScopes(ctx, gitBin, dataDir, names)
			if len(wedged) > 0 {
				return StatusWarn, fmt.Sprintf(
					"scope(s) %v: remote was force-pushed; local mirror is wedged — run `maz sync repair --scope <scope>` to delete the mirror, then `maz sync pull && maz rebuild` to restore",
					wedged), nil
			}
			return StatusOK, "no force-push wedges detected in local mirrors", nil
		},
	}
}

// findWedgedScopes walks names and returns those whose local mirror marker
// indicates the remote was force-pushed (oldHead is no longer an ancestor of
// origin/main). The check is local-only (no fetch); skip-on-any-unknown-error
// avoids false positives from corrupt or empty mirrors.
func findWedgedScopes(ctx context.Context, gitBin, dataDir string, names []string) []string {
	wedged := make([]string, 0)
	for _, name := range names {
		if !kernel.Scope(name).Valid() {
			continue
		}
		repoDir := filepath.Join(dataDir, "sync", name, "repo")
		if _, statErr := os.Stat(repoDir); os.IsNotExist(statErr) {
			continue // mirror not yet cloned
		}
		if isMirrorWedged(ctx, gitBin, repoDir) {
			wedged = append(wedged, name)
		}
	}
	return wedged
}

// isMirrorWedged reads the maz-last-remote-head marker and runs
// `git merge-base --is-ancestor <oldHead> origin/main` (local, no network).
// Returns true only on exit code 1 (NOT ancestor = force-push confirmed).
// Any other failure is treated as "unknown" — skip rather than false-positive.
func isMirrorWedged(ctx context.Context, gitBin, repoDir string) bool {
	markerPath := filepath.Join(repoDir, ".git", "maz-last-remote-head")
	raw, readErr := os.ReadFile(markerPath) //nolint:gosec // path = dataDir + validated scope + literal suffix
	if readErr != nil {
		return false // marker absent or unreadable — no prior state to compare
	}
	oldHead := strings.TrimSpace(string(raw))
	if oldHead == "" {
		return false
	}
	// #nosec G204 — gitBin is the resolved path from exec.LookPath("git");
	// repoDir is assembled from dataDir + a kernel.Scope-validated name.
	cmd := exec.CommandContext(ctx, gitBin, "-C", repoDir,
		"merge-base", "--is-ancestor", oldHead, "origin/main")
	if runErr := cmd.Run(); runErr != nil {
		var exitErr *exec.ExitError
		return errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1
	}
	return false
}

// sortedRemoteNames returns the keys of remotes sorted deterministically.
func sortedRemoteNames(remotes map[string]config.ScopeRemoteTable) []string {
	names := make([]string, 0, len(remotes))
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// newDEKCoverageCheck WARNs when the local mirror's MANIFEST reveals that
// the remote holds encrypted chunks from DEK epochs whose key is absent from
// the local keychain. A missing epoch means this device cannot decrypt some
// team chunks it has already pulled — or will pull on the next sync. The usual
// cause is a forward re-key after a revocation: a new or re-added member was
// granted only the latest epoch, not the earlier ones.
//
// Recovery: run `maz team key --request-regrant` to signal the admin that a
// re-wrap of the missing epoch(s) is needed, then run `maz sync pull` after
// the grant is published.
//
// The check is local-only (no network): it reads the on-disk MANIFEST.json
// written by the transport after each push, then probes the keychain for each
// epoch the manifest references. Scopes that are not encrypted are skipped; a
// missing mirror or manifest is not an error (fresh install or single-device
// setup).
func newDEKCoverageCheck(dataDir string, chain secret.Provider) CheckFunc {
	return CheckFunc{
		N:   "sync.dek_coverage",
		Sub: subsystemSync,
		Fn: func(ctx context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return StatusOK, "no remotes configured (dek coverage: n/a)", nil
			}

			names := sortedRemoteNames(resolved.Remotes)
			missing, covered := collectDEKCoverage(ctx, dataDir, names, resolved.Remotes, chain)
			if len(missing) > 0 {
				return StatusWarn, fmt.Sprintf(
					"missing DEK(s) for epoch(s) [%s]: cannot decrypt corresponding team chunks — run `maz team key --request-regrant` to signal the admin, then `maz sync pull` after the re-grant",
					strings.Join(missing, ", ")), nil
			}
			if covered == 0 {
				return StatusOK, "no encrypted team mirrors found (dek coverage: n/a)", nil
			}
			return StatusOK, fmt.Sprintf("%d DEK epoch(s) covered in local keychain", covered), nil
		},
	}
}

// dekCoverageEntry is the minimal MANIFEST.json chunk-entry shape needed for
// DEK-coverage analysis: whether the chunk is encrypted and its DEK epoch.
// Only encrypted entries contribute to the epoch set.
type dekCoverageEntry struct {
	Encrypted bool `json:"encrypted"`
	DEKEpoch  int  `json:"dekEpoch"`
}

// readMirrorDEKEpochs parses the MANIFEST.json at repoDir and returns the set
// of distinct DEK epochs referenced by encrypted chunks. A missing or
// unreadable manifest returns an empty set — the caller treats that as "no
// encrypted chunks found" (fresh mirror or plaintext scope).
func readMirrorDEKEpochs(repoDir string) map[int]struct{} {
	epochs := make(map[int]struct{})
	raw, err := os.ReadFile(filepath.Join(repoDir, "MANIFEST.json")) //nolint:gosec // fixed filename under validated scope mirror
	if err != nil {
		return epochs
	}
	var doc struct {
		Chunks []dekCoverageEntry `json:"chunks"`
	}
	if jErr := json.Unmarshal(raw, &doc); jErr != nil {
		return epochs
	}
	for _, c := range doc.Chunks {
		if c.Encrypted {
			epochs[c.DEKEpoch] = struct{}{}
		}
	}
	return epochs
}

// collectDEKCoverage scans each configured encrypted scope's local mirror
// MANIFEST and cross-references the referenced DEK epochs against the local
// keychain. Returns a sorted slice of missing epoch numbers (as strings for
// the WARN message) and the count of epochs that are covered.
func collectDEKCoverage(
	ctx context.Context,
	dataDir string,
	names []string,
	remotes map[string]config.ScopeRemoteTable,
	chain secret.Provider,
) (missing []string, covered int) {
	epochsSeen := make(map[int]struct{})
	for _, name := range names {
		r := remotes[name]
		if !r.EncryptedOrDefault() || !kernel.Scope(name).Valid() {
			continue
		}
		repoDir := filepath.Join(dataDir, "sync", name, "repo")
		if _, statErr := os.Stat(repoDir); os.IsNotExist(statErr) {
			continue // mirror not yet cloned
		}
		for epoch := range readMirrorDEKEpochs(repoDir) {
			epochsSeen[epoch] = struct{}{}
		}
	}
	for epoch := range epochsSeen {
		key := watchword.KeychainTeamDEKPrefix + strconv.Itoa(epoch)
		_, gErr := chain.Get(ctx, key)
		switch {
		case errors.Is(gErr, secret.ErrNotFound):
			missing = append(missing, strconv.Itoa(epoch))
		case gErr != nil:
			// Any unexpected keychain error: treat as missing (conservative —
			// avoid falsely reporting OK when the keychain is unavailable).
			missing = append(missing, strconv.Itoa(epoch))
		default:
			covered++
		}
	}
	sort.Strings(missing)
	return missing, covered
}

// newPendingRegrantCheck surfaces teamRegrantRequested events from team members
// whose device is missing DEK epochs. This is the admin-facing counterpart of
// sync.dek_coverage: when a member runs `maz team key --request-regrant` a
// teamRegrantRequested event is appended to the team log; after the admin's
// next sync pull the event lands in the local episodes table and this check
// WARNs, prompting the admin to run `maz team grant`.
//
// Queries the episodes projection via the shared read-only SQLite handle.
// Events from the last 30 days are surfaced; older ones are ignored.
func newPendingRegrantCheck(s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "sync.pending_regrant",
		Sub: subsystemSync,
		Fn: func(ctx context.Context) (Status, string, error) {
			reqs, qErr := queryPendingRegrantRequests(ctx, s)
			if qErr != nil {
				return StatusFail, "", qErr
			}
			if len(reqs) == 0 {
				return StatusOK, "no pending DEK regrant requests from team members", nil
			}
			lines := make([]string, 0, len(reqs))
			for _, r := range reqs {
				lines = append(lines, fmt.Sprintf("%s (epochs %v)", r.principalUID, r.missingEpochs))
			}
			return StatusWarn, fmt.Sprintf(
				"%d team member(s) need DEK epoch re-wrap: %s — run `maz team grant --join-key <member-key>` for each",
				len(reqs), strings.Join(lines, "; ")), nil
		},
	}
}

// pendingRegrantRow is one parsed teamRegrantRequested event row.
type pendingRegrantRow struct {
	principalUID  string
	missingEpochs []int
}

// queryPendingRegrantRequests fetches distinct teamRegrantRequested events
// from the last 30 days via the shared SQLite store, one row per requesting
// principal (most-recent first). Older events are excluded.
func queryPendingRegrantRequests(ctx context.Context, s *sharedSQLite) ([]pendingRegrantRow, error) {
	store, err := s.acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	const q = `
		SELECT
			json_extract(payload_json, '$.requestingPrincipalUid') AS principal_uid,
			json_extract(payload_json, '$.missingEpochs')          AS epochs_json
		FROM episodes
		WHERE kind = 'teamRegrantRequested'
		  AND scope = 'team'
		  AND create_time >= ?
		ORDER BY create_time DESC`
	rows, qErr := store.Reader().QueryContext(ctx, q, cutoff)
	if qErr != nil {
		return nil, fmt.Errorf("query teamRegrantRequested events: %w", qErr)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			_ = cerr
		}
	}()
	seen := make(map[string]struct{})
	out := make([]pendingRegrantRow, 0)
	for rows.Next() {
		var principalUID, epochsJSON string
		if sErr := rows.Scan(&principalUID, &epochsJSON); sErr != nil {
			return nil, fmt.Errorf("scan regrant row: %w", sErr)
		}
		if _, dup := seen[principalUID]; dup {
			continue // keep only the most-recent request per principal
		}
		seen[principalUID] = struct{}{}
		var epochs []int
		if jErr := json.Unmarshal([]byte(epochsJSON), &epochs); jErr != nil {
			epochs = nil // best-effort: still surface the principal
		}
		out = append(out, pendingRegrantRow{principalUID: principalUID, missingEpochs: epochs})
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("iterate regrant rows: %w", rErr)
	}
	return out, nil
}

// newChunkIntegrityCheck recomputes each local mirror chunk's content hash
// and compares it to the hash the MANIFEST recorded, FAILing when a chunk
// was edited outside Mazarbul. This is the local-mirror counterpart of the
// pull-time guard (a tampered remote chunk is refused on pull): it catches a
// hand-edit to <dataDir>/sync/<scope>/repo/chunks before it is pushed. Scopes
// whose MANIFEST predates the hash (schemaVersion < 2) are reported as
// unverified, never tampered, so an older mirror upgrades cleanly.
func newChunkIntegrityCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.chunk_integrity",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return StatusOK, "no remotes configured (chunk integrity: n/a)", nil
			}
			tampered, verified, unverified, aggErr := aggregateChunkIntegrity(dataDir, resolved.Remotes)
			if aggErr != nil {
				return StatusFail, "", aggErr
			}
			return chunkIntegrityVerdict(tampered, verified, unverified)
		},
	}
}

// aggregateChunkIntegrity scans every configured scope's local mirror and
// folds the per-scope results into one set of tampered IDs (scope-qualified)
// plus running verified/unverified counts. Scopes are walked in sorted order
// so the verdict string is deterministic.
func aggregateChunkIntegrity(dataDir string, remotes map[string]config.ScopeRemoteTable) (tampered []string, verified, unverified int, err error) {
	names := make([]string, 0, len(remotes))
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		bad, ok, unv, scanErr := scanChunkIntegrity(filepath.Join(dataDir, "sync", name, "repo"))
		if scanErr != nil {
			return nil, 0, 0, fmt.Errorf("scope=%s: %w", name, scanErr)
		}
		verified += ok
		unverified += unv
		for _, id := range bad {
			tampered = append(tampered, name+"/"+id)
		}
	}
	return tampered, verified, unverified, nil
}

// chunkIntegrityVerdict turns the aggregated counts into a doctor row:
// FAIL when any chunk mismatches its recorded hash, otherwise an OK row that
// distinguishes verified, unverified (pre-v2 manifest), and empty mirrors.
func chunkIntegrityVerdict(tampered []string, verified, unverified int) (Status, string, error) {
	if len(tampered) > 0 {
		return StatusFail, fmt.Sprintf(
			"%d chunk(s) edited outside Mazarbul: %s — restore from git history; chunks/ is machine-managed and must not be hand-edited",
			len(tampered), strings.Join(tampered, ", ")), nil
	}
	switch {
	case verified == 0 && unverified == 0:
		return StatusOK, "no local mirror chunks to verify yet", nil
	case verified == 0:
		return StatusOK, fmt.Sprintf("%d chunk(s) present but unverified (pre-v2 MANIFEST); will verify after next push", unverified), nil
	default:
		return StatusOK, fmt.Sprintf("%d chunk(s) verified against MANIFEST", verified), nil
	}
}

// newCommitIdentityCheck reports the host-visible commit author the sync
// transport will stamp (the bring-your-own-git surface, Workstream A). It WARNs
// when "real" mode would publish the operator's real git email to an
// off-device remote — the commit author/committer fields are plaintext even
// when chunk payloads are encrypted, so "real" leaks the address regardless of
// the scope's encryption. Noreply (default), anonymous, and an explicit
// override are all reported OK.
func newCommitIdentityCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.commit_identity",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return StatusOK, "no remotes configured (commit identity: n/a)", nil
			}
			status, detail := commitIdentityVerdict(&resolved)
			return status, detail, nil
		},
	}
}

// commitIdentityVerdict maps the resolved sync identity config to a doctor
// verdict. Pure (no filesystem/dispatcher) so it is unit-testable.
func commitIdentityVerdict(sync *config.SyncTable) (Status, string) {
	id := sync.Identity
	if id == nil {
		id = &config.SyncIdentityTable{}
	}
	if id.Name != "" && id.Email != "" {
		return StatusOK, fmt.Sprintf("explicit override: commits authored as %s <%s>", id.Name, id.Email)
	}
	switch normalizeDoctorCommitMode(id.CommitIdentity) {
	case config.CommitIdentityAnonymous:
		return StatusOK, "anonymous: commits use the generic Mazarbul author (no personal attribution)"
	case config.CommitIdentityReal:
		if hosts := offDeviceRemoteScopes(sync.Remotes); len(hosts) > 0 {
			return StatusWarn, fmt.Sprintf(
				"real mode publishes your git email to off-device remote(s) %s; set [sync.identity] commit_identity=\"noreply\" to attribute on GitHub without exposing your address",
				strings.Join(hosts, ", "))
		}
		return StatusOK, "real: commits use your git-config email (only local/file remotes configured)"
	default:
		return StatusOK, "noreply: commits attribute to you on GitHub via a no-reply email; your real address is never exposed"
	}
}

// normalizeDoctorCommitMode mirrors the daemon's mode normalization (an empty
// or unknown value defaults to noreply) without importing the daemon package.
func normalizeDoctorCommitMode(m string) string {
	switch m {
	case config.CommitIdentityReal, config.CommitIdentityAnonymous:
		return m
	default:
		return config.CommitIdentityNoreply
	}
}

// offDeviceRemoteScopes returns the scope names whose remote URL leaves the
// device (anything but a local file:// or path URL), sorted. These are the
// remotes that would receive a plaintext author line.
func offDeviceRemoteScopes(remotes map[string]config.ScopeRemoteTable) []string {
	out := make([]string, 0, len(remotes))
	for scope, r := range remotes {
		if r.URL == "" || isLocalRemoteURL(r.URL) {
			continue
		}
		out = append(out, scope)
	}
	sort.Strings(out)
	return out
}

// isLocalRemoteURL reports whether a remote URL is on the same machine (a
// file:// URL or a filesystem path), so it never carries the author line off
// the device.
func isLocalRemoteURL(u string) bool {
	return strings.HasPrefix(u, "file://") || strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".")
}

// scanChunkIntegrity walks one scope mirror's chunks dir and compares each
// chunk file's content hash to the MANIFEST record. Returns the IDs that
// mismatch, the count verified clean, and the count present-but-unrecorded
// (pre-v2 manifest). A missing chunks dir or manifest is not an error — a
// fresh/unpushed scope simply has nothing to verify.
func scanChunkIntegrity(repoDir string) (tampered []string, verified, unverified int, err error) {
	chunksDir := filepath.Join(repoDir, "chunks")
	entries, rerr := os.ReadDir(chunksDir)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return nil, 0, 0, nil
		}
		return nil, 0, 0, fmt.Errorf("readdir chunks: %w", rerr)
	}
	recorded := readScopeManifestHashes(filepath.Join(repoDir, "MANIFEST.json"))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := chunkIDFromFilename(e.Name())
		if !ok {
			continue
		}
		want, recordedHash := recorded[id]
		if !recordedHash {
			unverified++
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(chunksDir, e.Name())) //nolint:gosec // doctor-internal dataDir + fixed scope + chunk filename
		if readErr != nil {
			return nil, 0, 0, fmt.Errorf("read chunk %s: %w", e.Name(), readErr)
		}
		if chunk.ContentHash(raw) == want {
			verified++
		} else {
			tampered = append(tampered, id)
		}
	}
	sort.Strings(tampered)
	return tampered, verified, unverified, nil
}

// chunkIDFromFilename strips a recognised chunk extension (.jsonl plaintext,
// .maz encrypted) and returns the chunkID, or ok=false for any other file.
func chunkIDFromFilename(name string) (string, bool) {
	for _, ext := range []string{".jsonl", ".maz"} {
		if strings.HasSuffix(name, ext) {
			return strings.TrimSuffix(name, ext), true
		}
	}
	return "", false
}

// readScopeManifestHashes parses a local MANIFEST.json into a chunkID ->
// content-hash map. Returns an empty map when the manifest is absent,
// unparseable, or pre-v2 (no hashes recorded) — the caller treats a missing
// hash as unverified, not tampered.
func readScopeManifestHashes(path string) map[string]string {
	out := make(map[string]string)
	raw, err := os.ReadFile(path) //nolint:gosec // doctor-internal dataDir + literal MANIFEST.json
	if err != nil {
		return out
	}
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		Chunks        []struct {
			ChunkID     string `json:"chunkId"`
			ContentHash string `json:"contentHash"`
		} `json:"chunks"`
	}
	if jErr := json.Unmarshal(raw, &doc); jErr != nil || doc.SchemaVersion < 2 {
		return out
	}
	for _, c := range doc.Chunks {
		if c.ContentHash != "" {
			out[c.ChunkID] = c.ContentHash
		}
	}
	return out
}

// renderSizeThresholdsBytes are the soft warning ceilings the render-
// size check tests against. The values come from the renderer scale budgets:
// the per-scope rendered tree should stay under 1 GiB, and the
// canonical entities.jsonl should stay under 200 MiB (above that,
// github.com truncates the inline render and `git diff` slows
// noticeably). Exported as a var so tests can override without
// fork-and-restore plumbing.
//
//nolint:gochecknoglobals // tunable knob, intentionally package-level.
var (
	renderDirWarnBytes      int64 = 1 << 30   // 1 GiB
	renderEntitiesWarnBytes int64 = 200 << 20 // 200 MiB
)

// newRenderSizeCheck warns when the per-scope rendered/ tree gets
// uncomfortably large. Two signals:
//   - rendered/ total bytes > renderDirWarnBytes (1 GiB) → warn.
//   - rendered/entities.jsonl > renderEntitiesWarnBytes (200 MiB) → warn.
//
// Always StatusOK on encrypted scopes (no rendered tree there by
// design). The check is opportunistic: a missing tree is normal on
// fresh installs and not flagged.
func newRenderSizeCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.render_size",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return StatusOK, "no remotes configured (rendered tree: n/a)", nil
			}
			worst := renderSizeWorst{}
			for name, r := range resolved.Remotes {
				if r.EncryptedOrDefault() {
					continue
				}
				renderedDir := filepath.Join(dataDir, "sync", name, "repo", "rendered")
				total, entitiesBytes, scanErr := scanRenderedSize(renderedDir)
				if scanErr != nil {
					return StatusFail, "", fmt.Errorf("scope=%s: %w", name, scanErr)
				}
				worst.absorb(name, total, entitiesBytes)
			}
			return worst.verdict()
		},
	}
}

// renderSizeWorst tracks the largest rendered-tree footprint seen
// across all scopes so the verdict surfaces the worst offender by
// name rather than averaging. Render scale is per-scope (the renderer
// emits one tree per scope), and the operator wants to know which
// scope to inspect.
type renderSizeWorst struct {
	totalScope, totalSize        string
	totalBytes                   int64
	entityScope                  string
	entityBytes                  int64
	scopesSeen, scopesWithRender int
}

func (w *renderSizeWorst) absorb(scope string, total, entityBytes int64) {
	w.scopesSeen++
	if total > 0 {
		w.scopesWithRender++
	}
	if total > w.totalBytes {
		w.totalBytes = total
		w.totalScope = scope
		w.totalSize = humanBytesRender(total)
	}
	if entityBytes > w.entityBytes {
		w.entityBytes = entityBytes
		w.entityScope = scope
	}
}

func (w *renderSizeWorst) verdict() (Status, string, error) {
	if w.scopesSeen == 0 {
		return StatusOK, "no plaintext scopes configured (rendered tree: n/a)", nil
	}
	if w.scopesWithRender == 0 {
		return StatusOK, "no rendered trees on disk yet (daemon has not pushed)", nil
	}
	switch {
	case w.totalBytes > renderDirWarnBytes:
		return StatusWarn, fmt.Sprintf(
			"rendered tree for scope=%s is %s (>%s warn threshold); github.com starts truncating large trees — consider compacting",
			w.totalScope, w.totalSize, humanBytesRender(renderDirWarnBytes)), nil
	case w.entityBytes > renderEntitiesWarnBytes:
		return StatusWarn, fmt.Sprintf(
			"entities.jsonl for scope=%s is %s (>%s warn threshold); github.com declines to render large JSONL inline",
			w.entityScope, humanBytesRender(w.entityBytes), humanBytesRender(renderEntitiesWarnBytes)), nil
	default:
		return StatusOK, fmt.Sprintf("rendered trees within budget (worst: %s @ %s)",
			w.totalScope, w.totalSize), nil
	}
}

// scanRenderedSize returns the cumulative size of dir and the size of
// dir/entities.jsonl in bytes. Missing dir is (0, 0, nil) — fresh
// installs are not failures.
func scanRenderedSize(dir string) (total, entitiesBytes int64, err error) {
	info, statErr := os.Stat(dir)
	if os.IsNotExist(statErr) {
		return 0, 0, nil
	}
	if statErr != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", dir, statErr)
	}
	if !info.IsDir() {
		return 0, 0, fmt.Errorf("%s is not a directory", dir)
	}
	walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if fi.IsDir() {
			return nil
		}
		total += fi.Size()
		if filepath.Base(path) == "entities.jsonl" {
			entitiesBytes = fi.Size()
		}
		return nil
	})
	if walkErr != nil {
		return 0, 0, fmt.Errorf("walk %s: %w", dir, walkErr)
	}
	return total, entitiesBytes, nil
}

// humanBytesRender stringifies a byte count for the doctor row. Kept
// local to avoid coupling the doctor package to the CLI's sync.go
// helper of the same intent.
func humanBytesRender(n int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
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

// newStateTOMLCheck verifies that ${dataDir}/state.toml is either
// absent (the operator has not customised any overrides — fine) or
// parses cleanly. A parse error degrades the daemon to "stale
// overrides held" without any UI signal until restart; the check
// surfaces the failure before the operator has to chase it down.
func newStateTOMLCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.state_toml_parseable",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			_, err := config.LoadUserState(dataDir)
			switch {
			case errors.Is(err, config.ErrUserStateNotFound):
				return StatusOK, "state.toml absent (no overrides — fine)", nil
			case err != nil:
				return StatusFail, "", fmt.Errorf("parse state.toml: %w", err)
			default:
				return StatusOK, "state.toml parses cleanly", nil
			}
		},
	}
}

// newSyncRemotesCheck reads config.toml's [sync] block (overlaid by
// state.toml) and validates each configured scope remote. An unset
// block is fine: sync is opt-in. URLs must be non-empty when present.
func newSyncRemotesCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.remotes_configured",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			cfg, err := config.LoadUserConfig(dataDir)
			if errors.Is(err, config.ErrUserConfigNotFound) {
				return StatusOK, "config.toml absent (sync inactive)", nil
			}
			if err != nil {
				return StatusFail, "", fmt.Errorf("load config: %w", err)
			}
			state, sErr := config.LoadUserState(dataDir)
			if sErr != nil && !errors.Is(sErr, config.ErrUserStateNotFound) {
				return StatusFail, "", fmt.Errorf("load state: %w", sErr)
			}
			resolved := config.Merge(cfg, state)
			if len(resolved.Sync.Remotes) == 0 {
				return StatusOK, "no remotes configured (single-device mode)", nil
			}
			scopes := []string{}
			for name, r := range resolved.Sync.Remotes {
				if r.URL == "" {
					return StatusWarn, fmt.Sprintf("scope=%s remote has empty url; sealing OK, pushing skipped", name), nil
				}
				if !kernel.Scope(name).Valid() {
					return StatusWarn, fmt.Sprintf("scope=%s is not a recognised kernel scope", name), nil
				}
				scopes = append(scopes, name)
			}
			return StatusOK, fmt.Sprintf("remotes configured for %d scope(s): %v", len(scopes), scopes), nil
		},
	}
}

// newEncryptionPostureCheck reports the resolved encryption mode per
// configured scope. The default is plaintext; operators who opt in
// see "encrypted" with a per-scope breakdown. Always StatusOK — this
// is informational, not a verdict. Lets `maz doctor` answer the
// question "what does my git host actually see?" at a glance.
func newEncryptionPostureCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.encryption_posture",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return StatusOK, "no remotes configured (encryption posture: n/a)", nil
			}
			names := make([]string, 0, len(resolved.Remotes))
			for n := range resolved.Remotes {
				names = append(names, n)
			}
			sort.Strings(names)
			parts := make([]string, 0, len(names))
			plain, enc := 0, 0
			for _, n := range names {
				r := resolved.Remotes[n]
				mode := "plaintext"
				if r.EncryptedOrDefault() {
					mode = "encrypted"
					enc++
				} else {
					plain++
				}
				parts = append(parts, fmt.Sprintf("%s=%s", n, mode))
			}
			summary := fmt.Sprintf("%d plaintext, %d encrypted — %v", plain, enc, parts)
			return StatusOK, summary, nil
		},
	}
}

// newWatchwordPostureCheck enforces the encryption invariant: a Watchword
// is required iff at least one configured scope is encrypted. The
// matrix:
//
//	any-encrypted=true,  watchword-present=true  → OK
//	any-encrypted=true,  watchword-present=false → FAIL (unrecoverable)
//	any-encrypted=false, watchword-present=true  → OK (kept for migration)
//	any-encrypted=false, watchword-present=false → OK (the new plaintext default)
func newWatchwordPostureCheck(dataDir string, chain secret.Provider) CheckFunc {
	return CheckFunc{
		N:   "sync.watchword_required_when_encrypted",
		Sub: subsystemSync,
		Fn: func(ctx context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			encryptedScopes := []string{}
			for n, r := range resolved.Remotes {
				if r.EncryptedOrDefault() {
					encryptedScopes = append(encryptedScopes, n)
				}
			}
			sort.Strings(encryptedScopes)
			present := true
			v, gErr := chain.Get(ctx, watchword.KeychainKey)
			switch {
			case errors.Is(gErr, secret.ErrNotFound):
				present = false
			case gErr != nil:
				return StatusFail, "", fmt.Errorf("keychain Get: %w", gErr)
			default:
				if vErr := watchword.Validate(v); vErr != nil {
					return StatusFail, "", fmt.Errorf("watchword fails BIP-39 validation: %w", vErr)
				}
			}
			switch {
			case len(encryptedScopes) > 0 && !present:
				return StatusFail, fmt.Sprintf(
					"scope(s) %v are encrypted but no Watchword in keychain — sealing will skip these scopes and unsealed events will pile up",
					encryptedScopes), nil
			case len(encryptedScopes) > 0:
				return StatusOK, fmt.Sprintf("Watchword present; %d encrypted scope(s): %v", len(encryptedScopes), encryptedScopes), nil
			case present:
				return StatusOK, "Watchword present (no encrypted scopes — kept for opt-in)", nil
			default:
				return StatusOK, "plaintext-only setup; Watchword not required", nil
			}
		},
	}
}

// loadResolvedSync merges UserConfig + UserState so doctor checks see
// the same view the daemon does at boot. ErrNotFound on either layer
// is normal (fresh install or no overrides yet) and yields an empty
// SyncTable; anything else is a real failure that should surface to
// the operator with full context.
func loadResolvedSync(dataDir string) (config.SyncTable, error) {
	user, err := config.LoadUserConfig(dataDir)
	if err != nil && !errors.Is(err, config.ErrUserConfigNotFound) {
		return config.SyncTable{}, fmt.Errorf("load config: %w", err)
	}
	state, sErr := config.LoadUserState(dataDir)
	if sErr != nil && !errors.Is(sErr, config.ErrUserStateNotFound) {
		return config.SyncTable{}, fmt.Errorf("load state: %w", sErr)
	}
	return config.Merge(user, state).Sync, nil
}

// newSealedChunksReadableCheck verifies the on-disk FileCursor JSONL
// files (one per scope) parse without surfacing torn lines past the
// loader's tolerance. A torn-tolerance failure would mean the daemon
// is silently dropping previously-sealed UIDs and would re-seal them
// next pass — defendable but worth surfacing.
func newSealedChunksReadableCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.sealed_chunks_readable",
		Sub: subsystemSync,
		Fn: func(ctx context.Context) (Status, string, error) {
			cursor, err := chunk.NewFileCursor(dataDir)
			if err != nil {
				return StatusFail, "", fmt.Errorf("open cursor: %w", err)
			}
			totals := map[kernel.Scope]int{}
			for _, scope := range []kernel.Scope{
				kernel.ScopePersonal, kernel.ScopeProject, kernel.ScopeTeam,
			} {
				set, sErr := cursor.Sealed(ctx, scope)
				if sErr != nil {
					return StatusFail, "", fmt.Errorf("scope=%s: %w", scope, sErr)
				}
				totals[scope] = len(set)
			}
			n := totals[kernel.ScopePersonal] + totals[kernel.ScopeProject] + totals[kernel.ScopeTeam]
			if n == 0 {
				return StatusOK, "no sealed chunks yet (daemon has not rotated a chunk)", nil
			}
			return StatusOK, fmt.Sprintf(
				"sealed chunk cursor readable (personal=%d project=%d team=%d)",
				totals[kernel.ScopePersonal], totals[kernel.ScopeProject], totals[kernel.ScopeTeam]), nil
		},
	}
}

// newGitBinaryCheck verifies the `git` binary is present in PATH.
// Soft-prerequisite: the git transport refuses to construct without
// it; doctor surfacing this lets the operator install before the
// daemon's first push attempt logs a confusing exec error.
func newGitBinaryCheck() CheckFunc {
	return CheckFunc{
		N:   "sync.git_binary",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			path, err := exec.LookPath("git")
			if err != nil {
				// Warn (not fail) because a single-device deployment without
				// sync remotes does not need git at all. The error here is
				// the expected `exec.LookPath` "not found"; surfacing it as
				// nil keeps doctor's err column dedicated to unexpected
				// failures.
				return StatusWarn, "git not in PATH; install git to enable sync transport", nil //nolint:nilerr
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				return StatusFail, "", fmt.Errorf("stat git: %w", statErr)
			}
			return StatusOK, fmt.Sprintf("git found at %s (%d bytes)", filepath.Clean(path), info.Size()), nil
		},
	}
}

// newSensitivePayloadPostureCheck FAILs when a plaintext scope already
// holds a chunk carrying a sensitive event. The #204 fail-closed doctor
// primitive: the seal-time gate prevents fresh sensitive chunks, so a hit
// here means a chunk pulled from a remote (another writer's leak) or one
// sealed before a privacy marker existed — a privacy incident the
// operator must act on (enable encryption, or remove the offending chunk
// and remediate the remote). Encrypted scopes are skipped (payloads are
// sealed); project is exempt (intentionally public). A runtime scan, not
// a static config check: until connectors land there is no "this scope
// carries sensitive payload" config signal, and the scan auto-covers
// every downstream marker without doctor changes.
func newSensitivePayloadPostureCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.sensitive_payload_requires_encryption",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return StatusOK, "no remotes configured (nothing pushable)", nil
			}
			names := make([]string, 0, len(resolved.Remotes))
			for n := range resolved.Remotes {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				r := resolved.Remotes[n]
				scope := kernel.Scope(n)
				// Skip names that are not recognized sync scopes before the
				// name reaches firstSensitiveChunk's chunk.DirFor join:
				// sync.remotes_configured already reports a typo'd/invalid
				// remote name, and an unvalidated key (e.g. one containing a
				// path separator) must never steer the scan outside the
				// sync/{project,personal,team}/chunks trees.
				if !scope.Valid() || r.EncryptedOrDefault() || scope == kernel.ScopeProject {
					continue
				}
				chunkID, uid, scanErr := firstSensitiveChunk(dataDir, scope)
				if scanErr != nil {
					return StatusFail, "", fmt.Errorf("scope=%s: %w", n, scanErr)
				}
				if chunkID != "" {
					return StatusFail, fmt.Sprintf(
						"scope=%s is PLAINTEXT but chunk %s carries a sensitive event (uid=%s) — enable encryption or unbind the sensitive source (markers: %v)",
						n, chunkID, uid, privacy.MarkerNames()), nil
				}
			}
			return StatusOK, "no plaintext scope holds a sensitive chunk", nil
		},
	}
}

// newConnectorScopeEncryptionCheck flags, at config-load time, the
// misconfiguration the runtime sensitive-payload scan structurally cannot:
// a scope that has ingested connector-sourced content (#211) but is configured
// to sync as PLAINTEXT. The seal-time gate (#204) refuses to write a connector
// chunk into a plaintext scope, so such content never reaches chunks/*.jsonl —
// which means newSensitivePayloadPostureCheck stays GREEN while the operator's
// connector data silently piles up unsealed and can never sync. This check
// reads the connector_watermarks projection (the durable signal that ingestion
// has run, and the scope it landed in) and FAILS when a watermark's scope is a
// configured plaintext, non-project remote. Encrypted scopes are safe; project
// is exempt (ingest refuses it) and never receives connector content.
func newConnectorScopeEncryptionCheck(dataDir string, s *sharedSQLite) CheckFunc {
	return CheckFunc{
		N:   "sync.connector_scope_requires_encryption",
		Sub: subsystemSync,
		Fn: func(ctx context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return StatusOK, "no remotes configured (connector content stays local)", nil
			}
			scopes, err := ingestedConnectorScopes(ctx, s)
			if err != nil {
				return StatusFail, "", err
			}
			if len(scopes) == 0 {
				return StatusOK, "no connector sources ingested", nil
			}
			plaintext := []string{}
			for _, scope := range scopes {
				r, ok := resolved.Remotes[scope]
				if !ok || r.EncryptedOrDefault() || kernel.Scope(scope) == kernel.ScopeProject {
					continue
				}
				plaintext = append(plaintext, scope)
			}
			sort.Strings(plaintext)
			if len(plaintext) > 0 {
				return StatusFail, fmt.Sprintf(
					"scope(s) %v hold connector-sourced content but are configured PLAINTEXT — the seal gate refuses to sync this content; enable encryption for these scopes (markers: %v)",
					plaintext, privacy.MarkerNames()), nil
			}
			return StatusOK, fmt.Sprintf("connector content confined to encrypted scope(s): %v", scopes), nil
		},
	}
}

// ingestedConnectorScopes returns the distinct scopes that have ingested
// connector-sourced content, read from the connector_watermarks projection.
// The shared store is opened via sqlite.Open, which runs every migration, so
// the table is always present (a fresh store has it empty, not absent).
func ingestedConnectorScopes(ctx context.Context, s *sharedSQLite) ([]string, error) {
	store, err := s.acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	rows, err := store.Reader().QueryContext(ctx, "SELECT DISTINCT scope FROM connector_watermarks ORDER BY scope")
	if err != nil {
		return nil, fmt.Errorf("query connector_watermarks: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "doctor: close connector_watermarks rows: %v\n", cerr)
		}
	}()
	scopes := []string{}
	for rows.Next() {
		var scope string
		if scanErr := rows.Scan(&scope); scanErr != nil {
			return nil, fmt.Errorf("scan scope: %w", scanErr)
		}
		scopes = append(scopes, scope)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate connector scopes: %w", rowsErr)
	}
	return scopes, nil
}

// firstSensitiveChunk walks a scope's plaintext chunk store and returns
// the chunk id + event uid of the first sensitive event found, or ("",
// "", nil) when the scope is clean. A missing chunks dir is clean.
func firstSensitiveChunk(dataDir string, scope kernel.Scope) (chunkID, uid string, err error) {
	dir := chunk.DirFor(dataDir, scope)
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("readdir %s: %w", dir, rerr)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != chunk.ExtensionJSONL {
			continue
		}
		path := filepath.Join(dir, e.Name())
		blob, readErr := os.ReadFile(path) //nolint:gosec // bounded by ReadDir of an internal dir
		if readErr != nil {
			return "", "", fmt.Errorf("read %s: %w", path, readErr)
		}
		hdr, events, derr := chunk.DecodePlaintext(blob, scope)
		if derr != nil {
			return "", "", fmt.Errorf("decode %s: %w", path, derr)
		}
		for i := range events {
			if privacy.IsSensitivePayload(events[i]) {
				return hdr.ChunkID, events[i].UID, nil
			}
		}
	}
	return "", "", nil
}
