package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// promoteRelationInputSchema is the JSON Schema for the tools/call arguments.
// The relation is named by its AIP resource name — exactly the form recall
// hits, get, relations, and neighbors hand back — so an agent pipes an anchor
// straight in without string surgery.
const promoteRelationInputSchema = `{
  "type": "object",
  "properties": {
    "name":  {"type": "string", "pattern": "^relations/[0-9A-HJKMNP-TV-Z]{26}$", "description": "Resource name of the Relation to promote: relations/{uid}, as returned by recall/get/relations/neighbors."},
    "scope": {"type": "string", "enum": ["project", "personal", "team"], "description": "Scope the promotion event is written into. Defaults to personal. Use the scope the relation lives in; promoting a team-scope edge requires admitted team membership, else E_PERMISSION."}
  },
  "required": ["name"],
  "additionalProperties": false
}`

const promoteRelationOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^relations/[0-9A-HJKMNP-TV-Z]{26}$", "description": "Resource name of the promoted Relation."},
    "status": {"type": "string", "enum": ["asserted"], "description": "Always \"asserted\" on success — the relation now carries the authoritative trust posture."},
    "promoted": {"type": "boolean", "description": "true when this call flipped a proposed edge to asserted; false when the edge was already asserted (idempotent no-op, no event appended)."}
  },
  "required": ["name", "status", "promoted"],
  "additionalProperties": false
}`

// PromoteRelation lifts the trust quarantine on a connector-proposed relation:
// it flips a status=proposed edge to status=asserted by appending a
// relationPromoted event. A connector-ingested fact (#211, #212) lands
// status=proposed — it ranks on its own relevance (no confidence penalty) but
// is excluded from supersession and predicate minting until a human verifies
// it. Promotion is that verification step: once asserted, the edge behaves like
// any user-authored relation and the promoter becomes its signed asserter.
//
// The append is idempotent end to end: when a relation reader is wired the tool
// short-circuits an already-asserted edge without writing an event, and the
// dispatcher's projection is itself guarded on status='proposed', so a replay
// is harmless either way.
type PromoteRelation struct {
	appender  Appender
	relations relationReader
}

// relationReader is the read-side dependency PromoteRelation uses to confirm a
// relation exists and read its current trust status before appending. Narrowed
// from Navigator so a mistyped uid surfaces as E_NOT_FOUND and an
// already-asserted edge short-circuits without polluting the log. A nil reader
// disables the pre-check (the dispatcher projection is idempotent regardless).
type relationReader interface {
	GetRelation(ctx context.Context, uid string) (navigate.Relation, error)
}

// NewPromoteRelation constructs a PromoteRelation tool bound to appender a and
// the relation reader used to pre-validate the target. Relations may be nil to
// skip the existence/status pre-check.
func NewPromoteRelation(a Appender, relations relationReader) *PromoteRelation {
	return &PromoteRelation{appender: a, relations: relations}
}

// Name implements mcp.Tool.
func (*PromoteRelation) Name() string { return "promoteRelation" }

// Description implements mcp.Tool.
func (*PromoteRelation) Description() string {
	return "Promote a proposed relation to asserted: verify an untrusted connector-ingested fact and lift its trust quarantine. A fact extracted from an external source (Granola transcript, Claude-chats export) lands status=proposed — it surfaces in recall but is excluded from supersession until confirmed. Promoting it asserts it, so it then behaves like any user-authored edge. Idempotent: promoting an already-asserted edge is a no-op."
}

// InputSchema implements mcp.Tool.
func (*PromoteRelation) InputSchema() json.RawMessage {
	return json.RawMessage(promoteRelationInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*PromoteRelation) OutputSchema() json.RawMessage {
	return json.RawMessage(promoteRelationOutputSchema)
}

type promoteRelationInput struct {
	Name  string `json:"name"`
	Scope string `json:"scope,omitempty"`
}

type promoteRelationOutput struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Promoted bool   `json:"promoted"`
}

// Invoke implements mcp.Tool. It resolves the relation uid, optionally
// confirms the relation exists and is still proposed, appends a
// relationPromoted event when a flip is needed, and returns the relation's
// resource name with whether a promotion actually occurred.
func (p *PromoteRelation) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in promoteRelationInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	uid, err := relationUIDFromName(in.Name)
	if err != nil {
		return nil, err
	}
	scope, err := parseWriteScope(in.Scope)
	if err != nil {
		return nil, err
	}

	// Pre-check (best-effort, only when a reader is wired): a missing relation
	// is a clear E_NOT_FOUND rather than a silent no-op event, and an
	// already-asserted edge short-circuits without polluting the log.
	if p.relations != nil {
		// Scope-align the existence check with the write target so a team-scope
		// promotion reads the team scope (mirrors get/relations). An unset scope
		// keeps the reader's default ({project, personal}) — where the
		// connector-proposed facts this tool exists to verify live. The
		// type-assert falls back to the unscoped reader for test fakes.
		reader := p.relations
		if scope != "" {
			if s, ok := reader.(interface {
				WithScopes(kernel.ScopeSet) *navigate.Navigator
			}); ok {
				reader = s.WithScopes(kernel.NewScopeSet(scope))
			}
		}
		rel, gerr := reader.GetRelation(ctx, uid)
		switch {
		case errors.Is(gerr, navigate.ErrNotFound):
			return nil, mcp.NewNotFoundError(
				fmt.Sprintf("relation %q does not exist; pass a relations/{uid} from a recall hit, get, or relations", in.Name))
		case gerr != nil:
			return nil, mcp.NewInternalError(fmt.Errorf("read relation %q: %w", uid, gerr))
		case rel.Status == ontology.RelationStatusAsserted:
			return marshalPromoteOutput(uid, false)
		}
	}

	payload, err := json.Marshal(ontology.RelationPromotedEvent{
		RelationUID: ontology.ULID(uid),
		PromotedAt:  time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal relationPromoted payload: %w", err))
	}
	receipts, err := p.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindRelationPromoted),
		Source:     "mcp:promoteRelation",
		Confidence: 1.0,
		Scope:      scope,
		Payload:    payload,
	}})
	if err != nil {
		// A schema rejection (ErrInvalidEvent) is the caller's input mistake →
		// E_VALIDATION; everything else routes through mapAppendError so a
		// team-scope promotion by a non-member surfaces as E_PERMISSION, not a
		// misleading E_INTERNAL (matching reference/createEntity/recordSourceRef).
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("append relationPromoted: %v", err), nil)
		}
		return nil, mapAppendError("append relationPromoted", err)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}
	return marshalPromoteOutput(uid, true)
}

// marshalPromoteOutput renders the success body for a promotion of uid.
// Promoted distinguishes a real flip from an idempotent no-op.
func marshalPromoteOutput(uid string, promoted bool) (json.RawMessage, error) {
	body, err := json.Marshal(promoteRelationOutput{
		Name:     collectionRelations + "/" + uid,
		Status:   ontology.RelationStatusAsserted,
		Promoted: promoted,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// relationUIDFromName parses a relations/{uid} resource name (the form
// recall/get/relations/neighbors return) into a validated bare ULID. A bare
// uid is also accepted for CLI/direct-dispatch ergonomics. A name carrying a
// non-relation collection, or a uid that is not a 26-char ULID, is rejected as
// E_VALIDATION rather than left to surface as a no-op promotion downstream.
func relationUIDFromName(name string) (string, error) {
	if name == "" {
		return "", mcp.NewValidationError("name is required (relations/{uid})", nil)
	}
	uid := name
	if strings.Contains(name, "/") {
		collection, parsed, err := parseResourceName(name)
		if err != nil {
			return "", err
		}
		if collection != collectionRelations {
			return "", mcp.NewValidationError(
				fmt.Sprintf("expected a relations/{uid} name, got %q", name), nil)
		}
		uid = parsed
	}
	if _, err := ulid.Parse(uid); err != nil {
		return "", mcp.NewValidationError(
			fmt.Sprintf("relation reference %q is not a valid relations/{uid} name or 26-char ULID", name), nil)
	}
	return uid, nil
}
