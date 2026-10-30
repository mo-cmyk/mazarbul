package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// initRepo creates a fresh git repo in a temp dir using a scrubbed environment
// (so a leaked GIT_DIR cannot corrupt the developer's real repository — see the
// PR #146 hazard) and returns its path.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := runGit(context.Background(), dir, "init", "-b", "main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	return dir
}

// commit makes an empty commit with the given message and returns its sha.
//
// The message is delivered via -F <file>, not -m <arg>: a single command-line
// argument is capped at MAX_ARG_STRLEN (128 KiB) on Linux, so an oversized body
// (e.g. the maxEpisodeBodyBytes*2 cap test) fails with E2BIG there while passing
// on macOS, which has no such per-argument limit. Reading from a file has no
// length limit, and git applies the same `whitespace` cleanup to -F and -m when
// the message is not edited, so behaviour is unchanged for every caller.
func commit(t *testing.T, dir, message string) string {
	t.Helper()
	msgFile := filepath.Join(t.TempDir(), "commit-msg.txt")
	if err := os.WriteFile(msgFile, []byte(message), 0o600); err != nil {
		t.Fatalf("write commit message: %v", err)
	}
	_, err := runGit(context.Background(), dir,
		"-c", "user.email=test@example.com",
		"-c", "user.name=Test",
		"-c", "commit.gpgsign=false",
		"commit", "--allow-empty", "-F", msgFile)
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}
	out, err := runGit(context.Background(), dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestLocalGitSource_Validate(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	src := NewLocalGitSource(dir, nil, nil)
	if err := src.Validate(context.Background()); err != nil {
		t.Fatalf("Validate on a real repo: %v", err)
	}

	notRepo := t.TempDir()
	bad := NewLocalGitSource(notRepo, nil, nil)
	if err := bad.Validate(context.Background()); err == nil {
		t.Fatal("Validate on a non-repo: want error, got nil")
	}
}

func TestLocalGitSource_StreamsDependsOnGitHubClient(t *testing.T) {
	t.Parallel()
	commitsOnly := NewLocalGitSource("/tmp", nil, nil)
	if got := commitsOnly.Streams(); len(got) != 1 || got[0] != streamCommits {
		t.Fatalf("commits-only streams = %v, want [%s]", got, streamCommits)
	}
	withPR := NewLocalGitSource("/tmp", newGitHubClient("", "", nil), nil)
	if got := withPR.Streams(); len(got) != 2 {
		t.Fatalf("with-github streams = %v, want commits + pull-requests", got)
	}
}

func TestLocalGitSource_PullCommitsChronologicalAndCursor(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	sha1 := commit(t, dir, "first commit\n\nbody one")
	sha2 := commit(t, dir, "second commit\n\nbody two")

	src := NewLocalGitSource(dir, nil, nil)
	batch, err := src.Pull(context.Background(), streamCommits, Watermark{}, 10)
	if err != nil {
		t.Fatalf("Pull commits: %v", err)
	}
	if len(batch.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(batch.Items))
	}
	// --reverse => chronological order (oldest first).
	if batch.Items[0].ExternalID != sha1 || batch.Items[1].ExternalID != sha2 {
		t.Errorf("order = [%s,%s], want [%s,%s]",
			batch.Items[0].ExternalID, batch.Items[1].ExternalID, sha1, sha2)
	}
	if !strings.Contains(batch.Items[0].Body, "first commit") {
		t.Errorf("body = %q, want commit message", batch.Items[0].Body)
	}
	if batch.Items[0].DedupKey != "git-commit:"+sha1 {
		t.Errorf("dedupKey = %q, want git-commit:%s", batch.Items[0].DedupKey, sha1)
	}
	if batch.Items[0].SourceType != sourceTypeGitCommit {
		t.Errorf("sourceType = %q, want %s", batch.Items[0].SourceType, sourceTypeGitCommit)
	}
	if batch.Items[0].ContentHash == "" || batch.Items[0].ContentHash == batch.Items[1].ContentHash {
		t.Errorf("content hashes should be present and distinct")
	}
	if !batch.Done {
		t.Errorf("Done = false, want true (2 items < limit 10)")
	}
	if batch.NextSequence != 2 {
		t.Errorf("NextSequence = %d, want 2", batch.NextSequence)
	}

	// The advanced cursor must point at the last sha so a delta pull is empty.
	var cur commitCursor
	if err := json.Unmarshal(batch.NextCursor, &cur); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if cur.LastSHA != sha2 {
		t.Errorf("cursor.LastSHA = %q, want %q", cur.LastSHA, sha2)
	}
}

