package schemapack_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

func writeChecksumBundle(t *testing.T, withInstructions bool) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "manifest.toml"), `name = "crm"
version = "0.1.0"
description = "CRM vocab."
category = "extension"
kind = "schema-pack"
`)
	writeFile(t, filepath.Join(dir, "predicates.yaml"),
		"predicates:\n  - predicate: works_at\n    kind: functional\n    description: Employment.\n")
	if err := os.MkdirAll(filepath.Join(dir, "kinds"), 0o755); err != nil {
		t.Fatalf("mkdir kinds: %v", err)
	}
	writeFile(t, filepath.Join(dir, "kinds", "company.yaml"),
		"name: \"extension:crm:company\"\ndisplayName: Company\ndescription: A company.\n")
	if withInstructions {
		writeFile(t, filepath.Join(dir, "instructions.md"), "# Steering\nDo the thing.\n")
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBundleChecksum_DeterministicAndPrefixed(t *testing.T) {
	t.Parallel()
	dir := writeChecksumBundle(t, true)

	first, err := schemapack.BundleChecksum(os.DirFS(dir), ".")
	if err != nil {
		t.Fatalf("BundleChecksum: %v", err)
	}
	second, err := schemapack.BundleChecksum(os.DirFS(dir), ".")
	if err != nil {
		t.Fatalf("BundleChecksum (second): %v", err)
	}
	if first != second {
		t.Fatalf("checksum not deterministic: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("checksum %q missing sha256: prefix", first)
	}
}

func TestBundleChecksum_ChangesWithContent(t *testing.T) {
	t.Parallel()
	dir := writeChecksumBundle(t, true)
	before, err := schemapack.BundleChecksum(os.DirFS(dir), ".")
	if err != nil {
		t.Fatalf("BundleChecksum: %v", err)
	}
	writeFile(t, filepath.Join(dir, "instructions.md"), "# Steering\nDo a DIFFERENT thing.\n")
	after, err := schemapack.BundleChecksum(os.DirFS(dir), ".")
	if err != nil {
		t.Fatalf("BundleChecksum (after): %v", err)
	}
	if before == after {
		t.Fatalf("checksum did not change after editing instructions.md: %q", after)
	}
}

func TestBundleChecksum_IgnoresNonCanonicalFiles(t *testing.T) {
	t.Parallel()
	dir := writeChecksumBundle(t, true)
	before, err := schemapack.BundleChecksum(os.DirFS(dir), ".")
	if err != nil {
		t.Fatalf("BundleChecksum: %v", err)
	}
	// A README / LICENSE in the bundle dir must not affect the checksum: it
	// hashes only the canonical bundle file set.
	writeFile(t, filepath.Join(dir, "README.md"), "# crm preset\nincidental docs\n")
	writeFile(t, filepath.Join(dir, "LICENSE"), "Apache-2.0\n")
	after, err := schemapack.BundleChecksum(os.DirFS(dir), ".")
	if err != nil {
		t.Fatalf("BundleChecksum (after): %v", err)
	}
	if before != after {
		t.Fatalf("incidental files changed the checksum: %q vs %q", before, after)
	}
}

func TestBundleChecksum_MissingManifestRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "instructions.md"), "# orphan\n")
	if _, err := schemapack.BundleChecksum(os.DirFS(dir), "."); !errors.Is(err, schemapack.ErrInvalidManifest) {
		t.Fatalf("want ErrInvalidManifest, got %v", err)
	}
}

func TestBundleChecksum_RejectsOversizeBundle(t *testing.T) {
	t.Parallel()
	dir := writeChecksumBundle(t, true)
	// A kinds/*.yaml just over the total cap must be refused before its content
	// is read into the hash — bounding a hostile remote bundle's memory use.
	big := strings.Repeat("a", schemapack.MaxBundleBytes+1)
	writeFile(t, filepath.Join(dir, "kinds", "huge.yaml"), big)
	if _, err := schemapack.BundleChecksum(os.DirFS(dir), "."); !errors.Is(err, schemapack.ErrBundleTooLarge) {
		t.Fatalf("want ErrBundleTooLarge, got %v", err)
	}
}

func TestBundleChecksum_RejectsSymlinkedCanonicalFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	dir := writeChecksumBundle(t, false)
	// Replace instructions.md with a symlink to an arbitrary file: it must be
	// rejected before its target is read.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, secret, "PRIVATE\n")
	if err := os.Symlink(secret, filepath.Join(dir, "instructions.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := schemapack.BundleChecksum(os.DirFS(dir), "."); !errors.Is(err, schemapack.ErrBundleSymlink) {
		t.Fatalf("want ErrBundleSymlink, got %v", err)
	}
}

func TestBundleChecksum_RejectsSymlinkedKindFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	dir := writeChecksumBundle(t, true)
	// A symlinked kinds/*.yaml is also a canonical bundle file and must be
	// rejected before its target is read into the checksum.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, secret, "PRIVATE\n")
	kind := filepath.Join(dir, "kinds", "company.yaml")
	if err := os.Remove(kind); err != nil {
		t.Fatalf("remove kind: %v", err)
	}
	if err := os.Symlink(secret, kind); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := schemapack.BundleChecksum(os.DirFS(dir), "."); !errors.Is(err, schemapack.ErrBundleSymlink) {
		t.Fatalf("want ErrBundleSymlink, got %v", err)
	}
}
