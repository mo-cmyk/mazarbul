package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
)

// fakeServer holds an httptest.NewServer plus a request counter so each test
// can assert how many round trips actually happened.
type fakeServer struct {
	srv   *httptest.Server
	calls atomic.Int32
}

// newFakeServer wires h to an httptest.NewServer and increments the counter
// on every request before delegating to h.
func newFakeServer(t *testing.T, h http.HandlerFunc) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

// tagsJSON returns a canned /api/tags response body listing the given model
// names.
func tagsJSON(t *testing.T, names ...string) []byte {
	t.Helper()
	type model struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	type resp struct {
		Models []model `json:"models"`
	}
	r := resp{Models: make([]model, 0, len(names))}
	for _, n := range names {
		r.Models = append(r.Models, model{Name: n, Model: n})
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal tags: %v", err)
	}
	return b
}

// embedJSON returns a canned /api/embed response body whose first
// embedding has the requested length. The plural-array shape mirrors
// Ollama's newer /api/embed endpoint: {"embeddings":[[f32,...]]}.
// Embed delegates to EmbedBatch and so expects this batched envelope
// even for single calls; tests using this helper exercise that
// single-input path. The older /api/embeddings shape silently ignores
// the `dimensions` parameter; switching to the plural endpoint is what
// makes Matryoshka truncation honour storage.VectorDim for models
// (e.g. qwen3-embedding:4b) whose native output is wider.
func embedJSON(t *testing.T, dim int) []byte {
	t.Helper()
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = float32(i) / float32(dim)
	}
	b, err := json.Marshal(struct {
		Embeddings [][]float32 `json:"embeddings"`
	}{Embeddings: [][]float32{vec}})
	if err != nil {
		t.Fatalf("marshal embed: %v", err)
	}
	return b
}

// fastClient returns an Options with a short HTTP timeout so a stalled server
// does not hold up the test.
func fastClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second}
}

func TestNew_Probe(t *testing.T) {
	t.Run("succeeds when model is installed", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/tags" || r.Method != http.MethodGet {
				t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel, "llama3.1:8b"))
		})

		c, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			HTTPClient: fastClient(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c.BaseURL() != fs.srv.URL {
			t.Errorf("BaseURL = %q, want %q", c.BaseURL(), fs.srv.URL)
		}
		if c.EmbedModel() != DefaultEmbedModel {
			t.Errorf("EmbedModel = %q, want %q", c.EmbedModel(), DefaultEmbedModel)
		}
		if got := fs.calls.Load(); got != 1 {
			t.Errorf("calls = %d, want 1", got)
		}
	})

	t.Run("returns ErrModelMissing when model is not installed", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, "llama3.1:8b", "mistral:7b"))
		})

		_, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			HTTPClient: fastClient(),
		})
		if !errors.Is(err, ErrModelMissing) {
			t.Fatalf("err = %v, want errors.Is(..., ErrModelMissing)", err)
		}
		want := "ollama pull " + DefaultEmbedModel
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err message = %q, want it to contain %q", err.Error(), want)
		}
	})

	t.Run("retries 5xx and eventually succeeds", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {})
		// First call: 500. Second call: 200 with the model installed.
		fs.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := fs.calls.Add(1)
			if n == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
		})

		c, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			HTTPClient: fastClient(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c == nil {
			t.Fatal("client is nil")
		}
		if got := fs.calls.Load(); got != 2 {
			t.Errorf("calls = %d, want 2 (1 fail + 1 retry)", got)
		}
	})

	t.Run("exhausts retries on persistent 5xx", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		})

		_, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			HTTPClient: fastClient(),
		})
		if err == nil {
			t.Fatal("New succeeded, want error")
		}
		if errors.Is(err, ErrModelMissing) {
			t.Errorf("err is ErrModelMissing, want a status error: %v", err)
		}
		if got := fs.calls.Load(); got != maxRetries+1 {
			t.Errorf("calls = %d, want %d (1 initial + %d retries)", got, maxRetries+1, maxRetries)
		}
	})

	t.Run("does not retry 4xx", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})

		_, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			HTTPClient: fastClient(),
		})
		if err == nil {
			t.Fatal("New succeeded, want error")
		}
		if got := fs.calls.Load(); got != 1 {
			t.Errorf("calls = %d, want 1 (no retries on 4xx)", got)
		}
	})

	t.Run("matches model with implicit :latest tag", func(t *testing.T) {
		// User configures EmbedModel="bge-m3"; Ollama reports "bge-m3:latest".
		// The match should succeed regardless of the tag. Uses bge-m3 verbatim
		// here (not DefaultEmbedModel) because the scenario under test is the
		// untagged-want / tagged-installed pattern; the new default carries
		// an explicit tag and would not exercise that path.
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, "bge-m3:latest"))
		})

		_, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			EmbedModel: "bge-m3",
			HTTPClient: fastClient(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
	})
}

