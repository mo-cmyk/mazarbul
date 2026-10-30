// Package selfupdate resolves, verifies, and atomically installs maz release
// archives from GitHub Releases. It is pure logic with no cobra or stdout
// dependency: the API base URL, the *http.Client, and all filesystem paths are
// injectable, so the resolve -> download -> verify -> extract -> swap pipeline
// tests against an httptest server and a temp directory with no network and no
// real release.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Repo is the GitHub owner/repo releases are published under.
const Repo = "mo-cmyk/mazarbul"

// DefaultAPIBase is the public GitHub REST API root. Overridable via
// Options.APIBase so tests point at an httptest server.
const DefaultAPIBase = "https://api.github.com"

// defaultDownloadBase is the public release CDN root (the browser download
// URL). The anonymous path fetches archives from here; it carries no token and
// is not rate-limited the way the API is.
const defaultDownloadBase = "https://github.com/" + Repo + "/releases/download"

// apiVersionHeader pins the GitHub REST API version, matching the rest of the
// codebase (see internal/ingest/github.go).
const apiVersionHeader = "2022-11-28"

// acceptJSON is the Accept header for GitHub REST JSON endpoints.
const acceptJSON = "application/vnd.github+json"

// acceptOctet is the Accept header for the release-asset endpoint, which
// answers a 302 to the signed storage URL for the raw bytes.
const acceptOctet = "application/octet-stream"

// jsonBodyCap bounds an API JSON response (release metadata is small).
const jsonBodyCap = 8 << 20 // 8 MiB

// archiveBodyCap bounds a downloaded archive so a hostile or misbehaving server
// cannot stream an unbounded body onto disk.
const archiveBodyCap = 256 << 20 // 256 MiB

// apiTimeout bounds a metadata request.
const apiTimeout = 30 * time.Second

// downloadTimeout bounds an archive fetch (larger than apiTimeout, since
// archives are tens of MiB over a slow link).
const downloadTimeout = 5 * time.Minute

// newHTTPClient returns an *http.Client with the given timeout and a
// CheckRedirect that strips the Authorization header whenever a redirect
// crosses to a different host. The release-asset endpoint 302s to a signed
// storage host; forwarding the bearer token there would leak it to a third
// party. This mirrors curl's default (and install.sh's deliberate avoidance of
// --location-trusted). Go's stdlib already strips Authorization across
// registered domains; this is the same intent, made explicit and stricter (any
// host change) so the behavior is pinned by a test rather than implied.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
}

// httpClient returns the caller-supplied client or a default one with the given
// timeout. A caller-supplied client is used verbatim (tests inject one); only
// the default carries the token-stripping CheckRedirect, so a test client must
// replicate it if it needs that behavior.
func (o *Options) httpClient(timeout time.Duration) *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return newHTTPClient(timeout)
}

// apiBase returns the configured API base or the public default.
func (o *Options) apiBase() string {
	if o.APIBase != "" {
		return o.APIBase
	}
	return DefaultAPIBase
}

// getJSON performs an authenticated (when a token is set) GET and returns the
// response body, capped at jsonBodyCap. A non-200 status is an error carrying
// the trimmed body so an auth failure (401/403) surfaces distinctly from a
// missing release (404).
func (o *Options) getJSON(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: build request: %w", err)
	}
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("X-Github-Api-Version", apiVersionHeader)
	if o.Token != "" {
		req.Header.Set("Authorization", "Bearer "+o.Token)
	}
	resp, err := o.httpClient(apiTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: request %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	body, err := io.ReadAll(io.LimitReader(resp.Body, jsonBodyCap))
	if err != nil {
		return nil, fmt.Errorf("selfupdate: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("selfupdate: GET %s: status %d: %s", url, resp.StatusCode, trim(body))
	}
	return body, nil
}

// trim shortens a response body for an error message so a large HTML error page
// does not flood the terminal.
func trim(b []byte) string {
	const maxLen = 200
	s := string(b)
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}
