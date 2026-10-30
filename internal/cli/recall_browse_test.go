package cli

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/tui/browse"
)

// TestMergeFiltersIntoFlags_Kinds pins the fix for the double-patch bug: the
// recall --kind flag (flags.kinds) must survive into the merged flags so
// buildRecallRequest patches it exactly once, and an active strip Kind pill
// (Filters.Kinds) overrides it — mirroring how Scopes/MinScore/At merge. The
// earlier code patched kinds a second time in Search, silently dropping --kind
// whenever any strip pill was toggled.
func TestMergeFiltersIntoFlags_Kinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		baseKinds []string
		filter    browse.Filters
		want      []string
	}{
		{
			name:      "cli kind preserved when strip empty",
			baseKinds: []string{"document"},
			filter:    browse.Filters{},
			want:      []string{"document"},
		},
		{
			name:      "strip kind overrides cli kind",
			baseKinds: []string{"document"},
			filter:    browse.Filters{Kinds: []string{"episode"}},
			want:      []string{"episode"},
		},
		{
			name:      "no kinds anywhere stays empty",
			baseKinds: nil,
			filter:    browse.Filters{},
			want:      nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := &recallFlags{kinds: tc.baseKinds}
			got := mergeFiltersIntoFlags(base, &tc.filter)
			if len(got.kinds) != len(tc.want) {
				t.Fatalf("kinds = %v, want %v", got.kinds, tc.want)
			}
			for i := range tc.want {
				if got.kinds[i] != tc.want[i] {
					t.Errorf("kinds[%d] = %q, want %q", i, got.kinds[i], tc.want[i])
				}
			}
		})
	}
}
