package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// TestReviewDupsEmptyJSONIsArray pins that `maz review dups --output json` over a
// migrated-but-empty projection encodes an empty JSON array, not `null`. A nil
// slice would marshal to `null` and break consumers that iterate the result.
func TestReviewDupsEmptyJSONIsArray(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// A migrated but empty projection: entity_dup_candidates exists, no rows.
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_ = store.Close()

	var outBuf, errBuf bytes.Buffer
	root := makeReviewRoot(dir, &outBuf, &errBuf)
	root.SetArgs([]string{"--data-dir", dir, "--output", "json", "review", "dups"})
	if err := root.Execute(); err != nil {
		t.Fatalf("review dups should exit 0, got: %v (stderr=%q)", err, errBuf.String())
	}

	got := strings.TrimSpace(outBuf.String())
	if got != "[]" {
		t.Errorf("empty review dups --output json = %q, want %q", got, "[]")
	}
}
