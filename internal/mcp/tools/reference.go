package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// referenceIdentifierType is the Identifier.Type stamped on an external_ref's
// canonical URI identifier. Free-form per the entity schema; "uri" reads as the
// {type:"uri"} identity #208 specifies and is preserved as its own lookup lane.
const referenceIdentifierType = "uri"

const referenceInputSchema = `{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "minLength": 1,
      "description": "Absolute URI of the external thing, e.g. https://github.com/org/repo/issues/42, mailto:a@b.com, or a connector URL. Must carry a scheme."
    },
    "label": {
      "type": "string",
      "minLength": 1,
      "description": "Optional human-readable display name. Defaults to the URI. Keep third-party identity metadata out of project (git-rendered) scope."
    },
    "scope": {"type": "string", "enum": ["project", "personal", "team"], "description": "Scope to write into. Defaults to personal (local, not rendered to the shared project git tree). Pass \"project\" to share. Dedup is WITHIN scope: the same URI in two scopes yields two entities."}
  },
  "required": ["uri"],
  "additionalProperties": false
}`

const referenceOutputSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "pattern": "^entities/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the external_ref; pass to relate(localEntity, has_primary_source, name) or get(name)."},
    "uid": {"type": "string", "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$", "description": "Bare ULID of the external_ref, derived deterministically from (scope, uri)."},
    "uri": {"type": "string", "description": "The canonical URI, echoed back."},
    "scope": {"type": "string", "enum": ["project", "personal", "team"], "description": "The resolved scope the external_ref lives in."},
    "created": {"type": "boolean", "description": "true if a new external_ref was minted, false if an existing one was returned (mint-or-return)."}
  },
  "required": ["name", "uid", "uri", "scope", "created"],
  "additionalProperties": false
}`

// Reference mints-or-returns an external_ref entity for a URI: a first-class
// local pointer to information held in another system (a GitHub issue, a Drive
// doc, a Granola transcript, a repo file). Because the external_ref is a real
// local ULID entity, relate / recall / traverse / getContext work on it with no
// read-path change — relate(localEntity, has_primary_source, externalRef) is a
// separate call that asserts the provenance edge.
//
// Mazarbul never dereferences the URI (the socket is unauthenticated; a
// server-side fetch would be an SSRF/confused-deputy engine). To make the
// referenced CONTENT searchable, the agent fetches it with its own tools and
// remember()s it, recordSourceRef()s the provenance, then reference()s the URI
// and relates the two — all four converge on this canonical node.
//
// Dedup is deterministic and WITHIN scope: ExternalRefUID(scope, uri) keys the
// entity, so a repeat call for the same URI in the same scope returns the same
// entity, while the same URI in personal vs project scope yields two distinct
// entities — a personal-private pointer never collapses into a project-scope
// entity that renders to git.
type Reference struct {
	appender Appender
	// nav resolves the existence check; nil (minimal boot) skips it and always
	// mints. The check is scope-narrowed so a team-scope ref is found in team.
	nav Navigator
}

// NewReference constructs a Reference tool bound to appender a and navigator
// nav. Nav may be nil to skip the mint-or-return existence check (minimal boot).
func NewReference(a Appender, nav Navigator) *Reference {
	return &Reference{appender: a, nav: nav}
}

// Name implements mcp.Tool.
func (*Reference) Name() string { return "reference" }

// Description implements mcp.Tool.
func (*Reference) Description() string {
	return "Mint-or-return an external_ref entity for a URI: a first-class local pointer to information held in another system (a GitHub issue, a Drive doc, a Granola transcript, a repo file). Returns the external_ref's resource name so you can relate(localEntity, has_primary_source, name) to record that a local record is grounded in it. Idempotent: the same URI in the same scope returns the same entity. Defaults to personal scope (local, not rendered to the shared project git tree); pass scope=project to share. Mazarbul does NOT fetch the URI — to make the referenced content searchable, fetch it with your own tools, remember() it, then reference() the URI and relate the two. Returns E_VALIDATION on a URI with no scheme or an unknown scope."
}

// InputSchema implements mcp.Tool.
func (*Reference) InputSchema() json.RawMessage { return json.RawMessage(referenceInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Reference) OutputSchema() json.RawMessage { return json.RawMessage(referenceOutputSchema) }

type referenceInput struct {
	URI   string `json:"uri"`
	Label string `json:"label,omitempty"`
	Scope string `json:"scope,omitempty"`
}

type referenceOutput struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	URI     string `json:"uri"`
	Scope   string `json:"scope"`
	Created bool   `json:"created"`
}

// Invoke implements mcp.Tool. It validates the URI, resolves the write scope
// (default personal), derives the canonical external_ref ULID, and returns the
// existing entity if one is already present in that scope or mints a new one.
func (r *Reference) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in referenceInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	uri, verr := validateReferenceURI(in.URI)
	if verr != nil {
		return nil, verr
	}
	scope, serr := parseWriteScope(in.Scope)
	if serr != nil {
		return nil, serr
	}
	// parseWriteScope yields "" for the default; resolve it to the concrete
	// personal scope so the dedup key and the existence read both reference the
	// scope the event log will actually backfill the entity into.
	effective := scope
	if effective == "" {
		effective = kernel.ScopePersonal
	}

	uid := string(ontology.ExternalRefUID(string(effective), uri))
	name := collectionEntities + "/" + uid

	found, ferr := r.lookup(ctx, effective, uid)
	if ferr != nil {
		return nil, ferr
	}
	if found {
		return marshalOutput(referenceOutput{
			Name:    name,
			UID:     uid,
			URI:     uri,
			Scope:   string(effective),
			Created: false,
		})
	}

	displayName := in.Label
	if displayName == "" {
		displayName = uri
	}
	events, _, berr := buildCreateEntityEvents(createEntityInput{
		Kind:        string(ontology.EntityKindExternalRef),
		DisplayName: displayName,
		Identifiers: []createEntityIdentifier{{Type: referenceIdentifierType, Value: uri}},
	}, uid, createEntityDefaultConfidence, scope)
	if berr != nil {
		return nil, berr
	}
	if _, err := r.appender.Append(ctx, events); err != nil {
		// A schema rejection (ErrInvalidEvent) is the caller's input mistake →
		// E_VALIDATION. Everything else routes through mapAppendError so a
		// team-scope write by a non-member surfaces as E_PERMISSION (matching
		// relate), not a misleading E_INTERNAL; genuine storage/wiring faults
		// still fall through to E_INTERNAL.
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("append external_ref: %v", err), nil)
		}
		return nil, mapAppendError("append external_ref", err)
	}

	return marshalOutput(referenceOutput{
		Name:    name,
		UID:     uid,
		URI:     uri,
		Scope:   string(effective),
		Created: true,
	})
}

// lookup reports whether the external_ref uid already exists in scope. A nil
// navigator (minimal boot) skips the check and reports not-found so the caller
// mints. Navigate.ErrNotFound is the expected miss; anything else is an
// internal fault. The resource name is deterministic (entities/{uid}), so the
// caller reconstructs it rather than reading it back here.
func (r *Reference) lookup(ctx context.Context, scope kernel.Scope, uid string) (found bool, err error) {
	if r.nav == nil {
		return false, nil
	}
	if _, gerr := navWithScopes(r.nav, kernel.NewScopeSet(scope)).GetEntity(ctx, uid); gerr != nil {
		if errors.Is(gerr, navigate.ErrNotFound) {
			return false, nil
		}
		return false, mcp.NewInternalError(fmt.Errorf("look up external_ref %q: %w", uid, gerr))
	}
	return true, nil
}

// validateReferenceURI trims and parses the URI, requiring an absolute form
// with a scheme. The trimmed value is the byte-exact dedup input — no further
// canonicalization, so callers control identity. Empty or scheme-less input is
// E_VALIDATION.
func validateReferenceURI(raw string) (string, error) {
	uri := strings.TrimSpace(raw)
	if uri == "" {
		return "", mcp.NewValidationError("uri is required", nil)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", mcp.NewValidationError(fmt.Sprintf("uri %q is not a valid URI: %v", uri, err), nil)
	}
	if u.Scheme == "" {
		return "", mcp.NewValidationError(
			fmt.Sprintf("uri %q must include a scheme, e.g. https://… or mailto:…", uri), nil)
	}
	return uri, nil
}
