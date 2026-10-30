package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/feedback"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/version"
	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

const agentFeedbackInputSchema = `{
  "type": "object",
  "properties": {
    "category":     {"type": "string", "enum": ["confusing", "missing", "wrong_result", "slow", "tool_error", "suggestion", "lesson"], "description": "Kind of feedback. confusing|missing|wrong_result|slow|tool_error|suggestion are friction; lesson is a durable rule you learned. Required."},
    "severity":     {"type": "string", "enum": ["low", "medium", "high"], "description": "Impact. high promotes into guidance at a lower recurrence threshold. Required."},
    "summary":      {"type": "string", "minLength": 1, "description": "One-line description of the friction or rule. Required."},
    "surface":      {"type": "string", "description": "Tool / CLI / instruction the feedback is about, e.g. recall, \"maz load\". Optional but encouraged (part of the recurrence cluster)."},
    "detail":       {"type": "string", "description": "Optional longer context: reproduction or expected-vs-actual."},
    "rule":         {"type": "string", "description": "For a lesson: the distilled one-line durable rule, phrased as guidance. This is what gets promoted into the session guidance. Optional."},
    "keywords":     {"type": "array", "items": {"type": "string"}, "description": "Optional tags aiding clustering + search."},
    "activePreset": {"type": "string", "description": "Active steering preset, if any. Optional; auto-context."},
    "scope":        {"type": "string", "enum": ["personal", "project", "team"], "description": "Scope to record into. Defaults to personal (device-local diagnostic)."}
  },
  "required": ["category", "summary"],
  "additionalProperties": false
}`

const agentFeedbackOutputSchema = `{
  "type": "object",
  "properties": {
    "status":     {"type": "string", "enum": ["recorded"], "description": "Always \"recorded\" on success."},
    "clusterKey": {"type": "string", "description": "Recurrence cluster this feedback was filed under. Reportable via maz feedback."}
  },
  "required": ["status", "clusterKey"],
  "additionalProperties": false
}`

// validFeedbackCategories / validFeedbackSeverities mirror the schema enums for
// the CLI/in-process path, which bypasses JSON-schema validation.
//
//nolint:gochecknoglobals // immutable lookup sets, package-private
var (
	validFeedbackCategories = map[string]bool{
		"confusing": true, "missing": true, "wrong_result": true, "slow": true,
		"tool_error": true, "suggestion": true, "lesson": true,
	}
	validFeedbackSeverities = map[string]bool{"low": true, "medium": true, "high": true}
)

// AgentFeedback is the intake tool of the dynamic lessons / feedback loop. An
// agent flags a durable friction point or a distilled rule it learned; the
// dispatcher projects it into agent_feedback, and recurring items auto-promote
// into the per-session guidance (reviewable via `maz feedback`). Use it for
// DURABLE friction or rules, not one-off disagreements.
type AgentFeedback struct {
	appender Appender
	querier  feedback.Querier
}

// NewAgentFeedback constructs the tool bound to appender a and querier q.
// The querier is used to detect threshold crossing for auto-promotion audit
// events; pass nil to skip promotion detection (safe for tests that only
// exercise the record path).
func NewAgentFeedback(a Appender, q feedback.Querier) *AgentFeedback {
	return &AgentFeedback{appender: a, querier: q}
}

// Name implements mcp.Tool.
func (*AgentFeedback) Name() string { return "agentFeedback" }

// Description implements mcp.Tool.
func (*AgentFeedback) Description() string {
	return "Record durable feedback about using Mazarbul: a friction point (confusing|missing|wrong_result|slow|tool_error|suggestion) or a lesson you learned (a one-line rule). Give a category, severity, and one-line summary; optionally the surface (tool/CLI), detail, and a distilled rule. Recurring feedback auto-promotes into your per-session guidance (the user can pin/edit/veto). File DURABLE friction or rules, not one-off disagreements."
}

// InputSchema implements mcp.Tool.
func (*AgentFeedback) InputSchema() json.RawMessage { return json.RawMessage(agentFeedbackInputSchema) }

