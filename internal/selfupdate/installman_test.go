package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallManPages(t *testing.T) {
	t.Parallel()

	t.Run("copies staged pages and creates the man dir", func(t *testing.T) {
		t.Parallel()
		stage := t.TempDir()
		srcDir := filepath.Join(stage, "man", "man1")
		if err := os.MkdirAll(srcDir, 0o750); err != nil {
			t.Fatalf("mkdir staged man: %v", err)
		}
		for _, name := range []string{"maz.1", "maz-recall.1"} {
			if err := os.WriteFile(filepath.Join(srcDir, name), []byte(".TH "+name), 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		// A non-.1 sibling must be ignored.
		if err := os.WriteFile(filepath.Join(srcDir, "README"), []byte("x"), 0o600); err != nil {
			t.Fatalf("write README: %v", err)
		}

		// manDir does not exist yet: InstallManPages must create it.
		manDir := filepath.Join(t.TempDir(), "share", "man", "man1")
		if err := InstallManPages(stage, manDir); err != nil {
			t.Fatalf("InstallManPages: %v", err)
		}
		for _, name := range []string{"maz.1", "maz-recall.1"} {
			fi, err := os.Stat(filepath.Join(manDir, name))
			if err != nil {
				t.Errorf("man page %s not installed: %v", name, err)
				continue
			}
			// copyManPage preserves the staged source mode (0o600 here), so a
			// 0o000-corrupted archive page wouldn't silently pass.
			if got := fi.Mode().Perm(); got != 0o600 {
				t.Errorf("man page %s mode = %04o, want 0600 (preserves staged source)", name, got)
			}
		}
		if _, err := os.Stat(filepath.Join(manDir, "README")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("non-.1 file was copied (err=%v)", err)
		}
	})

	t.Run("returns nil when no man pages are staged", func(t *testing.T) {
		t.Parallel()
		stage := t.TempDir() // no man/ subtree
		manDir := filepath.Join(t.TempDir(), "share", "man", "man1")
		if err := InstallManPages(stage, manDir); err != nil {
			t.Errorf("InstallManPages with no staged man: %v", err)
		}
		if _, err := os.Stat(manDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("man dir created despite no staged pages (err=%v)", err)
		}
	})
}
