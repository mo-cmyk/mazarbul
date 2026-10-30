package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// repoWithOrigin builds a real git repo (so commit/remote shell-outs work) whose
// origin points at a github.com URL, so parseOwnerRepo yields owner/repo while
// the HTTP calls are redirected to the injected httptest baseURL.
func repoWithOrigin(t *testing.T, originURL string) string {
	t.Helper()
	dir := initRepo(t)
	_ = commit(t, dir, "seed commit")
	if _, err := runGit(context.Background(), dir, "remote", "add", "origin", originURL); err != nil {
		t.Fatalf("git remote add origin: %v", err)
	}
	return dir
}

// prServer returns an httptest server that serves a fixed pulls page.
func prServer(t *testing.T, prs []gitHubPR) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/repos/octo/widgets/pulls") {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		// Only the first page carries data; later pages are empty (exhausted).
		if r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode(prs)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestLocalGitSource_PullPullRequestsMapsProse pins the PR-prose mapping the
// plan's PR corpus targeted: a PR's title+body becomes the Item body, with the
// owner/repo#number dedup key, the git_pull_request source type, an html_url,
// and a monotone lastPR cursor.
func TestLocalGitSource_PullPullRequestsMapsProse(t *testing.T) {
	t.Parallel()
	srv := prServer(t, []gitHubPR{
		{Number: 1, Title: "Add gateway", Body: "Implements the payment gateway.", HTMLURL: "https://github.com/octo/widgets/pull/1"},
		{Number: 2, Title: "Wire metrics", Body: "Adds metrics.", HTMLURL: "https://github.com/octo/widgets/pull/2"},
	})
	dir := repoWithOrigin(t, "https://github.com/octo/widgets.git")
	src := NewLocalGitSource(dir, newGitHubClient(srv.URL, "", srv.Client()), nil)

	batch, err := src.Pull(context.Background(), streamPullRequests, Watermark{}, 10)
	if err != nil {
		t.Fatalf("Pull pull-requests: %v", err)
	}
	if len(batch.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(batch.Items))
	}
	it := batch.Items[0]
	if it.DedupKey != "git-pr:octo/widgets#1" {
		t.Errorf("dedupKey = %q, want git-pr:octo/widgets#1", it.DedupKey)
	}
	if it.SourceType != sourceTypeGitPR {
		t.Errorf("sourceType = %q, want %s", it.SourceType, sourceTypeGitPR)
	}
	if it.ExternalID != "pr-1" {
		t.Errorf("externalID = %q, want pr-1", it.ExternalID)
	}
	if it.ExternalURL != "https://github.com/octo/widgets/pull/1" {
		t.Errorf("externalURL = %q, want the html_url", it.ExternalURL)
	}
	if !strings.Contains(it.Body, "Add gateway") || !strings.Contains(it.Body, "payment gateway") {
		t.Errorf("body = %q, want title+body prose", it.Body)
	}
	if it.ContentHash == "" {
		t.Error("contentHash empty")
	}
	var cur prCursor
	if err := json.Unmarshal(batch.NextCursor, &cur); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if cur.LastPR != 2 {
		t.Errorf("cursor.LastPR = %d, want 2", cur.LastPR)
	}
}

// TestLocalGitSource_PullPullRequestsCursorSkipsSeen confirms the lastPR cursor
// makes a delta pull return only PRs created after the watermark.
func TestLocalGitSource_PullPullRequestsCursorSkipsSeen(t *testing.T) {
	t.Parallel()
	srv := prServer(t, []gitHubPR{
		{Number: 1, Title: "old", Body: "seen"},
		{Number: 2, Title: "new", Body: "fresh"},
	})
	dir := repoWithOrigin(t, "https://github.com/octo/widgets.git")
	src := NewLocalGitSource(dir, newGitHubClient(srv.URL, "", srv.Client()), nil)

	cursor, err := encodePRCursor(1, 0)
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}
	batch, err := src.Pull(context.Background(), streamPullRequests, Watermark{Cursor: cursor}, 10)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(batch.Items) != 1 || batch.Items[0].DedupKey != "git-pr:octo/widgets#2" {
		t.Fatalf("items = %+v, want only PR #2 after cursor", batch.Items)
	}
}

