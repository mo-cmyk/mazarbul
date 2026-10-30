package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
)

// newDimsClient is the local probed-client factory for the dimensions tests.
// Unlike newProbedClient it advertises DefaultEmbedModel in /api/tags (so
// the probe passes regardless of which constant the default is currently
// pinned to) and routes BOTH /api/embeddings and /api/embed to the same
// handler, letting one test fixture cover the single + batch paths.
func newDimsClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	fs := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			// Advertise the model under its exact configured tag so the
			// probe matches whether the default is tagged (e.g.
			// "qwen3-embedding:4b") or bare (e.g. "bge-m3"). Appending
			// ":latest" would force tagsHasTag=true and break the tagged
			// default's exact-match rule in modelInstalled.
			_, _ = w.Write(tagsJSON(t, DefaultEmbedModel))
		case "/api/embeddings", "/api/embed":
			handler(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	c, err := New(context.Background(), Options{
		BaseURL:    fs.srv.URL,
		HTTPClient: fastClient(),
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	return c
}

// TestDefaultEmbedModel pins the canonical default. The constant is the
// single source of truth quoted by the init wizard, the doctor check, the
// userconfig defaults, and a dozen tests; bumping it here cascades to all of
// those call sites via the constant, but a literal here keeps the assertion
// loud when a future change moves it accidentally.
//
// Pinned to "snowflake-arctic-embed2" (Arctic Embed L v2.0): multilingual
// (German + English) retrieval with a POSITIVE entity-dedup margin on our gold
// set, and a native 1024-dim output that equals storage.VectorDim — so no
// Matryoshka truncation is needed and it co-resides with the 7B chat model
// under OLLAMA_MAX_LOADED_MODELS=2 without GPU eviction thrash.
func TestDefaultEmbedModel(t *testing.T) {
	if got, want := DefaultEmbedModel, "snowflake-arctic-embed2"; got != want {
		t.Errorf("DefaultEmbedModel = %q, want %q", got, want)
	}
}

// TestEmbed_SendsDimensions proves the single-text Embed call posts
// "dimensions": storage.VectorDim in its JSON body. The default model
// (snowflake-arctic-embed2) emits storage.VectorDim natively, so the field is
// a no-op pass-through today; it is sent unconditionally so a future model
// with a larger native vector (a Matryoshka model like the former
// qwen3-embedding:4b at 2560-dim) is truncated to storage.VectorDim rather
// than returning an oversized vector that fails the boundary dim-check.
func TestEmbed_SendsDimensions(t *testing.T) {
	var got struct {
		Model      string   `json:"model"`
		Input      []string `json:"input"`
		Dimensions int      `json:"dimensions"`
	}
	c := newDimsClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Embed delegates to EmbedBatch, so the wire endpoint is the
		// batched /api/embed regardless of single-text input. The path
		// matters here because /api/embeddings would silently ignore
		// "dimensions" and the Matryoshka prefix would not apply.
		if r.URL.Path != "/api/embed" {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		_, _ = w.Write(embedJSON(t, storage.VectorDim))
	})

	if _, err := c.Embed(context.Background(), "hello"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got.Dimensions != storage.VectorDim {
		t.Errorf("req.Dimensions = %d, want %d (Matryoshka prefix must be requested explicitly)",
			got.Dimensions, storage.VectorDim)
	}
}

// TestEmbedBatch_SendsDimensions is the batch counterpart. Same rationale:
// the batched /api/embed endpoint needs the dimensions field too, or batched
// calls would return native-dim vectors that fail the per-vector dim check.
func TestEmbedBatch_SendsDimensions(t *testing.T) {
	var got struct {
		Model      string   `json:"model"`
		Input      []string `json:"input"`
		Dimensions int      `json:"dimensions"`
	}
	c := newDimsClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		_, _ = w.Write(embedBatchJSON(t, storage.VectorDim, len(got.Input)))
	})

	if _, err := c.EmbedBatch(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if got.Dimensions != storage.VectorDim {
		t.Errorf("batch req.Dimensions = %d, want %d", got.Dimensions, storage.VectorDim)
	}
}
