package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

const mergeEntityInputSchema = `{
  "type": "object",
  "properties": {
    "sourceEntityUid": {
      "type": "string",
      "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$",
      "description": "ULID of the entity to absorb (will be deleted). Required."
    },
    "targetEntityUid": {
      "type": "string",
      "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$",
      "description": "ULID of the surviving entity. Must differ from sourceEntityUid. Required."
    }
  },
  "required": ["sourceEntityUid", "targetEntityUid"],
  "additionalProperties": false
}`

const mergeEntityOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^entities/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the surviving (target) entity."},
    "status": {"type": "string", "enum": ["merged"], "description": "Always \"merged\" on success."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// MergeEntity merges the source entity into the target by appending an
// entityMerged event. The dispatcher repoints all relations, facts, and
// mentions from source to target, then removes the source entity row. Returns
// E_NOT_FOUND when either entity is absent, and E_VALIDATION when source and
// target are the same uid.
type MergeEntity struct {
	appender Appender
	nav      entityGetter
}

// NewMergeEntity constructs a MergeEntity tool.
func NewMergeEntity(a Appender, nav entityGetter) *MergeEntity {
	return &MergeEntity{appender: a, nav: nav}
}

// Name implements mcp.Tool. The agent-facing verb is imperative
// ("mergeEntities") to match its createEntity/deleteEntity/updateEntity
// siblings; the event it appends is the past-tense entityMerged (kept).
func (*MergeEntity) Name() string { return "mergeEntities" }

// Description implements mcp.Tool.
func (*MergeEntity) Description() string {
	return "Merge source entity into target: repoints all relations, facts, and mentions from source to target, then removes the source entity row. Append-only: an entityMerged event is written to the audit log. Returns E_VALIDATION when source and target are the same uid, E_NOT_FOUND when either uid is unknown."
}

// InputSchema implements mcp.Tool.
func (*MergeEntity) InputSchema() json.RawMessage { return json.RawMessage(mergeEntityInputSchema) }

// OutputSchema implements mcp.Tool.
func (*MergeEntity) OutputSchema() json.RawMessage { return json.RawMessage(mergeEntityOutputSchema) }

type mergeEntityInput struct {
	SourceEntityUID string `json:"sourceEntityUid"`
	TargetEntityUID string `json:"targetEntityUid"`
}

type mergeEntityOutput struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Invoke implements mcp.Tool.
func (m *MergeEntity) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in mergeEntityInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.SourceEntityUID == "" || in.TargetEntityUID == "" {
		return nil, mcp.NewValidationError("sourceEntityUid and targetEntityUid are required", nil)
	}
	if in.SourceEntityUID == in.TargetEntityUID {
		return nil, mcp.NewValidationError("sourceEntityUid and targetEntityUid must be different; merging an entity with itself is a no-op", nil)
	}

	// Verify both entities exist before appending. The source's scope is stamped
	// on the entityMerged event: a merge deletes the source and repoints its data
	// onto the target, so — like deleteEntity — the operation must be gated in the
	// scope of the entity being removed. Merging a team-scoped entity away must
	// require team membership, not slip through the personal default Append
	// applies to an unset scope. Left empty (→ personal) only when no nav wired.
	var scope kernel.Scope
	if m.nav != nil {
		src, err := m.requireEntity(ctx, in.SourceEntityUID)
		if err != nil {
			return nil, err
		}
		scope = kernel.Scope(src.Scope)
		if _, err := m.requireEntity(ctx, in.TargetEntityUID); err != nil {
			return nil, err
		}
	}

	payload, err := json.Marshal(ontology.EntityMergedEvent{
		SourceEntityUID: in.SourceEntityUID,
		TargetEntityUID: in.TargetEntityUID,
		MergedAt:        time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal entityMerged payload: %w", err))
	}

	receipts, err := m.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindEntityMerged),
		Source:     "mcp/mergeEntities",
		Confidence: 1.0,
		Scope:      scope,
		Payload:    payload,
	}})
	if err != nil {
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("append entityMerged: %v", err), nil)
		}
		return nil, mapAppendError("append entityMerged", err)
	}
	if len(receipts) == 0 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	return marshalOutput(mergeEntityOutput{
		Name:   collectionEntities + "/" + in.TargetEntityUID,
		Status: "merged",
	})
}

// requireEntity confirms the uid exists via the navigator and returns the
// entity (so the caller can read its stored scope). Returns an MCP ToolError
// (E_NOT_FOUND or E_INTERNAL) on failure.
func (m *MergeEntity) requireEntity(ctx context.Context, uid string) (navigate.Entity, error) {
	ent, err := m.nav.GetEntity(ctx, uid)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return navigate.Entity{}, mcp.NewNotFoundError("entities/" + uid + " not found")
		}
		return navigate.Entity{}, mcp.NewInternalError(fmt.Errorf("mergeEntities: verify entity %s: %w", uid, err))
	}
	return ent, nil
}
