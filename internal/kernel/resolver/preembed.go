package resolver

// preembed.go batches the matching pipeline's embeddings per episode.
//
// Before this seam, resolveMentions issued up to 2 sequential Embed calls
// per mention (display name for step-2 nameEmbMatch, summary text for
// step-3 summaryEmbMatch) — 2N round trips that each queue behind whatever
// the extraction workers have in flight on the shared local engine. Under
// a burst the queue wait dominates the call. One EmbedBatch round trip
// computes the same vectors up front; the per-mention paths then consume
// them from memory and fall back to single Embed calls only when batching
// is unavailable (non-Batcher embedder) or failed.
//
// Quality is untouched by construction: EmbedBatch returns the identical
// vectors Embed would (same model, same endpoint, same cache), so every
// threshold comparison sees the same numbers.

import (
	"context"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/embedder"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// preEmbeds is the per-episode embedding snapshot. Read-only after
// construction; a nil *preEmbeds is valid and reports every lookup as a
// miss, so call sites need no nil guards.
type preEmbeds struct {
	vecs map[string][]float32
}

// lookup returns the pre-computed vector for text, if the batch covered it.
func (p *preEmbeds) lookup(text string) ([]float32, bool) {
	if p == nil {
		return nil, false
	}
	v, ok := p.vecs[text]
	return v, ok
}

// preEmbedMentions batches every embedding the matching pipeline MAY need
// for the episode's mentions: each display name (step 2) and each
// meaningful summary text (step 3). Texts deduplicate; exact-name matches
// in step 1 can make some vectors unnecessary, which is an acceptable
// trade — one batched round trip with a few spare texts beats 2N queued
// round trips, and the embedding cache keeps any spare from costing twice.
//
// Returns nil — never an error — when there is nothing to gain (no
// batcher, <2 unique texts) or when the batch call failed; the per-mention
// fallback paths preserve behavior exactly.
func (r *Resolver) preEmbedMentions(ctx context.Context, mentions []ontology.Mention) *preEmbeds {
	batcher, ok := r.embedder.(embedder.Batcher)
	if !ok || len(mentions) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, 2*len(mentions))
	texts := make([]string, 0, 2*len(mentions))
	add := func(t string) {
		if t == "" {
			return
		}
		if _, dup := seen[t]; dup {
			return
		}
		seen[t] = struct{}{}
		texts = append(texts, t)
	}
	for _, m := range mentions {
		add(m.DisplayName)
		if mentionHasMeaningfulSummary(m) {
			add(buildMentionSummaryText(m))
		}
	}
	if len(texts) < 2 {
		// A single text gains nothing over the plain Embed path.
		return nil
	}

	vecs, err := batcher.EmbedBatch(ctx, texts)
	if err != nil || len(vecs) != len(texts) {
		r.logger.Warn("Resolver: pre-embed batch failed; falling back to per-mention embeds",
			zap.Int("texts", len(texts)),
			zap.Error(err),
		)
		return nil
	}
	out := make(map[string][]float32, len(texts))
	for i, t := range texts {
		out[t] = vecs[i]
	}
	return &preEmbeds{vecs: out}
}
