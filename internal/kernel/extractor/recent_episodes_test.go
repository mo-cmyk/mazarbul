package extractor_test

// recent_episodes_test.go pins the SQLite-backed RecentEpisodesProvider used
// by the daemon. The test runs against the real migration set (the same one
// the dispatcher uses in production) so the column layout this provider
// depends on cannot drift silently.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/extractor"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/testutil"
)

func TestSQLiteRecentEpisodes_ReturnsAscendingFromOldestExclusion(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openStore(t, ctx)

	insertEpisode(t, ctx, store, "01HEP000000000000000000001", "first episode about Frodo Baggins at Erebor",
		time.Unix(1_700_000_000, 0).UTC())
	insertEpisode(t, ctx, store, "01HEP000000000000000000002", "second mentions Eowyn at Rivendell",
		time.Unix(1_700_000_010, 0).UTC())
	insertEpisode(t, ctx, store, "01HEP000000000000000000003", "current text with pronoun she",
		time.Unix(1_700_000_020, 0).UTC())

	p := extractor.NewSQLiteRecentEpisodes(store.Reader())
	got, err := p.RecentEpisodes(ctx, "01HEP000000000000000000003", 5)
	if err != nil {
		t.Fatalf("RecentEpisodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d episodes, want 2 (excluding the anchor): %v", len(got), got)
	}
	if got[0] != "second mentions Eowyn at Rivendell" {
		t.Errorf("ordering broken; got[0] = %q, want most-recent-first", got[0])
	}
}

func TestSQLiteRecentEpisodes_NoAnchorReturnsAllRecent(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openStore(t, ctx)

	insertEpisode(t, ctx, store, "01HEP000000000000000000010", "alpha",
		time.Unix(1_700_000_000, 0).UTC())
	insertEpisode(t, ctx, store, "01HEP000000000000000000011", "beta",
		time.Unix(1_700_000_005, 0).UTC())

	p := extractor.NewSQLiteRecentEpisodes(store.Reader())
	got, err := p.RecentEpisodes(ctx, "", 5)
	if err != nil {
		t.Fatalf("RecentEpisodes: %v", err)
	}
	if len(got) != 2 || got[0] != "beta" || got[1] != "alpha" {
		t.Errorf("got %v, want [beta alpha]", got)
	}
}

func TestSQLiteRecentEpisodes_ZeroK(t *testing.T) {
	t.Parallel()
	ctx := testutil.TestContext(t)
	store := openStore(t, ctx)
	insertEpisode(t, ctx, store, "01HEP000000000000000000020", "one",
		time.Unix(1_700_000_000, 0).UTC())

	p := extractor.NewSQLiteRecentEpisodes(store.Reader())
	got, err := p.RecentEpisodes(ctx, "", 0)
	if err != nil {
		t.Fatalf("RecentEpisodes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("k=0 returned %d rows, want 0", len(got))
	}
}

func openStore(t *testing.T, ctx context.Context) *sqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func insertEpisode(t *testing.T, ctx context.Context, store *sqlite.Store, uid, text string, createTime time.Time) {
	t.Helper()
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO episodes (uid, create_time, kind, payload_json, text, source)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		uid, createTime.Format(time.RFC3339Nano), "episode",
		`{"text":"`+text+`","confidence":1.0,"source":"test"}`, text, "test",
	); err != nil {
		t.Fatalf("insert episode %s: %v", uid, err)
	}
}
