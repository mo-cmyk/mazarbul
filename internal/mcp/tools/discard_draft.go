package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

const discardDraftInputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "minLength": 1, "description": "Resource name or bare UID of the draft record to discard: episodes/{uid} or a bare ULID."},
    "reason": {"type": "string", "description": "Optional human-readable reason for discarding. Recorded in the audit event."}
  },
  "required": ["name"],
  "additionalProperties": false
}`

const discardDraftOutputSchema = `{
  "type": "object",
  "properties": {
    "status": {"type": "string", "enum": ["discarded"], "description": "Always \"discarded\" on success."},
    "name":   {"type": "string", "description": "Resource name of the discarded record."}
  },
  "required": ["status", "name"],
  "additionalProperties": false
}`

// DiscardDraft removes a draft-scoped episode from the staging area. It
// appends a scopeDiscarded event; the dispatcher projection DELETEs the
// episode and all derived rows sourced from it. The event log itself is
// append-only — the audit trail is preserved.
//
// Unlisted: this tool backs the `maz scope draft discard` CLI verb.
type DiscardDraft struct {
	appender Appender
	nav      Navigator
}

// NewDiscardDraft constructs a DiscardDraft tool.
func NewDiscardDraft(a Appender, nav Navigator) *DiscardDraft {
	return &DiscardDraft{appender: a, nav: nav}
}

// Name implements mcp.Tool.
func (*DiscardDraft) Name() string { return "discardDraft" }

// Unlisted hides this tool from tools/list; it backs the CLI surface.
func (*DiscardDraft) Unlisted() bool { return true }

// Description implements mcp.Tool.
func (*DiscardDraft) Description() string {
	return "Discard a draft-scoped episode from the staging area. " +
		"The episode and all derived rows (entities, mentions, relations, facts) are removed from the projection; " +
		"the event log audit trail is preserved. " +
		"Pass the resource name (episodes/{uid}) or bare uid from `maz scope draft list`."
}

// InputSchema implements mcp.Tool.
func (*DiscardDraft) InputSchema() json.RawMessage {
	return json.RawMessage(discardDraftInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*DiscardDraft) OutputSchema() json.RawMessage {
	return json.RawMessage(discardDraftOutputSchema)
}

type discardDraftInput struct {
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"`
}

type discardDraftOutput struct {
	Status string `json:"status"`
	Name   string `json:"name"`
}

// Invoke implements mcp.Tool.
func (d *DiscardDraft) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in discardDraftInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.Name == "" {
		return nil, mcp.NewValidationError("name is required", nil)
	}

	uid, err := resolveEpisodeUID(in.Name)
	if err != nil {
		return nil, err
	}

	// Pre-check: the episode must exist in draft scope.
	draftNav := navWithScopes(d.nav, kernel.ScopeDraftReadSet())
	if _, gerr := draftNav.GetEpisode(ctx, uid); gerr != nil {
		if errors.Is(gerr, navigate.ErrNotFound) {
			return nil, mcp.NewNotFoundError(
				fmt.Sprintf("episode %q not found in draft scope; use `maz scope draft list` to browse staged episodes", in.Name))
		}
		return nil, mcp.NewInternalError(fmt.Errorf("read draft episode %q: %w", uid, gerr))
	}

	payload, merr := json.Marshal(ontology.ScopeDiscardedEvent{
		TargetUID: uid,
		Reason:    in.Reason,
	})
	if merr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal scopeDiscarded payload: %w", merr))
	}

	receipts, aerr := d.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindScopeDiscarded),
		Source:     "mcp:discardDraft",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		Payload:    payload,
	}})
	if aerr != nil {
		if errors.Is(aerr, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("append scopeDiscarded: %v", aerr), nil)
		}
		return nil, mapAppendError("append scopeDiscarded", aerr)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	out := discardDraftOutput{
		Status: "discarded",
		Name:   collectionEpisodes + "/" + uid,
	}
	body, merr := json.Marshal(out)
	if merr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", merr))
	}
	return body, nil
}
