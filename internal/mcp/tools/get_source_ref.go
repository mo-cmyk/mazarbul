package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// sourceRefTrustUnverified is the only value getSourceRef emits for trustState.
// A SourceRef's provenance is self-asserted by the appending device and is NOT
// cryptographically verified: #209 signs the carrying event's authorship but
// does not attest the external claim, and no principal/signature trust exists
// for a SourceRef yet (#207). The enum leaves room for "verified"/"attested"
// when signed provenance lands, without a wire break.
const sourceRefTrustUnverified = "unverified"

// getSourceRefInputSchema is the JSON Schema for the tools/call arguments. The
// `name` is the AIP resource name sourceRefs/{uid}; the uid is a ULID.
const getSourceRefInputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^sourceRefs/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name to fetch: sourceRefs/{uid}. The uid is the SourceRef ULID returned by recordSourceRef."},
    "scopes": {"type": "array", "items": {"type": "string"}, "description": "Scopes to read from: project, personal, team. Defaults to project+personal. Unknown values are rejected."}
  },
  "required": ["name"],
  "additionalProperties": false
}`

// getSourceRefOutputSchema documents the response: the SourceRef fields plus a
// trustState label that always reads "unverified".
const getSourceRefOutputSchema = `{
  "type": "object",
  "properties": {
    "name":           {"type": "string", "pattern": "^sourceRefs/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of this SourceRef."},
    "uid":            {"type": "string", "description": "SourceRef ULID identity."},
    "subjectUid":     {"type": "string", "description": "ULID of the record this source was derived from (PROV wasDerivedFrom)."},
    "sourceType":     {"type": "string", "description": "Kind of external source (open taxonomy), e.g. email, slack_message, web_page."},
    "connector":      {"type": "string", "description": "Connector that produced the reference, e.g. gmail, slack. Omitted when absent."},
    "stream":         {"type": "string", "description": "Sub-channel within the connector. Omitted when absent."},
    "externalId":     {"type": "string", "description": "Source-system opaque identifier for the item. Omitted when absent."},
    "externalUrl":    {"type": "string", "description": "Dereferenceable location of the source item. Omitted when absent."},
    "contentHash":    {"type": "string", "description": "Appender-supplied SHA-256 hex of the fetched content. Omitted when absent."},
    "fetchTime":      {"type": "string", "description": "When the source content was fetched (RFC 3339 UTC)."},
    "dedupKey":       {"type": "string", "description": "Idempotency key for the source_refs projection."},
    "sourceEventUid": {"type": "string", "description": "UID of the sourceRefRecorded event that carried this record."},
    "createTime":     {"type": "string", "description": "When the SourceRef event was recorded (RFC 3339 UTC)."},
    "scope":          {"type": "string", "description": "Scope this SourceRef lives in: personal, project, or team."},
    "trustState":     {"type": "string", "enum": ["unverified"], "description": "Provenance is self-asserted by the appending device and is NOT cryptographically verified. No principal/signature trust exists for a SourceRef yet (#207)."}
  },
  "required": ["name", "uid", "subjectUid", "sourceType", "fetchTime", "dedupKey", "sourceEventUid", "trustState"],
  "additionalProperties": false
}`

// GetSourceRef is the LLM-free fetch-by-resource-name tool for external-source
// provenance. It returns the SourceRef addressed by sourceRefs/{uid} and labels
// its provenance explicitly UNVERIFIED (self-asserted), since no
// principal/signature trust exists for a SourceRef yet. It reads through the
// shared Navigator interface, so the production *navigate.Navigator and the test
// fake both satisfy it; per-request scope narrowing is handled by navWithScopes.
type GetSourceRef struct {
	nav Navigator
}

// NewGetSourceRef constructs a GetSourceRef tool over nav.
func NewGetSourceRef(nav Navigator) *GetSourceRef { return &GetSourceRef{nav: nav} }

// Name implements mcp.Tool.
func (*GetSourceRef) Name() string { return "getSourceRef" }

// Description implements mcp.Tool.
func (*GetSourceRef) Description() string {
	return "Fetch one external-source provenance record (SourceRef) by AIP resource name sourceRefs/{uid}. A SourceRef records what a memory was derived from — the connector, stream, external id/url, content hash, and fetch time (W3C PROV wasDerivedFrom). Its provenance is SELF-ASSERTED and UNVERIFIED (trustState is always \"unverified\"): there is no cryptographic attestation of the external claim yet. Use recordSourceRef to create one."
}

// InputSchema implements mcp.Tool.
func (*GetSourceRef) InputSchema() json.RawMessage { return json.RawMessage(getSourceRefInputSchema) }

// OutputSchema implements mcp.Tool.
func (*GetSourceRef) OutputSchema() json.RawMessage {
	return json.RawMessage(getSourceRefOutputSchema)
}

type getSourceRefInput struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes,omitempty"`
}

// getSourceRefOutput flattens the SourceRef projection and appends the
// unverified-provenance label.
type getSourceRefOutput struct {
	navigate.SourceRef
	TrustState string `json:"trustState"`
}

// Invoke implements mcp.Tool. A malformed name is E_VALIDATION; a well-formed
// name whose uid does not exist (or lies outside the requested scopes) is
// E_NOT_FOUND.
func (g *GetSourceRef) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in getSourceRefInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	uid, err := parseSourceRefName(in.Name)
	if err != nil {
		return nil, err
	}
	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}

	n := navWithScopes(g.nav, scopeSet)
	sr, err := n.GetSourceRef(ctx, uid)
	if err != nil {
		if errors.Is(err, navigate.ErrNotFound) {
			return nil, mcp.NewNotFoundError(in.Name + " not found")
		}
		return nil, mcp.NewInternalError(fmt.Errorf("getSourceRef %s: %w", in.Name, err))
	}

	body, err := json.Marshal(getSourceRefOutput{SourceRef: sr, TrustState: sourceRefTrustUnverified})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// parseSourceRefName splits "sourceRefs/{uid}" into the uid, validating the
// collection prefix AND that the uid is a structurally-valid ULID. Returns an
// E_VALIDATION ToolError on a malformed name so the CLI/in-process callers that
// bypass the schema validator still reject a bad name — a malformed uid is an
// invalid argument, not a missing resource (which would be E_NOT_FOUND).
func parseSourceRefName(name string) (string, error) {
	uid, ok := strings.CutPrefix(name, "sourceRefs/")
	if !ok || uid == "" || strings.Contains(uid, "/") {
		return "", mcp.NewValidationError(
			fmt.Sprintf("name %q must be a resource name of the form sourceRefs/{uid}", name), nil)
	}
	if _, err := ulid.Parse(uid); err != nil {
		return "", mcp.NewValidationError(
			fmt.Sprintf("name %q has a malformed uid: must be a 26-character Crockford base32 ULID", name), nil)
	}
	return uid, nil
}
