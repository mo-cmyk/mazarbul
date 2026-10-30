package composer

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

// Config holds tunable parameters for a Composer. Zero values are safe: New
// applies defaults for any zero-valued field.
type Config struct {
	// Model is the ChatProvider model name, e.g. "llama3.1:8b" or
	// "claude-sonnet-4-6". Required. Passed verbatim to ChatProvider.Complete.
	Model string

	// MaxAnswerTokens caps the model response length. Default 1024.
	MaxAnswerTokens int

	// MaxContextHits is the maximum number of hits included in the context
	// block. Default 10.
	MaxContextHits int

	// MaxSnippetRunes is the maximum rune length of each snippet in the
	// context block. Default 400.
	MaxSnippetRunes int

	// AbstainThreshold is the hard-floor abstention gate: when the best
	// recall hit's FusedScore falls below this value, Compose returns
	// the AbstainSentinel verbatim without an LLM call. Phase 2 of the
	// mem0-parity plan: LongMemEval's `abstention` ability scores
	// refusal-when-unsupported, and small composers (qwen2.5:7b) are
	// weak self-policers — a hard floor in code is more reliable than
	// trusting the LLM to refuse. Zero (the default) disables the
	// floor; the v12 ABSTENTION RULE prompt instruction still asks the
	// model to refuse when context is thin. Suggested production
	// starting value: 0.18 (tune from the recall-score histogram on a
	// 500q LongMemEval run).
	AbstainThreshold float32
}

// AbstainSentinel is the canonical refusal answer returned when the
// hard-floor abstention gate fires. Kept as a package-level constant so
// the eval harness can recognise it verbatim without coupling to the
// composer's internal wording. Mirrors mem0's "I don't know" style but
// is phrased to read naturally in chat: short, declarative, no
// confabulation hooks.
const AbstainSentinel = "I don't have enough information in your memory to answer that."

// applyDefaults fills zero fields with documented defaults.
func (c *Config) applyDefaults() {
	if c.MaxAnswerTokens <= 0 {
		c.MaxAnswerTokens = 1024
	}
	if c.MaxContextHits <= 0 {
		c.MaxContextHits = 10
	}
	if c.MaxSnippetRunes <= 0 {
		c.MaxSnippetRunes = 400
	}
}

// Answer is the structured output of Composer.Compose.
type Answer struct {
	// Answer is the prose response with inline [N]-style citations.
	Answer string `json:"answer"`

	// Entities lists entity references observed in the answer prose. Populated
	// by a naive scan; may be empty when no entity metadata is available.
	Entities []EntityReference `json:"entities,omitempty"`

	// Evidence lists subject-predicate-object triples inferred from the context
	// block. Reserved for future structured extraction; currently always empty.
	Evidence []EvidenceTriple `json:"evidence,omitempty"`

	// Citations is the full list of numbered citations. Index corresponds to
	// the [N] markers in Answer. Always populated when context hits are present.
	Citations []Citation `json:"citations"`
}

// EntityReference is a lightweight entity pointer embedded in an Answer.
type EntityReference struct {
	// UID is the entity's ULID identifier.
	UID string `json:"uid"`

	// DisplayName is the human-readable name for the entity.
	DisplayName string `json:"displayName"`

	// Kind is the entity kind, e.g. "person" or "organization".
	Kind string `json:"kind"`
}

// EvidenceTriple is a subject-predicate-object fact extracted from the answer
// context. Reserved for future structured output; not yet populated.
type EvidenceTriple struct {
	// Subject is the display name of the subject entity.
	Subject string `json:"subject"`

	// Predicate is the relation type, e.g. "works_at".
	Predicate string `json:"predicate"`

	// Object is the display name of the object entity.
	Object string `json:"object"`

	// ValidFrom is the optional inclusive lower bound of the relationship's
	// validity window.
	ValidFrom *time.Time `json:"validFrom,omitempty"`

	// ValidUntil is the optional exclusive upper bound of the relationship's
	// validity window.
	ValidUntil *time.Time `json:"validUntil,omitempty"`
}

