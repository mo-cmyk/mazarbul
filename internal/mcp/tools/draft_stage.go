package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

const draftStageInputSchema = `{
  "type": "object",
  "properties": {
    "view":  {"type": "string", "enum": ["list", "diff"], "description": "\"list\" returns draft-scoped episodes+entities+relations. \"diff\" returns draft and personal sets side by side for comparison before promotion. Defaults to \"list\"."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum number of items per category to return. Defaults to 50."}
  },
  "additionalProperties": false
}`

const draftStageOutputSchema = `{
  "type": "object",
  "properties": {
    "view":     {"type": "string", "description": "The view that was returned: list or diff."},
    "draft":    {"type": "object", "description": "Draft-scoped records."},
    "personal": {"type": "object", "description": "Personal-scoped records (diff view only)."}
  },
  "required": ["view", "draft"],
  "additionalProperties": false
}`

// DraftStage is the read tool for the draft-staging workflow. It returns
// draft-scoped episodes, entities, and relations so an agent or operator can
// review staged content before deciding to promote or discard it.
//
// Two views:
//   - "list": returns only the draft set.
//   - "diff": returns both draft and personal sets so the caller can compare
//     what promotion would add.
//
// Unlisted: this tool backs the `maz scope draft list` and `maz scope draft diff`
// CLI verbs.
type DraftStage struct {
	nav Navigator
}

// NewDraftStage constructs a DraftStage tool.
func NewDraftStage(nav Navigator) *DraftStage {
	return &DraftStage{nav: nav}
}

// Name implements mcp.Tool.
func (*DraftStage) Name() string { return "draftStage" }

// Unlisted hides this tool from tools/list; it backs the CLI surface.
func (*DraftStage) Unlisted() bool { return true }

// Description implements mcp.Tool.
func (*DraftStage) Description() string {
	return "Read the draft staging area: list draft-scoped episodes, entities, and relations " +
		"(view=list) or compare them against the personal scope to preview what promotion would add (view=diff). " +
		"Draft records are invisible to recall until promoted via promoteDraft."
}

// InputSchema implements mcp.Tool.
func (*DraftStage) InputSchema() json.RawMessage {
	return json.RawMessage(draftStageInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*DraftStage) OutputSchema() json.RawMessage {
	return json.RawMessage(draftStageOutputSchema)
}

// draftStage view names.
const (
	draftViewList = "list"
	draftViewDiff = "diff"
)

type draftStageInput struct {
	View  string `json:"view,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// draftSet is the draft-scoped record set returned by draftStage.
type draftSet struct {
	Episodes  []episodeWire  `json:"episodes"`
	Entities  []entityWire   `json:"entities"`
	Relations []relationWire `json:"relations"`
}

type draftStageOutput struct {
	View     string    `json:"view"`
	Draft    draftSet  `json:"draft"`
	Personal *draftSet `json:"personal,omitempty"`
}

// Invoke implements mcp.Tool.
func (d *DraftStage) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in draftStageInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	view := in.View
	if view == "" {
		view = draftViewList
	}
	if view != draftViewList && view != draftViewDiff {
		return nil, mcp.NewValidationError(
			fmt.Sprintf("view must be %q or %q, got %q", draftViewList, draftViewDiff, view), nil)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > maxNavigateLimit {
		limit = maxNavigateLimit
	}

	draftNav := navWithScopes(d.nav, kernel.ScopeDraftReadSet())
	draft, err := readDraftSet(ctx, draftNav, limit)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("read draft set: %w", err))
	}

	out := draftStageOutput{View: view, Draft: draft}

	if view == draftViewDiff {
		personalNav := navWithScopes(d.nav, kernel.NewScopeSet(kernel.ScopePersonal))
		personal, perr := readDraftSet(ctx, personalNav, limit)
		if perr != nil {
			return nil, mcp.NewInternalError(fmt.Errorf("read personal set: %w", perr))
		}
		out.Personal = &personal
	}

	body, merr := json.Marshal(out)
	if merr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", merr))
	}
	return body, nil
}

// readDraftSet reads episodes, entities, and relations from nav (already scoped).
func readDraftSet(ctx context.Context, nav Navigator, limit int) (draftSet, error) {
	var ds draftSet

	eps, err := nav.ListEpisodes(ctx, navigate.EpisodeFilter{Limit: limit})
	if err != nil {
		return draftSet{}, fmt.Errorf("list episodes: %w", err)
	}
	ds.Episodes = make([]episodeWire, 0, len(eps))
	for _, ep := range eps {
		ds.Episodes = append(ds.Episodes, projectEpisode(ep))
	}

	ents, err := nav.ListEntities(ctx, navigate.EntityFilter{Limit: limit})
	if err != nil {
		return draftSet{}, fmt.Errorf("list entities: %w", err)
	}
	ds.Entities = make([]entityWire, 0, len(ents))
	for _, e := range ents {
		ds.Entities = append(ds.Entities, projectEntity(e))
	}

	// Relations: list by walking the entities we found. Use the first entity
	// as an anchor; if none, the relation slice stays empty (a valid draft may
	// have only episodes before extraction runs).
	ds.Relations = []relationWire{}
	seen := make(map[string]bool)
	for _, e := range ents {
		rels, rerr := nav.EntityRelations(ctx, e.UID, navigate.RelationFilter{Limit: limit})
		if rerr != nil {
			return draftSet{}, fmt.Errorf("list relations for %s: %w", e.UID, rerr)
		}
		for _, r := range rels {
			if !seen[r.UID] {
				seen[r.UID] = true
				ds.Relations = append(ds.Relations, projectRelation(r))
			}
		}
	}

	return ds, nil
}
