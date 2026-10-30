package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// Insight states the Insight Inbox tools filter and report on. "hot" is the
// live, recall-visible state; "demoted" is the decay-sweep projection; and
// "dismissed" is the terminal, user-initiated state an insightDismissed event
// produces. The "all" value (insightStateAll) is the listInsights sentinel
// meaning "every state".
const (
	insightStateHot       = "hot"
	insightStateDemoted   = "demoted"
	insightStateDismissed = "dismissed"
	insightStateAll       = "all"
)

// listInsightsDefaultLimit caps an unbounded inbox listing, mirroring the
// navigator's defaultLimit so the CLI and tool agree.
const listInsightsDefaultLimit = 50

// insightNavigator is the subset of the navigator the Insight Inbox tools
// consume: the list reader behind listInsights plus the single-row fetch
// dismissInsight uses for its existence check. It is declared narrowly here -
// rather than widening the shared tools.Navigator surface - so a test fake can
// drive the projection + error mapping without implementing every navigation
// read, and so only these two tools depend on ListInsights.
type insightNavigator interface {
	ListInsights(ctx context.Context, state string, limit int) ([]navigate.Insight, error)
	GetInsight(ctx context.Context, uid string) (navigate.Insight, error)
}

// insightNavWithScopes narrows nav to scopes for one request, mirroring
// navWithScopes but over the narrow insightNavigator interface (Go interface
// methods need an exact return type, and *navigate.Navigator.WithScopes returns
// *navigate.Navigator, not insightNavigator). A test fake that does not carry
// scopes is returned unchanged, exactly as navWithScopes does.
func insightNavWithScopes(nav insightNavigator, scopes kernel.ScopeSet) insightNavigator {
	type scopable interface {
		WithScopes(kernel.ScopeSet) *navigate.Navigator
	}
	if s, ok := nav.(scopable); ok {
		return s.WithScopes(scopes)
	}
	return nav
}

// resolveInsightStateFilter maps the listInsights `state` argument to the value
// the navigator reader expects: an empty string means "every state". It is
// validated here, not only in the JSON schema, because the CLI and in-process
// dispatch call Invoke directly and bypass the schema enum.
func resolveInsightStateFilter(state string) (string, error) {
	switch state {
	case "", insightStateHot:
		return insightStateHot, nil
	case insightStateDemoted, insightStateDismissed:
		return state, nil
	case insightStateAll:
		return "", nil
	default:
		return "", mcp.NewValidationError(
			fmt.Sprintf("unknown state %q; expected hot, demoted, dismissed, or all", state), nil)
	}
}

// ---------- listInsights ----------

const listInsightsInputSchema = `{
  "type": "object",
  "properties": {
    "state":  {"type": "string", "enum": ["hot", "demoted", "dismissed", "all"], "description": "Which insights to list. \"hot\" (default) = live, recall-visible insights. \"demoted\" = decayed out of hot recall. \"dismissed\" = explicitly dismissed by the user (terminal). \"all\" = every state."},
    "limit":  {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum insights to return. Defaults to 50."},
    "scopes": {"type": "array", "items": {"type": "string"}, "description": "Scopes to read from: project, personal, team. Defaults to project+personal. Unknown values are rejected."}
  },
  "additionalProperties": false
}`

const listInsightsOutputSchema = `{
  "type": "object",
  "properties": {
    "insights": {
      "type": "array",
      "description": "Insights in the requested state, most-confident first.",
      "items": {
        "type": "object",
        "properties": {
          "name":                  {"type": "string", "description": "Resource name insights/{uid}; pass to get(insights/{uid}) or dismissInsight."},
          "uid":                   {"type": "string"},
          "statement":             {"type": "string", "description": "The synthesized insight text."},
          "insightType":           {"type": "string", "description": "The reflection category, e.g. preference, pattern, summary."},
          "state":                 {"type": "string", "enum": ["hot", "demoted", "dismissed"], "description": "Lifecycle state of the insight."},
          "currentConfidence":     {"type": "number", "description": "Decay-adjusted confidence used for ranking, 0..1."},
          "synthesizedConfidence": {"type": "number", "description": "Confidence at synthesis time, 0..1."},
          "seedEntityUid":         {"type": "string", "description": "The entity the reflection clustered around."},
          "synthesizedAt":         {"type": "string", "description": "When the insight was synthesized (RFC 3339)."},
          "sourceCount":           {"type": "integer", "description": "Number of entity source records the insight cites."},
          "sourceCitations":       {"type": "array", "items": {"type": "string"}, "description": "The cited source records as entities/{uid} resource names; feed straight into get/neighbors/relations."}
        },
        "required": ["name", "uid", "statement", "state", "currentConfidence", "sourceCount", "sourceCitations"]
      }
    }
  },
  "required": ["insights"],
  "additionalProperties": false
}`

// ListInsights lists synthesized insights by lifecycle state - the read half of
// the Insight Inbox. It is the "show me what reflection has produced" surface,
// distinct from get(insights/{uid}) (one record) and recall (which surfaces hot
// insights only as a side effect of a query).
type ListInsights struct{ nav insightNavigator }

