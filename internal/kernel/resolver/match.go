package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/entitymatch"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// MatchResult is the output of matchMention for one Mention.
type MatchResult struct {
	// EntityUID is the ULID of the matched or newly created Entity.
	EntityUID string

	// Status is the resolver's confidence verdict for this match.
	Status ontology.ResolutionStatus

	// IsNew is true when the resolver created a new Entity rather than merging
	// with an existing one.
	IsNew bool

	// Reason describes which matching step produced this result.
	// One of: "exact_name", "name_emb", "identifier", "summary_emb",
	// "llm_disambig", "new".
	Reason string
}

// matchMention runs the three-step matching pipeline for one Mention and
// returns the resolved MatchResult. The steps are:
//
//  1. Exact name or name-embedding >= cfg.NameEmbThreshold.
//  2. Identifier match against entity_identifiers (aliases extracted from
//     prior mentions).
//  3. Summary-embedding with cosine band thresholds.
//
// If all three steps find no match, a new Entity is signalled via IsNew=true.
func (r *Resolver) matchMention(ctx context.Context, m ontology.Mention, pre *preEmbeds) (MatchResult, error) {
	// Step 1: exact name match.
	if uid, err := r.exactNameMatch(ctx, m); err != nil {
		return MatchResult{}, fmt.Errorf("match: exact name: %w", err)
	} else if uid != "" {
		return MatchResult{
			EntityUID: uid,
			Status:    ontology.ResolutionConfirmed,
			IsNew:     false,
			Reason:    "exact_name",
		}, nil
	}

	// Step 1 (continued): name-embedding nearest-neighbour.
	if uid, err := r.nameEmbMatch(ctx, m, pre); err != nil {
		return MatchResult{}, fmt.Errorf("match: name embedding: %w", err)
	} else if uid != "" {
		return MatchResult{
			EntityUID: uid,
			Status:    ontology.ResolutionConfirmed,
			IsNew:     false,
			Reason:    "name_emb",
		}, nil
	}

	// Step 2: identifier match (aliases, emails, usernames, ...).
	if uid, err := r.identifierMatch(ctx, m); err != nil {
		return MatchResult{}, fmt.Errorf("match: identifier: %w", err)
	} else if uid != "" {
		return MatchResult{
			EntityUID: uid,
			Status:    ontology.ResolutionConfirmed,
			IsNew:     false,
			Reason:    "identifier",
		}, nil
	}

	// Step 3: summary-embedding with cosine bands.
	result, err := r.summaryEmbMatch(ctx, m, pre)
	if err != nil {
		return MatchResult{}, fmt.Errorf("match: summary embedding: %w", err)
	}
	return result, nil
}

// identifierMatch resolves m by querying entity_identifiers via the shared
// entitymatch lookup: an alias lane (kind="alias", gated on the candidate kind
// matching m.Kind) and a strong-identifier lane (typed non-alias kinds, length-
// guarded, not kind-gated). See entitymatch.Lookup.IdentifierMatch.
func (r *Resolver) identifierMatch(ctx context.Context, m ontology.Mention) (string, error) {
	// The resolver pins each mention's kind from extraction, so it discards the
	// matched entity's stored kind; only the direct-mint path (which trusts an
	// identifier across kinds) needs it.
	uid, _, err := r.lookup().IdentifierMatch(ctx, m.DisplayName, string(m.Kind))
	return uid, err
}

// exactNameMatch attempts a case-sensitive then case-insensitive exact lookup
// by (display_name, kind) in the entities table. Returns the entity UID on a
// hit, or empty string on a miss.
func (r *Resolver) exactNameMatch(ctx context.Context, m ontology.Mention) (string, error) {
	return r.lookup().ExactNameMatch(ctx, m.DisplayName, string(m.Kind))
}

// lookup returns the shared entity-dedup primitive. New() always sets r.matcher;
// this helper additionally tolerates Resolver literals assembled in unit tests
// that bypass New(). The Lookup is stateless, so a transient one built from the
// resolver's own collaborators behaves identically.
func (r *Resolver) lookup() *entitymatch.Lookup {
	if r.matcher != nil {
		return r.matcher
	}
	return entitymatch.NewLookup(r.reader, r.vec, r.embedder, r.cfg.NameEmbThreshold, entitymatch.AdvisoryDefault)
}

// nameEmbMatch embeds the mention's display name (reusing the episode's
// pre-embed batch) and delegates the nearest-neighbour search + kind gate to
// the shared entitymatch lookup. Returns the entity UID when the top
// kind-matching hit's cosine similarity >= cfg.NameEmbThreshold, else "".
func (r *Resolver) nameEmbMatch(ctx context.Context, m ontology.Mention, pre *preEmbeds) (string, error) {
	vec, err := r.embedName(ctx, m, pre)
	if err != nil {
		return "", err
	}
	cands, err := r.lookup().NameKNNByVector(ctx, vec, string(m.Kind), 1)
	if err != nil {
		return "", err
	}
	if len(cands) == 0 || cands[0].Similarity < r.cfg.NameEmbThreshold {
		return "", nil
	}
	return cands[0].UID, nil
}

