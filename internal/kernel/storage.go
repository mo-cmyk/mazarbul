package kernel

import (
	"context"
	"database/sql"
	"time"
)

// Storage is the four-layer index dispatcher. Real implementations open each
// store on the worktree's data dir, hold connections for the process lifetime,
// and apply events when EventLog.Rebuild calls In. Reads compose across the
// four sub-stores via Query (see query.go).
//
// The dispatcher is the only writer to the derived stores. Callers must not
// open a raw SQL connection or talk directly to the graph engine; they go
// through this surface so that schema migrations, transaction boundaries, and
// rebuild semantics stay consistent.
//
// Graph layer: M1 ships LadybugDB (the maintained continuation of Kùzu after
// Kùzu's October 2025 archival). The Open method here is vendor-neutral
// (OpenGraph, GraphConn) so a future vendor switch is a contained refactor;
// see `agent-docs/tech/ladybug.md` for the concrete bindings.
type Storage interface {
	OpenSQLite(ctx context.Context) (*sql.DB, error)
	OpenGraph(ctx context.Context) (GraphConn, error)
	OpenVector(ctx context.Context) (VectorConn, error)
	OpenFTS(ctx context.Context) (FTSConn, error)

	// Apply takes a batch of events and writes each one through the four
	// derived stores. Per-event atomicity is preserved: a single event lands
	// in a SQLite tx that wraps the relational, vector, and FTS5 writes,
	// with the LadybugDB MERGE before commit so a graph failure rolls back
	// the SQLite tx. Across events, M1 commits per-event; a saga in M2
	// closes the soft-inconsistency window. Called by EventLog.Rebuild and
	// by the kernel orchestrator after each Append. The concrete
	// implementation lives in internal/kernel/storage/dispatcher.
	//
	// Apply must not retain a reference to events after return, and must
	// not mutate the slice or its elements. Callers may reuse the slice
	// for the next Append.
	Apply(ctx context.Context, events []Event) error

	// HasEvent reports whether the derived store has already applied the
	// event identified by uid. EventLog.Rebuild uses this to skip already-
	// applied events when resuming after a partial rebuild. The lookup
	// MUST be cheap (a single indexed read against the authoritative
	// layer); a slow HasEvent dominates the cost of a long rebuild.
	//
	// Contract: an unknown UID returns (false, nil), never an error. A
	// non-nil error means the store could not be queried (driver fault,
	// closed connection) and Rebuild propagates it without falling back
	// to Apply.
	//
	// Apply is idempotent at the SQLite layer (ON CONFLICT(uid) DO UPDATE
	// for episodes), so a false negative from HasEvent costs only the
	// redundant tx, not correctness.
	HasEvent(ctx context.Context, uid string) (bool, error)

	// Provenance returns the recorded source/confidence/decided-at
	// triples for an Episode event UID. Returns an empty slice (not error)
	// when nothing has been recorded. Used by `maz audit` and by any read
	// path that needs to surface where a fact came from. Derived records
	// (mentions, relations) inherit provenance via source_event_uid; the
	// caller walks back to the source Episode and queries that UID here.
	Provenance(ctx context.Context, eventUID string) ([]ProvenanceRecord, error)

	// Close shuts down every open connection. Safe to call multiple times.
	Close() error
}

// GraphConn is the kernel's view of an embedded property-graph connection. M1
// implementations wrap the LadybugDB Go bindings; the kernel contracts to the
// methods here, not to the bindings directly, so the package boundary stays
// clean and the vendor stays swappable.
type GraphConn interface {
	Execute(ctx context.Context, cypher string, args map[string]any) (GraphResult, error)
	Close() error
}

// GraphResult is a forward-only iterator over graph-query rows. Real
// implementations should support row streaming for large traversals.
type GraphResult interface {
	Next() bool
	Scan(into ...any) error
	Err() error
	Close() error
}

// VectorConn is the kernel's view of the vector index. M1 uses sqlite-vec;
// the interface is small enough that pgvector or a remote vector DB can
// drop in later via a different provider.
type VectorConn interface {
	Upsert(ctx context.Context, items []VectorItem) error
	Search(ctx context.Context, query []float32, k int, filter VectorFilter) ([]VectorHit, error)
	Delete(ctx context.Context, ids []string) error
	Close() error
}

