package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// seedChunk writes a chunk file under repoDir/chunks and returns its id+bytes.
func seedChunk(t *testing.T, repoDir, id string, body []byte) {
	t.Helper()
	chunksDir := filepath.Join(repoDir, "chunks")
	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		t.Fatalf("mkdir chunks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chunksDir, id+".jsonl"), body, 0o644); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
}

// seedManifest writes a MANIFEST.json at the given schema version recording the
// supplied chunkID->hash pairs.
func seedManifest(t *testing.T, repoDir string, version int, hashes map[string]string) {
	t.Helper()
	var sb string
	sb = fmt.Sprintf(`{"schemaVersion":%d,"chunks":[`, version)
	first := true
	var sbSb32 strings.Builder
	for id, h := range hashes {
		if !first {
			sbSb32.WriteString(",")
		}
		first = false
		sbSb32.WriteString(fmt.Sprintf(`{"chunkId":%q,"contentHash":%q}`, id, h))
	}
	sb += sbSb32.String()
	sb += "]}"
	if err := os.WriteFile(filepath.Join(repoDir, "MANIFEST.json"), []byte(sb), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestScanChunkIntegrity(t *testing.T) {
	t.Parallel()

	t.Run("verified clean", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		body := []byte("chunk-bytes-here")
		seedChunk(t, repo, "01CLEAN", body)
		seedManifest(t, repo, 2, map[string]string{"01CLEAN": chunk.ContentHash(body)})
		tampered, verified, unverified, err := scanChunkIntegrity(repo)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(tampered) != 0 || verified != 1 || unverified != 0 {
			t.Errorf("got tampered=%v verified=%d unverified=%d; want [] 1 0", tampered, verified, unverified)
		}
	})

	t.Run("detects tamper", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		seedChunk(t, repo, "01BAD", []byte("edited-by-hand"))
		// MANIFEST records the hash of the ORIGINAL bytes, not what's on disk.
		seedManifest(t, repo, 2, map[string]string{"01BAD": chunk.ContentHash([]byte("original"))})
		tampered, verified, _, err := scanChunkIntegrity(repo)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(tampered) != 1 || tampered[0] != "01BAD" || verified != 0 {
			t.Errorf("got tampered=%v verified=%d; want [01BAD] 0", tampered, verified)
		}
	})

	t.Run("v1 manifest unverified", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		seedChunk(t, repo, "01OLD", []byte("bytes"))
		seedManifest(t, repo, 1, nil) // pre-v2: no hashes recorded
		tampered, verified, unverified, err := scanChunkIntegrity(repo)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(tampered) != 0 || verified != 0 || unverified != 1 {
			t.Errorf("got tampered=%v verified=%d unverified=%d; want [] 0 1", tampered, verified, unverified)
		}
	})

	t.Run("missing dir is clean", func(t *testing.T) {
		t.Parallel()
		tampered, verified, unverified, err := scanChunkIntegrity(t.TempDir())
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(tampered) != 0 || verified != 0 || unverified != 0 {
			t.Errorf("empty mirror must scan clean, got tampered=%v v=%d u=%d", tampered, verified, unverified)
		}
	})
}

func TestAggregateChunkIntegrity_ScopeQualifiesTamperedIDs(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	repo := filepath.Join(dataDir, "sync", "personal", "repo")
	seedChunk(t, repo, "01BAD", []byte("edited"))
	seedManifest(t, repo, 2, map[string]string{"01BAD": chunk.ContentHash([]byte("original"))})

	tampered, _, _, err := aggregateChunkIntegrity(dataDir, map[string]config.ScopeRemoteTable{"personal": {}})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(tampered) != 1 || tampered[0] != "personal/01BAD" {
		t.Errorf("tampered = %v; want [personal/01BAD] (scope-qualified)", tampered)
	}
}

func TestChunkIntegrityVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                 string
		tampered             []string
		verified, unverified int
		want                 Status
	}{
		{"tamper fails", []string{"personal/01BAD"}, 3, 0, StatusFail},
		{"all verified ok", nil, 5, 0, StatusOK},
		{"only unverified ok", nil, 0, 2, StatusOK},
		{"empty ok", nil, 0, 0, StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _, err := chunkIntegrityVerdict(tc.tampered, tc.verified, tc.unverified)
			if err != nil {
				t.Fatalf("verdict err: %v", err)
			}
			if got != tc.want {
				t.Errorf("status = %v, want %v", got, tc.want)
			}
		})
	}
}
