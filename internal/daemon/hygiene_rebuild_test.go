package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestCleanStaleRebuildArtifacts removes orphaned shadow dirs older than the
// staleness window, keeps a recent one (possibly in-flight), and never removes
// the commit sentinel (it persists until a successful rebuild clears it).
func TestCleanStaleRebuildArtifacts(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	oldDir := filepath.Join(dataDir, rebuildShadowPrefix+"01OLD")
	recentDir := filepath.Join(dataDir, rebuildShadowPrefix+"01NEW")
	sentinel := filepath.Join(dataDir, rebuildCommitSentinel)

	for _, d := range []string{oldDir, recentDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(sentinel, []byte("/some/.rebuild-01OLD\n"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// Age the old dir past the staleness window.
	past := time.Now().Add(-2 * staleRebuildAge)
	if err := os.Chtimes(oldDir, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	cleanStaleRebuildArtifacts(dataDir, zap.NewNop())

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("stale shadow dir survived (err=%v); want removed", err)
	}
	if _, err := os.Stat(recentDir); err != nil {
		t.Errorf("recent shadow dir was removed (err=%v); want kept (may be in-flight)", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("commit sentinel was removed (err=%v); want kept until a successful rebuild", err)
	}
}

// TestCleanStaleRebuildArtifactsNoDir tolerates a data dir with no rebuild
// artifacts (the common boot case).
func TestCleanStaleRebuildArtifactsNoDir(t *testing.T) {
	t.Parallel()
	// Must not panic or error on a clean data dir.
	cleanStaleRebuildArtifacts(t.TempDir(), zap.NewNop())
}
