// Package daemonclient provides a thin HTTP client for the Mazarbul daemon's
// /v1 REST surface. It dials the daemon over Unix socket (preferred) or TCP,
// encodes requests as JSON, and decodes AIP-shaped responses.
//
// All methods accept a context.Context; cancellation propagates into the
// underlying net/http.Request.
package daemonclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// defaultHTTPTimeout is the per-request timeout for non-streaming calls.
const defaultHTTPTimeout = 30 * time.Second

// Client is a thin wrapper over net/http that dials the daemon over
// Unix socket (preferred) or TCP (fallback). Construct via New; the
// zero value is unusable.
//
// Both transports are constructed up front; do/Stream try the Unix
// socket first, and on net.OpError / ECONNREFUSED / ENOENT fall back
// to the TCP loopback address. This means daemons running in
// containers (no socket file) or daemons where the socket has been
// reaped still serve every CLI command.
type Client struct {
	socketPath   string
	httpAddr     string
	unixClient   *http.Client
	httpClient   *http.Client // pure-TCP client; used as fallback or sole transport
	unixDisabled bool         // true when NewWithHTTP was used; no Unix attempt
}

// New returns a Client configured to reach the daemon whose data directory
// is dataDir. It uses mcp.DefaultUnixSocket(dataDir) for Unix-socket
// transport and mcp.DefaultHTTPAddr as the TCP fallback address.
//
// The returned client makes no network calls; the first Get / Post / Stream
// call establishes the connection.
func New(dataDir string) (*Client, error) {
	if dataDir == "" {
		return nil, errors.New("daemonclient: dataDir required")
	}
	socketPath := mcp.DefaultUnixSocket(dataDir)
	// Prefer Unix socket. Build a transport that dials the socket for every
	// request regardless of the URL host. This lets callers use a plain
	// http://daemon URL without real DNS.
	unixTransport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		socketPath: socketPath,
		httpAddr:   mcp.DefaultHTTPAddr,
		unixClient: &http.Client{
			Transport: unixTransport,
			Timeout:   defaultHTTPTimeout,
		},
		httpClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}, nil
}

// NewWithHTTP returns a Client that dials the daemon over TCP at addr
// instead of a Unix socket. Useful in environments where socket files are
// not accessible (Docker, remote hosts). The addr follows the same
// "host:port" format as mcp.DefaultHTTPAddr.
func NewWithHTTP(addr string) *Client {
	return &Client{
		httpAddr:     addr,
		unixDisabled: true,
		httpClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}
}

// NewWithSocket returns a Client that dials only the Unix socket at socketPath.
// Unlike New (which derives the socket path from the data directory), this
// constructor accepts an explicit socket path. Intended for tests and tooling
// that already have a resolved socket path. No TCP fallback is configured.
func NewWithSocket(socketPath string) (*Client, error) {
	if socketPath == "" {
		return nil, errors.New("daemonclient: socketPath required")
	}
	unixTransport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		socketPath: socketPath,
		httpAddr:   mcp.DefaultHTTPAddr,
		unixClient: &http.Client{
			Transport: unixTransport,
			Timeout:   defaultHTTPTimeout,
		},
		// httpClient intentionally nil: no TCP fallback from this constructor.
	}, nil
}

// NewWithSocketAndHTTP returns a Client that tries the Unix socket at
// socketPath first and falls back to TCP at httpAddr on dial errors. Intended
// for tests that need to verify the no-TCP-fallback invariant on admin paths.
func NewWithSocketAndHTTP(socketPath, httpAddr string) (*Client, error) {
	if socketPath == "" {
		return nil, errors.New("daemonclient: socketPath required")
	}
	unixTransport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		socketPath: socketPath,
		httpAddr:   httpAddr,
		unixClient: &http.Client{
			Transport: unixTransport,
			Timeout:   defaultHTTPTimeout,
		},
		httpClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}, nil
}

// unixBaseURL is the synthetic host name handed to the Unix-socket
// transport. The transport redirects the TCP dial to the socket file
// regardless of the host; the path component is what carries the
// request information.
const unixBaseURL = "http://daemon"