// VectorItem is a single embedded chunk or entity.
type VectorItem struct {
	ID        string
	Embedding []float32
	Metadata  map[string]any
}

// VectorFilter narrows a Search by metadata. Empty value means no filter.
// MinTime is an inclusive lower bound and MaxTime is an exclusive upper bound
// on the episode's create_time; both default to the zero time.Time which
// means "no bound on that side". Sub-second precision is preserved end-to-end
// , the storage layer compares via SQLite julianday() so an RFC3339Nano
// bound is honoured to ~microsecond accuracy rather than truncated to whole
// seconds.
type VectorFilter struct {
	Kinds   []EventKind
	MinTime time.Time
	MaxTime time.Time

	// VectorType narrows the entities_vec lane to a specific vector
	// discriminator ("name" or "summary"). Ignored unless Kinds = [entity].
	// Empty means "any vector_type".
	VectorType string

	// Scopes restricts KNN results to items whose sidecar scope is in the
	// set. An empty ScopeSet means no scope filter (all-pass), which
	// preserves backward compatibility with callers that predate scope
	// isolation. When non-empty, the KNN CTE gains AND scope IN (...) so
	// cross-scope hits are excluded from the top-k result.
	Scopes ScopeSet
}

// VectorHit is a single Search result.
type VectorHit struct {
	ID       string
	Score    float32
	Metadata map[string]any
}

// FTSConn is the kernel's view of the full-text index. M1 uses SQLite FTS5
// over the same SQLite database as the relational store; Tantivy or
// Elasticsearch can drop in later.
type FTSConn interface {
	Index(ctx context.Context, items []FTSItem) error
	Search(ctx context.Context, query string, k int) ([]FTSHit, error)
	Delete(ctx context.Context, ids []string) error
	Close() error
}

// FTSItem is a single tokenizable chunk.
type FTSItem struct {
	ID       string
	Text     string
	Metadata map[string]any
}

// FTSHit is a single FTS5 result, with the BM25 score exposed for fused
// scoring in the query composer.
type FTSHit struct {
	ID       string
	BM25     float32
	Snippet  string
	Metadata map[string]any
}

// NoopStorage is a placeholder that returns ErrNotImplemented for every
// operation. Downstream code uses it to compile until the M1 storage
// dispatcher lands.
type NoopStorage struct{}

// OpenSQLite implements Storage. It returns ErrNotImplemented.
func (NoopStorage) OpenSQLite(_ context.Context) (*sql.DB, error) {
	return nil, ErrNotImplemented
}

// OpenGraph implements Storage. It returns ErrNotImplemented.
func (NoopStorage) OpenGraph(_ context.Context) (GraphConn, error) {
	return nil, ErrNotImplemented
}

// OpenVector implements Storage. It returns ErrNotImplemented.
func (NoopStorage) OpenVector(_ context.Context) (VectorConn, error) {
	return nil, ErrNotImplemented
}

// OpenFTS implements Storage. It returns ErrNotImplemented.
func (NoopStorage) OpenFTS(_ context.Context) (FTSConn, error) {
	return nil, ErrNotImplemented
}

// Apply implements Storage. It returns ErrNotImplemented.
func (NoopStorage) Apply(_ context.Context, _ []Event) error {
	return ErrNotImplemented
}

// HasEvent implements Storage. It returns (false, nil): a Noop store has
// applied no events, so every UID is unknown. Returning an error here
// would force every consumer (Rebuild, test fakes that embed NoopStorage)
// to either special-case the Noop or refuse to run. (false, nil) matches
// the contract's "unknown UID" semantics, which is the truth.
func (NoopStorage) HasEvent(_ context.Context, _ string) (bool, error) {
	return false, nil
}

// Provenance implements Storage. A Noop store has never recorded
// provenance for any event, so it returns (nil, nil) — the "no rows"
// signal documented on the interface method.
func (NoopStorage) Provenance(_ context.Context, _ string) ([]ProvenanceRecord, error) {
	return nil, nil
}

// Close implements Storage. It is a no-op.
func (NoopStorage) Close() error { return nil }
