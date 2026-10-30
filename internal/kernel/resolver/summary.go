package resolver

import (
	"context"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// summaryWindowText assembles the excerpt text for an entity's summary from
// the last cfg.SummaryMentionWindow mentions (most-recent-last convention).
// Shared by the LLM regen prompt and the first-summary direct seed so both
// paths see the identical input window.
func (r *Resolver) summaryWindowText(mentions []ontology.Mention) string {
	window := mentions
	if len(window) > r.cfg.SummaryMentionWindow {
		window = window[len(window)-r.cfg.SummaryMentionWindow:]
	}
	return entitySummaryText(window)
}

// shouldRegenSummary returns true when an Entity's summary needs regeneration
// based on two thresholds:
//   - N = cfg.SummaryRegen new Mentions have accumulated since the last regen.
//   - cfg.SummaryRegenInterval has elapsed since SummaryUpdateTime.
func (r *Resolver) shouldRegenSummary(entity ontology.Entity, newMentionCount int) bool {
	if newMentionCount >= r.cfg.SummaryRegen {
		return true
	}
	if entity.SummaryUpdateTime == nil {
		// No prior regen; regenerate once we have at least one mention.
		return newMentionCount > 0
	}
	return time.Since(*entity.SummaryUpdateTime) >= r.cfg.SummaryRegenInterval
}

// regenSummary calls ChatProvider.Complete with the summary-v1 prompt over the
// entity's most recent K Mentions and returns the new summary text. A
// process-wide sync.Map prevents thundering-herd stacks during eval-harness
// backfill: if a regen for the same entity UID is already in flight this call
// returns an empty string and a nil error, signalling the caller to skip.
func (r *Resolver) regenSummary(ctx context.Context, entity ontology.Entity, mentions []ontology.Mention) (string, error) {
	// Guard against concurrent regens for the same entity.
	if _, loaded := r.regenGuard.LoadOrStore(entity.UID, struct{}{}); loaded {
		// Another goroutine is already regenerating this entity; skip.
		return "", nil
	}
	defer r.regenGuard.Delete(entity.UID)

	excerpts := r.summaryWindowText(mentions)
	if excerpts == "" {
		return "", nil
	}

	system, err := r.promptReg.Render(ctx, summaryTemplateName, summaryTemplateVersion, prompts.RenderVars{
		"displayName": entity.DisplayName,
		"kind":        string(entity.Kind),
		"excerpts":    excerpts,
	})
	if err != nil {
		return "", fmt.Errorf("regen summary: render prompt: %w", err)
	}

	resp, err := r.chat.Complete(ctx, kernel.CompleteRequest{
		Model:  r.cfg.Model,
		System: system,
		Messages: []kernel.Message{
			{Role: "user", Content: "Write the summary paragraph now."},
		},
		MaxTokens: r.cfg.SummaryMaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("regen summary: complete: %w", err)
	}
	return resp.Content, nil
}
