package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/ontology/dedup"
	"github.com/mo-cmyk/mazarbul/internal/warncode"
)

const defineSchemaInputSchema = `{
  "type": "object",
  "properties": {
    "schemaKind": {
      "type": "string",
      "pattern": "^[a-z][a-zA-Z0-9_]*(:[A-Za-z0-9_]+)*$",
      "description": "Lookup key for the schema, e.g. entityKind:vehicle or myCustomEvent. Bare camelCase or colon-namespaced. No spaces. Core kinds (entityKindRegistered, schemaRegistered, predicateRegistered, entity, episode, etc.) cannot be overwritten."
    },
    "version": {
      "type": "string",
      "pattern": "^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$",
      "description": "Semver version, e.g. v0.1.0. Higher versions replace lower ones; same version with a different definition is a conflict."
    },
    "definition": {
      "type": "object",
      "description": "JSON Schema document (Draft 2020-12) to register. Must be a JSON object."
    },
    "preview": {
      "type": "boolean",
      "description": "When true, compute and return a plan (supersede or register) WITHOUT appending any event. Use this to inspect impact before committing. Default false."
    }
  },
  "required": ["schemaKind", "version", "definition"],
  "additionalProperties": false
}`

const defineSchemaOutputSchema = `{
  "type": "object",
  "properties": {
    "schemaKind": {
      "type": "string",
      "description": "The registered schema kind identifier."
    },
    "version": {
      "type": "string",
      "description": "The registered schema version."
    },
    "status": {
      "type": "string",
      "enum": ["registered", "preview"],
      "description": "registered = schema written to the event log; preview = plan computed without writing any event (preview=true was set)."
    },
    "plan": {
      "type": "object",
      "description": "Present only when preview=true. Describes what the call would do.",
      "properties": {
        "action": {
          "type": "string",
          "enum": ["register", "supersede"],
          "description": "register = first-time registration; supersede = an existing version would be replaced."
        },
        "supersedesFrom": {"type": "string", "description": "Currently registered version string. Set when action=supersede."},
        "orphanCount": {"type": "integer", "minimum": 0, "description": "Number of fields removed from the existing definition."}
      },
      "required": ["action"]
    },
    "warnings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "code":    {"type": "string", "description": "Stable machine-readable warning code, e.g. MAZ-SCHEMA-001."},
          "message": {"type": "string", "description": "Human-readable explanation."}
        },
        "required": ["code", "message"]
      },
      "description": "Non-fatal advisory warnings. MAZ-SCHEMA-001 = supersede; MAZ-SCHEMA-002 = orphaned fields."
    }
  },
  "required": ["schemaKind", "version", "status"],
  "additionalProperties": false
}`

