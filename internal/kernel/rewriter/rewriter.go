// Package rewriter implements kernel.QueryRewriter via the query-rewrite-v2
// prompt registered in internal/prompts. The package is its own seam because
// internal/prompts already imports internal/kernel; if Rewriter lived in
// kernel we would close the cycle.
//
// The rewriter calls ChatProvider.ToolUse with a JSON Schema constraining
// the response shape to {query, at?}. ToolUse routes to Anthropic tool-use,
// Ollama JSON-mode, or a schema-as-prose fallback transparently.
//
// Graceful degradation: on prompt-render, provider, or decode failure
// Rewrite returns a RewriteResult{Query: original, At: nil} so callers can
// treat the (result, error) pair as best-effort and always use the
// returned Query string.
package rewriter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
)

const (
	// TemplateName is the registry name for the query-rewrite prompt.
	// Public so callers can pin specific versions in tests.
	TemplateName = "query-rewrite-v1"

	// TemplateVersion is the prompt-registry version bound at compile time.
	// Bumped to 2 with the date-arithmetic + JSON-output instructions.
	TemplateVersion prompts.Version = 2

	// RewriteMaxTokens caps the model response for the query rewrite call.
	// The spec targets ~500 output tokens; 512 leaves margin without
	// allowing runaway prose.
	rewriteMaxTokens = 512

	// RewriteToolName is the tool identifier the model uses when invoking
	// the structured-output tool. Stable wire string.
	rewriteToolName = "expand_query"
)

// rewriteSystemPrompt is the system prompt for the query-rewrite-v2 call.
// It teaches the model to (a) expand synonyms and (b) resolve a relative
// temporal anchor against the supplied CURRENT_DATE into an ISO 8601 UTC
// instant. The model must call the expand_query tool with {query, at?}.
const rewriteSystemPrompt = `You are a search-query expansion assistant for a personal knowledge graph.

TASK
Given a short search phrase, expand it for a hybrid retrieval system AND, when the
phrase contains a relative temporal anchor (e.g. "two months ago", "last Tuesday",
"yesterday"), resolve that anchor against the supplied CURRENT_DATE into an absolute
ISO 8601 UTC instant.

OUTPUT
Always invoke the expand_query tool exactly once. Fields:
- query (string, required): the expanded query text. One line, no quotes, no prefix.
- at (string, RFC 3339 UTC, optional): set ONLY when the original phrase contains a
  relative temporal anchor AND CURRENT_DATE was provided. Omit otherwise.

EXPANSION RULES
- Expand abbreviations and short names to their full forms when possible.
- Add synonyms and related terms that share the same intent.
- Preserve the original intent exactly; do NOT change what is being asked.
- The expansion should still contain the salient entities and nouns from the original
  phrase so a BM25 layer can match on them.

TEMPORAL RULES
- "yesterday" -> CURRENT_DATE minus 1 day, at 00:00:00 UTC.
- "last week" / "a week ago" -> CURRENT_DATE minus 7 days, at 00:00:00 UTC.
- "two months ago" / "N months ago" -> CURRENT_DATE minus N calendar months, at 00:00:00 UTC.
- "last Tuesday" -> the most recent Tuesday strictly before CURRENT_DATE.
- "Wednesday two months ago" -> the Wednesday of the week containing (CURRENT_DATE minus 2 months).
- If no CURRENT_DATE is provided, do NOT set at.
- If the phrase contains an absolute date already (e.g. "March 5"), do NOT set at; the
  recall layer can match it via FTS.

EXAMPLES

CURRENT_DATE: 2023-04-04
Input: Wednesday two months ago, what activity did Rachel and I start together?
Output tool call: {"query": "Rachel and me starting a new activity together hobby class lesson", "at": "2023-02-01T00:00:00Z"}

CURRENT_DATE: (not provided)
Input: Who works at Erebor?
Output tool call: {"query": "people employed by Erebor Inc. staff employees team members"}

CURRENT_DATE: 2026-05-20
Input: Berlin trip last week
Output tool call: {"query": "trips to Berlin Germany travel visits journey", "at": "2026-05-13T00:00:00Z"}

CURRENT_DATE: 2025-11-10
Input: NeurIPS 2024
Output tool call: {"query": "NeurIPS 2024 conference machine learning AI neural information processing systems Vancouver"}
`

