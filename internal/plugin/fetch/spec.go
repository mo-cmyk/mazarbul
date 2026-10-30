package fetch

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Provenance is the audit trail of a remote fetch: the source spec the operator
// typed, the commit SHA the bundle was pinned to, and the canonical content
// checksum of the bundle that landed. All three are empty for local-directory
// and builtin packs (which are not fetched).
type Provenance struct {
	// SourceURL is the remote spec verbatim, e.g. "github.com/user/repo@v1#pkg".
	SourceURL string
	// PinnedRef is the resolved commit SHA (git rev-parse HEAD) the bundle came
	// from — always a commit, even when the spec named a branch or no ref.
	PinnedRef string
	// Checksum is the canonical bundle checksum, "sha256:<hex>".
	Checksum string
}

// RemoteSpec is a parsed remote pack location.
type RemoteSpec struct {
	// CloneURL is what we pass to `git clone` (a full URL; short forge handles
	// are expanded to https://host/owner/repo.git).
	CloneURL string
	// Ref is the branch, tag, or commit SHA to pin; "" means the default branch.
	Ref string
	// Subdir is the bundle directory within the repo; "" means the repo root.
	Subdir string
	// Display is the original spec, verbatim, recorded as Provenance.SourceURL.
	Display string
}

// reSCP matches scp-style git remotes: user@host:path.
var reSCP = regexp.MustCompile(`^[^/@]+@[^/:]+:`)

// forgeHosts returns the short-handle hosts recognized without an explicit
// scheme.
func forgeHosts() []string {
	return []string{"github.com", "gitlab.com", "bitbucket.org"}
}

// IsRemoteSpec reports whether arg lexically looks like a remote pack location
// rather than a local directory or a builtin name. Detection touches neither the
// filesystem nor the network, so it is cheap and order-independent: the resolver
// can check it before os.Stat without risking a stray network call for a path.
func IsRemoteSpec(arg string) bool {
	switch {
	case strings.Contains(arg, "://"):
		return true
	case reSCP.MatchString(arg):
		return true
	default:
		return isForgeHandle(arg)
	}
}

// isForgeHandle reports whether arg begins with a known forge host followed by a
// path segment, e.g. "github.com/user/repo".
func isForgeHandle(arg string) bool {
	for _, host := range forgeHosts() {
		if strings.HasPrefix(arg, host+"/") {
			return true
		}
	}
	return false
}

// ParseRemoteSpec decomposes arg into a RemoteSpec. The syntax is
// <git-url-or-forge-handle>[@ref][#subdir]:
//
//	github.com/user/repo
//	github.com/user/repo@v1.2.0#packs/crm
//	https://github.com/user/repo.git@main
//	git@github.com:user/repo.git@deadbeef
//
// The host/userinfo prefix is stripped before the @ref is split, so the leading
// "git@" of an scp remote is never mistaken for a ref separator.
func ParseRemoteSpec(arg string) (RemoteSpec, error) {
	if !IsRemoteSpec(arg) {
		return RemoteSpec{}, fmt.Errorf("fetch: %q is not a remote spec", arg)
	}
	spec := RemoteSpec{Display: arg}

	base, frag, hasFrag := strings.Cut(arg, "#")
	if hasFrag {
		sub, err := cleanSubdir(frag)
		if err != nil {
			return RemoteSpec{}, err
		}
		spec.Subdir = sub
	}

	cloneRaw, ref := splitRef(base)
	spec.Ref = ref

	cloneURL, err := normalizeCloneURL(cloneRaw)
	if err != nil {
		return RemoteSpec{}, err
	}
	spec.CloneURL = cloneURL
	return spec, nil
}

// splitRef separates a trailing @ref from base, ignoring any @ that is part of a
// scheme/userinfo prefix. It returns the clone portion and the ref ("" if none).
func splitRef(base string) (string, string) {
	start := repoPathStart(base)
	if at := strings.LastIndexByte(base[start:], '@'); at >= 0 {
		return base[:start+at], base[start+at+1:]
	}
	return base, ""
}

// repoPathStart returns the index in s at which the repository path begins,
// i.e. just past the scheme/userinfo/host prefix. Any @ before this index is
// userinfo, not a ref separator.
func repoPathStart(s string) int {
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return i + 3 + j + 1
		}
		return len(s)
	}
	if m := reSCP.FindStringIndex(s); m != nil {
		return m[1]
	}
	if j := strings.IndexByte(s, '/'); j >= 0 {
		return j + 1
	}
	return len(s)
}

// normalizeCloneURL expands a short forge handle to a full https clone URL and
// leaves full URLs / scp remotes untouched. It validates that a handle names at
// least host/owner/repo. Extra path segments are kept (GitLab nested subgroups,
// e.g. gitlab.com/group/subgroup/repo, are valid clone paths); a bundle nested
// INSIDE a repo is selected with #subdir, not by appending it to the handle.
func normalizeCloneURL(cloneRaw string) (string, error) {
	if strings.Contains(cloneRaw, "://") || reSCP.MatchString(cloneRaw) {
		return cloneRaw, nil
	}
	if !isForgeHandle(cloneRaw) {
		return "", fmt.Errorf("fetch: unsupported remote spec %q", cloneRaw)
	}
	// host/owner/repo[/...] — require a non-empty owner and a non-empty trailing
	// repo segment; extra interior segments (subgroups) are allowed.
	segs := strings.Split(cloneRaw, "/")
	if len(segs) < 3 || segs[1] == "" || segs[len(segs)-1] == "" {
		return "", fmt.Errorf("fetch: forge handle %q must be host/owner/repo", cloneRaw)
	}
	url := "https://" + cloneRaw
	if !strings.HasSuffix(url, gitDir) {
		url += gitDir
	}
	return url, nil
}

// cleanSubdir validates and normalizes a #subdir fragment. It rejects absolute
// paths and any path that escapes — or collapses back to — the repo root with
// "..".
//
// SECURITY: only an explicitly empty or "." fragment selects the repo root. A
// non-trivial fragment that path.Clean collapses to "." (e.g. "sub/..", which
// Clean reduces to ".") is a traversal attempt and is rejected, NOT silently
// treated as the root. Accepting it would let "#pack/.." install a DIFFERENT
// bundle than the URL appears to name (the repo root, or a sibling pack in a
// multi-pack repo), defeating the trust the operator places in a published
// "#pack" URL.
func cleanSubdir(frag string) (string, error) {
	trimmed := strings.TrimSpace(frag)
	if trimmed == "" || trimmed == "." {
		return "", nil
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("fetch: subdir %q must stay within the repository", frag)
	}
	return cleaned, nil
}