// NewListInsights constructs the tool over nav.
func NewListInsights(nav insightNavigator) *ListInsights { return &ListInsights{nav: nav} }

// Name implements mcp.Tool.
func (*ListInsights) Name() string { return "listInsights" }

// Description implements mcp.Tool.
func (*ListInsights) Description() string {
	return "List synthesized insights by lifecycle state - the Insight Inbox read surface. Defaults to the hot (live, recall-visible) insights, most-confident first, each carrying its statement, confidence, lifecycle state, and the entity source records it cites (as entities/{uid} citations plus a count). Pass state=demoted, state=dismissed, or state=all to review the rest. Pair with get(insights/{uid}) to inspect one, dismissInsight to dismiss one, and triggerReflection to synthesize more."
}

// InputSchema implements mcp.Tool.
func (*ListInsights) InputSchema() json.RawMessage { return json.RawMessage(listInsightsInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ListInsights) OutputSchema() json.RawMessage { return json.RawMessage(listInsightsOutputSchema) }

type listInsightsInput struct {
	State  string   `json:"state,omitempty"`
	Limit  *int     `json:"limit,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

// insightListItem is the wire shape for one row of the inbox listing: the
// insight's review-relevant fields plus its source citations typed as
// entities/{uid} resource names and a precomputed count.
type insightListItem struct {
	Name                  string   `json:"name"`
	UID                   string   `json:"uid"`
	Statement             string   `json:"statement"`
	InsightType           string   `json:"insightType,omitempty"`
	State                 string   `json:"state"`
	CurrentConfidence     float64  `json:"currentConfidence"`
	SynthesizedConfidence float64  `json:"synthesizedConfidence"`
	SeedEntityUID         string   `json:"seedEntityUid,omitempty"`
	SynthesizedAt         string   `json:"synthesizedAt,omitempty"`
	SourceCount           int      `json:"sourceCount"`
	SourceCitations       []string `json:"sourceCitations"`
}

type listInsightsOutput struct {
	Insights []insightListItem `json:"insights"`
}

func (in listInsightsInput) limitOrDefault() int {
	if in.Limit != nil && *in.Limit > 0 {
		if *in.Limit > maxNavigateLimit {
			return maxNavigateLimit
		}
		return *in.Limit
	}
	return listInsightsDefaultLimit
}

// Invoke implements mcp.Tool. A bad state or scope is E_VALIDATION.
func (t *ListInsights) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in listInsightsInput
	if len(req) > 0 {
		if err := json.Unmarshal(req, &in); err != nil {
			return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
		}
	}
	stateFilter, verr := resolveInsightStateFilter(in.State)
	if verr != nil {
		return nil, verr
	}
	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}
	insights, err := insightNavWithScopes(t.nav, scopeSet).ListInsights(ctx, stateFilter, in.limitOrDefault())
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("list insights: %w", err))
	}
	items := make([]insightListItem, 0, len(insights))
	for i := range insights {
		items = append(items, projectInsightListItem(insights[i]))
	}
	body, mErr := json.Marshal(listInsightsOutput{Insights: items})
	if mErr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", mErr))
	}
	return body, nil
}

// projectInsightListItem maps a navigate.Insight to the inbox wire item,
// typing its source uids as entities/{uid} citations (insight sources are
// entities from the reflection pass) and stamping the citation count.
func projectInsightListItem(ins navigate.Insight) insightListItem {
	citations := make([]string, 0, len(ins.SourceUIDs))
	for _, s := range ins.SourceUIDs {
		citations = append(citations, collectionEntities+"/"+s)
	}
	return insightListItem{
		Name:                  collectionInsights + "/" + ins.UID,
		UID:                   ins.UID,
		Statement:             ins.Statement,
		InsightType:           ins.InsightType,
		State:                 ins.State,
		CurrentConfidence:     ins.CurrentConfidence,
		SynthesizedConfidence: ins.SynthesizedConfidence,
		SeedEntityUID:         ins.SeedEntityUID,
		SynthesizedAt:         ins.SynthesizedAt,
		SourceCount:           len(ins.SourceUIDs),
		SourceCitations:       citations,
	}
}

// ---------- dismissInsight ----------

const dismissInsightInputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "minLength": 1, "description": "The insight to dismiss: an insights/{uid} resource name (paste a listInsights / recall hit's name) or a bare uid."},
    "reason": {"type": "string", "description": "Optional free-text justification for the dismissal, retained on the event for audit."},
    "scopes": {"type": "array", "items": {"type": "string"}, "description": "Scopes to resolve the insight in: project, personal, team. Defaults to project+personal."}
  },
  "required": ["name"],
  "additionalProperties": false
}`

const dismissInsightOutputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "description": "Resource name insights/{uid} of the dismissed insight."},
    "status": {"type": "string", "enum": ["dismissed"], "description": "Always \"dismissed\" on success."}
  },
  "required": ["name", "status"],
  "additionalProperties": false
}`

// DismissInsight records a user's dismissal of a synthesized insight. It is the
// write half of the Insight Inbox: it appends an insightDismissed event (the
// dispatcher projects it to the terminal state='dismissed'), never an in-place
// UPDATE - the event log stays the source of truth and the decision survives
// `maz rebuild`. Dismissal is distinct from the decay sweep's projection-side
// 'demoted': it is a durable, auditable user choice that reinforce-on-access
// never re-promotes.
type DismissInsight struct {
	appender Appender
	nav      insightNavigator
}

// NewDismissInsight constructs the tool over appender a and the navigator used
// for the existence check.
func NewDismissInsight(a Appender, nav insightNavigator) *DismissInsight {
	return &DismissInsight{appender: a, nav: nav}
}

// Name implements mcp.Tool.
func (*DismissInsight) Name() string { return "dismissInsight" }

// Description implements mcp.Tool.
func (*DismissInsight) Description() string {
	return "Dismiss a synthesized insight the user judges wrong or unhelpful. Appends a durable insightDismissed event (the source of truth) that flips the insight to a terminal 'dismissed' state - it drops out of hot recall and is never re-promoted by access. This is NOT a decay/demotion (which the system does on its own); it records an explicit user decision and survives a rebuild. Identify the insight by insights/{uid} or a bare uid. Returns the dismissed insight's resource name."
}

// InputSchema implements mcp.Tool.
func (*DismissInsight) InputSchema() json.RawMessage {
	return json.RawMessage(dismissInsightInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*DismissInsight) OutputSchema() json.RawMessage {
	return json.RawMessage(dismissInsightOutputSchema)
}

type dismissInsightInput struct {
	Name   string   `json:"name"`
	Reason string   `json:"reason,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

