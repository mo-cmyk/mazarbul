package ingest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Stream identifiers for LocalGitSource.
const (
	streamCommits      = "commits"
	streamPullRequests = "pull-requests"
)

// SourceType taxonomy values emitted onto each item's SourceRef.
const (
	sourceTypeGitCommit = "git_commit"
	sourceTypeGitPR     = "git_pull_request"
)

// connectorLocalGit is the LocalGitSource name; it tags Event.Source as
// "connector:local-git".
const connectorLocalGit = "local-git"

// gitRecordSep / gitFieldSep are the actual bytes that delimit `git log` records
// and fields in the OUTPUT (used for parsing). NUL and the ASCII record-separator
// never appear in a commit body in practice, so parsing is unambiguous without
// quoting. The gitFmtFieldSep / gitFmtRecordSep values are the matching
// `--format` placeholders: a literal NUL in an argv entry makes exec fail with
// "invalid argument", so the format string must ask git to EMIT the byte via
// %xNN rather than embedding the raw byte.
const (
	gitRecordSep    = "\x1e"
	gitFieldSep     = "\x00"
	gitFmtFieldSep  = "%x00"
	gitFmtRecordSep = "%x1e"
)

// LocalGitSource ingests a local git repository: commit messages (always) and,
// when a GitHub client is configured, pull-request prose. It shells out to git
// for commits (no go-git dependency) and uses the GitHub REST API for PRs.
type LocalGitSource struct {
	repoDir string
	github  *gitHubClient // nil => commits-only (no PR stream)
	now     func() time.Time
}

// NewLocalGitSource builds a LocalGitSource for repoDir. Github may be nil
// (commits-only). Now may be nil (defaults to time.Now).
func NewLocalGitSource(repoDir string, github *gitHubClient, now func() time.Time) *LocalGitSource {
	if now == nil {
		now = time.Now
	}
	return &LocalGitSource{repoDir: repoDir, github: github, now: now}
}

// Name implements Source.
func (s *LocalGitSource) Name() string { return connectorLocalGit }

// Streams implements Source: commits always, pull-requests only when a GitHub
// client is wired (an optional token from the secret chain enables it).
func (s *LocalGitSource) Streams() []string {
	if s.github != nil {
		return []string{streamCommits, streamPullRequests}
	}
	return []string{streamCommits}
}

// Validate implements Source: confirm repoDir is a git work tree without pulling.
func (s *LocalGitSource) Validate(ctx context.Context) error {
	if _, err := runGit(ctx, s.repoDir, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("ingest: %q is not a git repository: %w", s.repoDir, err)
	}
	return nil
}

// Pull implements Source.
func (s *LocalGitSource) Pull(ctx context.Context, stream string, wm Watermark, limit int) (Batch, error) {
	switch stream {
	case streamCommits:
		return s.pullCommits(ctx, wm, limit)
	case streamPullRequests:
		return s.pullPullRequests(ctx, wm, limit)
	default:
		return Batch{}, fmt.Errorf("ingest: unknown local-git stream %q", stream)
	}
}

// commitCursor is the opaque cursor JSON for the commits stream.
type commitCursor struct {
	LastSHA string `json:"lastSHA"`
}

// pullCommits reads commits after the cursor SHA in chronological order.
func (s *LocalGitSource) pullCommits(ctx context.Context, wm Watermark, limit int) (Batch, error) {
	var cur commitCursor
	if len(wm.Cursor) > 0 {
		if err := json.Unmarshal(wm.Cursor, &cur); err != nil {
			return Batch{}, fmt.Errorf("ingest: decode commit cursor: %w", err)
		}
	}

	if cur.LastSHA != "" {
		// The watermark is keyed by (connector, stream), not by repo path, so a
		// cursor minted against one repository is loaded when the connector is
		// later pointed at a DIFFERENT repo (or after a history rewrite dropped
		// the commit). The bare `<sha>..HEAD` range then fails with a cryptic
		// "Invalid revision range". Detect the missing commit up front and return
		// an actionable error instead — the local-git connector tracks a single
		// repository per connector in v1 (multiple sources are #212).
		if !s.commitExists(ctx, cur.LastSHA) {
			return Batch{}, fmt.Errorf(
				"ingest: watermark commit %s is not present in repository %s — the local-git connector tracks a single repository per connector; point it at the original repo (multiple sources land in #212)",
				cur.LastSHA, s.repoDir)
		}
	}

	// Two-phase pull bounds memory to O(limit) regardless of repo size, so a
	// fresh backfill over a 100k-commit repository never buffers 100k commit
	// BODIES at once. Phase 1 streams the new commit OIDs oldest-first and stops
	// after limit (+1 to detect "more remain"); phase 2 fetches the full records
	// for only that bounded SHA set.
	shas, more, err := s.newCommitSHAs(ctx, cur.LastSHA, limit)
	if err != nil {
		return Batch{}, err
	}
	var items []Item
	if len(shas) > 0 {
		items, err = s.logCommits(ctx, shas)
		if err != nil {
			return Batch{}, err
		}
	}
	return s.buildBatch(items, wm, !more, func() (json.RawMessage, error) {
		last := cur.LastSHA
		if len(items) > 0 {
			last = items[len(items)-1].ExternalID
		}
		return json.Marshal(commitCursor{LastSHA: last})
	})
}