// rewriteInputSchema is the JSON Schema the model must satisfy when invoking
// expand_query. Pinned as a top-level const so tests can re-validate it.
const rewriteInputSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "minLength": 1, "description": "Expanded search phrase. Required."},
    "at":    {"type": "string", "format": "date-time", "description": "Resolved absolute RFC 3339 UTC instant when the original phrase contained a relative temporal anchor. Omit otherwise."}
  },
  "required": ["query"],
  "additionalProperties": false
}`

// RegisterPrompt registers the query-rewrite template with reg. Call once
// at process startup, before any Rewriter.Rewrite calls. The same
// (Name, Version, Body) triple is idempotent; a different Body at the same
// Version returns prompts.ErrVersionConflict so accidental downgrades surface
// at startup.
func RegisterPrompt(ctx context.Context, reg prompts.Registry) error {
	return reg.Register(ctx, prompts.Template{
		Name:    TemplateName,
		Version: TemplateVersion,
		Body:    rewriteSystemPrompt,
		Description: "Query rewriting prompt. Expands a terse search phrase into a richer query " +
			"and (when CURRENT_DATE is supplied) resolves relative temporal anchors into an ISO " +
			"8601 UTC instant. Registered by internal/kernel/rewriter; consumed via ToolUse.",
	})
}

// Rewriter wraps a kernel.ChatProvider and a prompts.Registry to expand terse
// recall queries into richer search phrases and resolve relative date anchors.
// It is the kernel implementation of LLM leverage point #7 (query-rewrite).
// Safe for concurrent use.
type Rewriter struct {
	chat    kernel.ChatProvider
	prompts prompts.Registry
	model   string
}

// New constructs a Rewriter. Chat and reg must be non-nil; model must be
// non-empty. The query-rewrite template must have been registered with reg
// via RegisterPrompt before the first Rewrite call.
func New(chat kernel.ChatProvider, reg prompts.Registry, model string) (*Rewriter, error) {
	if chat == nil {
		return nil, errors.New("rewriter: nil chat provider")
	}
	if reg == nil {
		return nil, errors.New("rewriter: nil prompts registry")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("rewriter: empty model")
	}
	return &Rewriter{
		chat:    chat,
		prompts: reg,
		model:   model,
	}, nil
}

// rewriteOutput is the decoded tool-call shape. Mirrors rewriteInputSchema.
type rewriteOutput struct {
	Query string `json:"query"`
	At    string `json:"at,omitempty"`
}

// Rewrite expands original into a richer search query and, when hint.CurrentDate
// is set and the original contains a relative temporal anchor, resolves the
// anchor into an absolute timestamp returned in the result's At field.
//
// On any error (prompt render failure, provider failure, missing tool call,
// decode failure, empty expansion) it returns RewriteResult{Query: original}
// and a wrapped error so the caller can continue with graceful degradation.
func (rw *Rewriter) Rewrite(ctx context.Context, original string, hint kernel.RewriteHint) (kernel.RewriteResult, error) {
	fallback := kernel.RewriteResult{Query: original}

	system, err := rw.prompts.Render(ctx, TemplateName, TemplateVersion, prompts.RenderVars{})
	if err != nil {
		return fallback, fmt.Errorf("rewriter: render prompt: %w", err)
	}

	userMsg := buildUserMessage(original, hint.CurrentDate)

	resp, err := rw.chat.ToolUse(ctx, kernel.ToolUseRequest{
		Model:  rw.model,
		System: system,
		Messages: []kernel.Message{
			{Role: "user", Content: userMsg},
		},
		Tool: kernel.ToolSchema{
			Name:        rewriteToolName,
			Description: "Emit the expanded query and, when the original phrase contains a relative temporal anchor resolvable against CURRENT_DATE, an absolute ISO 8601 UTC timestamp.",
			InputSchema: json.RawMessage(rewriteInputSchema),
		},
		MaxTokens: rewriteMaxTokens,
	})
	if err != nil {
		return fallback, fmt.Errorf("rewriter: tool use: %w", err)
	}
	if resp.ToolUse == nil {
		return fallback, fmt.Errorf("rewriter: model did not invoke tool (stop=%q)", resp.StopReason)
	}

	var out rewriteOutput
	if err := json.Unmarshal(resp.ToolUse.Input, &out); err != nil {
		return fallback, fmt.Errorf("rewriter: decode tool input: %w", err)
	}

	expanded := strings.TrimSpace(out.Query)
	if expanded == "" {
		return fallback, nil
	}

	result := kernel.RewriteResult{Query: expanded}
	if strings.TrimSpace(out.At) != "" {
		t, perr := time.Parse(time.RFC3339, out.At)
		if perr == nil {
			t = t.UTC()
			result.At = &t
		}
		// Parse failure is non-fatal: drop At, keep the expanded query.
	}
	return result, nil
}

// buildUserMessage formats the user-side turn for the model. The CURRENT_DATE
// line is omitted entirely when no hint is supplied so the model does not see
// a placeholder and try to resolve dates against "unknown".
func buildUserMessage(original string, currentDate *time.Time) string {
	var b strings.Builder
	if currentDate != nil {
		b.WriteString("CURRENT_DATE: ")
		b.WriteString(currentDate.UTC().Format("2006-01-02"))
		b.WriteString("\n\n")
	}
	b.WriteString("Input: ")
	b.WriteString(original)
	return b.String()
}
