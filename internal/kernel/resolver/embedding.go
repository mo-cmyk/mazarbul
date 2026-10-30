package resolver

import (
	"context"
	"fmt"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// embedName returns the embedding for the mention's display name. Used for
// name-vector nearest-neighbour matching in step 1 of the matching pipeline.
// The episode's pre-embed batch (preembed.go) is consulted first; the
// single-text call is the fallback, returning identical vectors.
func (r *Resolver) embedName(ctx context.Context, m ontology.Mention, pre *preEmbeds) ([]float32, error) {
	if vec, ok := pre.lookup(m.DisplayName); ok {
		return vec, nil
	}
	vec, err := r.embedder.Embed(ctx, m.DisplayName)
	if err != nil {
		return nil, fmt.Errorf("resolver: embed name %q: %w", m.DisplayName, err)
	}
	return vec, nil
}

// embedSummary returns the embedding for the mention's DisambiguationCtx +
// ExtractionText concatenation. Used for summary-vector matching in step 3 of
// the matching pipeline and for Entity summary-vector regeneration. Consults
// the pre-embed batch first, like embedName.
func (r *Resolver) embedSummary(ctx context.Context, m ontology.Mention, pre *preEmbeds) ([]float32, error) {
	text := buildMentionSummaryText(m)
	if vec, ok := pre.lookup(text); ok {
		return vec, nil
	}
	vec, err := r.embedder.Embed(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("resolver: embed summary for %q: %w", m.DisplayName, err)
	}
	return vec, nil
}

// buildMentionSummaryText concatenates the disambiguation context and extraction
// text of a single Mention into the text fed to the embedder for step 3.
func buildMentionSummaryText(m ontology.Mention) string {
	parts := make([]string, 0, 2)
	if m.DisambiguationCtx != "" {
		parts = append(parts, m.DisambiguationCtx)
	}
	if m.ExtractionText != "" {
		parts = append(parts, m.ExtractionText)
	}
	return strings.Join(parts, " ")
}

// minSummaryTextLen is the lower bound below which the combined
// disambiguation context + extraction text is treated as too sparse to
// produce a usable summary embedding. 16 characters covers single-word
// mentions ("Frodo", "Berlin") that still carry a useful disambiguation
// context elsewhere; below that the embedding collapses to the noise floor
// and matches arbitrary entities at threshold values calibrated for real
// text.
const minSummaryTextLen = 16

// mentionHasMeaningfulSummary reports whether the Mention carries enough
// text for a useful summary embedding. Used by summaryEmbMatch to short-
// circuit before issuing a wasted embedder call + vec0 search.
func mentionHasMeaningfulSummary(m ontology.Mention) bool {
	text := strings.TrimSpace(buildMentionSummaryText(m))
	return len(text) >= minSummaryTextLen
}

// entitySummaryText assembles the text fed to the summary-v1 prompt and used
// as the input for the summary-vector embedding for an Entity. It concatenates
// the ExtractionText spans of the provided Mentions, deduplicating adjacent
// identical strings. At most cfg.SummaryMentionWindow entries are used, taking
// the last K from the slice (assumed most-recent-last).
func entitySummaryText(mentions []ontology.Mention) string {
	if len(mentions) == 0 {
		return ""
	}

	seen := make(map[string]struct{}, len(mentions))
	parts := make([]string, 0, len(mentions))
	for _, m := range mentions {
		t := strings.TrimSpace(m.ExtractionText)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		parts = append(parts, t)
	}
	return strings.Join(parts, "\n")
}
