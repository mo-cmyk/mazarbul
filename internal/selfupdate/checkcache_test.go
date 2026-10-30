package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckCacheStale(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		cache    CheckCache
		channel  string
		wantStal bool
	}{
		{"fresh within ttl", CheckCache{LastCheck: now.Add(-time.Hour), Channel: "stable"}, "stable", false},
		{"older than ttl", CheckCache{LastCheck: now.Add(-25 * time.Hour), Channel: "stable"}, "stable", true},
		{"channel changed", CheckCache{LastCheck: now.Add(-time.Hour), Channel: "stable"}, "nightly", true},
		{"empty cache", CheckCache{}, "stable", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cache.Stale(tc.channel, now); got != tc.wantStal {
				t.Errorf("Stale = %v, want %v", got, tc.wantStal)
			}
		})
	}
}

func TestNewer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		current, latest string
		want            bool
	}{
		{"latest is higher", "v0.3.0", "v0.4.0", true},
		{"latest is lower", "v0.4.0", "v0.3.0", false},
		{"equal", "v0.3.0", "v0.3.0", false},
		{"dev current", "dev", "v1.0.0", false},
		{"empty current", "", "v1.0.0", false},
		{"non-semver latest", "v0.3.0", "not-a-version", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Newer(tc.current, tc.latest); got != tc.want {
				t.Errorf("Newer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
			}
		})
	}
}

func TestSaveLoadCheckCache(t *testing.T) {
	t.Parallel()

	t.Run("round trips", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
		path := filepath.Join(t.TempDir(), "update-check.json")
		want := CheckCache{LastCheck: now, LatestTag: "v1.0.0", Channel: "stable"}
		if err := SaveCheckCache(path, want); err != nil {
			t.Fatalf("SaveCheckCache: %v", err)
		}
		got, err := LoadCheckCache(path)
		if err != nil {
			t.Fatalf("LoadCheckCache: %v", err)
		}
		if got.LatestTag != want.LatestTag || got.Channel != want.Channel || !got.LastCheck.Equal(want.LastCheck) {
			t.Errorf("round trip = %+v, want %+v", got, want)
		}
	})

	t.Run("missing file reports not-exist", func(t *testing.T) {
		t.Parallel()
		_, err := LoadCheckCache(filepath.Join(t.TempDir(), "nope.json"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("err = %v, want os.ErrNotExist", err)
		}
	})
}
