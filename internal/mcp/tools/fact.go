package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// stateKeyRE validates a Fact state_key against the ontology pattern before the
// write reaches the event log, turning a malformed key into a clean
// E_VALIDATION rather than an opaque schema rejection inside Append.
var stateKeyRE = regexp.MustCompile(ontology.StateKeyPattern())

// defaultFactSource is the provenance label stamped on a fact event when the
// caller omits source. The CLI passes "cli:fact" instead.
const defaultFactSource = "mcp:fact"

// factInputSchema is the JSON Schema for the tools/call arguments. Subject
// accepts the same forms as relate (entities/{uid}, a bare ULID, "self", or
// a human display name resolved case-insensitively).
const factInputSchema = `{
  "type": "object",
  "properties": {
    "subject":        {"type": "string", "minLength": 1, "description": "Subject entity the fact is about: entities/{uid}, a bare ULID, the literal \"self\", or a human display name (resolved case-insensitively, like maz get)."},
    "stateKey":       {"type": "string", "minLength": 1, "description": "Hierarchical slot address: lowercase, dot-separated, e.g. state.task.status, preference.coffee.milk. The first segment is the category (event|state|relationship|preference|plan|absence|timeless)."},
    "value":          {"type": "string", "minLength": 1, "description": "The scalar value the slot holds, e.g. \"pending\", \"Steven\"."},
    "eventStart":     {"type": "string", "format": "date-time", "description": "When the fact became true in the world (RFC 3339 UTC). Optional; defaults to now."},
    "sourcePriority": {"type": "number", "minimum": 0, "maximum": 1, "description": "Recall-lane priority, 0..1. Optional; defaults to 1.0."},
    "source":         {"type": "string", "description": "Provenance label for the appended event, e.g. \"cli:fact\". Optional; defaults to \"mcp:fact\"."},
    "scope":          {"type": "string", "enum": ["project", "personal", "team"], "description": "Scope to write into — pick by who should see it. Defaults to personal (private to the user, syncs to their devices). Use \"project\" for facts tied to the current repo/codebase (shared with anyone who has repo access). Use \"team\" for the shared team brain (requires admitted membership, else E_PERMISSION)."}
  },
  "required": ["subject", "stateKey", "value"],
  "additionalProperties": false
}`

const factOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^facts/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the asserted Fact; pass to get(facts/{uid})."},
    "status": {"type": "string", "enum": ["asserted"], "description": "Always \"asserted\" on success."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// Fact is the direct scalar-write tool: it records a (subject, stateKey, value)
// state slot — an entity's attribute, state, or preference — without routing
// through LLM extraction. It is the fact-shaped counterpart to relate (which
// records edges between two entities): when you already hold "Rosie's status is
// pending" or "her first name is Steven" as a slot, fact writes it in one call.
//
// A later write to the same (subject, stateKey) supersedes this value; the
// prior one is retained as history, so get(facts/{uid}) can walk the chain and
// get(entities/{uid}) returns only the currently-open slots.
type Fact struct {
	appender Appender
	entities entityResolver
	binder   selfBinder
	// nameNav is an optional Navigator used to resolve a human display name when
	// the subject is not a ULID, "self", or an entities/{uid} resource name. Nil
	// (the default) preserves prior behaviour: only UIDs and "self" are accepted.
	nameNav Navigator
}

// NewFact constructs a Fact tool bound to appender a and the entity resolver
// used to validate the subject. Entities may be nil to skip the existence check.
func NewFact(a Appender, entities entityResolver) *Fact {
	return &Fact{appender: a, entities: entities}
}

// SetSelfBinder installs the self->principal resolver so a fact asserted
// against the "self" sentinel persists the concrete principal UID. Late-bound
// (like the daemon's other resolvers) because the binding is provisioned at
// genesis, after the tool is constructed. Nil leaves "self" verbatim.
func (f *Fact) SetSelfBinder(b selfBinder) { f.binder = b }

// SetNameNavigator injects the navigator used to resolve a human display name
// when the subject is not a ULID, "self", or entities/{uid}. When nil (the
// default), only UIDs and "self" are accepted, preserving prior behaviour.
func (f *Fact) SetNameNavigator(nav Navigator) { f.nameNav = nav }

// Name implements mcp.Tool.
func (*Fact) Name() string { return ontology.KindFact }

// Description implements mcp.Tool.
func (*Fact) Description() string {
	return "Record a scalar fact — an attribute, state, or preference — about an entity as a (subject, stateKey, value) slot, bypassing LLM extraction. The fact-shaped counterpart to relate: relate records edges between entities, fact records a single entity's slots (e.g. state.task.status=pending). A later write to the same stateKey supersedes the prior value (kept as history). Read them back via get(entities/{uid}) for the current slots, or get(facts/{uid}) for one."
}

// InputSchema implements mcp.Tool.
func (*Fact) InputSchema() json.RawMessage { return json.RawMessage(factInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Fact) OutputSchema() json.RawMessage { return json.RawMessage(factOutputSchema) }

type factInput struct {
	Subject        string   `json:"subject"`
	StateKey       string   `json:"stateKey"`
	Value          string   `json:"value"`
	EventStart     *string  `json:"eventStart,omitempty"`
	SourcePriority *float64 `json:"sourcePriority,omitempty"`
	Source         string   `json:"source,omitempty"`
	Scope          string   `json:"scope,omitempty"`
}

type factOutput struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Invoke implements mcp.Tool. It builds an ontology.Fact, appends it as a
// KindFact event, and returns the new fact's resource name.
func (f *Fact) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in factInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	fact, scope, verr := f.buildFact(ctx, in)
	if verr != nil {
		return nil, verr
	}
	payload, err := json.Marshal(fact)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal fact: %w", err))
	}
	source := in.Source
	if source == "" {
		source = defaultFactSource
	}
	if _, err := f.appender.Append(ctx, []kernel.Event{{
		UID:        fact.UID,
		Kind:       kernel.EventKind(ontology.KindFact),
		Source:     source,
		Confidence: 1.0, // direct assertion; per-fact weight is sourcePriority, not event confidence
		Scope:      scope,
		Payload:    payload,
	}}); err != nil {
		return nil, mapAppendError("append fact", err)
	}

	body, err := json.Marshal(factOutput{Name: fact.Name, Status: "asserted"})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// resolveFactSubject normalises the fact's subject argument into a concrete
// entity UID. It accepts entities/{uid}, a bare ULID, "self", or (when nameNav
// is wired) a human display name resolved case-insensitively. Factored out of
// buildFact to keep that function's cyclomatic complexity under the gate.
func (f *Fact) resolveFactSubject(ctx context.Context, subject string) (string, error) {
	uid, err := relateEndpoint(subject, true)
	if err != nil {
		// relateEndpoint only errors for a bare token that is neither a valid ULID
		// nor "self" (it returns "self" without error), so a name lookup is the right
		// fallback. A genuine resource name (recognized collection prefix) surfaces
		// the original error; a slashed display name resolves by name.
		if f.nameNav == nil || looksLikeAIPResourceName(subject) {
			return "", err
		}
		return resolveEntityByName(ctx, f.nameNav, subject, "")
	}
	return uid, nil
}

// buildFact validates the input and assembles an ontology.Fact with a fresh
// ULID. The fact is self-sourced (SourceEventUID == its own UID): a direct
// assertion has no upstream episode.
func (f *Fact) buildFact(ctx context.Context, in factInput) (ontology.Fact, kernel.Scope, error) {
	scope, err := parseWriteScope(in.Scope)
	if err != nil {
		return ontology.Fact{}, "", err
	}
	// resolveFactSubject accepts entities/{uid}, a bare ULID, "self", or a
	// human display name when nameNav is wired (same resolution as maz get).
	subjectUID, err := f.resolveFactSubject(ctx, in.Subject)
	if err != nil {
		return ontology.Fact{}, "", err
	}
	// Rebind the "self" sentinel to the concrete principal UID before the
	// subject is validated or persisted, so the synced fact references the
	// shareable principal rather than the local-only literal.
	subjectUID = rebindSelf(ctx, f.binder, subjectUID)
	// A team-scope fact must not persist the unresolved "self" literal: it would
	// leak a device-local sentinel into a shared slot. Reject until the
	// self->principal binding is provisioned (which rebinds it above).
	if err := guardTeamSelf(scope, subjectUID, "subject"); err != nil {
		return ontology.Fact{}, "", err
	}
	if err := f.checkSubjectExists(ctx, subjectUID); err != nil {
		return ontology.Fact{}, "", err
	}
	// Validate the state_key here, not just via the schema-on-Append: the CLI and
	// in-process paths bypass the event-log validator, and a malformed key would
	// otherwise surface as an opaque E_INTERNAL from the ontology rejection.
	if !stateKeyRE.MatchString(in.StateKey) {
		return ontology.Fact{}, "", mcp.NewValidationError(
			fmt.Sprintf("stateKey %q is not a valid slot address: expected lowercase dot-separated segments [a-z][a-z0-9_]*, e.g. state.task.status", in.StateKey), nil)
	}
	if in.Value == "" {
		return ontology.Fact{}, "", mcp.NewValidationError("value must not be empty", nil)
	}
	// newRelationULID is a generic fresh-ULID helper (shared with relate); the
	// resource prefix below is what makes this a facts/{uid} name.
	uid, err := newRelationULID()
	if err != nil {
		return ontology.Fact{}, "", mcp.NewInternalError(fmt.Errorf("generate fact uid: %w", err))
	}
	fact := ontology.Fact{
		Name:           collectionFacts + "/" + uid,
		UID:            uid,
		SubjectUID:     subjectUID,
		StateKey:       in.StateKey,
		Value:          in.Value,
		CreateTime:     time.Now().UTC(),
		SourceEventUID: uid,
	}
	if fact.EventStart, err = parseRelateTime(in.EventStart, "eventStart"); err != nil {
		return ontology.Fact{}, "", err
	}
	if in.SourcePriority != nil {
		if *in.SourcePriority < 0 || *in.SourcePriority > 1 {
			return ontology.Fact{}, "", mcp.NewValidationError(
				fmt.Sprintf("sourcePriority must be between 0 and 1, got %v", *in.SourcePriority), nil)
		}
		fact.SourcePriority = float32(*in.SourcePriority)
	}
	return fact, scope, nil
}

// checkSubjectExists mirrors Relate.checkEndpointExists: the "self" sentinel and
// a nil resolver pass; an unknown subject is E_NOT_FOUND so a fact is never
// written against a dangling uid that later hydrates a blank node.
func (f *Fact) checkSubjectExists(ctx context.Context, uid string) error {
	if uid == selfEntityUID || f.entities == nil {
		return nil
	}
	if _, err := f.entities.GetEntity(ctx, uid); err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return mcp.NewNotFoundError(
				fmt.Sprintf("subject entity %q does not exist; create it first (remember) or pass a known entities/{uid}", uid))
		}
		return mcp.NewInternalError(fmt.Errorf("verify subject entity %q: %w", uid, err))
	}
	return nil
}
