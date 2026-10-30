package identity

import "testing"

func TestNoreplyEmail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		login  string
		userID int64
		want   string
	}{
		{"id-prefixed", "octocat", 583231, "583231+octocat@users.noreply.github.com"},
		{"legacy zero id", "octocat", 0, "octocat@users.noreply.github.com"},
		{"empty login", "", 42, ""},
		{"sanitized login", "bad name!@#", 7, "7+badname@users.noreply.github.com"},
		{"sanitizes to empty", "@@@", 7, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NoreplyEmail(tc.login, tc.userID); got != tc.want {
				t.Errorf("NoreplyEmail(%q, %d) = %q, want %q", tc.login, tc.userID, got, tc.want)
			}
		})
	}
}

func TestSanitizeGitHubLogin(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"octocat":       "octocat",
		"  spaced  ":    "spaced",
		"a-b-c":         "a-b-c",
		"evil@host.com": "evilhostcom",
		"日本語x":          "x",
		"":              "",
	}
	for in, want := range cases {
		if got := SanitizeGitHubLogin(in); got != want {
			t.Errorf("SanitizeGitHubLogin(%q) = %q, want %q", in, got, want)
		}
	}
}
