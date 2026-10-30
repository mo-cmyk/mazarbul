package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeArchiveFile builds a .tar.gz at dir/<name> from the given build callback
// and returns its path, for the malicious-entry cases that need raw control.
func writeArchiveFile(t *testing.T, dir, name string, build func(tw *tar.Writer)) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	build(tw)
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return path
}

func TestExtract(t *testing.T) {
	t.Parallel()

	t.Run("stages bin and lib and preserves symlink chain", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		archivePath, _, _ := writeFixtureArchive(t, dir, "v9.9.9", runtime.GOOS, runtime.GOARCH)
		stage := t.TempDir()
		if err := Extract(archivePath, stage); err != nil {
			t.Fatalf("Extract: %v", err)
		}

		bin := filepath.Join(stage, "bin", "maz")
		fi, err := os.Stat(bin)
		if err != nil {
			t.Fatalf("staged maz: %v", err)
		}
		if fi.Mode().Perm()&0o100 == 0 {
			t.Errorf("staged maz not executable: %v", fi.Mode())
		}

		link := filepath.Join(stage, "lib", "liblbug.dylib")
		li, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("staged symlink: %v", err)
		}
		if li.Mode()&os.ModeSymlink == 0 {
			t.Errorf("liblbug.dylib is not a symlink: %v", li.Mode())
		}
		if target, _ := os.Readlink(link); target != "liblbug.0.dylib" {
			t.Errorf("symlink target = %q, want liblbug.0.dylib", target)
		}

		// The man page is staged under man/ so the upgrade can install it.
		if _, err := os.Stat(filepath.Join(stage, "man", "man1", "maz.1")); err != nil {
			t.Errorf("man/man1/maz.1 was not staged: %v", err)
		}
	})

	t.Run("smoke passes on the staged tree", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		archivePath, _, _ := writeFixtureArchive(t, dir, "v9.9.9", runtime.GOOS, runtime.GOARCH)
		stage := t.TempDir()
		if err := Extract(archivePath, stage); err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if err := Smoke(context.Background(), stage); err != nil {
			t.Fatalf("Smoke: %v", err)
		}
	})

	t.Run("rejects a path-traversal entry", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeArchiveFile(t, dir, "evil.tar.gz", func(tw *tar.Writer) {
			writeTarReg(t, tw, "wrapper/lib/../../evil", []byte("x"), 0o644)
		})
		if err := Extract(path, t.TempDir()); err == nil {
			t.Fatal("expected traversal entry to be rejected")
		}
	})

	t.Run("rejects a symlink escaping the staging tree", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeArchiveFile(t, dir, "evil.tar.gz", func(tw *tar.Writer) {
			writeTarReg(t, tw, "wrapper/maz", []byte(fixtureMazScript), 0o755)
			writeTarSymlink(t, tw, "wrapper/lib/liblbug.dylib", "../../../../etc/passwd")
		})
		if err := Extract(path, t.TempDir()); err == nil {
			t.Fatal("expected escaping symlink to be rejected")
		}
	})

	t.Run("rejects an archive without maz", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeArchiveFile(t, dir, "nomaz.tar.gz", func(tw *tar.Writer) {
			writeTarReg(t, tw, "wrapper/lib/liblbug.0.1.dylib", []byte("x"), 0o644)
		})
		if err := Extract(path, t.TempDir()); err == nil {
			t.Fatal("expected missing maz to be rejected")
		}
	})

	t.Run("rejects an archive without a liblbug engine library", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeArchiveFile(t, dir, "nolib.tar.gz", func(tw *tar.Writer) {
			writeTarReg(t, tw, "wrapper/maz", []byte(fixtureMazScript), 0o755)
		})
		if err := Extract(path, t.TempDir()); err == nil {
			t.Fatal("expected a missing liblbug engine library to be rejected")
		}
	})
}
