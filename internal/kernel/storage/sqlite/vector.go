package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
)

// Vector domain names. Each domain is backed by its own pair of tables
// (a vec0 virtual table holding the embeddings, and a sidecar table that
// maps the string VectorItem.ID to the synthetic INTEGER rowid that vec0
// requires).
const (
	// DomainEpisodes routes Upsert / Delete / SearchByDomain to the
	// episodes_vec virtual table. It is the default domain selected by
	// Search and by Upsert when an item has no "domain" metadata key.
	DomainEpisodes = "episodes"

	// DomainEntities routes Upsert / Delete / SearchByDomain to the
	// entities_vec virtual table. Migration 0005 replaced the single-vector
	// schema from 0003 with a multi-vector design; entities_vec now holds
	// one row per (entity, vector_type) discriminator value ('name' or
	// 'summary').
	DomainEntities = "entities"

	// DomainMentions routes Upsert / Delete / SearchByDomain to the
	// mentions_vec virtual table added in migration 0005. Each mention has
	// a single embedding keyed on extraction_text.
	DomainMentions = "mentions"

	// DomainDocuments routes Upsert / Delete / SearchByDomain to the
	// documents_vec virtual table added in migration 0016. Each document
	// contributes one row per body chunk (keyed by "<documentUid>#<chunkIdx>"),
	// so a long document yields multiple vec rows. The documents_vec_ids sidecar
	// carries heading and byte_offset for citation hydration in P3.
	DomainDocuments = "documents"

	// DomainInsights routes Upsert / Delete / SearchByDomain to the
	// insights_vec virtual table added in migration 0038 (issue #231). Each
	// synthesized insight contributes one row keyed by its insight ULID, so the
	// recall insight lane can surface hot insights by semantic similarity.
	DomainInsights = "insights"

	// DomainConsolidated routes Upsert / Delete / SearchByDomain to the
	// consolidated_vec virtual table added in migration 0039 (issue #231). Each
	// consolidated memory contributes one row keyed by its memory ULID.
	DomainConsolidated = "consolidated"
)

// metadataDomainKey is the VectorItem.Metadata key the Vector layer reads
// to route an Upsert (and an optional per-item Delete) to a specific
// vector domain. Items without this key land in DomainEpisodes.
const metadataDomainKey = "domain"

// metadataScopeKey is the VectorItem.Metadata key the Vector layer reads
// to stamp scope on the sidecar row. Items without this key default to
// kernel.DefaultScope ("personal"). The sidecar scope is defense-in-depth
// isolation: the KNN CTEs gain AND scope IN (...) filtering so cross-scope
// hits cannot surface even when the caller bypasses the relational guard.
// Migration 0054 adds the scope column to all *_vec_ids tables.
const metadataScopeKey = "scope"

// Vector is a kernel.VectorConn backed by the sqlite-vec extension loaded
// into the same SQLite database as the relational store. Writes go through
// the serialising writer pool; searches go through the reader pool.
//
// Vector borrows the *Store pools and does not own them; Close is a no-op
// and the *Store must be Closed by the caller that constructed it.
type Vector struct {
	store *Store
}

// Compile-time check that Vector satisfies the kernel surface.
var _ kernel.VectorConn = (*Vector)(nil)

// OpenVector returns a Vector wired to the episodes_vec and entities_vec
// virtual tables created by migration 0003. The caller-supplied *Store must
// already be Open (migrations applied); OpenVector probes for the schema
// and returns a clear error if it is missing, which usually means the
// migration runner did not pick up the 0003 file.
func OpenVector(ctx context.Context, store *Store) (*Vector, error) {
	if store == nil {
		return nil, errors.New("sqlite: vector: nil store")
	}

	for _, table := range []string{
		"episodes_vec", "entities_vec", "mentions_vec", "documents_vec",
		"episodes_vec_ids", "entities_vec_ids", "mentions_vec_ids", "documents_vec_ids",
	} {
		var n int
		err := store.Reader().QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE name = ?", table,
		).Scan(&n)
		if err != nil {
			return nil, fmt.Errorf("sqlite: vector: probe %s: %w", table, err)
		}
		if n == 0 {
			return nil, fmt.Errorf("sqlite: vector: table %s missing (migration 0003 not applied)", table)
		}
	}

	return &Vector{store: store}, nil
}

