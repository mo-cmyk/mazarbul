package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/contextbrief"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// indexBuilder is the assembly surface getIndex consumes. Satisfied by
// *contextbrief.Assembler.
type indexBuilder interface {
	BuildIndex(ctx context.Context, req contextbrief.IndexRequest) (contextbrief.Index, error)
}

const getIndexInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const getIndexOutputSchema = `{
  "type": "object",
  "properties": {
    "markdown":  {"type": "string",  "description": "The vault directory listing as markdown: folder groups, recently-updated section, and key-entity roll-up."},
    "documents": {"type": "integer", "description": "Total number of live vault documents listed."},
    "folders":   {"type": "integer", "description": "Number of distinct path-prefix folder groups."}
  },
  "required": ["markdown", "documents", "folders"],
  "additionalProperties": false
}`

// GetIndex assembles a master vault directory listing from the unified graph.
// Unlike GetContext (the budgeted brief), GetIndex is NOT token-budget-gated:
// it always enumerates the complete vault, grouped by path-prefix folder, with
// a recently-updated section and a key-entity roll-up. Use it to build index.md
// and get a navigation overview of the vault.
type GetIndex struct {
	builder indexBuilder
}

// NewGetIndex constructs a GetIndex tool over a brief assembler.
func NewGetIndex(builder indexBuilder) *GetIndex { return &GetIndex{builder: builder} }

// Name implements mcp.Tool.
func (*GetIndex) Name() string { return "getIndex" }

// Description implements mcp.Tool.
func (*GetIndex) Description() string {
	return "Assemble a master vault directory listing: all vault documents grouped by folder, a recently-updated section, and a key-entity roll-up. NOT token-budget-gated — always complete. Use for building index.md or getting a navigation overview. For the token-budgeted working-set brief (hot.md), use getContext instead."
}

// InputSchema implements mcp.Tool.
func (*GetIndex) InputSchema() json.RawMessage { return json.RawMessage(getIndexInputSchema) }

// OutputSchema implements mcp.Tool.
func (*GetIndex) OutputSchema() json.RawMessage { return json.RawMessage(getIndexOutputSchema) }

// Unlisted hides getIndex from tools/list: it is driven by `maz context index`
// and does not belong in the standard agent navigation vocabulary (agents that
// want vault navigation use listDocuments; agents that want a context warm-up
// use getContext).
func (*GetIndex) Unlisted() bool { return true }

// Invoke implements mcp.Tool.
func (g *GetIndex) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in struct{} // no inputs for now; IndexRequest may be extended in future phases
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	idx, err := g.builder.BuildIndex(ctx, contextbrief.IndexRequest{})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("getIndex: build index: %w", err))
	}
	return marshalTool(idx)
}
