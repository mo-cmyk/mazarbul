package ollama

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
)

// embedBatchRequest is the JSON shape POSTed to /api/embed. The newer
// endpoint accepts an array of inputs and returns one vector per input in a
// single round trip, eliminating per-call HTTP + Ollama queue overhead for
// the N-mention / N-entity case the write path hits during haystack
// ingestion. Unlike the older /api/embeddings endpoint it also honours the
// dimensions field for Matryoshka prefix truncation.
type embedBatchRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions"`
	KeepAlive  string   `json:"keep_alive,omitempty"`
}

// embedBatchResponse mirrors Ollama's /api/embed response. The order of
// returned vectors matches the order of inputs verbatim.
type embedBatchResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed sends text to the configured embed model and returns the resulting
// dense embedding as a []float32. The returned slice always has length
// storage.VectorDim; an embedding of any other size is rejected so a model
// mismatch surfaces here rather than poisoning the vector index.
//
// Embed retries transport errors and HTTP 5xx twice with exponential backoff
// (200ms, 400ms; ±25% jitter), does not retry 4xx, and respects ctx
// cancellation between attempts. Total wall-clock budget per call is bounded
// by the HTTPClient's Timeout (60s by default) and the backoff schedule.
//
// Embed returns an error when the client was constructed in chat-only mode
// (Options.ChatModel set, Options.EmbedModel empty). Callers who want both
// capabilities must set both fields at New time.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	// Delegate to EmbedBatch with a single-element slice so the embed
	// surface is a single code path against /api/embed. Only /api/embed
	// honours the dimensions field; the older /api/embeddings endpoint
	// silently ignores it. The default model (snowflake-arctic-embed2) emits
	// storage.VectorDim natively, so dimensions is a no-op pass-through today —
	// but it is still sent so a future model with a larger native vector (a
	// Matryoshka model like the former qwen3-embedding:4b at 2560-dim) is
	// truncated to the stored dimension instead of failing the VectorDim
	// check downstream.
	vecs, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("ollama: embed returned %d vectors for 1 input (model %q)", len(vecs), c.model)
	}
	return vecs[0], nil
}

// EmbedBatch sends a slice of texts to the configured embed model via Ollama's
// /api/embed endpoint and returns one []float32 per input, in matching order.
// Replaces N calls to Embed with one HTTP round trip; the saving on a haystack
// of N mentions + N entity names is N-1 fewer round trips and (more
// importantly) one queue entry through OLLAMA_NUM_PARALLEL instead of N.
//
// Empty input is a no-op: returns nil, nil without an HTTP call. Returns an
// error when the client was constructed in chat-only mode (Options.ChatModel
// set, Options.EmbedModel empty). Each returned vector must have length
// storage.VectorDim; a mismatch is fatal at the boundary (same invariant as
// Embed).
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if c.model == "" {
		return nil, errors.New("ollama: embed not configured; pass Options.EmbedModel to New")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	// keep_alive=30m pins the embed model in VRAM so haystack ingest
	// doesn't pay reload cost when chat extraction interleaves embed
	// calls. Default Ollama eviction is 5 min; the chat model
	// regularly displaces the embed model under burst load.
	req := embedBatchRequest{
		Model:      c.model,
		Input:      texts,
		Dimensions: storage.VectorDim,
		KeepAlive:  "30m",
	}
	var resp embedBatchResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/embed", req, &resp); err != nil {
		return nil, err
	}
	if got, want := len(resp.Embeddings), len(texts); got != want {
		return nil, fmt.Errorf(
			"ollama: batch embed returned %d vectors for %d inputs (model %q)",
			got, want, c.model,
		)
	}
	for i, v := range resp.Embeddings {
		if got := len(v); got != storage.VectorDim {
			return nil, fmt.Errorf(
				"ollama: batch embed[%d] length %d, want %d (model %q)",
				i, got, storage.VectorDim, c.model,
			)
		}
	}
	return resp.Embeddings, nil
}
