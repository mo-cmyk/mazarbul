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
)

const feedbackReviewInputSchema = `{
  "type": "object",
  "properties": {
    "clusterKey": {"type": "string", "minLength": 1, "description": "Cluster to review (the cluster_key from the feedback gotchas surface). Required."},
    "action":     {"type": "string", "enum": ["pin", "unpin", "veto", "unveto", "edit"], "description": "pin|unpin toggle cap-eviction protection; veto|unveto toggle suppression; edit sets editedRule. Required."},
    "editedRule": {"type": "string", "description": "Override one-liner for action=edit. Ignored for other actions."}
  },
  "required": ["clusterKey", "action"],
  "additionalProperties": false
}`

const feedbackReviewOutputSchema = `{
  "type": "object",
  "properties": {
    "status":     {"type": "string", "enum": ["reviewed"], "description": "Always \"reviewed\" on success."},
    "clusterKey": {"type": "string"},
    "action":     {"type": "string"}
  },
  "required": ["status", "clusterKey", "action"],
  "additionalProperties": false
}`

//nolint:gochecknoglobals // immutable lookup set, package-private
var validReviewActions = map[string]bool{
	"pin": true, "unpin": true, "veto": true, "unveto": true, "edit": true,
}

// FeedbackReview is the human-review write of the auto-promote-and-review loop:
// it appends an agentFeedbackReviewed event that pins, vetoes, or edits a
// promoted feedback cluster. Unlisted from agent tool discovery — it is the
// user's override surface (`maz feedback pin|veto|edit`), not an agent action.
type FeedbackReview struct {
	appender Appender
	querier  feedback.Querier
}

// NewFeedbackReview constructs the tool bound to appender a.
// Pass a querier to enable phantom-key detection (clusterKey must exist in
// agent_feedback before a review action is accepted). Nil querier skips the
// check (safe for tests that exercise validation paths only).
func NewFeedbackReview(a Appender, q ...feedback.Querier) *FeedbackReview {
	var q0 feedback.Querier
	if len(q) > 0 {
		q0 = q[0]
	}
	return &FeedbackReview{appender: a, querier: q0}
}

// Name implements mcp.Tool.
func (*FeedbackReview) Name() string { return "feedbackReview" }

// Unlisted implements mcp.UnlistedTool: hide from tools/list (still dispatchable
// by the `maz feedback` CLI). Review is a human override, not an agent action.
func (*FeedbackReview) Unlisted() bool { return true }

// Description implements mcp.Tool.
func (*FeedbackReview) Description() string {
	return "Review a promoted feedback cluster: pin (protect from cap eviction), unpin, veto (suppress + stop re-promoting), unveto, or edit (override the distilled rule text). Identify the cluster by its clusterKey."
}

// InputSchema implements mcp.Tool.
func (*FeedbackReview) InputSchema() json.RawMessage {
	return json.RawMessage(feedbackReviewInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*FeedbackReview) OutputSchema() json.RawMessage {
	return json.RawMessage(feedbackReviewOutputSchema)
}

type feedbackReviewInput struct {
	ClusterKey string `json:"clusterKey"`
	Action     string `json:"action"`
	EditedRule string `json:"editedRule,omitempty"`
}

type feedbackReviewOutput struct {
	Status     string `json:"status"`
	ClusterKey string `json:"clusterKey"`
	Action     string `json:"action"`
}

// Invoke validates + appends an agentFeedbackReviewed event.
func (t *FeedbackReview) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in feedbackReviewInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.ClusterKey == "" {
		return nil, mcp.NewValidationError("clusterKey is required", nil)
	}
	if !validReviewActions[in.Action] {
		return nil, mcp.NewValidationError(fmt.Sprintf("action %q is not one of pin|unpin|veto|unveto|edit", in.Action), nil)
	}
	if in.Action == "edit" && in.EditedRule == "" {
		return nil, mcp.NewValidationError("editedRule is required for action=edit", nil)
	}

	// Phantom-key guard: reject a review on a cluster that has never received
	// any feedback. Without this check, pin/veto/edit silently succeed on a
	// typo'd key and leave a dangling review row with no associated feedback.
	if t.querier != nil {
		exists, err := clusterExists(ctx, t.querier, in.ClusterKey)
		if err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("check cluster existence: %w", err))
		}
		if !exists {
			return nil, mcp.NewNotFoundError(fmt.Sprintf("cluster %q not found — use maz feedback list to see valid keys", in.ClusterKey))
		}
	}

	payload, err := json.Marshal(ontology.AgentFeedbackReviewedEvent{
		ClusterKey: in.ClusterKey,
		Action:     in.Action,
		EditedRule: in.EditedRule,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal feedback review: %w", err))
	}

	receipts, err := t.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindAgentFeedbackReviewed),
		Source:     "mcp:feedbackReview",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if err != nil {
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("invalid feedback review: %v", err), nil)
		}
		return nil, mapAppendError("append feedback review", err)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	body, err := json.Marshal(feedbackReviewOutput{Status: "reviewed", ClusterKey: in.ClusterKey, Action: in.Action})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// clusterExists reports whether any feedback row exists for the given clusterKey.
func clusterExists(ctx context.Context, q feedback.Querier, clusterKey string) (bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT 1 FROM agent_feedback WHERE cluster_key = ? LIMIT 1`, clusterKey)
	if err != nil {
		return false, fmt.Errorf("query cluster existence: %w", err)
	}
	defer rows.Close()
	exists := rows.Next()
	if rErr := rows.Err(); rErr != nil {
		return false, fmt.Errorf("iterate cluster existence: %w", rErr)
	}
	return exists, nil
}
