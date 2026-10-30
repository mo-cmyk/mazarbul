package cli

// blame_resolve_test.go pins the input shapes `maz show` and `maz blame`
// accept when resolving an entity identifier against the rendered/
// stub directory. The historic forms (bare UID prefix, display-name
// fragment, "kind:slug") still work; the AIP collection form
// "entities/<ULID>" — which `maz list entities --output json` and
// `maz remember` both emit — must also resolve.

import "testing"

func TestStripEntityResolverPrefix_HandlesAIPCollectionForm(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bare UID survives", "01KT8X0SERT5FJB0VGPYQQGAEN", "01kt8x0sert5fjb0vgpyqqgaen"},
		{"AIP entities/ prefix is stripped", "entities/01KT8X0SERT5FJB0VGPYQQGAEN", "01kt8x0sert5fjb0vgpyqqgaen"},
		{"kind colon prefix still stripped", "person:frodo", "frodo"},
		{"AIP plus kind colon", "entities/person:01KT8X", "01kt8x"},
		{"display name survives", "Frodo", "frodo"},
		{"empty input", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripEntityResolverPrefix(tc.in)
			if got != tc.want {
				t.Fatalf("stripEntityResolverPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
