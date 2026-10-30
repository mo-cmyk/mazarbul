package fetch_test

import (
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/plugin/fetch"
)

func TestIsRemoteSpec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		arg  string
		want bool
	}{
		{"github.com/user/repo", true},
		{"gitlab.com/group/repo", true},
		{"bitbucket.org/team/repo", true},
		{"https://github.com/user/repo.git", true},
		{"http://example.com/r.git", true},
		{"ssh://git@host/u/r.git", true},
		{"git://host/u/r.git", true},
		{"git@github.com:user/repo.git", true},
		{"./my-preset", false},
		{"/abs/path/preset", false},
		{"crm", false},
		{"business-de", false},
		{"agent-memory", false},
	}
	for _, tc := range cases {
		if got := fetch.IsRemoteSpec(tc.arg); got != tc.want {
			t.Errorf("IsRemoteSpec(%q) = %v, want %v", tc.arg, got, tc.want)
		}
	}
}

func TestParseRemoteSpec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		arg      string
		cloneURL string
		ref      string
		subdir   string
	}{
		{"github.com/user/repo", "https://github.com/user/repo.git", "", ""},
		{"github.com/user/repo@v1.2.0", "https://github.com/user/repo.git", "v1.2.0", ""},
		{"github.com/user/repo@main#packs/crm", "https://github.com/user/repo.git", "main", "packs/crm"},
		{"github.com/user/repo@feature/foo", "https://github.com/user/repo.git", "feature/foo", ""},
		{"https://github.com/user/repo.git@deadbeef", "https://github.com/user/repo.git", "deadbeef", ""},
		{"git@github.com:user/repo.git", "git@github.com:user/repo.git", "", ""},
		{"git@github.com:user/repo.git@v1", "git@github.com:user/repo.git", "v1", ""},
		{"ssh://git@host/u/r.git@v2#sub", "ssh://git@host/u/r.git", "v2", "sub"},
		// GitLab nested subgroups are valid clone paths (extra interior segments
		// are kept, NOT mistaken for a #subdir).
		{"gitlab.com/group/subgroup/repo", "https://gitlab.com/group/subgroup/repo.git", "", ""},
		{"gitlab.com/group/sub/repo@v1#crm", "https://gitlab.com/group/sub/repo.git", "v1", "crm"},
	}
	for _, tc := range cases {
		spec, err := fetch.ParseRemoteSpec(tc.arg)
		if err != nil {
			t.Errorf("ParseRemoteSpec(%q): %v", tc.arg, err)
			continue
		}
		if spec.CloneURL != tc.cloneURL {
			t.Errorf("ParseRemoteSpec(%q).CloneURL = %q, want %q", tc.arg, spec.CloneURL, tc.cloneURL)
		}
		if spec.Ref != tc.ref {
			t.Errorf("ParseRemoteSpec(%q).Ref = %q, want %q", tc.arg, spec.Ref, tc.ref)
		}
		if spec.Subdir != tc.subdir {
			t.Errorf("ParseRemoteSpec(%q).Subdir = %q, want %q", tc.arg, spec.Subdir, tc.subdir)
		}
		if spec.Display != tc.arg {
			t.Errorf("ParseRemoteSpec(%q).Display = %q, want verbatim", tc.arg, spec.Display)
		}
	}
}

func TestParseRemoteSpec_Rejects(t *testing.T) {
	t.Parallel()
	cases := []string{
		"github.com/user",                // missing repo segment
		"github.com/user/repo#/abs",      // absolute subdir
		"github.com/user/repo#../up",     // escaping subdir
		"github.com/user/repo#sub/..",    // traversal collapsing back to root
		"github.com/user/repo#sub/./..",  // same, via a "." hop
		"github.com/user/repo#a/b/../..", // deeper traversal to root
		"crm",                            // not a remote spec at all
	}
	for _, arg := range cases {
		if _, err := fetch.ParseRemoteSpec(arg); err == nil {
			t.Errorf("ParseRemoteSpec(%q) = nil error, want rejection", arg)
		}
	}
}