// Upsert inserts or replaces a batch of embedded items. Routing per-item:
// metadata["domain"] selects "episodes" (default) or "entities". Each item
// is verified against storage.VectorDim before insertion; the provider
// boundary enforces the same invariant, but a duplicate check here is
// cheap and protects callers that bypass the provider (tests, future MCP).
//
// Scope per-item: metadata["scope"] stamps the sidecar row (defense-in-depth
// isolation; migration 0054). Absent scope defaults to kernel.DefaultScope
// ("personal"), the fail-safe direction per migration 0027's rationale.
//
// The whole batch lands in a single writer transaction so a partial failure
// rolls back the sidecar inserts together with the vec0 rows.
func (v *Vector) Upsert(ctx context.Context, items []kernel.VectorItem) (retErr error) {
	if len(items) == 0 {
		return nil
	}

	tx, err := v.store.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: vector: begin upsert tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Named return retErr so a rollback failure surfaces to the caller
		// instead of being silently dropped. An ErrTxDone here means the
		// commit raced ahead of our defer (or the tx already closed); not
		// worth joining onto retErr.
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	for i := range items {
		if err := upsertOne(ctx, tx, items[i]); err != nil {
			return fmt.Errorf("sqlite: vector: item %d (id=%q): %w", i, items[i].ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: vector: commit upsert: %w", err)
	}
	committed = true
	return nil
}

// Search returns the k nearest neighbours of query in DomainEpisodes. When
// VectorFilter carries a non-zero MinTime / MaxTime, the predicate is pushed
// into the vec0 MATCH via a rowid-IN sub-select so the returned top-k is
// guaranteed to satisfy the time window , the recall composer no longer
// needs to oversample and post-filter, eliminating the candidate-window
// starvation when in-window matches rank below the first k. Kinds-by-
// metadata filtering remains M1 wave 2 work and surfaces as
// ErrNotImplemented. For per-domain access (notably for the entities
// domain), use SearchByDomain.
//
// Scope filtering: when filter.Scopes is non-empty, a scope predicate
// is added to each KNN CTE so results are restricted to the named scopes.
// An empty ScopeSet means no scope filter (all-pass), preserving backward
// compatibility with callers that predate scope isolation.
func (v *Vector) Search(ctx context.Context, query []float32, k int, filter kernel.VectorFilter) ([]kernel.VectorHit, error) {
	if domain, ok := domainFromKinds(filter.Kinds); ok {
		// Domain hints (Kinds = [entity] / [mention] / [episode]) route to
		// the matching vector table directly. Time-window filtering with a
		// domain hint is M2 work; ignore the window in that case.
		if domain == DomainEntities && filter.VectorType != "" {
			return v.searchEntitiesByVectorTypeScoped(ctx, query, k, filter.VectorType, filter.Scopes)
		}
		return v.searchByDomainScoped(ctx, domain, query, k, filter.Scopes)
	}
	if len(filter.Kinds) > 0 {
		return nil, fmt.Errorf("sqlite: vector: Kinds filter wiring deferred for %v: %w", filter.Kinds, kernel.ErrNotImplemented)
	}
	if filter.MinTime.IsZero() && filter.MaxTime.IsZero() {
		return v.searchByDomainScoped(ctx, DomainEpisodes, query, k, filter.Scopes)
	}
	return v.searchEpisodesInTimeRange(ctx, query, k, filter.MinTime, filter.MaxTime, filter.Scopes)
}

// domainFromKinds maps a single-Kind filter to a vector domain when the
// caller's intent is unambiguous (Kinds = [entity] / [mention] / [episode]).
// Multi-Kind filters or unrecognised kinds fall through to the default path.
func domainFromKinds(kinds []kernel.EventKind) (string, bool) {
	if len(kinds) != 1 {
		return "", false
	}
	switch kinds[0] {
	case kernel.EventKind("entity"):
		return DomainEntities, true
	case kernel.EventKind("mention"):
		return DomainMentions, true
	case kernel.EventKind("episode"):
		return DomainEpisodes, true
	case kernel.EventKind("documentRevision"):
		return DomainDocuments, true
	default:
		return "", false
	}
}

// searchEpisodesInTimeRange does the vec0 MATCH against episodes_vec with a
// rowid pre-filter derived from episodes.create_time. MinBound and maxBound
// are time.Time values; either zero (IsZero) means "no bound on that side".
// MinBound is inclusive (>=); maxBound is exclusive (<). The CTE scans
// episodes (small relative to vec0) and the resulting rowid set becomes the
// candidate space for the MATCH , vec0 returns top-k from within that space,
// so top-k is top-k IN WINDOW.
//
// Scope filtering: when scopes is non-empty, a scope predicate is added
// to the CTE restricting results to the named scopes. An empty ScopeSet
// means no scope filter (all-pass).
//
// Precision: episodes.create_time is RFC 3339 text written with
// time.RFC3339Nano. Both sides of the comparison are normalised to
// nanoseconds-since-epoch INTEGER in SQL (via unixNanoExpr on the
// column, t.UnixNano() on the bound) so the comparison is bit-exact at
// nanosecond resolution , a row at .0001Z and a bound at .0004Z stay
// distinct rather than rounding through julianday's float
// representation. M2's dispatcher can add a generated unix-nanos column
// and index it if benchmarks demand it.
func (v *Vector) searchEpisodesInTimeRange(ctx context.Context, query []float32, k int, minBound, maxBound time.Time, scopes kernel.ScopeSet) ([]kernel.VectorHit, error) {
	if k <= 0 {
		return nil, fmt.Errorf("sqlite: vector: k must be > 0, got %d", k)
	}
	if k > maxSearchK {
		return nil, fmt.Errorf("sqlite: vector: k=%d exceeds ceiling %d", k, maxSearchK)
	}
	if err := validateEmbedding(query); err != nil {
		return nil, fmt.Errorf("sqlite: vector: search query: %w", err)
	}

	blob, err := sqlite_vec.SerializeFloat32(query)
	if err != nil {
		return nil, fmt.Errorf("sqlite: vector: serialize query: %w", err)
	}

	// Build the WHERE clause incrementally so a one-sided window (only
	// MinTime, only MaxTime) does not embed a useless predicate. Both
	// sides are normalised to UnixNano integers in SQL so the comparison
	// is bit-exact at nanosecond resolution; julianday() loses precision
	// below the microsecond boundary (the column is reconstructed via
	// strftime + variable-width fractional extraction, see unixNanoExpr).
	const baseCTE = `
		WITH allowed AS (
			SELECT ids.rowid
			FROM episodes_vec_ids AS ids
			JOIN episodes AS e ON e.uid = ids.id
			WHERE 1=1`
	clauses := baseCTE
	args := []any{}
	if !minBound.IsZero() {
		clauses += " AND " + unixNanoExpr("e.create_time") + " >= ?"
		args = append(args, minBound.UnixNano())
	}
	if !maxBound.IsZero() {
		clauses += " AND " + unixNanoExpr("e.create_time") + " < ?"
		args = append(args, maxBound.UnixNano())
	}
	// Defense-in-depth scope filter on the sidecar (belt and braces alongside
	// the relational join above). An empty ScopeSet is all-pass.
	if !scopes.IsEmpty() {
		pred, scopeArgs := scopes.Predicate("ids")
		clauses += " AND " + pred
		args = append(args, scopeArgs...)
	}
	clauses += `
		)
		SELECT ids.id, v.distance
		FROM episodes_vec AS v
		JOIN episodes_vec_ids AS ids ON ids.rowid = v.rowid
		WHERE v.embedding MATCH ?
		  AND k = ?
		  AND v.rowid IN (SELECT rowid FROM allowed)
		ORDER BY v.distance`
	args = append(args, blob, k)

	rows, err := v.store.Reader().QueryContext(ctx, clauses, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: vector: search episodes_vec (time-bounded): %w", err)
	}
	defer func() { _ = rows.Close() }()

	hits := make([]kernel.VectorHit, 0, k)
	for rows.Next() {
		var (
			id       string
			distance float32
		)
		if err := rows.Scan(&id, &distance); err != nil {
			return nil, fmt.Errorf("sqlite: vector: scan time-bounded hit: %w", err)
		}
		hits = append(hits, kernel.VectorHit{ID: id, Score: distance})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: vector: iterate time-bounded hits: %w", err)
	}
	return hits, nil
}

// unixNanoExpr returns a SQLite expression that converts an RFC3339Nano
// text column (e.g., episodes.create_time) to nanoseconds since the unix
// epoch as an INTEGER. The corpus writes timestamps via Go's
// time.RFC3339Nano which omits trailing zeros, so the fractional part has
// variable width (0 to 9 digits).
//
// To stay immune to any SQLite version that might round (rather than
// truncate) fractional seconds in strftime('%s', ...), we hand strftime
// the whole-second TEXT PREFIX of the column , the substring before the
// '.' with a 'Z' appended , so strftime never sees a fractional part it
// could round. The fractional nanoseconds are then added back from the
// text between '.' and 'Z', padded to 9 digits.
//
// The col argument is spliced literally into SQL and MUST come from
// trusted internal code (a literal column reference like "e.create_time"),
// never from user input.
func unixNanoExpr(col string) string {
	// Whole-second prefix: text up to (but not including) '.', then 'Z'.
	// When no fractional part is present, use the column verbatim.
	wholePrefix := "(CASE WHEN instr(" + col + ", '.') > 0 " +
		"THEN substr(" + col + ", 1, instr(" + col + ", '.') - 1) || 'Z' " +
		"ELSE " + col + " END)"
	return "(CAST(strftime('%s', " + wholePrefix + ") AS INTEGER) * 1000000000 + " +
		"CASE WHEN instr(" + col + ", '.') > 0 THEN " +
		"CAST(substr(" +
		"substr(" + col + ", instr(" + col + ", '.') + 1, instr(" + col + ", 'Z') - instr(" + col + ", '.') - 1)" +
		" || '000000000', 1, 9" +
		") AS INTEGER) " +
		"ELSE 0 END)"
}

// maxSearchK caps the k parameter on Search / SearchByDomain. Vec0 will
// happily accept arbitrarily large k and scan the entire index, which a
// future MCP caller could trigger to pin a reader. The ceiling is generous
// enough to cover any plausible recall composer batch (M1 wave 2 fuses
// vector + FTS top-K and rarely needs more than a few hundred candidates).
const maxSearchK = 1024

// SearchEntitiesByVectorType runs a top-k vec0 search against entities_vec
// restricted to a single vector_type ("name" or "summary"). The
// vector_type discriminator on entities_vec_ids lets the resolver
// distinguish a name-embedding nearest-neighbour (Step 1 of the matching
// pipeline) from a summary-embedding nearest-neighbour (Step 3) without
// mixing the two ranking spaces.
//
// Scope filtering is all-pass (empty ScopeSet) to preserve backward
// compatibility with callers that predate scope isolation. For scope-filtered
// entity searches use Search with VectorFilter.Scopes set.
func (v *Vector) SearchEntitiesByVectorType(ctx context.Context, query []float32, k int, vectorType string) ([]kernel.VectorHit, error) {
	return v.searchEntitiesByVectorTypeScoped(ctx, query, k, vectorType, kernel.ScopeSet{})
}

// searchEntitiesByVectorTypeScoped is the internal implementation that applies
// optional scope filtering to the entities KNN CTE. An empty ScopeSet means no
// scope filter (all-pass). Called by SearchEntitiesByVectorType (empty scopes)
// and by Search when VectorFilter.Scopes is non-empty.
func (v *Vector) searchEntitiesByVectorTypeScoped(ctx context.Context, query []float32, k int, vectorType string, scopes kernel.ScopeSet) ([]kernel.VectorHit, error) {
	if k <= 0 {
		return nil, fmt.Errorf("sqlite: vector: k must be > 0, got %d", k)
	}
	if k > maxSearchK {
		return nil, fmt.Errorf("sqlite: vector: k=%d exceeds ceiling %d", k, maxSearchK)
	}
	if err := validateEmbedding(query); err != nil {
		return nil, fmt.Errorf("sqlite: vector: search query: %w", err)
	}
	if vectorType != "name" && vectorType != "summary" {
		return nil, fmt.Errorf("sqlite: vector: vector_type %q: only 'name' or 'summary' supported", vectorType)
	}

	blob, err := sqlite_vec.SerializeFloat32(query)
	if err != nil {
		return nil, fmt.Errorf("sqlite: vector: serialize query: %w", err)
	}

	// Two-step: collect the rowids on entities_vec_ids that carry the
	// requested vector_type (and optionally the scope filter), then ask vec0
	// for the top-k embedding distances restricted to that rowid set. vec0
	// supports a `rowid IN (...)` predicate alongside the MATCH clause.
	cteWhere := "vector_type = ?"
	args := []any{vectorType}
	if !scopes.IsEmpty() {
		pred, scopeArgs := scopes.Predicate("") // no alias; column is bare "scope"
		cteWhere += " AND " + pred
		args = append(args, scopeArgs...)
	}
	q := fmt.Sprintf(`
		WITH allowed AS (
			SELECT rowid, id FROM entities_vec_ids WHERE %s
		)
		SELECT a.id, v.distance
		FROM entities_vec AS v
		JOIN allowed AS a ON a.rowid = v.rowid
		WHERE v.embedding MATCH ?
		  AND v.rowid IN (SELECT rowid FROM allowed)
		  AND k = ?
		ORDER BY v.distance
	`, cteWhere)
	args = append(args, blob, k)

	rows, err := v.store.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: vector: search entities_vec[%s]: %w", vectorType, err)
	}
	defer func() { _ = rows.Close() }()

	hits := make([]kernel.VectorHit, 0, k)
	for rows.Next() {
		var (
			id       string
			distance float32
		)
		if err := rows.Scan(&id, &distance); err != nil {
			return nil, fmt.Errorf("sqlite: vector: scan hit: %w", err)
		}
		hits = append(hits, kernel.VectorHit{ID: id, Score: distance})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: vector: iterate hits: %w", err)
	}
	return hits, nil
}

// SearchNameVectors is a thin alias for SearchEntitiesByVectorType with
// vectorType="name". The kernel's HybridQuery uses an unexported
// `nameVectorSearcher` interface (declared in internal/kernel/query_hybrid.go)
// to feed its fourth fusion lane; until this method existed the type
// assertion silently returned ok=false and the lane was a no-op in every
// production run. Keep the method name as-is; the interface is named-typed,
// not structural-typed, by the kernel package.
//
// Scope is all-pass (empty ScopeSet) to preserve the pre-scope behaviour of
// this wikilink-resolution lane. The relational guard in scoped_reader.go
// enforces isolation for all entity reads; this lane is defense-in-depth.
func (v *Vector) SearchNameVectors(ctx context.Context, query []float32, k int) ([]kernel.VectorHit, error) {
	return v.searchEntitiesByVectorTypeScoped(ctx, query, k, "name", kernel.ScopeSet{})
}

// SearchByDomain runs a top-k vec0 search against the named domain. It is
// the access path used by tests and by the recall composer once it lands;
// the canonical Search method always targets DomainEpisodes.
//
// Scope filtering is all-pass (empty ScopeSet) to preserve backward
// compatibility with callers that predate scope isolation. For scope-filtered
// domain searches, call Search with a VectorFilter that sets Scopes.
//
// Distance metric: vec0's default is L2. BGE-M3 (the M1 default embedder
// per storage.VectorDim's docstring) emits L2-normalised vectors, so L2
// distance is rank-equivalent to cosine for the M1 corpus. When a future
// embedder lands that does not L2-normalise, this migration grows a
// distance_metric=cosine constraint on the vec0 DDL.
func (v *Vector) SearchByDomain(ctx context.Context, domain string, query []float32, k int) ([]kernel.VectorHit, error) {
	return v.searchByDomainScoped(ctx, domain, query, k, kernel.ScopeSet{})
}

// searchByDomainScoped is the internal implementation that applies optional
// scope filtering to the domain KNN CTE. An empty ScopeSet means no scope
// filter (all-pass). Called by SearchByDomain (empty scopes) and by Search
// when VectorFilter.Scopes is non-empty.
func (v *Vector) searchByDomainScoped(ctx context.Context, domain string, query []float32, k int, scopes kernel.ScopeSet) ([]kernel.VectorHit, error) {
	if k <= 0 {
		return nil, fmt.Errorf("sqlite: vector: k must be > 0, got %d", k)
	}
	if k > maxSearchK {
		return nil, fmt.Errorf("sqlite: vector: k=%d exceeds ceiling %d", k, maxSearchK)
	}
	if err := validateEmbedding(query); err != nil {
		return nil, fmt.Errorf("sqlite: vector: search query: %w", err)
	}
	vecTable, idsTable, err := resolveDomain(domain)
	if err != nil {
		return nil, err
	}

	blob, err := sqlite_vec.SerializeFloat32(query)
	if err != nil {
		return nil, fmt.Errorf("sqlite: vector: serialize query: %w", err)
	}

	// Build WHERE clause: scope IN (...) is appended when scopes are non-empty
	// (defense-in-depth on top of the relational guard). The empty-set case
	// preserves all-scopes behavior for callers that predate scope isolation.
	scopeClause := ""
	scopeArgs := []any{}
	if !scopes.IsEmpty() {
		pred, sa := scopes.Predicate("ids")
		scopeClause = "AND " + pred
		scopeArgs = sa
	}
	q := fmt.Sprintf(`
		SELECT ids.id, v.distance
		FROM %s AS v
		JOIN %s AS ids ON ids.rowid = v.rowid
		WHERE v.embedding MATCH ?
		  AND k = ?
		  %s
		ORDER BY v.distance
	`, vecTable, idsTable, scopeClause)
	args := append([]any{blob, k}, scopeArgs...)

	rows, err := v.store.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: vector: search %s: %w", vecTable, err)
	}
	defer func() { _ = rows.Close() }()

	hits := make([]kernel.VectorHit, 0, k)
	for rows.Next() {
		var (
			id       string
			distance float32
		)
		if err := rows.Scan(&id, &distance); err != nil {
			return nil, fmt.Errorf("sqlite: vector: scan hit: %w", err)
		}
		// kernel.VectorHit.Score carries the raw vec0 distance (lower = closer)
		// for M1. The recall composer (M1 wave 2) is responsible for fusing
		// distances with FTS BM25 by converting to similarity; do not flip the
		// sign here without coordinating with that fusion code.
		hits = append(hits, kernel.VectorHit{ID: id, Score: distance})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: vector: iterate hits: %w", err)
	}
	return hits, nil
}