// TestLocalGitSource_PullRequestsNoOriginSkipsCleanly pins the common
// local-only case: a repo with NO 'origin' remote has no PR stream, so the pull
// must skip cleanly (Done, no error, no items) rather than returning an error
// that surfaces as a scary WARN with raw git output on every ingest.
func TestLocalGitSource_PullRequestsNoOriginSkipsCleanly(t *testing.T) {
	t.Parallel()
	dir := initRepo(t) // no origin remote
	_ = commit(t, dir, "local only")
	src := NewLocalGitSource(dir, newGitHubClient("", "", nil), nil)

	batch, err := src.Pull(context.Background(), streamPullRequests, Watermark{}, 10)
	if err != nil {
		t.Fatalf("no-origin PR pull should skip cleanly, got error: %v", err)
	}
	if !batch.Done {
		t.Error("Done = false, want true (no PR stream to advance)")
	}
	if len(batch.Items) != 0 {
		t.Errorf("items = %d, want 0", len(batch.Items))
	}
}

// TestLocalGitSource_PullRequestsNonGitHubOriginSkipsCleanly pins that a
// non-GitHub origin (e.g. GitLab) skips the PR stream cleanly rather than
// failing — GitHub Enterprise / other hosts are #212, not an error today.
func TestLocalGitSource_PullRequestsNonGitHubOriginSkipsCleanly(t *testing.T) {
	t.Parallel()
	dir := repoWithOrigin(t, "https://gitlab.com/octo/widgets.git")
	src := NewLocalGitSource(dir, newGitHubClient("", "", nil), nil)

	batch, err := src.Pull(context.Background(), streamPullRequests, Watermark{}, 10)
	if err != nil {
		t.Fatalf("non-github origin PR pull should skip cleanly, got error: %v", err)
	}
	if !batch.Done || len(batch.Items) != 0 {
		t.Errorf("want clean skip (Done, 0 items); got Done=%v items=%d", batch.Done, len(batch.Items))
	}
}

// TestLocalGitSource_PullPullRequestsThroughOrchestrator confirms PR prose is
// ingested as a connector-tagged, fenced, confidence-capped episode — the same
// untrusted hardening the commit stream gets, end to end.
func TestLocalGitSource_PullPullRequestsThroughOrchestrator(t *testing.T) {
	t.Parallel()
	srv := prServer(t, []gitHubPR{
		{Number: 7, Title: "Refund flow", Body: "Carol adds refunds."},
	})
	dir := repoWithOrigin(t, "git@github.com:octo/widgets.git")
	src := NewLocalGitSource(dir, newGitHubClient(srv.URL, "", srv.Client()), nil)

	app := &fakeAppender{}
	o := NewOrchestrator(app, newFakeReader(), jobs.NewRegistry(), nil)
	if _, err := o.Run(context.Background(), src, kernel.ScopePersonal, Bounds{}, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	eps := app.eventsOfKind(ontology.KindEpisode)
	// 1 seed commit + 1 PR.
	if len(eps) != 2 {
		t.Fatalf("episodes = %d, want 2 (commit + PR)", len(eps))
	}
	var sawPR bool
	for i := range eps {
		if eps[i].Source != ontology.ConnectorSourceColonPrefix+connectorLocalGit {
			t.Errorf("episode Source = %q, want connector-tagged", eps[i].Source)
		}
		if eps[i].Confidence != 1.0 {
			t.Errorf("episode Confidence = %v, want 1.0 (trust via source prefix, not a cap)", eps[i].Confidence)
		}
		var ep ontology.Episode
		if err := json.Unmarshal(eps[i].Payload, &ep); err != nil {
			t.Fatalf("unmarshal episode: %v", err)
		}
		if strings.Contains(ep.Text, "Refund flow") {
			sawPR = true
			if !strings.Contains(ep.Text, "UNTRUSTED") {
				t.Errorf("PR episode not fenced as untrusted: %s", ep.Text)
			}
		}
	}
	if !sawPR {
		t.Error("PR prose episode not found among ingested episodes")
	}
}