type dismissInsightOutput struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Invoke implements mcp.Tool. A malformed/empty name or wrong-collection
// resource name is E_VALIDATION; a well-formed but unknown (or out-of-scope)
// uid is E_NOT_FOUND; a team-scope permission failure on append maps through
// mapAppendError to E_PERMISSION.
func (t *DismissInsight) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in dismissInsightInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	uid, verr := insightUIDFromArg(in.Name)
	if verr != nil {
		return nil, verr
	}
	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}
	// Existence check (scope-isolated). A well-formed but unknown / out-of-scope
	// uid is E_NOT_FOUND rather than a silent no-op append: the dispatcher's
	// UPDATE no-ops on a missing row, so without this the caller could not tell a
	// real dismissal from a typo. The fetched row also supplies the event's scope.
	ins, gerr := insightNavWithScopes(t.nav, scopeSet).GetInsight(ctx, uid)
	if gerr != nil {
		if errors.Is(gerr, navigate.ErrNotFound) {
			return nil, mcp.NewNotFoundError(fmt.Sprintf("insight %q not found", in.Name))
		}
		return nil, mcp.NewInternalError(fmt.Errorf("get insight %q: %w", uid, gerr))
	}
	// Already dismissed: idempotent no-op. Re-applying the event would converge to
	// the same terminal state, but appending a duplicate only clutters the log.
	if ins.State == insightStateDismissed {
		return marshalDismissResponse(ins.UID)
	}
	payload, err := json.Marshal(ontology.InsightDismissedEvent{
		InsightUID:  ontology.ULID(ins.UID),
		Reason:      in.Reason,
		DismissedAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal insightDismissed: %w", err))
	}
	receipts, aerr := t.appender.Append(ctx, []kernel.Event{{
		Kind:    kernel.EventKind(ontology.KindInsightDismissed),
		Source:  "mcp:dismissInsight",
		Scope:   kernel.Scope(ins.Scope),
		Payload: payload,
	}})
	if aerr != nil {
		return nil, mapAppendError("append insightDismissed", aerr)
	}
	if len(receipts) != 1 {
		return nil, mcp.NewInternalError(errors.New("eventlog: append returned no receipt"))
	}
	return marshalDismissResponse(ins.UID)
}

// marshalDismissResponse builds the {name, status} response for a dismissed uid.
func marshalDismissResponse(uid string) (json.RawMessage, error) {
	body, err := json.Marshal(dismissInsightOutput{Name: collectionInsights + "/" + uid, Status: "dismissed"})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", err))
	}
	return body, nil
}

// insightUIDFromArg accepts an insights/{uid} resource name (the form
// listInsights and recall hand back) or a bare uid, and returns the uid. A name
// carrying any other collection is rejected as E_VALIDATION so an agent that
// passes, e.g., an episodes/{uid} name to dismiss gets a clear error rather
// than a confusing E_NOT_FOUND.
func insightUIDFromArg(arg string) (string, error) {
	if arg == "" {
		return "", mcp.NewValidationError("name is required (insights/{uid} or a bare uid)", nil)
	}
	if !strings.Contains(arg, "/") {
		return arg, nil // bare uid
	}
	collection, uid, err := parseResourceName(arg)
	if err != nil {
		return "", err
	}
	if collection != collectionInsights {
		return "", mcp.NewValidationError(
			fmt.Sprintf("expected an insights/{uid} name, got %q", arg), nil)
	}
	return uid, nil
}
