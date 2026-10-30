// Package ollama is the M1 default local provider for both dense embeddings
// and chat completions. It talks to a local Ollama daemon (default
// http://localhost:11434) over HTTP, validates that the configured model(s)
// are installed, and dispatches Embed / Complete calls.
//
// The package deliberately does not yet implement the plugin.Provider
// interface (Manifest / Init / Shutdown / Slots). M1 calls New, Embed, and
// Complete directly from the kernel; the M3 plugin loader wraps this client
// as a Provider when the loader lands.
//
// Scope (M1 only):
//   - Dense embeddings via POST /api/embed with {model, input, dimensions}.
//     The plural endpoint honours Matryoshka truncation; the older
//     /api/embeddings silently ignores `dimensions`.
//   - Single text per Embed call. Batch lands with the embedding cache in M2+.
//   - Chat completions via POST /api/chat with {model, messages, stream:false}.
//   - Streaming responses, tool / function calling, and non-localhost daemons
//     are deferred to M2+ / M3+ respectively.
//   - Local-only. Remote providers (Anthropic, OpenAI, Voyage, Cohere) live in
//     sibling packages.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/embedder"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// Compile-time check that *Client satisfies the kernel embedder.Embedder
// contract. The cache (internal/kernel/cache) and the future orchestrator
// (#55) consume Client through that interface; pinning the assertion here
// makes a contract drift fail at build time in this package rather than
// surface at the wiring site.
var _ embedder.Embedder = (*Client)(nil)

// Default configuration knobs. Exported so callers and tests can reference
// them without re-declaring the literals.
const (
	// DefaultBaseURL is the URL of a local Ollama daemon started with
	// `ollama serve` and no overrides.
	DefaultBaseURL = "http://localhost:11434"

	// DefaultEmbedModel is the default embedding model. Snowflake-arctic-embed2
	// (Snowflake Arctic Embed L v2.0) is a multilingual retrieval model with
	// strong German performance and a POSITIVE entity-dedup margin on our gold
	// set — qwen3-embedding:4b's margin was negative, which over-merged distinct
	// entities. It emits 1024-dim vectors natively (== storage.VectorDim, no
	// Matryoshka truncation needed) and at ~1.2 GB co-resides with the 7B chat
	// model under OLLAMA_MAX_LOADED_MODELS=2 with no GPU eviction thrash (the
	// 9.9 GB qwen3-embedding:4b would evict the chat model on every embed↔chat
	// switch). See agent-docs/architecture.md section 10 for the full rationale.
	DefaultEmbedModel = "snowflake-arctic-embed2"

	// DefaultChatModel is the recommended local chat model. Users with a
	// different installed model pass Options.ChatModel explicitly; this
	// constant exists so the init wizard (#47) and docs can quote a single
	// canonical value. It matches the model scripts/install.sh pulls, and at
	// 7B co-resides with snowflake-arctic-embed2 under OLLAMA_MAX_LOADED_MODELS=2.
	DefaultChatModel = "qwen2.5:7b"

	// DefaultTimeout is the per-request HTTP timeout. BGE-M3 inference is
	// heavier than smaller embedding models, so the budget is generous.
	DefaultTimeout = 60 * time.Second

	// MaxRetries is the number of retries on top of the initial attempt.
	// Total HTTP attempts per call: maxRetries + 1.
	maxRetries = 2

	// BaseBackoff is the first retry sleep. Each subsequent retry doubles
	// the duration; ±25% jitter is added to avoid thundering herd.
	baseBackoff = 200 * time.Millisecond
)

// Options configures a Client. The zero value enables embeddings: BaseURL
// defaults to localhost:11434, EmbedModel to bge-m3, and HTTPClient to a
// fresh client with a 60-second timeout. ChatModel defaults to empty (chat
// disabled); set it explicitly to enable Complete.
type Options struct {
	// BaseURL overrides the Ollama daemon URL. Empty means DefaultBaseURL.
	// Trailing slashes are trimmed.
	BaseURL string

	// EmbedModel overrides the embedding model. Empty means DefaultEmbedModel.
	EmbedModel string

	// ChatModel selects the chat model used by Complete. Empty disables the
	// chat path: New skips the chat-model probe, and Complete returns an
	// error. Callers who want chat must set this explicitly, typically to
	// DefaultChatModel (qwen2.5:7b).
	ChatModel string

	// HTTPClient overrides the default *http.Client. Empty means a fresh
	// client with Timeout = DefaultTimeout (or RequestTimeout when set).
	// Provide your own to install custom transports, mocks, or shorter
	// timeouts in tests; an explicit HTTPClient takes precedence over
	// RequestTimeout.
	HTTPClient *http.Client

	// RequestTimeout overrides DefaultTimeout for the per-request HTTP
	// deadline when no explicit HTTPClient is supplied. The chat path sets a
	// generous value because a single large-transcript extraction (prompt-eval
	// of a multi-KB body plus constrained generation) routinely exceeds the
	// 60s embed default; the embed client keeps the short default so recall
	// fails fast when Ollama is unreachable. Zero means use DefaultTimeout.
	RequestTimeout time.Duration

	// SecretProvider is accepted for signature parity with anthropic.Options
	// so the init wizard (#47) can construct any chat provider with the same
	// shape. Ollama needs no API key; this field is intentionally unused.
	SecretProvider secret.Provider
}

