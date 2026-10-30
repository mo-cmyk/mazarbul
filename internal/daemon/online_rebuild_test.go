package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// writeFile is a test helper that writes content to path, creating parents.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// readFile reads path or fails.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestSwapStorePaths replaces the live store artifacts (including SQLite
// sidecars) with the shadow's, and clears the commit sentinel on success.
func TestSwapStorePaths(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	shadowDir := filepath.Join(dataDir, rebuildShadowPrefix+"01TEST")

	// Live (old) projection, with a stale -shm sidecar the shadow lacks.
	writeFile(t, filepath.Join(dataDir, "index.db"), "old-sqlite")
	writeFile(t, filepath.Join(dataDir, "index.db-wal"), "old-wal")
	writeFile(t, filepath.Join(dataDir, "index.db-shm"), "old-shm")
	writeFile(t, filepath.Join(dataDir, "graph.ladybug"), "old-graph")

	// Shadow (new) projection.
	writeFile(t, filepath.Join(shadowDir, "index.db"), "new-sqlite")
	writeFile(t, filepath.Join(shadowDir, "index.db-wal"), "new-wal")
	writeFile(t, filepath.Join(shadowDir, "graph.ladybug"), "new-graph")

	if err := swapStorePaths(dataDir, shadowDir, zap.NewNop()); err != nil {
		t.Fatalf("swapStorePaths: %v", err)
	}

	if got := readFile(t, filepath.Join(dataDir, "index.db")); got != "new-sqlite" {
		t.Errorf("index.db = %q; want new-sqlite", got)
	}
	if got := readFile(t, filepath.Join(dataDir, "index.db-wal")); got != "new-wal" {
		t.Errorf("index.db-wal = %q; want new-wal", got)
	}
	if got := readFile(t, filepath.Join(dataDir, "graph.ladybug")); got != "new-graph" {
		t.Errorf("graph.ladybug = %q; want new-graph", got)
	}
	// The stale -shm sidecar (live-only, not in the shadow) must be removed so
	// SQLite does not reopen against a mismatched shared-memory file.
	if _, err := os.Stat(filepath.Join(dataDir, "index.db-shm")); !os.IsNotExist(err) {
		t.Errorf("stale index.db-shm survived the swap (err=%v); want removed", err)
	}
	// The shadow artifacts were moved OUT.
	if _, err := os.Stat(filepath.Join(shadowDir, "index.db")); !os.IsNotExist(err) {
		t.Errorf("shadow index.db not moved out (err=%v)", err)
	}
	// The commit sentinel is cleared after a successful swap.
	if _, err := os.Stat(filepath.Join(dataDir, rebuildCommitSentinel)); !os.IsNotExist(err) {
		t.Errorf("commit sentinel survived a successful swap (err=%v); want removed", err)
	}
}

// TestSwapStorePathsGraphDir verifies the swap handles graph.ladybug as a
// DIRECTORY (LadybugDB may lay the graph out as a directory tree).
func TestSwapStorePathsGraphDir(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	shadowDir := filepath.Join(dataDir, rebuildShadowPrefix+"01DIR")

	writeFile(t, filepath.Join(dataDir, "index.db"), "old")
	writeFile(t, filepath.Join(dataDir, "graph.ladybug", "data.kz"), "old-graph-data")

	writeFile(t, filepath.Join(shadowDir, "index.db"), "new")
	writeFile(t, filepath.Join(shadowDir, "graph.ladybug", "data.kz"), "new-graph-data")

	if err := swapStorePaths(dataDir, shadowDir, zap.NewNop()); err != nil {
		t.Fatalf("swapStorePaths: %v", err)
	}
	if got := readFile(t, filepath.Join(dataDir, "graph.ladybug", "data.kz")); got != "new-graph-data" {
		t.Errorf("graph.ladybug/data.kz = %q; want new-graph-data", got)
	}
}
