package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/extractor"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// rememberDefaultConfidence is the confidence value applied when the
// caller omits the optional `confidence` field. 1.0 is the right default
// for a remember() call: the agent is asserting a fact verbatim from a
// user note. Lower confidence belongs to enrichment / inference paths,
// not to the primary write surface.
const rememberDefaultConfidence = 1.0

// rememberInputSchema is the JSON Schema for the tools/call arguments.
// The adapter's precompiled validator enforces this before Invoke runs,
// so the handler can trust its decoded input shape. AIP-compliant
// naming (camelCase, no `*_at` suffixes); additionalProperties:false so
// a typoed field surfaces as a validation error rather than a silent
// drop.
const rememberInputSchema = `{
  "type": "object",
  "properties": {
    "text":       {"type": "string", "minLength": 1, "description": "Natural-language record to remember. Stored verbatim as the Episode text."},
    "source":     {"type": "string", "minLength": 1, "description": "Optional. Where the Episode originated (free-form, e.g. \"cli:remember\", \"providers/anthropic\"). Defaults to \"mcp:remember\" when omitted — just call remember({text}) for an ordinary fact."},
    "actor":      {"type": "string", "enum": ["user", "assistant", "system"], "description": "Who said it. \"user\" (the human), \"assistant\" (the AI agent), or \"system\". Defaults to \"user\" when omitted. Assistant-actor episodes are routed through a different extractor branch so AI-generated facts (\"I've booked your flight for March 3rd\") become first-class memories instead of being filtered as third-party narration."},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1, "description": "Writer's confidence in the Episode. Range 0..1 inclusive. Defaults to 1.0 when omitted."},
    "coordinates": {
      "type": "object",
      "description": "Optional geographic coordinates (WGS84) associated with this Episode. When present, the resolver forwards them onto any matched or created place Entity.",
      "properties": {
        "latitude":  {"type": "number", "minimum": -90,  "maximum": 90,  "description": "Latitude in WGS84 decimal degrees. Range -90..90 inclusive."},
        "longitude": {"type": "number", "minimum": -180, "maximum": 180, "description": "Longitude in WGS84 decimal degrees. Range -180..180 inclusive."}
      },
      "required": ["latitude", "longitude"],
      "additionalProperties": false
    },
    "coordinatesProvenance": {"type": "string", "minLength": 1, "description": "Origin of the coordinates, e.g. user_provided or geocoded:nominatim. Required when coordinates is set; rejected when coordinates is absent."},
    "scope":      {"type": "string", "enum": ["personal", "project", "team", "draft"], "description": "Sync / encryption boundary for the Episode. \"personal\" (default) is local-only; \"project\" is in-repo; \"team\" is shared under the team key; \"draft\" is private staging — invisible to recall until promoted via the CLI (maz scope draft promote); there is no listed MCP tool for draft promotion. Omit for the personal default."}
  },
  "required": ["text"],
  "additionalProperties": false
}`

// rememberOutputSchema documents the response shape. The single `name`
// field carries the AIP resource name of the newly created Episode,
// which composes directly into a future getEpisode / updateEpisode /
// deleteEpisode call. Pattern mirrors ontology.Episode.Name.
const rememberOutputSchema = `{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "pattern": "^episodes/[0-9A-HJKMNP-TV-Z]{26}$",
      "description": "AIP resource name of the created Episode: \"episodes/{uid}\"."
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`

// Appender is the narrow write-side dependency the remember tool needs.
// *kernel.FileEventLog satisfies it; tests pass a fake to assert the
// handler dispatches the right Event without standing up the four-layer
// storage stack. The signature matches kernel.EventLog.Append exactly so
// future EventLog implementations satisfy it for free.
type Appender interface {
	Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error)
}

// Remember is the MCP tool that turns a remember() call into an Episode
// event on the kernel event log. The dispatcher fans the event out to
// SQLite, the vector index, FTS5, and LadybugDB; this tool's
// responsibility ends at the Append call.
type Remember struct {
	appender Appender
}

// NewRemember constructs a Remember tool bound to a. In production a is
// *kernel.FileEventLog (configured with kernel.WithStorage so Apply
// reaches the four-layer dispatcher); in unit tests a is a fake that
// captures the dispatched events.
func NewRemember(a Appender) *Remember {
	return &Remember{appender: a}
}