// Delete removes the named IDs from both domains. An ID present in neither
// domain is a silent no-op (deletes are idempotent). All work lands in a
// single writer transaction.
//
// Large batches are split into chunks of deleteBatchSize per SQL statement
// so that callers cleaning up a large corpus do not trip SQLite's default
// 999-bound-variable limit. The single outer transaction still gives the
// caller all-or-nothing semantics across every chunk.
func (v *Vector) Delete(ctx context.Context, ids []string) (retErr error) {
	if len(ids) == 0 {
		return nil
	}

	tx, err := v.store.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: vector: begin delete tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Named return retErr so a rollback failure surfaces to the caller
		// instead of being silently dropped.
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	for _, domain := range []string{DomainEpisodes, DomainEntities, DomainMentions, DomainDocuments, DomainInsights, DomainConsolidated} {
		for start := 0; start < len(ids); start += deleteBatchSize {
			end := start + deleteBatchSize
			if end > len(ids) {
				end = len(ids)
			}
			if err := deleteFromDomain(ctx, tx, domain, ids[start:end]); err != nil {
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: vector: commit delete: %w", err)
	}
	committed = true
	return nil
}

// deleteBatchSize caps the number of placeholders a single DELETE statement
// can emit. SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 999; we leave
// headroom for the two-statement-per-domain shape (one SELECT plus two
// DELETEs all parameterised against the same chunk).
const deleteBatchSize = 500

// deleteFromDomain removes every id present in the named domain's sidecar
// (and the matching vec0 row). Missing ids are silently skipped so the
// caller sees the canonical idempotent-delete semantics.
//
// Batching strategy: vec0 does not accept a correlated subquery in its
// DELETE clause, so we first collect the matching rowids from the sidecar,
// then issue one DELETE per table. Total round trips: 3 per domain
// (SELECT + DELETE vec + DELETE sidecar), regardless of len(ids).
func deleteFromDomain(ctx context.Context, tx *sql.Tx, domain string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	vecTable, idsTable := tableNames(domain)

	placeholders, args := inClause(ids)

	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf("SELECT rowid FROM %s WHERE id IN (%s)", idsTable, placeholders), args...,
	)
	if err != nil {
		return fmt.Errorf("sqlite: vector: lookup ids in %s: %w", idsTable, err)
	}
	defer func() { _ = rows.Close() }()

	rowids := make([]int64, 0, len(ids))
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			return fmt.Errorf("sqlite: vector: scan rowid from %s: %w", idsTable, err)
		}
		rowids = append(rowids, rowid)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: vector: iterate %s: %w", idsTable, err)
	}
	if len(rowids) == 0 {
		return nil
	}

	rowidPlaceholders, rowidArgs := inClauseInt64(rowids)
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE rowid IN (%s)", vecTable, rowidPlaceholders), rowidArgs...,
	); err != nil {
		return fmt.Errorf("sqlite: vector: delete from %s: %w", vecTable, err)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE rowid IN (%s)", idsTable, rowidPlaceholders), rowidArgs...,
	); err != nil {
		return fmt.Errorf("sqlite: vector: delete from %s: %w", idsTable, err)
	}
	return nil
}

