package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
)

// Channel selects which release stream to resolve. The zero value is invalid;
// use ParseChannel or one of the constants.
type Channel string

// ChannelStable resolves the highest published non-prerelease release.
const ChannelStable Channel = "stable"

// ChannelNightly resolves the newest release entry including prereleases. The
// nightly tag scheme is not finalized (see issue #258); this takes the first
// entry as a forward-compatible placeholder, matching install.sh.
const ChannelNightly Channel = "nightly"

// ParseChannel maps a channel string (empty defaults to stable) to a Channel.
func ParseChannel(s string) (Channel, error) {
	switch s {
	case "", string(ChannelStable):
		return ChannelStable, nil
	case string(ChannelNightly):
		return ChannelNightly, nil
	default:
		return "", fmt.Errorf("unknown channel %q (stable|nightly)", s)
	}
}

// Options configures release resolution and download. All network and platform
// inputs are injectable so the pipeline is fully testable offline. It is passed
// by pointer (it is large and reused across resolve and download).
type Options struct {
	// Channel selects the release stream. Ignored when Version is set.
	Channel Channel
	// Version pins an exact tag (e.g. "v0.3.0"). A pinned version wins over the
	// channel, matching install.sh.
	Version string
	// Token authenticates private-repo asset access. Empty selects the
	// anonymous public-CDN download path.
	Token string
	// APIBase overrides the GitHub REST API root (tests point it at httptest).
	APIBase string
	// BaseURL, when set, bypasses the GitHub API and fetches the archive and
	// checksums.txt directly from "<BaseURL>/<name>". Supports file:// for
	// offline end-to-end tests and http(s):// mirrors. Resolving "latest" still
	// needs the API, so pair BaseURL with a pinned Version for a fully offline
	// run.
	BaseURL string
	// OS and Arch override the target platform (default runtime.GOOS/GOARCH).
	OS, Arch string
	// HTTP overrides the HTTP client (tests inject one). When nil, a default
	// client with the token-stripping CheckRedirect is used.
	HTTP *http.Client
}

// osArch returns the resolved target OS and architecture.
func (o *Options) osArch() (string, string) {
	osName, arch := o.OS, o.Arch
	if osName == "" {
		osName = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	return osName, arch
}

// Release is a resolved release ready to download. The URLs are absolute and
// already account for the anonymous, token-asset, and mirror paths.
type Release struct {
	// Tag is the resolved release tag (e.g. "v0.3.0").
	Tag string
	// ArchiveName is the platform archive filename.
	ArchiveName string
	// ArchiveURL and SumsURL locate the archive and its checksums.txt. For the
	// token path these are API asset endpoints that 302 to signed storage.
	ArchiveURL string
	SumsURL    string
	// ViaAsset is true when the URLs are GitHub API asset endpoints requiring an
	// Accept: application/octet-stream + bearer token (the private-repo path).
	ViaAsset bool
}

// ghRelease is the subset of the GitHub release payload we read.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"assets"`
}

// archiveName builds the platform archive filename for a tag.
func archiveName(tag, osName, arch string) string {
	return fmt.Sprintf("mazarbul_%s_%s_%s.tar.gz", tag, osName, arch)
}

// ResolveRelease resolves the release to install for the given options: it
// determines the tag (pinned or latest-on-channel) and the archive + checksums
// URLs for the appropriate download path (mirror, private asset, or public
// CDN).
func ResolveRelease(ctx context.Context, opts *Options) (Release, error) {
	tag := opts.Version
	if tag == "" {
		var err error
		tag, err = resolveLatestTag(ctx, opts)
		if err != nil {
			return Release{}, err
		}
	}
	osName, arch := opts.osArch()
	name := archiveName(tag, osName, arch)
	rel := Release{Tag: tag, ArchiveName: name}

	switch {
	case opts.BaseURL != "":
		base := strings.TrimRight(opts.BaseURL, "/")
		rel.ArchiveURL = base + "/" + name
		rel.SumsURL = base + "/checksums.txt"
	case opts.Token != "":
		ids, err := resolveAssetIDs(ctx, opts, tag, name)
		if err != nil {
			return Release{}, err
		}
		base := opts.apiBase() + "/repos/" + Repo + "/releases/assets/"
		rel.ArchiveURL = base + strconv.FormatInt(ids.archive, 10)
		rel.SumsURL = base + strconv.FormatInt(ids.sums, 10)
		rel.ViaAsset = true
	default:
		base := defaultDownloadBase + "/" + tag
		rel.ArchiveURL = base + "/" + name
		rel.SumsURL = base + "/checksums.txt"
	}
	return rel, nil
}

