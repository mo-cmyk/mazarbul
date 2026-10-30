package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// FTSStore implements kernel.FTSConn against the episodes_fts virtual table
// created by 0002_fts5_episodes.up.sql. It borrows the reader pool from the
// parent *Store; the parent owns lifecycle, so Close here is a no-op and is
// safe to call multiple times.
//
// Index and Delete are no-ops in M1: the episodes_fts table runs in FTS5
// external-content mode (content='episodes'), and the AFTER INSERT, UPDATE,
// DELETE triggers on episodes keep the index in lock-step with the content
// table. The methods exist to satisfy kernel.FTSConn for the eventual
// dispatcher path that may write directly to non-episode-backed indexes.
type FTSStore struct {
	reader *sql.DB
}

// OpenFTS returns a kernel.FTSConn bound to this Store's reader pool. The
// future four-layer dispatcher wraps the returned value into
// kernel.Storage.OpenFTS.
func (s *Store) OpenFTS(_ context.Context) (kernel.FTSConn, error) {
	if s.writer == nil || s.reader == nil {
		return nil, errors.New("sqlite: store is closed")
	}
	return &FTSStore{reader: s.reader}, nil
}

// Index is a no-op. See the type-level godoc for why.
func (f *FTSStore) Index(_ context.Context, _ []kernel.FTSItem) error { return nil }

