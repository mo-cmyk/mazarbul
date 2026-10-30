package contextbrief

import (
	"context"
	"sort"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// ActivePreset is one active preset's brief-relevant projection (the `presets`
// table, migration 0029). Name and Version are trusted (install-validated)
// labels; Instructions is the UNTRUSTED agent-steering body, hard-capped at
// install time and sanitized + fenced again here before it reaches an agent.
type ActivePreset struct {
	Name         string // e.g. "crm"; trusted label
	Version      string // e.g. "0.1.0"; trusted label
	Instructions string // UNTRUSTED body
}

// PresetReader lists the presets currently active in a scope span, backing the
// brief's top-priority "Operating instructions" section. Satisfied by the
// SQLite-backed reader the daemon wires over the `presets` projection. The
// Assembler holds it optionally: a nil reader means "no presets", and the
// section is simply absent (the test / unscoped path).
type PresetReader interface {
	ActivePresets(ctx context.Context, scopes kernel.ScopeSet) ([]ActivePreset, error)
}

// sortPresets orders presets by Name then Version, ascending. Byte-stable
// across machines and reinstalls (unlike install time), so the brief — which is
// also rendered as index.md content — does not churn when the active set is
// unchanged.
func sortPresets(p []ActivePreset) {
	sort.Slice(p, func(i, j int) bool {
		if p[i].Name != p[j].Name {
			return p[i].Name < p[j].Name
		}
		return p[i].Version < p[j].Version
	})
}
