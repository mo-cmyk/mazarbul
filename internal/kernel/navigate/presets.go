package navigate

import (
	"context"
	"fmt"
)

// Preset is a row of the presets projection (migration 0029): one active
// preset. The table holds only active presets (install upserts, uninstall
// deletes), so every row read here is currently steering agents. Instructions
// is the verbatim, untrusted agent-steering body; callers that surface it to an
// agent must sanitize and fence it (see internal/contextbrief). There is no
// scope column: presets are personal-only, so listings are unscoped.
type Preset struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Description  string `json:"description"`
	Instructions string `json:"instructions,omitempty"`
	AddedAt      string `json:"addedAt,omitempty"`
}

// ActivePresets returns every active preset, ordered by name (deterministic).
// The verbatim Instructions body is included; callers that must not expose it
// (e.g. the listPresets MCP tool) drop the field after reading.
func (n *Navigator) ActivePresets(ctx context.Context) ([]Preset, error) {
	rows, err := n.reader.QueryContext(ctx,
		`SELECT name, version, description, instructions, added_at
		   FROM presets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list presets: %w", err)
	}
	defer rows.Close()

	var out []Preset
	for rows.Next() {
		var p Preset
		if scanErr := rows.Scan(&p.Name, &p.Version, &p.Description, &p.Instructions, &p.AddedAt); scanErr != nil {
			return nil, fmt.Errorf("scan preset row: %w", scanErr)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list presets: %w", err)
	}
	return out, nil
}
