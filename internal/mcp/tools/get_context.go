package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/contextbrief"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// briefBuilder is the assembly surface getContext consumes. Satisfied by
// *contextbrief.Assembler. Declared as an interface so the tool test can drive
// it without a live navigator/recall stack.
type briefBuilder interface {
	Build(ctx context.Context, req contextbrief.Request) (contextbrief.Brief, error)
}

const getContextInputSchema = `{
  "type": "object",
  "properties": {
    "query":        {"type": "string", "description": "Optional focus. When set, the brief adds a section of the most query-relevant hits (documents + episodes), co-ranked by the same recall used by recall/ask."},
    "budgetTokens": {"type": "integer", "minimum": 1, "maximum": 100000, "description": "Token budget for the brief (default 4000). The brief never exceeds it; the lowest-priority tail is dropped behind a marker."},
    "scopes":       {"type": "array", "items": {"type": "string"}, "description": "Scopes to read from: project, personal, team. Defaults to project+personal. Unknown values are rejected."}
  },
  "additionalProperties": false
}`

const getContextOutputSchema = `{
  "type": "object",
  "properties": {
    "markdown": {"type": "string", "description": "The assembled context brief as markdown: pinned documents, recently-updated documents, key entities, and (when a query is given) relevant hits. Load this verbatim at session start."},
    "tokens":   {"type": "integer", "description": "Estimated token count of the brief (always <= budget)."},
    "budget":   {"type": "integer", "description": "The token budget that was applied."},
    "dropped":  {"type": "integer", "description": "How many lower-priority items were omitted to fit the budget."},
    "sections": {"type": "array", "description": "Per-section counts of what made it into the brief.", "items": {"type": "object"}}
  },
  "required": ["markdown", "tokens", "budget"],
  "additionalProperties": false
}`

// GetContext assembles a token-budgeted cold-start context brief from the unified
// graph. Deterministic and LLM-free: it is the working set an agent loads at
// session open (plan §C).
type GetContext struct {
	builder briefBuilder
	waiter  BackgroundWaiter
}

// NewGetContext constructs a GetContext tool over a brief assembler.
func NewGetContext(builder briefBuilder) *GetContext { return &GetContext{builder: builder} }

// WithBackgroundWaiter installs a BackgroundWaiter consulted before a QUERIED
// brief is built. The relevant section runs the same HybridQuery as recall, so
// without this drain a brief built right after a burst of writes or imports
// would search the derived tables before extraction + resolution have populated
// them — omitting hits a direct recall would return. Returns g so the daemon can
// chain it at wiring time; a nil waiter is a silent no-op.
func (g *GetContext) WithBackgroundWaiter(w BackgroundWaiter) *GetContext {
	if w != nil {
		g.waiter = w
	}
	return g
}

// Name implements mcp.Tool.
func (*GetContext) Name() string { return "getContext" }

// Description implements mcp.Tool.
func (*GetContext) Description() string {
	return "Assemble a token-budgeted markdown brief of the user's working set — pinned documents, recently-updated documents, key entities, and (when you pass a `query`) the most relevant hits — for loading at the start of a session. Deterministic and fast (no LLM). The brief never exceeds `budgetTokens`; lower-priority content is dropped behind a visible marker. Prefer this over many small reads when you need to warm up context about the user."
}

// InputSchema implements mcp.Tool.
func (*GetContext) InputSchema() json.RawMessage { return json.RawMessage(getContextInputSchema) }

// OutputSchema implements mcp.Tool.
func (*GetContext) OutputSchema() json.RawMessage { return json.RawMessage(getContextOutputSchema) }

type getContextInput struct {
	Query        string   `json:"query"`
	BudgetTokens int      `json:"budgetTokens"`
	Scopes       []string `json:"scopes,omitempty"`
}

// Invoke implements mcp.Tool.
func (g *GetContext) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in getContextInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	// Only a queried brief reads the recall lane, so the drain is gated on a
	// query: a query-less brief (pinned/recent/entities) reads projections that
	// the dispatcher keeps current and needs no wait. Mirrors recall's drain;
	// ctx-respecting, so an expired deadline searches partial state rather than
	// erroring.
	if in.Query != "" && g.waiter != nil {
		if werr := g.waiter.WaitForBackgroundWork(ctx); werr != nil {
			logging.FromContext(ctx).Debug(
				"getContext: background drain incomplete, building partial brief",
				zap.Error(werr),
			)
		}
	}
	if len(in.Scopes) > 0 {
		ss, serr := kernel.ParseScopeSet(in.Scopes)
		if serr != nil {
			return nil, mcp.NewValidationError(
				fmt.Sprintf("invalid scopes: %v (allowed: project, personal, team)", serr), nil)
		}
		// Draft is staging-only: the context brief must never surface unreviewed
		// draft data. Inspect the staging area via the draftStage tool instead.
		if ss.Contains(kernel.ScopeDraft) {
			return nil, mcp.NewValidationError(
				`scope "draft" is not readable here; use the draftStage tool to inspect the staging area`, nil)
		}
	}
	brief, err := g.builder.Build(ctx, contextbrief.Request{Query: in.Query, BudgetTokens: in.BudgetTokens, Scopes: in.Scopes})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("getContext: build brief: %w", err))
	}
	return marshalTool(brief)
}
