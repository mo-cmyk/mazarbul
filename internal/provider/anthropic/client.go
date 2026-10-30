package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// Default values for Options. Override per-field on Options to change.
const (
	defaultBaseURL          = "https://api.anthropic.com"
	defaultMessagesPath     = "/v1/messages"
	defaultAnthropicVersion = "2023-06-01"
	defaultTimeout          = 30 * time.Second
	defaultUserAgent        = "mazarbul/0.x"
)

// Options configures Client. Zero values are filled in by New with the
// defaults above. Pass &Options{...} to New.
type Options struct {
	// BaseURL is the API base URL. Defaults to defaultBaseURL. Tests pass
	// httptest.NewServer().URL here.
	BaseURL string

	// AnthropicVersion sets the "anthropic-version" header. Defaults to
	// defaultAnthropicVersion.
	AnthropicVersion string

	// HTTPClient is the transport. When nil, New constructs a client with
	// Timeout = Options.Timeout (or defaultTimeout).
	HTTPClient *http.Client

	// Timeout applies only when HTTPClient is nil. Defaults to
	// defaultTimeout.
	Timeout time.Duration

	// MaxRetries is the number of retries on top of the initial attempt.
	// Defaults to defaultMaxRetries (3 retries = up to 4 attempts).
	MaxRetries int

	// UserAgent is sent as the "User-Agent" header. Defaults to
	// defaultUserAgent.
	UserAgent string

	// SecretProvider supplies the Anthropic API key. Required.
	SecretProvider secret.Provider

	// SecretName names the entry in SecretProvider to read. Defaults to
	// secret.NameAnthropicAPIKey.
	SecretName string

	// sleep, if non-nil, replaces the production context-aware sleep.
	// Tests set this to avoid waiting on the real clock during retry loops.
	sleep func(context.Context, time.Duration) error

	// now, if non-nil, replaces time.Now. Tests use it to assert CreateTime.
	now func() time.Time

	// rng, if non-nil, replaces the jitter RNG. Tests seed a deterministic
	// PCG so backoff sleeps become predictable.
	rng *rand.Rand
}

// Client is an Anthropic Messages API client. The zero value is not usable;
// call New. Client is safe for concurrent use.
type Client struct {
	baseURL          string
	messagesURL      string
	anthropicVersion string
	userAgent        string
	maxRetries       int
	http             *http.Client
	secretProvider   secret.Provider
	secretName       string

	sleep func(context.Context, time.Duration) error
	now   func() time.Time

	rngMu sync.Mutex
	rng   *rand.Rand

	keyMu  sync.Mutex
	apiKey string
}

// nextJitter returns a fresh jitter value in [0.0, 1.0) under the rngMu
// lock. The rand.Rand is not safe for concurrent use; isolating every read
// here keeps Client.Complete race-clean across goroutines.
func (c *Client) nextJitter() float64 {
	c.rngMu.Lock()
	defer c.rngMu.Unlock()
	return c.rng.Float64()
}

// New validates opts and returns a Client. It does not read the API key;
// the first Complete call performs the read so a missing or unreadable
// secret surfaces with a specific error at the call site.
func New(opts *Options) (*Client, error) {
	if opts == nil {
		return nil, errors.New("anthropic: Options is required")
	}
	if opts.SecretProvider == nil {
		return nil, errors.New("anthropic: Options.SecretProvider is required")
	}

	c := &Client{
		baseURL:          firstNonEmpty(opts.BaseURL, defaultBaseURL),
		anthropicVersion: firstNonEmpty(opts.AnthropicVersion, defaultAnthropicVersion),
		userAgent:        firstNonEmpty(opts.UserAgent, defaultUserAgent),
		maxRetries:       opts.MaxRetries,
		secretProvider:   opts.SecretProvider,
		secretName:       firstNonEmpty(opts.SecretName, secret.NameAnthropicAPIKey),
	}
	if c.maxRetries <= 0 {
		c.maxRetries = defaultMaxRetries
	}
	// Trim trailing slashes so a user-configured base URL like
	// "https://proxy.example/" does not produce a double-slash that some
	// muxes route to a 404.
	c.messagesURL = strings.TrimRight(c.baseURL, "/") + defaultMessagesPath

	if opts.HTTPClient != nil {
		c.http = opts.HTTPClient
	} else {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		c.http = &http.Client{Timeout: timeout}
	}

	c.sleep = opts.sleep
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	c.now = opts.now
	if c.now == nil {
		c.now = time.Now
	}
	c.rng = opts.rng
	if c.rng == nil {
		c.rng = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0xC0FFEE))
	}

	return c, nil
}

