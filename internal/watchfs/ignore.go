package watchfs

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode/utf8"

	gitignore "github.com/sabhiram/go-gitignore"
	"go.uber.org/zap"
)

// defaultDenylistDirs are directory base names never watched or
// captured: VCS metadata, dependency caches, and build output. These are
// skipped even when no .gitignore is present, so the watcher is sane on a
// non-git directory.
//
//nolint:gochecknoglobals // read-only denylist, never mutated
var defaultDenylistDirs = map[string]struct{}{
	".git": {}, ".hg": {}, ".svn": {}, ".bzr": {},
	"node_modules": {}, "bower_components": {}, "vendor": {},
	"dist": {}, "build": {}, "out": {}, "target": {}, "bin": {}, "obj": {},
	".venv": {}, "venv": {}, "__pycache__": {}, ".mypy_cache": {}, ".pytest_cache": {}, ".tox": {},
	".idea": {}, ".vscode": {}, ".cache": {}, ".next": {}, ".nuxt": {}, ".svelte-kit": {},
	".gradle": {}, ".terraform": {}, ".dart_tool": {}, "Pods": {},
}

// binaryExts are file extensions captured as binary by default — skipped
// up front (before any read) so a delete of a known-binary path is
// filtered too. Content-based detection (isProbablyText) is the
// authoritative second gate for everything that slips past this list.
//
//nolint:gochecknoglobals // read-only lookup, never mutated
var binaryExts = map[string]struct{}{
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".bmp": {}, ".ico": {}, ".webp": {}, ".tiff": {}, ".heic": {},
	".pdf": {}, ".zip": {}, ".gz": {}, ".tar": {}, ".tgz": {}, ".bz2": {}, ".xz": {}, ".7z": {}, ".rar": {},
	".mp3": {}, ".mp4": {}, ".wav": {}, ".flac": {}, ".avi": {}, ".mov": {}, ".mkv": {}, ".webm": {}, ".ogg": {},
	".exe": {}, ".dll": {}, ".so": {}, ".dylib": {}, ".a": {}, ".o": {}, ".class": {}, ".jar": {},
	".bin": {}, ".dat": {}, ".db": {}, ".sqlite": {}, ".sqlite3": {}, ".wasm": {},
	".woff": {}, ".woff2": {}, ".ttf": {}, ".otf": {}, ".eot": {},
	".pyc": {}, ".pyo": {},
}

// ignorer decides which directories to descend into and which file paths
// to capture. It combines a static denylist, an extension-based binary
// filter, optional extra patterns, and the .gitignore files found from
// each watched root down through its subdirectories.
type ignorer struct {
	roots []string
	extra *gitignore.GitIgnore // compiled from Options.ExtraIgnores; applied to every root
	// matchers memoizes one compiled .gitignore per absolute directory (nil
	// when that dir has none). git applies a directory's .gitignore to
	// paths beneath it, so a path is tested against every ancestor dir's
	// matcher from its own directory up to the watched root — honouring
	// nested .gitignore files, not just the root's. Populated lazily on the
	// single Run goroutine (and pre-seeded for roots in newIgnorer), so it
	// needs no lock, matching the debounce-map invariant.
	matchers map[string]*gitignore.GitIgnore
	log      *zap.Logger
}

// newIgnorer pre-seeds each root's own .gitignore (to surface an unreadable
// one early) and compiles any extra patterns. Nested .gitignore files are
// loaded lazily as paths under them are checked.
func newIgnorer(roots, extra []string, logger *zap.Logger) *ignorer {
	ig := &ignorer{
		roots:    roots,
		matchers: make(map[string]*gitignore.GitIgnore),
		log:      logger,
	}
	for _, r := range roots {
		ig.matcherForDir(r)
	}
	if len(extra) > 0 {
		ig.extra = gitignore.CompileIgnoreLines(extra...)
	}
	return ig
}