// Citation links a [N] marker in the Answer prose to its source record — an
// Episode, or a vault document chunk when Path is set.
type Citation struct {
	// Index is the 1-based citation number, matching [N] in the prose.
	Index int `json:"index"`

	// EpisodeUID is the ULID of the source record: the Episode event for
	// episode hits, the document UID (= its entity UID) for document hits.
	EpisodeUID string `json:"episodeUid"`

	// Snippet is the rune-capped excerpt from the episode text.
	Snippet string `json:"snippet"`

	// CreateTime is the time the episode was first observed, in UTC.
	CreateTime time.Time `json:"createTime"`

	// Path is the vault-relative document path when the citation points
	// into a document ("notes/foo.md"); empty for episode citations.
	Path string `json:"path,omitempty"`

	// Heading is the markdown heading nearest above the cited chunk, so a
	// caller can render "notes/foo.md › ## Goals". Empty when the chunk
	// precedes the first heading or the citation is an episode.
	Heading string `json:"heading,omitempty"`
}

// Composer assembles grounded answers from recall hits via ChatProvider.
// Safe for concurrent use.
type Composer struct {
	chat      kernel.ChatProvider
	promptReg prompts.Registry
	cfg       Config
	logger    *zap.Logger
}

// citationIndexRe matches bare [N] markers in prose where N is one or more
// digits. Used by parseCitedIndices to identify which citations the model
// referenced.
var citationIndexRe = regexp.MustCompile(`\[(\d+)\]`)

// New constructs a Composer. Chat and reg must be non-nil.
// RegisterComposePrompt must have been called on reg before the first Compose
// call, or the Render step will return prompts.ErrNotFound.
func New(cfg Config, chat kernel.ChatProvider, reg prompts.Registry, logger *zap.Logger) (*Composer, error) {
	cfg.applyDefaults()
	switch {
	case chat == nil:
		return nil, errors.New("composer: chat provider is required")
	case reg == nil:
		return nil, errors.New("composer: prompt registry is required")
	case logger == nil:
		return nil, errors.New("composer: logger is required")
	case cfg.Model == "":
		return nil, errors.New("composer: Config.Model is required")
	}
	return &Composer{
		chat:      chat,
		promptReg: reg,
		cfg:       cfg,
		logger:    logger,
	}, nil
}

// Compose builds a grounded Answer from the provided recall hits. The
// question is used as the user message; hits are rendered into a numbered
// context block and injected into the compose-v1 system prompt.
//
// When hits is empty the compose-v1 abstain instruction produces an "I don't
// have enough information" response, which is surfaced verbatim in Answer.
//
// Errors from the ChatProvider are returned unwrapped so callers can apply
// errors.Is checks against provider-specific sentinel values.
func (c *Composer) Compose(ctx context.Context, question string, hits []kernel.Hit) (Answer, error) {
	return c.ComposeWithCurrentDate(ctx, question, hits, time.Time{})
}