func TestLocalGitSource_DeltaPullAfterCursorIsEmpty(t *testing.T) {
	t.Parallel()
	dir := initRepo(t)
	_ = commit(t, dir, "only commit")

	src := NewLocalGitSource(dir, nil, nil)
	first, err := src.Pull(context.Background(), streamCommits, Watermark{}, 10)
	if err != nil {
		t.Fatalf("first pull: %v", err)
	}
	// Re-pull from the advanced watermark: no new commits.
	wm := Watermark{Connector: connectorLocalGit, Stream: streamCommits, Cursor: first.NextCursor, Sequence: first.NextSequence}
	second, err := src.Pull(context.Background(), streamCommits, wm, 10)
	if err != nil {
		t.Fatalf("delta pull: %v", err)
	}
	if len(second.Items) != 0 {
		t.Fatalf("delta items = %d, want 0", len(second.Items))
	}
	if !second.Done {
		t.Errorf("delta Done = false, want true")
	}
	if second.NextSequence != first.NextSequence {
		t.Errorf("delta NextSequence = %d, want unchanged %d", second.NextSequence, first.NextSequence)
	}
}

func TestLocalGitSource_UnknownStreamErrors(t *testing.T) {
	t.Parallel()
	src := NewLocalGitSource(initRepo(t), nil, nil)
	if _, err := src.Pull(context.Background(), "branches", Watermark{}, 10); err == nil {
		t.Fatal("Pull unknown stream: want error, got nil")
	}
}

// TestLocalGitSource_PaginatesAllCommitsAcrossSlices is the regression guard for
// the `git log --reverse -n` gotcha: with more commits than the slice size, every
// commit must be ingested oldest-first across slices — none dropped. Before the
// fix, `-n` selected the NEWEST slice and reversed it, stranding older commits.
func TestLocalGitSource_PaginatesAllCommitsAcrossSlices(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := initRepo(t)
	const n = 7
	for i := range n {
		_ = commit(t, dir, fmt.Sprintf("commit body %d", i))
	}

	app := &fakeAppender{}
	o := NewOrchestrator(app, newFakeReader(), jobs.NewRegistry(), nil)
	report, err := o.Run(ctx, NewLocalGitSource(dir, nil, nil), kernel.ScopePersonal,
		Bounds{SliceSize: 3}, false) // slice 3 < 7 -> forces multi-slice pagination
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.ItemsApplied != n {
		t.Fatalf("applied = %d, want %d (oldest commits must not be dropped)", report.ItemsApplied, n)
	}
	eps := app.eventsOfKind(ontology.KindEpisode)
	if len(eps) != n {
		t.Fatalf("episodes = %d, want %d", len(eps), n)
	}
	// Episodes are appended in ingestion order; assert oldest-first 0..n-1.
	for i := range eps {
		marker := fmt.Sprintf("commit body %d", i)
		if !strings.Contains(string(eps[i].Payload), marker) {
			t.Errorf("episode %d does not contain %q; pagination order wrong", i, marker)
		}
	}
}

// TestLocalGitSource_CursorFromOtherRepoErrorsClearly pins the single-repo-per-
// connector edge: when a watermark cursor (a commit SHA) does not exist in the
// repository being pulled — because the connector was pointed at a different
// repo, or history was rewritten — Pull returns an actionable error naming the
// constraint, not a cryptic "Invalid revision range" from git.
func TestLocalGitSource_CursorFromOtherRepoErrorsClearly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Repo A: produce a real cursor SHA.
	dirA := initRepo(t)
	shaA := commit(t, dirA, "repo A commit")

	// Repo B: a different repo that does NOT contain shaA.
	dirB := initRepo(t)
	_ = commit(t, dirB, "repo B commit")

	cursor, err := json.Marshal(commitCursor{LastSHA: shaA})
	if err != nil {
		t.Fatalf("marshal cursor: %v", err)
	}
	srcB := NewLocalGitSource(dirB, nil, nil)
	_, err = srcB.Pull(ctx, streamCommits, Watermark{Cursor: cursor}, 10)
	if err == nil {
		t.Fatal("Pull with a foreign cursor: want error, got nil")
	}
	if !strings.Contains(err.Error(), "single repository per connector") {
		t.Errorf("error = %q, want the single-repo-per-connector explanation", err)
	}
	if !strings.Contains(err.Error(), shaA) {
		t.Errorf("error = %q, want it to name the missing commit %s", err, shaA)
	}
}

func TestContentHash_StableAndDistinct(t *testing.T) {
	t.Parallel()
	a1 := contentHash("alpha")
	a2 := contentHash("alpha")
	b := contentHash("beta")
	if a1 != a2 {
		t.Errorf("hash not stable: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Error("distinct inputs hashed to the same value")
	}
	if len(a1) != 64 {
		t.Errorf("hash len = %d, want 64 (sha-256 hex)", len(a1))
	}
}