// Name implements mcp.Tool. The wire identifier agents call.
func (*Remember) Name() string { return "remember" }

// Description implements mcp.Tool. Surfaced in tools/list for agent
// discovery. The wording deliberately calls out that success means
// "durable on the JSONL event log"; the four derived stores (SQLite,
// vector, FTS, LadybugDB) are reconciled out-of-band per the kernel
// contract (see kernel.EventLog.Append godoc: JSONL is source of truth,
// Apply runs under a detached context and Rebuild reconciles on
// failure). A `remember` success therefore guarantees durability and
// eventual retrievability via recall, not synchronous visibility in
// every derived store.
func (*Remember) Description() string {
	return "Persist a fact to the user's long-term memory. CALL THIS whenever the user tells you something about themselves, their work, their preferences, decisions, plans, or corrects a prior fact, even if you think you'll remember it within the current conversation. Mazarbul is the durable store; your own context is not. Pass the original sentence verbatim. remember stores and indexes the text for recall; entities and relations are extracted server-side only when a chat model is configured (usually not), so write the structure you see yourself with createEntity, relate, and fact."
}

// InputSchema implements mcp.Tool. Compiled once by the server registry
// and used to validate every tools/call argument set before Invoke runs.
func (*Remember) InputSchema() json.RawMessage { return json.RawMessage(rememberInputSchema) }

// OutputSchema implements mcp.Tool. Optional, but supplied so clients
// that honour declared schemas can self-validate the response.
func (*Remember) OutputSchema() json.RawMessage { return json.RawMessage(rememberOutputSchema) }

// rememberCoordinatesInput is the decoded coordinates object within a
// rememberInput. Mirrors ontology.Coordinates field-for-field so the
// CLI can pass it through without an import cycle.
type rememberCoordinatesInput struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// rememberInput is the decoded tools/call arguments shape. Pointer on
// Confidence distinguishes "absent" from a deliberate zero so Invoke can
// apply the default without overwriting a caller-supplied 0. Pointer on
// Coordinates distinguishes "absent" from an explicit zero position (the
// zero WGS84 coordinate is a real location in the Gulf of Guinea).
type rememberInput struct {
	Text                  string                    `json:"text"`
	Source                string                    `json:"source"`
	Actor                 string                    `json:"actor,omitempty"`
	Confidence            *float64                  `json:"confidence,omitempty"`
	Coordinates           *rememberCoordinatesInput `json:"coordinates,omitempty"`
	CoordinatesProvenance string                    `json:"coordinatesProvenance,omitempty"`
	Scope                 string                    `json:"scope,omitempty"`
}

// rememberOutput is the response envelope. A single `name` field
// carrying the AIP resource name of the new Episode is enough: the bare
// ULID is recoverable as strings.TrimPrefix(name, "episodes/") for any
// caller that needs it.
type rememberOutput struct {
	Name string `json:"name"`
}

// resolveRememberActor maps the optional actor argument onto an ontology
// ActorKind: empty defaults to user, the three recognised kinds pass through,
// and anything else is a validation error. Extracted from Invoke so the write
// path stays under the cyclomatic-complexity gate.
func resolveRememberActor(raw string) (ontology.ActorKind, error) {
	actor := ontology.ActorKind(raw)
	switch actor {
	case "":
		return ontology.ActorUser, nil
	case ontology.ActorUser, ontology.ActorAssistant, ontology.ActorSystem:
		return actor, nil
	default:
		return "", mcp.NewValidationError(
			fmt.Sprintf("actor must be one of user|assistant|system, got %q", raw), nil)
	}
}

