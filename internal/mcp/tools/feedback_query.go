package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/feedback"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

const feedbackQueryInputSchema = `{
  "type": "object",
  "properties": {
    "view":     {"type": "string", "enum": ["gotchas", "list", "export"], "description": "gotchas = the promoted guidance set; list = raw triage rows; export = allowlist-redacted diagnostic. Defaults to gotchas."},
    "category": {"type": "string", "description": "Filter by category (list/export)."},
    "severity": {"type": "string", "description": "Filter by severity (list/export)."},
    "surface":  {"type": "string", "description": "Filter by surface (list/export)."},
    "since":    {"type": "string", "description": "RFC3339 lower bound on createTime (list/export)."},
    "limit":    {"type": "integer", "description": "Max rows (list/export). 0 = default."},
    "format":   {"type": "string", "enum": ["json", "md"], "description": "export only: json items (default) or a markdown diagnostic."}
  },
  "additionalProperties": false
}`

const feedbackQueryOutputSchema = `{
  "type": "object",
  "properties": {
    "view":     {"type": "string"},
    "gotchas":  {"type": "array"},
    "items":    {"type": "array"},
    "markdown": {"type": "string"}
  },
  "additionalProperties": true
}`

// FeedbackQuery is the read surface of the feedback loop: gotchas (the promoted
// guidance set), list (raw triage), and export (allowlist-redacted diagnostic).
// Unlisted — it backs the `maz feedback` CLI; the agent already receives the
// promoted gotchas in its session instructions.
type FeedbackQuery struct {
	q feedback.Querier
}

// NewFeedbackQuery constructs the tool bound to read-querier q.
func NewFeedbackQuery(q feedback.Querier) *FeedbackQuery { return &FeedbackQuery{q: q} }

// Name implements mcp.Tool.
func (*FeedbackQuery) Name() string { return "feedbackQuery" }

// Unlisted implements mcp.UnlistedTool.
func (*FeedbackQuery) Unlisted() bool { return true }

// Description implements mcp.Tool.
func (*FeedbackQuery) Description() string {
	return "Read the feedback loop: view=gotchas (the promoted per-session guidance set with recurrence + pin status), list (raw triage rows with cluster keys), or export (allowlist-redacted diagnostic, json or markdown). Backs the maz feedback CLI."
}

// InputSchema implements mcp.Tool.
func (*FeedbackQuery) InputSchema() json.RawMessage { return json.RawMessage(feedbackQueryInputSchema) }

// OutputSchema implements mcp.Tool.
func (*FeedbackQuery) OutputSchema() json.RawMessage {
	return json.RawMessage(feedbackQueryOutputSchema)
}

type feedbackQueryInput struct {
	View     string `json:"view,omitempty"`
	Category string `json:"category,omitempty"`
	Severity string `json:"severity,omitempty"`
	Surface  string `json:"surface,omitempty"`
	Since    string `json:"since,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Format   string `json:"format,omitempty"`
}

// Invoke dispatches on the view, reading through the feedback package.
func (t *FeedbackQuery) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in feedbackQueryInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	view := in.View
	if view == "" {
		view = "gotchas"
	}
	sincet, err := feedback.ParseSince(in.Since)
	if err != nil {
		return nil, mcp.NewValidationError(err.Error(), nil)
	}
	filter := feedback.Filter{
		Category: in.Category, Severity: in.Severity, Surface: in.Surface,
		Since: sincet, Limit: in.Limit,
	}

	switch view {
	case "gotchas":
		gs, err := feedback.ActiveGotchas(ctx, t.q)
		if err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("read gotchas: %w", err))
		}
		return marshalFeedbackResult(map[string]any{"view": view, "gotchas": gs})
	case "list":
		items, err := feedback.List(ctx, t.q, &filter)
		if err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("read feedback list: %w", err))
		}
		return marshalFeedbackResult(map[string]any{"view": view, "items": items})
	case "export":
		items, err := feedback.Export(ctx, t.q, &filter)
		if err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("read feedback export: %w", err))
		}
		if in.Format == "md" {
			return marshalFeedbackResult(map[string]any{"view": view, "markdown": feedback.RenderExportMarkdown(items)})
		}
		return marshalFeedbackResult(map[string]any{"view": view, "items": items})
	default:
		return nil, mcp.NewValidationError(fmt.Sprintf("view %q is not one of gotchas|list|export", view), nil)
	}
}

func marshalFeedbackResult(m map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal feedback result: %w", err))
	}
	return body, nil
}