// inClause builds a SQL placeholder list ("?, ?, ?, ...") matching the
// length of vals along with the []any args slice the database/sql package
// expects. SQLite's parameter limit (999 by default) caps the practical
// batch size; callers pre-validate len(ids) upstream if they go higher.
func inClause(vals []string) (string, []any) {
	placeholders := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		placeholders[i] = "?"
		args[i] = v
	}
	return strings.Join(placeholders, ","), args
}

// inClauseInt64 is the int64 sibling of inClause.
func inClauseInt64(vals []int64) (string, []any) {
	placeholders := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		placeholders[i] = "?"
		args[i] = v
	}
	return strings.Join(placeholders, ","), args
}

// Close is a no-op: Vector borrows the *Store pools and does not own them.
// Safe to call multiple times.
func (v *Vector) Close() error { return nil }

// upsertOne writes a single VectorItem inside the caller-supplied
// transaction. Factored out of Upsert so the per-item code stays readable
// (gocognit keeps the public method under threshold). Errors are wrapped
// by the caller with the item index for batch-position context.
func upsertOne(ctx context.Context, tx *sql.Tx, item kernel.VectorItem) error {
	// Reject empty IDs up front. The sidecar column is TEXT NOT NULL UNIQUE,
	// so SQLite accepts "" silently; two malformed items would then collide
	// on the same sidecar row and the second Upsert would overwrite the
	// first vector's vec0 slot. Catching the missing UID at this boundary
	// keeps a forgotten ULID assignment loud rather than silent.
	if item.ID == "" {
		return errors.New("empty id")
	}
	if err := validateEmbedding(item.Embedding); err != nil {
		return err
	}
	domain, err := domainFor(item.Metadata)
	if err != nil {
		return err
	}
	if domain == DomainDocuments {
		// documents_vec is managed exclusively by the dispatcher's per-chunk
		// path: applyDocumentRevision pre-inserts documents_vec_ids rows with the
		// required NOT NULL document_uid + chunk_index (plus heading/byte_offset),
		// then writeDocumentChunkVectorAsync writes the vec0 slot against the
		// stable rowid. The generic id-only upsertIDMapping here cannot satisfy
		// those sidecar columns, so reject the call loudly rather than let it fail
		// later with an opaque NOT NULL constraint error.
		return fmt.Errorf("vector upsert: domain %q is dispatcher-managed; emit a documentRevision event instead of calling Vector.Upsert", DomainDocuments)
	}
	vecTable, idsTable := tableNames(domain)
	scope := scopeFromMetadata(item.Metadata)

	// Serialize the new embedding before mutating any tables so a malformed
	// blob never leaves vec0 transiently empty mid-transaction. The outer
	// tx rollback would mask the issue, but keeping the failure path before
	// the destructive step makes the intent clear.
	blob, err := sqlite_vec.SerializeFloat32(item.Embedding)
	if err != nil {
		return fmt.Errorf("serialize embedding: %w", err)
	}

	var rowid int64
	if domain == DomainEntities {
		// entities_vec_ids has UNIQUE(id, vector_type) rather than UNIQUE(id);
		// the standard upsertIDMapping conflict clause would not match. Route
		// to the entity-specific helper which uses the correct conflict target
		// and selects the 'name' slot (the default and the only slot reachable
		// through the generic Vector.Upsert API; dispatcher paths write 'summary'
		// rows with explicit vector_type).
		rowid, err = upsertEntityIDMapping(ctx, tx, item.ID, scope)
	} else {
		rowid, err = upsertIDMapping(ctx, tx, idsTable, item.ID, scope)
	}
	if err != nil {
		return err
	}

	// vec0 virtual tables do not support "INSERT OR REPLACE"; the replace
	// path is delete-then-insert. The sidecar row stays (so the rowid is
	// stable across re-upsert) and only the vec0 slot is rewritten. Both
	// statements live inside the same tx.
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE rowid = ?", vecTable), rowid,
	); err != nil {
		return fmt.Errorf("clear %s: %w", vecTable, err)
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("INSERT INTO %s(rowid, embedding) VALUES (?, ?)", vecTable),
		rowid, blob,
	); err != nil {
		return fmt.Errorf("write %s: %w", vecTable, err)
	}
	return nil
}

