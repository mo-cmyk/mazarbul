package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestParseOwnerRepo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		remote    string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"https with .git", "https://github.com/mo-cmyk/mazarbul.git", "mo-cmyk", "mazarbul", false},
		{"https without .git", "https://github.com/mo-cmyk/mazarbul", "mo-cmyk", "mazarbul", false},
		{"scp form", "git@github.com:mo-cmyk/mazarbul.git", "mo-cmyk", "mazarbul", false},
		{"trailing whitespace", "  https://github.com/a/b\n", "a", "b", false},
		{"not owner/repo", "https://github.com/justowner", "", "", true},
		{"unrecognised shape", "ftp://example.com/x/y", "", "", true},
		{"empty", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := parseOwnerRepo(tc.remote)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Errorf("got %q/%q, want %q/%q", owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

func TestPRCursorRoundTrip(t *testing.T) {
	t.Parallel()
	raw, err := encodePRCursor(42, 3)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	cur, err := decodePRCursor(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cur.LastPR != 42 {
		t.Errorf("LastPR = %d, want 42", cur.LastPR)
	}
	if cur.Page != 3 {
		t.Errorf("Page = %d, want 3", cur.Page)
	}
	// Empty cursor decodes to the zero value.
	empty, err := decodePRCursor(nil)
	if err != nil || empty.LastPR != 0 || empty.Page != 0 {
		t.Errorf("decode(nil) = %+v, %v; want zero", empty, err)
	}
}

// prPage builds one page of fake PRs numbered [lo, hi].
func prPage(lo, hi int) []gitHubPR {
	var out []gitHubPR
	for n := lo; n <= hi; n++ {
		out = append(out, gitHubPR{
			Number:  n,
			Title:   "PR " + strconv.Itoa(n),
			Body:    "body of pr " + strconv.Itoa(n),
			HTMLURL: "https://example.test/pr/" + strconv.Itoa(n),
		})
	}
	return out
}

func TestListPullsAfter_FiltersAndPaginates(t *testing.T) {
	t.Parallel()
	// Server returns 50 PRs on page 1 (a full page => keep paging), 10 on page 2.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		var body []gitHubPR
		switch page {
		case "1":
			body = prPage(1, 50)
		case "2":
			body = prPage(51, 60)
		default:
			body = nil
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	c := newGitHubClient(srv.URL, "", srv.Client())
	prs, _, err := c.listPullsAfter(context.Background(), "o", "r", 55, 1, 100)
	if err != nil {
		t.Fatalf("listPullsAfter: %v", err)
	}
	// Only PRs strictly after #55 should survive: 56..60 => 5 items.
	if len(prs) != 5 {
		t.Fatalf("got %d PRs, want 5 (after #55)", len(prs))
	}
	if prs[0].Number != 56 {
		t.Errorf("first PR = #%d, want #56", prs[0].Number)
	}
}

func TestListPullsAfter_HonorsLimit(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "1" {
			_ = json.NewEncoder(w).Encode(prPage(1, 50))
			return
		}
		_ = json.NewEncoder(w).Encode([]gitHubPR{})
	}))
	defer srv.Close()

	c := newGitHubClient(srv.URL, "tok", srv.Client())
	prs, _, err := c.listPullsAfter(context.Background(), "o", "r", 0, 1, 3)
	if err != nil {
		t.Fatalf("listPullsAfter: %v", err)
	}
	if len(prs) != 3 {
		t.Fatalf("got %d PRs, want 3 (limit)", len(prs))
	}
}

// TestListPullsAfter_ResumesFromPageHint pins the rate-limit fix: a delta pull
// begins at the cursor's page hint instead of re-fetching from page 1, and it
// reports the page to resume from next time. The server records the pages it was
// asked for, so the test proves the already-consumed earlier page was NOT
// re-fetched while the afterNumber filter still guards correctness on the
// resumed page.
func TestListPullsAfter_ResumesFromPageHint(t *testing.T) {
	t.Parallel()
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		requested = append(requested, page)
		var body []gitHubPR
		switch page {
		case "1":
			body = prPage(1, 50)
		case "2":
			body = prPage(51, 60)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	c := newGitHubClient(srv.URL, "", srv.Client())
	// Resume at page 2 having already consumed through PR #55.
	prs, resumePage, err := c.listPullsAfter(context.Background(), "o", "r", 55, 2, 100)
	if err != nil {
		t.Fatalf("listPullsAfter: %v", err)
	}
	if len(prs) != 5 {
		t.Fatalf("got %d PRs, want 5 (#56..#60 after #55 from page 2)", len(prs))
	}
	if prs[0].Number != 56 {
		t.Errorf("first PR = #%d, want #56", prs[0].Number)
	}
	for _, p := range requested {
		if p == "1" {
			t.Errorf("page 1 was re-fetched despite the page-2 hint: requested=%v", requested)
		}
	}
	if resumePage != 2 {
		t.Errorf("resumePage = %d, want 2", resumePage)
	}
}

func TestFetchPullsPage_Non200IsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer srv.Close()

	c := newGitHubClient(srv.URL, "", srv.Client())
	if _, _, err := c.listPullsAfter(context.Background(), "o", "r", 0, 1, 10); err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

func TestPRExternalID(t *testing.T) {
	t.Parallel()
	if got := prExternalID(7); got != "pr-7" {
		t.Errorf("prExternalID(7) = %q, want pr-7", got)
	}
}
