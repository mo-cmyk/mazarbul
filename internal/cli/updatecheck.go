package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/selfupdate"
	"github.com/mo-cmyk/mazarbul/internal/version"
)

// updateCheckTimeout bounds the synchronous refresh of the update-check cache.
// It is intentionally short: the refresh runs at most once per TTL, after the
// command's own output, and is best-effort, so it must not stall process exit.
const updateCheckTimeout = 2 * time.Second

// updateCheckCacheFile is the cache file name under the owned cache dir.
const updateCheckCacheFile = "update-check.json"

// maybeNotifyUpdate prints a one-line "newer version available" hint when the
// update-check cache shows this build is behind, refreshing a missing or stale
// cache first. It is wired as a PersistentPostRunE so it runs after any one-shot
// command, and it is fully gated (see updateCheckEnabled): no hint, no network,
// and no file access when disabled. It never returns an error.
//
// The refresh is synchronous, not a background goroutine: this runs right before
// the process exits, and Go kills outstanding goroutines the moment main()
// returns, so a fire-and-forget refresh would be cut off mid-request and the
// cache would never settle. The cost is bounded: the common path (a fresh cache)
// does no network and never blocks, and only once per TTL does a command pay a
// short, best-effort fetch after its own output has already been written.
func maybeNotifyUpdate(cmd *cobra.Command) {
	if !updateCheckEnabled(cmd) {
		return
	}
	cachePath := updateCheckCachePath()
	if cachePath == "" {
		return // no resolvable cache dir; skip silently
	}
	channel := updateChannel()

	cache, err := selfupdate.LoadCheckCache(cachePath)
	if err != nil || cache.Stale(channel, time.Now()) {
		cache = refreshUpdateCache(channel, cachePath, upgradeToken(), cache)
	}
	if cache.Channel == channel && selfupdate.Newer(version.Version, cache.LatestTag) {
		// Honour an explicit "skip until next version" choice: the user asked
		// not to be reminded about this particular tag.
		if readSkipVersion() == cache.LatestTag {
			return
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "\na newer maz is available: %s (run `maz upgrade`)\n", cache.LatestTag) //nolint:errcheck // best-effort hint to stderr
	}
}

// notifyParams is the resolved gating input for shouldNotify, split out so the
// decision is a pure function testable across the full matrix (the live path
// reads a real TTY a test buffer cannot fake).
type notifyParams struct {
	disabledFlag bool
	disabledEnv  bool
	ci           bool
	version      string
	longRunning  bool
	isUpgradeCmd bool
	format       output.Format
	isTTY        bool
}

// shouldNotify decides whether the passive hint may print. Conditions are
// ordered cheapest-and-most-disabling first.
func shouldNotify(p notifyParams) bool {
	switch {
	case p.disabledFlag, p.disabledEnv, p.ci:
		return false
	case !selfupdate.IsReleaseVersion(p.version): // dev builds are never nagged
		return false
	case p.longRunning, p.isUpgradeCmd: // servers and the upgrade command itself
		return false
	case p.format != output.FormatText, !p.isTTY: // never pollute JSON/pipes
		return false
	default:
		return true
	}
}

// updateCheckEnabled gathers the live gating inputs and delegates the decision
// to shouldNotify.
func updateCheckEnabled(cmd *cobra.Command) bool {
	noCheck, _ := cmd.Root().PersistentFlags().GetBool("no-update-check")
	wr := output.From(cmd)
	return shouldNotify(notifyParams{
		disabledFlag: noCheck,
		disabledEnv:  os.Getenv("MAZ_NO_UPDATE_CHECK") != "",
		ci:           os.Getenv("CI") != "",
		version:      version.Version,
		longRunning:  isLongRunningCmd(cmd.CommandPath()),
		isUpgradeCmd: cmd.Name() == "upgrade",
		format:       wr.Format(),
		isTTY:        wr.IsTTY(),
	})
}

// refreshUpdateCache resolves the latest release on the channel and rewrites the
// cache, returning the resulting entry. It is fail-silent. To honor the
// once-per-TTL contract even when offline, a failed resolve still rewrites the
// cache with a bumped timestamp, carrying the previously known tag forward, so a
// transient outage neither spams the API nor makes every later command pay the
// fetch. MAZ_API_BASE is honored for parity with `maz upgrade` (and tests).
func refreshUpdateCache(channel, cachePath, token string, prev selfupdate.CheckCache) selfupdate.CheckCache {
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()
	next := selfupdate.CheckCache{LastCheck: time.Now(), Channel: channel, LatestTag: prev.LatestTag}
	if prev.Channel != channel {
		next.LatestTag = "" // a channel switch invalidates the prior tag
	}
	if ch, perr := selfupdate.ParseChannel(channel); perr == nil {
		opts := &selfupdate.Options{Channel: ch, Token: token, APIBase: os.Getenv("MAZ_API_BASE")}
		if rel, rerr := selfupdate.ResolveRelease(ctx, opts); rerr == nil {
			next.LatestTag = rel.Tag
		}
	}
	_ = selfupdate.SaveCheckCache(cachePath, next) //nolint:errcheck // best-effort cache warm
	return next
}

// updateCheckCachePath returns the path to the update-check cache file, or ""
// when no cache directory can be resolved (e.g. no home directory).
func updateCheckCachePath() string {
	dir := config.OwnedDaemonLogDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, updateCheckCacheFile)
}

// updateChannel returns the release channel from MAZ_CHANNEL, defaulting to
// stable. An invalid value falls back to stable rather than erroring: the
// passive check must never block a command on a bad env var.
func updateChannel() string {
	ch, err := selfupdate.ParseChannel(os.Getenv("MAZ_CHANNEL"))
	if err != nil {
		return string(selfupdate.ChannelStable)
	}
	return string(ch)
}

// upgradeToken returns the GitHub token for release access: MAZ_INSTALL_TOKEN
// wins over the more ambient GITHUB_TOKEN, matching scripts/install.sh. Empty
// selects the anonymous public path.
func upgradeToken() string {
	if t := os.Getenv("MAZ_INSTALL_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GITHUB_TOKEN")
}
