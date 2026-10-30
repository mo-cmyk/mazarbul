package privacy

import (
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// This sentinel wires the preset.instructions marker through the public
// RegisterMarker hook at package load, mirroring the built-in seed sentinel in
// marker.go (a var-initialiser, not func init(), which is lint-gated).
//

var _ = func() struct{} {
	RegisterMarker("preset.instructions", presetInstructionsMarker)
	return struct{}{}
}()

// presetInstructionsMarker treats a presetInstalled event — which carries the
// verbatim, untrusted instructions.md body — as sensitive on any NON-personal
// scope, never on personal. Presets are minted at ScopePersonal (#210), where
// the operator's own data dir / personal remote is the trust boundary, so the
// body is allowed in a personal plaintext chunk and personal plaintext sync
// keeps working.
//
// The test is `!= ScopePersonal` rather than an explicit project/team allow-list
// so the marker is fail-closed: a malformed, empty, or future scope is treated
// as sensitive by default rather than slipping through as non-sensitive.
//
// The effective enforcement is on TEAM scope: the seal gate and push guard scan
// team plaintext and refuse it here, while ENCRYPTED team chunks pass. Project
// is the deliberately public, unencryptable in-repo scope and is exempt from
// those gates by design — so marking it changes nothing at the chunk boundary,
// but it is included for intent + defense-in-depth should that exemption ever
// change. Presets are never project-scoped in practice, and the renderer
// separately excludes the body from the human-readable tree on every scope.
func presetInstructionsMarker(ev kernel.Event) bool {
	if ev.Kind != kernel.EventKind(ontology.KindPresetInstalled) {
		return false
	}
	return ev.Scope != kernel.ScopePersonal
}
