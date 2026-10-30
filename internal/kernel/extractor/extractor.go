package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// episodeKind is the event kind for Episode events. Duplicated here (rather
// than importing ontology.KindEpisode) to avoid an import-cycle risk; the
// value must stay in sync with ontology.KindEpisode = "episode".
const episodeKind = kernel.EventKind("episode")

// Config holds all tunable parameters for an Extractor. Zero values are safe:
// NewExtractor applies defaults for any zero-valued field.
type Config struct {
	// Model is the provider model name, e.g. "llama3.1:8b" or
	// "claude-opus-4-5". Required. Passed verbatim to ChatProvider.ToolUse.
	Model string

	// GroundingThreshold is the minimum GroundingScore a Mention must carry to
	// survive the filter pass. Mentions below this threshold are dropped.
	// Default 0.6. Range [0,1].
	GroundingThreshold float64

	// ChunkThreshold is the byte length above which an episode is split into
	// overlapping chunks before extraction. Default 6000.
	ChunkThreshold int

	// ChunkOverlap is the overlap in bytes between consecutive chunks.
	// Default 200.
	ChunkOverlap int

	// MaxTokens caps the model response per ToolUse call. Default 2048.
	MaxTokens int

	// RecentEpisodes optionally returns up to k previous Episode texts so the
	// extractor can disambiguate references that span sessions (e.g. "she"
	// referring back to a person introduced an hour ago) without confabulating.
	// The result is rendered into the prompt's PRIOR CONTEXT section. Nil is
	// safe: the section is omitted and the prompt behaves as in v2. Implementations
	// must respect ctx cancellation and return quickly: the extractor is on the
	// hot path of every Episode write.
	RecentEpisodes RecentEpisodesProvider

	// RecentEpisodeWindow is the upper bound on episodes fetched via
	// RecentEpisodes. Default 5. Larger windows raise token cost without
	// measurable disambiguation gain past the same-session boundary.
	RecentEpisodeWindow int

	// MinEpisodeChars short-circuits ExtractFrom when an Episode's text
	// length is below this threshold. The LLM round-trip on filler turns
	// like "ok thanks" or "got it" never produces useful Mentions, but a
	// LongMemEval-style haystack contains many such turns. Set to 0 to
	// preserve the prior behaviour (always invoke the model). Default
	// applied via defaultMinEpisodeChars when zero.
	MinEpisodeChars int

	// Vocab provides registered entity-kind and predicate vocabulary from the
	// ontology registry. When non-nil, extension kinds and predicates are
	// injected into the extraction prompt so the LLM prefers registered
	// pack vocabulary over generic core kinds. A nil value or query failure
	// degrades silently to the core-kinds-only baseline.
	Vocab VocabReader
}

// RecentEpisodesProvider is the contract the extractor uses to pull prior
// episode text into the prompt for disambiguation. Implementations typically
// wrap an SQLite reader and run `SELECT text FROM episodes WHERE create_time
// < ? ORDER BY create_time DESC LIMIT ?`. The provider is consulted on every
// extract call; callers that want caching should layer it inside the provider.
type RecentEpisodesProvider interface {
	RecentEpisodes(ctx context.Context, beforeEpisodeUID string, k int) ([]string, error)
}

// applyDefaults fills zero fields with documented defaults.
func (c *Config) applyDefaults() {
	if c.GroundingThreshold <= 0 {
		c.GroundingThreshold = defaultGroundingThreshold
	}
	if c.ChunkThreshold <= 0 {
		c.ChunkThreshold = defaultChunkThreshold
	}
	if c.ChunkOverlap <= 0 {
		c.ChunkOverlap = defaultChunkOverlap
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 2048
	}
	if c.RecentEpisodeWindow <= 0 {
		c.RecentEpisodeWindow = defaultRecentEpisodeWindow
	}
	if c.MinEpisodeChars < 0 {
		c.MinEpisodeChars = 0
	} else if c.MinEpisodeChars == 0 {
		c.MinEpisodeChars = defaultMinEpisodeChars
	}
}