// tcpBaseURL returns "http://<httpAddr>" for the TCP transport.
func (c *Client) tcpBaseURL() string { return "http://" + c.httpAddr }

// isDialError reports whether err looks like a connection problem the
// fallback path should retry against. We treat any net.OpError plus
// the common "no such file" / "connection refused" syscall errors as
// fallback-eligible; non-network errors (timeout, response read) are
// returned unchanged.
func isDialError(err error) bool {
	if err == nil {
		return false
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	// Wrapped fmt.Errorf strings still mention "no such file" or
	// "connection refused"; cheap substring match keeps the check
	// robust across go versions without importing syscall.
	msg := err.Error()
	return strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "socket: connect")
}

// Get sends a GET request to path (e.g. "/v1/entities") and returns the
// raw response body. The caller owns the returned bytes. An HTTP error
// status is mapped to an HTTPError.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

// Post sends a POST request to path with body encoded as JSON and returns
// the raw response body. An HTTP error status is mapped to an HTTPError.
func (c *Client) Post(ctx context.Context, path string, body any) ([]byte, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("daemonclient: marshal POST %s body: %w", path, err)
		}
		bodyBytes = b
	}
	return c.do(ctx, http.MethodPost, path, bodyBytes)
}

// Stream opens a GET request to path and returns the response body without
// reading it. The caller must close the returned ReadCloser. Used for
// Server-Sent Event streams (/v1/events:stream). When a Unix socket is
// configured Stream tries it first, then falls back to TCP on dial errors.
//
// Stream bypasses the client's Timeout so the connection can stay open
// indefinitely; cancellation via ctx is the caller's mechanism to terminate.
func (c *Client) Stream(ctx context.Context, path string) (io.ReadCloser, error) {
	attempt := func(httpClient *http.Client, base string) (io.ReadCloser, error) {
		streamClient := *httpClient
		streamClient.Timeout = 0
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, fmt.Errorf("daemonclient: build stream GET %s: %w", path, err)
		}
		req.Header.Set("Accept", "text/event-stream")
		resp, err := streamClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("daemonclient: stream GET %s: %w", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close() //nolint:errcheck // best-effort close on error path
			return nil, &HTTPError{StatusCode: resp.StatusCode, Path: path}
		}
		return resp.Body, nil
	}
	if !c.unixDisabled && c.unixClient != nil {
		body, err := attempt(c.unixClient, unixBaseURL)
		if err == nil {
			return body, nil
		}
		if !isDialError(err) {
			return nil, err
		}
	}
	return attempt(c.httpClient, c.tcpBaseURL())
}

// do executes a request against the daemon. It first tries the Unix
// socket transport (when configured); on a dial error it retries
// against the TCP loopback transport. HTTP-level errors (404, 500)
// are NOT retried — they come from the daemon, not the transport.
//
//nolint:gocognit // the function is a Unix→TCP fallback ladder; splitting it would obscure the dial-error retry policy
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	build := func(base string) (*http.Request, error) {
		var bodyR io.Reader
		if body != nil {
			bodyR = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, bodyR)
		if err != nil {
			return nil, fmt.Errorf("daemonclient: build %s %s: %w", method, path, err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		return req, nil
	}
	run := func(client *http.Client, base string) ([]byte, error) {
		req, err := build(base)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("daemonclient: %s %s: %w", req.Method, req.URL.Path, err)
		}
		defer resp.Body.Close() //nolint:errcheck // deferred close; body already consumed
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("daemonclient: read body %s %s: %w", req.Method, req.URL.Path, err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &HTTPError{StatusCode: resp.StatusCode, Path: req.URL.Path, Body: raw}
		}
		return raw, nil
	}
	if !c.unixDisabled && c.unixClient != nil {
		raw, err := run(c.unixClient, unixBaseURL)
		if err == nil {
			return raw, nil
		}
		if !isDialError(err) {
			return nil, err
		}
	}
	return run(c.httpClient, c.tcpBaseURL())
}

