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

// forgetInputSchema is the JSON Schema for the tools/call arguments.
// Exactly one of {episodeUid, relationUid, factUid} is required. The three
// modes are mutually exclusive: episodeUid cascades through mentions,
// provenance, the episode_vec row, relations sourced by the episode,
// the LadybugDB Episode node, and any RELATES_TO edges those relations
// produced. RelationUid is the surgical drop — the SQLite relations row
// plus the matching RELATES_TO graph edge — leaving the source episode
// intact. FactUid hard-deletes a single Fact (a state/preference slot
// value) and reconciles the slot it occupied; use retractFact instead to
// close a fact while preserving its history. A future revision will add a
// free-text `query` field that runs a single-shot recall + resolve so
// agents can call forget("the thing about X") without first running a recall.
const forgetInputSchema = `{
  "type": "object",
  "properties": {
    "episodeUid":  {"type": "string", "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$", "description": "ULID of the Episode to forget. Mutually exclusive with relationUid/factUid."},
    "relationUid": {"type": "string", "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$", "description": "ULID of a single Relation to forget. Mutually exclusive with episodeUid/factUid."},
    "factUid":     {"type": "string", "pattern": "^[0-9A-HJKMNP-TV-Z]{26}$", "description": "ULID of a single Fact (a state/preference slot value) to hard-delete. Mutually exclusive with episodeUid/relationUid. To close a fact while keeping its history, use retractFact instead."},
    "reason":      {"type": "string", "description": "Optional free-text reason for the forget; surfaced in audit/log views."}
  },
  "oneOf": [
    {"required": ["episodeUid"]},
    {"required": ["relationUid"]},
    {"required": ["factUid"]}
  ],
  "additionalProperties": false
}`

const forgetOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "pattern": "^(episodes|relations|facts)/[0-9A-HJKMNP-TV-Z]{26}$", "description": "AIP resource name of the forgotten Episode, Relation, or Fact."},
    "status": {"type": "string", "enum": ["forgotten"], "description": "Always \"forgotten\" on success."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// Forget is the MCP tool that appends an EpisodeForgotten tombstone to
// the event log. The dispatcher's apply path removes the derived rows
// (episodes, mentions, provenance, episodes_vec, LadybugDB Episode node).
// The JSONL log retains both the original Episode and the tombstone, so
// `maz rebuild` replays both and arrives at the same end state
// deterministically.
type Forget struct {
	appender Appender
	checker  existenceChecker // nil ⇒ existence unchecked (degraded / test-fake wiring)
}

// existenceChecker is the read-side dependency Forget uses to confirm a target
// actually exists before writing its tombstone. Satisfied by *navigate.Navigator
// (via the tools.Navigator surface). Optional so minimal-boot and test-fake
// wiring — which have no navigator — still construct the tool; existence is then
// unchecked, matching the pre-existence-check behaviour.
type existenceChecker interface {
	GetEpisode(ctx context.Context, uid string) (navigate.Episode, error)
	GetRelation(ctx context.Context, uid string) (navigate.Relation, error)
	GetFact(ctx context.Context, uid string) (navigate.Fact, error)
}

// NewForget constructs a Forget tool bound to a.
func NewForget(a Appender) *Forget {
	return &Forget{appender: a}
}

// SetExistenceChecker wires the read-side existence guard. Called by the daemon
// wiring once the Navigator is built, so `forget` on a non-existent episode or
// relation returns E_NOT_FOUND instead of fabricating a tombstone for a record
// that never existed (and reporting "forgotten").
func (f *Forget) SetExistenceChecker(c existenceChecker) { f.checker = c }

// Name implements mcp.Tool.
func (*Forget) Name() string { return "forget" }

// Description implements mcp.Tool.
func (*Forget) Description() string {
	return "Forget an Episode, single Relation, or single Fact by ULID: appends a tombstone that removes derived state across the storage layers while preserving the JSONL audit trail. factUid hard-deletes a fact (state/preference) slot value — use retractFact to close a fact while keeping its history."
}

// InputSchema implements mcp.Tool.
func (*Forget) InputSchema() json.RawMessage { return json.RawMessage(forgetInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Forget) OutputSchema() json.RawMessage { return json.RawMessage(forgetOutputSchema) }

type forgetInput struct {
	EpisodeUID  string `json:"episodeUid,omitempty"`
	RelationUID string `json:"relationUid,omitempty"`
	FactUID     string `json:"factUid,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type forgetOutput struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Invoke implements mcp.Tool. Exactly one of episodeUid, relationUid, or
// factUid must be set; zero or more than one is a validation error so the
// dispatch is unambiguous.
func (f *Forget) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in forgetInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("decode arguments: %w", err))
	}
	n := 0
	for _, set := range []bool{in.EpisodeUID != "", in.RelationUID != "", in.FactUID != ""} {
		if set {
			n++
		}
	}
	switch {
	case n == 0:
		return nil, mcp.NewValidationError("exactly one of episodeUid, relationUid, or factUid is required", nil)
	case n > 1:
		return nil, mcp.NewValidationError("episodeUid, relationUid, and factUid are mutually exclusive", nil)
	}

	switch {
	case in.EpisodeUID != "":
		return f.invokeEpisode(ctx, in)
	case in.RelationUID != "":
		return f.invokeRelation(ctx, in)
	default:
		return f.invokeFact(ctx, in)
	}
}

func (f *Forget) invokeEpisode(ctx context.Context, in forgetInput) (json.RawMessage, error) {
	// Confirm the episode exists before tombstoning it. Without this, forgetting
	// a bogus or wrong-kind uid (e.g. an entity ULID) fabricated an
	// episodeForgotten event for a record that never existed and reported
	// "forgotten" — a destructive command lying about a no-op while polluting the
	// append-only log.
	if f.checker != nil {
		if _, err := f.checker.GetEpisode(ctx, in.EpisodeUID); err != nil {
			if errors.Is(err, navigate.ErrNotFound) {
				return nil, mcp.NewNotFoundError("episodes/" + in.EpisodeUID + " not found; nothing to forget")
			}
			return nil, mcp.NewInternalError(fmt.Errorf("forget: verify episode: %w", err))
		}
	}
	payload, err := json.Marshal(ontology.EpisodeForgottenEvent{
		EpisodeUID:  in.EpisodeUID,
		Reason:      in.Reason,
		ForgottenAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal forget payload: %w", err))
	}
	receipts, err := f.appender.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindEpisodeForgotten,
		Source:     "mcp:forget",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("append forget: %w", err))
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(fmt.Errorf("eventlog: append returned %d receipts, want 1", len(receipts)))
	}
	body, err := json.Marshal(forgetOutput{
		Name:   "episodes/" + in.EpisodeUID,
		Status: "forgotten",
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

func (f *Forget) invokeRelation(ctx context.Context, in forgetInput) (json.RawMessage, error) {
	// Confirm the relation exists before tombstoning it (see invokeEpisode).
	if f.checker != nil {
		if _, err := f.checker.GetRelation(ctx, in.RelationUID); err != nil {
			if errors.Is(err, navigate.ErrNotFound) {
				return nil, mcp.NewNotFoundError("relations/" + in.RelationUID + " not found; nothing to forget")
			}
			return nil, mcp.NewInternalError(fmt.Errorf("forget: verify relation: %w", err))
		}
	}
	payload, err := json.Marshal(ontology.RelationForgottenEvent{
		RelationUID: in.RelationUID,
		Reason:      in.Reason,
		ForgottenAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal forget payload: %w", err))
	}
	receipts, err := f.appender.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindRelationForgotten,
		Source:     "mcp:forget",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("append forget: %w", err))
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(fmt.Errorf("eventlog: append returned %d receipts, want 1", len(receipts)))
	}
	body, err := json.Marshal(forgetOutput{
		Name:   "relations/" + in.RelationUID,
		Status: "forgotten",
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

func (f *Forget) invokeFact(ctx context.Context, in forgetInput) (json.RawMessage, error) {
	// Confirm the fact exists before tombstoning it (see invokeEpisode).
	if f.checker != nil {
		if _, err := f.checker.GetFact(ctx, in.FactUID); err != nil {
			if errors.Is(err, navigate.ErrNotFound) {
				return nil, mcp.NewNotFoundError("facts/" + in.FactUID + " not found; nothing to forget")
			}
			return nil, mcp.NewInternalError(fmt.Errorf("forget: verify fact: %w", err))
		}
	}
	payload, err := json.Marshal(ontology.FactForgottenEvent{
		FactUID:     in.FactUID,
		Reason:      in.Reason,
		ForgottenAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal forget payload: %w", err))
	}
	receipts, err := f.appender.Append(ctx, []kernel.Event{{
		Kind:       ontology.KindFactForgotten,
		Source:     "mcp:forget",
		Confidence: 1.0,
		Payload:    payload,
	}})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("append forget: %w", err))
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(fmt.Errorf("eventlog: append returned %d receipts, want 1", len(receipts)))
	}
	body, err := json.Marshal(forgetOutput{
		Name:   "facts/" + in.FactUID,
		Status: "forgotten",
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}
