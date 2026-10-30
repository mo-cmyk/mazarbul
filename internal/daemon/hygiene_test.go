package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// TestApplyDataDirHygiene_CreatesSpotlightMarker pins the Spotlight
// opt-out marker that protects against the macOS Tahoe (26.x)
// corespotlightd indexing-loop freeze the daemon's long-lived SQLite
// WAL handles otherwise trigger. The marker file is a documented
// Apple opt-out (Spotlight skips any directory containing a
// .metadata_never_index file), so dropping it once at OpenWiring is
// the surgical fix.
func TestApplyDataDirHygiene_CreatesSpotlightMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ApplyDataDirHygiene(dir, zap.NewNop()); err != nil {
		t.Fatalf("ApplyDataDirHygiene: %v", err)
	}

	marker := filepath.Join(dir, ".metadata_never_index")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("missing Spotlight marker %s: %v", marker, err)
	}
}

// TestApplyDataDirHygiene_Idempotent confirms a second call is a
// no-op so calling it on every OpenWiring (CLI + daemon) is safe.
func TestApplyDataDirHygiene_Idempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := ApplyDataDirHygiene(dir, zap.NewNop()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	marker := filepath.Join(dir, ".metadata_never_index")
	first, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("first stat: %v", err)
	}

	if err := ApplyDataDirHygiene(dir, zap.NewNop()); err != nil {
		t.Fatalf("second call: %v", err)
	}
	second, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("second stat: %v", err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Fatalf("marker modtime changed on idempotent call: %v -> %v", first.ModTime(), second.ModTime())
	}
}

// TestApplyDataDirHygiene_MissingDirReturnsError surfaces a clear
// error when the caller hands in a path that doesn't exist, so the
// OpenWiring wiring failure mode is unambiguous.
func TestApplyDataDirHygiene_MissingDirReturnsError(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	err := ApplyDataDirHygiene(missing, zap.NewNop())
	if err == nil {
		t.Fatalf("expected error for missing dir, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}
