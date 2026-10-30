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

// unrelateInputSchema is the JSON Schema for the tools/call arguments. Identify
// the relation to close either by its resource name/uid (`relation`) or by the
// (subject, predicate, object) triple, which resolves the single currently-open
// matching edge. OneOf enforces that at least one mode is supplied; Invoke
// rejects the ambiguous both-modes case precisely.
const unrelateInputSchema = `{
  "type": "object",
  "properties": {
    "relation":     {"type": "string", "minLength": 1, "description": "The relation to close: relations/{uid} or a bare ULID (as returned by relations() or recall). Use THIS or a (subject, predicate, object) triple, not both."},
    "subject":      {"type": "string", "minLength": 1, "description": "Subject of the relation to close: entities/{uid}, a bare ULID, \"self\", or a display name. Provide with predicate+object as an alternative to relation."},
    "predicate":    {"type": "string", "minLength": 1, "description": "Predicate of the relation to close, e.g. works_at."},
    "object":       {"type": "string", "minLength": 1, "description": "Object of the relation to close: entities/{uid}, a bare ULID, \"self\", or a display name."},
    "endedAt":      {"type": "string", "format": "date-time", "description": "When the relationship ended (RFC 3339 UTC); becomes the edge's valid_until. Defaults to now."},
    "supersededBy": {"type": "string", "minLength": 1, "description": "Optional relations/{uid} of a newer relation that replaces this one (a correction). Omit for a pure close — the relationship simply ended."},
    "scopes":       {"type": "array", "items": {"type": "string"}, "description": "Scopes to search when identifying the edge by triple: project, personal, team. Defaults to project+personal."}
  },
  "oneOf": [
    {"required": ["relation"]},
    {"required": ["subject", "predicate", "object"]}
  ],
  "additionalProperties": false
}`

const unrelateOutputSchema = `{
  "type": "object",
  "properties": {
    "name":       {"type": "string", "pattern": "^relations/[0-9A-HJKMNP-TV-Z]{26}$", "description": "Resource name of the closed relation."},
    "status":     {"type": "string", "enum": ["retracted"], "description": "Always \"retracted\" on success: the edge is closed (valid_until set) but its history is preserved."},
    "validUntil": {"type": "string", "format": "date-time", "description": "The valid_until the relation was closed at (RFC 3339 UTC)."}
  },
  "required": ["name", "status", "validUntil"],
  "additionalProperties": false
}`

// Unrelate is the temporal counterpart to Relate: it END-DATES an existing
// relation by appending a relationRetracted tombstone (#205) that closes the
// edge with a valid_until. Unlike forget — which drops the derived relation row
// entirely — the edge SURVIVES with a bounded validity, so "X worked at Y until
// 2026-05-22" stays in history and time-travel reads (relations at=…) still see
// it. Relate only ever appends, so without this tool there was no way to record
// that a relationship ended without either lying (a parallel dead edge) or
// destroying history (forget).
//
// The close is a pure temporal one (empty SupersededByUID) unless the caller
// passes supersededBy, the correction case the resolver's auto-retraction also
// uses. The tombstone fold (recomputeRelationClose) is the sole authority for
// the closed columns, so the close converges across paired devices regardless
// of replay order.
type Unrelate struct {
	appender Appender
	nav      Navigator
	binder   selfBinder
}

// NewUnrelate constructs an Unrelate tool bound to appender a and the navigator
// used to resolve the target relation (by uid existence check or by triple).
func NewUnrelate(a Appender, nav Navigator) *Unrelate {
	return &Unrelate{appender: a, nav: nav}
}

// SetSelfBinder injects the self->principal binder so a "self" subject/object
// in the triple form resolves to the bound principal (mirrors relate/relations).
func (u *Unrelate) SetSelfBinder(b selfBinder) { u.binder = b }

// Name implements mcp.Tool.
func (*Unrelate) Name() string { return "unrelate" }

// Description implements mcp.Tool.
func (*Unrelate) Description() string {
	return "End a relation as of a date — the temporal counterpart to relate. Records that a relationship STOPPED being true (someone left a job, a project ended) by closing the edge with a valid_until: it drops out of current reads but its history is PRESERVED (unlike forget, which deletes the edge). Identify the edge by its relations/{uid} or by the (subject, predicate, object) triple (which closes the single open match). Pass endedAt for when it ended (defaults to now); pass supersededBy when a newer relation replaces it."
}

