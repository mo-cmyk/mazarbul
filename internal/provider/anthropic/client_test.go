package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// staticSecret is a secret.Provider that returns a fixed value or error.
// Tests use it to feed an API key without touching the OS keychain.
type staticSecret struct {
	value string
	err   error
}

func (s *staticSecret) Get(_ context.Context, _ string) (string, error) {
	return s.value, s.err
}

func (*staticSecret) Set(_ context.Context, _ string, _ string) error {
	return secret.ErrReadOnly
}

func (*staticSecret) Delete(_ context.Context, _ string) error {
	return secret.ErrReadOnly
}

// successResponse is the standard 200 OK body used by happy-path tests.
const successResponse = `{
  "id": "msg_test_001",
  "type": "message",
  "role": "assistant",
  "model": "claude-opus-4-7",
  "content": [{"type": "text", "text": "hello"}],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 42,
    "output_tokens": 7,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0
  }
}`

// errorResponseBody renders an Anthropic-style error body.
func errorResponseBody(typ, msg string) string {
	return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg)
}

// instantSleep is a sleep replacement that returns immediately. Used by
// tests that want to assert retry count without waiting on wall clock.
func instantSleep(ctx context.Context, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("anthropic: %w", err)
	}
	return nil
}

// recordedSleep captures the durations passed to sleep so tests can assert
// that backoff or retry-after was honored.
type recordedSleep struct {
	durations []time.Duration
}

func (r *recordedSleep) sleep(ctx context.Context, d time.Duration) error {
	r.durations = append(r.durations, d)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("anthropic: %w", err)
	}
	return nil
}

// newTestClient wires a Client that points at ts and uses the given hooks.
// The keys are deterministic to keep the suite race-clean.
func newTestClient(t *testing.T, ts *httptest.Server, hooks func(*Options)) *Client {
	t.Helper()
	opts := &Options{
		BaseURL:        ts.URL,
		SecretProvider: &staticSecret{value: "sk-ant-test"},
		MaxRetries:     3,
		sleep:          instantSleep,
		now:            func() time.Time { return time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC) },
		rng:            rand.New(rand.NewPCG(1, 2)),
	}
	if hooks != nil {
		hooks(opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func sampleRequest() *CompleteRequest {
	return &CompleteRequest{
		Model:     "claude-opus-4-7",
		MaxTokens: 256,
		Messages: []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("hello")}},
		},
	}
}

func TestNew_RequiresOptions(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) returned no error")
	}
}

func TestNew_RequiresSecretProvider(t *testing.T) {
	if _, err := New(&Options{}); err == nil {
		t.Fatal("New with no SecretProvider returned no error")
	}
}

