package doctor

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

func ptrBool(b bool) *bool { return &b }

func TestCommitIdentityVerdict(t *testing.T) {
	t.Parallel()
	remoteGitHub := map[string]config.ScopeRemoteTable{
		"personal": {URL: "https://github.com/me/mem.git"},
	}
	remoteLocal := map[string]config.ScopeRemoteTable{
		"personal": {URL: "file:///tmp/mirror"},
	}

	cases := []struct {
		name       string
		sync       config.SyncTable
		wantStatus Status
		wantSub    string
	}{
		{
			"explicit override",
			config.SyncTable{Remotes: remoteGitHub, Identity: &config.SyncIdentityTable{Name: "X", Email: "x@y.z"}},
			StatusOK, "explicit override",
		},
		{
			"default noreply",
			config.SyncTable{Remotes: remoteGitHub},
			StatusOK, "noreply",
		},
		{
			"anonymous",
			config.SyncTable{Remotes: remoteGitHub, Identity: &config.SyncIdentityTable{CommitIdentity: "anonymous"}},
			StatusOK, "anonymous",
		},
		{
			"real to off-device warns",
			config.SyncTable{Remotes: remoteGitHub, Identity: &config.SyncIdentityTable{CommitIdentity: "real"}},
			StatusWarn, "publishes your git email",
		},
		{
			"real to local-only ok",
			config.SyncTable{Remotes: remoteLocal, Identity: &config.SyncIdentityTable{CommitIdentity: "real"}},
			StatusOK, "only local/file remotes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sync := tc.sync
			status, detail := commitIdentityVerdict(&sync)
			if status != tc.wantStatus {
				t.Errorf("status = %v, want %v (detail: %s)", status, tc.wantStatus, detail)
			}
			if !strings.Contains(detail, tc.wantSub) {
				t.Errorf("detail %q missing %q", detail, tc.wantSub)
			}
		})
	}
}

func TestOffDeviceRemoteScopes(t *testing.T) {
	t.Parallel()
	remotes := map[string]config.ScopeRemoteTable{
		"personal":  {URL: "https://github.com/me/p.git"},
		"project":   {URL: "file:///tmp/proj"},
		"team":      {URL: "git@github.com:org/team.git"},
		"empty":     {URL: ""},
		"encrypted": {URL: "https://gitea.example/x.git", Encrypted: ptrBool(true)}, // encryption does
		// not hide the plaintext author line, so this still counts.
	}
	got := offDeviceRemoteScopes(remotes)
	want := []string{"encrypted", "personal", "team"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("off-device scopes = %v, want %v", got, want)
	}
}
