package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/mod/semver"
)

// checkTTL is how long a cached update-check result is considered fresh. The
// passive check refreshes at most once per this window so a hot loop of maz
// invocations never spams the GitHub API.
const checkTTL = 24 * time.Hour

// CheckCache is the persisted state of the passive update-check: when it last
// ran, the newest tag it saw, and the channel that result is for.
type CheckCache struct {
	// LastCheck is when the cache was last refreshed.
	LastCheck time.Time `json:"last_check"`
	// LatestTag is the newest release tag seen on Channel.
	LatestTag string `json:"latest_tag"`
	// Channel is the release stream LatestTag was resolved on; a channel switch
	// invalidates the cache.
	Channel string `json:"channel"`
}

// LoadCheckCache reads the cache file. A missing file returns the zero value and
// os.ErrNotExist (wrapped), which callers treat as "no cache yet".
func LoadCheckCache(path string) (CheckCache, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the daemon-owned cache file
	if err != nil {
		// Wrapped with %w so callers can still errors.Is(err, os.ErrNotExist)
		// to treat a missing cache as "no cache yet".
		return CheckCache{}, fmt.Errorf("selfupdate: read update-check cache: %w", err)
	}
	var c CheckCache
	if err := json.Unmarshal(data, &c); err != nil {
		return CheckCache{}, fmt.Errorf("selfupdate: decode update-check cache: %w", err)
	}
	return c, nil
}

// Stale reports whether the cache must be refreshed: it is older than checkTTL
// or was recorded for a different channel.
func (c CheckCache) Stale(channel string, now time.Time) bool {
	if c.Channel != channel {
		return true
	}
	return now.Sub(c.LastCheck) >= checkTTL
}

// SaveCheckCache writes the cache atomically (temp file in the same directory,
// then rename), creating the parent directory if needed.
func SaveCheckCache(path string, c CheckCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("selfupdate: create cache dir: %w", err)
	}
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("selfupdate: encode update-check cache: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("selfupdate: create temp cache: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("selfupdate: write temp cache: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("selfupdate: chmod temp cache: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("selfupdate: sync temp cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("selfupdate: close temp cache: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("selfupdate: rename temp cache: %w", err)
	}
	return nil
}

// IsReleaseVersion reports whether v looks like a real release tag (a valid
// semver, not the "dev" default). The passive update-check is skipped entirely
// for non-release builds.
func IsReleaseVersion(v string) bool {
	return v != "" && v != "dev" && semver.IsValid(v)
}

// Newer reports whether latest is a strictly higher semver than current. A "dev"
// or non-semver current never triggers a hint (dev builds are not nagged), and
// a malformed latest is ignored.
func Newer(current, latest string) bool {
	if !IsReleaseVersion(current) {
		return false
	}
	if !semver.IsValid(latest) {
		return false
	}
	return semver.Compare(latest, current) > 0
}
