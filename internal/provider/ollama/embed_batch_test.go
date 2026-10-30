package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
)

// embedBatchJSON returns a canned /api/embed response body for a batch of
// inputs. Each row carries an embedding of the requested length.
func embedBatchJSON(t *testing.T, dim int, count int) []byte {
	t.Helper()
	rows := make([][]float32, count)
	for i := range rows {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(i*dim+j) / float32(count*dim)
		}
		rows[i] = v
	}
	b, err := json.Marshal(struct {
		Embeddings [][]float32 `json:"embeddings"`
	}{Embeddings: rows})
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	return b
}

// TestEmbedBatch_SingleRoundTrip proves EmbedBatch posts a slice of inputs to
// /api/embed and returns one vector per input in a single HTTP call. The
// pre-fix shape would have forced N calls to /api/embeddings via N invocations
// of Embed; the test asserts exactly one round trip happens for N inputs.
func TestEmbedBatch_SingleRoundTrip(t *testing.T) {
	const inputs = 5

	var apiEmbedCalls atomic.Int32
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel, "llama3.1:8b"))
		case "/api/embed":
			apiEmbedCalls.Add(1)
			var body struct {
				Model string   `json:"model"`
				Input []string `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode batch request: %v", err)
			}
			if got, want := len(body.Input), inputs; got != want {
				t.Errorf("batch input length = %d, want %d", got, want)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(embedBatchJSON(t, storage.VectorDim, len(body.Input)))
		default:
			t.Errorf("unexpected hit: %s", r.URL.Path)
		}
	})

	c, err := New(context.Background(), Options{
		BaseURL:    fs.srv.URL,
		HTTPClient: fastClient(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	texts := make([]string, inputs)
	for i := range texts {
		texts[i] = "input " + string(rune('A'+i))
	}
	vecs, err := c.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if got := apiEmbedCalls.Load(); got != 1 {
		t.Errorf("/api/embed call count = %d, want 1 (one round trip for the whole batch)", got)
	}
	if got, want := len(vecs), inputs; got != want {
		t.Errorf("returned vectors = %d, want %d", got, want)
	}
	for i, v := range vecs {
		if len(v) != storage.VectorDim {
			t.Errorf("vec[%d] dim = %d, want %d", i, len(v), storage.VectorDim)
		}
	}
}

// TestEmbedBatch_Empty returns immediately without an HTTP round trip when the
// caller passes an empty slice. Saves a wasted call from defensive callers.
func TestEmbedBatch_Empty(t *testing.T) {
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
		case "/api/embed":
			t.Error("EmbedBatch with empty input must not hit /api/embed")
		}
	})
	c, err := New(context.Background(), Options{
		BaseURL:    fs.srv.URL,
		HTTPClient: fastClient(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vecs, err := c.EmbedBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("EmbedBatch(nil): %v", err)
	}
	if len(vecs) != 0 {
		t.Errorf("EmbedBatch(nil) = %d vectors, want 0", len(vecs))
	}
}

// TestEmbedBatch_RejectsWhenEmbedDisabled mirrors TestEmbed_RejectsWhenEmbedDisabled:
// chat-only clients must reject batch calls just like single calls.
func TestEmbedBatch_RejectsWhenEmbedDisabled(t *testing.T) {
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = w.Write(tagsJSON(t, "llama3.1:8b"))
			return
		}
		t.Errorf("unexpected hit: %s", r.URL.Path)
	})
	c, err := New(context.Background(), Options{
		BaseURL:    fs.srv.URL,
		ChatModel:  "llama3.1:8b",
		HTTPClient: fastClient(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.EmbedBatch(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("EmbedBatch succeeded, want error")
	}
	if !strings.Contains(err.Error(), "embed not configured") {
		t.Errorf("err = %q, want it to mention embed not configured", err.Error())
	}
}
