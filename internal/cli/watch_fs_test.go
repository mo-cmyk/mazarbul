package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/daemon"
	"github.com/mo-cmyk/mazarbul/internal/watchfs"
)

// newTestWatchFSCmd builds a bare command with the watch-fs flags bound so
// tests can drive resolveWatchFSOptions without the full root command.
func newTestWatchFSCmd() (*cobra.Command, *watchFSFlags) {
	flags := &watchFSFlags{}
	cmd := &cobra.Command{Use: "watch-fs"}
	bindWatchFSFlags(cmd, flags)
	return cmd, flags
}

func TestResolveWatchFSOptions_NoRootsErrors(t *testing.T) {
	cmd, flags := newTestWatchFSCmd()
	_, _, _, err := resolveWatchFSOptions(cmd, nil, flags, t.TempDir(), zap.NewNop())
	if err == nil {
		t.Fatal("expected error when no roots are given")
	}
}

func TestResolveWatchFSOptions_MissingRootErrors(t *testing.T) {
	cmd, flags := newTestWatchFSCmd()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, _, _, err := resolveWatchFSOptions(cmd, []string{missing}, flags, t.TempDir(), zap.NewNop())
	if err == nil {
		t.Fatal("expected error for a non-existent root directory")
	}
}

func TestResolveWatchFSOptions_FileRootErrors(t *testing.T) {
	cmd, flags := newTestWatchFSCmd()
	file := filepath.Join(t.TempDir(), "f.txt")
	if werr := os.WriteFile(file, []byte("x"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	_, _, _, err := resolveWatchFSOptions(cmd, []string{file}, flags, t.TempDir(), zap.NewNop())
	if err == nil {
		t.Fatal("expected error when a root is a file, not a directory")
	}
}

func TestResolveInstallRoots_MissingRootErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := resolveInstallRoots([]string{missing}, t.TempDir()); err == nil {
		t.Fatal("expected error: install must reject a non-existent root before writing the plist")
	}
}

func TestResolveInstallRoots_ExistingRootOK(t *testing.T) {
	root := t.TempDir()
	roots, err := resolveInstallRoots([]string{root}, t.TempDir())
	if err != nil {
		t.Fatalf("resolveInstallRoots: %v", err)
	}
	if len(roots) != 1 || roots[0] != root {
		t.Errorf("roots = %v, want [%s]", roots, root)
	}
}

func TestResolveWatchFSOptions_InvalidScopeErrors(t *testing.T) {
	cmd, flags := newTestWatchFSCmd()
	if serr := cmd.Flags().Set("scope", "bogus"); serr != nil {
		t.Fatal(serr)
	}
	_, _, _, err := resolveWatchFSOptions(cmd, []string{t.TempDir()}, flags, t.TempDir(), zap.NewNop())
	if err == nil {
		t.Fatal("expected error for an invalid scope")
	}
}

func TestResolveWatchFSOptions_Defaults(t *testing.T) {
	cmd, flags := newTestWatchFSCmd()
	root := t.TempDir() // resolveWatchFSOptions now requires roots to exist
	opts, scope, confidence, err := resolveWatchFSOptions(cmd, []string{root}, flags, t.TempDir(), zap.NewNop())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if scope != defaultWatchFSScope {
		t.Errorf("scope = %q, want %q", scope, defaultWatchFSScope)
	}
	if confidence != defaultWatchFSConfidence {
		t.Errorf("confidence = %v, want %v", confidence, defaultWatchFSConfidence)
	}
	if len(opts.Roots) != 1 || opts.Roots[0] != root {
		t.Errorf("roots = %v", opts.Roots)
	}
	// Unset size-cap and debounce stay zero so watchfs.New applies its
	// own defaults rather than the command forcing them.
	if opts.SizeCap != 0 || opts.Debounce != 0 {
		t.Errorf("unset tuning should be zero: sizeCap=%d debounce=%v", opts.SizeCap, opts.Debounce)
	}
}

func TestResolveWatchFSOptions_FlagsOverride(t *testing.T) {
	cmd, flags := newTestWatchFSCmd()
	if err := cmd.Flags().Set("scope", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("size-cap", "1024"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("debounce", "250"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("confidence", "0.9"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("ignore", "*.bak"); err != nil {
		t.Fatal(err)
	}

	opts, scope, confidence, err := resolveWatchFSOptions(cmd, []string{t.TempDir()}, flags, t.TempDir(), zap.NewNop())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if scope != "personal" {
		t.Errorf("scope = %q, want personal", scope)
	}
	if confidence != 0.9 {
		t.Errorf("confidence = %v, want 0.9", confidence)
	}
	if opts.SizeCap != 1024 {
		t.Errorf("sizeCap = %d, want 1024", opts.SizeCap)
	}
	if opts.Debounce != 250*time.Millisecond {
		t.Errorf("debounce = %v, want 250ms", opts.Debounce)
	}
	if len(opts.ExtraIgnores) != 1 || opts.ExtraIgnores[0] != "*.bak" {
		t.Errorf("extraIgnores = %v", opts.ExtraIgnores)
	}
}

func TestResolveWatchFSOptions_BuildsValidWatcher(t *testing.T) {
	cmd, flags := newTestWatchFSCmd()
	opts, _, _, err := resolveWatchFSOptions(cmd, []string{t.TempDir()}, flags, t.TempDir(), zap.NewNop())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := watchfs.New(opts); err != nil {
		t.Fatalf("watchfs.New rejected resolved options: %v", err)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"first wins", []string{"a", "b"}, "a"},
		{"skips empties", []string{"", "", "c"}, "c"},
		{"all empty", []string{"", ""}, ""},
		{"none", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstNonEmpty(tc.in...); got != tc.want {
				t.Errorf("firstNonEmpty(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWatchFSStatusFromPID(t *testing.T) {
	dir := t.TempDir()

	t.Run("absent pidfile is stopped", func(t *testing.T) {
		st := watchFSStatusFromPID(dir)
		if st.Running {
			t.Errorf("expected stopped, got %+v", st)
		}
	})

	t.Run("live pidfile is running", func(t *testing.T) {
		path := daemon.WatchFSPIDPath(dir)
		if err := daemon.WritePID(path, daemon.WatchFSPIDToken, dir); err != nil {
			t.Fatalf("WritePID: %v", err)
		}
		st := watchFSStatusFromPID(dir)
		if !st.Running {
			t.Errorf("expected running after WritePID, got %+v", st)
		}
	})
}
