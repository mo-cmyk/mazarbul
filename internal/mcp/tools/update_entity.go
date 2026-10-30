package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

const updateEntityInputSchema = `{
  "type": "object",
  "properties": {
    "entityUid": {
      "type": "string",
      "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$",
      "description": "ULID of the entity to update. Required."
    },
    "kind": {
      "type": "string",
      "pattern": "^([a-z][a-z0-9_]*|[a-z][a-z0-9_]*(:[A-Za-z0-9_]+)+)$",
      "description": "New entity kind in snake_case. Omit to leave unchanged."
    },
    "displayName": {
      "type": "string",
      "minLength": 1,
      "description": "New human-readable label. Omit to leave unchanged."
    }
  },
  "required": ["entityUid"],
  "additionalProperties": false
}`

const updateEntityOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^entities/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the updated entity."},
    "status": {"type": "string", "enum": ["updated"], "description": "Always \"updated\" on success."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// UpdateEntity updates a named entity's kind and/or displayName by appending
// an entityUpdated event. At least one of kind or displayName must be provided;
// a no-op (neither field set) is rejected with E_VALIDATION. The entity must
// exist; an unknown uid is E_NOT_FOUND.
type UpdateEntity struct {
	appender Appender
	nav      entityGetter
}

// entityGetter is the narrow read-side surface UpdateEntity needs to confirm
// the target entity exists before appending.
type entityGetter interface {
	GetEntity(ctx context.Context, uid string) (navigate.Entity, error)
}

// NewUpdateEntity constructs an UpdateEntity tool.
func NewUpdateEntity(a Appender, nav entityGetter) *UpdateEntity {
	return &UpdateEntity{appender: a, nav: nav}
}

// Name implements mcp.Tool.
func (*UpdateEntity) Name() string { return "updateEntity" }

// Description implements mcp.Tool.
func (*UpdateEntity) Description() string {
	return "Relabel an entity's kind and/or displayName by appending an entityUpdated event. Supply at least one of kind or displayName; omitting both is E_VALIDATION. Returns E_NOT_FOUND when the uid does not resolve. Only the supplied fields change; confidence, properties, identifiers, and coordinates are untouched."
}

// InputSchema implements mcp.Tool.
func (*UpdateEntity) InputSchema() json.RawMessage { return json.RawMessage(updateEntityInputSchema) }

// OutputSchema implements mcp.Tool.
func (*UpdateEntity) OutputSchema() json.RawMessage { return json.RawMessage(updateEntityOutputSchema) }

type updateEntityInput struct {
	EntityUID   string `json:"entityUid"`
	Kind        string `json:"kind,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

type updateEntityOutput struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Invoke implements mcp.Tool.
func (u *UpdateEntity) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in updateEntityInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.EntityUID == "" {
		return nil, mcp.NewValidationError("entityUid is required", nil)
	}
	if strings.TrimSpace(in.Kind) == "" && strings.TrimSpace(in.DisplayName) == "" {
		return nil, mcp.NewValidationError("at least one of kind or displayName must be provided; omitting both is a no-op", nil)
	}

	// Confirm the entity exists before appending.
	if u.nav != nil {
		if _, err := u.nav.GetEntity(ctx, in.EntityUID); err != nil {
			if errors.Is(err, navigate.ErrNotFound) {
				return nil, mcp.NewNotFoundError("entities/" + in.EntityUID + " not found")
			}
			return nil, mcp.NewInternalError(fmt.Errorf("updateEntity: verify entity: %w", err))
		}
	}

	payload, err := json.Marshal(ontology.EntityUpdatedEvent{
		EntityUID:   in.EntityUID,
		Kind:        in.Kind,
		DisplayName: in.DisplayName,
		UpdatedAt:   time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal entityUpdated payload: %w", err))
	}

	receipts, err := u.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindEntityUpdated),
		Source:     "mcp/updateEntity",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if err != nil {
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("append entityUpdated: %v", err), nil)
		}
		return nil, mapAppendError("append entityUpdated", err)
	}
	if len(receipts) == 0 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	return marshalOutput(updateEntityOutput{
		Name:   collectionEntities + "/" + in.EntityUID,
		Status: "updated",
	})
}
