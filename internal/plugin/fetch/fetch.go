// Package fetch resolves a remote git spec to a local, pinned, integrity-checked
// schema-pack bundle for `maz preset install <git-url>` and `maz install
// <git-url>` (#224).
//
// It shells out to the system git to clone a spec at a pinned commit into a
// content-addressed cache under the data dir, computes the canonical bundle
// checksum, and — when a checksum is supplied — refuses a mismatch before the
// bundle is ever parsed or installed. Fetching changes only WHERE the bytes come
// from: the caller hands the on-disk bundle to the unchanged
// schemapack.ParseDir + #210 install path, so every downstream trust control
// (parse cap, sanitize/fence, human gate, render-exclusion, privacy marker) is
// reused verbatim.
package fetch

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

const (
	// DefaultGitTimeout bounds each git invocation.
	defaultGitTimeout = 60 * time.Second
	// CacheDirName is the per-data-dir content-addressed pack cache.
	cacheDirName = "packs-cache"
	// CacheKeyLen truncates the hex SHA-256 cache key to 32 chars (128 bits) —
	// ample collision resistance for a content-addressed local cache.
	cacheKeyLen = 32
	// GitDir is the git metadata directory name (skipped when copying a bundle
	// and when scanning a repo for nested bundles) and the conventional ".git"
	// clone-URL suffix.
	gitDir = ".git"
)

// ErrChecksumMismatch is returned when a fetched bundle's computed checksum does
// not match the operator-supplied checksum. The bundle is refused before it is
// parsed, cached, or installed.
var ErrChecksumMismatch = errors.New("fetch: bundle checksum mismatch")

// Options carries inputs the fetcher must not re-derive. DataDir in particular
// is passed in so the CLI's --data-dir / MAZ_DATA_DIR isolation is honored.
type Options struct {
	// DataDir is the resolved data dir; the content cache lives under it.
	DataDir string
	// WantChecksum, when non-empty ("sha256:..."), gates the fetch: a bundle
	// whose computed checksum differs is refused (ErrChecksumMismatch).
	WantChecksum string
	// Timeout bounds each git invocation; 0 means defaultGitTimeout.
	Timeout time.Duration
}

// Fetch resolves arg (a remote git spec) to a local bundle directory, pinned to
// a commit and integrity-checked, and returns the cached bundle dir plus the
// fetch provenance. The caller then runs schemapack.ParseDir on the returned
// dir. The computed checksum is recorded in the returned Provenance regardless
// of whether WantChecksum was supplied.
func Fetch(ctx context.Context, arg string, opts Options) (string, Provenance, error) {
	spec, err := ParseRemoteSpec(arg)
	if err != nil {
		return "", Provenance{}, err
	}
	if err := validateChecksumArg(opts.WantChecksum); err != nil {
		return "", Provenance{}, err
	}
	if err := requireGit(); err != nil {
		return "", Provenance{}, err
	}
	if strings.TrimSpace(opts.DataDir) == "" {
		return "", Provenance{}, errors.New("fetch: DataDir is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultGitTimeout
	}

	cacheRoot := filepath.Join(opts.DataDir, cacheDirName)
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return "", Provenance{}, fmt.Errorf("fetch: create cache dir: %w", err)
	}

	staging, err := os.MkdirTemp(cacheRoot, ".staging-")
	if err != nil {
		return "", Provenance{}, fmt.Errorf("fetch: create staging dir: %w", err)
	}
	//nolint:errcheck // best-effort cleanup of the staging clone
	defer func() { _ = os.RemoveAll(staging) }()

	bundleRoot, prov, err := cloneAndInspect(ctx, cacheRoot, staging, spec, timeout)
	if err != nil {
		return "", prov, err
	}

	// Integrity gate: refuse a mismatch BEFORE the bundle is cached or parsed.
	if opts.WantChecksum != "" && !checksumEqual(prov.Checksum, opts.WantChecksum) {
		return "", prov, fmt.Errorf("%w: fetched %s, wanted %s", ErrChecksumMismatch, prov.Checksum, opts.WantChecksum)
	}

	cacheDir, err := cacheBundle(cacheRoot, bundleRoot, prov)
	if err != nil {
		return "", prov, err
	}
	return cacheDir, prov, nil
}

// cloneAndInspect clones spec into staging, pins HEAD, locates the bundle dir,
// asserts a manifest is present, and computes the canonical checksum. It returns
// the bundle root within staging and the fetch provenance.
func cloneAndInspect(ctx context.Context, cacheRoot, staging string, spec RemoteSpec, timeout time.Duration) (string, Provenance, error) {
	if err := cloneAndCheckout(ctx, cacheRoot, staging, spec, timeout); err != nil {
		return "", Provenance{}, err
	}
	pinned, err := revParseHEAD(ctx, staging, timeout)
	if err != nil {
		return "", Provenance{}, err
	}
	bundleRoot := staging
	if spec.Subdir != "" {
		bundleRoot = filepath.Join(staging, filepath.FromSlash(spec.Subdir))
	}
	if err := assertManifest(bundleRoot, spec); err != nil {
		return "", Provenance{}, err
	}
	sum, err := schemapack.BundleChecksum(os.DirFS(bundleRoot), ".")
	if err != nil {
		return "", Provenance{}, fmt.Errorf("fetch: checksum bundle: %w", err)
	}
	return bundleRoot, Provenance{SourceURL: spec.Display, PinnedRef: pinned, Checksum: sum}, nil
}

