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

// consolidationSource is the Event.Source stamped on consolidated memories.
const consolidationSource = "consolidation/v1"

// consolidatedDomain is the vector domain consolidated memories dedup against.
// MUST match sqlite.DomainConsolidated / kernel's domainConsolidated.
const consolidatedDomain = "consolidated"

// consolidateMaxEntitiesDefault caps how many cited-entity groups one
// consolidation pass compresses, bounding the LLM calls per tick.
const consolidateMaxEntitiesDefault = 5

// consolidateEpisodeLimit caps the episodes pulled per cited entity, and the
// minimum group size worth compressing.
const (
	consolidateEpisodeLimit = 20
	consolidateMinGroupSize = 2
)

// consolidateInputSchema constrains the structured output of the consolidation
// tool.
//
//nolint:gochecknoglobals // immutable JSON Schema literal, package-level by design
var consolidateInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "The compressed memory, a few sentences."},
    "memoryType": {"type": "string", "description": "Open taxonomy: semantic | procedural | summary."},
    "sourceUids": {"type": "array", "items": {"type": "string"}, "description": "Episode UIDs compressed into the summary. Cite only uids shown to you."},
    "confidence": {"type": "number", "description": "Confidence in the consolidation, 0..1."}
  },
  "required": ["summary", "memoryType", "sourceUids", "confidence"]
}`)

const consolidateSystemPrompt = "You are a memory-consolidation module. Given a group of recent " +
	"episodes that all mention one entity, compress them into ONE concise consolidated memory that " +
	"preserves the durable facts and drops the chatter. Cite the episode uids you compressed in " +
	"sourceUids — cite ONLY uids shown to you. If the episodes hold nothing worth keeping, return an " +
	"empty summary."

// consolidateInput is the model's structured consolidation output.
type consolidateInput struct {
	Summary    string   `json:"summary"`
	MemoryType string   `json:"memoryType"`
	SourceUids []string `json:"sourceUids"`
	Confidence float64  `json:"confidence"`
}

// Consolidator compresses recent episodes grouped by cited entity (the
// consolidation job).
type Consolidator struct {
	chat        kernel.ChatProvider
	nav         *navigate.Navigator
	appender    Appender
	deduper     Deduper
	model       string
	maxEntities int
	logger      *zap.Logger
}

// NewConsolidator builds a consolidation job. Nav should already be
// scope-narrowed by the caller. Deduper may be nil (dedup is then skipped).
func NewConsolidator(chat kernel.ChatProvider, nav *navigate.Navigator, appender Appender, deduper Deduper, model string, logger *zap.Logger) *Consolidator {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Consolidator{
		chat:        chat,
		nav:         nav,
		appender:    appender,
		deduper:     deduper,
		model:       model,
		maxEntities: consolidateMaxEntitiesDefault,
		logger:      logger,
	}
}

// Run executes one consolidation pass: for each recent cited entity, it pulls
// the episodes citing it, asks the LLM to compress the group into a consolidated
// memory citing the source episodes, dedups the candidate against existing
// consolidated memories, and appends a memoryConsolidated event when accepted.
// A tripped breaker stops the pass cleanly (Skipped="breaker-open").
func (c *Consolidator) Run(ctx context.Context) (Result, error) {
	if c.chat == nil || c.nav == nil || c.appender == nil {
		return Result{Skipped: "not-wired"}, nil
	}
	entities, err := c.nav.ListEntities(ctx, navigate.EntityFilter{Limit: c.maxEntities})
	if err != nil {
		return Result{}, fmt.Errorf("cognitive: consolidate: list entities: %w", err)
	}
	if len(entities) == 0 {
		return Result{Skipped: "no-candidates"}, nil
	}

	var appended []string
	for i := range entities {
		uid, cerr := c.consolidateForEntity(ctx, entities[i])
		if cerr != nil {
			if isBreakerOpen(cerr) {
				c.logger.Debug("cognitive: consolidate: breaker open; deferring to next tick")
				return Result{AppendedEventUIDs: appended, Skipped: "breaker-open"}, nil
			}
			c.logger.Warn("cognitive: consolidate: entity failed; skipping",
				zap.String("entityUid", entities[i].UID), zap.Error(cerr))
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

// consolidateForEntity compresses the episodes citing one entity. Returns the
// appended event UID, or "" when the group was too small, the model declined or
// produced malformed/hallucinated output, or the summary deduplicated against an
// existing consolidated memory.
func (c *Consolidator) consolidateForEntity(ctx context.Context, entity navigate.Entity) (string, error) {
	episodes, err := c.nav.EpisodesCitingEntity(ctx, entity.UID, consolidateEpisodeLimit)
	if err != nil {
		return "", fmt.Errorf("episodes citing entity: %w", err)
	}
	if len(episodes) < consolidateMinGroupSize {
		return "", nil
	}

	prompt, candidates := buildEpisodeCluster(entity, episodes)
	in, ok, err := c.compress(ctx, prompt)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	sources := filterToCandidates(in.SourceUids, candidates)
	if strings.TrimSpace(in.Summary) == "" || len(sources) == 0 {
		return "", nil
	}
	if c.isDuplicate(ctx, in.Summary) {
		return "", nil
	}
	return c.appendConsolidated(ctx, entity, in, sources)
}

// buildEpisodeCluster renders the LLM prompt for one entity's episode group and
// returns the set of episode UIDs the model is allowed to cite.
func buildEpisodeCluster(entity navigate.Entity, episodes []navigate.Episode) (string, map[string]struct{}) {
	candidates := make(map[string]struct{}, len(episodes))
	var sb strings.Builder
	fmt.Fprintf(&sb, "Entity: %s (%s) [uid=%s]\n\nEpisodes:\n", entity.DisplayName, entity.Kind, entity.UID)
	for i := range episodes {
		candidates[episodes[i].UID] = struct{}{}
		fmt.Fprintf(&sb, "- [uid=%s] %s\n", episodes[i].UID, episodes[i].Text)
	}
	return sb.String(), candidates
}

// compress runs the consolidation ToolUse and decodes the structured output.
// Ok is false (with a nil error) when the model declined or returned malformed
// output — both clean skips.
func (c *Consolidator) compress(ctx context.Context, prompt string) (consolidateInput, bool, error) {
	resp, err := c.chat.ToolUse(ctx, kernel.ToolUseRequest{
		Model:     c.model,
		System:    consolidateSystemPrompt,
		Messages:  []kernel.Message{{Role: "user", Content: prompt}},
		Tool:      kernel.ToolSchema{Name: "consolidate_memory", Description: "Record one consolidated memory compressing the episode group.", InputSchema: consolidateInputSchema},
		MaxTokens: 1024,
	})
	if err != nil {
		return consolidateInput{}, false, fmt.Errorf("tool use: %w", err)
	}
	if resp.ToolUse == nil {
		return consolidateInput{}, false, nil
	}
	var in consolidateInput
	if uerr := json.Unmarshal(resp.ToolUse.Input, &in); uerr != nil {
		c.logger.Warn("cognitive: consolidate: malformed tool output; skipping", zap.Error(uerr))
		return consolidateInput{}, false, nil
	}
	return in, true, nil
}

// isDuplicate reports whether summary deduplicates against an existing
// consolidated memory. A dedup-check error is logged and treated as not a
// duplicate (proceed) — dedup is an optimization, not a gate.
func (c *Consolidator) isDuplicate(ctx context.Context, summary string) bool {
	if c.deduper == nil {
		return false
	}
	dup, _, derr := c.deduper.CheckDuplicate(ctx, consolidatedDomain, "", summary, consolidationDedupThreshold)
	if derr != nil {
		c.logger.Warn("cognitive: consolidate: dedup check failed; proceeding", zap.Error(derr))
		return false
	}
	if dup != "" {
		c.logger.Debug("cognitive: consolidate: duplicate summary skipped", zap.String("existingUid", dup))
		return true
	}
	return false
}

// appendConsolidated builds and appends the memoryConsolidated event, returning
// its event UID.
func (c *Consolidator) appendConsolidated(ctx context.Context, entity navigate.Entity, in consolidateInput, sources []string) (string, error) {
	sourceUids := make([]ontology.ULID, len(sources))
	for i, u := range sources {
		sourceUids[i] = ontology.ULID(u)
	}
	conf := clampConfidence(in.Confidence)
	payload, err := json.Marshal(ontology.MemoryConsolidatedEvent{
		MemoryUID:      ontology.ULID(newULID()),
		Summary:        in.Summary,
		MemoryType:     firstNonEmpty(in.MemoryType, "semantic"),
		SourceUids:     sourceUids,
		CitedEntityUID: ontology.ULID(entity.UID),
		Confidence:     conf,
		ConsolidatedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal consolidated memory: %w", err)
	}
	receipts, err := c.appender.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindMemoryConsolidated,
		Source:     consolidationSource,
		Confidence: conf,
		Payload:    payload,
	}})
	if err != nil {
		return "", fmt.Errorf("append consolidated memory: %w", err)
	}
	if len(receipts) > 0 {
		return receipts[0].UID, nil
	}
	return "", nil
}