// scopeFromMetadata extracts the scope string from a VectorItem's Metadata,
// defaulting to kernel.DefaultScope ("personal") when absent. This is the
// fail-safe direction: an item without an explicit scope tag is treated as
// personal rather than team, so a forgotten scope is a visibility miss, never
// a cross-scope leak.
func scopeFromMetadata(meta map[string]any) string {
	if meta == nil {
		return string(kernel.DefaultScope)
	}
	raw, ok := meta[metadataScopeKey]
	if !ok {
		return string(kernel.DefaultScope)
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return string(kernel.DefaultScope)
	}
	return s
}

// validateEmbedding rejects any embedding whose length differs from
// storage.VectorDim. Mirrors the provider-side check at
// internal/provider/ollama/embed.go so logs use the same error idiom across
// the two enforcement points; a future model swap that bumps VectorDim
// surfaces as a build-time recompile, not a silent index corruption.
func validateEmbedding(v []float32) error {
	if got := len(v); got != storage.VectorDim {
		return fmt.Errorf("embedding length %d, want %d: refusing to mix dimensions in the vector index", got, storage.VectorDim)
	}
	return nil
}

// domainFor returns the routing domain for an item. An item without a
// "domain" metadata key lands in DomainEpisodes; any other unrecognised
// value is an error so a typo cannot silently sink writes into the wrong
// table.
func domainFor(meta map[string]any) (string, error) {
	if meta == nil {
		return DomainEpisodes, nil
	}
	raw, ok := meta[metadataDomainKey]
	if !ok {
		return DomainEpisodes, nil
	}
	domain, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("metadata[%q] is %T, want string", metadataDomainKey, raw)
	}
	if _, _, err := resolveDomain(domain); err != nil {
		return "", err
	}
	return domain, nil
}