// ComposeWithCurrentDate is Compose with an explicit anchor for relative-date
// reasoning ("how many weeks ago", "how many days passed"). Zero currentDate
// is permitted; the prompt's TODAY'S DATE block is omitted and date arithmetic
// degrades to whatever the model can infer from the dated context entries.
// LongMemEval's temporal-reasoning ability scored 0/5 on the 20q probe without
// this anchor because qwen2.5:7b had no reference point for "now" and either
// guessed or abstained.
func (c *Composer) ComposeWithCurrentDate(ctx context.Context, question string, hits []kernel.Hit, currentDate time.Time) (Answer, error) {
	// Hard-floor abstention gate. When the best hit's FusedScore is
	// below threshold (or there are no hits at all), refuse without
	// calling the LLM. This is the Phase 2 abstention surface: small
	// models confabulate from weak context, so the cheap, reliable fix
	// is to skip the LLM entirely when recall is too thin to ground a
	// citation. The v12 ABSTENTION RULE prompt instruction backstops
	// the case where recall is just above the floor but still
	// unsupported — but the floor below is what stops the
	// most-common failure mode (zero usable evidence -> hallucinated
	// answer).
	if c.cfg.AbstainThreshold > 0 && (len(hits) == 0 || hits[0].FusedScore < c.cfg.AbstainThreshold) {
		c.logger.Debug("composer: abstain gate fired",
			zap.Int("hits", len(hits)),
			zap.Float32("threshold", c.cfg.AbstainThreshold),
			zap.Float32("topScore", topScore(hits)),
		)
		// Citations is a non-nil empty slice so the JSON round-trip
		// emits `"citations": []` (not `null`). The MCP ask tool's
		// OutputSchema declares citations as required type=array;
		// null would fail jsonschema validation client-side and crash
		// every caller that runs the abstain path.
		return Answer{Answer: AbstainSentinel, Citations: []Citation{}}, nil
	}

	// Cap hits to MaxContextHits.
	capped := hits
	if len(capped) > c.cfg.MaxContextHits {
		capped = capped[:c.cfg.MaxContextHits]
	}

	intent := ClassifyIntent(question)
	contextBlock, citations := FormatContextBlockWithIntent(capped, c.cfg.MaxSnippetRunes, intent)

	currentDateStr := ""
	if !currentDate.IsZero() {
		currentDateStr = currentDate.UTC().Format("2006-01-02")
	}
	// DURATION HINT is only emitted for IntentDuration; other intents
	// would dilute the prompt with irrelevant deltas.
	durationHint := ""
	if intent == IntentDuration && !currentDate.IsZero() {
		durationHint = renderDurationHint(citations, currentDate)
	}
	system, err := c.promptReg.Render(ctx, composeTemplateName, composeTemplateVersion, prompts.RenderVars{
		"question":     question,
		"context":      contextBlock,
		"currentDate":  currentDateStr,
		"durationHint": durationHint,
	})
	if err != nil {
		return Answer{}, fmt.Errorf("composer: render prompt: %w", err)
	}

	resp, err := c.chat.Complete(ctx, kernel.CompleteRequest{
		Model:  c.cfg.Model,
		System: system,
		Messages: []kernel.Message{
			{Role: "user", Content: question},
		},
		MaxTokens: c.cfg.MaxAnswerTokens,
	})
	if err != nil {
		return Answer{}, fmt.Errorf("composer: chat complete: %w", err)
	}

	prose := resp.Content
	c.logger.Debug("Composer: answer generated",
		zap.Int("inputTokens", resp.Usage.InputTokens),
		zap.Int("outputTokens", resp.Usage.OutputTokens),
		zap.String("stopReason", resp.StopReason),
	)

	usedIndices := parseCitedIndices(prose)
	_ = usedIndices // all citations are returned regardless; field reserved for future filtering

	return Answer{
		Answer:    prose,
		Citations: citations,
	}, nil
}

// topScore returns the FusedScore of the first hit or 0 when hits is
// empty. Used only for debug logging on the abstain path; the gate
// itself reads hits[0] directly with an explicit empty-check.
func topScore(hits []kernel.Hit) float32 {
	if len(hits) == 0 {
		return 0
	}
	return hits[0].FusedScore
}

// parseCitedIndices scans prose for [N] markers and returns the set of
// 1-based citation indices the model referenced. The set is used to annotate
// which citations were actually cited (future use); all citations are returned
// in Answer.Citations regardless.
func parseCitedIndices(prose string) map[int]struct{} {
	matches := citationIndexRe.FindAllStringSubmatch(prose, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make(map[int]struct{}, len(matches))
	for _, m := range matches {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			out[n] = struct{}{}
		}
	}
	return out
}