const defaultRecentEpisodeWindow = 5

// defaultMinEpisodeChars is the default short-text guard. 20 chars is roughly
// 3-4 words: enough to require a short utterance ("yes please thanks") to
// pass, but small enough that any substantive ingestible Episode clears it.
// Tuned against LongMemEvalₛ filler turns; raise via Config.MinEpisodeChars
// when ingesting only long-form sources (e.g. Slack threads where the bar is
// already higher).
const defaultMinEpisodeChars = 20

// Extractor is the LangExtract-mirror extraction pipeline. It wraps a
// ChatProvider, a prompts.Registry (for the extract-v1 template), and a set
// of tunable parameters. Safe for concurrent use.
type Extractor struct {
	provider kernel.ChatProvider
	prompts  prompts.Registry
	cfg      Config
}

// New returns a ready-to-use Extractor. Provider and reg must be non-nil.
// RegisterExtractPrompt must have been called on reg before the first Extract
// call (or the Render step will return prompts.ErrNotFound).
func New(provider kernel.ChatProvider, reg prompts.Registry, cfg Config) *Extractor {
	cfg.applyDefaults()
	return &Extractor{
		provider: provider,
		prompts:  reg,
		cfg:      cfg,
	}
}

// Extract runs the full LangExtract-mirror pipeline for one episode and
// returns grounded Mentions and relation candidates. It does NOT append events;
// ExtractFrom (the kernel.ExtractorIface adapter) handles event emission.
//
// Zero referenceTime is permitted; the prompt's REFERENCE DATE block is
// omitted and relative-date temporal hints degrade silently to "year unknown"
// (the model drops the offending validFrom / validUntil rather than guessing).
//
// Pipeline:
//  1. Chunk the text if it exceeds cfg.ChunkThreshold.
//  2. For each chunk: run up to maxPasses extraction passes.
//  3. Ground all mentions, filter below threshold, adjust to global offsets.
//  4. Deduplicate across chunks.
func (e *Extractor) Extract(ctx context.Context, episodeUID, episodeText string) ([]ontology.Mention, []ontology.RelationCandidate, error) {
	return e.ExtractWithRefTime(ctx, episodeUID, episodeText, time.Time{})
}

// ExtractWithRefTime is Extract with an explicit reference time used to
// anchor relative-date resolution in temporal hints. Pass the Episode's
// create_time so the LLM can convert "last week" / "since 2024" into
// concrete validFrom / validUntil bounds.
func (e *Extractor) ExtractWithRefTime(ctx context.Context, episodeUID, episodeText string, referenceTime time.Time) ([]ontology.Mention, []ontology.RelationCandidate, error) {
	return e.ExtractWithActor(ctx, episodeUID, episodeText, referenceTime, "")
}

// ExtractWithActor is the v6 entrypoint: identical to ExtractWithRefTime
// but propagates an actor channel ("user" | "assistant" | "system" | "")
// into prompt selection. Empty / "user" preserves v5 behaviour bit-for-bit;
// "assistant" activates the relaxed-first-person prompt branch (mem0's
// AGENT_MEMORY_EXTRACTION_PROMPT equivalent, +53.6pp on the
// single-session-assistant LongMemEval category).
func (e *Extractor) ExtractWithActor(ctx context.Context, episodeUID, episodeText string, referenceTime time.Time, actor string) ([]ontology.Mention, []ontology.RelationCandidate, error) {
	chunks := ChunkText(episodeText, e.cfg.ChunkThreshold, e.cfg.ChunkOverlap)
	priorContext := e.fetchPriorContext(ctx, episodeUID)
	extKinds, extPredicates := e.fetchVocab(ctx)

	var allMentions []ontology.Mention
	var allCandidates []ontology.RelationCandidate

	for _, chunk := range chunks {
		ms, cs, err := e.extractChunk(ctx, episodeUID, chunk, priorContext, referenceTime, actor, extKinds, extPredicates)
		if err != nil {
			return nil, nil, err
		}
		adjusted := AdjustSpans(ms, chunk.ByteOffset)
		allMentions = append(allMentions, adjusted...)
		allCandidates = append(allCandidates, cs...)
	}

	allMentions = DeduplicateMentions(allMentions)
	return allMentions, allCandidates, nil
}