// doUnixOnly executes a request against the daemon over the Unix socket ONLY.
// Unlike do, it does NOT fall back to the TCP loopback transport on a dial
// error. This is the required transport for admin-path calls: the admin write
// surface is structurally absent from TCP, so a TCP fallback would silently
// route to a 404 instead of returning a clear transport error. An error on
// the unix dial path is returned directly.
func (c *Client) doUnixOnly(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if c.unixDisabled || c.unixClient == nil {
		return nil, fmt.Errorf("daemonclient: admin path %s requires a Unix socket client; TCP-only client cannot reach the admin surface", path)
	}
	build := func() (*http.Request, error) {
		var bodyR io.Reader
		if body != nil {
			bodyR = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, unixBaseURL+path, bodyR)
		if err != nil {
			return nil, fmt.Errorf("daemonclient: build %s %s: %w", method, path, err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		return req, nil
	}
	req, err := build()
	if err != nil {
		return nil, err
	}
	resp, err := c.unixClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daemonclient: %s %s (unix-only): %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // deferred close; body already consumed
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("daemonclient: read body %s %s: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Path: req.URL.Path, Body: raw}
	}
	return raw, nil
}

// AppendEvents sends a batch of events to the daemon's privileged admin
// endpoint (POST /admin/v1/events:append) over the Unix socket ONLY. It
// never falls back to TCP: the admin surface is structurally absent from the
// TCP transport. Use daemonclient.New (not NewWithHTTP) to obtain a client
// capable of admin calls.
//
// The Precheck and Checks fields mirror the daemon-side MatCheckWire
// semantics: Precheck is evaluated atomically before the append (idempotency
// guard); Checks is evaluated after (materialization confirmation).
func (c *Client) AppendEvents(ctx context.Context, req *daemon.AppendEventsRequest) (*daemon.AppendEventsResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("daemonclient: marshal AppendEvents request: %w", err)
	}
	raw, err := c.doUnixOnly(ctx, http.MethodPost, "/admin/v1/events:append", body)
	if err != nil {
		return nil, err
	}
	var resp daemon.AppendEventsResponse
	if err := decodeJSON(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RebuildStatus mirrors the daemon's GET /admin/v1/rebuild JSON body (the
// coordinator's authoritative online-rebuild status, which survives the swap's
// serving-stack reload). It is the contract the CLI polls.
type RebuildStatus struct {
	JobUID   string  `json:"jobUid"`
	Kind     string  `json:"kind"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Detail   string  `json:"detail,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// RebuildTriggerResponse mirrors the 202 body of POST /admin/v1/rebuild and
// /admin/v1/reindex.
type RebuildTriggerResponse struct {
	JobUID string `json:"jobUid"`
	Kind   string `json:"kind"`
}

// Terminal rebuild states the CLI poll loop stops on.
const (
	rebuildStateSwapping  = "swapping"
	rebuildStateSucceeded = "succeeded"
	rebuildStateFailed    = "failed"
)

// TriggerRebuild starts a full online rebuild on the daemon (POST
// /admin/v1/rebuild) over the Unix socket ONLY. It returns the new job's UID;
// poll PollRebuild for completion. A 409 (already running) surfaces as an
// *HTTPError.
func (c *Client) TriggerRebuild(ctx context.Context, strict bool) (*RebuildTriggerResponse, error) {
	return c.postRebuildTrigger(ctx, "/admin/v1/rebuild", strict)
}

// TriggerReindex starts an online reindex (re-embed everything via a from-log
// rebuild) on the daemon (POST /admin/v1/reindex) over the Unix socket ONLY.
func (c *Client) TriggerReindex(ctx context.Context) (*RebuildTriggerResponse, error) {
	return c.postRebuildTrigger(ctx, "/admin/v1/reindex", false)
}

func (c *Client) postRebuildTrigger(ctx context.Context, path string, strict bool) (*RebuildTriggerResponse, error) {
	body, err := json.Marshal(map[string]bool{"strict": strict})
	if err != nil {
		return nil, fmt.Errorf("daemonclient: marshal rebuild request: %w", err)
	}
	raw, err := c.doUnixOnly(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	var resp RebuildTriggerResponse
	if derr := decodeJSON(raw, &resp); derr != nil {
		return nil, derr
	}
	return &resp, nil
}

// RebuildStatus fetches the current online-rebuild status (GET
// /admin/v1/rebuild) over the Unix socket ONLY.
func (c *Client) RebuildStatus(ctx context.Context) (*RebuildStatus, error) {
	raw, err := c.doUnixOnly(ctx, http.MethodGet, "/admin/v1/rebuild", nil)
	if err != nil {
		return nil, err
	}
	var st RebuildStatus
	if derr := decodeJSON(raw, &st); derr != nil {
		return nil, derr
	}
	return &st, nil
}

// PollRebuild polls the rebuild status every interval until it reaches a
// terminal state, invoking onUpdate on each state change. It TOLERATES the
// socket-drop window the swap opens: a dial error is treated as "swapping"
// (the daemon is reloading its serving stack) and retried, not surfaced — the
// coordinator status survives the reload, so once the socket returns the poll
// reads the terminal state. A non-dial error is returned. PollRebuild returns
// nil on success and a descriptive error when the rebuild failed.
func (c *Client) PollRebuild(ctx context.Context, interval time.Duration, onUpdate func(RebuildStatus)) error {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	lastState := ""
	emit := func(st RebuildStatus) {
		if onUpdate != nil && st.State != lastState {
			onUpdate(st)
			lastState = st.State
		}
	}
	for {
		st, err := c.RebuildStatus(ctx)
		switch {
		case err == nil:
			emit(*st)
			switch st.State {
			case rebuildStateSucceeded:
				return nil
			case rebuildStateFailed:
				if st.Error != "" {
					return fmt.Errorf("rebuild failed: %s", st.Error)
				}
				return errors.New("rebuild failed")
			}
		case isDialError(err):
			// Swap reload window: socket briefly gone. Show progress, keep polling.
			emit(RebuildStatus{State: rebuildStateSwapping, Detail: "daemon reloading"})
		default:
			return err
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return fmt.Errorf("daemonclient: poll rebuild: %w", ctx.Err())
		}
	}
}

// HTTPError is returned when the daemon responds with a non-2xx status code.
type HTTPError struct {
	// StatusCode is the HTTP status code the daemon returned.
	StatusCode int
	// Path is the request path.
	Path string
	// Body is the raw response body (may contain an AIP error envelope).
	Body []byte
}

func (e *HTTPError) Error() string {
	if len(e.Body) > 0 && len(e.Body) < 256 {
		return fmt.Sprintf("daemonclient: %s: HTTP %d: %s", e.Path, e.StatusCode, strings.TrimSpace(string(e.Body)))
	}
	return fmt.Sprintf("daemonclient: %s: HTTP %d", e.Path, e.StatusCode)
}

// ListResponse is the generic AIP list envelope. Items is left as
// json.RawMessage so callers can decode into typed slices.
type ListResponse struct {
	// Items is the page of resources. Callers decode into their typed slice.
	Items json.RawMessage `json:"items"`
	// NextPageToken is set when additional pages are available.
	NextPageToken string `json:"nextPageToken,omitempty"`
}

// decodeJSON unmarshals raw JSON bytes into dst.
func decodeJSON(raw []byte, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("daemonclient: decode response: %w", err)
	}
	return nil
}

// sseScanner reads lines from an SSE body and calls fn for each "data: ..."
// line. It returns when the context is cancelled or the body is exhausted.
// Any "dropped:" event lines are forwarded as-is so callers can inspect them.
func sseScanner(ctx context.Context, body io.Reader, fn func(data []byte) error) error {
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("daemonclient: sse context cancelled: %w", err)
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := []byte(strings.TrimPrefix(line, "data: "))
		if err := fn(data); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("daemonclient: sse scanner: %w", err)
	}
	return nil
}
