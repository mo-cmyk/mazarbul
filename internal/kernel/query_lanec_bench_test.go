package kernel

// Lane C benchmarks:
//   - BenchmarkApplyAnchorFilter  — pre-fusion anchor filter with varying
//     row and frontier sizes (R11 hot-path gate).
//   - BenchmarkBuildVerbatimFTSQuery — verbatim-query builder called on
//     every FTS lane invocation (R3).
//   - BenchmarkRunFTSLane_verbatim_no_fallback — single-pass FTS path when
//     verbatim returns ≥ threshold hits (R3 common path).
//   - BenchmarkRunFTSLane_fuzzy_fallback — two-pass FTS path when verbatim
//     returns fewer hits than threshold (R3 fallback path).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// BenchmarkApplyAnchorFilter measures the pre-fusion anchor filter with
// N episodes × M frontier entities. Mirrors applySpatialFilter's SQL round
// trip cost via episodesCitingEntities.
func BenchmarkApplyAnchorFilter(b *testing.B) {
	sizes := []struct{ episodes, frontier int }{
		{10, 5},
		{100, 10},
		{500, 50},
	}
	for _, sz := range sizes {
		b.Run(fmt.Sprintf("episodes=%d frontier=%d", sz.episodes, sz.frontier), func(b *testing.B) {
			db := buildBenchDB(b, sz.episodes, sz.frontier)
			h, err := NewHybridQuery(nil, &stubVector{}, &stubFTS{}, db)
			if err != nil {
				b.Fatalf("NewHybridQuery: %v", err)
			}

			rows := make(map[string]hydratedRow, sz.episodes)
			for i := range sz.episodes {
				rows[fmt.Sprintf("ep%05d", i)] = hydratedRow{kind: "episode"}
			}
			frontier := make(map[string]struct{}, sz.frontier)
			for i := range sz.frontier {
				frontier[fmt.Sprintf("ent%05d", i)] = struct{}{}
			}

			ctx := withScopes(context.Background(), defaultScopeSet())
			b.ResetTimer()
			for b.Loop() {
				if _, err := h.applyAnchorFilter(ctx, rows, frontier); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBuildVerbatimFTSQuery measures the token-parsing path called on
// every FTS lane invocation before the verbatim probe.
func BenchmarkBuildVerbatimFTSQuery(b *testing.B) {
	queries := []string{
		"berlin",
		"who do I know in Berlin",
		"machine learning project Zephyr quarterly review",
	}
	for _, q := range queries {
		b.Run(fmt.Sprintf("tokens=%d", len(q)/5+1), func(b *testing.B) {
			for b.Loop() {
				_ = buildVerbatimFTSQuery(q)
			}
		})
	}
}

// BenchmarkRunFTSLane_verbatim_no_fallback benchmarks the common one-pass
// path where the verbatim query already returns enough hits.
func BenchmarkRunFTSLane_verbatim_no_fallback(b *testing.B) {
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()

	hits := make([]FTSHit, 10)
	for i := range hits {
		hits[i] = FTSHit{ID: fmt.Sprintf("ep%02d", i)}
	}
	fts := &stubFTS{hits: hits}

	h, _ := NewHybridQuery(nil, &stubVector{}, fts, db,
		WithFuzzyFallbackThreshold(3),
	)
	verbatim := buildVerbatimFTSQuery("berlin project")
	prefix := escapeFTSQuery("berlin project")
	req := RecallRequest{}
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		_, _, _ = h.runFTSLane(ctx, verbatim, prefix, 10, req)
	}
}

// BenchmarkRunFTSLane_fuzzy_fallback benchmarks the two-pass path where the
// verbatim query returns fewer hits than the threshold.
func BenchmarkRunFTSLane_fuzzy_fallback(b *testing.B) {
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()

	// twoPassFTS re-used from query_lanec_test.go (same package).
	allHits := make([]FTSHit, 5)
	for i := range allHits {
		allHits[i] = FTSHit{ID: fmt.Sprintf("ep%02d", i)}
	}

	verbatim := buildVerbatimFTSQuery("niche query")
	prefix := escapeFTSQuery("niche query")
	req := RecallRequest{}
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		fts := &twoPassFTS{allHits: allHits}
		h, _ := NewHybridQuery(nil, &stubVector{}, fts, db,
			WithFuzzyFallbackThreshold(3),
		)
		_, _, _ = h.runFTSLane(ctx, verbatim, prefix, 10, req)
	}
}

// buildBenchDB creates a minimal SQLite DB populated with nEpisodes episodes
// and nFrontier entities, each episode citing one entity.
func buildBenchDB(b *testing.B, nEpisodes, nFrontier int) *sql.DB {
	b.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		b.Fatalf("open sqlite: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `
		CREATE TABLE episodes (uid TEXT PRIMARY KEY, kind TEXT, scope TEXT DEFAULT 'project');
		CREATE TABLE mentions (source_event_uid TEXT, entity_uid TEXT, scope TEXT DEFAULT 'project');
		CREATE INDEX mentions_src ON mentions(source_event_uid);
		CREATE INDEX mentions_ent ON mentions(entity_uid);
	`)
	if err != nil {
		b.Fatalf("create schema: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("begin tx: %v", err)
	}
	for i := range nEpisodes {
		epUID := fmt.Sprintf("ep%05d", i)
		entUID := fmt.Sprintf("ent%05d", i%nFrontier)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO episodes(uid, kind) VALUES (?, 'episode')`, epUID,
		); err != nil {
			b.Fatalf("insert episode: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO mentions(source_event_uid, entity_uid) VALUES (?, ?)`, epUID, entUID,
		); err != nil {
			b.Fatalf("insert mention: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}
	return db
}