// Client talks to a single Ollama daemon. Safe for concurrent use: the
// underlying *http.Client is goroutine-safe and the Client holds no mutable
// state after construction.
type Client struct {
	baseURL   string
	model     string
	chatModel string
	http      *http.Client
}

// New constructs a Client and probes the Ollama daemon via GET /api/tags.
// The configured models are each verified against the daemon's installed
// list; any missing model wraps ErrModelMissing with the exact
// `ollama pull <model>` command the user must run.
//
// Probe rules (single /api/tags round trip in all cases):
//   - Options{}                       -> embed defaults to DefaultEmbedModel,
//     embed-only legacy path; chat disabled.
//   - Options{EmbedModel: m}          -> embed probes m; chat disabled.
//   - Options{ChatModel: c}           -> chat-only; embed disabled.
//     Embed() returns an explicit not-configured error.
//   - Options{EmbedModel: m, ChatModel: c} -> both probed.
//
// Chat-only mode deliberately skips the embed default-fill so a user who
// only installed `llama3.1:8b` is not asked to pull an embedding model
// they never requested.
//
// New honours ctx for the probe. Subsequent Embed / Complete / ListModels
// calls take their own ctx and rely on the HTTPClient's own Timeout for the
// per-request deadline.
func New(ctx context.Context, opts Options) (*Client, error) { //nolint:gocritic // hugeParam: New is a once-per-boot constructor; the by-value Options API is shared with every call site (incl. all tests) and is not worth a pointer break for an 80-byte copy.
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	// Validate the URL up front so a typo surfaces here, not on first call.
	// Wrapping ErrInvalidConfig (not the raw url.Parse error) lets the
	// daemon wiring's embedder-error classifier keep config errors fatal
	// while still allowing transient probe failures to downgrade to
	// write-only mode. Without the sentinel, a malformed BaseURL would
	// look indistinguishable from "Ollama daemon down" and silently hide
	// recall instead of failing daemon startup.
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid base URL %q: %w", ErrInvalidConfig, baseURL, err)
	}
	// url.Parse is lenient — it accepts almost any string. Enforce the
	// invariants the Ollama HTTP path actually needs (an absolute URL with
	// a scheme and host) so common typos like "localhost:11434" (no
	// scheme) or "" (after default-fill is skipped by a caller passing a
	// literal " ") surface at config-load time rather than during the
	// first probe RoundTrip where the diagnostic would be opaque.
	if !parsed.IsAbs() || parsed.Host == "" {
		return nil, fmt.Errorf("%w: base URL %q must include a scheme and host (e.g. http://127.0.0.1:11434)", ErrInvalidConfig, baseURL)
	}
	// Strip trailing slash so path joining stays unambiguous.
	for len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}

	embedModel := opts.EmbedModel
	if embedModel == "" && opts.ChatModel == "" {
		// Legacy embed-only path: zero-value Options still works.
		embedModel = DefaultEmbedModel
	}

	httpClient := newHTTPClient(opts.HTTPClient, opts.RequestTimeout)

	c := &Client{
		baseURL:   baseURL,
		model:     embedModel,
		chatModel: opts.ChatModel,
		http:      httpClient,
	}

	logger := logging.FromContext(ctx)
	logger.Info("ollama: probing daemon",
		zap.String("baseURL", baseURL),
		zap.String("embedModel", c.model),
		zap.String("chatModel", c.chatModel),
	)

	models, err := c.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("ollama: probe failed: %w", err)
	}
	if c.model != "" && !modelInstalled(models, c.model) {
		return nil, fmt.Errorf("%w: %q; run: ollama pull %s", ErrModelMissing, c.model, c.model)
	}
	if c.chatModel != "" && !modelInstalled(models, c.chatModel) {
		return nil, fmt.Errorf("%w: %q; run: ollama pull %s", ErrModelMissing, c.chatModel, c.chatModel)
	}

	logger.Info("ollama: probe ok",
		zap.Int("installedModels", len(models)),
		zap.String("embedModel", c.model),
		zap.String("chatModel", c.chatModel),
	)
	return c, nil
}

// newHTTPClient builds the per-request HTTP client for New. An explicit custom
// client wins (tests, custom transports); otherwise the deadline is
// requestTimeout when set, else DefaultTimeout.
func newHTTPClient(custom *http.Client, requestTimeout time.Duration) *http.Client {
	if custom != nil {
		return custom
	}
	timeout := DefaultTimeout
	if requestTimeout > 0 {
		timeout = requestTimeout
	}
	return &http.Client{Timeout: timeout}
}

// BaseURL returns the resolved base URL the client talks to. Exposed for
// downstream consumers (init wizard, doctor) that surface the value to users.
func (c *Client) BaseURL() string { return c.baseURL }

// EmbedModel returns the embed model name configured on the client. Empty
// when the client was constructed in chat-only mode.
func (c *Client) EmbedModel() string { return c.model }

