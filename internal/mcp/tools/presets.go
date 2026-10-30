package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// PresetLister is the read surface ListPresets needs: the active-preset
// projection. Satisfied by *navigate.Navigator. Kept narrow (interface
// segregation) so the broad tools.Navigator interface and its test fakes are
// untouched.
type PresetLister interface {
	ActivePresets(ctx context.Context) ([]navigate.Preset, error)
}

// listPresetsInputSchema takes no arguments: it lists every active preset.
const listPresetsInputSchema = `{
  "type": "object",
  "additionalProperties": false
}`

// listPresetsOutputSchema documents the response. The verbatim instructions
// body is deliberately ABSENT: an agent is already steered by the preset via the
// getContext "Operating instructions" section, so this tool reports only which
// presets are active (name/version/description), not their steering text.
const listPresetsOutputSchema = `{
  "type": "object",
  "properties": {
    "presets": {
      "type": "array",
      "description": "Active presets, ordered by name.",
      "items": {
        "type": "object",
        "properties": {
          "name":        {"type": "string"},
          "version":     {"type": "string"},
          "description": {"type": "string"}
        },
        "required": ["name", "version", "description"],
        "additionalProperties": false
      }
    }
  },
  "required": ["presets"],
  "additionalProperties": false
}`

// ListPresets is the read-only agent surface for presets: it reports which
// presets are currently steering the agent. There is deliberately no
// agent-callable install — presets are installed only via the `maz preset`
// CLI, behind a human-confirmation gate for non-builtin bodies.
type ListPresets struct {
	src PresetLister
}

// NewListPresets constructs the tool over the active-preset reader.
func NewListPresets(src PresetLister) *ListPresets { return &ListPresets{src: src} }

// Name implements mcp.Tool.
func (*ListPresets) Name() string { return "listPresets" }

// Description implements mcp.Tool.
func (*ListPresets) Description() string {
	return "List the presets currently active (steering this agent's operating instructions). Returns name, version, and description for each — read-only. Presets are installed via the `maz preset install` CLI, not by agents."
}

// InputSchema implements mcp.Tool.
func (*ListPresets) InputSchema() json.RawMessage { return json.RawMessage(listPresetsInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ListPresets) OutputSchema() json.RawMessage { return json.RawMessage(listPresetsOutputSchema) }

type presetSummary struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type listPresetsOutput struct {
	Presets []presetSummary `json:"presets"`
}

// Invoke implements mcp.Tool.
func (l *ListPresets) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	rows, err := l.src.ActivePresets(ctx)
	if err != nil {
		return nil, navQueryError("list presets", err)
	}
	out := listPresetsOutput{Presets: make([]presetSummary, 0, len(rows))}
	for i := range rows {
		out.Presets = append(out.Presets, presetSummary{
			Name:        rows[i].Name,
			Version:     rows[i].Version,
			Description: rows[i].Description,
		})
	}
	body, mErr := json.Marshal(out)
	if mErr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal response: %w", mErr))
	}
	return body, nil
}
