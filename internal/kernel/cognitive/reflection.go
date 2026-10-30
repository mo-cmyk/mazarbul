package cognitive

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// reflectionSource is the Event.Source stamped on synthesized insights.
const reflectionSource = "reflection/v1"

// insightsDomain is the vector domain insight embeddings are stored in, and
// the domain checked for semantic duplicates before appending a new insight.
// MUST match sqlite.DomainInsights / the dispatcher's scheduleDomainEmbedding call.
const insightsDomain = "insights"

// reflectionDedupThreshold is the cosine-similarity floor above which a
// candidate insight is treated as a duplicate of an existing one and skipped.
// Tuned for snowflake-arctic-embed2 paraphrase distance: 0.88 catches near-
// paraphrases that share a seed cluster but differ by a few words, while
// staying above ~0.85 to avoid over-merging genuinely distinct insights.
// Consolidation uses 0.92 (tighter, intentional — memories are longer and
// more distinctive than 1-2 sentence reflection insights).
const reflectionDedupThreshold = 0.88

// reflectMaxSeedsDefault caps how many entity neighborhoods one reflection pass
// synthesizes from, bounding the LLM calls per tick.
const reflectMaxSeedsDefault = 5

// reflectNeighborLimit caps the neighborhood expansion per seed.
const reflectNeighborLimit = 20

