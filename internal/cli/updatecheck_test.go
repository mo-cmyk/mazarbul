package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/selfupdate"
)

func TestShouldNotify(t *testing.T) {
	t.Parallel()
	// base is the all-conditions-pass case.
	base := notifyParams{version: "v1.0.0", format: output.FormatText, isTTY: true}
	if !shouldNotify(base) {
		t.Fatal("baseline should notify")
	}
	cases := []struct {
		name   string
		mutate func(p *notifyParams)
	}{
		{"disabled by flag", func(p *notifyParams) { p.disabledFlag = true }},
		{"disabled by env", func(p *notifyParams) { p.disabledEnv = true }},
		{"in ci", func(p *notifyParams) { p.ci = true }},
		{"dev build", func(p *notifyParams) { p.version = "dev" }},
		{"long running command", func(p *notifyParams) { p.longRunning = true }},
		{"upgrade command", func(p *notifyParams) { p.isUpgradeCmd = true }},
		{"json output", func(p *notifyParams) { p.format = output.FormatJSON }},
		{"not a tty", func(p *notifyParams) { p.isTTY = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := base
			tc.mutate(&p)
			if shouldNotify(p) {
				t.Errorf("%s: should not notify", tc.name)
			}
		})
	}
}

func TestUpgradeToken(t *testing.T) {
	cases := []struct {
		name    string
		install string
		github  string
		want    string
	}{
		{"install token wins over github", "itok", "gtok", "itok"},
		{"falls back to the github token", "", "gtok", "gtok"},
		{"empty when neither is set", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAZ_INSTALL_TOKEN", tc.install)
			t.Setenv("GITHUB_TOKEN", tc.github)
			if got := upgradeToken(); got != tc.want {
				t.Errorf("upgradeToken() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUpdateChannel(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"defaults to stable when unset", "", string(selfupdate.ChannelStable)},
		{"honors nightly", "nightly", string(selfupdate.ChannelNightly)},
		{"falls back to stable on a bad value", "bogus", string(selfupdate.ChannelStable)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAZ_CHANNEL", tc.env)
			if got := updateChannel(); got != tc.want {
				t.Errorf("updateChannel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRefreshUpdateCache covers the synchronous refresh: the cache must actually
// be written before the function returns (the bug a fire-and-forget goroutine
// hit — it was killed on process exit, so the cache never settled).
func TestRefreshUpdateCache(t *testing.T) {
	t.Run("writes the resolved tag and persists it for the next run", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v9.9.9"}`)
		}))
		defer srv.Close()
		t.Setenv("MAZ_API_BASE", srv.URL)
		cachePath := filepath.Join(t.TempDir(), "update-check.json")

		got := refreshUpdateCache("stable", cachePath, "", selfupdate.CheckCache{})
		if got.LatestTag != "v9.9.9" {
			t.Errorf("returned LatestTag = %q, want v9.9.9", got.LatestTag)
		}
		reloaded, err := selfupdate.LoadCheckCache(cachePath)
		if err != nil || reloaded.LatestTag != "v9.9.9" {
			t.Errorf("reloaded cache = %+v err=%v, want LatestTag v9.9.9", reloaded, err)
		}
	})

	t.Run("carries the prior tag and bumps the timestamp when the resolve fails", func(t *testing.T) {
		t.Setenv("MAZ_API_BASE", "http://127.0.0.1:1") // refused; no release resolvable
		cachePath := filepath.Join(t.TempDir(), "update-check.json")
		prev := selfupdate.CheckCache{LatestTag: "v1.0.0", Channel: "stable"}

		got := refreshUpdateCache("stable", cachePath, "", prev)
		if got.LatestTag != "v1.0.0" {
			t.Errorf("LatestTag = %q, want the carried-forward v1.0.0", got.LatestTag)
		}
		if got.LastCheck.IsZero() {
			t.Error("LastCheck not bumped on failure: every later command would refetch and block")
		}
		if reloaded, err := selfupdate.LoadCheckCache(cachePath); err != nil || reloaded.LatestTag != "v1.0.0" {
			t.Errorf("reloaded cache = %+v err=%v, want carried-forward v1.0.0", reloaded, err)
		}
	})
}