// cloneAndCheckout clones spec.CloneURL at spec.Ref into staging, choosing the
// strategy UP FRONT from the ref shape so a real failure (bad ref, auth, network)
// surfaces directly instead of being masked by a fallback:
//   - no ref          : shallow clone of the default branch.
//   - branch/tag name : shallow clone with --branch; its error is returned as-is
//     (a typo'd ref fails fast, never silently triggers a full clone).
//   - commit SHA      : full clone + checkout (`git clone --branch` rejects a
//     SHA), the only case that needs the whole history.
//
// SECURITY: submodules are deliberately NOT recursed (--no-recurse-submodules):
// a submodule's post-checkout would execute attacker-controlled code, and a
// preset bundle never legitimately spans submodules. Argument-injection via the
// ref is not a concern in either remaining path: a branch/tag is passed as the
// VALUE of --branch (git reads it as a ref, never a flag), and the commit-SHA
// checkout below only runs when looksLikeCommitSHA(spec.Ref) is true, which
// requires the ref to be all-hex — so it can never begin with "-". A "--"
// pathspec separator must NOT be used on the commit checkout: `git checkout --
// <sha>` is pathspec/restore mode (it would try to restore a file named by the
// SHA and fail), not the detach-HEAD-at-commit we need.
func cloneAndCheckout(ctx context.Context, cacheRoot, staging string, spec RemoteSpec, timeout time.Duration) error {
	switch {
	case spec.Ref == "":
		_, _, err := runGit(ctx, cacheRoot, timeout, "clone", "--quiet", "--depth", "1", "--no-recurse-submodules", spec.CloneURL, staging)
		return err
	case !looksLikeCommitSHA(spec.Ref):
		_, _, err := runGit(ctx, cacheRoot, timeout, "clone", "--quiet", "--depth", "1", "--no-recurse-submodules", "--branch", spec.Ref, spec.CloneURL, staging)
		return err
	default:
		if _, _, err := runGit(ctx, cacheRoot, timeout, "clone", "--quiet", "--no-recurse-submodules", spec.CloneURL, staging); err != nil {
			return err
		}
		if _, _, err := runGit(ctx, staging, timeout, "checkout", "--quiet", spec.Ref); err != nil {
			return fmt.Errorf("fetch: checkout ref %q: %w", spec.Ref, err)
		}
		return nil
	}
}

// revParseHEAD returns the resolved commit SHA at HEAD in dir. The result must
// be a 40-char (SHA-1) or 64-char (SHA-256) lowercase hex digest; anything else
// means the pin is not a real commit and is refused.
func revParseHEAD(ctx context.Context, dir string, timeout time.Duration) (string, error) {
	out, _, err := runGit(ctx, dir, timeout, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !isCommitSHA(sha) {
		return "", fmt.Errorf("fetch: rev-parse HEAD returned %q, not a commit SHA", sha)
	}
	return sha, nil
}

// isCommitSHA reports whether s is a FULL SHA-1 or SHA-256 hex digest; used to
// validate the resolved pin from rev-parse HEAD (always full-length).
func isCommitSHA(s string) bool {
	return (len(s) == 40 || len(s) == 64) && isHex(s)
}

// looksLikeCommitSHA reports whether s is plausibly a (possibly abbreviated)
// commit SHA — all hex, 7..64 chars — and so must be fetched via a full clone +
// checkout rather than `git clone --branch`, which only accepts ref names. A
// branch/tag merely named like hex still resolves on the checkout path, so the
// only cost of a false positive is a full clone, never a failure.
func looksLikeCommitSHA(s string) bool {
	return len(s) >= 7 && len(s) <= 64 && isHex(s)
}

// isHex reports whether s is non-empty and all hex digits (either case).
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// assertManifest returns an actionable error when bundleRoot has no manifest.
// For a multi-pack repo (no manifest at the root), it enumerates the immediate
// subdirectories that DO hold a manifest so the operator can pick one via
// #subdir, rather than seeing a bare "not found".
func assertManifest(bundleRoot string, spec RemoteSpec) error {
	// Lstat (not Stat) so a symlinked manifest is detected here with lstat
	// semantics, matching BundleChecksum, rather than silently followed.
	switch info, err := os.Lstat(filepath.Join(bundleRoot, schemapack.ManifestFile)); {
	case err == nil && info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%w: %s", schemapack.ErrBundleSymlink, schemapack.ManifestFile)
	case err == nil:
		return nil
	}
	if spec.Subdir != "" {
		return fmt.Errorf("fetch: no %s in subdir %q of %s", schemapack.ManifestFile, spec.Subdir, spec.Display)
	}
	if dirs := discoverBundleDirs(bundleRoot); len(dirs) > 0 {
		return fmt.Errorf("fetch: no %s at the repository root of %s; it is a multi-pack repo — re-run with #subdir set to one of: %s",
			schemapack.ManifestFile, spec.Display, strings.Join(dirs, ", "))
	}
	return fmt.Errorf("fetch: no %s at the repository root of %s; add #subdir to point at the bundle directory",
		schemapack.ManifestFile, spec.Display)
}

// discoverBundleDirs returns the immediate subdirectories of root that contain a
// manifest, sorted, so an ambiguous multi-pack repo can list its bundles.
func discoverBundleDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == gitDir {
			continue
		}
		if _, serr := os.Stat(filepath.Join(root, e.Name(), schemapack.ManifestFile)); serr == nil {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	return dirs
}

// cacheBundle copies the bundle into a content-addressed cache entry keyed by
// source + pinned commit + checksum, and returns the cache dir. A valid existing
// entry is reused (offline-tolerant); its checksum is re-verified before reuse.
func cacheBundle(cacheRoot, bundleRoot string, prov Provenance) (string, error) {
	cacheDir := filepath.Join(cacheRoot, cacheKey(prov.SourceURL, prov.PinnedRef, prov.Checksum))
	if cacheValid(cacheDir, prov.Checksum) {
		return cacheDir, nil
	}
	tmp, err := os.MkdirTemp(cacheRoot, ".materialize-")
	if err != nil {
		return "", fmt.Errorf("fetch: create cache staging: %w", err)
	}
	if err := copyBundleTree(bundleRoot, tmp); err != nil {
		//nolint:errcheck // best-effort cleanup
		_ = os.RemoveAll(tmp)
		return "", err
	}
	//nolint:errcheck // replace any stale entry before publishing
	_ = os.RemoveAll(cacheDir)
	if err := os.Rename(tmp, cacheDir); err != nil {
		//nolint:errcheck // best-effort cleanup
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("fetch: publish cache entry: %w", err)
	}
	return cacheDir, nil
}

// cacheKey returns a stable hex key derived from its parts.
func cacheKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:cacheKeyLen]
}

