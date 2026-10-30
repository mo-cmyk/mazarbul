package selfupdate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InstallMethod classifies how the running maz was installed, which decides
// whether self-replacement is allowed.
type InstallMethod int

const (
	// MethodUnknown means the binary is not in a recognized bin/+lib/ layout.
	MethodUnknown InstallMethod = iota
	// MethodPrefix is a curl|sh / manual install under a writable prefix; it is
	// safe to self-replace.
	MethodPrefix
	// MethodHomebrew is a Homebrew-managed install; upgrade defers to brew.
	MethodHomebrew
)

// Location describes where the running maz lives and how it was installed.
type Location struct {
	// Exe is the symlink-resolved path of the running binary.
	Exe string
	// BinDir is the directory containing the binary.
	BinDir string
	// Prefix is the install prefix (parent of BinDir).
	Prefix string
	// LibDir is "<Prefix>/lib", where the liblbug engine library lives.
	LibDir string
	// Method is the detected install method.
	Method InstallMethod
}

// ManDir returns the man-page install directory for an install prefix
// ("<prefix>/share/man/man1"), matching what install.sh writes.
func ManDir(prefix string) string {
	return filepath.Join(prefix, "share", "man", "man1")
}

// homebrewMarkers are path fragments that identify a Homebrew-managed binary.
// A formula install symlinks "<prefix>/bin/maz" into a Cellar keg, so the
// resolved path carries "/Cellar/"; the prefix roots cover non-Cellar layouts.
var homebrewMarkers = []string{"/Cellar/", "/opt/homebrew/", "/home/linuxbrew/.linuxbrew/"} //nolint:gochecknoglobals // immutable lookup table

// DetectLocation resolves the running binary's install location and method.
// Exe should come from daemon.ResolveExecutable(); it is symlink-resolved here
// so a Homebrew symlink classifies against its real Cellar path.
func DetectLocation(exe string) Location {
	real := exe
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		real = r
	}
	binDir := filepath.Dir(real)
	prefix := filepath.Dir(binDir)
	return Location{
		Exe:    real,
		BinDir: binDir,
		Prefix: prefix,
		LibDir: filepath.Join(prefix, "lib"),
		Method: classify(real, binDir),
	}
}

// classify decides the install method from the resolved paths.
func classify(real, binDir string) InstallMethod {
	if isHomebrew(real) || isHomebrew(binDir) {
		return MethodHomebrew
	}
	if filepath.Base(binDir) == "bin" {
		return MethodPrefix
	}
	return MethodUnknown
}

// isHomebrew reports whether p sits under a Homebrew prefix or Cellar.
func isHomebrew(p string) bool {
	withSep := p + string(filepath.Separator)
	for _, m := range homebrewMarkers {
		if strings.Contains(withSep, m) {
			return true
		}
	}
	return false
}

// EnsureWritable confirms both the bin and lib directories are writable by
// probing with a temp file. Stat mode bits are unreliable under ACLs or sudo,
// so an actual write is the only honest check. It is called before any swap so
// a permission problem fails up front rather than half-way through.
func (l Location) EnsureWritable() error {
	for _, d := range []string{l.BinDir, l.LibDir} {
		tmp, err := os.CreateTemp(d, ".maz-upgrade-probe-*")
		if err != nil {
			return fmt.Errorf("cannot write to %s (need write access to bin/ and lib/): %w", d, err)
		}
		name := tmp.Name()
		_ = tmp.Close()
		_ = os.Remove(name) //nolint:errcheck // best-effort cleanup of the probe file
	}
	return nil
}

// replaceOp is one staged file to move into place.
type replaceOp struct {
	target string      // final live path
	source string      // staged source path under the staging tree
	isLink bool        // true when source is a symlink to recreate
	mode   os.FileMode // file mode for a regular file
	link   string      // symlink target when isLink
}

// Swap atomically replaces the live install's binary and engine library with the
// staged tree, then prunes stale liblbug entries the new release dropped. It
// stages every replacement as a sibling ".new" file first, then commits each
// with an atomic rename; on any failure mid-commit it restores from the ".bak"
// siblings so the install is never left half-updated. POSIX rename over a
// running binary is safe (the open inode persists), so a live daemon keeps
// running the old code until restarted.
func Swap(stageDir string, live Location) error {
	ops, err := planSwap(stageDir, live)
	if err != nil {
		return err
	}
	prune, err := planPrune(stageDir, live)
	if err != nil {
		return err
	}

	staged, err := stageAll(ops)
	if err != nil {
		removeAll(staged)
		return err
	}

	committed, err := commitAll(ops, prune)
	if err != nil {
		restoreErrs := rollback(committed)
		removeAll(staged)
		if len(restoreErrs) > 0 {
			// The commit failed and rollback could not fully restore the prior
			// install: surface both so the user knows maz may be inconsistent.
			return fmt.Errorf("%w (rollback also failed, install may be inconsistent: %w)", err, errors.Join(restoreErrs...))
		}
		return err
	}
	// Success: drop the backups.
	for _, c := range committed {
		if c.backup != "" {
			_ = os.Remove(c.backup) //nolint:errcheck // best-effort backup cleanup
		}
	}
	return nil
}

