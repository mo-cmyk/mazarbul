// Package reranker implements kernel.Reranker over an LLM chat provider.
// The reranker is invoked after RRF fusion (HybridQuery.collectHits) and
// re-orders the surviving hits by topical relevance to the user's query.
//
// Architectural choice (per PLAN.md W1.5): we drive the local chat
// provider (qwen2.5:7b via Ollama by default) with a single
// structured-output call. This adds zero new model dependencies, runs
// fully on-device, and shares the prompt-registry pattern that already
// powers extractor / composer / disambiguate. A future cross-encoder
// (e.g. bge-reranker-v2-m3) can drop in behind the same kernel.Reranker
// interface without API churn.
package reranker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// Config controls the reranker's behaviour. Zero values are safe; see
// the field godoc for the applied defaults.
type Config struct {
	// Model is the chat model identifier passed to the ChatProvider.
	// Defaults to "qwen2.5:7b" (the M1 local chat default).
	Model string

	// MaxTokens caps the model response. Defaults to 512, enough for
	// "{\"order\":[0,1,...,19]}" plus slack.
	MaxTokens int

	// Temperature is the sampling temperature. Defaults to 0.0; the
	// rerank task is deterministic ranking, not creative generation.
	Temperature float64
}

// ApplyDefaults fills zero-valued fields with their defaults.
func (c *Config) ApplyDefaults() {
	if c.Model == "" {
		c.Model = "qwen2.5:7b"
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = 512
	}
}

// LLMReranker wraps a kernel.ChatProvider and implements kernel.Reranker.
type LLMReranker struct {
	cfg    Config
	chat   kernel.ChatProvider
	logger *zap.Logger
}

// New constructs an LLMReranker. Chat and logger must be non-nil.
func New(cfg Config, chat kernel.ChatProvider, logger *zap.Logger) (*LLMReranker, error) {
	if chat == nil {
		return nil, errors.New("reranker: chat provider is required")
	}
	if logger == nil {
		return nil, errors.New("reranker: logger is required")
	}
	cfg.ApplyDefaults()
	return &LLMReranker{cfg: cfg, chat: chat, logger: logger}, nil
}

// Rerank implements kernel.Reranker. On any error (LLM unreachable,
// malformed response, context cancellation) the original ordering is
// returned alongside the error so the caller can choose to log and
// proceed rather than fail the recall.
func (r *LLMReranker) Rerank(ctx context.Context, query string, hits []kernel.Hit) ([]kernel.Hit, error) {
	if len(hits) <= 1 {
		return hits, nil
	}

	prompt, err := buildRerankPrompt(query, hits)
	if err != nil {
		return hits, fmt.Errorf("reranker: build prompt: %w", err)
	}

	resp, err := r.chat.ToolUse(ctx, kernel.ToolUseRequest{
		Model:       r.cfg.Model,
		System:      rerankSystem,
		Messages:    []kernel.Message{{Role: "user", Content: prompt}},
		Tool:        rerankToolSchema(),
		MaxTokens:   r.cfg.MaxTokens,
		Temperature: r.cfg.Temperature,
	})
	if err != nil {
		return hits, fmt.Errorf("reranker: chat: %w", err)
	}
	if resp.ToolUse == nil {
		return hits, errors.New("reranker: provider returned no tool invocation")
	}

	order, err := decodeRerankOrder(resp.ToolUse.Input, len(hits))
	if err != nil {
		r.logger.Warn("Reranker: malformed order, falling back to original ranking",
			zap.Error(err),
		)
		return hits, err
	}

	out := make([]kernel.Hit, 0, len(hits))
	for _, idx := range order {
		out = append(out, hits[idx])
	}
	return out, nil
}

// rerankSystem is the system prompt for the structured-output call. Kept
// short to keep the input-token budget low; the schema in the tool does
// most of the constraint work. The explicit "MUST return ALL N indices"
// language addresses qwen2.5:7b's tendency to silently drop 1-3 indices
// from longer lists (observed routinely at N=20).
const rerankSystem = `You are a relevance ranker. You will receive a user question and a numbered list of N candidate passages indexed 0..N-1. Return the indices reordered from most to least relevant to the question.

Strict rules:
- You MUST return ALL N indices. Count them before returning.
- Each index must appear EXACTLY ONCE.
- Never invent an index that wasn't in the input range.
- If two passages tie, keep the lower-indexed one earlier.`