// newCommitSHAs streams the OIDs of commits reachable from HEAD but after
// cursorSHA (or all of HEAD when cursorSHA is empty), oldest-first, returning at
// most limit of them plus whether more remain after that. It reads only SHAs
// (~40 bytes each) and stops after limit+1, so memory stays O(limit) however
// large the repository is.
//
// NB: do NOT use `git rev-list --reverse -n N`. Like `git log`, git applies the
// count BEFORE --reverse, so -n N yields the NEWEST N reversed — silently
// skipping the oldest commits and jumping the watermark to HEAD. We bound by
// reading exactly limit+1 records from the (correct) oldest-first stream.
func (s *LocalGitSource) newCommitSHAs(ctx context.Context, cursorSHA string, limit int) (shas []string, more bool, err error) {
	args := []string{"rev-list", "--reverse"}
	if cursorSHA != "" {
		args = append(args, cursorSHA+"..HEAD")
	} else {
		args = append(args, "HEAD")
	}
	st, err := runGitStream(ctx, s.repoDir, args...)
	if err != nil {
		return nil, false, fmt.Errorf("ingest: git rev-list: %w", err)
	}
	stoppedEarly := false
	sc := bufio.NewScanner(st.Stdout())
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // SHAs are 40 bytes; 1 MiB is ample headroom
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if limit > 0 && len(shas) == limit {
			more = true // the (limit+1)-th record proves more commits remain
			stoppedEarly = true
			break
		}
		shas = append(shas, line)
	}
	scanErr := sc.Err()
	closeErr := st.Close(stoppedEarly)
	if scanErr != nil {
		return nil, false, fmt.Errorf("ingest: read rev-list: %w", scanErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("ingest: git rev-list: %w", closeErr)
	}
	return shas, more, nil
}

// logCommits fetches the full record (sha, committer date, body) for each SHA,
// preserving the given oldest-first order. --no-walk=unsorted keeps the
// command-line order (plain --no-walk would re-sort by date); the body-bearing
// output is bounded to len(shas) commits, which the caller has already capped.
func (s *LocalGitSource) logCommits(ctx context.Context, shas []string) ([]Item, error) {
	args := make([]string, 0, 4+len(shas))
	args = append(args,
		"log", "--no-walk=unsorted", "--no-color",
		"--format=%H"+gitFmtFieldSep+"%cI"+gitFmtFieldSep+"%B"+gitFmtRecordSep,
	)
	args = append(args, shas...)
	out, err := runGit(ctx, s.repoDir, args...)
	if err != nil {
		return nil, fmt.Errorf("ingest: git log: %w", err)
	}
	return s.parseCommits(string(out)), nil
}

