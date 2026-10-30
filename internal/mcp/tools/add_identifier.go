package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

const addIdentifierInputSchema = `{
  "type": "object",
  "properties": {
    "entityUid": {
      "type": "string",
      "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$",
      "description": "ULID of the existing entity to attach the identifier to. Required."
    },
    "identifier": {
      "type": "string",
      "minLength": 1,
      "description": "The surface form / external identifier to attach, e.g. a prior display name \"Erebor Global\", an email, a username, or a domain id. Required."
    },
    "kind": {
      "type": "string",
      "pattern": "^[a-z][a-z0-9_]*$",
      "description": "Identifier kind in snake_case. Canonical: alias, email, username, phone, id; domain types (hrb, lei, tax_id, …) keep their own lookup lane. Defaults to \"alias\" when omitted — the kind to use for retaining an old display name after a rename."
    }
  },
  "required": ["entityUid", "identifier"],
  "additionalProperties": false
}`

const addIdentifierOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^entities/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the entity the identifier was attached to."},
    "status": {"type": "string", "enum": ["added"], "description": "Always \"added\" on success."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

var identifierKindRE = regexp.MustCompile(ontology.IdentifierKindPattern)

// AddIdentifier attaches an identifier (alias by default) to an EXISTING entity
// by appending an entityIdentifierAdded event. This is the missing counterpart
// to createEntity, which only seeds identifiers at mint time: it lets a rename
// (updateEntity displayName) keep the old surface form as an alias so wikilinks
// and the resolver still attach to the same entity. The entity must exist
// (E_NOT_FOUND otherwise); an empty identifier or a non-snake_case kind is
// E_VALIDATION.
type AddIdentifier struct {
	appender Appender
	nav      entityGetter
}

// NewAddIdentifier constructs an AddIdentifier tool. Nav verifies the target
// entity exists before the event is appended; pass nil to skip that check.
func NewAddIdentifier(a Appender, nav entityGetter) *AddIdentifier {
	return &AddIdentifier{appender: a, nav: nav}
}

// Name implements mcp.Tool.
func (*AddIdentifier) Name() string { return "addIdentifier" }

// Description implements mcp.Tool.
func (*AddIdentifier) Description() string {
	return "Attach an identifier (an alias by default) to an EXISTING entity, appending an entityIdentifierAdded event so the resolver and vault wikilinks match the entity by that surface form. Use this after renaming an entity's displayName via updateEntity to retain the prior name: addIdentifier{entityUid, identifier:\"Old Name\", kind:\"alias\"}. createEntity only seeds identifiers at mint time; this adds them later. kind defaults to \"alias\"; pass email/username/phone/id or a snake_case domain type for other identifiers. Returns E_NOT_FOUND when the uid does not resolve and E_VALIDATION on an empty identifier or malformed kind."
}

// InputSchema implements mcp.Tool.
func (*AddIdentifier) InputSchema() json.RawMessage { return json.RawMessage(addIdentifierInputSchema) }

// OutputSchema implements mcp.Tool.
func (*AddIdentifier) OutputSchema() json.RawMessage {
	return json.RawMessage(addIdentifierOutputSchema)
}

type addIdentifierInput struct {
	EntityUID  string `json:"entityUid"`
	Identifier string `json:"identifier"`
	Kind       string `json:"kind,omitempty"`
}

type addIdentifierOutput struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Invoke implements mcp.Tool.
func (a *AddIdentifier) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in addIdentifierInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.EntityUID == "" {
		return nil, mcp.NewValidationError("entityUid is required", nil)
	}
	if strings.TrimSpace(in.Identifier) == "" {
		return nil, mcp.NewValidationError("identifier is required and must be non-empty", nil)
	}
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		kind = string(ontology.IdentifierKindAlias)
	}
	if !identifierKindRE.MatchString(kind) {
		return nil, mcp.NewValidationError(fmt.Sprintf("kind %q must be snake_case (%s)", kind, ontology.IdentifierKindPattern), nil)
	}

	// Confirm the entity exists before appending so a typo'd uid is a clean
	// E_NOT_FOUND rather than an orphaned identifier row. The entity's scope is
	// read here so the identifier event lands in the same scope envelope.
	var entityScope kernel.Scope
	if a.nav != nil {
		ent, err := a.nav.GetEntity(ctx, in.EntityUID)
		if err != nil {
			if errors.Is(err, navigate.ErrNotFound) {
				return nil, mcp.NewNotFoundError("entities/" + in.EntityUID + " not found")
			}
			return nil, mcp.NewInternalError(fmt.Errorf("addIdentifier: verify entity: %w", err))
		}
		entityScope = kernel.Scope(ent.Scope)
	}

	payload, err := json.Marshal(ontology.EntityIdentifierAddedEvent{
		EntityUID:  in.EntityUID,
		Identifier: in.Identifier,
		Kind:       ontology.IdentifierKind(kind),
		Source:     ontology.IdentifierSourceUser,
		AddedAt:    time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal entityIdentifierAdded payload: %w", err))
	}

	receipts, err := a.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindEntityIdentifierAdded),
		Source:     "mcp/addIdentifier",
		Confidence: 1.0,
		Scope:      entityScope,
		Payload:    payload,
	}})
	if err != nil {
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("append entityIdentifierAdded: %v", err), nil)
		}
		return nil, mapAppendError("append entityIdentifierAdded", err)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(fmt.Errorf("eventlog: append returned %d receipts, want 1", len(receipts)))
	}

	return marshalOutput(addIdentifierOutput{
		Name:   collectionEntities + "/" + in.EntityUID,
		Status: "added",
	})
}