// rerankInputSchema documents the structured output expected from the
// model. The kernel does not enforce the schema beyond the JSON shape;
// the provider's tool-use surface validates structurally.
const rerankInputSchema = `{
  "type": "object",
  "properties": {
    "order": {
      "type": "array",
      "items": {"type": "integer", "minimum": 0},
      "description": "Original passage indices reordered from most to least relevant. Must be a permutation of [0, n-1]."
    }
  },
  "required": ["order"],
  "additionalProperties": false
}`

func rerankToolSchema() kernel.ToolSchema {
	return kernel.ToolSchema{
		Name:        "rerank",
		Description: "Return the relevance-ordered indices of the candidate passages.",
		InputSchema: json.RawMessage(rerankInputSchema),
	}
}

// buildRerankPrompt formats the user-side prompt: the question plus a
// numbered list of candidate snippets. Snippets are truncated to keep
// the token budget bounded; the index is what we care about, not the
// full text.
func buildRerankPrompt(query string, hits []kernel.Hit) (string, error) {
	if len(hits) == 0 {
		return "", errors.New("no hits")
	}
	const maxSnippet = 280
	var b []byte
	b = append(b, "Question: "...)
	b = append(b, query...)
	b = append(b, "\n\nCandidates:\n"...)
	for i, h := range hits {
		snippet := h.Snippet
		if len(snippet) > maxSnippet {
			snippet = snippet[:maxSnippet] + "..."
		}
		line := fmt.Sprintf("%d. %s\n", i, snippet)
		b = append(b, line...)
	}
	b = append(b, fmt.Sprintf("\nThere are exactly %d candidates (indices 0..%d). Return all %d indices reordered from most to least relevant via the rerank tool.",
		len(hits), len(hits)-1, len(hits))...)
	return string(b), nil
}

// decodeRerankOrder extracts and validates the order array, then PADS any
// missing indices onto the tail in their original positions.
//
// Real-world LLMs (qwen2.5:7b at 7B params, especially) routinely return
// permutations that are missing one or two indices when ranking 10-20
// similar candidates. The strict "length must equal n" check was rejecting
// those as malformed and dropping back to the RRF ordering, throwing away
// the LLM's partial work.
//
// We honor the LLM's verdict for the indices it returned (those are the
// ones it judged most relevant) and append the missing indices in their
// original input order on the tail. The interpretation: "the LLM didn't
// mention them" is a soft "less relevant than what it did mention".
//
// We still HARD-reject duplicates and out-of-range indices: those are
// hallucinations, not omissions, and silently coercing them would corrupt
// the output. The caller (LLMReranker.Rerank) returns the original
// ordering plus error in that case, and the recall pipeline degrades to
// the RRF ranking.
func decodeRerankOrder(raw json.RawMessage, n int) ([]int, error) {
	var out struct {
		Order []int `json:"order"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode order: %w", err)
	}
	if len(out.Order) > n {
		return nil, fmt.Errorf("order length = %d, want at most %d", len(out.Order), n)
	}
	seen := make(map[int]struct{}, n)
	for _, idx := range out.Order {
		if idx < 0 || idx >= n {
			return nil, fmt.Errorf("order contains out-of-range index %d (n=%d)", idx, n)
		}
		if _, dup := seen[idx]; dup {
			return nil, fmt.Errorf("order contains duplicate index %d", idx)
		}
		seen[idx] = struct{}{}
	}
	if len(out.Order) == n {
		return out.Order, nil
	}
	// Short permutation: pad missing indices onto the tail in their
	// original input order.
	full := make([]int, 0, n)
	full = append(full, out.Order...)
	for i := range n {
		if _, present := seen[i]; !present {
			full = append(full, i)
		}
	}
	return full, nil
}
