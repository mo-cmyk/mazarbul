package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectLocation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		exe  string
		want InstallMethod
	}{
		{"homebrew opt prefix", "/opt/homebrew/bin/maz", MethodHomebrew},
		{"homebrew cellar", "/usr/local/Cellar/mazarbul/0.3.0/bin/maz", MethodHomebrew},
		{"linuxbrew prefix", "/home/linuxbrew/.linuxbrew/bin/maz", MethodHomebrew},
		{"prefix install", "/home/user/.local/bin/maz", MethodPrefix},
		{"non bin layout", "/weird/place/maz", MethodUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DetectLocation(tc.exe).Method; got != tc.want {
				t.Errorf("DetectLocation(%q).Method = %d, want %d", tc.exe, got, tc.want)
			}
		})
	}
}

func TestEnsureWritable(t *testing.T) {
	t.Parallel()

	t.Run("passes on a writable prefix", func(t *testing.T) {
		t.Parallel()
		loc := newLiveFixture(t)
		if err := loc.EnsureWritable(); err != nil {
			t.Errorf("EnsureWritable: %v", err)
		}
	})

	t.Run("fails on a read-only directory", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permission bits")
		}
		loc := newLiveFixture(t)
		if err := os.Chmod(loc.BinDir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(loc.BinDir, 0o750) })
		if err := loc.EnsureWritable(); err == nil {
			t.Error("expected EnsureWritable to fail on a read-only bin dir")
		}
	})
}

func TestSwap(t *testing.T) {
	t.Parallel()

	t.Run("replaces binary and lib and prunes stale", func(t *testing.T) {
		t.Parallel()
		loc := newLiveFixture(t)
		// A stale engine lib the new release no longer ships.
		writeFile(t, filepath.Join(loc.LibDir, "liblbug.0.0.dylib"), "STALE")
		stage := newStageFixture(t)

		if err := Swap(stage, loc); err != nil {
			t.Fatalf("Swap: %v", err)
		}
		if got := readFile(t, filepath.Join(loc.BinDir, "maz")); got != "NEW" {
			t.Errorf("binary = %q, want NEW", got)
		}
		if got := readFile(t, filepath.Join(loc.LibDir, "liblbug.0.1.dylib")); got != "NEWLIB" {
			t.Errorf("lib = %q, want NEWLIB", got)
		}
		if _, err := os.Lstat(filepath.Join(loc.LibDir, "liblbug.0.0.dylib")); !os.IsNotExist(err) {
			t.Errorf("stale lib not pruned (err=%v)", err)
		}
		assertNoTempArtifacts(t, loc)
	})

	t.Run("rolls back on commit failure", func(t *testing.T) {
		t.Parallel()
		loc := newLiveFixture(t)
		stage := newStageFixture(t)

		ops, err := planSwap(stage, loc)
		if err != nil {
			t.Fatalf("planSwap: %v", err)
		}
		prune, err := planPrune(stage, loc)
		if err != nil {
			t.Fatalf("planPrune: %v", err)
		}
		staged, err := stageAll(ops)
		if err != nil {
			t.Fatalf("stageAll: %v", err)
		}
		// Force the final commit to fail by removing its staged file.
		if err := os.Remove(staged[len(staged)-1]); err != nil {
			t.Fatalf("remove staged: %v", err)
		}
		committed, err := commitAll(ops, prune)
		if err == nil {
			t.Fatal("expected commitAll to fail")
		}
		rollback(committed)
		removeAll(staged)

		if got := readFile(t, filepath.Join(loc.BinDir, "maz")); got != "OLD" {
			t.Errorf("binary after rollback = %q, want OLD", got)
		}
		if got := readFile(t, filepath.Join(loc.LibDir, "liblbug.0.1.dylib")); got != "OLDLIB" {
			t.Errorf("lib after rollback = %q, want OLDLIB", got)
		}
		assertNoTempArtifacts(t, loc)
	})

	t.Run("restores the live binary when its own commit fails", func(t *testing.T) {
		t.Parallel()
		loc := newLiveFixture(t)
		stage := newStageFixture(t)

		ops, err := planSwap(stage, loc)
		if err != nil {
			t.Fatalf("planSwap: %v", err)
		}
		staged, err := stageAll(ops)
		if err != nil {
			t.Fatalf("stageAll: %v", err)
		}
		// Drop the binary's staged file (ops[0]) so its own .new -> target rename
		// fails after the live binary has already been moved aside to .bak. This is
		// the highest-stakes path: the live binary is mid-replacement.
		if err := os.Remove(staged[0]); err != nil {
			t.Fatalf("remove staged binary: %v", err)
		}
		committed, err := commitAll(ops, nil)
		if err == nil {
			t.Fatal("expected commitAll to fail on the binary")
		}
		if errs := rollback(committed); len(errs) != 0 {
			t.Errorf("rollback reported restore errors: %v", errs)
		}
		removeAll(staged)

		if got := readFile(t, filepath.Join(loc.BinDir, "maz")); got != "OLD" {
			t.Errorf("binary after rollback = %q, want OLD", got)
		}
		assertNoTempArtifacts(t, loc)
	})
}

// newLiveFixture builds a fake install prefix (bin/maz=OLD, lib with the OLDLIB
// engine file and the liblbug symlink chain) and returns its Location.
func newLiveFixture(t *testing.T) Location {
	t.Helper()
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin")
	lib := filepath.Join(prefix, "lib")
	mkdir(t, bin)
	mkdir(t, lib)
	writeFile(t, filepath.Join(bin, "maz"), "OLD")
	writeFile(t, filepath.Join(lib, "liblbug.0.1.dylib"), "OLDLIB")
	symlink(t, "liblbug.0.1.dylib", filepath.Join(lib, "liblbug.0.dylib"))
	symlink(t, "liblbug.0.dylib", filepath.Join(lib, "liblbug.dylib"))
	return Location{Prefix: prefix, BinDir: bin, LibDir: lib, Method: MethodPrefix}
}

// newStageFixture builds a staging tree (bin/maz=NEW, lib with NEWLIB and the
// symlink chain) and returns the staging dir.
func newStageFixture(t *testing.T) string {
	t.Helper()
	stage := t.TempDir()
	bin := filepath.Join(stage, "bin")
	lib := filepath.Join(stage, "lib")
	mkdir(t, bin)
	mkdir(t, lib)
	writeFile(t, filepath.Join(bin, "maz"), "NEW")
	writeFile(t, filepath.Join(lib, "liblbug.0.1.dylib"), "NEWLIB")
	symlink(t, "liblbug.0.1.dylib", filepath.Join(lib, "liblbug.0.dylib"))
	symlink(t, "liblbug.0.dylib", filepath.Join(lib, "liblbug.dylib"))
	return stage
}

// assertNoTempArtifacts fails if any .new or .bak siblings linger in the live
// bin or lib dirs after a swap or rollback.
func assertNoTempArtifacts(t *testing.T, loc Location) {
	t.Helper()
	for _, dir := range []string{loc.BinDir, loc.LibDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".new") || strings.HasSuffix(e.Name(), ".bak") {
				t.Errorf("leftover temp artifact %s in %s", e.Name(), dir)
			}
		}
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- test path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func symlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink %s: %v", path, err)
	}
}