func TestNew_DefaultsApplied(t *testing.T) {
	c, err := New(&Options{SecretProvider: &staticSecret{value: "sk"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, defaultBaseURL)
	}
	if c.anthropicVersion != defaultAnthropicVersion {
		t.Errorf("anthropicVersion = %q, want %q", c.anthropicVersion, defaultAnthropicVersion)
	}
	if c.maxRetries != defaultMaxRetries {
		t.Errorf("maxRetries = %d, want %d", c.maxRetries, defaultMaxRetries)
	}
	if c.secretName != secret.NameAnthropicAPIKey {
		t.Errorf("secretName = %q, want %q", c.secretName, secret.NameAnthropicAPIKey)
	}
	if c.http.Timeout != defaultTimeout {
		t.Errorf("http.Timeout = %v, want %v", c.http.Timeout, defaultTimeout)
	}
}

func TestComplete_Success(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Headers per the issue's acceptance criteria.
		if got := r.Header.Get("Anthropic-Version"); got != defaultAnthropicVersion {
			t.Errorf("Anthropic-Version = %q, want %q", got, defaultAnthropicVersion)
		}
		if got := r.Header.Get("X-Api-Key"); got != "sk-ant-test" {
			t.Errorf("X-Api-Key = %q, want %q", got, "sk-ant-test")
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want %q", got, "application/json")
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			t.Errorf("path = %s, want suffix /v1/messages", r.URL.Path)
		}

		// Request body shape.
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var wire wireRequest
		if err := json.Unmarshal(buf, &wire); err != nil {
			t.Fatalf("unmarshal body: %v\nbody: %s", err, buf)
		}
		if wire.Model != "claude-opus-4-7" {
			t.Errorf("model = %q, want claude-opus-4-7", wire.Model)
		}
		if wire.MaxTokens != 256 {
			t.Errorf("max_tokens = %d, want 256", wire.MaxTokens)
		}
		if len(wire.Messages) != 1 || wire.Messages[0].Role != RoleUser {
			t.Errorf("messages = %+v, want one user message", wire.Messages)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_test_001")
		_, _ = io.WriteString(w, successResponse)
	}))
	t.Cleanup(ts.Close)

	c := newTestClient(t, ts, nil)
	resp, err := c.Complete(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("server calls = %d, want 1", calls.Load())
	}
	if resp.ID != "msg_test_001" {
		t.Errorf("resp.ID = %q, want msg_test_001", resp.ID)
	}
	if resp.Role != RoleAssistant {
		t.Errorf("resp.Role = %q, want %q", resp.Role, RoleAssistant)
	}
	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want 42/7", resp.Usage)
	}
	if resp.CreateTime.IsZero() {
		t.Errorf("CreateTime not populated")
	}
	if !resp.CreateTime.Equal(time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("CreateTime = %v, want fixed test time", resp.CreateTime)
	}
}

func TestComplete_CacheControlPropagates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var wire wireRequest
		if err := json.Unmarshal(buf, &wire); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(wire.System) != 1 || wire.System[0].CacheControl == nil {
			t.Errorf("system block missing cache_control: %+v", wire.System)
		}
		if wire.System[0].CacheControl.Type != "ephemeral" {
			t.Errorf("cache_control.type = %q, want ephemeral", wire.System[0].CacheControl.Type)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, successResponse)
	}))
	t.Cleanup(ts.Close)

	c := newTestClient(t, ts, nil)
	req := sampleRequest()
	req.System = []ContentBlock{
		{Type: ContentTypeText, Text: "you are a helpful assistant", CacheControl: EphemeralCache()},
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestComplete_RetryOn429WithRetryAfter(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, errorResponseBody("rate_limit_error", "slow down"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, successResponse)
	}))
	t.Cleanup(ts.Close)

	rec := &recordedSleep{}
	c := newTestClient(t, ts, func(o *Options) { o.sleep = rec.sleep })

	resp, err := c.Complete(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("server calls = %d, want 2", calls.Load())
	}
	if resp.ID != "msg_test_001" {
		t.Errorf("resp.ID = %q, want msg_test_001", resp.ID)
	}
	if len(rec.durations) != 1 || rec.durations[0] != time.Second {
		t.Errorf("sleeps = %v, want [1s] honoring retry-after", rec.durations)
	}
}

func TestComplete_RetryOn5xx(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, errorResponseBody("overloaded_error", "try again"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, successResponse)
	}))
	t.Cleanup(ts.Close)

	c := newTestClient(t, ts, nil)
	if _, err := c.Complete(context.Background(), sampleRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("server calls = %d, want 2", calls.Load())
	}
}

func TestComplete_ExhaustedRetries(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, errorResponseBody("api_error", "internal"))
	}))
	t.Cleanup(ts.Close)

	c := newTestClient(t, ts, nil)
	_, err := c.Complete(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("Complete returned no error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not *APIError", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", apiErr.StatusCode)
	}
	// 1 initial + 3 retries = 4 attempts.
	if calls.Load() != 4 {
		t.Errorf("server calls = %d, want 4 (1 initial + 3 retries)", calls.Load())
	}
}

func TestComplete_FastFailOn401(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, errorResponseBody("authentication_error", "bad key"))
	}))
	t.Cleanup(ts.Close)

	c := newTestClient(t, ts, nil)
	_, err := c.Complete(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("Complete returned no error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", apiErr.StatusCode)
	}
	if apiErr.Type != "authentication_error" {
		t.Errorf("type = %q, want authentication_error", apiErr.Type)
	}
	if calls.Load() != 1 {
		t.Errorf("server calls = %d, want 1 (no retry on 4xx)", calls.Load())
	}
}

