package cli

import "testing"

// TestMatchDocRef pins the document-reference resolution shared by `doc show`
// and `doc links`: an exact path or slug wins outright, a human title resolves
// case-insensitively, an unknown ref falls through (empty path, no candidates)
// so the downstream tool emits its own not-found error, and an ambiguous title
// returns the candidate paths.
func TestMatchDocRef(t *testing.T) {
	t.Parallel()
	docs := []docResolveRow{
		{Path: "auth-service.md", Slug: "auth-service", Title: "Auth Service"},
		{Path: "notes/billing.md", Slug: "billing", Title: "Billing Service"},
		{Path: "a.md", Slug: "a", Title: "Dup"},
		{Path: "b.md", Slug: "b", Title: "Dup"},
	}
	cases := []struct {
		name      string
		ref       string
		wantPath  string
		wantCands int
	}{
		{"exact path", "auth-service.md", "auth-service.md", 0},
		{"slug bare", "auth-service", "auth-service.md", 0},
		{"slug with dir", "billing", "notes/billing.md", 0},
		{"title exact", "Auth Service", "auth-service.md", 0},
		{"title case-insensitive", "billing service", "notes/billing.md", 0},
		{"path with dir", "notes/billing.md", "notes/billing.md", 0},
		{"unknown falls through", "Nope", "", 0},
		{"ambiguous title", "Dup", "", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path, cands := matchDocRef(tc.ref, docs)
			if path != tc.wantPath {
				t.Errorf("matchDocRef(%q) path = %q, want %q", tc.ref, path, tc.wantPath)
			}
			if len(cands) != tc.wantCands {
				t.Errorf("matchDocRef(%q) candidates = %v, want %d", tc.ref, cands, tc.wantCands)
			}
		})
	}
}