// Complete issues a single non-streaming completion. Streaming lands in M2.
// The caller passes req by pointer because CompleteRequest is wide enough
// that the linter flags by-value copies; semantically the function does not
// mutate req.
func (c *Client) Complete(ctx context.Context, req *CompleteRequest) (CompleteResponse, error) {
	if req == nil {
		return CompleteResponse{}, errors.New("anthropic: CompleteRequest is required")
	}
	if err := req.validate(); err != nil {
		return CompleteResponse{}, err
	}

	body, err := json.Marshal(toWire(req))
	if err != nil {
		return CompleteResponse{}, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	apiKey, err := c.loadAPIKey(ctx)
	if err != nil {
		return CompleteResponse{}, err
	}

	return c.runWithRetries(ctx, body, apiKey)
}

// attemptResult is the outcome of a single attempt inside runWithRetries.
// Exactly one of done or retryErr is set; retryAfter is consulted only when
// retryErr is set.
type attemptResult struct {
	done       bool
	resp       CompleteResponse
	finalErr   error
	retryErr   error
	retryAfter time.Duration
}

func (c *Client) runWithRetries(ctx context.Context, body []byte, apiKey string) (CompleteResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		res := c.attempt(ctx, body, apiKey)
		if res.done {
			return res.resp, res.finalErr
		}

		lastErr = res.retryErr
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CompleteResponse{}, fmt.Errorf("anthropic: %w", ctxErr)
		}
		if attempt == c.maxRetries {
			break
		}
		if err := c.sleep(ctx, backoff(attempt, res.retryAfter, c.nextJitter())); err != nil {
			return CompleteResponse{}, fmt.Errorf("anthropic: backoff interrupted: %w", err)
		}
	}

	if lastErr == nil {
		lastErr = errors.New("retries exhausted")
	}
	return CompleteResponse{}, fmt.Errorf("anthropic: after %d attempts: %w", c.maxRetries+1, lastErr)
}

// attempt issues one request and classifies the result. Transport failures
// are retryable; 2xx is terminal success; retryable status codes are
// retryable; every other status is terminal failure with an APIError.
func (c *Client) attempt(ctx context.Context, body []byte, apiKey string) attemptResult {
	resp, err := c.do(ctx, body, apiKey)
	if err != nil {
		return attemptResult{retryErr: err}
	}

	switch {
	case resp.statusCode >= 200 && resp.statusCode < 300:
		out, decodeErr := c.decode(resp)
		return attemptResult{done: true, resp: out, finalErr: decodeErr}
	case retryable(resp.statusCode):
		return attemptResult{
			retryErr:   c.decodeError(resp),
			retryAfter: parseRetryAfter(resp.header),
		}
	default:
		return attemptResult{done: true, finalErr: c.decodeError(resp)}
	}
}

// rawResponse is the buffered HTTP response we hand around between do,
// decode, and decodeError. Buffering up front lets retry decision logic
// inspect status, headers, and body without holding the response open.
type rawResponse struct {
	statusCode int
	header     http.Header
	body       []byte
}

func (c *Client) do(ctx context.Context, body []byte, apiKey string) (raw *rawResponse, retErr error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.messagesURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Api-Key", apiKey)
	httpReq.Header.Set("Anthropic-Version", c.anthropicVersion)
	httpReq.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: http do: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("anthropic: close body: %w", cerr)
		}
	}()

	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: read body: %w", err)
	}

	return &rawResponse{
		statusCode: resp.StatusCode,
		header:     resp.Header,
		body:       buf,
	}, nil
}

func (c *Client) decode(r *rawResponse) (CompleteResponse, error) {
	var out CompleteResponse
	if err := json.Unmarshal(r.body, &out); err != nil {
		return CompleteResponse{}, fmt.Errorf("anthropic: decode response: %w", err)
	}
	out.CreateTime = c.now().UTC()
	return out, nil
}

func (c *Client) decodeError(r *rawResponse) *APIError {
	apiErr := &APIError{
		StatusCode: r.statusCode,
		RequestID:  r.header.Get("Request-Id"),
	}
	var w wireError
	if len(r.body) > 0 && json.Unmarshal(r.body, &w) == nil {
		apiErr.Type = w.Error.Type
		apiErr.Message = w.Error.Message
	}
	return apiErr
}

// loadAPIKey reads the API key from the SecretProvider, caching the value
// after the first successful read. Failed reads are NOT cached: a
// context-cancelled or transient keychain error on the first Complete call
// must not poison subsequent calls that pass a healthy context. The lock
// serializes concurrent first-time loads so the provider sees at most one
// in-flight Get until a value is cached.
func (c *Client) loadAPIKey(ctx context.Context) (string, error) {
	c.keyMu.Lock()
	defer c.keyMu.Unlock()
	if c.apiKey != "" {
		return c.apiKey, nil
	}
	v, err := c.secretProvider.Get(ctx, c.secretName)
	if err != nil {
		return "", fmt.Errorf("anthropic: read secret %q: %w", c.secretName, err)
	}
	c.apiKey = v
	return v, nil
}

// validate enforces the minimum invariants Anthropic itself enforces.
// Catching them client-side gives the caller a useful error before the
// network round-trip.
func (r *CompleteRequest) validate() error {
	if r.Model == "" {
		return errors.New("anthropic: CompleteRequest.Model is required")
	}
	if len(r.Messages) == 0 {
		return errors.New("anthropic: CompleteRequest.Messages must be non-empty")
	}
	if r.MaxTokens <= 0 {
		return errors.New("anthropic: CompleteRequest.MaxTokens must be positive")
	}
	return nil
}

// toWire builds the JSON shape Anthropic expects from the Mazarbul-side
// CompleteRequest.
func toWire(r *CompleteRequest) wireRequest {
	return wireRequest{
		Model:         r.Model,
		MaxTokens:     r.MaxTokens,
		Messages:      r.Messages,
		System:        r.System,
		Temperature:   r.Temperature,
		StopSequences: r.StopSequences,
		Metadata:      r.Metadata,
	}
}

// sleepCtx is the production sleep implementation: a context-aware wait
// that returns the wrapped ctx.Err() when cancelled mid-sleep.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("anthropic: %w", err)
		}
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("anthropic: %w", ctx.Err())
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
