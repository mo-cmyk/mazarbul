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

const deleteEntityInputSchema = `{
  "type": "object",
  "properties": {
    "entityUid": {
      "type": "string",
      "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$",
      "description": "ULID of the entity to delete. Required."
    },
    "cascade": {
      "type": "boolean",
      "description": "Delete the entity even when it still has active relations. When true, each active relation is first tombstoned (relationForgotten) for the audit trail, then the entity and all its remaining derived data (facts, mentions, identifiers, citations) are removed. When false (default) the delete is REFUSED if any active relation exists, so a well-connected entity is never dropped by accident — merge it with mergeEntities instead, or pass cascade to confirm. Defaults to false."
    },
    "reason": {
      "type": "string",
      "description": "Optional free-text reason for the deletion. Surfaced in audit/log views and stamped on the cascaded relationForgotten tombstones."
    }
  },
  "required": ["entityUid"],
  "additionalProperties": false
}`

const deleteEntityOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^entities/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the deleted entity."},
    "status": {"type": "string", "enum": ["deleted"], "description": "Always \"deleted\" on success."},
    "forgottenRelations": {"type": "integer", "description": "Number of active relations tombstoned by the cascade before the entity was removed (0 unless cascade was needed)."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// DeleteEntity hard-deletes an entity by appending an entityDeleted tombstone.
// The dispatcher removes the entity row and every derived row that references it
// (relations, facts, mentions, identifiers, insights, citations, provenance) and
// detaches the graph node — all in one transaction. Unlike a merge there is
// no surviving target: this is for genuinely-unwanted or junk entities, not for
// consolidating duplicates (use mergeEntities for that, which preserves the data
// by repointing it).
//
// Safe default: an entity that still has active relations is NOT deleted unless
// cascade is set, so a connected entity is never silently destroyed. With
// cascade, each active relation is tombstoned (relationForgotten) for a per-edge
// audit trail before the entityDeleted tombstone is written.
type DeleteEntity struct {
	appender Appender
	nav      entityDeleteNav // may be nil → existence + active-edge guard skipped
}

// entityDeleteNav is the read-side surface DeleteEntity needs: confirm the
// entity exists, and enumerate its active relations to enforce the safe-delete
// guard and drive the cascade. Satisfied by *navigate.Navigator. Optional so
// minimal-boot / test-fake wiring still constructs the tool (guard then skipped).
type entityDeleteNav interface {
	GetEntity(ctx context.Context, uid string) (navigate.Entity, error)
	EntityRelations(ctx context.Context, uid string, opts navigate.RelationFilter) ([]navigate.Relation, error)
}

// NewDeleteEntity constructs a DeleteEntity tool bound to appender a. Nav may be
// nil to skip the existence and active-edge guards (minimal boot / tests).
func NewDeleteEntity(a Appender, nav entityDeleteNav) *DeleteEntity {
	return &DeleteEntity{appender: a, nav: nav}
}

// Name implements mcp.Tool.
func (*DeleteEntity) Name() string { return "deleteEntity" }

// Description implements mcp.Tool.
func (*DeleteEntity) Description() string {
	return "Hard-delete an entity by ULID: removes the entity and all its derived data (relations, facts, mentions, identifiers, citations) and detaches its graph node, appending an entityDeleted tombstone to the audit log. This is for unwanted or junk entities — to consolidate DUPLICATES without losing data use mergeEntities instead. Safe default: refuses with E_VALIDATION when the entity still has active relations (so a connected entity is never dropped by accident); pass cascade=true to tombstone those relations first and proceed. Returns E_NOT_FOUND when the uid is unknown."
}

// InputSchema implements mcp.Tool.
func (*DeleteEntity) InputSchema() json.RawMessage { return json.RawMessage(deleteEntityInputSchema) }

// OutputSchema implements mcp.Tool.
func (*DeleteEntity) OutputSchema() json.RawMessage { return json.RawMessage(deleteEntityOutputSchema) }

type deleteEntityInput struct {
	EntityUID string `json:"entityUid"`
	Cascade   bool   `json:"cascade,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type deleteEntityOutput struct {
	Name               string `json:"name"`
	Status             string `json:"status"`
	ForgottenRelations int    `json:"forgottenRelations,omitempty"`
}

// Invoke implements mcp.Tool. It confirms the entity exists, enforces the
// safe-delete guard (active relations require cascade), and appends a batch of
// relationForgotten tombstones (cascade only) followed by the entityDeleted
// tombstone in a single Append so the delete is atomic in the log.
func (d *DeleteEntity) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in deleteEntityInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.EntityUID == "" {
		return nil, mcp.NewValidationError("entityUid is required", nil)
	}

	// scope is the entity's storage scope, stamped on every tombstone this tool
	// appends so the team write-permission gate sees the real scope: deleting a
	// team-scoped entity (and cascading its edges) must require team membership,
	// not slip through the personal default Append applies to an unset scope.
	// Left empty (→ personal at Append) only when no nav is wired (minimal boot).
	var scope kernel.Scope
	var active []navigate.Relation
	if d.nav != nil {
		ent, err := d.nav.GetEntity(ctx, in.EntityUID)
		if err != nil {
			if errors.Is(err, navigate.ErrNotFound) {
				return nil, mcp.NewNotFoundError("entities/" + in.EntityUID + " not found; nothing to delete")
			}
			return nil, mcp.NewInternalError(fmt.Errorf("deleteEntity: verify entity %s: %w", in.EntityUID, err))
		}
		scope = kernel.Scope(ent.Scope)
		rels, err := d.nav.EntityRelations(ctx, in.EntityUID, navigate.RelationFilter{Limit: -1})
		if err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("deleteEntity: list relations for %s: %w", in.EntityUID, err))
		}
		active = rels
		if len(active) > 0 && !in.Cascade {
			return nil, mcp.NewValidationError(
				fmt.Sprintf("entities/%s still has %d active relation(s); pass cascade=true to tombstone them and delete the entity, or merge it with mergeEntities to preserve them", in.EntityUID, len(active)), nil)
		}
	}

	events := d.buildDeleteEvents(in, active, scope)
	receipts, err := d.appender.Append(ctx, events)
	if err != nil {
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("append entityDeleted: %v", err), nil)
		}
		return nil, mapAppendError("append entityDeleted", err)
	}
	if len(receipts) == 0 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	return marshalOutput(deleteEntityOutput{
		Name:               collectionEntities + "/" + in.EntityUID,
		Status:             "deleted",
		ForgottenRelations: len(active),
	})
}

// buildDeleteEvents assembles the event batch: one relationForgotten per active
// relation (cascade path; empty otherwise) followed by the entityDeleted
// tombstone. Marshal failures are impossible for these fixed-shape payloads, so
// the encode errors are dropped to keep the signature simple; a corrupt payload
// would surface as ErrInvalidEvent at Append.
func (d *DeleteEntity) buildDeleteEvents(in deleteEntityInput, active []navigate.Relation, scope kernel.Scope) []kernel.Event {
	now := time.Now().UTC()
	events := make([]kernel.Event, 0, len(active)+1)
	for _, rel := range active {
		payload, _ := json.Marshal(ontology.RelationForgottenEvent{ //nolint:errcheck // fixed-shape payload; a marshal fault surfaces as ErrInvalidEvent at Append
			RelationUID: rel.UID,
			Reason:      in.Reason,
			ForgottenAt: now,
		})
		events = append(events, kernel.Event{
			Kind:       ontology.KindRelationForgotten,
			Source:     "mcp/deleteEntity",
			Confidence: 1.0,
			Scope:      scope,
			Payload:    payload,
		})
	}
	payload, _ := json.Marshal(ontology.EntityDeletedEvent{ //nolint:errcheck // fixed-shape payload; a marshal fault surfaces as ErrInvalidEvent at Append
		EntityUID: in.EntityUID,
		Reason:    in.Reason,
		DeletedAt: now,
	})
	events = append(events, kernel.Event{
		Kind:       kernel.EventKind(ontology.KindEntityDeleted),
		Source:     "mcp/deleteEntity",
		Confidence: 1.0,
		Scope:      scope,
		Payload:    payload,
	})
	return events
}