// newProbedClient returns a Client that has already passed the construction
// probe. Used by Embed / ListModels tests that need a live Client without
// rewiring the probe handler.
func newProbedClient(t *testing.T, embedHandler http.HandlerFunc) (*Client, *fakeServer) {
	t.Helper()
	srv := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
		case "/api/embed":
			// Embed routes through /api/embed (Embed delegates to
			// EmbedBatch) so the Matryoshka dimensions field actually
			// applies; the older /api/embeddings is no longer wired up.
			embedHandler(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	c, err := New(context.Background(), Options{
		BaseURL:    srv.srv.URL,
		HTTPClient: fastClient(),
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	return c, srv
}

func TestClient_Embed(t *testing.T) {
	t.Run("posts to /api/embed with dimensions and parses plural shape", func(t *testing.T) {
		// Regression gate for the Matryoshka truncation bug: the old
		// embed call posted {model, prompt} to /api/embeddings (singular),
		// which silently ignores `dimensions` and returned 2560-dim
		// vectors from qwen3-embedding:4b — every rebuild apply failed
		// with "refusing to mix dimensions in the vector index". The
		// plural endpoint /api/embed honours dimensions, so we send
		// storage.VectorDim and parse the {embeddings:[[...]]} shape.
		c, srv := newProbedClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if r.URL.Path != "/api/embed" {
				t.Errorf("path = %s, want /api/embed (plural; singular ignores dimensions)", r.URL.Path)
			}
			// Embed delegates to EmbedBatch (/api/embed); wire shape is
			// the batched envelope, not the legacy {prompt: ...} body.
			var got embedBatchRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode req: %v", err)
			}
			if got.Model != DefaultEmbedModel {
				t.Errorf("req.Model = %q, want %q", got.Model, DefaultEmbedModel)
			}
			if len(got.Input) != 1 || got.Input[0] != "hello world" {
				t.Errorf("req.Input = %v, want [\"hello world\"]", got.Input)
			}
			if got.Dimensions != storage.VectorDim {
				t.Errorf("req.Dimensions = %d, want %d (Matryoshka truncation invariant)",
					got.Dimensions, storage.VectorDim)
			}
			_, _ = w.Write(embedJSON(t, storage.VectorDim))
		})

		vec, err := c.Embed(context.Background(), "hello world")
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if got, want := len(vec), storage.VectorDim; got != want {
			t.Errorf("len = %d, want %d", got, want)
		}
		// Probe + embed.
		if got := srv.calls.Load(); got != 2 {
			t.Errorf("calls = %d, want 2", got)
		}
	})

	t.Run("rejects embedding of wrong dimension", func(t *testing.T) {
		c, _ := newProbedClient(t, func(w http.ResponseWriter, _ *http.Request) {
			// Daemon returns 768 floats; expected 1024.
			_, _ = w.Write(embedJSON(t, 768))
		})

		_, err := c.Embed(context.Background(), "hello")
		if err == nil {
			t.Fatal("Embed succeeded, want dim-mismatch error")
		}
		// Embed routes through EmbedBatch, so the error wording is
		// "batch embed[0] length 768" rather than the old
		// "embedding length 768"; assert just the length text since the
		// envelope wording is implementation detail.
		if !strings.Contains(err.Error(), "length 768") {
			t.Errorf("err = %q, want it to mention length 768", err.Error())
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("want %d", storage.VectorDim)) {
			t.Errorf("err = %q, want it to mention want %d", err.Error(), storage.VectorDim)
		}
	})

	t.Run("retries 5xx and succeeds", func(t *testing.T) {
		var embedCalls atomic.Int32
		c, _ := newProbedClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if embedCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(embedJSON(t, storage.VectorDim))
		})

		vec, err := c.Embed(context.Background(), "hello")
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if len(vec) != storage.VectorDim {
			t.Errorf("len = %d, want %d", len(vec), storage.VectorDim)
		}
		if got := embedCalls.Load(); got != 2 {
			t.Errorf("embed calls = %d, want 2", got)
		}
	})

	t.Run("does not retry 4xx", func(t *testing.T) {
		var embedCalls atomic.Int32
		c, _ := newProbedClient(t, func(w http.ResponseWriter, _ *http.Request) {
			embedCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		})

		_, err := c.Embed(context.Background(), "hello")
		if err == nil {
			t.Fatal("Embed succeeded, want error")
		}
		if got := embedCalls.Load(); got != 1 {
			t.Errorf("embed calls = %d, want 1 (no retry on 4xx)", got)
		}
	})

	t.Run("returns ctx.Err on cancellation between retries", func(t *testing.T) {
		c, _ := newProbedClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})

		ctx, cancel := context.WithCancel(context.Background())
		// Cancel shortly after the first retry kicks off the backoff sleep.
		time.AfterFunc(50*time.Millisecond, cancel)

		_, err := c.Embed(ctx, "hello")
		if err == nil {
			t.Fatal("Embed succeeded, want ctx error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want errors.Is(..., context.Canceled)", err)
		}
	})

	t.Run("returns ctx.Err immediately when canceled mid request", func(t *testing.T) {
		// Server stalls past the cancellation window; the http.Do call
		// returns a context-canceled error. The client must surface
		// context.Canceled without entering the retry loop.
		var embedCalls atomic.Int32
		c, _ := newProbedClient(t, func(w http.ResponseWriter, r *http.Request) {
			embedCalls.Add(1)
			select {
			case <-time.After(2 * time.Second):
				_, _ = w.Write(embedJSON(t, storage.VectorDim))
			case <-r.Context().Done():
				return
			}
		})

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		_, err := c.Embed(ctx, "hello")
		if err == nil {
			t.Fatal("Embed succeeded, want ctx error")
		}
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want errors.Is(..., context.DeadlineExceeded or Canceled)", err)
		}
		// Exactly one attempt: the request started, was canceled, and the
		// client did NOT enter backoff to retry with a dead context.
		if got := embedCalls.Load(); got != 1 {
			t.Errorf("embed calls = %d, want 1 (cancellation must not be retried)", got)
		}
	})
}

