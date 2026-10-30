package capacity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestReporter_ReadsRealStorageLayout reproduces the live-tmux bug
// where the Usage panel showed "0 B" for sqlite/ladybug/vector while
// the files clearly existed at multi-megabyte size on disk. Root
// cause was capacity.Reporter looking for `<dataDir>/sqlite`,
// `<dataDir>/graph`, `<dataDir>/vector` subdirectories — but the
// daemon writes the SQLite store as `<dataDir>/index.db` and the
// Ladybug store as `<dataDir>/graph.ladybug` (top-level files, not
// subdirectories). The reporter was scanning empty/nonexistent
// directories and returning zeros.
//
// This test pins the actual on-disk layout (verified against the
// running daemon's lsof output) so the reporter matches reality.
func TestReporter_ReadsRealStorageLayout(t *testing.T) {
	dir := t.TempDir()

	// Replicate the real daemon layout:
	//   events/      — event log JSONL + index.bolt
	//   index.db     — SQLite + FTS5 (top-level FILE)
	//   index.db-wal — WAL
	//   graph.ladybug      — Ladybug DB (top-level FILE)
	//   graph.ladybug.wal  — Ladybug WAL
	//   cache/       — embed cache (subdirectory)
	mustWrite(t, filepath.Join(dir, "events", "log.jsonl"), make([]byte, 100))
	mustWrite(t, filepath.Join(dir, "events", "index.bolt"), make([]byte, 200))
	mustWrite(t, filepath.Join(dir, "index.db"), make([]byte, 500))
	mustWrite(t, filepath.Join(dir, "index.db-wal"), make([]byte, 50))
	mustWrite(t, filepath.Join(dir, "index.db-shm"), make([]byte, 10))
	mustWrite(t, filepath.Join(dir, "graph.ladybug"), make([]byte, 700))
	mustWrite(t, filepath.Join(dir, "graph.ladybug.wal"), make([]byte, 70))
	mustWrite(t, filepath.Join(dir, "cache", "embed.bin"), make([]byte, 1000))

	r := NewReporter(dir)
	rep, err := r.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	if rep.OnDisk.EventLog == 0 {
		t.Errorf("EventLog=0 — events/ subdirectory not summed")
	}
	if rep.OnDisk.SQLiteFTS == 0 {
		t.Errorf("SQLiteFTS=0 — the reporter is missing index.db + WAL + SHM (live bug)")
	}
	if rep.OnDisk.Graph == 0 {
		t.Errorf("Graph=0 — the reporter is missing graph.ladybug + WAL (live bug)")
	}
	if rep.OnDisk.EmbedCache == 0 {
		t.Errorf("EmbedCache=0 — cache/ subdirectory not summed")
	}

	wantTotal := int64(100 + 200 + 500 + 50 + 10 + 700 + 70 + 1000)
	if rep.OnDisk.Total != wantTotal {
		t.Errorf("Total=%d want %d", rep.OnDisk.Total, wantTotal)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
