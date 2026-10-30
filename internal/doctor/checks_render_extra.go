package doctor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// renderTombstoneWarnRatio is the soft threshold above which the
// per-scope relations.jsonl is considered fragmented enough to
// warrant a compaction sweep. 50% matches the plan budget: the
// renderer's tombstone-and-compact strategy targets <10% steady
// state, so 50% means at least 5× growth in tombstone debris and
// the next push will pay for it.
//
//nolint:gochecknoglobals // tunable knob, intentionally package-level.
var renderTombstoneWarnRatio = 0.50

// renderCursorBehindWarnEvents is the per-scope cursor-lag tolerance
// in events. 100 events is the plan's stated soft ceiling: the
// renderer should keep pace with each push; falling >100 events
// behind suggests the renderer crashed mid-push or the operator
// killed the daemon between chunk write and rendered/.cursor.json
// write.
//
//nolint:gochecknoglobals // tunable knob, intentionally package-level.
var renderCursorBehindWarnEvents = 100

// scopeMetric is the result of one per-scope render-side scan.
// Used by findWorstPlaintextScope to converge on the single worst
// offender, which the doctor row then names in the verdict string.
type scopeMetric struct {
	score float64
	found bool
}

// findWorstPlaintextScope walks every configured plaintext scope,
// runs probe(name, dataDir) per scope, and returns the scope name
// that scored highest along with the score and the count of scopes
// where probe.found was true. Centralises the per-scope iteration
// the three render-side checks used to inline (and tripped
// gocognit on). Encrypted scopes are skipped per the render contract
// that no rendered tree exists for them.
func findWorstPlaintextScope(
	dataDir string,
	probe func(scope, dataDir string) (scopeMetric, error),
) (worstScope string, worstScore float64, seen int, err error) {
	resolved, lerr := loadResolvedSync(dataDir)
	if lerr != nil {
		return "", 0, 0, lerr
	}
	for name, r := range resolved.Remotes {
		if r.EncryptedOrDefault() {
			continue
		}
		m, perr := probe(name, dataDir)
		if perr != nil {
			return "", 0, 0, fmt.Errorf("scope=%s: %w", name, perr)
		}
		if !m.found {
			continue
		}
		seen++
		if m.score > worstScore {
			worstScore = m.score
			worstScope = name
		}
	}
	return worstScope, worstScore, seen, nil
}

// noRemotesVerdict returns the canonical OK row when no remotes are
// configured. Mirrors the shape used by the other sync checks so
// the doctor table stays self-consistent.
func noRemotesVerdict(suffix string) (Status, string, error) {
	return StatusOK, "no remotes configured (" + suffix + ")", nil
}

// newRenderTombstoneRatioCheck inspects each plaintext scope's
// rendered/relations.jsonl and reports how much of the file is
// tombstones (relations marked superseded or forgotten). High
// tombstone ratios mean the working tree carries dead-edge bloat
// the renderer would prune on its next compaction sweep — useful
// to surface so the operator knows whether a slow push is the
// renderer doing work or something else.
//
// Counts lines with `"superseded_by":"...` or `"forgotten":true`
// over the total non-blank line count. Cheap O(file_size) scan via
// bufio.Scanner; no JSON decode.
func newRenderTombstoneRatioCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.render_tombstone_ratio",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return noRemotesVerdict("tombstone ratio: n/a")
			}
			worstScope, worstRatio, seen, ferr := findWorstPlaintextScope(dataDir,
				func(scope, dir string) (scopeMetric, error) {
					path := filepath.Join(dir, "sync", scope, "repo", "rendered", "relations.jsonl")
					ratio, found, scanErr := scanTombstoneRatio(path)
					return scopeMetric{score: ratio, found: found}, scanErr
				})
			if ferr != nil {
				return StatusFail, "", ferr
			}
			if seen == 0 {
				return StatusOK, "no relations.jsonl on disk yet", nil
			}
			if worstRatio > renderTombstoneWarnRatio {
				return StatusWarn, fmt.Sprintf(
					"scope=%s relations.jsonl is %.0f%% tombstones (>%.0f%% warn threshold); compaction will reclaim this on next push",
					worstScope, worstRatio*100, renderTombstoneWarnRatio*100), nil
			}
			return StatusOK, fmt.Sprintf("tombstone ratios within budget (worst: %s @ %.0f%%)",
				worstScope, worstRatio*100), nil
		},
	}
}

