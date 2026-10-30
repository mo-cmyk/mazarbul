package daemon

import (
	"context"

	"github.com/mo-cmyk/mazarbul/internal/contextbrief"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// presetBriefReader adapts the Navigator's presets projection to the
// contextbrief.PresetReader the Assembler consumes for the top-priority
// "Operating instructions" section. The verbatim Instructions body is passed
// through here (the brief sanitizes and fences it); it is the only path that
// carries the body into agent context.
//
// Presets are personal-scoped operator config (the install path always mints
// presetInstalled at ScopePersonal). The body therefore travels only on a read
// whose ScopeSet includes personal: a strict team-only getContext must not
// surface the operator's personal preset bodies into its highest-priority
// section. Honoring the ScopeSet boundary here keeps the Operating instructions
// section consistent with the recall/document lanes' fail-closed scope
// isolation (#206) and is the read-side complement to the scope-aware privacy
// marker that guards the sync surface.
type presetBriefReader struct {
	nav *navigate.Navigator
}

func (r presetBriefReader) ActivePresets(ctx context.Context, scopes kernel.ScopeSet) ([]contextbrief.ActivePreset, error) {
	// Fail-closed: omit operating instructions unless the read explicitly spans
	// personal scope (an empty set contains nothing, so it omits too).
	if !scopes.Contains(kernel.ScopePersonal) {
		return nil, nil
	}
	rows, err := r.nav.ActivePresets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]contextbrief.ActivePreset, len(rows))
	for i := range rows {
		out[i] = contextbrief.ActivePreset{
			Name:         rows[i].Name,
			Version:      rows[i].Version,
			Instructions: rows[i].Instructions,
		}
	}
	return out, nil
}