// Search runs an FTS5 MATCH query and returns up to k hits ordered most-
// relevant first. The episodes JOIN translates the FTS rowid back to the
// kernel-level episode uid so callers do not see the SQLite rowid.
//
// SQLite's rank column returns smaller-is-better (negative for matches)
// under the configured bm25(10.0, 5.0) weight function from migration
// 0002_fts5_episodes.up.sql. The FTSHit.BM25 contract is larger-is-
// better, so we negate before returning. We use the special `rank`
// column (rather than calling `bm25()` directly) so the configured
// weights apply; a bare `bm25(episodes_fts)` call defaults to weights
// (1.0, 1.0) and silently bypasses the migration's text/source bias.
//
// The query string is passed verbatim to FTS5's MATCH operator. Callers
// should pre-validate or escape user input that may contain FTS5 syntax
// operators (`"`, `*`, `:`, `AND`/`OR`/`NOT`). Malformed queries surface
// as a wrapped error from this method rather than silently returning no
// hits. An empty or whitespace-only query short-circuits to no hits.
func (f *FTSStore) Search(ctx context.Context, query string, k int) ([]kernel.FTSHit, error) {
	if k <= 0 {
		return nil, nil
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	const q = `
SELECT e.uid,
       snippet(episodes_fts, 0, '<b>', '</b>', '...', 30),
       episodes_fts.rank
FROM episodes_fts
JOIN episodes e ON e.rowid = episodes_fts.rowid
WHERE episodes_fts MATCH ?
ORDER BY episodes_fts.rank
LIMIT ?;`
	rows, err := f.reader.QueryContext(ctx, q, query, k)
	if err != nil {
		return nil, fmt.Errorf("sqlite/fts: search: %w", err)
	}
	defer rows.Close()

	var hits []kernel.FTSHit
	for rows.Next() {
		var (
			uid     string
			snippet string
			score   float64
		)
		if err := rows.Scan(&uid, &snippet, &score); err != nil {
			return nil, fmt.Errorf("sqlite/fts: scan: %w", err)
		}
		hits = append(hits, kernel.FTSHit{
			ID:      uid,
			BM25:    float32(-score),
			Snippet: snippet,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite/fts: iterate: %w", err)
	}
	return hits, nil
}

// SearchDocuments runs an FTS5 MATCH over documents_fts (title + body, BM25
// weights from migration 0016) and returns up to k live documents,
// most-relevant first. FTSHit.ID is the document UID (= its entity UID).
// The snippet is taken from the body column (index 1) so a title-only match
// still excerpts the opening prose. Satisfies the kernel's optional
// document-lane capability probed via type assertion (see
// kernel.documentFTSSearcher); fakes without this method simply run recall
// without the document FTS lane.
func (f *FTSStore) SearchDocuments(ctx context.Context, query string, k int) ([]kernel.FTSHit, error) {
	if k <= 0 {
		return nil, nil
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	const q = `
SELECT d.document_uid,
       snippet(documents_fts, 1, '<b>', '</b>', '...', 30),
       documents_fts.rank
FROM documents_fts
JOIN documents d ON d.rowid = documents_fts.rowid
WHERE documents_fts MATCH ?
  AND d.deleted = 0
ORDER BY documents_fts.rank
LIMIT ?;`
	rows, err := f.reader.QueryContext(ctx, q, query, k)
	if err != nil {
		return nil, fmt.Errorf("sqlite/fts: search documents: %w", err)
	}
	defer rows.Close()

	var hits []kernel.FTSHit
	for rows.Next() {
		var (
			uid     string
			snippet string
			score   float64
		)
		if err := rows.Scan(&uid, &snippet, &score); err != nil {
			return nil, fmt.Errorf("sqlite/fts: scan document hit: %w", err)
		}
		// rank is smaller-is-better (negative for matches); FTSHit.BM25 is
		// larger-is-better, mirroring Search above.
		hits = append(hits, kernel.FTSHit{
			ID:      uid,
			BM25:    float32(-score),
			Snippet: snippet,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite/fts: iterate document hits: %w", err)
	}
	return hits, nil
}

// SearchInTimeRange is the time-bounded sibling of Search. It JOINs
// episodes_fts to episodes and applies a julianday() predicate on
// create_time so LIMIT k returns the top-k IN-WINDOW hits , the recall
// composer no longer has to oversample and post-filter. MinBound and
// maxBound are time.Time values; either zero (IsZero) means "no bound on
// that side". The bound semantics match RecallRequest godoc: minBound is
// inclusive (>=), maxBound is exclusive (<).
//
// Precision: both sides of the comparison are normalised to
// nanoseconds-since-epoch INTEGER in SQL (via unixNanoExpr on the column,
// t.UnixNano() on the bound) so the comparison is bit-exact at
// nanosecond resolution rather than rounding through julianday's float
// representation. Matches the vector retriever's push-down path so a
// sub-millisecond bound is honoured consistently across both lanes.
//
// This method is an additive complement to Search; it lives on the concrete
// *FTSStore rather than the kernel.FTSConn interface so introducing it does
// not force every implementation (today: this one, plus the noop) to grow a
// new method. The composer reaches for it via a type-assertion.
func (f *FTSStore) SearchInTimeRange(ctx context.Context, query string, k int, minBound, maxBound time.Time) ([]kernel.FTSHit, error) {
	if k <= 0 {
		return nil, nil
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	clauses := `
SELECT e.uid,
       snippet(episodes_fts, 0, '<b>', '</b>', '...', 30),
       episodes_fts.rank
FROM episodes_fts
JOIN episodes e ON e.rowid = episodes_fts.rowid
WHERE episodes_fts MATCH ?`
	args := []any{query}
	if !minBound.IsZero() {
		clauses += "\n  AND " + unixNanoExpr("e.create_time") + " >= ?"
		args = append(args, minBound.UnixNano())
	}
	if !maxBound.IsZero() {
		clauses += "\n  AND " + unixNanoExpr("e.create_time") + " < ?"
		args = append(args, maxBound.UnixNano())
	}
	clauses += "\nORDER BY episodes_fts.rank\nLIMIT ?;"
	args = append(args, k)

	rows, err := f.reader.QueryContext(ctx, clauses, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite/fts: search (time-bounded): %w", err)
	}
	defer rows.Close()

	var hits []kernel.FTSHit
	for rows.Next() {
		var (
			uid     string
			snippet string
			score   float64
		)
		if err := rows.Scan(&uid, &snippet, &score); err != nil {
			return nil, fmt.Errorf("sqlite/fts: scan time-bounded hit: %w", err)
		}
		hits = append(hits, kernel.FTSHit{
			ID:      uid,
			BM25:    float32(-score),
			Snippet: snippet,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite/fts: iterate time-bounded hits: %w", err)
	}
	return hits, nil
}

// Delete is a no-op. See the type-level godoc for why.
func (f *FTSStore) Delete(_ context.Context, _ []string) error { return nil }

// Close is a no-op; the parent *Store owns the pools.
func (f *FTSStore) Close() error { return nil }

var _ kernel.FTSConn = (*FTSStore)(nil)
