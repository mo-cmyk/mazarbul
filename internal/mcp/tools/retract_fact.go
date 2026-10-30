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

// retractFactInputSchema is the JSON Schema for the tools/call arguments.
// Identify the slot to close either by the fact's resource name/uid (`fact`) or
// by the (subject, stateKey) pair, which resolves the slot's single open value.
const retractFactInputSchema = `{
  "type": "object",
  "properties": {
    "fact":     {"type": "string", "minLength": 1, "description": "The fact to retract: facts/{uid} or a bare ULID (as returned by fact() or get). Use THIS or a (subject, stateKey) pair, not both."},
    "subject":  {"type": "string", "minLength": 1, "description": "Subject entity whose slot to close: entities/{uid}, a bare ULID, \"self\", or a display name. Provide with stateKey as an alternative to fact."},
    "stateKey": {"type": "string", "minLength": 1, "description": "Slot address to close, e.g. state.task.status, preference.coffee.milk. Provide with subject."},
    "scopes":   {"type": "array", "items": {"type": "string"}, "description": "Scopes to search when identifying the slot by (subject, stateKey): project, personal, team. Defaults to project+personal."}
  },
  "oneOf": [
    {"required": ["fact"]},
    {"required": ["subject", "stateKey"]}
  ],
  "additionalProperties": false
}`

const retractFactOutputSchema = `{
  "type": "object",
  "properties": {
    "name":        {"type": "string", "pattern": "^facts/[0-9A-HJKMNP-TV-Z]{26}$", "description": "Resource name of the retracted fact."},
    "status":      {"type": "string", "enum": ["retracted"], "description": "Always \"retracted\" on success: the value is closed but its history is preserved."},
    "retractedAt": {"type": "string", "format": "date-time", "description": "The instant the fact was retracted (RFC 3339 UTC)."}
  },
  "required": ["name", "status", "retractedAt"],
  "additionalProperties": false
}`

// RetractFact is the scalar-slot counterpart to unrelate: it closes the CURRENT
// value of a Fact slot by appending a factRetracted tombstone (#0046). Unlike
// forget — which hard-deletes the fact row — the row SURVIVES with a bounded
// validity, so the value stays in history (get(facts/{uid}) still walks it). The
// slot then reads as EMPTY (no fallback to an earlier value): retraction records
// that the value stopped being true, not "undo". Fact only ever appends a new
// value; without this tool there was no way to record that a slot became
// empty/untrue without overwriting it with a replacement or destroying history
// (forget). To set a different value, just assert it with fact.
//
// The retraction is stamped server-side as of now, so a retracted value is
// always excluded from current reads. The tombstone fold (reconcileFactSlot) is
// the sole authority for the closed columns, so the close converges across
// paired devices regardless of replay order.
type RetractFact struct {
	appender Appender
	nav      Navigator
	binder   selfBinder
}

// NewRetractFact constructs a RetractFact tool bound to appender a and the
// navigator used to resolve the target fact (by uid or by subject+stateKey).
func NewRetractFact(a Appender, nav Navigator) *RetractFact {
	return &RetractFact{appender: a, nav: nav}
}

// SetSelfBinder injects the self->principal binder so a "self" subject resolves
// to the bound principal (mirrors fact/unrelate).
func (r *RetractFact) SetSelfBinder(b selfBinder) { r.binder = b }

// Name implements mcp.Tool.
func (*RetractFact) Name() string { return "retractFact" }

// Description implements mcp.Tool.
func (*RetractFact) Description() string {
	return "Close the current value of a fact slot — the scalar counterpart to unrelate. Records that a fact STOPPED being true (a status that no longer holds, a preference that changed with no replacement) by retracting it: it drops out of current reads but its history is PRESERVED (unlike forget, which deletes the row). The slot then reads as EMPTY — retraction does NOT fall back to an earlier value. Identify the value by its facts/{uid} or by the (subject, stateKey) pair (which closes the slot's single open value). To set a different value instead, just write it with fact() — a later write supersedes the prior."
}

// InputSchema implements mcp.Tool.
func (*RetractFact) InputSchema() json.RawMessage { return json.RawMessage(retractFactInputSchema) }

// OutputSchema implements mcp.Tool.
func (*RetractFact) OutputSchema() json.RawMessage { return json.RawMessage(retractFactOutputSchema) }

