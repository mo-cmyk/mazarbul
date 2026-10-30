package tools

// author_lookup.go — per-hit author attribution for recall and getContext.
//
// When verbose recall is served, each hit's Citations[0] is an event UID.
// Resolving the event's Author.PrincipalUID via json_extract on the episodes
// table and then joining the principals table yields the human-readable
// display name.  The lookup is batched across all hits in one SQL round-trip
// to keep the per-recall overhead negligible.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// AuthorLookup resolves a batch of event UIDs to their author's display name.
// A nil implementation (or a nil *SQLiteAuthorLookup) is a valid no-op: all
// names are returned as "" so the caller falls back to omitting the author
// field rather than panicking. Wire the real implementation via
// Recall.WithAuthorLookup.
type AuthorLookup interface {
	// BatchEventAuthors returns a map from eventUID to the author's display
	// name. An event UID not present in the returned map should be treated as
	// unknown (empty name). Best-effort: errors are returned but the caller
	// typically logs them and continues with an empty map rather than failing
	// the entire recall.
	BatchEventAuthors(ctx context.Context, eventUIDs []string) (map[string]string, error)
}

// SQLiteAuthorLookup implements AuthorLookup against the kernel's SQLite
// read-only connection. It runs a single batched query that joins episodes
// and principals so N hits cost one SQL round-trip regardless of hit count.
type SQLiteAuthorLookup struct {
	db *sql.DB
}

// NewSQLiteAuthorLookup constructs a SQLiteAuthorLookup backed by db, which
// must be the kernel's SQLite read handle (the same one passed to query.go
// and get_context.go). Nil db is accepted and produces a no-op lookup.
func NewSQLiteAuthorLookup(db *sql.DB) *SQLiteAuthorLookup {
	return &SQLiteAuthorLookup{db: db}
}

// BatchEventAuthors queries the episodes and principals tables for the given
// event UIDs and returns a map from eventUID → author display name.
// Unknown or anonymous events are silently omitted from the map (the caller
// treats a missing key as "author unknown").
func (a *SQLiteAuthorLookup) BatchEventAuthors(ctx context.Context, eventUIDs []string) (map[string]string, error) {
	if a == nil || a.db == nil || len(eventUIDs) == 0 {
		return map[string]string{}, nil
	}
	// Build the IN (?, ?, …) clause.  placeholders is programmer-controlled
	// (only "?" characters and commas); no user input enters the query string.
	placeholders := strings.Repeat("?,", len(eventUIDs))
	placeholders = placeholders[:len(placeholders)-1]
	//nolint:gosec // G202: placeholder string is "?,?,?" — fully programmer-controlled, not user input
	q := `
		SELECT
			e.uid,
			COALESCE(p.display_name, '')
		FROM episodes e
		LEFT JOIN principals p
			ON p.principal_uid = json_extract(e.payload_json, '$.author.principalUid')
		WHERE e.uid IN (` + placeholders + `)`
	args := make([]any, len(eventUIDs))
	for i, uid := range eventUIDs {
		args[i] = uid
	}
	rows, err := a.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("author lookup: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string, len(eventUIDs))
	for rows.Next() {
		var uid, name string
		if sErr := rows.Scan(&uid, &name); sErr != nil {
			return nil, fmt.Errorf("author lookup: scan: %w", sErr)
		}
		if name != "" {
			out[uid] = name
		}
	}
	if rErr := rows.Err(); rErr != nil {
		return nil, fmt.Errorf("author lookup: iterate: %w", rErr)
	}
	return out, nil
}