// InputSchema implements mcp.Tool.
func (*Unrelate) InputSchema() json.RawMessage { return json.RawMessage(unrelateInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Unrelate) OutputSchema() json.RawMessage { return json.RawMessage(unrelateOutputSchema) }

type unrelateInput struct {
	Relation     string   `json:"relation,omitempty"`
	Subject      string   `json:"subject,omitempty"`
	Predicate    string   `json:"predicate,omitempty"`
	Object       string   `json:"object,omitempty"`
	EndedAt      string   `json:"endedAt,omitempty"`
	SupersededBy string   `json:"supersededBy,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

type unrelateOutput struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	ValidUntil string `json:"validUntil"`
}

// Invoke implements mcp.Tool. It resolves the target relation, then appends a
// relationRetracted closing it as of endedAt (default now).
func (u *Unrelate) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in unrelateInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	relUID, supersededBy, endedAt, verr := u.prepare(ctx, in)
	if verr != nil {
		return nil, verr
	}

	payload, err := json.Marshal(ontology.RelationRetractedEvent{
		RelationUID:     ontology.ULID(relUID),
		SupersededByUID: ontology.ULID(supersededBy),
		RetractedAt:     endedAt,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal relationRetracted: %w", err))
	}
	receipts, err := u.appender.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindRelationRetracted,
		Source:     "mcp:unrelate",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if err != nil {
		return nil, mapAppendError("append relationRetracted", err)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	body, err := json.Marshal(unrelateOutput{
		Name:   "relations/" + relUID,
		Status: "retracted",
		// Echo at the layout the dispatcher stores valid_until with, so the
		// response matches what a subsequent read of the edge reports.
		ValidUntil: endedAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// prepare validates the request mode and resolves the target relation uid, the
// optional superseder uid, and the close instant. Factored out of Invoke to
// keep that function's branch count under the cyclomatic-complexity gate.
func (u *Unrelate) prepare(ctx context.Context, in unrelateInput) (relUID, supersededBy string, endedAt time.Time, err error) {
	hasRelation, _, err := validateUnrelateMode(in)
	if err != nil {
		return "", "", time.Time{}, err
	}

	endedAt, err = parseEndedAt(in.EndedAt)
	if err != nil {
		return "", "", time.Time{}, err
	}
	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return "", "", time.Time{}, serr
	}
	nav := navWithScopes(u.nav, scopeSet)

	relUID, err = u.resolveTarget(ctx, nav, in, hasRelation)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if in.SupersededBy != "" {
		supersededBy, err = relationUIDFromName(in.SupersededBy)
		if err != nil {
			return "", "", time.Time{}, err
		}
	}
	return relUID, supersededBy, endedAt, nil
}

// resolveTarget returns the uid of the relation to close, either from the
// explicit relation arg (validated + existence-checked) or by resolving the
// single open edge matching the (subject, predicate, object) triple.
func (u *Unrelate) resolveTarget(ctx context.Context, nav Navigator, in unrelateInput, hasRelation bool) (string, error) {
	if hasRelation {
		relUID, err := relationUIDFromName(in.Relation)
		if err != nil {
			return "", err
		}
		rel, err := nav.GetRelation(ctx, relUID)
		if err != nil {
			if errors.Is(err, navigate.ErrNotFound) {
				return "", mcp.NewNotFoundError("relations/" + relUID + " not found; nothing to close")
			}
			return "", mcp.NewInternalError(fmt.Errorf("unrelate: verify relation: %w", err))
		}
		// Mirror the triple path's open-only contract (IncludeInactive:false):
		// refuse to re-close an edge already closed as of now (superseded, or a
		// valid_until in the past). Re-closing would silently backdate the close
		// via the earliest-wins fold. A still-active edge with a FUTURE valid_until
		// stays closable — its end can be moved earlier.
		if relationClosedNow(rel) {
			return "", mcp.NewValidationError(fmt.Sprintf(
				"relations/%s is already closed (valid until %s); nothing to do", relUID, rel.ValidUntil), nil)
		}
		return relUID, nil
	}
	return u.resolveOpenEdge(ctx, nav, in.Subject, in.Predicate, in.Object)
}

// resolveOpenEdge finds the single currently-open relation matching the
// directed (subject)-[predicate]->(object) triple. Zero matches → E_NOT_FOUND
// (already ended/superseded or never existed); more than one → E_VALIDATION
// asking the caller to disambiguate by relation uid.
func (u *Unrelate) resolveOpenEdge(ctx context.Context, nav Navigator, subject, predicate, object string) (string, error) {
	subjectUID, err := resolveEntityArg(ctx, nav, u.binder, subject)
	if err != nil {
		return "", err
	}
	objectUID, err := resolveEntityArg(ctx, nav, u.binder, object)
	if err != nil {
		return "", err
	}
	rels, err := nav.EntityRelations(ctx, subjectUID, navigate.RelationFilter{
		Predicates:      []string{predicate},
		IncludeInactive: false, // only edges live now: never re-close an already-closed edge
	})
	if err != nil {
		return "", mcp.NewInternalError(fmt.Errorf("unrelate: list relations for %q: %w", subjectUID, err))
	}
	var matches []string
	for i := range rels {
		r := rels[i]
		if r.SubjectUID == subjectUID && r.ObjectUID == objectUID && r.Predicate == predicate {
			matches = append(matches, r.UID)
		}
	}
	switch len(matches) {
	case 0:
		return "", mcp.NewNotFoundError(fmt.Sprintf(
			"no open %q relation from %q to %q to close; it may already be ended, superseded, or never existed", predicate, subject, object))
	case 1:
		return matches[0], nil
	default:
		return "", mcp.NewValidationError(fmt.Sprintf(
			"%d open %q relations from %q to %q; pass the specific relations/{uid} via 'relation' to disambiguate", len(matches), predicate, subject, object), nil)
	}
}

// validateUnrelateMode checks the caller supplied exactly one way to identify
// the edge — a relation (uid/resource name) or a full (subject, predicate,
// object) triple — and returns the two mode flags. A partial triple gets its own
// pointed message so an agent that supplied two of three fields knows what is
// missing.
func validateUnrelateMode(in unrelateInput) (hasRelation, hasTriple bool, err error) {
	hasRelation = in.Relation != ""
	hasTriple = in.Subject != "" && in.Predicate != "" && in.Object != ""
	anyTriple := in.Subject != "" || in.Predicate != "" || in.Object != ""
	switch {
	case hasRelation && anyTriple:
		return false, false, mcp.NewValidationError("provide either relation OR a (subject, predicate, object) triple, not both", nil)
	case !hasRelation && anyTriple && !hasTriple:
		return false, false, mcp.NewValidationError("a (subject, predicate, object) triple needs all three fields to identify the edge; you supplied only some — add the missing field(s) or pass a relation uid", nil)
	case !hasRelation && !hasTriple:
		return false, false, mcp.NewValidationError("provide a relation (relations/{uid} or a bare uid) or a full (subject, predicate, object) triple identifying the edge to close", nil)
	}
	return hasRelation, hasTriple, nil
}

// relationClosedNow reports whether rel is already closed as of now — superseded
// by a newer edge, or carrying a valid_until that has already passed. It mirrors
// the v_relations.active contract the triple path enforces via
// IncludeInactive:false, so both ways of identifying an edge refuse to re-close
// an already-ended one (which would silently backdate the close via the fold's
// earliest-wins reduction). An unparseable bound is treated as open rather than
// blocking a legitimate close.
func relationClosedNow(rel navigate.Relation) bool {
	if rel.SupersededByUID != "" {
		return true
	}
	if rel.ValidUntil == "" {
		return false
	}
	// Parse with the same layout the dispatcher serializes the column with
	// (Format(RFC3339Nano)). time.Parse(RFC3339, …) happens to tolerate the
	// fractional seconds too, but matching the storage layout is self-documenting
	// and immune to a future reader "correcting" it the wrong way.
	end, err := time.Parse(time.RFC3339Nano, rel.ValidUntil)
	if err != nil {
		return false
	}
	return !time.Now().UTC().Before(end)
}

// parseEndedAt parses the optional endedAt arg into a UTC instant, defaulting to
// now when omitted. A non-RFC-3339 value is a caller error → E_VALIDATION.
func parseEndedAt(raw string) (time.Time, error) {
	if raw == "" {
		return time.Now().UTC(), nil
	}
	// RFC3339Nano accepts both whole-second and fractional-second RFC 3339
	// instants, so a programmatic caller passing time.Now().Format(RFC3339Nano)
	// is honoured rather than rejected.
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, mcp.NewValidationError(fmt.Sprintf("endedAt must be RFC 3339 (e.g. 2026-05-22T00:00:00Z): %v", err), nil)
	}
	return t.UTC(), nil
}