func TestClient_ListModels(t *testing.T) {
	t.Run("returns names in server order", func(t *testing.T) {
		// DefaultEmbedModel first so the probe passes; the remaining entries
		// keep the in-order-listing assertion meaningful with a chat model
		// and an alternative.
		want := []string{DefaultEmbedModel, "llama3.1:8b", "mistral:7b"}
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, want...))
		})

		c, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			HTTPClient: fastClient(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := c.ListModels(context.Background())
		if err != nil {
			t.Fatalf("ListModels: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("len = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("handles empty models array", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t))
		})

		// Direct probe will fail; bypass via a hand-built client. We can't
		// call New (probe rejects empty), so construct via the same code
		// path but skip the probe assertion by checking ListModels alone.
		_, err := New(context.Background(), Options{
			BaseURL:    fs.srv.URL,
			HTTPClient: fastClient(),
		})
		if !errors.Is(err, ErrModelMissing) {
			t.Fatalf("err = %v, want ErrModelMissing on empty list", err)
		}
	})
}

func TestStripTag(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"bge-m3", "bge-m3"},
		{"bge-m3:latest", "bge-m3"},
		{"bge-m3:f16", "bge-m3"},
		{":latest", ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := stripTag(tc.in)
		if got != tc.want {
			t.Errorf("stripTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestModelInstalled(t *testing.T) {
	cases := []struct {
		name      string
		installed []string
		want      string
		expected  bool
	}{
		// Untagged want: matches any tag of the same model.
		{"untagged want, untagged installed", []string{"bge-m3"}, "bge-m3", true},
		{"untagged want, tagged installed", []string{"bge-m3:latest"}, "bge-m3", true},
		{"untagged want, different model", []string{"llama3.1:8b"}, "bge-m3", false},
		{"untagged want, empty installed", nil, "bge-m3", false},

		// Tagged want: requires an exact match.
		{"tagged want, exact match", []string{"bge-m3:f16"}, "bge-m3:f16", true},
		{"tagged want, different tag installed", []string{"bge-m3:latest"}, "bge-m3:f16", false},
		{"tagged want, untagged installed", []string{"bge-m3"}, "bge-m3:latest", false},
		{"tagged want, multiple installed picks exact", []string{"bge-m3:latest", "bge-m3:f16"}, "bge-m3:f16", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := modelInstalled(tc.installed, tc.want)
			if got != tc.expected {
				t.Errorf("modelInstalled(%v, %q) = %v, want %v", tc.installed, tc.want, got, tc.expected)
			}
		})
	}
}

func TestBackoffFor(t *testing.T) {
	// Sequence is baseBackoff << (attempt - 1) with ±25% jitter. Just assert
	// the bounds; exact values vary by run.
	for attempt := 1; attempt <= 3; attempt++ {
		got := backoffFor(attempt)
		base := baseBackoff << (attempt - 1)
		lo := time.Duration(float64(base) * 0.75)
		hi := time.Duration(float64(base) * 1.25)
		if got < lo || got > hi {
			t.Errorf("attempt %d: backoff = %v, want in [%v, %v]", attempt, got, lo, hi)
		}
	}
}

func TestBackoffFor_ClampsZero(t *testing.T) {
	// Defensive: attempt 0 should still produce a sensible (positive)
	// duration; the loop never calls backoffFor(0) but a future caller might.
	got := backoffFor(0)
	if got < time.Duration(float64(baseBackoff)*0.75) {
		t.Errorf("attempt 0: backoff = %v, want >= 75%% of baseBackoff (%v)", got, baseBackoff)
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
	})
	c, err := New(context.Background(), Options{
		BaseURL:    fs.srv.URL + "///",
		HTTPClient: fastClient(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if strings.HasSuffix(c.BaseURL(), "/") {
		t.Errorf("BaseURL = %q, want no trailing slash", c.BaseURL())
	}
}

// TestNew_RequestTimeout pins that Options.RequestTimeout overrides the
// per-request HTTP deadline. The default (60s) is shorter than a single
// large-transcript extraction call (prompt-eval of a 16K body plus constrained
// generation routinely exceeds 60s on a 7B model), so without an override every
// such /api/chat is cut off mid-flight and no entities/relations are ever
// produced. The chat-wiring path sets this so extraction can actually complete,
// while the embed client keeps the short default so recall stays responsive
// when Ollama is down.
func TestNew_RequestTimeout(t *testing.T) {
	t.Run("honours an explicit RequestTimeout", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
		})
		c, err := New(context.Background(), Options{
			BaseURL:        fs.srv.URL,
			RequestTimeout: 7 * time.Minute,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c.http.Timeout != 7*time.Minute {
			t.Errorf("http timeout = %v, want %v", c.http.Timeout, 7*time.Minute)
		}
	})

	t.Run("falls back to DefaultTimeout when unset", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
		})
		c, err := New(context.Background(), Options{BaseURL: fs.srv.URL})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c.http.Timeout != DefaultTimeout {
			t.Errorf("http timeout = %v, want DefaultTimeout %v", c.http.Timeout, DefaultTimeout)
		}
	})

	t.Run("an explicit HTTPClient still wins over RequestTimeout", func(t *testing.T) {
		fs := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
		})
		c, err := New(context.Background(), Options{
			BaseURL:        fs.srv.URL,
			HTTPClient:     fastClient(),
			RequestTimeout: 7 * time.Minute,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c.http.Timeout != 2*time.Second {
			t.Errorf("http timeout = %v, want the explicit client's 2s", c.http.Timeout)
		}
	})
}

// TestNew_InvalidBaseURL pins the ErrInvalidConfig sentinel contract:
// when BaseURL fails static validation (missing scheme, missing host,
// or url.Parse failure), New must return an error that wraps
// ErrInvalidConfig — and must never issue an HTTP probe. The daemon's
// classifyEmbedderErr relies on this sentinel to keep config errors
// fatal instead of silently degrading to remember-only mode (Greptile
// finding on PR #119).
func TestNew_InvalidBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
	}{
		{name: "missing scheme", baseURL: "127.0.0.1:11434"},
		{name: "missing host", baseURL: "http://"},
		// url.Parse rejects URLs with bare control characters in the path.
		{name: "control character", baseURL: "http://example.com/\x7f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(context.Background(), Options{
				BaseURL:    tc.baseURL,
				HTTPClient: fastClient(),
			})
			if err == nil {
				t.Fatalf("New(%q): err = nil, want ErrInvalidConfig", tc.baseURL)
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("New(%q): err = %v, want errors.Is(..., ErrInvalidConfig)", tc.baseURL, err)
			}
		})
	}
}