// OutputSchema implements mcp.Tool.
func (*AgentFeedback) OutputSchema() json.RawMessage {
	return json.RawMessage(agentFeedbackOutputSchema)
}

type agentFeedbackInput struct {
	Category     string   `json:"category"`
	Severity     string   `json:"severity"`
	Summary      string   `json:"summary"`
	Surface      string   `json:"surface,omitempty"`
	Detail       string   `json:"detail,omitempty"`
	Rule         string   `json:"rule,omitempty"`
	Keywords     []string `json:"keywords,omitempty"`
	ActivePreset string   `json:"activePreset,omitempty"`
	Scope        string   `json:"scope,omitempty"`
}

type agentFeedbackOutput struct {
	Status     string             `json:"status"`
	ClusterKey string             `json:"clusterKey"`
	Warnings   []warncode.Warning `json:"warnings,omitempty"`
}

// Invoke validates the input, derives a deterministic cluster key, and appends
// an agentFeedbackRecorded event. Required fields + enums are re-checked here
// because the CLI/in-process path bypasses the JSON-schema validator.
func (t *AgentFeedback) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in agentFeedbackInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.Summary == "" {
		return nil, mcp.NewValidationError("summary is required", nil)
	}
	if !validFeedbackCategories[in.Category] {
		return nil, mcp.NewValidationError(fmt.Sprintf("category %q is not one of confusing|missing|wrong_result|slow|tool_error|suggestion|lesson", in.Category), nil)
	}
	severity := in.Severity
	if severity == "" {
		severity = "low"
	}
	if !validFeedbackSeverities[severity] {
		return nil, mcp.NewValidationError(fmt.Sprintf("severity %q is not one of low|medium|high", severity), nil)
	}
	scope, serr := parseWriteScope(in.Scope)
	if serr != nil {
		return nil, serr
	}

	// Salient text for the deterministic cluster key: the distilled rule for a
	// lesson, else the summary. This collapses paraphrases of the same friction
	// onto one recurrence cluster (embedder-free; stable on replay).
	salient := in.Rule
	if salient == "" {
		salient = in.Summary
	}
	clusterKey := ontology.LexicalClusterKey(in.Category, in.Surface, salient)

	payload, err := json.Marshal(ontology.AgentFeedbackRecordedEvent{
		Category:     in.Category,
		Severity:     severity,
		Surface:      in.Surface,
		Summary:      in.Summary,
		Detail:       in.Detail,
		Rule:         in.Rule,
		Keywords:     in.Keywords,
		Version:      version.Version,
		ActivePreset: in.ActivePreset,
		ClusterKey:   clusterKey,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal agent feedback: %w", err))
	}

	receipts, err := t.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindAgentFeedbackRecorded),
		Source:     "mcp:agentFeedback",
		Confidence: 1.0,
		Scope:      scope,
		Payload:    payload,
	}})
	if err != nil {
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("invalid agent feedback: %v", err), nil)
		}
		return nil, mapAppendError("append agent feedback", err)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	// Auto-promotion audit: if this cluster just crossed its recurrence threshold
	// for the first time, append an agentFeedbackPromoted event (the event log is
	// the durable changelog) and surface MAZ-FEEDBACK-001 to the caller.
	warnings := t.promotionWarnings(ctx, clusterKey, in.Rule, severity)

	body, err := json.Marshal(agentFeedbackOutput{Status: "recorded", ClusterKey: clusterKey, Warnings: warnings})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// promotionWarnings runs the best-effort auto-promotion check and returns the
// MAZ-FEEDBACK-001 warning iff this call promoted the cluster for the first
// time. A nil querier (tests) or a promotion-append error yields no warning:
// the feedback itself is already durably recorded by the caller, and promotion
// detection is intentionally non-fatal. Idempotency is enforced by the
// agent_feedback_promotions table (ON CONFLICT DO NOTHING in the projection).
func (t *AgentFeedback) promotionWarnings(ctx context.Context, clusterKey, rule, severity string) []warncode.Warning {
	if t.querier == nil {
		return nil
	}
	promoted, err := t.maybeAppendPromotion(ctx, clusterKey, rule, severity)
	if err != nil || !promoted {
		return nil
	}
	return []warncode.Warning{warncode.New(warncode.CodeFeedbackPromoted,
		"recurring lesson crossed the promotion threshold; review with `maz feedback gotchas`")}
}