// insightInputSchema constrains the structured output of the reflection tool.
//
//nolint:gochecknoglobals // immutable JSON Schema literal, package-level by design
var insightInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "statement": {"type": "string", "description": "The synthesized insight, one or two sentences."},
    "insightType": {"type": "string", "description": "Open taxonomy: pattern | preference | summary."},
    "sourceUids": {"type": "array", "items": {"type": "string"}, "description": "UIDs from the provided cluster that support the insight. Cite only UIDs shown to you."},
    "confidence": {"type": "number", "description": "Confidence in the insight, 0..1."}
  },
  "required": ["statement", "insightType", "sourceUids", "confidence"]
}`)

const reflectSystemPrompt = "You are a memory-reflection module. Given a cluster of related " +
	"entities and the edges connecting them, synthesize ONE durable, non-obvious insight about " +
	"the cluster. Cite the supporting records by their uid in sourceUids — cite ONLY uids that " +
	"appear in the cluster shown to you; never invent a uid. If nothing meaningful can be said, " +
	"return an empty statement."

// reflectInput is the model's structured insight output.
type reflectInput struct {
	Statement   string   `json:"statement"`
	InsightType string   `json:"insightType"`
	SourceUids  []string `json:"sourceUids"`
	Confidence  float64  `json:"confidence"`
}

// Reflector synthesizes insights from entity neighborhoods (the reflection job).
type Reflector struct {
	chat     kernel.ChatProvider
	nav      *navigate.Navigator
	appender Appender
	deduper  Deduper
	model    string
	maxSeeds int
	logger   *zap.Logger
}

// NewReflector builds a reflection job. Nav should already be scope-narrowed
// (e.g. via Navigator.WithScopes) by the caller. Deduper may be nil (dedup is
// then skipped — safe degradation when the embedder is unavailable).
func NewReflector(chat kernel.ChatProvider, nav *navigate.Navigator, appender Appender, deduper Deduper, model string, logger *zap.Logger) *Reflector {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Reflector{
		chat:     chat,
		nav:      nav,
		appender: appender,
		deduper:  deduper,
		model:    model,
		maxSeeds: reflectMaxSeedsDefault,
		logger:   logger,
	}
}

// Run executes one reflection pass: it picks recent seed entities, clusters
// each via a 1-2 hop neighborhood walk, asks the LLM to synthesize an insight
// citing the cluster's records, and appends a well-formed insightSynthesized
// event per accepted insight. A tripped breaker stops the pass cleanly
// (Skipped="breaker-open"); other per-seed errors are logged and skipped.
func (r *Reflector) Run(ctx context.Context) (Result, error) {
	if r.chat == nil || r.nav == nil || r.appender == nil {
		return Result{Skipped: "not-wired"}, nil
	}
	seeds, err := r.nav.ListEntities(ctx, navigate.EntityFilter{Limit: r.maxSeeds})
	if err != nil {
		return Result{}, fmt.Errorf("cognitive: reflect: list seed entities: %w", err)
	}
	if len(seeds) == 0 {
		return Result{Skipped: "no-candidates"}, nil
	}

	var appended []string
	for i := range seeds {
		uid, serr := r.synthesizeForSeed(ctx, seeds[i])
		if serr != nil {
			if isBreakerOpen(serr) {
				r.logger.Debug("cognitive: reflect: breaker open; deferring to next tick")
				return Result{AppendedEventUIDs: appended, Skipped: "breaker-open"}, nil
			}
			r.logger.Warn("cognitive: reflect: seed failed; skipping",
				zap.String("seedUid", seeds[i].UID), zap.Error(serr))
			continue
		}
		if uid != "" {
			appended = append(appended, uid)
		}
	}
	if len(appended) == 0 {
		return Result{Skipped: "no-candidates"}, nil
	}
	return Result{AppendedEventUIDs: appended}, nil
}

// synthesizeForSeed clusters one seed and, if the LLM returns a usable insight
// citing in-cluster records, appends it. Returns the appended event UID, or ""
// when the seed produced nothing (empty neighborhood, model declined, malformed,
// hallucinated output, or the seed is already covered by a hot insight).
func (r *Reflector) synthesizeForSeed(ctx context.Context, seed navigate.Entity) (string, error) {
	// Saturation guard: skip seeds that already have a live (hot) insight so
	// repeated reflection passes do not re-synthesize the same cluster. The
	// seed reopens naturally once its insight decays to 'demoted'.
	if hot, herr := r.nav.HasHotInsightForSeed(ctx, seed.UID); herr != nil {
		r.logger.Warn("cognitive: reflect: saturation check failed; proceeding",
			zap.String("seedUid", seed.UID), zap.Error(herr))
	} else if hot {
		return "", nil
	}

	neighbors, err := r.nav.Neighbors(ctx, seed.UID, 2, navigate.RelationFilter{Limit: reflectNeighborLimit})
	if err != nil {
		return "", fmt.Errorf("neighbors: %w", err)
	}
	if len(neighbors) == 0 {
		return "", nil
	}

	candidates, prompt := r.buildClusterPrompt(seed, neighbors)
	return r.callAndAppendInsight(ctx, seed, candidates, prompt)
}

// buildClusterPrompt assembles the candidate-UID set and the user-turn prompt
// text from a seed entity and its expanded neighborhood.
func (r *Reflector) buildClusterPrompt(seed navigate.Entity, neighbors []navigate.Neighbor) (map[string]struct{}, string) {
	candidates := map[string]struct{}{seed.UID: {}}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Seed entity: %s (%s) [uid=%s]\n\nEdges:\n", seed.DisplayName, seed.Kind, seed.UID)
	for i := range neighbors {
		n := neighbors[i]
		candidates[n.Entity.UID] = struct{}{}
		if n.Via.SourceEventUID != "" {
			candidates[n.Via.SourceEventUID] = struct{}{}
		}
		fmt.Fprintf(&sb, "- %s --[%s]--> %s [uid=%s]\n",
			firstNonEmpty(n.Via.SubjectName, n.Via.SubjectUID), n.Via.Predicate,
			firstNonEmpty(n.Via.ObjectName, n.Via.ObjectUID), n.Entity.UID)
	}
	return candidates, sb.String()
}

// callAndAppendInsight calls the LLM with the assembled cluster prompt, validates
// the structured response, checks for semantic duplicates, and appends the event.
// It returns the appended event UID, or "" when the model declines, returns a
// malformed or hallucinated response, or the insight is a semantic duplicate.
func (r *Reflector) callAndAppendInsight(ctx context.Context, seed navigate.Entity, candidates map[string]struct{}, prompt string) (string, error) {
	resp, err := r.chat.ToolUse(ctx, kernel.ToolUseRequest{
		Model:     r.model,
		System:    reflectSystemPrompt,
		Messages:  []kernel.Message{{Role: "user", Content: prompt}},
		Tool:      kernel.ToolSchema{Name: "synthesize_insight", Description: "Record one synthesized insight about the entity cluster.", InputSchema: insightInputSchema},
		MaxTokens: 1024,
	})
	if err != nil {
		return "", fmt.Errorf("tool use: %w", err)
	}
	if resp.ToolUse == nil {
		return "", nil
	}
	var in reflectInput
	if uerr := json.Unmarshal(resp.ToolUse.Input, &in); uerr != nil {
		r.logger.Warn("cognitive: reflect: malformed tool output; skipping", zap.Error(uerr))
		return "", nil
	}
	sources := filterToCandidates(in.SourceUids, candidates)
	if strings.TrimSpace(in.Statement) == "" || len(sources) == 0 {
		return "", nil
	}
	if r.isInsightDuplicate(ctx, in.Statement) {
		return "", nil
	}
	return r.appendInsight(ctx, seed, in, sources)
}

// appendInsight marshals and appends the validated insightSynthesized event.
// It returns the new event UID on success.
func (r *Reflector) appendInsight(ctx context.Context, seed navigate.Entity, in reflectInput, sources []string) (string, error) {
	sourceUids := make([]ontology.ULID, len(sources))
	for i, u := range sources {
		sourceUids[i] = ontology.ULID(u)
	}
	conf := clampConfidence(in.Confidence)
	payload, err := json.Marshal(ontology.InsightSynthesizedEvent{
		InsightUID:            ontology.ULID(newULID()),
		Statement:             in.Statement,
		InsightType:           firstNonEmpty(in.InsightType, "pattern"),
		SourceUids:            sourceUids,
		SeedEntityUID:         ontology.ULID(seed.UID),
		SynthesizedConfidence: conf,
		SynthesizedAt:         time.Now().UTC(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal insight: %w", err)
	}
	receipts, err := r.appender.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindInsightSynthesized,
		Source:     reflectionSource,
		Confidence: conf,
		Payload:    payload,
	}})
	if err != nil {
		return "", fmt.Errorf("append insight: %w", err)
	}
	if len(receipts) > 0 {
		return receipts[0].UID, nil
	}
	return "", nil
}

// isInsightDuplicate reports whether statement deduplicates against an existing
// insight in the "insights" vector domain. A dedup-check error is logged and
// treated as not a duplicate (proceed) — dedup is an optimization, not a gate.
// When the deduper is nil (embedder unavailable) the method returns false so the
// breaker-open graceful path is preserved (#236).
func (r *Reflector) isInsightDuplicate(ctx context.Context, statement string) bool {
	if r.deduper == nil {
		return false
	}
	dup, _, derr := r.deduper.CheckDuplicate(ctx, insightsDomain, "", statement, reflectionDedupThreshold)
	if derr != nil {
		r.logger.Warn("cognitive: reflect: dedup check failed; proceeding", zap.Error(derr))
		return false
	}
	if dup != "" {
		r.logger.Debug("cognitive: reflect: duplicate insight skipped", zap.String("existingUid", dup))
		return true
	}
	return false
}