type retractFactInput struct {
	Fact     string   `json:"fact,omitempty"`
	Subject  string   `json:"subject,omitempty"`
	StateKey string   `json:"stateKey,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
}

type retractFactOutput struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	RetractedAt string `json:"retractedAt"`
}

// Invoke implements mcp.Tool. It resolves the open fact to close, then appends a
// factRetracted closing it as of now.
func (r *RetractFact) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in retractFactInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	factUID, verr := r.resolveTarget(ctx, in)
	if verr != nil {
		return nil, verr
	}

	retractedAt := time.Now().UTC()
	payload, err := json.Marshal(ontology.FactRetractedEvent{
		FactUID:     factUID,
		RetractedAt: retractedAt,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal factRetracted: %w", err))
	}
	receipts, err := r.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindFactRetracted),
		Source:     "mcp:retractFact",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if err != nil {
		return nil, mapAppendError("append factRetracted", err)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(fmt.Errorf("eventlog: append returned %d receipts, want 1", len(receipts)))
	}

	return marshalOutput(retractFactOutput{
		Name:        collectionFacts + "/" + factUID,
		Status:      "retracted",
		RetractedAt: retractedAt.Format(time.RFC3339Nano),
	})
}

// resolveTarget validates the request mode and resolves the open fact uid to
// retract — either from the explicit fact arg (validated + open-checked) or by
// resolving the single open value of the (subject, stateKey) slot.
func (r *RetractFact) resolveTarget(ctx context.Context, in retractFactInput) (string, error) {
	hasFact := in.Fact != ""
	hasSlot := in.Subject != "" && in.StateKey != ""
	anySlot := in.Subject != "" || in.StateKey != ""
	switch {
	case hasFact && anySlot:
		return "", mcp.NewValidationError("provide either fact OR a (subject, stateKey) pair, not both", nil)
	case !hasFact && anySlot && !hasSlot:
		return "", mcp.NewValidationError("a (subject, stateKey) pair needs both fields; add the missing one or pass a fact uid", nil)
	case !hasFact && !hasSlot:
		return "", mcp.NewValidationError("provide a fact (facts/{uid} or a bare uid) or a (subject, stateKey) pair identifying the slot to close", nil)
	}

	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return "", serr
	}
	nav := navWithScopes(r.nav, scopeSet)

	if hasFact {
		return r.resolveByUID(ctx, nav, in.Fact)
	}
	return r.resolveOpenSlot(ctx, nav, in.Subject, in.StateKey)
}

// resolveByUID validates the fact resource name/uid, confirms it exists, and
// refuses to re-close a value that is already closed (superseded or retracted),
// which would otherwise re-stamp the tombstone for no observable effect.
func (r *RetractFact) resolveByUID(ctx context.Context, nav Navigator, name string) (string, error) {
	factUID, err := factUIDFromName(name)
	if err != nil {
		return "", err
	}
	f, err := nav.GetFact(ctx, factUID)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return "", mcp.NewNotFoundError("facts/" + factUID + " not found; nothing to retract")
		}
		return "", mcp.NewInternalError(fmt.Errorf("retractFact: verify fact: %w", err))
	}
	if f.EventEnd != "" || f.SupersededByUID != "" {
		return "", mcp.NewValidationError(fmt.Sprintf(
			"facts/%s is not the slot's current value (already superseded or retracted); nothing to do", factUID), nil)
	}
	return factUID, nil
}

// resolveOpenSlot finds the single open value of the (subject, stateKey) slot.
// Zero open values → E_NOT_FOUND (the slot is already empty or never existed).
func (r *RetractFact) resolveOpenSlot(ctx context.Context, nav Navigator, subject, stateKey string) (string, error) {
	subjectUID, err := resolveEntityArg(ctx, nav, r.binder, subject)
	if err != nil {
		return "", err
	}
	// The zero FactFilter returns each slot's current value (open, not
	// superseded), so the slot has at most one match for stateKey.
	facts, err := nav.FactsForSubject(ctx, subjectUID, navigate.FactFilter{})
	if err != nil {
		return "", navQueryError(fmt.Sprintf("retractFact: facts for %q", subjectUID), err)
	}
	for i := range facts {
		if facts[i].StateKey == stateKey {
			return facts[i].UID, nil
		}
	}
	return "", mcp.NewNotFoundError(fmt.Sprintf(
		"no open %q fact for %q to retract; the slot may already be empty or never existed", stateKey, subject))
}

// factUIDFromName parses a facts/{uid} resource name (the form fact/get return)
// into a validated bare ULID. A bare uid is also accepted for CLI/direct-
// dispatch ergonomics. A name carrying a non-fact collection, or a uid that is
// not a 26-char ULID, is rejected as E_VALIDATION.
func factUIDFromName(name string) (string, error) {
	if name == "" {
		return "", mcp.NewValidationError("fact is required (facts/{uid})", nil)
	}
	uid := name
	if strings.Contains(name, "/") {
		collection, parsed, err := parseResourceName(name)
		if err != nil {
			return "", err
		}
		if collection != collectionFacts {
			return "", mcp.NewValidationError(
				fmt.Sprintf("expected a facts/{uid} name, got %q", name), nil)
		}
		uid = parsed
	}
	if _, err := ulid.Parse(uid); err != nil {
		return "", mcp.NewValidationError(
			fmt.Sprintf("fact reference %q is not a valid facts/{uid} name or 26-char ULID", name), nil)
	}
	return uid, nil
}