// fetchVocab consults Config.Vocab (if set) and returns the extension kinds and
// predicates to inject into the prompt. Both slices are nil when Vocab is unset
// or the query fails, degrading cleanly to core-kinds-only extraction.
func (e *Extractor) fetchVocab(ctx context.Context) (kinds, predicates []VocabEntry) {
	if e.cfg.Vocab == nil {
		return nil, nil
	}
	ks, err := e.cfg.Vocab.ExtensionKinds(ctx, maxExtensionKinds)
	if err != nil {
		logging.FromContext(ctx).Warn(
			"extractor: vocab kinds fetch failed, continuing without extension kinds",
			zap.Error(err),
		)
		ks = nil
	}
	ps, err := e.cfg.Vocab.ExtensionPredicates(ctx, maxExtensionPredicates)
	if err != nil {
		logging.FromContext(ctx).Warn(
			"extractor: vocab predicates fetch failed, continuing without extension predicates",
			zap.Error(err),
		)
		ps = nil
	}
	return ks, ps
}

// fetchPriorContext consults Config.RecentEpisodes (if set) and returns the
// joined text block to render into the PRIOR CONTEXT section of the prompt.
// Failures degrade gracefully to an empty string with a warning: the
// extractor's hot path must not fail because a side-channel disambiguation
// query timed out.
func (e *Extractor) fetchPriorContext(ctx context.Context, episodeUID string) string {
	if e.cfg.RecentEpisodes == nil {
		return ""
	}
	texts, err := e.cfg.RecentEpisodes.RecentEpisodes(ctx, episodeUID, e.cfg.RecentEpisodeWindow)
	if err != nil {
		logging.FromContext(ctx).Warn(
			"extractor: recent-episodes fetch failed, continuing without prior context",
			zap.Error(err),
		)
		return ""
	}
	if len(texts) == 0 {
		return ""
	}
	return strings.Join(texts, "\n---\n")
}

// extractChunk runs the multi-pass extraction loop for one text chunk.
func (e *Extractor) extractChunk(ctx context.Context, episodeUID string, chunk Chunk, priorContext string, referenceTime time.Time, actor string, extKinds, extPredicates []VocabEntry) ([]ontology.Mention, []ontology.RelationCandidate, error) {
	mentions, candidates, err := e.runPass(ctx, episodeUID, chunk.Text, priorContext, referenceTime, actor, extKinds, extPredicates)
	if err != nil {
		return nil, nil, err
	}
	if allWellGrounded(mentions) {
		return mentions, candidates, nil
	}

	for pass := 2; pass <= maxPasses; pass++ {
		if !needsSecondPass(mentions, candidates) {
			break
		}
		focusKinds := lowConfidenceKinds(mentions)
		ms2, cs2, err2 := e.runFocusedPass(ctx, episodeUID, chunk.Text, focusKinds, priorContext, referenceTime, actor, extKinds, extPredicates)
		if err2 != nil {
			logging.FromContext(ctx).Warn(
				"extractor: pass failed, keeping prior results",
				zap.Int("pass", pass),
				zap.Error(err2),
			)
			break
		}
		mentions, candidates = mergePasses(mentions, candidates, ms2, cs2)
		if allWellGrounded(mentions) {
			break
		}
	}
	return mentions, candidates, nil
}

// runPass issues one full-extraction ToolUse call and returns grounded,
// filtered Mentions and raw RelationCandidates.
func (e *Extractor) runPass(ctx context.Context, episodeUID, text, priorContext string, referenceTime time.Time, actor string, extKinds, extPredicates []VocabEntry) ([]ontology.Mention, []ontology.RelationCandidate, error) {
	system, err := BuildSystemPromptWithVocab(ctx, e.prompts, text, priorContext, referenceTime, actor, extKinds, extPredicates)
	if err != nil {
		return nil, nil, fmt.Errorf("extractor: build prompt: %w", err)
	}
	return e.callToolUse(ctx, episodeUID, text, system)
}