// InstallManPages copies the staged man pages into the live man directory. It is
// best-effort and intentionally separate from Swap: man pages are supplementary
// documentation, so a man-dir write failure must not abort an otherwise-good
// binary/lib upgrade. It returns nil when the staged tree carries no man pages
// (e.g. an archive built before man pages were bundled).
func InstallManPages(stageDir, manDir string) error {
	srcDir := filepath.Join(stageDir, "man", "man1")
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("selfupdate: read staged man: %w", err)
	}
	// 0o755 (not 0o750): man pages must stay world-readable/traversable so a
	// shared/system install (e.g. /usr/local) lets any user run `man maz`,
	// matching install.sh's umask-default mkdir.
	if err := os.MkdirAll(manDir, 0o755); err != nil { //nolint:gosec // G301: man pages must stay world-readable on shared installs (matches install.sh)
		return fmt.Errorf("selfupdate: mkdir %s: %w", manDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".1") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			return fmt.Errorf("selfupdate: stat staged man %s: %w", e.Name(), ierr)
		}
		if cerr := copyManPage(filepath.Join(srcDir, e.Name()), filepath.Join(manDir, e.Name()), info.Mode().Perm()); cerr != nil {
			return cerr
		}
	}
	return nil
}

// copyManPage installs a single staged man page atomically: it writes to a
// sibling ".tmp" file and renames it over the destination only after a clean
// close, so a write failure (e.g. a full disk) leaves any pre-existing page
// intact instead of truncating it to a partial or zero-byte file. It preserves
// the staged file's mode (0644 in a release archive).
func copyManPage(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) // #nosec G304 -- src is under a caller-created staging dir
	if err != nil {
		return fmt.Errorf("selfupdate: open %s: %w", src, err)
	}
	defer in.Close()
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)                                                     //nolint:errcheck // clear any leftover from a prior aborted run
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) // #nosec G304 -- tmp is a sibling of the resolved install destination
	if err != nil {
		return fmt.Errorf("selfupdate: create %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp) //nolint:errcheck // best-effort cleanup of the partial temp file
		return fmt.Errorf("selfupdate: write %s: %w", tmp, err)
	}
	// No Sync before Close (unlike stageOne): man-page install is best-effort
	// and nothing reads these pages back during the upgrade, so we accept the
	// small risk of a partial temp page after an ill-timed crash. The rename is
	// what protects the pre-existing page on a normal write failure.
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort cleanup of the temp file
		return fmt.Errorf("selfupdate: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort cleanup of the temp file
		return fmt.Errorf("selfupdate: install %s: %w", dst, err)
	}
	return nil
}

