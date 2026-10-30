package schemapack

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// ChecksumAlgo is the hash algorithm prefix of a bundle checksum string, e.g.
// the "sha256" in "sha256:<hex>".
const ChecksumAlgo = "sha256"

// MaxBundleBytes caps the combined size of the canonical bundle file set
// (manifest.toml + predicates.yaml + instructions.md + every kinds/*.yaml) that
// BundleChecksum will read and hash. A real schema pack is a handful of small
// TOML/YAML/markdown files — kilobytes — so this 4 MiB ceiling never bites a
// legitimate bundle. It exists only to bound a hostile remote bundle (#224): a
// multi-megabyte kinds/*.yaml would otherwise be cloned, then fully read into
// memory and hashed before any later validator could object. The cap is checked
// against on-disk file sizes (via fs.Stat) BEFORE each file is read, so an
// oversized member is refused without ever being slurped into memory.
const MaxBundleBytes = 4 << 20

// ErrBundleSymlink is returned when a canonical bundle file (manifest.toml,
// predicates.yaml, instructions.md, or a kinds/ entry) is a symlink. A symlink
// in the bundle could redirect a read to an arbitrary file the operator can
// read (e.g. instructions.md -> ~/.ssh/id_ed25519), which would then be hashed
// here and surfaced in the install review block. Rejecting symlinks among the
// files we actually read keeps the checksum honest and is the path-traversal
// defense the remote-install human gate relies on (#224).
var ErrBundleSymlink = errors.New("schemapack: bundle contains a symlink")

// ErrBundleTooLarge is returned when the canonical bundle file set exceeds
// MaxBundleBytes in total on-disk size.
var ErrBundleTooLarge = errors.New("schemapack: bundle exceeds size cap")

// BundleChecksum computes a deterministic content checksum over the CANONICAL
// bundle file set — manifest.toml, predicates.yaml, instructions.md, and the
// kinds/*.yaml|*.yml files — and returns it as "sha256:<hex>".
//
// Only the canonical set is hashed, not the whole tree: a real schema-pack repo
// also carries README/LICENSE/CI files, and a whole-tree hash would change on an
// unrelated README edit (breaking a published --checksum pin) and would be
// non-reproducible from `maz preset package`, which only sees the bundle dir.
// Hashing exactly the files ParseDir consumes makes the checksum a stable
// identity of the installable bundle, identical whether computed by the remote
// fetcher or by the author's `maz preset package`.
//
// The hash is a single running sha256 over each canonical file in sorted
// slash-relative path order, framed as: path-bytes || 0x00 ||
// uint64LE(len(content)) || raw-content. The length prefix removes any
// path/content boundary ambiguity. Raw bytes are hashed (no line-ending
// normalization), so authors must commit bundles with LF endings
// (.gitattributes `text eol=lf`) for a cross-platform-stable checksum.
//
// SECURITY: a canonical file (or the kinds/ directory) that is a symlink is
// rejected with ErrBundleSymlink before any content is read.
func BundleChecksum(fsys fs.FS, root string) (string, error) {
	paths, err := canonicalBundleFiles(fsys, root)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	var lenbuf [8]byte
	var total int64
	for _, rel := range paths {
		full := path.Join(root, rel)
		// Stat first (no content read) and reject the whole set the moment its
		// cumulative size would exceed the cap — bounds memory before ReadFile
		// slurps a hostile multi-megabyte member.
		info, serr := fs.Stat(fsys, full)
		if serr != nil {
			return "", fmt.Errorf("schemapack: stat %q for checksum: %w", rel, serr)
		}
		total += info.Size()
		if total > MaxBundleBytes {
			return "", fmt.Errorf("%w: canonical files exceed cap %d bytes", ErrBundleTooLarge, MaxBundleBytes)
		}
		data, rerr := fs.ReadFile(fsys, full)
		if rerr != nil {
			return "", fmt.Errorf("schemapack: read %q for checksum: %w", rel, rerr)
		}
		// Hash the slash-relative path (independent of the containing dir name),
		// a NUL separator, the content length, then the raw content.
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		binary.LittleEndian.PutUint64(lenbuf[:], uint64(len(data)))
		_, _ = h.Write(lenbuf[:])
		_, _ = h.Write(data)
	}
	return ChecksumAlgo + ":" + hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalBundleFiles returns the sorted slash-relative paths of the canonical
// bundle files present under root. Manifest.toml is required; predicates.yaml,
// instructions.md and the kinds/ directory are optional. A symlinked canonical
// file or kinds/ directory is rejected (ErrBundleSymlink): DirEntry.Type comes
// from lstat, so a symlink is detected here without ever following it.
func canonicalBundleFiles(fsys fs.FS, root string) ([]string, error) {
	top, err := readBundleRoot(fsys, root)
	if err != nil {
		return nil, err
	}

	manifest, ok := top[ManifestFile]
	if !ok {
		return nil, fmt.Errorf("%w: missing %s", ErrInvalidManifest, ManifestFile)
	}
	if err := assertNotSymlink(ManifestFile, manifest); err != nil {
		return nil, err
	}
	paths := []string{ManifestFile}

	for _, name := range []string{PredicatesFile, InstructionsFile} {
		e, present := top[name]
		if !present {
			continue
		}
		if err := assertNotSymlink(name, e); err != nil {
			return nil, err
		}
		paths = append(paths, name)
	}

	kinds, err := canonicalKindFiles(fsys, root, top)
	if err != nil {
		return nil, err
	}
	paths = append(paths, kinds...)

	sort.Strings(paths)
	return paths, nil
}

// readBundleRoot reads root and returns its direct entries keyed by name.
func readBundleRoot(fsys fs.FS, root string) (map[string]fs.DirEntry, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("schemapack: read bundle root for checksum: %w", err)
	}
	top := make(map[string]fs.DirEntry, len(entries))
	for _, e := range entries {
		top[e.Name()] = e
	}
	return top, nil
}

// canonicalKindFiles returns the slash-relative kinds/*.yaml|*.yml paths, or nil
// when there is no kinds/ directory. A symlinked kinds/ directory or kind file
// is rejected with ErrBundleSymlink.
func canonicalKindFiles(fsys fs.FS, root string, top map[string]fs.DirEntry) ([]string, error) {
	ke, present := top[KindsDir]
	if !present {
		return nil, nil
	}
	if err := assertNotSymlink(KindsDir, ke); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(fsys, path.Join(root, KindsDir))
	if err != nil {
		return nil, fmt.Errorf("schemapack: read kinds dir for checksum: %w", err)
	}
	kinds := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		rel := path.Join(KindsDir, name)
		if err := assertNotSymlink(rel, e); err != nil {
			return nil, err
		}
		kinds = append(kinds, rel)
	}
	return kinds, nil
}

// assertNotSymlink returns ErrBundleSymlink when e is a symlink.
func assertNotSymlink(rel string, e fs.DirEntry) error {
	if e.Type()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrBundleSymlink, rel)
	}
	return nil
}
