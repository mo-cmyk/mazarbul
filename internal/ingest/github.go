package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultGitHubBaseURL is the public GitHub REST API root. Injectable so tests
// point at an httptest server and the GitHub Enterprise case is a config change.
const defaultGitHubBaseURL = "https://api.github.com"

// githubPRPageSize is the per-page pull count. GitHub caps page size at 100.
const githubPRPageSize = 50

// gitHubClient is a minimal GitHub REST client for pull-request prose. It is
// deliberately tiny (one endpoint) and injectable: baseURL + httpClient let the
// bench/E2E drive it against a fixture server with zero network.
type gitHubClient struct {
	baseURL    string
	token      string // optional; empty = unauthenticated (public repos, rate-limited)
	httpClient *http.Client
}

// newGitHubClient builds a client. An empty baseURL defaults to the public API;
// a nil httpClient defaults to a 30s-timeout client.
func newGitHubClient(baseURL, token string, hc *http.Client) *gitHubClient {
	if baseURL == "" {
		baseURL = defaultGitHubBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &gitHubClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, httpClient: hc}
}

// gitHubPR is the subset of the pull-request payload we ingest. Body + title
// become the Episode text; Number is the cursor + external id.
type gitHubPR struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

// listPullsAfter returns up to limit PRs created strictly after afterNumber,
// ascending by creation order, for owner/repo, plus the page to resume from next
// time. GitHub's pulls list is sorted created-asc here so "after PR N" is a
// stable, monotone cursor and existing pages never shift (new PRs append at the
// end). It begins at startPage — the page the previous pull last drew from — so a
// delta over a large repo does not re-fetch and re-discard every consumed page.
// The afterNumber filter still runs on every page (including the resumed one),
// so a stale startPage can only cost a redundant fetch, never skip a PR.
func (c *gitHubClient) listPullsAfter(ctx context.Context, owner, repo string, afterNumber, startPage, limit int) (prs []gitHubPR, resumePage int, err error) {
	if startPage < 1 {
		startPage = 1
	}
	resumePage = startPage
	page := startPage
	for len(prs) < limit {
		batch, ferr := c.fetchPullsPage(ctx, owner, repo, page)
		if ferr != nil {
			return nil, 0, ferr
		}
		if len(batch) == 0 {
			break // exhausted
		}
		for _, pr := range batch {
			if pr.Number <= afterNumber {
				continue // already consumed (created-asc => number-asc)
			}
			prs = append(prs, pr)
			resumePage = page // the page that yielded the last kept PR
			if len(prs) == limit {
				break // stop exactly at the bound; do not over-collect
			}
		}
		if len(prs) == limit || len(batch) < githubPRPageSize {
			break // bound reached, or last page
		}
		page++
	}
	return prs, resumePage, nil
}

// fetchPullsPage GETs one page of pulls (state=all, sort=created, asc).
func (c *gitHubClient) fetchPullsPage(ctx context.Context, owner, repo string, page int) ([]gitHubPR, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/pulls?state=all&sort=created&direction=asc&per_page=%d&page=%d",
		c.baseURL, url.PathEscape(owner), url.PathEscape(repo), githubPRPageSize, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close of a read-only response body
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("github: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: GET pulls page %d: status %d: %s", page, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var prs []gitHubPR
	if err := json.Unmarshal(body, &prs); err != nil {
		return nil, fmt.Errorf("github: decode pulls: %w", err)
	}
	return prs, nil
}

// parseOwnerRepo extracts owner/repo from a GitHub git remote URL. It handles
// the https (https://github.com/owner/repo[.git]) and scp-like
// (git@github.com:owner/repo[.git]) forms. It returns an error for a non-GitHub
// host or any other shape so a non-GitHub remote degrades cleanly (the PR stream
// soft-skips) rather than the client guessing and querying api.github.com for a
// repository that does not exist there.
func parseOwnerRepo(remoteURL string) (owner, repo string, err error) {
	s := strings.TrimSpace(remoteURL)
	s = strings.TrimSuffix(s, ".git")
	var host, path string
	switch {
	case strings.HasPrefix(s, "git@"):
		// git@github.com:owner/repo
		at := strings.Index(s, "@")
		colon := strings.Index(s, ":")
		if colon < 0 || colon < at {
			return "", "", fmt.Errorf("github: cannot parse scp remote %q", remoteURL)
		}
		host = s[at+1 : colon]
		path = s[colon+1:]
	case strings.Contains(s, "://"):
		u, perr := url.Parse(s)
		if perr != nil {
			return "", "", fmt.Errorf("github: parse remote %q: %w", remoteURL, perr)
		}
		host = u.Hostname()
		path = strings.TrimPrefix(u.Path, "/")
	default:
		return "", "", fmt.Errorf("github: unrecognised remote shape %q", remoteURL)
	}
	if !isGitHubHost(host) {
		return "", "", fmt.Errorf("github: remote %q is not a github.com remote", remoteURL)
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("github: remote %q is not owner/repo", remoteURL)
	}
	return parts[0], parts[1], nil
}

// isGitHubHost reports whether host is the public GitHub host. GitHub Enterprise
// (custom hosts) is out of scope for v1 (issue #212).
func isGitHubHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || host == "www.github.com"
}

// prCursor is the opaque cursor JSON for the pull-requests stream. LastPR is the
// authoritative resume key (PRs are listed created-asc, so PR-number is monotone
// and "after LastPR" is exact). Page is a pagination HINT: the page that last
// yielded a kept PR, so a delta pull resumes there instead of re-paging from
// page 1 and re-fetching — then discarding — every already-consumed page (the
// rate-limit-exhaustion risk on large repos). It is only a hint: the LastPR
// filter still runs on the resumed page, so a stale Page can never skip a PR.
type prCursor struct {
	LastPR int `json:"lastPR"`
	Page   int `json:"page,omitempty"`
}

// decodePRCursor reads a prCursor from opaque watermark bytes (nil/empty => 0).
func decodePRCursor(raw json.RawMessage) (prCursor, error) {
	if len(raw) == 0 {
		return prCursor{}, nil
	}
	var c prCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return prCursor{}, fmt.Errorf("github: decode PR cursor: %w", err)
	}
	return c, nil
}

// encodePRCursor marshals a prCursor to opaque watermark bytes.
func encodePRCursor(lastPR, page int) (json.RawMessage, error) {
	raw, err := json.Marshal(prCursor{LastPR: lastPR, Page: page})
	if err != nil {
		return nil, fmt.Errorf("github: encode PR cursor: %w", err)
	}
	return raw, nil
}

// prExternalID renders a PR number as the SourceRef external id.
func prExternalID(number int) string { return "pr-" + strconv.Itoa(number) }