// resolveLatestTag queries the GitHub API for the newest tag on the channel.
// The response is decoded in its own step (separate from the request) so an
// auth failure (401/403) surfaces as such rather than as a misleading "no
// release found", matching install.sh's deliberate two-step.
func resolveLatestTag(ctx context.Context, opts *Options) (string, error) {
	ch := opts.Channel
	if ch == "" {
		ch = ChannelStable
	}
	switch ch {
	case ChannelStable:
		body, err := opts.getJSON(ctx, opts.apiBase()+"/repos/"+Repo+"/releases/latest")
		if err != nil {
			return "", fmt.Errorf("resolve stable release (token valid? repo access?): %w", err)
		}
		var rel ghRelease
		if err := json.Unmarshal(body, &rel); err != nil {
			return "", fmt.Errorf("selfupdate: decode latest release: %w", err)
		}
		if rel.TagName == "" {
			return "", errors.New("selfupdate: no stable release found (set --version to install a specific tag)")
		}
		return rel.TagName, nil
	case ChannelNightly:
		body, err := opts.getJSON(ctx, opts.apiBase()+"/repos/"+Repo+"/releases?per_page=1")
		if err != nil {
			return "", fmt.Errorf("resolve nightly release (token valid? repo access?): %w", err)
		}
		var rels []ghRelease
		if err := json.Unmarshal(body, &rels); err != nil {
			return "", fmt.Errorf("selfupdate: decode nightly releases: %w", err)
		}
		if len(rels) == 0 || rels[0].TagName == "" {
			return "", errors.New("selfupdate: no nightly release found (set --version to install a specific tag)")
		}
		return rels[0].TagName, nil
	default:
		return "", fmt.Errorf("unknown channel %q (stable|nightly)", ch)
	}
}

// assetIDs holds the numeric ids of a release's archive and checksums assets.
type assetIDs struct {
	archive int64
	sums    int64
}

// resolveAssetIDs fetches the release by tag and returns the numeric asset ids
// for the archive and checksums.txt. The private-repo browser download URL will
// not serve private assets, so the token path must address each asset by id
// through the API octet-stream endpoint.
func resolveAssetIDs(ctx context.Context, opts *Options, tag, name string) (assetIDs, error) {
	body, err := opts.getJSON(ctx, opts.apiBase()+"/repos/"+Repo+"/releases/tags/"+tag)
	if err != nil {
		return assetIDs{}, fmt.Errorf("resolve release %s (token valid? repo access?): %w", tag, err)
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return assetIDs{}, fmt.Errorf("selfupdate: decode release %s: %w", tag, err)
	}
	ids := assetIDs{archive: findAssetID(rel, name), sums: findAssetID(rel, "checksums.txt")}
	if ids.archive == 0 {
		return assetIDs{}, fmt.Errorf("selfupdate: release %s has no asset %s", tag, name)
	}
	if ids.sums == 0 {
		return assetIDs{}, fmt.Errorf("selfupdate: release %s has no asset checksums.txt", tag)
	}
	return ids, nil
}

// findAssetID returns the numeric id of the asset whose name matches exactly, or
// 0 when absent. The match is on the structured field, immune to ordering.
func findAssetID(rel ghRelease, name string) int64 {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.ID
		}
	}
	return 0
}