// summaryEmbMatch embeds the mention summary text and runs a nearest-neighbour
// search over entities_vec where vector_type='summary'. Applies the three cosine
// bands defined in cfg:
//
//   - score >= cfg.SummaryAutoMergeThreshold: confirmed merge.
//   - cfg.SummaryGrayLow <= score < cfg.SummaryAutoMergeThreshold: LLM disambiguation.
//   - score < cfg.SummaryGrayLow: new Entity.
func (r *Resolver) summaryEmbMatch(ctx context.Context, m ontology.Mention, pre *preEmbeds) (MatchResult, error) {
	// Empty-summary guard: when the Mention carries neither DisambiguationCtx
	// nor a meaningful ExtractionText, the summary embedding collapses to the
	// noise floor and matches arbitrary entities at threshold values
	// calibrated for real text. Treat as "no embedding to compare" and let
	// the caller create a new entity rather than risk a false merge.
	if !mentionHasMeaningfulSummary(m) {
		return MatchResult{
			Status: ontology.ResolutionConfirmed,
			IsNew:  true,
			Reason: "new",
		}, nil
	}

	vec, err := r.embedSummary(ctx, m, pre)
	if err != nil {
		return MatchResult{}, err
	}

	hits, err := r.vec.Search(ctx, vec, 1, kernel.VectorFilter{
		Kinds:      []kernel.EventKind{kernel.EventKind(ontology.KindEntity)},
		VectorType: "summary",
	})
	if err != nil {
		return MatchResult{}, fmt.Errorf("summary emb search: %w", err)
	}

	if len(hits) == 0 {
		return MatchResult{
			Status: ontology.ResolutionConfirmed,
			IsNew:  true,
			Reason: "new",
		}, nil
	}
	hit := hits[0]
	score := entitymatch.L2DistanceToCosine(float64(hit.Score))
	if score < r.cfg.SummaryGrayLow {
		return MatchResult{
			Status: ontology.ResolutionConfirmed,
			IsNew:  true,
			Reason: "new",
		}, nil
	}

	entityUID, ok := hit.Metadata["entity_uid"].(string)
	if !ok || entityUID == "" {
		entityUID = hit.ID
	}

	// Kind gate: the summary vector lane has no metadata filter on
	// Mention.Kind, so a person Mention can land near an organisation entity
	// whose summary mentions that person. Load the candidate, check the kind
	// before committing to either branch (auto-merge or gray-zone LLM).
	candidate, err := r.loadEntity(ctx, entityUID)
	if err != nil {
		r.logger.Warn("Resolver: summary-emb candidate load failed, treating as new",
			zap.String("entityUID", entityUID),
			zap.Error(err),
		)
		return MatchResult{
			Status: ontology.ResolutionConfirmed,
			IsNew:  true,
			Reason: "new",
		}, nil
	}
	if candidate.Kind != m.Kind {
		return MatchResult{
			Status: ontology.ResolutionConfirmed,
			IsNew:  true,
			Reason: "new",
		}, nil
	}

	if score >= r.cfg.SummaryAutoMergeThreshold {
		return MatchResult{
			EntityUID: entityUID,
			Status:    ontology.ResolutionConfirmed,
			IsNew:     false,
			Reason:    "summary_emb",
		}, nil
	}

	verdict, err := r.llmDisambiguate(ctx, m, candidate)
	if err != nil {
		r.logger.Warn("Resolver: LLM disambiguation failed, defaulting to unsure",
			zap.String("entityUID", entityUID),
			zap.Error(err),
		)
		verdict = VerdictUnsure
	}

	switch verdict {
	case VerdictSame:
		return MatchResult{
			EntityUID: entityUID,
			Status:    ontology.ResolutionConfirmed,
			IsNew:     false,
			Reason:    "llm_disambig",
		}, nil
	case VerdictDifferent:
		return MatchResult{
			Status: ontology.ResolutionConfirmed,
			IsNew:  true,
			Reason: "new",
		}, nil
	default: // VerdictUnsure
		return MatchResult{
			EntityUID: entityUID,
			Status:    ontology.ResolutionTentative,
			IsNew:     false,
			Reason:    "llm_disambig",
		}, nil
	}
}

// renderDisambiguatePrompt renders the disambiguate-v1 system prompt with the
// mention and candidate entity serialised as compact JSON strings.
func (r *Resolver) renderDisambiguatePrompt(ctx context.Context, mention ontology.Mention, candidate ontology.Entity) (string, error) {
	mentionJSON, err := json.Marshal(mention)
	if err != nil {
		return "", fmt.Errorf("marshal mention: %w", err)
	}
	entityJSON, err := json.Marshal(candidate)
	if err != nil {
		return "", fmt.Errorf("marshal entity: %w", err)
	}
	system, err := r.promptReg.Render(ctx, disambiguateTemplateName, disambiguateTemplateVersion, prompts.RenderVars{
		"mention": strings.TrimSpace(string(mentionJSON)),
		"entity":  strings.TrimSpace(string(entityJSON)),
	})
	if err != nil {
		return "", fmt.Errorf("render disambiguate prompt: %w", err)
	}
	return system, nil
}
