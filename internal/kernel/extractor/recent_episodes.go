package extractor

import (
	"context"
	"database/sql"
	"fmt"
)

// SQLiteRecentEpisodes is the production RecentEpisodesProvider. It runs a
// single indexed query against the episodes table ordered by create_time
// descending, returning up to k texts strictly older than the current
// episode's create_time. Constructed with the dispatcher's reader pool so the
// extractor never contends with the writer.
//
// The lookup is keyed by create_time rather than by ULID prefix because two
// episodes generated in the same millisecond can race on the ULID's random
// suffix, and create_time is the field every downstream consumer already
// relies on for temporal ordering.
type SQLiteRecentEpisodes struct {
	reader *sql.DB
}

// NewSQLiteRecentEpisodes returns a RecentEpisodesProvider backed by reader.
// Reader must be the dispatcher's read-only pool: the provider is consulted
// on every extraction call and must not block writes.
func NewSQLiteRecentEpisodes(reader *sql.DB) *SQLiteRecentEpisodes {
	return &SQLiteRecentEpisodes{reader: reader}
}

// RecentEpisodes implements RecentEpisodesProvider. Returns up to k episode
// texts with create_time strictly less than the current episode's
// create_time, ordered most-recent-first. Missing current-episode rows
// (extraction running before the SQLite row lands) fall back to "all
// episodes" so the provider degrades gracefully under write-then-extract
// ordering races.
func (p *SQLiteRecentEpisodes) RecentEpisodes(ctx context.Context, beforeEpisodeUID string, k int) ([]string, error) {
	if p.reader == nil || k <= 0 {
		return nil, nil
	}

	var anchor sql.NullString
	if beforeEpisodeUID != "" {
		err := p.reader.QueryRowContext(ctx,
			`SELECT create_time FROM episodes WHERE uid = ?`, beforeEpisodeUID,
		).Scan(&anchor)
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("recent episodes: load anchor: %w", err)
		}
	}

	var (
		rows *sql.Rows
		err  error
	)
	if anchor.Valid {
		rows, err = p.reader.QueryContext(ctx,
			`SELECT text FROM episodes
			 WHERE create_time < ? AND text != ''
			 ORDER BY create_time DESC
			 LIMIT ?`, anchor.String, k,
		)
	} else {
		rows, err = p.reader.QueryContext(ctx,
			`SELECT text FROM episodes
			 WHERE text != ''
			 ORDER BY create_time DESC
			 LIMIT ?`, k,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("recent episodes: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0, k)
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("recent episodes: scan: %w", err)
		}
		out = append(out, text)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent episodes: iterate: %w", err)
	}
	return out, nil
}
