package doctor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
)

// seedWatermarkAt inserts one connector_watermarks row with a given advance
// time, opening (and migrating) the index.db under dir.
func seedWatermarkAt(t *testing.T, dir, connector, stream string, advance time.Time) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, sqliteIndexPath(dir))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			t.Fatalf("close sqlite: %v", cerr)
		}
	}()
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO connector_watermarks (connector, stream, cursor, sequence, advance_time, scope)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		connector, stream, `{"lastSHA":"abc"}`, 1, advance.UTC().Format(time.RFC3339Nano), "personal",
	); err != nil {
		t.Fatalf("insert watermark: %v", err)
	}
}

// TestSourceFreshness_FlagsStaleAndDown: a source that advanced recently is
// live (not flagged); one >1d is stale; one >7d is down. The check WARNs (never
// FAILs) and names the stale/down connectors but not the live one.
func TestSourceFreshness_FlagsStaleAndDown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now().UTC()
	seedWatermarkAt(t, dir, "local-git", "main", now.Add(-1*time.Hour))    // live
	seedWatermarkAt(t, dir, "github", "commits", now.Add(-2*24*time.Hour)) // stale
	seedWatermarkAt(t, dir, "slack", "general", now.Add(-10*24*time.Hour)) // down

	status, detail, err := newSourceFreshnessCheck(newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusWarn {
		t.Fatalf("status = %v (%s), want WARN", status, detail)
	}
	if !strings.Contains(detail, "github") {
		t.Errorf("detail = %q, want it to name the stale source github", detail)
	}
	if !strings.Contains(detail, "slack") {
		t.Errorf("detail = %q, want it to name the down source slack", detail)
	}
	if strings.Contains(detail, "local-git") {
		t.Errorf("detail = %q, must NOT flag the live source local-git", detail)
	}
	// Advisory only: a stale/down source must never flip the exit code.
	if code := ExitCode([]Result{{Status: status}}); code != 0 {
		t.Errorf("ExitCode = %d, want 0 (freshness is advisory)", code)
	}
}

// TestSourceFreshness_AllFresh_OK: every source advanced recently → OK.
func TestSourceFreshness_AllFresh_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now().UTC()
	seedWatermarkAt(t, dir, "local-git", "main", now.Add(-30*time.Minute))

	status, detail, err := newSourceFreshnessCheck(newSharedFor(dir)).Fn(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK {
		t.Errorf("status = %v (%s), want OK", status, detail)
	}
}

// TestSourceFreshness_NoSources_OK: a store with no connected sources is OK,
// not an error.
func TestSourceFreshness_NoSources_OK(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Open+migrate the store (so the table exists) without seeding any row.
	ctx := context.Background()
	store, err := sqlite.Open(ctx, sqliteIndexPath(dir))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if cerr := store.Close(); cerr != nil {
		t.Fatalf("close sqlite: %v", cerr)
	}

	status, detail, err := newSourceFreshnessCheck(newSharedFor(dir)).Fn(ctx)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != StatusOK || !strings.Contains(detail, "no sources") {
		t.Errorf("status=%v detail=%q, want OK + 'no sources'", status, detail)
	}
}