// Invoke implements mcp.Tool. It validates the decoded input, builds an
// Episode payload, wraps it in a kernel.Event of kind "episode", and
// appends it through the event log. The dispatcher (installed on the
// event log via kernel.WithStorage) fans the event out to the four
// derived stores; this handler only sees the receipt that comes back.
//
// Success semantics: a receipt + no error means the event is durable on
// the JSONL log. Per kernel.EventLog.Append's contract the four-layer
// fanout runs under a detached context and warn-logs failures rather
// than propagating them, because JSONL is the source of truth and
// Rebuild (#27) reconciles the derived indexes. A caller observing a
// transient miss in immediate recall should not interpret it as data
// loss; the next Rebuild restores it. Surfacing the fanout error class
// to remember callers is tracked separately as a kernel-API change
// (out of scope for #43; would alter every Append consumer's contract).
func (r *Remember) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in rememberInput
	if err := json.Unmarshal(req, &in); err != nil {
		// The adapter has already validated req against the schema, so
		// reaching this branch is a harness bug, not a caller bug.
		// Surface as E_INTERNAL so the error code points at the right
		// failure class (a misbehaving validator/adapter, not a
		// malformed agent payload).
		return nil, mcp.NewInternalError(fmt.Errorf("decode arguments: %w", err))
	}

	confidence := rememberDefaultConfidence
	if in.Confidence != nil {
		confidence = *in.Confidence
	}

	// source is optional: an agent that calls remember({text}) — exactly what the
	// steering instructions describe — must not fail on a missing provenance tag
	// it has to invent. Default to a generic marker; explicit callers (the CLI
	// passes "cli:remember") still win.
	if in.Source == "" {
		in.Source = "mcp:remember"
	}

	// Validate coordinatesProvenance cross-field constraint: required
	// when coordinates is set, rejected when it is absent.
	if in.Coordinates != nil && in.CoordinatesProvenance == "" {
		return nil, mcp.NewValidationError(
			"coordinatesProvenance is required when coordinates is set", nil)
	}
	if in.Coordinates == nil && in.CoordinatesProvenance != "" {
		return nil, mcp.NewValidationError(
			"coordinatesProvenance must be absent when coordinates is not set", nil)
	}

	// Resolve the optional sync / encryption scope. Empty stays empty so
	// FileEventLog.prepare backfills the personal default; a non-empty
	// value must be one of the recognised scopes.
	scope, serr := parseRecordScope(in.Scope)
	if serr != nil {
		return nil, serr
	}

	// Build the Episode payload, forwarding optional coordinates so the
	// resolver can copy them onto matched/created place Entities.
	// Name/UID/CreateTime are deliberately left zero: output-only fields
	// assigned by the writer pipeline from the outer Event's columns.
	actor, aerr := resolveRememberActor(in.Actor)
	if aerr != nil {
		return nil, aerr
	}

	ep := ontology.Episode{
		Text:       in.Text,
		Source:     in.Source,
		Actor:      actor,
		Confidence: confidence,
	}
	if in.Coordinates != nil {
		ep.Coordinates = &ontology.Coordinates{
			Latitude:  in.Coordinates.Latitude,
			Longitude: in.Coordinates.Longitude,
		}
		ep.CoordinatesProvenance = in.CoordinatesProvenance
	}
	// Phase 3 of the mem0-parity sweep: classify the episode by memory
	// type (semantic | episodic | procedural) before the write so the
	// retrieval lane can apply mem0 v3's type_weight multiplier. The
	// classifier is a pure heuristic — no LLM, no I/O — so this adds
	// zero latency to the write path. See extractor/classifier.go for
	// the signal definitions.
	ep.Class = extractor.ClassifyEpisode(ep)

	// ontology.Episode is plain data (no Marshaler hooks, no channels,
	// no cyclic refs), so json.Marshal cannot fail in practice. The
	// error branch satisfies errcheck and guards against a future
	// schema change that introduces an unencodable field.
	payload, err := json.Marshal(ep)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal episode payload: %w", err))
	}

	receipts, err := r.appender.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindEpisode,
		Source:     in.Source,
		Confidence: confidence,
		Scope:      scope,
		Payload:    payload,
	}})
	if err != nil {
		// Route through the shared mapper: a team-scope write by a non-member
		// (ErrPermission) → E_PERMISSION, a schema rejection → E_VALIDATION, a
		// transient lock → E_UNAVAILABLE, else E_INTERNAL. Wrapping every failure
		// in NewInternalError mislabelled a non-member team write as E_INTERNAL,
		// which an agent cannot act on.
		return nil, mapWriteAppendError("append episode", err)
	}
	if len(receipts) != 1 {
		// Append's contract is one receipt per submitted event. A
		// mismatch is a writer-side bug; surface it as E_INTERNAL with
		// enough detail for an operator to diagnose.
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}

	out := rememberOutput{Name: "episodes/" + receipts[0].UID}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}