// commitExists reports whether sha resolves to a commit object in the repo.
// Used to distinguish "no new commits" from "this cursor belongs to a different
// repository" before the SHA is fed into a `<sha>..HEAD` revision range.
func (s *LocalGitSource) commitExists(ctx context.Context, sha string) bool {
	_, err := runGit(ctx, s.repoDir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// parseCommits turns `git log` output into Items. Each record is
// sha<FS>committerDateISO<FS>body, records separated by gitRecordSep.
func (s *LocalGitSource) parseCommits(out string) []Item {
	records := strings.Split(out, gitRecordSep)
	items := make([]Item, 0, len(records))
	for _, rec := range records {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, gitFieldSep, 3)
		if len(parts) != 3 {
			continue
		}
		sha, dateISO, body := parts[0], parts[1], parts[2]
		fetch := s.now().UTC()
		if t, terr := time.Parse(time.RFC3339, strings.TrimSpace(dateISO)); terr == nil {
			fetch = t.UTC()
		}
		items = append(items, Item{
			DedupKey:    "git-commit:" + sha,
			ExternalID:  sha,
			SourceType:  sourceTypeGitCommit,
			ContentHash: contentHash(body),
			Body:        body,
			FetchTime:   fetch,
		})
	}
	return items
}

// skipPRStream returns a terminal, no-advance Batch used when this repo has no
// GitHub PR stream (no client, no origin, or a non-GitHub origin). Done with a
// preserved cursor and unchanged sequence — the orchestrator records a clean
// zero-item stream with no error.
func (s *LocalGitSource) skipPRStream(wm Watermark) Batch {
	cursor := wm.Cursor
	if len(cursor) == 0 {
		cursor = json.RawMessage(`{}`)
	}
	return Batch{Done: true, NextCursor: cursor, NextSequence: wm.Sequence}
}

// pullPullRequests reads PRs after the cursor number via the GitHub REST API.
func (s *LocalGitSource) pullPullRequests(ctx context.Context, wm Watermark, limit int) (Batch, error) {
	if s.github == nil {
		return s.skipPRStream(wm), nil
	}
	// A repo with no 'origin' remote (the common local-only case) or a
	// non-GitHub origin simply has no PR stream — that is not an error. Skip it
	// cleanly so a successful commits-only ingest is not drowned in a WARN with
	// raw git output on every run. Genuine GitHub API failures below DO surface.
	remote, err := runGit(ctx, s.repoDir, "remote", "get-url", "origin")
	if err != nil {
		return s.skipPRStream(wm), nil //nolint:nilerr // no origin remote => no PR stream; skip cleanly, not an error
	}
	owner, repo, err := parseOwnerRepo(strings.TrimSpace(string(remote)))
	if err != nil {
		return s.skipPRStream(wm), nil //nolint:nilerr // non-github origin => no PR stream; skip cleanly (#212)
	}

	cur, err := decodePRCursor(wm.Cursor)
	if err != nil {
		return Batch{}, err
	}
	prs, resumePage, err := s.github.listPullsAfter(ctx, owner, repo, cur.LastPR, cur.Page, limit)
	if err != nil {
		return Batch{}, fmt.Errorf("ingest: list pulls: %w", err)
	}

	items := make([]Item, 0, len(prs))
	maxPR := cur.LastPR
	for _, pr := range prs {
		body := strings.TrimSpace(pr.Title + "\n\n" + pr.Body)
		fetch := s.now().UTC()
		if !pr.CreatedAt.IsZero() {
			fetch = pr.CreatedAt.UTC()
		}
		items = append(items, Item{
			DedupKey:    fmt.Sprintf("git-pr:%s/%s#%d", owner, repo, pr.Number),
			ExternalID:  prExternalID(pr.Number),
			ExternalURL: pr.HTMLURL,
			SourceType:  sourceTypeGitPR,
			ContentHash: contentHash(body),
			Body:        body,
			FetchTime:   fetch,
		})
		if pr.Number > maxPR {
			maxPR = pr.Number
		}
	}
	return s.buildBatch(items, wm, len(items) < limit, func() (json.RawMessage, error) {
		return encodePRCursor(maxPR, resumePage)
	})
}

// buildBatch assembles a Batch with a well-formed advanced cursor. EncodeCursor
// produces the next opaque cursor; when there are no items the sequence and
// cursor stay put (a terminal no-advance the orchestrator accepts as Done). Done
// is supplied by the caller because the two streams compute exhaustion
// differently (commits slice an over-fetched list; PRs page up to limit).
func (s *LocalGitSource) buildBatch(items []Item, wm Watermark, done bool, encodeCursor func() (json.RawMessage, error)) (Batch, error) {
	cursor, err := encodeCursor()
	if err != nil {
		return Batch{}, err
	}
	next := wm.Sequence + uint64(len(items))
	return Batch{
		Items:        items,
		NextCursor:   cursor,
		NextSequence: next,
		Done:         done,
	}, nil
}

// contentHash is the lower-hex SHA-256 of body — the change-detection key,
// matching the vault importer's hashHex convention.
func contentHash(body string) string {
	return contentHashBytes([]byte(body))
}

// contentHashBytes is contentHash over raw bytes. It avoids the string copy a
// caller would otherwise pay to satisfy contentHash, which matters for the
// claude-chats export buffer (bounded at maxConversationsJSONBytes, up to
// 512 MiB) that is hashed whole.
func contentHashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
