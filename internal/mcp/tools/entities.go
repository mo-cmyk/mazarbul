package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// Sort orders for the entities tool.
const (
	entitiesSortRecent      = "recent"
	entitiesSortRecallCount = "recallCount"
)

// entitiesDefaultLimit caps an unfiltered listing.
const entitiesDefaultLimit = 50

// entitiesInputSchema is the JSON Schema for the tools/call arguments. All
// filters are optional; with none set it returns the most recent entities.
// SortBy=recallCount subsumes the former listTopEntities tool.
const entitiesInputSchema = `{
  "type": "object",
  "properties": {
    "kind":          {"type": "string", "description": "Filter to one entity kind, e.g. person, place, organization. See introspect for the kinds present."},
    "nameLike":      {"type": "string", "description": "Case-insensitive substring match on display name."},
    "nameExact":     {"type": "string", "description": "Case-insensitive exact match on display name (unbounded; takes precedence over nameLike). Use this to resolve a known name to its entity."},
    "filter":        {"type": "string", "description": "AIP-160 filter expression, AND-combined with the other params. Reference entity columns (kind, displayName, confidence, resolutionStatus, createTime), the short alias status for resolutionStatus (values: resolved, confirmed, tentative, split_candidate, deleted), an entity's fact slots by state_key, and related(\"predicate\", \"target\"). Operators: = != < > <= >= : (substring) with AND / OR / NOT and parentheses. OR binds tighter than AND. Example: kind = \"person\" AND status = \"resolved\" AND related(\"works_at\", \"Mithril\"). Applies to the default sortBy=recent."},
    "minConfidence": {"type": "number", "minimum": 0, "maximum": 1, "description": "Drop entities below this resolution confidence."},
    "limit":         {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum entities to return. Defaults to 50."},
    "offset":        {"type": "integer", "minimum": 0, "description": "Pagination offset (recent sort only). Defaults to 0."},
    "sortBy":        {"type": "string", "enum": ["recent", "recallCount"], "description": "Ordering. \"recent\" (default) = newest first. \"recallCount\" = most-recalled first (requires usage stats)."},
    "window":        {"type": "string", "enum": ["today", "last7Days", "last30Days", "lifetime"], "description": "Aggregation window for sortBy=recallCount. Defaults to last7Days."},
    "scopes":        {"type": "array", "items": {"type": "string"}, "description": "Scopes to read from: project, personal, team. Defaults to project+personal. Unknown values are rejected."}
  },
  "additionalProperties": false
}`

// entitiesOutputSchema documents the response. RecallCount is present only
// under sortBy=recallCount.
const entitiesOutputSchema = `{
  "type": "object",
  "properties": {
    "entities": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name":             {"type": "string", "description": "Resource name entities/{uid}; pass to get/neighbors/relations."},
          "uid":              {"type": "string"},
          "kind":             {"type": "string"},
          "displayName":      {"type": "string"},
          "confidence":       {"type": "number"},
          "resolutionStatus": {"type": "string"},
          "createTime":       {"type": "string"},
          "recallCount":      {"type": "integer", "minimum": 0, "description": "Times recalled in the window. Present only for sortBy=recallCount."}
        },
        "required": ["name", "uid", "kind", "displayName", "confidence", "resolutionStatus"],
        "additionalProperties": false
      }
    }
  },
  "required": ["entities"],
  "additionalProperties": false
}`

// Entities lists and filters entities, LLM-free. It is the discovery surface:
// an agent that does not yet have an anchor lists by kind / name to find one,
// then pivots into neighbors/relations. SortBy=recallCount folds in the former
// listTopEntities tool (most-recalled first), backed by the usage stats store.
type Entities struct {
	nav   Navigator
	stats statsTopStore // optional; nil disables sortBy=recallCount
}

// statsTopStore is the slice of stats.Store the entities tool needs for the
// recall-count sort. Narrowed to an interface so tests can inject a stub;
// *the production stats.Store satisfies it.
type statsTopStore interface {
	Top(ctx context.Context, n int, w stats.Window) ([]stats.TopItem, error)
}

// NewEntities constructs an Entities tool. Stats may be nil; when nil,
// sortBy=recallCount returns an actionable validation error rather than failing
// obscurely.
func NewEntities(nav Navigator, stats statsTopStore) *Entities {
	return &Entities{nav: nav, stats: stats}
}

// Name implements mcp.Tool.
func (*Entities) Name() string { return "entities" }

// Description implements mcp.Tool.
func (*Entities) Description() string {
	return "List and filter entities by kind, name substring, and confidence, with pagination. LLM-free discovery surface: find an anchor here, then pivot into neighbors/relations. sortBy=recallCount returns the most-recalled entities (replaces listTopEntities). Spot a duplicate of the same real-world thing? Consolidate the pair with mergeEntities (lossless); remove a junk entity with deleteEntity."
}

