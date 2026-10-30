package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

const askDefaultLimit = 10

const askInputSchema = `{
  "type": "object",
  "properties": {
    "question": {"type": "string", "minLength": 1, "description": "Natural-language question to answer from the event log."},
    "kinds":    {"type": "array", "items": {"type": "string"}, "description": "Restrict the evidence to these record kinds (set semantics). Omit for the default mix of episodes + vault documents. Pass [\"documentRevision\"] to answer from documents only, [\"episode\"] to exclude documents."},
    "limit":    {"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum number of hits to feed the composer. Defaults to 10."},
    "at":       {"type": "string", "format": "date-time", "description": "Optional RFC 3339 UTC instant to scope the answer. Bi-temporal filter (valid-time anchor)."},
    "asOf":     {"type": "string", "format": "date-time", "description": "Optional RFC 3339 UTC instant. Hides any event whose transaction time is later than this anchor, answering 'what did we know on X?'. Bi-temporal filter (transaction-time anchor)."},
    "currentDate": {"type": "string", "description": "Caller's \"now\" anchor (RFC 3339 UTC or YYYY-MM-DD) the rewriter uses to resolve relative phrases like \"two months ago\" into an absolute timestamp. Folded into 'at' only when 'at' is unset. Requires rewriteQuery=true to take effect."},
    "noGraph":  {"type": "boolean", "description": "Skip the graph traversal lane when true (vector + FTS only)."},
    "rewriteQuery": {"type": "boolean", "description": "When true, expand the question via the query-rewrite-v1 LLM prompt before retrieval. Off by default."},
    "near": {
      "type": "object",
      "description": "Restrict recall to episodes citing entities within a geographic radius of a named place. Both fields required when present.",
      "properties": {
        "placeQuery": {"type": "string", "minLength": 1, "description": "Name of the place to resolve, e.g. Berlin. FTS + name-vector lookup."},
        "radiusKm":   {"type": "number", "exclusiveMinimum": 0, "description": "Search radius in kilometres. Must be greater than 0."}
      },
      "required": ["placeQuery", "radiusKm"],
      "additionalProperties": false
    }
  },
  "required": ["question"],
  "additionalProperties": false
}`

const askOutputSchema = `{
  "type": "object",
  "properties": {
    "answer":    {"type": "string"},
    "entities":  {"type": "array"},
    "evidence":  {"type": "array"},
    "citations": {"type": "array", "description": "Numbered evidence for the [N] markers in answer. Each entry carries episodeUid + snippet + createTime; citations into vault documents additionally carry path (vault-relative, e.g. notes/foo.md) and heading - render them as path#heading."}
  },
  "required": ["answer", "citations"],
  "additionalProperties": false
}`

// Ask is the MCP tool that runs hybrid recall, calls the composer, and returns
// a grounded Answer with citations. Mirrors Recall but adds the composer step.
type Ask struct {
	q        kernel.Query
	composer *composer.Composer
	// nowFn supplies the wall clock used when the caller does not pass
	// a currentDate field in the request. Tests pin it; production
	// defaults to time.Now().UTC() so the TODAY'S DATE block always
	// renders with a real anchor (without this, "how long ago" /
	// "how many weeks ago" questions abstained because the compose-v1
	// prompt's TODAY'S DATE block is conditional on a non-zero anchor).
	nowFn func() time.Time
}

// NewAsk constructs an Ask tool bound to q (typically *kernel.HybridQuery) and
// c (the composer wired in daemon.OpenWiring).
func NewAsk(q kernel.Query, c *composer.Composer) *Ask {
	return &Ask{q: q, composer: c, nowFn: func() time.Time { return time.Now().UTC() }}
}

// WithNowFn overrides the wall-clock seam used to populate the
// TODAY'S DATE anchor. Tests use this to pin the clock; production
// callers should not invoke it.
func (a *Ask) WithNowFn(nowFn func() time.Time) *Ask {
	if nowFn != nil {
		a.nowFn = nowFn
	}
	return a
}

// Name implements mcp.Tool.
func (*Ask) Name() string { return "ask" }

// Unlisted implements mcp.UnlistedTool: ask is hidden from tools/list so
// external agents stay on the LLM-free navigation verbs (recall, get,
// neighbors, relations, …) rather than spending the daemon's own LLM on answer
// composition. It remains dispatchable by name, so `maz ask` — which routes a
// tools/call through the daemon — keeps working.
func (*Ask) Unlisted() bool { return true }

// Description implements mcp.Tool.
func (*Ask) Description() string {
	return "Compose a grounded answer from the user's long-term memory, with citations. PREFER THIS over recall+manual synthesis when the user asks anything about themselves, their history, decisions, or anything they previously told you — Mazarbul handles retrieval, currency (superseded facts), and citation linking that your conversation context cannot. The answer cites ULIDs from the event log; quote them so the user can verify."
}

