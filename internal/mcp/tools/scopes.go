package tools

import (
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// scopesFromParam converts the raw `scopes` array from a tool's input into a
// kernel.ScopeSet. When raw is empty (the caller omitted the field) it returns
// DefaultReadSet — {project, personal} — so existing callers see no change in
// behaviour. When raw is non-empty each element is validated by
// kernel.ParseScopeSet; an unrecognised scope returns an E_VALIDATION error
// that names the offending value and the allowed set.
//
// Callers that receive an error must return it verbatim to the MCP adapter;
// it is already a *mcp.ToolError.
func scopesFromParam(raw []string) (kernel.ScopeSet, error) {
	if len(raw) == 0 {
		return kernel.DefaultReadSet(), nil
	}
	ss, err := kernel.ParseScopeSet(raw)
	if err != nil {
		return kernel.ScopeSet{}, mcp.NewValidationError(
			fmt.Sprintf("invalid scopes: %v (allowed: project, personal, team)", err), nil)
	}
	// ScopeDraft is REJECTED here even though kernel.ParseScopeSet accepts it:
	// the draft staging area is reachable ONLY through the dedicated draftStage /
	// promoteDraft / discardDraft tools, never the general-purpose read verbs
	// (entities/get/neighbors/relations/canvas/unrelate). Allowing it would let
	// any caller read unreviewed staged data by passing scopes:["draft"].
	if ss.Contains(kernel.ScopeDraft) {
		return kernel.ScopeSet{}, mcp.NewValidationError(
			`scope "draft" is not readable here; use the draftStage tool to inspect the staging area`, nil)
	}
	return ss, nil
}
