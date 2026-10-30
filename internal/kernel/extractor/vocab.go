package extractor

import (
	"context"
	"database/sql"
	"fmt"
)

// VocabEntry is one entry in the registered ontology vocabulary surfaced to
// the extraction prompt. It carries the name and a human-readable description
// so the LLM can select the best-fit term rather than falling back to a generic
// core kind or predicate.
type VocabEntry struct {
	// Name is the kind or predicate identifier, e.g. "company",
	// "extension:business_de:company", or "manages_company".
	Name string

	// Description is the one-sentence explanation stored in the registry.
	Description string
}

// VocabReader is the contract the extractor uses to pull registered entity
// kinds and predicates into the prompt. Implementations must respect ctx
// cancellation and return quickly: the extractor is on the hot path of every
// episode write. A nil VocabReader or a failure degrades gracefully to the
// core-kinds-only baseline.
type VocabReader interface {
	// ExtensionKinds returns up to maxKinds active non-core entity kinds,
	// preferring extension:* rows over user:* rows. Returns nil on error so
	// callers can degrade gracefully.
	ExtensionKinds(ctx context.Context, maxKinds int) ([]VocabEntry, error)

	// ExtensionPredicates returns up to maxPredicates predicates registered by
	// a pack or user (added_by != 'seed'). Returns nil on error so callers can
	// degrade gracefully.
	ExtensionPredicates(ctx context.Context, maxPredicates int) ([]VocabEntry, error)
}

// maxExtensionKinds caps the number of extension entity kinds injected into
// the prompt. Keeps the prompt bounded even when many packs are installed.
const maxExtensionKinds = 30

// maxExtensionPredicates caps the number of extension predicates injected.
const maxExtensionPredicates = 30

// SQLiteVocabReader reads registered kinds and predicates from the SQLite
// writer pool. It is stateless (no cache): each call issues a lightweight
// indexed query to pick up the latest registered vocabulary without requiring
// a process restart. The queries are indexed and return at most maxKinds /
// maxPredicates rows, so latency is negligible relative to the LLM call.
type SQLiteVocabReader struct {
	db *sql.DB
}

// NewSQLiteVocabReader constructs a SQLiteVocabReader backed by db.
// Db must be the same writer-pool connection used for entity_kinds and
// predicates reads (the single-writer SQLite pool).
func NewSQLiteVocabReader(db *sql.DB) *SQLiteVocabReader {
	return &SQLiteVocabReader{db: db}
}

// ExtensionKinds queries entity_kinds for active non-core rows, ordering
// extension:* before user:* so pack-installed vocabulary is preferred.
func (v *SQLiteVocabReader) ExtensionKinds(ctx context.Context, maxKinds int) ([]VocabEntry, error) {
	// CASE expression puts extension:* rows first (rank 0) then user:* (rank 1).
	// Core kinds have provenance='core' and are excluded by the WHERE clause.
	rows, err := v.db.QueryContext(ctx, `
		SELECT name, description
		FROM entity_kinds
		WHERE COALESCE(status,'active') = 'active'
		  AND provenance != 'core'
		ORDER BY
			CASE WHEN provenance LIKE 'extension:%' THEN 0 ELSE 1 END,
			name
		LIMIT ?`, maxKinds)
	if err != nil {
		return nil, fmt.Errorf("vocab: extension kinds query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanVocabRows(rows)
}

// ExtensionPredicates queries predicates for non-seed rows (added_by != 'seed'),
// ordered alphabetically.
func (v *SQLiteVocabReader) ExtensionPredicates(ctx context.Context, maxPredicates int) ([]VocabEntry, error) {
	rows, err := v.db.QueryContext(ctx, `
		SELECT predicate, description
		FROM predicates
		WHERE added_by != 'seed'
		ORDER BY
			CASE WHEN added_by = 'extension' THEN 0 ELSE 1 END,
			predicate
		LIMIT ?`, maxPredicates)
	if err != nil {
		return nil, fmt.Errorf("vocab: extension predicates query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanVocabRows(rows)
}

// scanVocabRows scans a (name, description) result set into VocabEntry slice.
func scanVocabRows(rows *sql.Rows) ([]VocabEntry, error) {
	var entries []VocabEntry
	for rows.Next() {
		var e VocabEntry
		if err := rows.Scan(&e.Name, &e.Description); err != nil {
			return nil, fmt.Errorf("vocab: scan row: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vocab: iterate rows: %w", err)
	}
	return entries, nil
}
