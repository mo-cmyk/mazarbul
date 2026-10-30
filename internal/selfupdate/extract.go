package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxExtractBytes bounds the total uncompressed size written during extraction,
// guarding against a decompression bomb (a small archive that expands to fill
// the disk).
const maxExtractBytes = 512 << 20 // 512 MiB

// smokeTimeout bounds the staged-binary smoke test.
const smokeTimeout = 15 * time.Second

// Extract unpacks the release archive into a staging tree that mirrors the live
// install layout: the top-level "maz" lands at "<stageDir>/bin/maz" and the
// "lib/" tree lands at "<stageDir>/lib/", so the staged binary resolves liblbug
// via its "../lib" rpath for the smoke test. Man pages are staged under
// "<stageDir>/man/" so the upgrade can install them; other archive members
// (completions, docs) are skipped, matching what install.sh actually places.
//
// It strips the single top-level directory (as install.sh does with
// --strip-components=1), preserves the liblbug symlink chain, and rejects any
// entry whose path or symlink target escapes the staging tree.
func Extract(archivePath, stageDir string) error {
	f, err := os.Open(archivePath) // #nosec G304 -- archivePath is under a caller-created temp dir
	if err != nil {
		return fmt.Errorf("selfupdate: open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("selfupdate: gunzip archive: %w", err)
	}
	defer gz.Close() //nolint:errcheck // read-only

	tr := tar.NewReader(gz)
	remaining := int64(maxExtractBytes)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("selfupdate: read archive: %w", err)
		}
		if err := extractEntry(stageDir, hdr, tr, &remaining); err != nil {
			return err
		}
	}

	if _, err := os.Stat(filepath.Join(stageDir, "bin", "maz")); err != nil {
		return fmt.Errorf("selfupdate: archive did not contain maz: %w", err)
	}
	if !hasLiblbug(filepath.Join(stageDir, "lib")) {
		return errors.New("selfupdate: archive lib/ contains no liblbug.* engine library")
	}
	return nil
}

// extractEntry validates and writes a single archive entry into the staging
// tree, advancing the remaining extraction budget. Entries that are not maz or
// under lib/ are skipped.
func extractEntry(stageDir string, hdr *tar.Header, tr io.Reader, remaining *int64) error {
	rel, ok := stripFirst(hdr.Name)
	if !ok {
		return nil // the top-level dir entry itself
	}
	target, want := stageTarget(stageDir, rel)
	if !want {
		return nil // not maz or lib/*
	}
	if !withinDir(stageDir, target) {
		return fmt.Errorf("selfupdate: archive entry %q escapes staging dir", hdr.Name)
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o750); err != nil {
			return fmt.Errorf("selfupdate: mkdir %s: %w", target, err)
		}
	case tar.TypeReg:
		n, err := writeReg(target, tr, hdr.FileInfo().Mode().Perm(), *remaining)
		if err != nil {
			return err
		}
		*remaining -= n
		if *remaining < 0 {
			_ = os.Remove(target) //nolint:errcheck // best-effort cleanup of the oversize partial
			return fmt.Errorf("selfupdate: archive exceeds %d bytes uncompressed", maxExtractBytes)
		}
	case tar.TypeSymlink:
		if err := writeSymlink(stageDir, target, hdr.Linkname); err != nil {
			return err
		}
	default:
		// Skip char/block devices, fifos, hardlinks: a release archive
		// contains only regular files, dirs, and the liblbug symlinks.
	}
	return nil
}

// stripFirst drops the first path segment (the versioned wrapper dir), returning
// the remainder and whether one exists.
func stripFirst(name string) (string, bool) {
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(name)), "./")
	i := strings.IndexByte(clean, '/')
	if i < 0 {
		return "", false
	}
	rest := clean[i+1:]
	if rest == "" {
		return "", false
	}
	return rest, true
}

// stageTarget maps a stripped archive path to its staging destination, or
// reports want=false for members the upgrade does not place.
func stageTarget(stageDir, rel string) (target string, want bool) {
	switch {
	case rel == "maz":
		return filepath.Join(stageDir, "bin", "maz"), true
	case strings.HasPrefix(rel, "lib/"):
		return filepath.Join(stageDir, filepath.FromSlash(rel)), true
	case strings.HasPrefix(rel, "man/"):
		return filepath.Join(stageDir, filepath.FromSlash(rel)), true
	default:
		return "", false
	}
}

// withinDir reports whether target is inside dir (no traversal escape).
func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// writeReg writes a regular file, capped at the remaining extraction budget so a
// single oversized member cannot blow past maxExtractBytes. It returns the
// number of bytes written.
func writeReg(target string, r io.Reader, mode os.FileMode, remaining int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return 0, fmt.Errorf("selfupdate: mkdir %s: %w", filepath.Dir(target), err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) // #nosec G304 -- target validated within stageDir
	if err != nil {
		return 0, fmt.Errorf("selfupdate: create %s: %w", target, err)
	}
	n, err := io.Copy(f, io.LimitReader(r, remaining+1))
	if err != nil {
		_ = f.Close()
		return n, fmt.Errorf("selfupdate: write %s: %w", target, err)
	}
	// Sync before close, matching stageOne in install.go: the staged tree is read
	// back by the swap (and could be by a re-run after a crash), so flush it.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return n, fmt.Errorf("selfupdate: sync %s: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return n, fmt.Errorf("selfupdate: close %s: %w", target, err)
	}
	return n, nil
}

// writeSymlink recreates a symlink, rejecting an absolute target or one that
// escapes the staging tree (the liblbug chain is relative within lib/).
func writeSymlink(stageDir, target, linkname string) error {
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("selfupdate: symlink %s has absolute target %q", target, linkname)
	}
	resolved := filepath.Join(filepath.Dir(target), linkname)
	if !withinDir(stageDir, resolved) {
		return fmt.Errorf("selfupdate: symlink %s -> %q escapes staging dir", target, linkname)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("selfupdate: mkdir %s: %w", filepath.Dir(target), err)
	}
	if err := os.Symlink(linkname, target); err != nil {
		return fmt.Errorf("selfupdate: symlink %s: %w", target, err)
	}
	return nil
}

// hasLiblbug reports whether libDir contains at least one liblbug.* entry.
func hasLiblbug(libDir string) bool {
	entries, err := os.ReadDir(libDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "liblbug.") {
			return true
		}
	}
	return false
}

// Smoke runs the staged binary's "version --output json" to confirm it executes
// and resolves the engine library before any live file is touched. It is run
// only on a checksum-verified, freshly extracted tree.
func Smoke(ctx context.Context, stageDir string) error {
	bin := filepath.Join(stageDir, "bin", "maz")
	ctx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version", "--output", "json").Output() // #nosec G204 -- bin is the just-extracted, checksum-verified maz
	if err != nil {
		return fmt.Errorf("selfupdate: staged binary failed to run (engine lib not resolved?): %w", err)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return fmt.Errorf("selfupdate: staged binary produced unparseable version output: %w", err)
	}
	if v.Version == "" {
		return errors.New("selfupdate: staged binary reported an empty version")
	}
	return nil
}