// maybeAppendPromotion checks whether clusterKey just crossed its promotion
// threshold for the first time and, if so, appends an agentFeedbackPromoted
// event. It is idempotent: the dispatcher's applyAgentFeedbackPromoted uses
// ON CONFLICT DO NOTHING on agent_feedback_promotions, so a replayed event is
// a cheap no-op. The querier must not be nil. It returns true only when it
// actually appended a promotion event this call, so the caller can surface
// MAZ-FEEDBACK-001 on the response.
func (t *AgentFeedback) maybeAppendPromotion(ctx context.Context, clusterKey, rule, severity string) (bool, error) {
	count, repRule, err := feedbackClusterCount(ctx, t.querier, clusterKey)
	if err != nil {
		return false, err
	}
	if count < feedback.PromotionThresholdFor(severity) {
		return false, nil // has not yet crossed threshold
	}
	already, err := feedbackAlreadyPromoted(ctx, t.querier, clusterKey)
	if err != nil {
		return false, err
	}
	if already {
		return false, nil // already promoted; do not emit a second event
	}
	// Prefer the caller-supplied rule; fall back to the representative from DB.
	if rule == "" {
		rule = repRule
	}
	promoPayload, err := json.Marshal(ontology.AgentFeedbackPromotedEvent{
		ClusterKey: clusterKey,
		Rule:       rule,
		Recurrence: count,
		Severity:   severity,
	})
	if err != nil {
		return false, fmt.Errorf("marshal promotion event: %w", err)
	}
	if _, aErr := t.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindAgentFeedbackPromoted),
		Source:     "mcp:agentFeedback",
		Confidence: 1.0,
		Payload:    promoPayload,
	}}); aErr != nil {
		return false, fmt.Errorf("append promotion event: %w", aErr)
	}
	return true, nil
}

// feedbackClusterCount returns the recurrence count and representative rule for
// a cluster. Each call opens and closes its own *sql.Rows so no cursor is live
// when the caller issues the next query on the same connection.
func feedbackClusterCount(ctx context.Context, q feedback.Querier, clusterKey string) (int, string, error) {
	// MAX(create_time) selects the newest row; SQLite's bare-column rule then
	// pulls `rule` from that same row, so the representative rule is the latest
	// one — not MAX(rule), which would pick the lexicographically-largest (stale)
	// text. create_time sorts chronologically (matching loadClusters' ORDER BY).
	rows, err := q.QueryContext(ctx,
		`SELECT COUNT(*), rule, MAX(create_time) FROM agent_feedback WHERE cluster_key = ?`, clusterKey)
	if err != nil {
		return 0, "", fmt.Errorf("query cluster count: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var count int
	var repRule, newest string
	if rows.Next() {
		if sErr := rows.Scan(&count, &repRule, &newest); sErr != nil {
			return 0, "", fmt.Errorf("scan cluster count: %w", sErr)
		}
	}
	if rErr := rows.Err(); rErr != nil {
		return 0, "", fmt.Errorf("iterate cluster count: %w", rErr)
	}
	return count, repRule, nil
}

// feedbackAlreadyPromoted reports whether clusterKey has an entry in
// agent_feedback_promotions (the idempotency guard). Each call opens and closes
// its own *sql.Rows so no cursor is live when the caller issues the next query.
func feedbackAlreadyPromoted(ctx context.Context, q feedback.Querier, clusterKey string) (bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT 1 FROM agent_feedback_promotions WHERE cluster_key = ?`, clusterKey)
	if err != nil {
		return false, fmt.Errorf("query promotions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	found := rows.Next()
	if rErr := rows.Err(); rErr != nil {
		return false, fmt.Errorf("iterate promotions: %w", rErr)
	}
	return found, nil
}