func TestComplete_FastFailOn400(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, errorResponseBody("invalid_request_error", "bad shape"))
	}))
	t.Cleanup(ts.Close)

	c := newTestClient(t, ts, nil)
	if _, err := c.Complete(context.Background(), sampleRequest()); err == nil {
		t.Fatal("Complete returned no error")
	}
	if calls.Load() != 1 {
		t.Errorf("server calls = %d, want 1 (no retry on 4xx)", calls.Load())
	}
}

func TestComplete_ContextCancelDuringBackoff(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, errorResponseBody("overloaded_error", "later"))
	}))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel-aware sleep that flips the context on first invocation, so the
	// retry loop observes ctx.Err() and bails out without further attempts.
	sleep := func(_ context.Context, _ time.Duration) error {
		cancel()
		return nil
	}
	c := newTestClient(t, ts, func(o *Options) { o.sleep = sleep })

	_, err := c.Complete(ctx, sampleRequest())
	if err == nil {
		t.Fatal("Complete returned no error on cancelled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls.Load() != 1 {
		t.Errorf("server calls = %d, want 1 (ctx cancelled after first attempt)", calls.Load())
	}
}

func TestComplete_MissingAPIKey(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("server should not be reached when secret is missing")
	}))
	t.Cleanup(ts.Close)

	c := newTestClient(t, ts, func(o *Options) {
		o.SecretProvider = &staticSecret{err: secret.ErrNotFound}
	})

	_, err := c.Complete(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("Complete returned no error when secret missing")
	}
	if !errors.Is(err, secret.ErrNotFound) {
		t.Errorf("err = %v, want errors.Is ErrNotFound", err)
	}
}

func TestComplete_ValidatesRequest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("server should not be reached on validation error")
	}))
	t.Cleanup(ts.Close)

	c := newTestClient(t, ts, nil)
	cases := []*CompleteRequest{
		nil,
		{},
		{Model: "claude-opus-4-7"},
		{Model: "claude-opus-4-7", Messages: []Message{{Role: RoleUser}}},
	}
	for i, req := range cases {
		if _, err := c.Complete(context.Background(), req); err == nil {
			t.Errorf("case %d: Complete returned no error for invalid req %+v", i, req)
		}
	}
}

func TestComplete_APIKeyCachedAcrossCalls(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, successResponse)
	}))
	t.Cleanup(ts.Close)

	var secretReads atomic.Int32
	prov := &countingSecret{value: "sk-cached", reads: &secretReads}
	c := newTestClient(t, ts, func(o *Options) { o.SecretProvider = prov })

	for i := range 3 {
		if _, err := c.Complete(context.Background(), sampleRequest()); err != nil {
			t.Fatalf("Complete %d: %v", i, err)
		}
	}
	if secretReads.Load() != 1 {
		t.Errorf("secret reads = %d, want 1 (api key must be cached)", secretReads.Load())
	}
	if calls.Load() != 3 {
		t.Errorf("server calls = %d, want 3", calls.Load())
	}
}

// countingSecret counts Get calls so the cache-once assertion is exact.
type countingSecret struct {
	value string
	err   error
	reads *atomic.Int32
}

func (s *countingSecret) Get(_ context.Context, _ string) (string, error) {
	s.reads.Add(1)
	return s.value, s.err
}

func (*countingSecret) Set(_ context.Context, _ string, _ string) error {
	return secret.ErrReadOnly
}

func (*countingSecret) Delete(_ context.Context, _ string) error {
	return secret.ErrReadOnly
}

// scriptedSecret returns errors then a success, used to assert that a
// transient failure on the first secret read does not stick forever.
type scriptedSecret struct {
	value     string
	errs      []error // one error per call, in order; success after the slice is exhausted
	reads     *atomic.Int32
	stickyErr error
}

