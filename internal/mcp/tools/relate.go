package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// relateDefaultConfidence is applied when the caller omits confidence: a direct
// structured assertion is treated as user-confirmed.
const relateDefaultConfidence = 1.0

// selfEntityUID is the canonical UID of the seeded self entity (migration 0012;
// resolver.SelfEntityUID). Mirrored as a local constant to avoid importing the
// resolver package into the MCP tool layer. The Relation schema accepts it as a
// subject in place of a ULID.
const selfEntityUID = "self"

// relateInputSchema is the JSON Schema for the tools/call arguments. Subject
// accepts the "self" sentinel; both endpoints also accept the entities/{uid}
// resource-name form so an agent can paste a recall anchor directly, or a
// human display name which is resolved case-insensitively.
const relateInputSchema = `{
  "type": "object",
  "properties": {
    "subject":    {"type": "string", "minLength": 1, "description": "Subject entity: entities/{uid}, a bare ULID, the literal \"self\", or a human display name (resolved case-insensitively, like maz get)."},
    "predicate":  {"type": "string", "minLength": 1, "description": "Relationship type, e.g. works_at, reports_to, knows. See introspect for registered predicates."},
    "object":     {"type": "string", "minLength": 1, "description": "Object entity: entities/{uid}, a bare ULID, the literal \"self\", or a human display name (resolved case-insensitively, like maz get)."},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1, "description": "Writer confidence in the edge. Defaults to 1.0."},
    "validFrom":  {"type": "string", "format": "date-time", "description": "Inclusive lower bound of the relation's valid-time (RFC 3339 UTC). Optional."},
    "validUntil": {"type": "string", "format": "date-time", "description": "Exclusive upper bound of the relation's valid-time (RFC 3339 UTC). Optional."},
    "attributes": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Qualifying key/value pairs, e.g. {\"role\":\"CEO\"}. Optional."},
    "provenance": {"type": "string", "description": "Free-text rationale for the edge. Optional."},
    "source":     {"type": "string", "description": "Provenance label for the appended event, e.g. \"cli:relate\". Optional; defaults to \"mcp:relate\"."},
    "scope":      {"type": "string", "enum": ["project", "personal", "team", "draft"], "description": "Scope to write into — pick by who should see it. Defaults to personal (private to the user, syncs to their devices). Use \"project\" for edges tied to the current repo/codebase (shared with anyone who has repo access). Use \"team\" for the shared team brain (requires admitted membership, else E_PERMISSION). \"draft\" is private staging — invisible to recall until promoted via the CLI (maz scope draft promote); there is no listed MCP tool for draft promotion."}
  },
  "required": ["subject", "predicate", "object"],
  "additionalProperties": false
}`

const relateOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^relations/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the asserted Relation; pass to get(relations/{uid})."},
    "status": {"type": "string", "enum": ["asserted"], "description": "Always \"asserted\" on success."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// Relate is the direct structured-write tool: it asserts a (subject, predicate,
// object) relation without routing through LLM extraction. It appends a
// KindRelation event the dispatcher applies straight to the relations table —
// the structured counterpart to remember()'s natural-language write. Use it to
// record a fact an agent already holds as a triple.
//
// Like `maz relate`, this bypasses the resolver's supersession check: it is a
// raw assertion, not a managed update. Conflicting functional facts coexist
// until a forget or a resolver-routed write closes the prior one.
type Relate struct {
	appender Appender
	entities entityResolver
	binder   selfBinder
	// nameNav is an optional Navigator used to resolve a human display name when
	// the subject or object is not a ULID, "self", or an entities/{uid} resource
	// name. Nil (the default) preserves pre-name-resolution behaviour: only UIDs
	// and the "self" sentinel are accepted.
	nameNav Navigator
}

// entityResolver is the read-side dependency Relate uses to confirm a relation's
// subject and object exist before the edge is written. Narrowed from Navigator
// so a mistyped uid surfaces as E_NOT_FOUND here rather than as a blank-named
// node during later navigation. A nil resolver disables the check (e.g. a
// minimal boot where the navigator is unavailable).
type entityResolver interface {
	GetEntity(ctx context.Context, uid string) (navigate.Entity, error)
}

// NewRelate constructs a Relate tool bound to appender a and the entity resolver
// used to validate endpoints. Entities may be nil to skip existence checks.
func NewRelate(a Appender, entities entityResolver) *Relate {
	return &Relate{appender: a, entities: entities}
}

// SetSelfBinder installs the self->principal resolver so a relation whose
// subject or object is the "self" sentinel persists the concrete principal
// UID. Late-bound (the binding is provisioned at genesis, after the tool is
// constructed); nil leaves "self" verbatim.
func (r *Relate) SetSelfBinder(b selfBinder) { r.binder = b }

// SetNameNavigator injects the navigator used to resolve a human display name
// when subject or object is not a ULID, "self", or entities/{uid}. When nil
// (the default), only UIDs and "self" are accepted, preserving prior behaviour.
func (r *Relate) SetNameNavigator(nav Navigator) { r.nameNav = nav }

// Name implements mcp.Tool.
func (*Relate) Name() string { return "relate" }

// Description implements mcp.Tool.
func (*Relate) Description() string {
	return "Assert a structured (subject, predicate, object) relation directly. You are the extractor: when you see a relationship, record it here rather than relying on server-side extraction (off unless a chat model is configured). Pass source for provenance. Returns the new relation's resource name."
}

// InputSchema implements mcp.Tool.
func (*Relate) InputSchema() json.RawMessage { return json.RawMessage(relateInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Relate) OutputSchema() json.RawMessage { return json.RawMessage(relateOutputSchema) }

type relateInput struct {
	Subject    string            `json:"subject"`
	Predicate  string            `json:"predicate"`
	Object     string            `json:"object"`
	Confidence *float64          `json:"confidence,omitempty"`
	ValidFrom  *string           `json:"validFrom,omitempty"`
	ValidUntil *string           `json:"validUntil,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Provenance string            `json:"provenance,omitempty"`
	Source     string            `json:"source,omitempty"`
	Scope      string            `json:"scope,omitempty"`
}

type relateOutput struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Invoke implements mcp.Tool. It builds an ontology.Relation, appends it as a
// KindRelation event, and returns the new relation's resource name.
func (r *Relate) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in relateInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	rel, scope, verr := r.buildRelation(ctx, in)
	if verr != nil {
		return nil, verr
	}
	payload, err := json.Marshal(rel)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal relation: %w", err))
	}
	// Reuse the confidence buildRelation already resolved (default 1.0 when the
	// caller omits it) so the event-log row and the relation projection can
	// never disagree on the value.
	source := in.Source
	if source == "" {
		source = "mcp:relate"
	}
	receipts, err := r.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindRelation),
		Source:     source,
		Confidence: rel.Confidence,
		Scope:      scope,
		Payload:    payload,
	}})
	if err != nil {
		return nil, mapAppendError("append relation", err)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	body, err := json.Marshal(relateOutput{Name: rel.Name, Status: "asserted"})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// buildRelation validates the input and assembles an ontology.Relation with a
// fresh ULID. The relation is self-sourced (SourceEventUID == its own UID):
// a direct assertion has no upstream episode. Returns an E_VALIDATION error on
// a bad endpoint or unparseable validity bound.
//
// It delegates to buildRelationWithResolver so the RelateBatch tool can reuse
// the same logic without going through a Relate receiver.
func (r *Relate) buildRelation(ctx context.Context, in relateInput) (ontology.Relation, kernel.Scope, error) {
	return buildRelationWithResolver(ctx, r, in)
}

// buildRelationWithResolver is the package-level helper that implements
// buildRelation without requiring a Relate receiver. RelateBatch uses this to
// validate and build each edge before the single-batch append.
func buildRelationWithResolver(ctx context.Context, r *Relate, in relateInput) (ontology.Relation, kernel.Scope, error) {
	scope, err := parseWriteScope(in.Scope)
	if err != nil {
		return ontology.Relation{}, "", err
	}
	// Validate the predicate before touching storage: an empty predicate is
	// caught by the JSON schema (minLength:1) for MCP calls, but direct CLI
	// dispatch bypasses it, so check both conditions here.
	if in.Predicate == "" {
		return ontology.Relation{}, "", mcp.NewValidationError("predicate is required", nil)
	}
	if !predicateNamePattern.MatchString(in.Predicate) {
		return ontology.Relation{}, "", mcp.NewValidationError(
			fmt.Sprintf("predicate %q must be snake_case (lowercase letters, digits, underscores; start with a letter), e.g. works_at", in.Predicate), nil)
	}
	subjectUID, err := r.resolveEndpoint(ctx, in.Subject, "subject", scope)
	if err != nil {
		return ontology.Relation{}, "", err
	}
	objectUID, err := r.resolveEndpoint(ctx, in.Object, "object", scope)
	if err != nil {
		return ontology.Relation{}, "", err
	}
	uid, err := newRelationULID()
	if err != nil {
		return ontology.Relation{}, "", mcp.NewInternalError(fmt.Errorf("generate relation uid: %w", err))
	}
	// Range-check confidence here, not just in the JSON schema: direct/CLI
	// dispatch bypasses the schema's minimum/maximum, and an out-of-range value
	// persists into the relation and skews later confidence filtering.
	confidence := relateDefaultConfidence
	if in.Confidence != nil {
		if *in.Confidence < 0 || *in.Confidence > 1 {
			return ontology.Relation{}, "", mcp.NewValidationError(
				fmt.Sprintf("confidence must be between 0 and 1, got %v", *in.Confidence), nil)
		}
		confidence = *in.Confidence
	}
	rel := ontology.Relation{
		Name:           "relations/" + uid,
		UID:            uid,
		SubjectUID:     subjectUID,
		Predicate:      in.Predicate,
		ObjectUID:      objectUID,
		Confidence:     confidence,
		SourceEventUID: uid,
		CreateTime:     time.Now().UTC(),
		Provenance:     in.Provenance,
		Attributes:     in.Attributes,
	}
	if rel.ValidFrom, err = parseRelateTime(in.ValidFrom, "validFrom"); err != nil {
		return ontology.Relation{}, "", err
	}
	if rel.ValidUntil, err = parseRelateTime(in.ValidUntil, "validUntil"); err != nil {
		return ontology.Relation{}, "", err
	}
	// Reject an empty or inverted valid-time window: validUntil is the exclusive
	// upper bound, so it must be strictly after validFrom. Otherwise the write
	// succeeds but default (point-in-time) navigation never returns the edge,
	// making the relation look silently lost.
	if rel.ValidFrom != nil && rel.ValidUntil != nil && !rel.ValidUntil.After(*rel.ValidFrom) {
		return ontology.Relation{}, "", mcp.NewValidationError(
			fmt.Sprintf("validUntil (%s) must be after validFrom (%s)",
				rel.ValidUntil.Format(time.RFC3339), rel.ValidFrom.Format(time.RFC3339)), nil)
	}
	return rel, scope, nil
}

// resolveEndpoint normalizes one relation endpoint into a concrete entity UID:
// it parses the argument, rebinds the "self" sentinel to the local principal,
// guards against a team-scope write that still references the unresolved "self"
// literal, and confirms the entity exists. Role is "subject" or "object" and
// labels validation errors. When nameNav is set and the arg is a bare token
// that fails ULID validation, name resolution is attempted via resolveEntityByName
// so display names like "Bilbo Baggins" work alongside UIDs and resource names.
// Factored out of buildRelation to keep that function's branch count under the
// cyclomatic-complexity gate.
func (r *Relate) resolveEndpoint(ctx context.Context, arg, role string, scope kernel.Scope) (string, error) {
	uid, err := relateEndpoint(arg, role == "subject")
	if err != nil {
		// relateEndpoint only errors for a bare token that is neither a valid ULID
		// nor "self" (it returns "self" without error), so when nameNav is wired we
		// try that token as a display name. A genuine resource name (a recognized
		// collection prefix before the "/") is not a display name, so those still
		// surface the original ULID error; a slashed display name like
		// "Ring Mk 4000 / RFC 1149" does NOT look like a resource name and resolves.
		if r.nameNav == nil || looksLikeAIPResourceName(arg) {
			return "", err
		}
		uid, err = resolveEntityByName(ctx, r.nameNav, arg, "")
		if err != nil {
			return "", err
		}
	}
	// Rebind the "self" sentinel to the concrete principal UID before validation
	// or persistence, so the synced edge references the shareable principal
	// rather than the local-only literal.
	uid = rebindSelf(ctx, r.binder, uid)
	// A team-scope edge must not persist the unresolved "self" literal: it would
	// leak a device-local sentinel into a shared edge. Reject until the
	// self->principal binding is provisioned (which rebinds it above).
	if err := guardTeamSelf(scope, uid, role); err != nil {
		return "", err
	}
	if err := r.checkEndpointExists(ctx, uid, role); err != nil {
		return "", err
	}
	return uid, nil
}

// checkEndpointExists confirms a resolved endpoint uid refers to a real entity,
// so a mistyped uid is rejected up front rather than written as a dangling edge
// that later hydrates a blank node. The "self" sentinel is always allowed; a nil
// resolver (no navigator wired) disables the check.
func (r *Relate) checkEndpointExists(ctx context.Context, uid, role string) error {
	if uid == selfEntityUID || r.entities == nil {
		return nil
	}
	if _, err := r.entities.GetEntity(ctx, uid); err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return mcp.NewNotFoundError(
				fmt.Sprintf("%s entity %q does not exist; create it first (remember) or pass a known entities/{uid}", role, uid))
		}
		return mcp.NewInternalError(fmt.Errorf("verify %s entity %q: %w", role, uid, err))
	}
	return nil
}

// relateEndpoint normalizes a subject/object argument into a bare uid and
// validates it. It accepts the entities/{uid} resource-name form (so recall
// anchors paste straight in) and the "self" sentinel for either endpoint
// (the Relation schema and checkEndpointExists both accept "self" as either
// subject or object). A non-ULID, non-self reference is rejected here as
// E_VALIDATION rather than being left to surface as an opaque E_INTERNAL when
// the ontology schema rejects the append downstream.
func relateEndpoint(arg string, isSubject bool) (string, error) {
	uid, err := entityUIDFromArg(arg)
	if err != nil {
		return "", err
	}
	if uid == selfEntityUID {
		return uid, nil
	}
	if _, perr := ulid.Parse(uid); perr != nil {
		role := "object"
		if isSubject {
			role = "subject"
		}
		return "", mcp.NewValidationError(
			fmt.Sprintf("%s %q is not a valid entity reference: expected a 26-char ULID or \"self\", optionally as entities/{uid}", role, arg), nil)
	}
	return uid, nil
}

// parseRelateTime parses an optional RFC 3339 validity bound into a *time.Time.
// A nil/empty input yields (nil, nil); a malformed value is E_VALIDATION.
func parseRelateTime(s *string, field string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil //nolint:nilnil // (nil, nil) is the documented "absent" signal for an optional field
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("parse '%s': %v", field, err), nil)
	}
	t = t.UTC()
	return &t, nil
}

// newRelationULID returns a fresh ULID in Crockford base32, matching the CLI's
// relate command.
func newRelationULID() (string, error) {
	id, err := ulid.New(ulid.Now(), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		return "", fmt.Errorf("ulid: %w", err)
	}
	return id.String(), nil
}