// InputSchema implements mcp.Tool.
func (*Entities) InputSchema() json.RawMessage { return json.RawMessage(entitiesInputSchema) }

// OutputSchema implements mcp.Tool.
func (*Entities) OutputSchema() json.RawMessage { return json.RawMessage(entitiesOutputSchema) }

type entitiesInput struct {
	Kind          string   `json:"kind,omitempty"`
	NameLike      string   `json:"nameLike,omitempty"`
	NameExact     string   `json:"nameExact,omitempty"`
	Filter        string   `json:"filter,omitempty"`
	MinConfidence *float64 `json:"minConfidence,omitempty"`
	Limit         *int     `json:"limit,omitempty"`
	Offset        *int     `json:"offset,omitempty"`
	SortBy        string   `json:"sortBy,omitempty"`
	Window        string   `json:"window,omitempty"`
	Scopes        []string `json:"scopes,omitempty"`
}

// entityListItem is an entityWire plus an optional recall count (set only in
// the recallCount sort). Embedding flattens the entity fields onto the object.
type entityListItem struct {
	entityWire
	RecallCount *int64 `json:"recallCount,omitempty"`
}

type entitiesOutput struct {
	Entities []entityListItem `json:"entities"`
}

func (in entitiesInput) limitOrDefault() int {
	if in.Limit != nil && *in.Limit > 0 {
		// Re-enforce the schema's 200 ceiling for direct (CLI / in-process)
		// callers that bypass the MCP schema validator.
		if *in.Limit > maxNavigateLimit {
			return maxNavigateLimit
		}
		return *in.Limit
	}
	return entitiesDefaultLimit
}

// Invoke implements mcp.Tool.
func (e *Entities) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in entitiesInput
	if len(req) > 0 {
		if err := json.Unmarshal(req, &in); err != nil {
			return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
		}
	}
	// Range-check minConfidence here, not just in the JSON schema: the CLI and
	// in-process paths bypass the schema's [0,1] bound. An out-of-range value
	// would silently empty the result (>1) or disable the filter (<0).
	if in.MinConfidence != nil && (*in.MinConfidence < 0 || *in.MinConfidence > 1) {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("minConfidence must be between 0 and 1, got %v", *in.MinConfidence), nil)
	}
	// The recall-count sort hydrates from the usage-stats store and applies only
	// kind/confidence during hydration; the structured filter and the name
	// filters cannot post-filter that ranked set coherently. Reject the
	// combination rather than silently ignoring the unapplied param (a request
	// like {"sortBy":"recallCount","nameExact":"mithril"} would otherwise return
	// the most-recalled entities, not the Mithril match).
	if in.SortBy == entitiesSortRecallCount {
		switch {
		case in.Filter != "":
			return nil, mcp.NewValidationError("filter is only supported with sortBy=recent", nil)
		case in.NameExact != "":
			return nil, mcp.NewValidationError("nameExact is only supported with sortBy=recent", nil)
		case in.NameLike != "":
			return nil, mcp.NewValidationError("nameLike is only supported with sortBy=recent", nil)
		}
	}

	scopeSet, serr := scopesFromParam(in.Scopes)
	if serr != nil {
		return nil, serr
	}
	nav := navWithScopes(e.nav, scopeSet)

	var (
		items []entityListItem
		err   error
	)
	// Validate sortBy here, not just in the JSON schema: the CLI and in-process
	// dispatch call Invoke directly and bypass the schema enum. A typo like
	// "recall-count" would otherwise fall through to listRecent and silently
	// return newest-first without recall counts under the wrong label.
	switch in.SortBy {
	case "", entitiesSortRecent:
		items, err = e.listRecentNav(ctx, nav, in)
	case entitiesSortRecallCount:
		items, err = e.listByRecallCountNav(ctx, nav, in)
	default:
		return nil, mcp.NewValidationError(
			fmt.Sprintf("unknown sortBy %q; expected recent or recallCount", in.SortBy), nil)
	}
	if err != nil {
		return nil, err
	}

	body, mErr := json.Marshal(entitiesOutput{Entities: items})
	if mErr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", mErr))
	}
	return body, nil
}

// listRecentNav returns entities newest-first via the Navigator's structural
// filter using the supplied (scope-bounded) navigator. No recall counts are attached.
func (e *Entities) listRecentNav(ctx context.Context, nav Navigator, in entitiesInput) ([]entityListItem, error) {
	filter := navigate.EntityFilter{
		Kind:      in.Kind,
		NameLike:  in.NameLike,
		NameExact: in.NameExact,
		Filter:    in.Filter,
		Limit:     in.limitOrDefault(),
	}
	if in.MinConfidence != nil {
		filter.MinConfidence = *in.MinConfidence
	}
	if in.Offset != nil {
		filter.Offset = *in.Offset
	}
	ents, err := nav.ListEntities(ctx, filter)
	if err != nil {
		return nil, navQueryError("list entities", err)
	}
	out := make([]entityListItem, 0, len(ents))
	for _, ent := range ents {
		out = append(out, entityListItem{entityWire: projectEntity(ent)})
	}
	return out, nil
}