// planSwap builds the replace operations: the binary plus every staged lib
// entry, mapped to their live destinations.
func planSwap(stageDir string, live Location) ([]replaceOp, error) {
	ops := []replaceOp{{
		target: filepath.Join(live.BinDir, "maz"),
		source: filepath.Join(stageDir, "bin", "maz"),
		mode:   0o755,
	}}
	stageLib := filepath.Join(stageDir, "lib")
	entries, err := os.ReadDir(stageLib)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: read staged lib: %w", err)
	}
	for _, e := range entries {
		src := filepath.Join(stageLib, e.Name())
		info, err := os.Lstat(src)
		if err != nil {
			return nil, fmt.Errorf("selfupdate: stat staged lib %s: %w", src, err)
		}
		op := replaceOp{target: filepath.Join(live.LibDir, e.Name()), source: src, mode: info.Mode().Perm()}
		if info.Mode()&os.ModeSymlink != 0 {
			link, rerr := os.Readlink(src)
			if rerr != nil {
				return nil, fmt.Errorf("selfupdate: read staged symlink %s: %w", src, rerr)
			}
			op.isLink = true
			op.link = link
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// planPrune lists live liblbug.* entries the new release no longer ships, so a
// version bump that renames the engine library does not leave a stale copy.
func planPrune(stageDir string, live Location) ([]string, error) {
	keep := map[string]bool{}
	stageEntries, err := os.ReadDir(filepath.Join(stageDir, "lib"))
	if err != nil {
		return nil, fmt.Errorf("selfupdate: read staged lib: %w", err)
	}
	for _, e := range stageEntries {
		keep[e.Name()] = true
	}
	liveEntries, err := os.ReadDir(live.LibDir)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: read live lib: %w", err)
	}
	var prune []string
	for _, e := range liveEntries {
		if strings.HasPrefix(e.Name(), "liblbug.") && !keep[e.Name()] {
			prune = append(prune, filepath.Join(live.LibDir, e.Name()))
		}
	}
	return prune, nil
}

// stageAll writes every op's ".new" sibling next to its target. It returns the
// staged paths so a later failure can clean them up.
func stageAll(ops []replaceOp) ([]string, error) {
	staged := make([]string, 0, len(ops))
	for _, op := range ops {
		newPath := op.target + ".new"
		if err := stageOne(op, newPath); err != nil {
			return staged, err
		}
		staged = append(staged, newPath)
	}
	return staged, nil
}

// stageOne materializes a single ".new" file (a copied regular file or a
// recreated symlink).
func stageOne(op replaceOp, newPath string) error {
	_ = os.Remove(newPath) //nolint:errcheck // clear any leftover from a prior aborted run
	if op.isLink {
		if err := os.Symlink(op.link, newPath); err != nil {
			return fmt.Errorf("selfupdate: stage symlink %s: %w", newPath, err)
		}
		return nil
	}
	src, err := os.Open(op.source) // #nosec G304 -- source is under a caller-created staging dir
	if err != nil {
		return fmt.Errorf("selfupdate: open staged %s: %w", op.source, err)
	}
	defer src.Close()
	dst, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, op.mode) // #nosec G304 -- newPath is a sibling of the live target
	if err != nil {
		return fmt.Errorf("selfupdate: create %s: %w", newPath, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("selfupdate: write %s: %w", newPath, err)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("selfupdate: sync %s: %w", newPath, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("selfupdate: close %s: %w", newPath, err)
	}
	return nil
}

// committedOp records a completed rename so it can be rolled back.
type committedOp struct {
	target  string // final path that now holds the new content (empty for a prune)
	backup  string // ".bak" holding the prior content, "" if there was none
	newPath string // staged ".new" path, restored on rollback of a replace
}

// commitAll moves staged files into place and removes pruned files, recording
// each step for rollback. Each replace is: rename target -> .bak (if present),
// then rename .new -> target. Each prune is: rename target -> .bak.
func commitAll(ops []replaceOp, prune []string) ([]committedOp, error) {
	done := make([]committedOp, 0, len(ops)+len(prune))
	for _, op := range ops {
		c := committedOp{target: op.target, newPath: op.target + ".new"}
		if _, err := os.Lstat(op.target); err == nil {
			c.backup = op.target + ".bak"
			if err := os.Rename(op.target, c.backup); err != nil {
				return done, fmt.Errorf("selfupdate: back up %s: %w", op.target, err)
			}
		}
		if err := os.Rename(c.newPath, op.target); err != nil {
			return append(done, committedOp{target: op.target, backup: c.backup}), fmt.Errorf("selfupdate: install %s: %w", op.target, err)
		}
		done = append(done, c)
	}
	for _, target := range prune {
		c := committedOp{target: target, backup: target + ".bak"}
		if err := os.Rename(target, c.backup); err != nil {
			return done, fmt.Errorf("selfupdate: prune %s: %w", target, err)
		}
		done = append(done, c)
	}
	return done, nil
}

// rollback restores the prior install from the recorded backups, in reverse
// order. It is best-effort and continues past individual failures, returning
// every restore error so the caller can warn that the install may be left
// inconsistent (a rollback failure cannot itself be cleanly recovered).
func rollback(done []committedOp) []error {
	var errs []error
	for i := len(done) - 1; i >= 0; i-- {
		c := done[i]
		if c.backup != "" {
			if err := os.Rename(c.backup, c.target); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", c.target, err))
			}
			continue
		}
		// No prior file existed: remove the newly-installed one.
		if err := os.Remove(c.target); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", c.target, err))
		}
	}
	return errs
}

// removeAll deletes the given (staged ".new") paths, best-effort.
func removeAll(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p) //nolint:errcheck // best-effort cleanup
	}
}

// errNoLib is returned when a prefix install is missing its lib directory.
var errNoLib = errors.New("selfupdate: install lib directory not found")

// ValidatePrefix confirms a MethodPrefix location has the bin and lib dirs that
// Swap will write into.
func (l Location) ValidatePrefix() error {
	if fi, err := os.Stat(l.BinDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("selfupdate: install bin directory %s not found", l.BinDir)
	}
	if fi, err := os.Stat(l.LibDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%w: %s", errNoLib, l.LibDir)
	}
	return nil
}