// Model satisfies the kernel embedder.Embedder interface; returns the same
// value as EmbedModel. The alias keeps the existing init-wizard and
// maz-doctor consumers working while letting the cache wrap the Client via
// the kernel-level interface.
func (c *Client) Model() string { return c.model }

// ChatModel returns the chat model name configured on the client. Empty when
// chat is disabled (Options.ChatModel was not set at construction).
func (c *Client) ChatModel() string { return c.chatModel }

// doJSON performs a JSON request with retry semantics:
//   - 2 retries (3 total attempts) on transport error or HTTP 5xx.
//   - No retry on 4xx; the response is returned immediately.
//   - Respects ctx cancellation between attempts.
//
// reqBody may be nil for GET. On success, the JSON-decoded response is
// written to out. On non-2xx, doJSON returns a *httpStatusError so callers
// can switch on status code (model-missing maps to 200 with empty list, not
// a non-2xx; this is just for surfacing 4xx/5xx errors).
func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, out any) error {
	encoded, err := encodeBody(reqBody)
	if err != nil {
		return err
	}

	logger := logging.FromContext(ctx)
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, logger, method, path, attempt, lastErr); err != nil {
				return err
			}
		}
		done, err := c.attempt(ctx, method, path, encoded, out)
		if done {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// encodeBody marshals reqBody to JSON. Nil reqBody returns nil bytes.
func encodeBody(reqBody any) ([]byte, error) {
	if reqBody == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}
	return encoded, nil
}

// sleepBackoff logs the retry then sleeps backoffFor(attempt), returning
// ctx.Err() (wrapped) if ctx cancels first.
func sleepBackoff(ctx context.Context, logger *zap.Logger, method, path string, attempt int, lastErr error) error {
	sleep := backoffFor(attempt)
	logger.Warn("ollama: retrying",
		zap.String("method", method),
		zap.String("path", path),
		zap.Int("attempt", attempt),
		zap.Duration("sleep", sleep),
		zap.Error(lastErr),
	)
	select {
	case <-time.After(sleep):
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ollama: %w", ctx.Err())
	}
}

// attempt performs one HTTP round trip. It returns (done=true, err) when the
// caller must stop the retry loop (success, 4xx, or non-retryable build
// error), and (done=false, lastErr) when the caller should retry.
func (c *Client) attempt(ctx context.Context, method, path string, encoded []byte, out any) (bool, error) {
	req, err := c.buildRequest(ctx, method, path, encoded)
	if err != nil {
		return true, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// http.Do returns an error when the request context is canceled or
		// its deadline expires. Treat that as terminal so callers (recall
		// or remember flows) stop immediately instead of sleeping the
		// backoff and retrying with an already-dead context.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return true, fmt.Errorf("ollama: %w", ctxErr)
		}
		return false, fmt.Errorf("ollama: http %s %s: %w", method, path, err)
	}
	logger := logging.FromContext(ctx)
	defer closeBody(logger, resp)

	switch {
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return true, readStatusError(resp)
	case resp.StatusCode >= 500:
		return false, readStatusError(resp)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return true, fmt.Errorf("ollama: decode response: %w", err)
	}
	return true, nil
}

// closeBody closes resp.Body and logs (but does not propagate) any close
// error. Close failures on the response body are non-actionable for callers:
// the request has already completed and we have the bytes we needed.
func closeBody(logger *zap.Logger, resp *http.Response) {
	if err := resp.Body.Close(); err != nil {
		logger.Warn("ollama: close response body", zap.Error(err))
	}
}

// buildRequest constructs an *http.Request bound to ctx with the configured
// JSON headers.
func (c *Client) buildRequest(ctx context.Context, method, path string, encoded []byte) (*http.Request, error) {
	var body io.Reader
	if encoded != nil {
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	if encoded != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// readStatusError reads up to 1 KiB of the response body for diagnostics and
// returns an error carrying the HTTP status. The body cap protects against
// pathological servers that stream gigabytes of error text.
func readStatusError(resp *http.Response) error {
	const maxBody = 1024
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return &httpStatusError{statusCode: resp.StatusCode, body: "<unreadable body: " + err.Error() + ">"}
	}
	return &httpStatusError{
		statusCode: resp.StatusCode,
		body:       string(bytes.TrimSpace(buf)),
	}
}

// httpStatusError carries the HTTP status code plus a (capped) snippet of the
// response body. Callers do `var hse *httpStatusError; errors.As(err, &hse)`
// when they need the code; the default Error() string keeps the body short.
type httpStatusError struct {
	statusCode int
	body       string
}

func (e *httpStatusError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("ollama: HTTP %d", e.statusCode)
	}
	return fmt.Sprintf("ollama: HTTP %d: %s", e.statusCode, e.body)
}

// backoffFor returns the sleep duration for the given retry attempt. The
// sequence is 200ms, 400ms, 800ms, ... With ±25% jitter. Attempt starts at 1.
func backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := baseBackoff << (attempt - 1)
	// Jitter: multiplier in [0.75, 1.25].
	jitter := 0.75 + rand.Float64()*0.5
	return time.Duration(float64(base) * jitter)
}