// listByRecallCountNav returns entities ordered by recall count using the
// supplied (scope-bounded) navigator for hydration.
func (e *Entities) listByRecallCountNav(ctx context.Context, nav Navigator, in entitiesInput) ([]entityListItem, error) {
	return e.listByRecallCountCore(ctx, nav, in)
}

// listByRecallCountCore returns entities ordered by recall count for the window,
// hydrating each via the given Navigator and dropping any kind-filtered out. Requires
// a wired stats store.
func (e *Entities) listByRecallCountCore(ctx context.Context, nav Navigator, in entitiesInput) ([]entityListItem, error) {
	if e.stats == nil {
		return nil, mcp.NewValidationError("sortBy=recallCount requires usage stats, which are not enabled on this server", nil)
	}
	window := in.Window
	if window == "" {
		window = windowLast7Days
	}
	// parseWindow falls back to "today" for an unknown string; reject the typo
	// rather than silently returning today's counts under the wrong label. The
	// MCP enum catches HTTP calls, but in-process/CLI dispatch bypasses it.
	w, canon := parseWindow(window)
	if canon != window {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("unknown window %q; expected one of today, last7Days, last30Days, lifetime", window), nil)
	}

	target := in.limitOrDefault()
	// kind / minConfidence are applied after hydration, so asking the stats store
	// for exactly `target` rows underfills when the top entities are filtered out
	// (e.g. the 10 most-recalled are organizations but the caller wants people).
	// Grow the fetch — deduping by uid — until `target` matching rows are
	// collected or the store is exhausted, capped so a filter that matches
	// nothing cannot scan unbounded.
	const recallScanCap = 4096
	out := make([]entityListItem, 0, target)
	seen := make(map[string]struct{})
	for fetchN := target; len(out) < target; {
		top, err := e.stats.Top(ctx, fetchN, w)
		if err != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("top entities: %w", err))
		}
		batch, err := e.matchingTopEntitiesNav(ctx, nav, top, in, target-len(out), seen)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		// Enough rows, the store returned fewer than asked (exhausted), or we hit
		// the scan ceiling — stop.
		if len(out) >= target || len(top) < fetchN || fetchN >= recallScanCap {
			break
		}
		if fetchN *= 2; fetchN > recallScanCap {
			fetchN = recallScanCap
		}
	}
	return out, nil
}

// matchingTopEntitiesNav scans one Top() batch and returns up to limit rows that
// hydrate successfully and pass the kind/confidence filters. Nav is the
// scope-bounded navigator for per-request isolation. Seen is updated in place
// (keyed by uid) so the caller can grow the fetch across batches without
// double-counting a row. A non-nil error is fatal to the whole listing.
func (e *Entities) matchingTopEntitiesNav(ctx context.Context, nav Navigator, top []stats.TopItem, in entitiesInput, limit int, seen map[string]struct{}) ([]entityListItem, error) {
	out := make([]entityListItem, 0, limit)
	for _, it := range top {
		if len(out) >= limit {
			break
		}
		if _, dup := seen[it.UID]; dup {
			continue
		}
		seen[it.UID] = struct{}{}
		item, keep, err := e.hydrateTopEntityNav(ctx, nav, it, in)
		if err != nil {
			return nil, err
		}
		if keep {
			out = append(out, item)
		}
	}
	return out, nil
}

// hydrateTopEntityNav loads the entity behind a recall-stats row using the
// supplied navigator and applies the post-hydration kind/confidence filters.
// Keep is false (with a nil error) when the row should be skipped.
func (e *Entities) hydrateTopEntityNav(ctx context.Context, nav Navigator, it stats.TopItem, in entitiesInput) (item entityListItem, keep bool, err error) {
	ent, gErr := nav.GetEntity(ctx, it.UID)
	if errors.Is(gErr, navigate.ErrNotFound) {
		return entityListItem{}, false, nil // recalled entity since forgotten
	}
	if gErr != nil {
		return entityListItem{}, false, mcp.NewInternalError(fmt.Errorf("hydrate top entity %q: %w", it.UID, gErr))
	}
	if in.Kind != "" && ent.Kind != strings.ToLower(in.Kind) {
		return entityListItem{}, false, nil
	}
	if in.MinConfidence != nil && ent.Confidence < *in.MinConfidence {
		return entityListItem{}, false, nil
	}
	count := it.RecallCount
	return entityListItem{entityWire: projectEntity(ent), RecallCount: &count}, true, nil
}