// schemaKindPattern validates the schemaKind identifier grammar.
var schemaKindPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]*(:[A-Za-z0-9_]+)*$`)

// isCoreSchemaKind reports whether the given schemaKind is a kernel-owned
// schema that the MCP authoring surface must not overwrite.
func isCoreSchemaKind(kind string) bool {
	switch kind {
	case ontology.KindEntityKindRegistered, ontology.KindSchemaRegistered, ontology.KindPredicateRegistered,
		ontology.KindEntity, ontology.KindEpisode, ontology.KindMention, ontology.KindRelation,
		ontology.KindRelationRetracted, ontology.KindEpisodeForgotten, ontology.KindRelationForgotten,
		ontology.KindFactRetracted, ontology.KindFactForgotten,
		ontology.KindEntityIdentifierAdded:
		return true
	}
	return false
}

// schemaLister is the optional read-side dependency for preview: it lists all
// currently registered schemas so PlanSchema can detect supersede+orphan. When
// nil, preview always reports action="register" (safe degradation).
//
// The interface returns navigate.RegisteredSchema rather than dedup.ExistingSchema
// to avoid a circular import: dedup already imports navigate, so navigate cannot
// import dedup. The invokePreview method converts between the two types locally.
type schemaLister interface {
	RegisteredSchemas(ctx context.Context) ([]navigate.RegisteredSchema, error)
}

// DefineSchema registers a JSON Schema document in the dynamic-ontology schema
// registry. Use this to attach per-kind property validation schemas or to
// register schemas for custom event types before those events are appended.
type DefineSchema struct {
	appender Appender
	schemas  schemaLister // optional; enables schema preview
}

// NewDefineSchema constructs a DefineSchema tool without schema-preview support.
func NewDefineSchema(a Appender) *DefineSchema {
	return &DefineSchema{appender: a}
}

// NewDefineSchemaWithLister constructs a DefineSchema tool with an optional
// schemaLister for preview support. When lister is nil, preview degrades to
// action="register" (no existing schemas consulted).
func NewDefineSchemaWithLister(a Appender, lister schemaLister) *DefineSchema {
	return &DefineSchema{appender: a, schemas: lister}
}

// Name implements mcp.Tool.
func (*DefineSchema) Name() string { return "defineSchema" }

// Description implements mcp.Tool.
func (*DefineSchema) Description() string {
	return "Register a JSON Schema document in the dynamic-ontology schema registry. Use this to attach validation schemas for custom entity-kind properties (schemaKind=entityKind:vehicle) or to define schemas for new event types. Inputs: schemaKind (bare camelCase or colon-namespaced, e.g. entityKind:deal), version (semver vX.Y.Z), definition (JSON Schema object). Core schema kinds (entity, episode, etc.) are protected and return E_VALIDATION. The schema is immediately usable for property validation via ValidateEntityProperties. Returns E_VALIDATION if the definition is not a JSON object or if schemaKind is a protected core kind."
}

// InputSchema implements mcp.Tool.
func (*DefineSchema) InputSchema() json.RawMessage {
	return json.RawMessage(defineSchemaInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*DefineSchema) OutputSchema() json.RawMessage {
	return json.RawMessage(defineSchemaOutputSchema)
}

type defineSchemaInput struct {
	SchemaKind string          `json:"schemaKind"`
	Version    string          `json:"version"`
	Definition json.RawMessage `json:"definition"`
	// Preview, when true, computes the plan without appending any event.
	Preview bool `json:"preview,omitempty"`
}

// defineSchemaPlan carries the preview plan (only when preview=true).
type defineSchemaPlan struct {
	// Action is "register" or "supersede".
	Action string `json:"action"`
	// SupersedesFrom is the currently registered version when Action=="supersede".
	SupersedesFrom string `json:"supersedesFrom,omitempty"`
	// OrphanCount is the number of fields removed from the existing definition.
	OrphanCount int `json:"orphanCount,omitempty"`
}

type defineSchemaOutput struct {
	SchemaKind string `json:"schemaKind"`
	Version    string `json:"version"`
	// Status is "registered" or "preview".
	Status string `json:"status"`
	// Plan carries the preview plan when Status is "preview". Absent otherwise.
	Plan *defineSchemaPlan `json:"plan,omitempty"`
	// Warnings is a list of non-fatal advisory warnings (MAZ-SCHEMA-NNN codes).
	// Absent when empty.
	Warnings []warncode.Warning `json:"warnings,omitempty"`
}

// Invoke implements mcp.Tool.
func (d *DefineSchema) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in defineSchemaInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}

	if !schemaKindPattern.MatchString(in.SchemaKind) {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("schemaKind %q does not match pattern ^[a-z][a-zA-Z0-9_]*(:[A-Za-z0-9_]+)*$", in.SchemaKind), nil)
	}

	if isCoreSchemaKind(in.SchemaKind) {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("schemaKind %q is a core schema and cannot be overwritten", in.SchemaKind), nil)
	}

	// definition must be a JSON object.
	var defDoc map[string]any
	if err := json.Unmarshal(in.Definition, &defDoc); err != nil {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("definition must be a JSON Schema object: %v", err), nil)
	}
	// It must also compile: an object that is not a valid JSON Schema (e.g.
	// {"type":12}) would be "registered" here but skipped by
	// applySchemaRegistered, so the schema would never be usable. Reject it as
	// E_VALIDATION up front instead of returning a successful no-op.
	if err := kernel.CompileSchemaDocument(in.Definition); err != nil {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("definition is not a compilable JSON Schema: %v", err), nil)
	}

	// Preview path: compute the plan without touching the event log.
	if in.Preview {
		return d.invokePreview(ctx, &in)
	}

	payload := ontology.SchemaRegisteredEvent{
		SchemaKind: in.SchemaKind,
		Version:    in.Version,
		Definition: in.Definition,
		Provenance: "user",
		AddedBy:    ontology.EntityKindAddedByUser,
		AddedAt:    time.Now().UTC(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal schemaRegistered: %w", err))
	}
	if _, err := d.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindSchemaRegistered),
		Source:     "mcp/defineSchema",
		Confidence: 1.0,
		Payload:    raw,
	}}); err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("append schemaRegistered: %w", err))
	}

	out := defineSchemaOutput{SchemaKind: in.SchemaKind, Version: in.Version, Status: "registered"}
	return marshalOutput(out)
}

// invokePreview computes the plan for a preview=true call without appending
// any event. When no schemaLister is wired, action is always "register" (safe
// degradation: no existing schemas to compare against).
func (d *DefineSchema) invokePreview(ctx context.Context, in *defineSchemaInput) (json.RawMessage, error) {
	var existing []dedup.ExistingSchema
	if d.schemas != nil {
		schemas, err := d.schemas.RegisteredSchemas(ctx)
		if err == nil {
			// Convert navigate.RegisteredSchema → dedup.ExistingSchema. The two
			// types carry the same fields; the indirection avoids a circular import
			// (dedup imports navigate, so navigate cannot import dedup).
			existing = make([]dedup.ExistingSchema, len(schemas))
			for i, s := range schemas {
				existing[i] = dedup.ExistingSchema{
					SchemaKind: s.SchemaKind,
					Version:    s.Version,
					Definition: s.Definition,
				}
			}
		}
		// On lister error degrade to empty (→ always register).
	}

	pr := dedup.PlanSchema(dedup.SchemaPreviewRequest{
		SchemaKind: in.SchemaKind,
		Version:    in.Version,
		Definition: in.Definition,
	}, existing)

	plan := &defineSchemaPlan{
		Action:         pr.Action,
		SupersedesFrom: pr.SupersedesFrom,
		OrphanCount:    pr.OrphanCount,
	}
	out := defineSchemaOutput{
		SchemaKind: in.SchemaKind,
		Version:    in.Version,
		Status:     "preview",
		Plan:       plan,
		Warnings:   pr.Warnings,
	}
	return marshalOutput(out)
}
