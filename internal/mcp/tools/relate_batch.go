package tools

// relate_batch.go implements the relateBatch MCP tool: assert N (subject,
// predicate, object) relations in ONE atomic event-log append, so a multi-edge
// graph seed lands as a single projection commit.
//
// Validation-before-write contract: ALL relations in the batch are resolved and
// validated before any event is appended. A single invalid endpoint or a
// misconfigured validity window aborts the entire batch with E_VALIDATION so
// the caller can fix and retry without partial state.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

const relateBatchInputSchema = `{
  "type": "object",
  "properties": {
    "relations": {
      "type": "array",
      "minItems": 1,
      "description": "Array of relation assertions to append atomically.",
      "items": {
        "type": "object",
        "properties": {
          "subject":    {"type": "string", "minLength": 1, "description": "Subject entity: entities/{uid}, a bare ULID, the literal \"self\", or a human display name."},
          "predicate":  {"type": "string", "minLength": 1, "description": "Relationship type, e.g. knows, works_at. See introspect for registered predicates."},
          "object":     {"type": "string", "minLength": 1, "description": "Object entity: entities/{uid}, a bare ULID, the literal \"self\", or a human display name."},
          "confidence": {"type": "number", "minimum": 0, "maximum": 1, "description": "Writer confidence in the edge. Defaults to 1.0."},
          "validFrom":  {"type": "string", "format": "date-time", "description": "Inclusive lower bound of the relation's valid-time (RFC 3339 UTC). Optional."},
          "validUntil": {"type": "string", "format": "date-time", "description": "Exclusive upper bound of the relation's valid-time (RFC 3339 UTC). Optional."},
          "attributes": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Qualifying key/value pairs. Optional."},
          "provenance": {"type": "string", "description": "Free-text rationale for the edge. Optional."},
          "scope":      {"type": "string", "enum": ["project", "personal", "team"], "description": "Scope to write into — pick by who should see it (applies to every edge in the batch). Defaults to personal (private to the user, syncs to their devices). Use \"project\" for edges tied to the current repo/codebase (shared with anyone who has repo access). Use \"team\" for the shared team brain (requires admitted membership, else E_PERMISSION)."}
        },
        "required": ["subject", "predicate", "object"],
        "additionalProperties": false
      }
    },
    "source": {"type": "string", "description": "Provenance label for all appended events, e.g. \"mcp:relateBatch\". Optional; defaults to \"mcp:relateBatch\"."}
  },
  "required": ["relations"],
  "additionalProperties": false
}`

const relateBatchOutputSchema = `{
  "type": "object",
  "properties": {
    "names":   {"type": "array", "items": {"type": "string", "pattern": "^relations/[0-9A-HJKMNP-TV-Z]{26}$"}, "description": "AIP resource names of the asserted Relations, in input order."},
    "count":   {"type": "integer", "minimum": 0, "description": "Number of relations appended."},
    "status":  {"type": "string", "enum": ["asserted"], "description": "Always \"asserted\" on success."}
  },
  "required": ["names", "count", "status"],
  "additionalProperties": false
}`

// RelateBatch asserts a list of (subject, predicate, object) relations in a
// single event-log Append call, making the entire set atomic from the
// projection's point of view. All relations are validated before any event is
// written; a single invalid edge aborts the batch.
type RelateBatch struct {
	// inner holds the resolver/binder/nameNav the single-edge Relate tool
	// already owns; RelateBatch delegates all endpoint resolution to it.
	inner *Relate
}

// NewRelateBatch constructs a RelateBatch tool that reuses the resolver stack
// of the given Relate tool. Pass the same Relate instance registered in wiring
// so both tools share binder and nameNav state.
func NewRelateBatch(r *Relate) *RelateBatch { return &RelateBatch{inner: r} }

// Name implements mcp.Tool.
func (*RelateBatch) Name() string { return "relateBatch" }

// Description implements mcp.Tool.
func (*RelateBatch) Description() string {
	return "Assert multiple (subject, predicate, object) relations in ONE atomic event-log append. All edges are validated before any write: a single invalid endpoint aborts the entire batch. Use this to seed a graph in one call instead of N sequential relate() calls. Returns the new relation resource names in input order."
}

// InputSchema implements mcp.Tool.
func (*RelateBatch) InputSchema() json.RawMessage { return json.RawMessage(relateBatchInputSchema) }

// OutputSchema implements mcp.Tool.
func (*RelateBatch) OutputSchema() json.RawMessage { return json.RawMessage(relateBatchOutputSchema) }

type relateBatchInput struct {
	Relations []relateInput `json:"relations"`
	Source    string        `json:"source,omitempty"`
}

type relateBatchOutput struct {
	Names  []string `json:"names"`
	Count  int      `json:"count"`
	Status string   `json:"status"`
}

// Invoke implements mcp.Tool. It validates ALL relations, then appends them in
// one batch — the validate-all-before-append contract prevents partial state.
func (rb *RelateBatch) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in relateBatchInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if len(in.Relations) == 0 {
		return nil, mcp.NewValidationError("relations array must contain at least one entry", nil)
	}

	source := in.Source
	if source == "" {
		source = "mcp:relateBatch"
	}

	// Phase 1: validate ALL relations before touching the event log.
	type built struct {
		rel   ontology.Relation
		scope kernel.Scope
	}
	builtRels := make([]built, 0, len(in.Relations))
	for i, ri := range in.Relations {
		rel, scope, err := buildRelationWithResolver(ctx, rb.inner, ri)
		if err != nil {
			return nil, wrapBatchValidation(i, err)
		}
		builtRels = append(builtRels, built{rel: rel, scope: scope})
	}

	// Phase 2: marshal + assemble the event batch.
	events := make([]kernel.Event, 0, len(builtRels))
	for _, b := range builtRels {
		payload, err := json.Marshal(b.rel)
		if err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("marshal relation %s: %w", b.rel.UID, err))
		}
		events = append(events, kernel.Event{
			Kind:       kernel.EventKind(ontology.KindRelation),
			Source:     source,
			Confidence: b.rel.Confidence,
			Scope:      b.scope,
			Payload:    payload,
		})
	}

	// Phase 3: single atomic append.
	if _, err := rb.inner.appender.Append(ctx, events); err != nil {
		return nil, mapAppendError("append relation batch", err)
	}

	names := make([]string, len(builtRels))
	for i, b := range builtRels {
		names[i] = b.rel.Name
	}

	body, err := json.Marshal(relateBatchOutput{Names: names, Count: len(names), Status: "asserted"})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// wrapBatchValidation prefixes a per-relation validation error with its index
// so the caller can identify which entry caused the failure.
func wrapBatchValidation(idx int, err error) error {
	var te *mcp.ToolError
	if errors.As(err, &te) {
		return &mcp.ToolError{
			Code:    te.Code,
			Message: fmt.Sprintf("relations[%d]: %s", idx, te.Message),
			Cause:   te.Cause,
		}
	}
	return mcp.NewValidationError(fmt.Sprintf("relations[%d]: %v", idx, err), nil)
}
