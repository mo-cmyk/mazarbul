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

const promoteDraftInputSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "minLength": 1, "description": "Resource name or bare UID of the draft record to promote: episodes/{uid} or a bare ULID."}
  },
  "required": ["name"],
  "additionalProperties": false
}`

const promoteDraftOutputSchema = `{
  "type": "object",
  "properties": {
    "status":  {"type": "string", "enum": ["promoted"], "description": "Always \"promoted\" on success."},
    "name":    {"type": "string", "description": "Resource name of the promoted record."},
    "toScope": {"type": "string", "description": "The durable scope the record was promoted into."}
  },
  "required": ["status", "name", "toScope"],
  "additionalProperties": false
}`

// PromoteDraft promotes a draft-scoped episode out of the staging area and
// into the personal scope. It appends a scopePromoted event; the dispatcher
// projection re-scopes the episode and all derived rows (mentions, relations,
// facts) sourced from it.
//
// Unlisted: this tool backs the `maz scope draft promote` CLI verb and is
// callable by agents that have been steered to use the staging workflow.
type PromoteDraft struct {
	appender Appender
	nav      Navigator
}

// NewPromoteDraft constructs a PromoteDraft tool.
func NewPromoteDraft(a Appender, nav Navigator) *PromoteDraft {
	return &PromoteDraft{appender: a, nav: nav}
}

// Name implements mcp.Tool.
func (*PromoteDraft) Name() string { return "promoteDraft" }

// Unlisted hides this tool from tools/list; it backs the CLI surface.
func (*PromoteDraft) Unlisted() bool { return true }

// Description implements mcp.Tool.
func (*PromoteDraft) Description() string {
	return "Promote a draft-scoped episode out of the staging area and into the personal scope. " +
		"Draft records are invisible to recall until promoted. " +
		"Pass the resource name (episodes/{uid}) or bare uid from `maz scope draft list`."
}

// InputSchema implements mcp.Tool.
func (*PromoteDraft) InputSchema() json.RawMessage {
	return json.RawMessage(promoteDraftInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*PromoteDraft) OutputSchema() json.RawMessage {
	return json.RawMessage(promoteDraftOutputSchema)
}

type promoteDraftInput struct {
	Name string `json:"name"`
}

type promoteDraftOutput struct {
	Status  string `json:"status"`
	Name    string `json:"name"`
	ToScope string `json:"toScope"`
}

// Invoke implements mcp.Tool.
func (p *PromoteDraft) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in promoteDraftInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.Name == "" {
		return nil, mcp.NewValidationError("name is required", nil)
	}

	// Resolve the uid from an episodes/{uid} name or bare uid.
	uid, err := resolveEpisodeUID(in.Name)
	if err != nil {
		return nil, err
	}

	// Pre-check: the episode must exist in draft scope.
	draftNav := navWithScopes(p.nav, kernel.ScopeDraftReadSet())
	if _, gerr := draftNav.GetEpisode(ctx, uid); gerr != nil {
		if errors.Is(gerr, navigate.ErrNotFound) {
			return nil, mcp.NewNotFoundError(
				fmt.Sprintf("episode %q not found in draft scope; use `maz scope draft list` to browse staged episodes", in.Name))
		}
		return nil, mcp.NewInternalError(fmt.Errorf("read draft episode %q: %w", uid, gerr))
	}

	const toScope = string(kernel.ScopePersonal)
	payload, merr := json.Marshal(ontology.ScopePromotedEvent{
		TargetUID: uid,
		FromScope: string(kernel.ScopeDraft),
		ToScope:   toScope,
	})
	if merr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal scopePromoted payload: %w", merr))
	}

	receipts, aerr := p.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindScopePromoted),
		Source:     "mcp:promoteDraft",
		Confidence: 1.0,
		Scope:      kernel.ScopePersonal,
		Payload:    payload,
	}})
	if aerr != nil {
		if errors.Is(aerr, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("append scopePromoted: %v", aerr), nil)
		}
		return nil, mapAppendError("append scopePromoted", aerr)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	out := promoteDraftOutput{
		Status:  "promoted",
		Name:    collectionEpisodes + "/" + uid,
		ToScope: toScope,
	}
	body, merr := json.Marshal(out)
	if merr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", merr))
	}
	return body, nil
}

// resolveEpisodeUID parses an episodes/{uid} resource name or bare uid.
func resolveEpisodeUID(name string) (string, error) {
	if name == "" {
		return "", mcp.NewValidationError("name is required", nil)
	}
	if looksLikeAIPResourceName(name) {
		collection, uid, err := parseResourceName(name)
		if err != nil {
			return "", err
		}
		if collection != collectionEpisodes {
			return "", mcp.NewValidationError(
				fmt.Sprintf("expected an episodes/{uid} name, got %q", name), nil)
		}
		return uid, nil
	}
	// Bare uid: pass through (dispatcher validates the ULID).
	return name, nil
}