// resolveDomain maps a domain name to its (vec table, ids table) pair, or
// returns an error if the name is unrecognised. Use tableNames in hot paths
// where the domain has already been validated upstream.
func resolveDomain(domain string) (vec, ids string, err error) {
	switch domain {
	case DomainEpisodes, DomainEntities, DomainMentions, DomainDocuments, DomainInsights, DomainConsolidated:
		v, i := tableNames(domain)
		return v, i, nil
	default:
		return "", "", fmt.Errorf("unknown vector domain %q (want %q, %q, %q, %q, %q, or %q)", domain, DomainEpisodes, DomainEntities, DomainMentions, DomainDocuments, DomainInsights, DomainConsolidated)
	}
}

// tableNames is the trusted version of resolveDomain for callers that have
// already validated the domain.
func tableNames(domain string) (vec, ids string) {
	switch domain {
	case DomainEntities:
		return "entities_vec", "entities_vec_ids"
	case DomainMentions:
		return "mentions_vec", "mentions_vec_ids"
	case DomainDocuments:
		return "documents_vec", "documents_vec_ids"
	case DomainInsights:
		return "insights_vec", "insights_vec_ids"
	case DomainConsolidated:
		return "consolidated_vec", "consolidated_vec_ids"
	default:
		return "episodes_vec", "episodes_vec_ids"
	}
}