// scanTombstoneRatio walks one relations.jsonl file and returns the
// ratio of tombstoned lines to total. Bumps a generous bufio
// Scanner buffer because a single relation line can hold a long
// attributes map. Reports found=false when the file is missing.
func scanTombstoneRatio(path string) (ratio float64, found bool, err error) {
	f, oerr := os.Open(path) //nolint:gosec // path is constructed from doctor-internal dataDir + fixed scope name + literal filename
	if os.IsNotExist(oerr) {
		return 0, false, nil
	}
	if oerr != nil {
		return 0, false, fmt.Errorf("open %s: %w", path, oerr)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", path, cerr)
		}
	}()
	const maxLine = 1 << 20 // 1 MiB per line — generous for relations.jsonl
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLine)
	var total, tomb int
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		total++
		if isTombstoneLine(line) {
			tomb++
		}
	}
	if serr := scanner.Err(); serr != nil {
		return 0, false, fmt.Errorf("scan %s: %w", path, serr)
	}
	if total == 0 {
		return 0, true, nil
	}
	return float64(tomb) / float64(total), true, nil
}

// isTombstoneLine reports whether a JSONL row encodes a
// superseded or forgotten relation. Cheap byte-scan rather than a
// full JSON decode — the field tokens are unambiguous in the
// renderer's stable key order.
func isTombstoneLine(line []byte) bool {
	return bytesContains(line, []byte(`"superseded_by":`)) ||
		bytesContains(line, []byte(`"forgotten":true`))
}

// newRenderCursorBehindCheck reports whether the per-scope
// rendered/.cursor.json head ULID has fallen behind the
// MANIFEST.json's last-pushed ULID by more than the warn threshold.
// A renderer that crashed mid-push (the chunk lands, the rendered
// reconcile partially fails, .cursor.json is not advanced) is the
// canonical failure mode that produces a stale rendered tree on
// github.com — this check surfaces it cheaply.
func newRenderCursorBehindCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.render_cursor_behind",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return noRemotesVerdict("cursor lag: n/a")
			}
			worstScope, worstScore, seen, ferr := findWorstPlaintextScope(dataDir,
				func(scope, dir string) (scopeMetric, error) {
					delta, found, scanErr := scanCursorLag(dir, scope)
					return scopeMetric{score: float64(delta), found: found}, scanErr
				})
			if ferr != nil {
				return StatusFail, "", ferr
			}
			if seen == 0 {
				return StatusOK, "no rendered cursor yet (daemon has not pushed)", nil
			}
			worstDeltaN := int(worstScore)
			if worstDeltaN > renderCursorBehindWarnEvents {
				return StatusWarn, fmt.Sprintf(
					"scope=%s rendered cursor is %d events behind MANIFEST head (>%d warn threshold); the renderer may have crashed mid-push",
					worstScope, worstDeltaN, renderCursorBehindWarnEvents), nil
			}
			return StatusOK, fmt.Sprintf("rendered cursors keeping pace (worst lag: %s @ %d events)",
				worstScope, worstDeltaN), nil
		},
	}
}

