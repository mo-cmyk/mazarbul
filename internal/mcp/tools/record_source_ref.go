package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// recordSourceRefInputSchema is the JSON Schema for the tools/call arguments.
// It is the typed ontology.SourceRef minus the uid (derived from dedupKey via
// ontology.SourceRefUID) and minus source_event_uid (the carrying event supplies
// it).
const recordSourceRefInputSchema = `{
  "type": "object",
  "properties": {
    "subjectUid":  {"type": "string", "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$", "description": "ULID of the record this source was derived from (PROV wasDerivedFrom): an entity, episode, or fact uid. Required."},
    "sourceType":  {"type": "string", "minLength": 1, "description": "Kind of external source, drawn from an open taxonomy: email, slack_message, notion_page, web_page, etc. Required."},
    "dedupKey":    {"type": "string", "minLength": 1, "description": "Idempotency key. Recording the same external item twice (same dedupKey) collapses to one SourceRef. Required."},
    "connector":   {"type": "string", "description": "Connector that produced the reference, e.g. gmail, slack. Optional."},
    "stream":      {"type": "string", "description": "Sub-channel within the connector, e.g. a mailbox label or channel id. Optional."},
    "externalId":  {"type": "string", "description": "Source-system opaque identifier for the item, e.g. a message id. Optional."},
    "externalUrl": {"type": "string", "format": "uri", "description": "Dereferenceable location of the source item. Optional."},
    "contentHash": {"type": "string", "pattern": "^[0-9a-f]{64}$", "description": "SHA-256 hex of the fetched content. Optional; supplied by the caller (fetch is out of scope)."},
    "fetchTime":   {"type": "string", "format": "date-time", "description": "When the source content was fetched (RFC 3339 UTC). Defaults to now."},
    "scope":       {"type": "string", "enum": ["personal", "project", "team"], "description": "Scope to record into. Defaults to personal."}
  },
  "required": ["subjectUid", "sourceType", "dedupKey"],
  "additionalProperties": false
}`

const recordSourceRefOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^sourceRefs/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the recorded SourceRef; pass to getSourceRef."},
    "status": {"type": "string", "enum": ["recorded"], "description": "Always \"recorded\" on success."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// RecordSourceRef is the direct structured-write tool for external-source
// provenance: it appends a sourceRefRecorded event the dispatcher projects into
// the source_refs table. The structured counterpart to remember()'s
// natural-language write — when a caller already holds where a record came
// from, recordSourceRef captures it in one call.
//
// Provenance recorded here is self-asserted and unverified (see getSourceRef's
// trustState); this tool does not fetch or dereference the source.
type RecordSourceRef struct {
	appender Appender
}

// NewRecordSourceRef constructs a RecordSourceRef tool bound to appender a. In
// production a is *kernel.FileEventLog (configured with kernel.WithStorage so
// Apply reaches the dispatcher); in tests a is a fake capturing the event.
func NewRecordSourceRef(a Appender) *RecordSourceRef { return &RecordSourceRef{appender: a} }

// Name implements mcp.Tool.
func (*RecordSourceRef) Name() string { return "recordSourceRef" }

// Description implements mcp.Tool.
func (*RecordSourceRef) Description() string {
	return "Record where a memory came from: append an external-source provenance reference (SourceRef, W3C PROV wasDerivedFrom) for a subject record. Give the subject's uid, a sourceType (open taxonomy), and a dedupKey for idempotency; optionally a connector, stream, externalId, externalUrl, and contentHash. Returns the new SourceRef's resource name. Provenance is self-asserted and UNVERIFIED. Does not fetch the source."
}

// InputSchema implements mcp.Tool.
func (*RecordSourceRef) InputSchema() json.RawMessage {
	return json.RawMessage(recordSourceRefInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*RecordSourceRef) OutputSchema() json.RawMessage {
	return json.RawMessage(recordSourceRefOutputSchema)
}

type recordSourceRefInput struct {
	SubjectUID  string `json:"subjectUid"`
	SourceType  string `json:"sourceType"`
	DedupKey    string `json:"dedupKey"`
	Connector   string `json:"connector,omitempty"`
	Stream      string `json:"stream,omitempty"`
	ExternalID  string `json:"externalId,omitempty"`
	ExternalURL string `json:"externalUrl,omitempty"`
	ContentHash string `json:"contentHash,omitempty"`
	FetchTime   string `json:"fetchTime,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

type recordSourceRefOutput struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Invoke implements mcp.Tool. It validates the input, mints a SourceRef ULID,
// and appends a sourceRefRecorded event. The required fields are re-checked
// here, not just in the JSON schema: direct/CLI dispatch bypasses the schema's
// minLength/required constraints.
func (r *RecordSourceRef) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in recordSourceRefInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.SubjectUID == "" {
		return nil, mcp.NewValidationError("subjectUid is required", nil)
	}
	// Validate the subjectUid ULID up front (mirrors relate's endpoint check) so
	// a malformed subject is a clean E_VALIDATION rather than surfacing as the
	// kernel's E_INTERNAL schema-rejection on the CLI/in-process path.
	if _, perr := ulid.Parse(in.SubjectUID); perr != nil {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("subjectUid %q is not a valid ULID (26-char Crockford base32)", in.SubjectUID), nil)
	}
	if in.SourceType == "" {
		return nil, mcp.NewValidationError("sourceType is required", nil)
	}
	if in.DedupKey == "" {
		return nil, mcp.NewValidationError("dedupKey is required", nil)
	}
	scope, serr := parseRecordScope(in.Scope)
	if serr != nil {
		return nil, serr
	}
	// Connector-sourced provenance is sensitive (the same class as captureExternal
	// and connect, which already refuse project scope). Refuse here too, before
	// appending, so connector identifiers never reach the shared git tree via the
	// project shard's source-refs.jsonl render.
	if in.Connector != "" && scope == kernel.ScopeProject {
		return nil, mcp.NewValidationError(
			"recordSourceRef with a connector set refuses project scope: "+
				"connector-sourced provenance is sensitive and project renders to shared git; "+
				"use personal or team", nil)
	}
	fetchTime, ferr := parseRecordFetchTime(in.FetchTime)
	if ferr != nil {
		return nil, ferr
	}

	// Derive the uid from dedupKey (not a random mint): source_refs is
	// UNIQUE(dedup_key), so a random per-call uid would 404 on any re-record. A
	// derived uid keeps PRIMARY KEY(uid) and UNIQUE(dedup_key) 1:1, so the name
	// returned here always resolves through getSourceRef.
	uid := ontology.SourceRefUID(in.DedupKey)
	payload, err := json.Marshal(ontology.SourceRefRecordedEvent{
		UID:         uid,
		SubjectUID:  ontology.ULID(in.SubjectUID),
		SourceType:  in.SourceType,
		Connector:   in.Connector,
		Stream:      in.Stream,
		ExternalID:  in.ExternalID,
		ExternalURL: in.ExternalURL,
		ContentHash: in.ContentHash,
		FetchTime:   fetchTime,
		DedupKey:    in.DedupKey,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal source ref: %w", err))
	}

	receipts, err := r.appender.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindSourceRefRecorded),
		Source:     "mcp:recordSourceRef",
		Confidence: 1.0,
		Scope:      scope,
		Payload:    payload,
	}})
	if err != nil {
		// A payload that fails the kernel's pre-write schema validation (e.g. a
		// malformed contentHash that slipped past this tool because CLI/in-process
		// dispatch bypasses the JSON-schema validator) is a caller error, not an
		// internal fault — map it to E_VALIDATION like the wire path does.
		if errors.Is(err, kernel.ErrInvalidEvent) {
			return nil, mcp.NewValidationError(fmt.Sprintf("invalid source ref: %v", err), nil)
		}
		// A team-scope write by a non-member surfaces kernel.ErrPermission from the
		// append boundary; route through mapAppendError so it becomes E_PERMISSION
		// (a transient lock → E_UNAVAILABLE), not an opaque E_INTERNAL.
		return nil, mapAppendError("append source ref", err)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	body, err := json.Marshal(recordSourceRefOutput{Name: "sourceRefs/" + string(uid), Status: "recorded"})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// parseRecordScope validates an optional scope string. Empty → the zero Scope,
// which FileEventLog.prepare backfills to personal. A non-empty value must be
// one of the recognised scopes (including draft for staging).
func parseRecordScope(raw string) (kernel.Scope, error) {
	switch raw {
	case "":
		return "", nil
	case string(kernel.ScopePersonal), string(kernel.ScopeProject), string(kernel.ScopeTeam), string(kernel.ScopeDraft):
		return kernel.Scope(raw), nil
	default:
		return "", mcp.NewValidationError(
			fmt.Sprintf("invalid scope %q (allowed: personal, project, team, draft)", raw), nil)
	}
}

// parseRecordFetchTime parses the optional RFC 3339 fetchTime, defaulting to
// now when omitted. A malformed timestamp is an E_VALIDATION error.
func parseRecordFetchTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Now().UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, mcp.NewValidationError(fmt.Sprintf("parse 'fetchTime': %v", err), nil)
	}
	return t.UTC(), nil
}