func (s *scriptedSecret) Get(_ context.Context, _ string) (string, error) {
	i := s.reads.Add(1) - 1
	if s.stickyErr != nil {
		return "", s.stickyErr
	}
	if int(i) < len(s.errs) {
		return "", s.errs[i]
	}
	return s.value, nil
}

func (*scriptedSecret) Set(_ context.Context, _ string, _ string) error {
	return secret.ErrReadOnly
}

func (*scriptedSecret) Delete(_ context.Context, _ string) error {
	return secret.ErrReadOnly
}

// TestComplete_TransientSecretErrorNotCached covers the Greptile P1 finding
// that a cancelled context on the first Complete call must not poison
// subsequent calls. The scripted secret returns context.Canceled once, then
// succeeds; the client should call Get twice and the second Complete must
// succeed.
func TestComplete_TransientSecretErrorNotCached(t *testing.T) {
	var serverCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serverCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, successResponse)
	}))
	t.Cleanup(ts.Close)

	var secretReads atomic.Int32
	prov := &scriptedSecret{
		value: "sk-eventually-readable",
		errs:  []error{context.Canceled},
		reads: &secretReads,
	}
	c := newTestClient(t, ts, func(o *Options) { o.SecretProvider = prov })

	// First call hits the cancelled-secret path.
	if _, err := c.Complete(context.Background(), sampleRequest()); err == nil {
		t.Fatal("first Complete returned no error despite secret returning context.Canceled")
	}

	// Second call must retry the secret read, not return the cached error.
	if _, err := c.Complete(context.Background(), sampleRequest()); err != nil {
		t.Fatalf("second Complete: %v (transient error must not be cached)", err)
	}
	if secretReads.Load() != 2 {
		t.Errorf("secret reads = %d, want 2 (failed read must not be cached)", secretReads.Load())
	}
	if serverCalls.Load() != 1 {
		t.Errorf("server calls = %d, want 1 (only the second Complete reaches HTTP)", serverCalls.Load())
	}
}

// TestComplete_ConcurrentRetriesRaceClean exercises the Greptile P1 RNG
// race finding. N goroutines run Complete concurrently against a server
// that responds 429 once per call before 200, so every call invokes
// backoff() and therefore the shared jitter source. The -race detector
// reports a data race on the unsynchronised *rand.Rand without the fix.
func TestComplete_ConcurrentRetriesRaceClean(t *testing.T) {
	const callers = 16
	var attempts sync.Map
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		key := string(buf)
		n, _ := attempts.LoadOrStore(key, new(atomic.Int32))
		count := n.(*atomic.Int32).Add(1)
		if count == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, errorResponseBody("rate_limit_error", "slow down"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, successResponse)
	}))
	t.Cleanup(ts.Close)

	c := newTestClient(t, ts, nil)

	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			// Distinct bodies so the per-key attempt counter above gives
			// every caller its own 429-then-200 sequence.
			req := sampleRequest()
			req.Metadata = map[string]string{"user_id": fmt.Sprintf("u%d", i)}
			if _, err := c.Complete(context.Background(), req); err != nil {
				t.Errorf("caller %d: Complete: %v", i, err)
			}
		}()
	}
	wg.Wait()
}

// TestNew_NormalizesTrailingSlash covers the Greptile P2 finding. A base
// URL with a trailing slash must produce one and only one slash before the
// /v1/messages path.
func TestNew_NormalizesTrailingSlash(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://proxy.example", "https://proxy.example/v1/messages"},
		{"https://proxy.example/", "https://proxy.example/v1/messages"},
		{"https://proxy.example//", "https://proxy.example/v1/messages"},
	}
	for _, c := range cases {
		client, err := New(&Options{
			BaseURL:        c.in,
			SecretProvider: &staticSecret{value: "sk"},
		})
		if err != nil {
			t.Fatalf("New(%q): %v", c.in, err)
		}
		if client.messagesURL != c.want {
			t.Errorf("messagesURL for %q = %q, want %q", c.in, client.messagesURL, c.want)
		}
	}
}
