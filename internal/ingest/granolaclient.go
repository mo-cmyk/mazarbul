package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultGranolaBaseURL is the official public REST API root. Injectable so
// tests point at an httptest server with zero network.
const defaultGranolaBaseURL = "https://public-api.granola.ai/v1"

// granolaMaxBodyBytes caps a single response body. Note + transcript payloads are
// small; this is a guard against a hostile/oversized response, not a real limit.
const granolaMaxBodyBytes = 16 << 20

// Granola publishes a 5 rps / 25-per-5s budget. We honour a Retry-After header
// when the API sends one and otherwise back off exponentially from a base sized
// to that budget, giving up after granolaMaxRetries 429s.
const (
	granolaMaxRetries  = 4
	granolaBaseBackoff = 500 * time.Millisecond
	granolaMaxBackoff  = 8 * time.Second
)

// Granola request sentinels. ErrGranolaUnauthorized (401/403) lets the Source
// surface the honest "requires a Business-plan API key" precondition rather than
// a raw HTTP status; errGranolaRateLimited signals a 429 that survived all
// retries; errGranolaNotFound (404) marks a note with no AI summary / transcript
// yet, which the Source skips rather than failing on.
var (
	errGranolaUnauthorized = errors.New("granola: unauthorized")
	errGranolaRateLimited  = errors.New("granola: rate limited")
	errGranolaNotFound     = errors.New("granola: note not found")
)

// granolaClient is a minimal client for the public Granola REST API: list notes
// (paged, created_after + opaque cursor) and fetch a note's transcript. Like the
// GitHub client it is tiny and injectable (baseURL + httpClient).
type granolaClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// newGranolaClient builds a client. An empty baseURL defaults to the public API;
// a nil httpClient defaults to a 30s-timeout client.
func newGranolaClient(baseURL, apiKey string, hc *http.Client) *granolaClient {
	if baseURL == "" {
		baseURL = defaultGranolaBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &granolaClient{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, httpClient: hc}
}

// granolaSpeaker / granolaSegment / granolaNote / granolaNotesPage model the
// subset of the API payload we ingest. Nullable fields (title, summary) are plain
// strings: a JSON null leaves them empty, which the body builder tolerates.
type granolaSpeaker struct {
	Source           string `json:"source"`
	DiarizationLabel string `json:"diarization_label"`
}

type granolaSegment struct {
	Speaker granolaSpeaker `json:"speaker"`
	Text    string         `json:"text"`
}

type granolaNote struct {
	ID              string           `json:"id"`
	Title           string           `json:"title"`
	SummaryMarkdown string           `json:"summary_markdown"`
	SummaryText     string           `json:"summary_text"`
	WebURL          string           `json:"web_url"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	Transcript      []granolaSegment `json:"transcript"`
}

type granolaNotesPage struct {
	Notes   []granolaNote `json:"notes"`
	Cursor  string        `json:"cursor"`
	HasMore bool          `json:"hasMore"`
}

// listNotes fetches one page of notes created after createdAfter (RFC 3339, empty
// = from the beginning), resuming from the opaque page cursor. PageSize is capped
// at the API maximum of 30.
func (c *granolaClient) listNotes(ctx context.Context, createdAfter, cursor string, pageSize int) (granolaNotesPage, error) {
	if pageSize <= 0 || pageSize > granolaPageSize {
		pageSize = granolaPageSize
	}
	q := url.Values{}
	q.Set("page_size", strconv.Itoa(pageSize))
	if createdAfter != "" {
		q.Set("created_after", createdAfter)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	body, err := c.get(ctx, "/notes?"+q.Encode())
	if err != nil {
		return granolaNotesPage{}, err
	}
	var page granolaNotesPage
	if err := json.Unmarshal(body, &page); err != nil {
		return granolaNotesPage{}, fmt.Errorf("granola: decode notes page: %w", err)
	}
	return page, nil
}

// getNote fetches a single note with its transcript. A 404 (an un-summarised /
// un-transcribed note) is returned as a sentinel the caller skips, not an error.
func (c *granolaClient) getNote(ctx context.Context, id string) (granolaNote, error) {
	body, err := c.get(ctx, "/notes/"+url.PathEscape(id)+"?include=transcript")
	if err != nil {
		return granolaNote{}, err
	}
	var note granolaNote
	if err := json.Unmarshal(body, &note); err != nil {
		return granolaNote{}, fmt.Errorf("granola: decode note %s: %w", id, err)
	}
	return note, nil
}

// get performs an authenticated GET against path (relative to baseURL), honouring
// 429 backoff. It classifies 401/403 and 404 into sentinels the Source maps to a
// clear precondition or a skip.
func (c *granolaClient) get(ctx context.Context, path string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= granolaMaxRetries; attempt++ {
		body, status, hdr, err := c.doOnce(ctx, path)
		if err != nil {
			return nil, err
		}
		switch status {
		case http.StatusOK:
			return body, nil
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, fmt.Errorf("%w: status %d", errGranolaUnauthorized, status)
		case http.StatusNotFound:
			return nil, errGranolaNotFound
		case http.StatusTooManyRequests:
			lastErr = fmt.Errorf("%w: status 429", errGranolaRateLimited)
			if attempt == granolaMaxRetries {
				return nil, lastErr
			}
			if werr := sleepCtx(ctx, granolaBackoff(attempt, hdr.Get("Retry-After"))); werr != nil {
				return nil, werr
			}
		default:
			return nil, fmt.Errorf("granola: GET %s: status %d: %s", path, status, strings.TrimSpace(string(body)))
		}
	}
	// Unreachable in practice: the final attempt's 429 branch returns lastErr and
	// every other status returns within the loop, so the loop never falls through.
	// Kept to satisfy the compiler's terminating-statement requirement.
	return nil, lastErr
}

// doOnce issues a single GET and returns the body, status, and headers.
func (c *granolaClient) doOnce(ctx context.Context, path string) ([]byte, int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("granola: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("granola: request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close of a read-only response body
	body, err := io.ReadAll(io.LimitReader(resp.Body, granolaMaxBodyBytes))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("granola: read body: %w", err)
	}
	return body, resp.StatusCode, resp.Header, nil
}

// granolaBackoff returns the wait before the next retry. It honours a Retry-After
// in either RFC 7231 form — a delay in seconds or an HTTP-date — when present,
// otherwise grows exponentially from the base. Every result is capped.
func granolaBackoff(attempt int, retryAfter string) time.Duration {
	if ra := strings.TrimSpace(retryAfter); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return capGranolaBackoff(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(ra); err == nil {
			d := time.Until(t)
			if d < 0 {
				d = 0 // a past date means retry immediately
			}
			return capGranolaBackoff(d)
		}
	}
	return capGranolaBackoff(granolaBaseBackoff << attempt)
}

// capGranolaBackoff clamps d to the maximum backoff.
func capGranolaBackoff(d time.Duration) time.Duration {
	if d > granolaMaxBackoff {
		return granolaMaxBackoff
	}
	return d
}

// sleepCtx waits for d or until ctx is cancelled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // context cancellation propagates verbatim
	case <-t.C:
		return nil
	}
}