// matcherForDir returns the compiled .gitignore for dir, memoizing the
// result (including a nil for a dir that has none). A present-but-unreadable
// .gitignore is logged at warn (silently not applying its rules would
// otherwise capture files the operator expected to be ignored) and treated
// as absent.
func (ig *ignorer) matcherForDir(dir string) *gitignore.GitIgnore {
	if m, ok := ig.matchers[dir]; ok {
		return m
	}
	path := filepath.Join(dir, ".gitignore")
	gi, err := gitignore.CompileIgnoreFile(path)
	var m *gitignore.GitIgnore
	switch {
	case err == nil:
		m = gi
	case !errors.Is(err, fs.ErrNotExist):
		ig.log.Warn("watchfs: .gitignore present but unreadable; its rules will not apply",
			zap.String("path", path), zap.Error(err))
	}
	ig.matchers[dir] = m
	return m
}

// ignoreDir reports whether a directory should not be watched (and its
// subtree skipped). True when the base name is denylisted or a gitignore
// rule matches the directory.
func (ig *ignorer) ignoreDir(path string) bool {
	if _, ok := defaultDenylistDirs[filepath.Base(path)]; ok {
		return true
	}
	root, _, ok := rootFor(ig.roots, path)
	if !ok {
		return false
	}
	return ig.gitignored(root, path, true)
}

// ignoreFile reports whether a file path should not be captured. True
// when any relative path component is denylisted, the extension is a
// known binary type, or a gitignore rule matches. The path need not
// exist (deletes are filtered the same way).
func (ig *ignorer) ignoreFile(path string) bool {
	root, rel, ok := rootFor(ig.roots, path)
	if !ok {
		return true // outside every root: not ours to capture
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if _, denied := defaultDenylistDirs[part]; denied {
			return true
		}
	}
	if _, ok := binaryExts[strings.ToLower(filepath.Ext(path))]; ok {
		return true
	}
	return ig.gitignored(root, path, false)
}

// gitignored reports whether absPath is matched by the extra patterns or by
// any ancestor directory's .gitignore, from the path's own directory up to
// and including root. When isDir is true the trailing-slash form is also
// tested so a directory-only rule like "build/" matches a directory.
func (ig *ignorer) gitignored(root, absPath string, isDir bool) bool {
	// The watched root itself is never ignored: it has no in-tree ancestor,
	// and consulting .gitignore files above the root (filepath.Dir(root) and
	// up) would let an unrelated parent repo's rule matching the root's
	// basename skip the entire tree, silently capturing nothing.
	if absPath == root {
		return false
	}
	if ig.extra != nil {
		if rel, err := filepath.Rel(root, absPath); err == nil && ig.matches(ig.extra, filepath.ToSlash(rel), isDir) {
			return true
		}
	}
	// Each dir's .gitignore applies to paths relative to that dir, so walk
	// from the path's parent up to the root.
	for d := filepath.Dir(absPath); ; d = filepath.Dir(d) {
		if m := ig.matcherForDir(d); m != nil {
			if rel, err := filepath.Rel(d, absPath); err == nil && ig.matches(m, filepath.ToSlash(rel), isDir) {
				return true
			}
		}
		if d == root {
			break
		}
		if parent := filepath.Dir(d); parent == d {
			break // reached the filesystem root without meeting root; stop
		}
	}
	return false
}

// matches tests rel against gi, also trying the directory (trailing-slash)
// form when isDir so a directory-only rule matches the directory itself.
func (ig *ignorer) matches(gi *gitignore.GitIgnore, rel string, isDir bool) bool {
	if gi.MatchesPath(rel) {
		return true
	}
	return isDir && gi.MatchesPath(rel+"/")
}

// rootFor returns the watched root containing path, the slash-separated
// relative path, and whether a containing root was found. Paths equal to
// a root return rel ".".
func rootFor(roots []string, path string) (root, rel string, ok bool) {
	for _, r := range roots {
		rp, err := filepath.Rel(r, path)
		if err != nil {
			continue
		}
		if rp == ".." || strings.HasPrefix(rp, ".."+string(filepath.Separator)) {
			continue // path is outside r
		}
		return r, filepath.ToSlash(rp), true
	}
	return "", "", false
}

// isProbablyText reports whether b looks like text rather than binary,
// using git's heuristic: a NUL byte marks binary, and the remainder must
// be valid UTF-8. A capped read can clip a multi-byte rune at the tail,
// so up to three trailing bytes are trimmed before declaring invalid.
func isProbablyText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	if utf8.Valid(b) {
		return true
	}
	for i := 1; i <= 3 && i < len(b); i++ {
		if utf8.Valid(b[:len(b)-i]) {
			return true
		}
	}
	return false
}
