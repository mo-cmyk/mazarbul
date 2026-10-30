package git

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadManifestContentHashes_V2 confirms a v2 manifest's per-chunk hashes
// are surfaced keyed by chunkID, and that an entry without a hash is skipped.
func TestReadManifestContentHashes_V2(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{
  "schemaVersion": 2,
  "chunks": [
    {"chunkId": "01AAA", "scope": "personal", "count": 1, "contentHash": "deadbeef"},
    {"chunkId": "01BBB", "scope": "personal", "count": 1}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, manifestFilename), []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	got := readManifestContentHashes(dir)
	if got["01AAA"] != "deadbeef" {
		t.Errorf("hash for 01AAA = %q, want deadbeef", got["01AAA"])
	}
	if _, ok := got["01BBB"]; ok {
		t.Errorf("01BBB has no recorded hash; it must be absent (unverified), got %q", got["01BBB"])
	}
}

// TestReadManifestContentHashes_V1ParsesToEmpty pins the upgrade path: a legacy
// v1 manifest (no contentHash field) parses cleanly to an empty map, so the
// consumer treats its chunks as unverified rather than failing.
func TestReadManifestContentHashes_V1ParsesToEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{
  "schemaVersion": 1,
  "chunks": [
    {"chunkId": "01AAA", "scope": "personal", "count": 1, "firstUlid": "01AAA", "lastUlid": "01AAA"}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, manifestFilename), []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	got := readManifestContentHashes(dir)
	if len(got) != 0 {
		t.Errorf("v1 manifest must yield no hashes (unverified), got %v", got)
	}
}

// TestReadManifestContentHashes_MissingFileIsEmpty confirms an absent manifest
// is a non-nil empty map, never a nil deref or panic.
func TestReadManifestContentHashes_MissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	got := readManifestContentHashes(t.TempDir())
	if got == nil || len(got) != 0 {
		t.Errorf("absent manifest must yield an empty non-nil map, got %v", got)
	}
}