// runFocusedPass builds a prompt that appends a focus hint for the given
// entity kinds and issues a ToolUse call. Used for pass 2+ in the multi-pass
// loop.
func (e *Extractor) runFocusedPass(ctx context.Context, episodeUID, text string, focusKinds []ontology.EntityKind, priorContext string, referenceTime time.Time, actor string, extKinds, extPredicates []VocabEntry) ([]ontology.Mention, []ontology.RelationCandidate, error) {
	hint := buildFocusHint(focusKinds)
	system, err := BuildSystemPromptWithVocab(ctx, e.prompts, text+hint, priorContext, referenceTime, actor, extKinds, extPredicates)
	if err != nil {
		return nil, nil, fmt.Errorf("extractor: build focused prompt: %w", err)
	}
	return e.callToolUse(ctx, episodeUID, text, system)
}

// callToolUse issues the ChatProvider.ToolUse call, parses the response, and
// grounds the resulting mentions.
func (e *Extractor) callToolUse(ctx context.Context, episodeUID, text, system string) ([]ontology.Mention, []ontology.RelationCandidate, error) {
	resp, err := e.provider.ToolUse(ctx, kernel.ToolUseRequest{
		Model:  e.cfg.Model,
		System: system,
		Messages: []kernel.Message{
			{Role: "user", Content: text},
		},
		Tool: kernel.ToolSchema{
			Name:        "extract_entities_and_relations",
			Description: "Extract all named entities and their relations from the provided text.",
			InputSchema: ExtractionToolSchema(),
		},
		MaxTokens: e.cfg.MaxTokens,
		// Extraction uses structured-output decoding: the schema drives the
		// response grammar and no tools array is sent. The native
		// function-calling path returns an EMPTY tool_call on some local models
		// (qwen2.5:7b emits a call with no arguments → zero mentions on
		// otherwise-correct input); the same model fills message.content
		// correctly when handed the identical schema as the response format. The
		// grammar still guarantees conforming JSON the parser can rely on. (An
		// earlier throughput worry that motivated unconstrained free-form
		// decoding was mostly an Ollama OLLAMA_NUM_PARALLEL>1 artifact, not the
		// grammar; free-form produced unusable output against this complex schema
		// and was dropped.)
		PreferStructuredOutput: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("extractor: tool-use call: %w", err)
	}
	if resp.ToolUse == nil {
		return nil, nil, nil
	}
	return e.parseAndGround(resp.ToolUse.Input, episodeUID, text)
}

// parseAndGround parses the raw tool-use JSON and grounds every mention.
func (e *Extractor) parseAndGround(raw json.RawMessage, episodeUID, sourceText string) ([]ontology.Mention, []ontology.RelationCandidate, error) {
	mentions, candidates, err := ParseToolUseResponse(raw, episodeUID)
	if err != nil {
		return nil, nil, fmt.Errorf("extractor: parse response: %w", err)
	}
	grounded := make([]ontology.Mention, 0, len(mentions))
	for _, m := range mentions {
		g := Ground(m, sourceText)
		grounded = append(grounded, g)
	}
	filtered := FilterByThreshold(grounded, e.cfg.GroundingThreshold)
	return filtered, candidates, nil
}

// buildFocusHint appends a short human-readable focus directive to the text
// passed to the second-pass prompt so the model re-examines the specific
// entity kinds that scored poorly in the first pass.
func buildFocusHint(kinds []ontology.EntityKind) string {
	if len(kinds) == 0 {
		return ""
	}
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	return "\n\n[Focus: re-examine the following entity kinds which may have been missed: " +
		strings.Join(names, ", ") + "]"
}

// ExtractFrom implements kernel.ExtractorIface. It decodes the event payload,
// calls Extract, and marshals the results into ready-to-append kernel.Event
// slices. Non-episode events return nil, nil immediately.
func (e *Extractor) ExtractFrom(ctx context.Context, ev kernel.Event) ([]kernel.Event, error) {
	if ev.Kind != episodeKind {
		return nil, nil
	}
	var ep episodePayload
	if err := json.Unmarshal(ev.Payload, &ep); err != nil {
		return nil, fmt.Errorf("extractor: decode episode payload: %w", err)
	}
	if ep.Text == "" {
		return nil, nil
	}
	if e.cfg.MinEpisodeChars > 0 && len(ep.Text) < e.cfg.MinEpisodeChars {
		return nil, nil
	}

	mentions, candidates, err := e.ExtractWithActor(ctx, ev.UID, ep.Text, ev.CreateTime, ep.Actor)
	if err != nil {
		return nil, err
	}

	// Augment with first-person self-statements only when the episode is
	// user-authored. For assistant-actor episodes, "I" refers to the
	// assistant (not the user) — running detectSelfStatements would
	// produce wrong-subject bindings (e.g. has_name="Claude") and
	// poison the user's self-fact graph. The v6 ACTOR CONTEXT prompt
	// branch handles assistant first-person extraction natively via
	// the LLM. The LLM extractor's GROUNDING CONTRACT excludes
	// first-person references for user-authored episodes, so a sentence
	// like "I changed my name to Lucas" yields a Lucas mention but no
	// has_name relation. The lexical detector closes that gap by
	// emitting synthetic candidates with the SelfSubjectSentinel
	// subject — the resolver binds that to the canonical seeded 'self'
	// entity and the supersede pipeline auto-closes the prior open row.
	if ep.Actor == "" || ep.Actor == "user" {
		for _, sc := range detectSelfStatements(ep.Text, mentions) {
			candidates = append(candidates, ontology.RelationCandidate{
				SubjectMentionIdx: ontology.SelfSubjectSentinel,
				Predicate:         sc.Predicate,
				ObjectMentionIdx:  sc.ObjectMentionIdx,
			})
		}
	}

	return buildExtractionEvents(ev.UID, ev.Scope, mentions, candidates)
}

// episodePayload is a minimal decode target for an Episode event; only
// Text and Actor are needed for extraction. A full ontology.Episode
// decode is avoided here to keep the extractor package free of the
// ontology import cycle risk.
type episodePayload struct {
	Text  string `json:"text"`
	Actor string `json:"actor,omitempty"`
}

// buildExtractionEvents marshals grounded Mentions and RelationCandidates into
// kernel.Event slices ready for l.Append. Scope is copied from the originating
// episode event so derived mention/candidate events carry the same scope
// boundary — draft-episode extractions must not silently land in personal scope.
func buildExtractionEvents(episodeUID string, scope kernel.Scope, mentions []ontology.Mention, candidates []ontology.RelationCandidate) ([]kernel.Event, error) {
	if len(mentions) == 0 && len(candidates) == 0 {
		return nil, nil
	}

	evts := make([]kernel.Event, 0, len(mentions)+1)
	for _, m := range mentions {
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("extractor: marshal mention: %w", err)
		}
		evts = append(evts, kernel.Event{
			Kind:       kernel.EventKind(ontology.KindMention),
			Source:     "extractor/v1",
			Scope:      scope,
			Confidence: m.GroundingScore,
			Payload:    raw,
		})
	}

	if len(candidates) > 0 {
		proposed := ontology.RelationCandidatesProposedEvent{
			EpisodeUID: episodeUID,
			Candidates: candidates,
		}
		raw, err := json.Marshal(proposed)
		if err != nil {
			return nil, fmt.Errorf("extractor: marshal candidates: %w", err)
		}
		evts = append(evts, kernel.Event{
			Kind:       kernel.EventKind(ontology.KindRelationCandidatesProposed),
			Source:     "extractor/v1",
			Scope:      scope,
			Confidence: 1.0,
			Payload:    raw,
		})
	}
	return evts, nil
}

// Ensure Extractor satisfies kernel.ExtractorIface at compile time.
var _ kernel.ExtractorIface = (*Extractor)(nil)
