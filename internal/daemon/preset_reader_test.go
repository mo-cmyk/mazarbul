package daemon

import (
	"context"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// TestPresetBriefReader_OmitsBodyForNonPersonalScope proves the read-side scope
// gate: a getContext whose ScopeSet excludes personal must NOT surface the
// operator's personal preset bodies into the highest-priority Operating
// instructions section. The gate short-circuits before consulting the
// Navigator, so a nil nav must NOT panic — a nil-pointer dereference would mean
// the projection was queried and the body would have leaked. Fail-closed: an
// empty set omits too.
func TestPresetBriefReader_OmitsBodyForNonPersonalScope(t *testing.T) {
	t.Parallel()
	// nav is deliberately nil: reaching it for these scopes is the bug.
	r := presetBriefReader{nav: nil}
	for _, tc := range []struct {
		name   string
		scopes kernel.ScopeSet
	}{
		{"team only", kernel.NewScopeSet(kernel.ScopeTeam)},
		{"project only", kernel.NewScopeSet(kernel.ScopeProject)},
		{"project and team", kernel.NewScopeSet(kernel.ScopeProject, kernel.ScopeTeam)},
		{"empty fail-closed", kernel.NewScopeSet()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := r.ActivePresets(context.Background(), tc.scopes)
			if err != nil {
				t.Fatalf("ActivePresets(%s): unexpected error %v", tc.name, err)
			}
			if len(got) != 0 {
				t.Errorf("ActivePresets(%s) = %v, want no presets (personal-scoped body must not leak)", tc.name, got)
			}
		})
	}
}