// scanCursorLag returns the lag (in event count) of the per-scope
// rendered/.cursor.json behind the per-scope chunks MANIFEST.json.
// "Lag" here is an upper bound — we count how many committed chunk
// headers in MANIFEST sit STRICTLY AFTER the cursor's lastUlid,
// summed by their EventCount field. A missing .cursor.json or
// MANIFEST is "not found" (no opinion).
func scanCursorLag(dataDir, scope string) (delta int, found bool, err error) {
	cursorPath := filepath.Join(dataDir, "sync", scope, "repo", "rendered", ".cursor.json")
	manifestPath := filepath.Join(dataDir, "sync", scope, "repo", "MANIFEST.json")

	var cursor struct {
		LastUlid string `json:"lastUlid"`
	}
	cBytes, cerr := os.ReadFile(cursorPath) //nolint:gosec // internal dir
	if os.IsNotExist(cerr) {
		return 0, false, nil
	}
	if cerr != nil {
		return 0, false, fmt.Errorf("read %s: %w", cursorPath, cerr)
	}
	if err := json.Unmarshal(cBytes, &cursor); err != nil {
		return 0, false, fmt.Errorf("parse %s: %w", cursorPath, err)
	}

	mBytes, merr := os.ReadFile(manifestPath) //nolint:gosec // internal dir
	if os.IsNotExist(merr) {
		return 0, false, nil
	}
	if merr != nil {
		return 0, false, fmt.Errorf("read %s: %w", manifestPath, merr)
	}
	var manifest struct {
		Chunks []struct {
			LastUlid   string `json:"lastUlid"`
			EventCount int    `json:"eventCount"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal(mBytes, &manifest); err != nil {
		return 0, false, fmt.Errorf("parse %s: %w", manifestPath, err)
	}
	for _, ch := range manifest.Chunks {
		if ch.LastUlid > cursor.LastUlid {
			delta += ch.EventCount
		}
	}
	return delta, true, nil
}

// newRelationCandidatesStuckCheck scans the per-scope chunks
// directory's plaintext JSONL frames looking for
// relationCandidatesProposed events that have not yet been promoted
// to relations or rejected via supersede. A growing pool of
// unpromoted candidates points at a wedged resolver pipeline (most
// commonly: Ollama crashed but the daemon kept appending; the
// extractor proposed candidates the resolver never consumed).
//
// We deliberately walk only the most recent N chunks (latestChunks
// = 5) to keep the scan cheap — a stuck pipeline shows up there
// first, and a deep historical scan would risk false positives
// from pre-resolver-fix data.
func newRelationCandidatesStuckCheck(dataDir string) CheckFunc {
	return CheckFunc{
		N:   "sync.relation_candidates_stuck",
		Sub: subsystemSync,
		Fn: func(_ context.Context) (Status, string, error) {
			resolved, err := loadResolvedSync(dataDir)
			if err != nil {
				return StatusFail, "", err
			}
			if len(resolved.Remotes) == 0 {
				return noRemotesVerdict("resolver pipeline: n/a")
			}
			worstScope, worstScore, seen, ferr := findWorstPlaintextScope(dataDir,
				func(scope, dir string) (scopeMetric, error) {
					count, found, scanErr := scanStuckCandidates(dir, scope)
					return scopeMetric{score: float64(count), found: found}, scanErr
				})
			if ferr != nil {
				return StatusFail, "", ferr
			}
			if seen == 0 {
				return StatusOK, "no chunks on disk yet", nil
			}
			worstCount := int(worstScore)
			if worstCount > 0 {
				return StatusWarn, fmt.Sprintf(
					"scope=%s has %d unresolved relationCandidatesProposed events; resolver may be stuck (check Ollama health, then re-run extraction)",
					worstScope, worstCount), nil
			}
			return StatusOK, "no stuck relation candidates across scopes", nil
		},
	}
}

// scanStuckCandidates walks the per-scope plaintext chunks dir and reports
// whether the resolver pipeline appears stuck. A pipeline is considered stuck
// when the most-recent chunk that contains a relationCandidatesProposed event
// is NOT followed (in that same chunk or any later chunk) by any relation or
// relationForgotten event — meaning the resolver stopped consuming new
// proposals.
//
// Chunks are sorted lexicographically by filename. Because chunk files are
// named with ULIDs, lexicographic order equals chronological order. The
// "hasResolved" signal is therefore scoped to recent activity: we find the
// index of the last chunk that contains candidates and then check whether any
// resolutions appear from that chunk onward. A store where the resolver ran
// once (old chunk has a relation) but has since stalled (new chunk has only
// candidates) will correctly report stuck.
//
// Previous versions compared event UIDs directly (incorrect: relation events
// carry freshly-minted UIDs, not the candidate UID) and later checked for
// resolutions anywhere in the scope (incorrect: old resolutions masked new
// stalls). Both false-positive paths are closed by this implementation.
//
// Encrypted chunks are skipped (we cannot decode them here). Missing
// directory returns found=false.
func scanStuckCandidates(dataDir, scope string) (count int, found bool, err error) {
	chunksDir := filepath.Join(dataDir, "sync", scope, "repo", "chunks")
	entries, rderr := os.ReadDir(chunksDir)
	if os.IsNotExist(rderr) {
		return 0, false, nil
	}
	if rderr != nil {
		return 0, false, fmt.Errorf("readdir %s: %w", chunksDir, rderr)
	}

	// Collect JSONL filenames in sorted (chronological) order.
	var names []string
	for _, e := range entries {
		if !e.IsDir() && hasJSONLSuffix(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	// Scan each chunk, recording per-chunk presence of candidates and
	// resolutions.
	type chunkResult struct {
		hasCandidates bool
		hasResolved   bool
	}
	results := make([]chunkResult, 0, len(names))
	for _, name := range names {
		path := filepath.Join(chunksDir, name)
		cands, resolved, scanErr := scanChunkForCandidatePresence(path)
		if scanErr != nil {
			return 0, false, scanErr
		}
		results = append(results, chunkResult{cands, resolved})
	}

	// Find the index of the last chunk that contains candidates.
	lastCandIdx := -1
	for i, r := range results {
		if r.hasCandidates {
			lastCandIdx = i
		}
	}
	if lastCandIdx == -1 {
		// No candidates anywhere — pipeline is idle, not stuck.
		return 0, false, nil
	}

	// For the last-candidate chunk, check whether a resolution appears AFTER
	// the final candidate in that chunk. A chunk shaped as [relation,
	// candidatesProposed] has hasResolved=true but the resolution precedes
	// the new candidates, so it must not clear the warning.
	lastChunkPath := filepath.Join(chunksDir, names[lastCandIdx])
	resolvedAfter, raErr := scanChunkResolvedAfterLastCandidate(lastChunkPath)
	if raErr != nil {
		return 0, false, raErr
	}
	if resolvedAfter {
		return 0, true, nil
	}
	// Check subsequent chunks for any resolution event: if the resolver
	// is still running it will emit into a later chunk.
	for _, r := range results[lastCandIdx+1:] {
		if r.hasResolved {
			return 0, true, nil
		}
	}
	// Candidates present in the most-recent activity window but no
	// resolutions followed — resolver is stuck.
	return 1, true, nil
}

// scanChunkForCandidatePresence streams one plaintext JSONL chunk and reports
// whether it contains any relationCandidatesProposed events (hasCandidates)
// and any relation or relationForgotten events (hasResolved). Stays at the
// byte level for performance; the kind discriminator tokens are stable in the
// kernel's payload encoding.
func scanChunkForCandidatePresence(path string) (hasCandidates, hasResolved bool, err error) {
	f, oerr := os.Open(path) //nolint:gosec // path is constructed from doctor-internal dataDir + fixed scope name + a chunks/ entry already filtered through ReadDir
	if oerr != nil {
		return false, false, fmt.Errorf("open %s: %w", path, oerr)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", path, cerr)
		}
	}()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20) // up to 4 MiB per line — generous for chunk events
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		switch {
		case bytesContains(line, []byte(`"kind":"relationCandidatesProposed"`)):
			hasCandidates = true
		case bytesContains(line, []byte(`"kind":"relation"`)),
			bytesContains(line, []byte(`"kind":"relationForgotten"`)):
			hasResolved = true
		}
		// Short-circuit once both signals are found.
		if hasCandidates && hasResolved {
			return true, true, nil
		}
	}
	if serr := scanner.Err(); serr != nil {
		return false, false, fmt.Errorf("scan %s: %w", path, serr)
	}
	return hasCandidates, hasResolved, nil
}

// scanChunkResolvedAfterLastCandidate reports whether a relation or
// relationForgotten event appears AFTER the final relationCandidatesProposed
// event in the chunk. This is stricter than the presence-only check: a chunk
// shaped as [relation, candidatesProposed] has a resolution but it precedes the
// new candidates, so the resolver has not yet consumed those candidates.
func scanChunkResolvedAfterLastCandidate(path string) (resolvedAfterLastCandidate bool, err error) {
	f, oerr := os.Open(path) //nolint:gosec // path is constructed from doctor-internal dataDir + fixed scope name + a chunks/ entry already filtered through ReadDir
	if oerr != nil {
		return false, fmt.Errorf("open %s: %w", path, oerr)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", path, cerr)
		}
	}()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	// Track whether we have seen a candidate since the last resolution reset.
	// seenCandidateSinceReset flips true when we see a candidate; any
	// subsequent resolution sets resolvedAfterLastCandidate=true. When we see
	// another candidate we reset the resolution flag so only the last trailing
	// [candidate...resolution] window counts.
	var pendingCandidate bool
	var resolvedAfterCandidate bool
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		switch {
		case bytesContains(line, []byte(`"kind":"relationCandidatesProposed"`)):
			// New candidate: any prior resolution is now before this
			// candidate, so it no longer satisfies the "after" condition.
			pendingCandidate = true
			resolvedAfterCandidate = false
		case bytesContains(line, []byte(`"kind":"relation"`)),
			bytesContains(line, []byte(`"kind":"relationForgotten"`)):
			if pendingCandidate {
				resolvedAfterCandidate = true
			}
		}
	}
	if serr := scanner.Err(); serr != nil {
		return false, fmt.Errorf("scan %s: %w", path, serr)
	}
	return resolvedAfterCandidate, nil
}

// hasJSONLSuffix returns true when name ends in ".jsonl". Used to
// skip encrypted chunks and directory entries.
func hasJSONLSuffix(name string) bool {
	const ext = ".jsonl"
	return len(name) > len(ext) && name[len(name)-len(ext):] == ext
}

// bytesContains/bytesIndex are local helpers so this file does not pull in
// the bytes package just for two Index calls. The doctor package's other files
// use idiomatic stdlib but these are the byte-scan inner loop; keeping the
// helpers small and inlinable mattered when the sync scale stress benches were
// measured.
func bytesContains(haystack, needle []byte) bool {
	return bytesIndex(haystack, needle) >= 0
}

func bytesIndex(haystack, needle []byte) int {
	n := len(needle)
	if n == 0 {
		return 0
	}
	end := len(haystack) - n
	for i := 0; i <= end; i++ {
		match := true
		for j := range n {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