// cacheValid reports whether dir holds a bundle whose checksum equals want.
func cacheValid(dir, want string) bool {
	if _, err := os.Stat(filepath.Join(dir, schemapack.ManifestFile)); err != nil {
		return false
	}
	got, err := schemapack.BundleChecksum(os.DirFS(dir), ".")
	if err != nil {
		return false
	}
	return checksumEqual(got, want)
}

// copyBundleTree copies src into dst, skipping the .git directory and refusing
// any symlink (SECURITY: a symlink could redirect a later read outside the
// bundle; BundleChecksum rejects symlinked canonical files, this covers the
// rest of the tree).
func copyBundleTree(src, dst string) error {
	walk := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %q: %w", p, err)
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return fmt.Errorf("relativize %q: %w", p, rerr)
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if d.Name() == gitDir {
				return filepath.SkipDir
			}
			if merr := os.MkdirAll(filepath.Join(dst, rel), 0o700); merr != nil {
				return fmt.Errorf("mkdir %q: %w", rel, merr)
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", schemapack.ErrBundleSymlink, rel)
		}
		return copyFile(p, filepath.Join(dst, rel))
	}
	if err := filepath.WalkDir(src, walk); err != nil {
		return fmt.Errorf("fetch: copy bundle tree: %w", err)
	}
	return nil
}

// copyFile copies a single regular file from src to dst (0o600).
func copyFile(src, dst string) error {
	//nolint:gosec // G304: src is a path within the fetched bundle tree; symlinks are rejected before copy.
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("fetch: open %q: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("fetch: mkdir for %q: %w", dst, err)
	}
	//nolint:gosec // G304: dst is inside the content-addressed cache dir this process created.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fetch: create %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("fetch: copy %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("fetch: close %q: %w", dst, err)
	}
	return nil
}

// validateChecksumArg rejects a non-empty --checksum that is not a well-formed
// "sha256:<64 hex>" string, up front, so a typo surfaces as a clear format error
// before any clone — rather than as an opaque post-fetch mismatch comparing the
// real bundle checksum against garbage. An empty arg is allowed (the gate is
// optional; the checksum is still recorded as provenance).
func validateChecksumArg(want string) error {
	if strings.TrimSpace(want) == "" {
		return nil
	}
	prefix := schemapack.ChecksumAlgo + ":"
	hexPart, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(want)), prefix)
	if !ok || len(hexPart) != sha256.Size*2 || !isHex(hexPart) {
		return fmt.Errorf("fetch: --checksum must be %q followed by %d hex chars, got %q",
			prefix, sha256.Size*2, want)
	}
	return nil
}

// checksumEqual compares two "sha256:..." checksums in constant time. Length is
// not secret (checksums are fixed-width), so a length-mismatch short-circuit is
// safe and keeps the constant-time compare on equal-length inputs.
func checksumEqual(a, b string) bool {
	na := strings.ToLower(strings.TrimSpace(a))
	nb := strings.ToLower(strings.TrimSpace(b))
	if len(na) != len(nb) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(na), []byte(nb)) == 1
}
