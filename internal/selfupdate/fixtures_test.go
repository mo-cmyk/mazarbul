package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fixtureMazScript stands in for the maz binary: a tiny shell script that emits
// the version JSON the smoke test parses. It lets a test exercise the real
// extract + smoke path without a compiled maz.
const fixtureMazScript = "#!/bin/sh\nprintf '{\"version\":\"v9.9.9\"}\\n'\n"

// writeFixtureArchive builds a release archive and its checksums.txt for tag at
// the given GOOS/GOARCH into dir. It mirrors a real release: one wrapper dir
// holding a top-level maz plus lib/ with the liblbug symlink chain, and a man
// page the upgrade installs. It returns the archive path, checksums path, and
// archive filename.
func writeFixtureArchive(t *testing.T, dir, tag, osName, arch string) (archivePath, sumsPath, name string) {
	t.Helper()
	name = archiveName(tag, osName, arch)
	wrapper := fmt.Sprintf("mazarbul_%s_%s_%s", tag, osName, arch)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	writeTarDir(t, tw, wrapper+"/")
	writeTarReg(t, tw, wrapper+"/maz", []byte(fixtureMazScript), 0o755)
	writeTarDir(t, tw, wrapper+"/lib/")
	writeTarReg(t, tw, wrapper+"/lib/liblbug.0.1.dylib", []byte("ENGINE-BYTES"), 0o644)
	writeTarSymlink(t, tw, wrapper+"/lib/liblbug.0.dylib", "liblbug.0.1.dylib")
	writeTarSymlink(t, tw, wrapper+"/lib/liblbug.dylib", "liblbug.0.dylib")
	writeTarReg(t, tw, wrapper+"/man/man1/maz.1", []byte(".TH MAZ 1"), 0o644)
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	archivePath = filepath.Join(dir, name)
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	sumsPath = filepath.Join(dir, "checksums.txt")
	line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
	if err := os.WriteFile(sumsPath, []byte(line), 0o600); err != nil {
		t.Fatalf("write checksums: %v", err)
	}
	return archivePath, sumsPath, name
}

func writeTarDir(t *testing.T, tw *tar.Writer, name string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatalf("tar dir %s: %v", name, err)
	}
}

func writeTarReg(t *testing.T, tw *tar.Writer, name string, data []byte, mode int64) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data))}); err != nil {
		t.Fatalf("tar header %s: %v", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("tar write %s: %v", name, err)
	}
}

func writeTarSymlink(t *testing.T, tw *tar.Writer, name, target string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}); err != nil {
		t.Fatalf("tar symlink %s: %v", name, err)
	}
}