// InputSchema implements mcp.Tool.
func (*Ask) InputSchema() json.RawMessage { return json.RawMessage(askInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Ask) OutputSchema() json.RawMessage { return json.RawMessage(askOutputSchema) }

// askNearInput is the decoded near filter within askInput. Mirrors
// kernel.NearFilter field-for-field.
type askNearInput struct {
	PlaceQuery string  `json:"placeQuery"`
	RadiusKm   float64 `json:"radiusKm"`
}

type askInput struct {
	Question     string        `json:"question"`
	Kinds        []string      `json:"kinds,omitempty"`
	Limit        *int          `json:"limit,omitempty"`
	At           *string       `json:"at,omitempty"`
	AsOf         *string       `json:"asOf,omitempty"`
	CurrentDate  *string       `json:"currentDate,omitempty"`
	NoGraph      *bool         `json:"noGraph,omitempty"`
	RewriteQuery *bool         `json:"rewriteQuery,omitempty"`
	Near         *askNearInput `json:"near,omitempty"`
}

// Invoke implements mcp.Tool.
func (a *Ask) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in askInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}

	rreq, perr := buildAskRecallRequest(in)
	if perr != nil {
		return nil, perr
	}

	hits, err := collectHits(ctx, a.q, rreq)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("ask: recall: %w", err))
	}

	// Default the TODAY'S DATE anchor to the wall clock when the caller
	// did not pass one. Without this, "how long ago" / "how many weeks
	// ago" questions land with composeCurrentDate=time.Time{}, the
	// compose-v1 template's conditional TODAY'S DATE block is omitted,
	// and the LLM has no reference for "now" — it then either guesses
	// or abstains.
	composeCurrentDate := a.nowFn()
	if rreq.CurrentDate != nil {
		composeCurrentDate = *rreq.CurrentDate
	}
	answer, err := a.composer.ComposeWithCurrentDate(ctx, in.Question, hits, composeCurrentDate)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("ask: compose: %w", err))
	}

	body, err := json.Marshal(answer)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("ask: marshal: %w", err))
	}
	return body, nil
}

// buildAskRecallRequest projects the parsed askInput into a kernel.RecallRequest.
// Extracted out of Invoke so the cyclomatic-complexity budget stays under the
// project ceiling after the currentDate plumbing landed; the input has six
// optional fields, each of which contributes a branch.
func buildAskRecallRequest(in askInput) (kernel.RecallRequest, error) {
	limit := askDefaultLimit
	if in.Limit != nil {
		limit = *in.Limit
	}
	rreq := kernel.RecallRequest{Query: in.Question, Limit: limit, Kinds: toEventKinds(in.Kinds)}

	if in.At != nil && *in.At != "" {
		t, perr := time.Parse(time.RFC3339Nano, *in.At)
		if perr != nil {
			return kernel.RecallRequest{}, mcp.NewValidationError(fmt.Sprintf("parse 'at': %v", perr), nil)
		}
		t = t.UTC()
		rreq.At = &t
	}
	if in.AsOf != nil && *in.AsOf != "" {
		t, perr := time.Parse(time.RFC3339Nano, *in.AsOf)
		if perr != nil {
			return kernel.RecallRequest{}, mcp.NewValidationError(fmt.Sprintf("parse 'asOf': %v", perr), nil)
		}
		t = t.UTC()
		rreq.AsOf = &t
	}
	if in.CurrentDate != nil && *in.CurrentDate != "" {
		t, perr := parseCurrentDate(*in.CurrentDate)
		if perr != nil {
			return kernel.RecallRequest{}, mcp.NewValidationError(fmt.Sprintf("parse 'currentDate': %v", perr), nil)
		}
		rreq.CurrentDate = &t
	}
	if in.NoGraph != nil {
		rreq.NoGraph = *in.NoGraph
	}
	if in.RewriteQuery != nil {
		rreq.RewriteQuery = *in.RewriteQuery
	}
	if in.Near != nil {
		rreq.Near = &kernel.NearFilter{
			PlaceQuery: in.Near.PlaceQuery,
			RadiusKm:   in.Near.RadiusKm,
		}
	}
	return rreq, nil
}

// collectHits drains the kernel.Query iterator into a slice of raw kernel.Hit.
// Shared by both ask and recall; it returns the original hit shape rather than
// an MCP projection so each caller can render its own (ask's answer synthesis,
// recall's verbose/compact envelopes) from the same retrieval pass.
func collectHits(ctx context.Context, q kernel.Query, req kernel.RecallRequest) ([]kernel.Hit, error) {
	hits := make([]kernel.Hit, 0, req.Limit)
	for hit, err := range q.Recall(ctx, req) {
		if err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, nil
}