// upsertIDMapping ensures the sidecar table has a row for id with the given
// scope, and returns the integer rowid the vec0 table should use. The upsert
// uses ON CONFLICT(id) DO UPDATE SET scope = excluded.scope so a re-Upsert
// refreshes the scope in the sidecar (e.g. when an item is promoted from
// personal to team scope). Existing rowid is preserved — a re-Upsert against
// vec0 hits the same slot — and fresh ids allocate a new rowid via AUTOINCREMENT.
//
// Valid for every sidecar table with UNIQUE(id). Do NOT call for entities_vec_ids
// which has UNIQUE(id, vector_type); use upsertEntityIDMapping instead.
func upsertIDMapping(ctx context.Context, tx *sql.Tx, idsTable, id, scope string) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(
			"INSERT INTO %s(id, scope) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET scope = excluded.scope",
			idsTable,
		), id, scope,
	); err != nil {
		return 0, fmt.Errorf("insert sidecar %s: %w", idsTable, err)
	}
	var rowid int64
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT rowid FROM %s WHERE id = ?", idsTable), id,
	).Scan(&rowid); err != nil {
		return 0, fmt.Errorf("lookup sidecar %s rowid: %w", idsTable, err)
	}
	return rowid, nil
}

// upsertEntityIDMapping is the entities_vec_ids counterpart of upsertIDMapping.
// The entities_vec_ids sidecar uses UNIQUE(id, vector_type) rather than UNIQUE(id),
// so the conflict target must name both columns. The generic Vector.Upsert API has
// no vector_type parameter; the row is written with the default vector_type='name'.
// Dispatcher paths that write 'summary' embeddings use their own INSERT path and
// are not affected by this function.
func upsertEntityIDMapping(ctx context.Context, tx *sql.Tx, id, scope string) (int64, error) {
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO entities_vec_ids(id, scope) VALUES (?, ?) "+
			"ON CONFLICT(id, vector_type) DO UPDATE SET scope = excluded.scope",
		id, scope,
	); err != nil {
		return 0, fmt.Errorf("insert entities sidecar: %w", err)
	}
	// Select the 'name' slot specifically: entities can have both 'name' and
	// 'summary' rows for the same id, so a bare WHERE id=? is ambiguous.
	var rowid int64
	if err := tx.QueryRowContext(ctx,
		"SELECT rowid FROM entities_vec_ids WHERE id = ? AND vector_type = 'name'",
		id,
	).Scan(&rowid); err != nil {
		return 0, fmt.Errorf("lookup entity sidecar rowid: %w", err)
	}
	return rowid, nil
}
