package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel/cognitive"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// CognitiveTrigger runs the cognitive-lifecycle jobs on demand (issue #231).
// Satisfied by *daemon.cognitiveSupervisor. The MCP triggers drive the SAME job
// methods the autonomous ticker calls, so integration tests and the
// isolated-daemon E2E can run reflection/consolidation deterministically instead
// of waiting on the (hourly/daily) tick. Unlike the ticker, the manual triggers
// are not idle-gated — the caller asked explicitly.
type CognitiveTrigger interface {
	RunReflection(ctx context.Context) (cognitive.Result, error)
	RunConsolidation(ctx context.Context) (cognitive.Result, error)
}

const cognitiveTriggerInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const cognitiveTriggerOutputSchema = `{
  "type": "object",
  "properties": {
    "appendedEventUids": {"type": "array", "items": {"type": "string"}, "description": "Event UIDs appended by this pass (insightSynthesized or memoryConsolidated)."},
    "appendedCount": {"type": "integer", "minimum": 0, "description": "Number of derived events appended."},
    "skipped": {"type": "string", "description": "Non-empty reason when the pass produced nothing: breaker-open (LLM backend unavailable), no-candidates, not-wired."}
  },
  "required": ["appendedEventUids", "appendedCount"],
  "additionalProperties": false
}`

type cognitiveTriggerOutput struct {
	AppendedEventUIDs []string `json:"appendedEventUids"`
	AppendedCount     int      `json:"appendedCount"`
	Skipped           string   `json:"skipped,omitempty"`
}

// cognitiveResultJSON marshals a job Result into the shared trigger output
// shape, normalizing a nil UID slice to [] so the schema's array type holds.
func cognitiveResultJSON(res cognitive.Result) (json.RawMessage, error) {
	uids := res.AppendedEventUIDs
	if uids == nil {
		uids = []string{}
	}
	body, err := json.Marshal(cognitiveTriggerOutput{
		AppendedEventUIDs: uids,
		AppendedCount:     len(uids),
		Skipped:           res.Skipped,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", err))
	}
	return body, nil
}

// ---------- triggerReflection ----------

// TriggerReflection runs one reflection pass (insight synthesis over entity
// clusters) on demand.
type TriggerReflection struct{ sv CognitiveTrigger }

// NewTriggerReflection constructs the tool over the cognitive supervisor.
func NewTriggerReflection(sv CognitiveTrigger) *TriggerReflection {
	return &TriggerReflection{sv: sv}
}

// Name implements mcp.Tool.
func (*TriggerReflection) Name() string { return "triggerReflection" }

// Description implements mcp.Tool.
func (*TriggerReflection) Description() string {
	return "Run one reflection pass now: cluster connected entities and synthesize durable insights, each citing the source records it was drawn from. Normally runs autonomously on an idle daily tick; call this to trigger it on demand. Returns the appended insightSynthesized event UIDs, or a 'skipped' reason (breaker-open when the LLM backend is down, no-candidates when there is nothing to reflect on)."
}

// InputSchema implements mcp.Tool.
func (*TriggerReflection) InputSchema() json.RawMessage {
	return json.RawMessage(cognitiveTriggerInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*TriggerReflection) OutputSchema() json.RawMessage {
	return json.RawMessage(cognitiveTriggerOutputSchema)
}

// Invoke implements mcp.Tool.
func (t *TriggerReflection) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	res, err := t.sv.RunReflection(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("reflection: %w", err))
	}
	return cognitiveResultJSON(res)
}

// ---------- triggerConsolidation ----------

// TriggerConsolidation runs one consolidation pass (compress recent episodes
// grouped by cited entity) on demand.
type TriggerConsolidation struct{ sv CognitiveTrigger }

// NewTriggerConsolidation constructs the tool over the cognitive supervisor.
func NewTriggerConsolidation(sv CognitiveTrigger) *TriggerConsolidation {
	return &TriggerConsolidation{sv: sv}
}

// Name implements mcp.Tool.
func (*TriggerConsolidation) Name() string { return "triggerConsolidation" }

// Description implements mcp.Tool.
func (*TriggerConsolidation) Description() string {
	return "Run one consolidation pass now: compress groups of recent episodes (grouped by the entity they cite) into concise consolidated memories, each citing the source episodes. Normally runs autonomously on an idle hourly tick; call this to trigger it on demand. Returns the appended memoryConsolidated event UIDs, or a 'skipped' reason (breaker-open when the LLM backend is down, no-candidates when there is nothing to consolidate)."
}

// InputSchema implements mcp.Tool.
func (*TriggerConsolidation) InputSchema() json.RawMessage {
	return json.RawMessage(cognitiveTriggerInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*TriggerConsolidation) OutputSchema() json.RawMessage {
	return json.RawMessage(cognitiveTriggerOutputSchema)
}

// Invoke implements mcp.Tool.
func (t *TriggerConsolidation) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	res, err := t.sv.RunConsolidation(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("consolidation: %w", err))
	}
	return cognitiveResultJSON(res)
}
