package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestNewLoggerStderrUnchanged verifies the default (no FilePath) path still
// writes JSON entries containing the canonical "ts"/"level"/"msg" keys to the
// supplied io.Writer.
func TestNewLoggerStderrUnchanged(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(Config{Output: &buf, Level: LevelInfo})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	logger.Info("hello", zap.String("event_id", "evt_1"))
	if err := logger.Sync(); err != nil {
		// stderr-style sinks occasionally fail Sync on bytes.Buffer; not
		// fatal for this assertion.
		_ = err
	}
	out := buf.String()
	for _, want := range []string{`"ts":`, `"level":"info"`, `"msg":"hello"`, `"event_id":"evt_1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output: %s", want, out)
		}
	}
}

// TestNewLoggerFileSinkPermissions verifies the parent directory is created
// with mode 0700 and the log file with mode 0600.
func TestNewLoggerFileSinkPermissions(t *testing.T) {
	root := t.TempDir()
	logFile := filepath.Join(root, "nested", "daemon.log")

	logger, err := NewLogger(Config{FilePath: logFile, Level: LevelInfo})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	logger.Info("first entry")
	_ = logger.Sync()

	dirInfo, err := os.Stat(filepath.Dir(logFile))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %o, want 0700", mode)
	}

	fileInfo, err := os.Stat(logFile)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if mode := fileInfo.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
}

// TestNewLoggerFileSinkTightensExistingFile verifies that NewLogger tightens
// a pre-existing log file's mode to 0600 (so a stale 0644 daemon.log left by
// a prior run does not stay world-readable) but DOES NOT chmod a pre-existing
// parent directory. Greptile P1 (#78 round 2): MAZ_LOG_FILE can point at
// /tmp/maz.log where the parent is a shared directory; tightening shared
// parents would break unrelated processes.
func TestNewLoggerFileSinkTightensExistingFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "preexisting")
	logFile := filepath.Join(dir, "daemon.log")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := os.WriteFile(logFile, []byte("preexisting\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	logger, err := NewLogger(Config{FilePath: logFile, Level: LevelInfo})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	logger.Info("after tighten")
	_ = logger.Sync()

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	// Pre-existing parent dir must be left alone (could be a shared dir).
	if mode := dirInfo.Mode().Perm(); mode != 0o755 {
		t.Errorf("pre-existing dir mode = %o, want 0755 (unchanged)", mode)
	}
	fileInfo, err := os.Stat(logFile)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	// Pre-existing file should be tightened: we are the explicit writer.
	if mode := fileInfo.Mode().Perm(); mode != 0o600 {
		t.Errorf("pre-existing file mode = %o, want 0600", mode)
	}
}

// TestNewLoggerFileSinkTightensOwnedDefaultDir is the regression test for
// the Greptile P1 (#78 round 3) finding: when FilePath sits under the
// Mazarbul-owned default cache dir ($XDG_CACHE_HOME/mazarbul or
// ~/.cache/mazarbul), a pre-existing 0755 owned dir must still be tightened
// to 0700. Only custom MAZ_LOG_FILE parents are left alone.
func TestNewLoggerFileSinkTightensOwnedDefaultDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("MAZ_LOG_FILE", "")
	ownedDir := filepath.Join(root, "mazarbul")
	if err := os.MkdirAll(ownedDir, 0o755); err != nil {
		t.Fatalf("seed owned dir: %v", err)
	}
	// Re-chmod after MkdirAll in case umask shaved bits.
	if err := os.Chmod(ownedDir, 0o755); err != nil {
		t.Fatalf("chmod seed: %v", err)
	}
	logFile := filepath.Join(ownedDir, "daemon.log")

	logger, err := NewLogger(Config{FilePath: logFile, Level: LevelInfo})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	logger.Info("entry")
	_ = logger.Sync()

	dirInfo, err := os.Stat(ownedDir)
	if err != nil {
		t.Fatalf("stat owned dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("owned dir mode = %o, want 0700 (NewLogger must tighten Mazarbul-owned parent)", mode)
	}
}

// TestNewLoggerFileSinkSkipsChmodOnSymlinkedOwnedDir is the regression test
// for the Greptile P1 (#78 round 4) finding: even when the owned-dir gate
// matches, os.Chmod follows symlinks and would mutate a shared target. If
// $XDG_CACHE_HOME/mazarbul is a symlink, NewLogger must Lstat-detect that
// and skip the chmod (still allowing the log to be written).
func TestNewLoggerFileSinkSkipsChmodOnSymlinkedOwnedDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("MAZ_LOG_FILE", "")

	// Create the real shared target with mode 0755 and a symlink at the
	// owned path. NewLogger should not mutate the target.
	target := filepath.Join(root, "shared-target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatalf("chmod target: %v", err)
	}
	ownedDir := filepath.Join(root, "mazarbul")
	if err := os.Symlink(target, ownedDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	logFile := filepath.Join(ownedDir, "daemon.log")

	logger, err := NewLogger(Config{FilePath: logFile, Level: LevelInfo})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	logger.Info("entry")
	_ = logger.Sync()

	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if mode := targetInfo.Mode().Perm(); mode != 0o755 {
		t.Errorf("symlink target mode = %o, want 0755 (NewLogger must not chmod through a symlink)", mode)
	}
	linkInfo, err := os.Lstat(ownedDir)
	if err != nil {
		t.Fatalf("lstat owned link: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("owned dir is no longer a symlink (NewLogger should not have replaced it)")
	}
}

// TestNewLoggerFileSinkLeavesSharedParentAlone is the regression test for
// the Greptile P1 (#78 round 2) finding: NewLogger must not chmod a
// pre-existing parent that the user didn't ask Mazarbul to own. Models the
// MAZ_LOG_FILE=/tmp/maz.log case via a 0777-sticky-bit-style mode.
func TestNewLoggerFileSinkLeavesSharedParentAlone(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.MkdirAll(shared, 0o777); err != nil {
		t.Fatalf("seed shared: %v", err)
	}
	// Reapply mode after MkdirAll in case umask masked bits.
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatalf("chmod shared: %v", err)
	}
	logFile := filepath.Join(shared, "daemon.log")

	logger, err := NewLogger(Config{FilePath: logFile, Level: LevelInfo})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	logger.Info("entry")
	_ = logger.Sync()

	dirInfo, err := os.Stat(shared)
	if err != nil {
		t.Fatalf("stat shared: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o777 {
		t.Errorf("shared parent dir mode = %o, want 0777 (NewLogger must not chmod a parent it did not create)", mode)
	}
}

// TestNewLoggerRotatesAtThreshold verifies lumberjack rotates the file when
// it crosses MaxSizeMB and that at least one backup is produced.
func TestNewLoggerRotatesAtThreshold(t *testing.T) {
	root := t.TempDir()
	logFile := filepath.Join(root, "daemon.log")

	logger, err := NewLogger(Config{
		FilePath:   logFile,
		MaxSizeMB:  1, // smallest threshold lumberjack accepts (MB)
		MaxBackups: 2,
		Level:      LevelInfo,
	})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	// Each entry is ~100 bytes after encoding. 12000 entries = ~1.2 MB,
	// comfortably past the 1 MB threshold.
	payload := strings.Repeat("x", 60)
	for range 12000 {
		logger.Info("rotation probe", zap.String("payload", payload))
	}
	_ = logger.Sync()

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var liveFound, backupFound bool
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "daemon.log":
			liveFound = true
		case strings.HasPrefix(name, "daemon-") && strings.HasSuffix(name, ".log"):
			backupFound = true
			info, err := e.Info()
			if err != nil {
				t.Fatalf("stat backup %s: %v", name, err)
			}
			if mode := info.Mode().Perm(); mode != 0o600 {
				t.Errorf("backup %s mode = %o, want 0600", name, mode)
			}
		}
	}
	if !liveFound {
		t.Error("live daemon.log missing after rotation")
	}
	if !backupFound {
		t.Errorf("no rotated backup found in %s; entries=%v", root, entries)
	}
}

// TestNewLoggerDefaultsFromZeroConfig verifies an empty Config returns a
// usable logger writing to stderr-equivalent without panicking.
func TestNewLoggerDefaultsFromZeroConfig(t *testing.T) {
	logger, err := NewLogger(Config{})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	if logger == nil {
		t.Fatal("logger is nil")
	}
	// Smoke: emit one entry; we cannot inspect stderr cleanly here, but a
	// panic in encoder wiring would surface.
	logger.Info("zero config smoke")
	_ = logger.Sync()
}
