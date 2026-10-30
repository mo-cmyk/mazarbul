// Package dispatcher implements kernel.Storage by fanning each Apply'd
// event into the four derived stores: SQLite (relational), LadybugDB
// (graph), sqlite-vec (vector), and FTS5 (full text). It is the only
// writer to those stores per the architectural commitment in
// agent-docs/synthesis.md and agent-docs/designs/storage.md §5.
//
// Dispatches on event.Kind: KindEpisode (four-layer write), KindEntity
// (relational + multi-vector + graph), KindMention (relational + vector).
// Unknown kinds are logged at DEBUG and skipped.
package dispatcher

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.uber.org/zap"
	"golang.org/x/mod/semver"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/ladybug"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// ErrClosed is returned by Apply / Open* once Close has run. The
// dispatcher cannot be reopened; construct a new one via Open.
var ErrClosed = errors.New("dispatcher: closed")

// ErrEmbeddingDim is returned by Apply when the configured Embedder
// returns a vector whose length differs from storage.VectorDim. The
// dispatcher surfaces this before opening any transaction so neither
// SQLite nor LadybugDB observes a partial write.
var ErrEmbeddingDim = errors.New("dispatcher: embedding dimension mismatch")

// Embedder produces a fixed-dimension embedding for the natural-language
// payload of an Episode. Implementations must return vectors of length
// storage.VectorDim. The dispatcher imports this interface rather than
// importing internal/provider/* directly so the kernel boundary stays
// vendor-neutral; #55 wires the Ollama-backed embedder into Config.Embedder.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Config bundles the inputs the dispatcher needs at construction time.
// A non-empty DataDir is required. Embedder may be nil (vector inserts
// are skipped, useful for tests that only exercise relational + graph).
// Logger may be nil (defaults to zap.NewNop()).
type Config struct {
	DataDir  string
	Embedder Embedder
	Logger   *zap.Logger

	// Stats is the runtime counter sink. When non-nil the dispatcher
	// emits IncDedup("semantic") on every suppressed write so the
	// doctor / Usage panel can report how often the gate fired. Nil
	// falls back to stats.Noop() so the rest of the code can call the
	// counter unconditionally.
	Stats stats.Store

	// DedupSemanticThreshold is the cosine-similarity floor that
	// suppresses near-duplicate Episode writes. Zero (the default)
	// disables the check. When > 0, applyEpisode embeds the candidate
	// synchronously, runs a top-K nearest-neighbor search against
	// existing Episode vectors, and skips persistence if the best
	// match is above threshold. Matches mem0 v3's NOOP path
	// (mem0/memory/main.py search_with_filters).
	DedupSemanticThreshold float64
}

// Dispatcher implements kernel.Storage by owning the four layer
// connections and routing every Apply call through them in a single
// logical transaction.
//
// Lifecycle: Open constructs all four layers under cfg.DataDir; Close
// shuts every one down. Apply is safe to call concurrently — the SQLite
// writer pool serialises via MaxOpenConns=1 and LadybugDB serialises via
// its own internal mutex, so per-event ordering across goroutines is
// implementation-defined but per-event atomicity is preserved.
type Dispatcher struct {
	// mu guards the lifecycle: Open* / Apply hold an RLock so they run
	// concurrently; Close holds the Lock and waits for in-flight Apply
	// calls to drain before tearing down the layer connections. Without
	// this coordination, an Apply that already passed the closed check
	// can race against Close zeroing the layer pointers (Greptile #95).
	mu     sync.RWMutex
	closed bool

	sqlite *sqlite.Store
	vector *sqlite.Vector
	fts    kernel.FTSConn
	graph  *ladybug.Conn
	embed  Embedder
	logger *zap.Logger
	stats  stats.Store

	// dedupSemanticThreshold is the cosine-similarity floor for the
	// pre-write semantic dedup branch. Zero disables the branch entirely
	// — applyEpisode keeps its legacy async-embed path. When > 0,
	// applyEpisode embeds the candidate synchronously, searches the
	// existing Episode vectors, and suppresses the write if the best
	// existing match is at or above this similarity.
	dedupSemanticThreshold float64

	// bgVec tracks background episode-embedding goroutines spawned by
	// applyEpisode. Apply returns as soon as the episode row + graph node
	// are durable; the embedder call + vec0 upsert run on a detached
	// goroutine because the embedder is a network round-trip whose
	// latency must not pin the writer (Ollama bge-m3 typically 50-500ms
	// per call; a LongMemEval haystack of 500+ episodes amplified that
	// into multi-minute Apply stalls under the prior synchronous shape).
	// Close drains bgVec before tearing down the SQLite pools so a worker
	// never writes through a closed vec0.
	bgVec sync.WaitGroup

	// bgVecDepth mirrors bgVec's internal counter so external callers
	// (the idleStatus MCP tool, eval benchmarks) can poll progress
	// without the unexported WaitGroup state. Incremented next to
	// every bgVec.Add(1); decremented next to every bgVec.Done().
	bgVecDepth atomic.Int64

	// genesisPin is the persisted genesis trust anchor read from state.toml
	// at daemon start. When non-nil, ReduceMembershipPinned is used in place
	// of ReduceMembership so the fold is anchored to the pinned genesis
	// self-grant rather than the lowest-ULID TOFU root. Protected by mu.
	genesisPin *ontology.GenesisPin
}

// noopCloseGraph wraps a kernel.GraphConn so Close is a no-op. The
// dispatcher hands these out from OpenGraph; closing the wrapper does
// not propagate to the dispatcher-owned LadybugDB connection, so a
// read-side caller that does `g, _ := d.OpenGraph(ctx); defer g.Close()`
// cannot brick the dispatcher (Greptile #95).
type noopCloseGraph struct{ inner kernel.GraphConn }

func (g *noopCloseGraph) Execute(ctx context.Context, cypher string, args map[string]any) (kernel.GraphResult, error) {
	return g.inner.Execute(ctx, cypher, args)
}

func (*noopCloseGraph) Close() error { return nil }

var _ kernel.Storage = (*Dispatcher)(nil)

// SetEmbedder installs the embedder used by Apply for vector inserts.
// The kernel orchestrator (issue #55) needs this to break a
// construction cycle: the embedding cache wraps the SQLite pools that
// only Open can provision, so Open is called with Config.Embedder=nil,
// the cache is constructed against the dispatcher's pools, and then
// SetEmbedder installs the cache. Safe to call before any Apply.
//
// SetEmbedder still writes the field after Close, but Apply checks
// d.closed under the same lock and rejects with ErrClosed, so the
// newly installed embedder is never actually invoked on a closed
// dispatcher.
func (d *Dispatcher) SetEmbedder(e Embedder) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.embed = e
}

// SetGenesisPin installs the persisted genesis trust anchor that was loaded
// from state.toml at daemon start. When the pin is non-nil, every subsequent
// membership fold uses ReduceMembershipPinned (anchored to the pinned
// principal's self-grant) instead of the TOFU lowest-ULID root. This closes
// the remote-replay genesis-hijack surface: an adversary who pushes a crafted
// self-grant with a lower ULID than the real genesis can no longer displace
// the genesis owner on replay. Call before accepting any events (i.e. before
// or immediately after Open, not mid-flight during Apply).
func (d *Dispatcher) SetGenesisPin(pin *ontology.GenesisPin) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.genesisPin = pin
}

// Open initialises all four layers under cfg.DataDir and returns a
// Dispatcher ready to Apply events. On a partial-failure path Open
// closes every layer it had already opened before returning the error.
//
// Layout under cfg.DataDir:
//   - index.db          SQLite database (relational + vec + FTS5)
//   - graph.ladybug     LadybugDB database root directory
func Open(ctx context.Context, cfg Config) (*Dispatcher, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("dispatcher: empty DataDir")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	sqlitePath := filepath.Join(cfg.DataDir, "index.db")
	graphPath := filepath.Join(cfg.DataDir, "graph.ladybug")

	store, err := sqlite.Open(ctx, sqlitePath)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: open sqlite: %w", err)
	}
	vec, err := sqlite.OpenVector(ctx, store)
	if err != nil {
		return nil, joinClose(fmt.Errorf("dispatcher: open vector: %w", err), store.Close())
	}
	ftsConn, err := store.OpenFTS(ctx)
	if err != nil {
		return nil, joinClose(fmt.Errorf("dispatcher: open fts: %w", err), store.Close())
	}
	graph, err := ladybug.OpenLadybug(ctx, graphPath)
	if err != nil {
		return nil, joinClose(fmt.Errorf("dispatcher: open ladybug: %w", err), store.Close())
	}

	st := cfg.Stats
	if st == nil {
		st = stats.Noop()
	}
	return &Dispatcher{
		sqlite:                 store,
		vector:                 vec,
		fts:                    ftsConn,
		graph:                  graph,
		embed:                  cfg.Embedder,
		logger:                 logger,
		stats:                  st,
		dedupSemanticThreshold: cfg.DedupSemanticThreshold,
	}, nil
}

// OpenSQLite returns the dispatcher's SQLite *writer* pool
// (MaxOpenConns=1, serialised). It satisfies the kernel.Storage
// interface; pair with SQLiteReader for the read-only pool. Direct
// writes are discouraged — go through Apply so the four layers stay
// coherent. The returned *sql.DB is borrowed: callers must not Close
// it; the dispatcher owns the pool lifecycle.
func (d *Dispatcher) OpenSQLite(_ context.Context) (*sql.DB, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}
	return d.sqlite.Writer(), nil
}

// SQLiteReader returns the dispatcher's read-only SQLite pool (scales
// with NumCPU; never blocks the single-writer pool). The kernel
// orchestrator (#55) needs both reader and writer to construct the
// embedding cache (cache.New takes them separately to mirror the
// vec0 layer's pool split). Borrowed handle: callers must not Close
// it. Returns ErrClosed after Dispatcher.Close.
func (d *Dispatcher) SQLiteReader() (*sql.DB, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}
	return d.sqlite.Reader(), nil
}

// OpenGraph returns a non-closing wrapper over the dispatcher's
// LadybugDB connection. Calling Close on the returned handle is a
// no-op; the dispatcher owns the underlying connection lifecycle and
// closes it from Dispatcher.Close.
func (d *Dispatcher) OpenGraph(_ context.Context) (kernel.GraphConn, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}
	return &noopCloseGraph{inner: d.graph}, nil
}

// OpenVector returns the dispatcher's vector layer. Vector.Close is
// already a no-op (the *sqlite.Vector borrows pools from *sqlite.Store)
// so no wrapper is needed.
func (d *Dispatcher) OpenVector(_ context.Context) (kernel.VectorConn, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}
	return d.vector, nil
}

// OpenFTS returns the dispatcher's FTS5 layer. FTSStore.Close is
// already a no-op (it borrows the reader pool from *sqlite.Store) so
// no wrapper is needed.
func (d *Dispatcher) OpenFTS(_ context.Context) (kernel.FTSConn, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}
	return d.fts, nil
}

// Apply writes each event through all four layers. Events whose Kind is
// not ontology.KindEpisode are skipped (DEBUG-logged) for M1; future
// kinds extend the switch.
//
// Per-event transactional shape:
//  1. Embed the Episode text outside the SQLite tx (no writer lock held
//     while the embedder runs).
//  2. Open a SQLite writer tx (immediate lock).
//  3. INSERT/UPSERT into episodes (triggers project text/source into
//     episodes_fts inside the same tx).
//  4. INSERT/UPSERT into episodes_vec_ids and episodes_vec.
//  5. MERGE the (:Episode) node in LadybugDB BEFORE committing SQLite.
//  6. Commit SQLite. If LadybugDB or the commit fails, the SQLite tx
//     rolls back and the error surfaces to the caller.
//
// Callers should pass a context that is not subject to short-deadline
// cancellation: when Embedder is a *cache.CachingEmbedder, a canceled
// ctx returns control to applyEpisode early while the cache's
// singleflight goroutine (context.WithoutCancel) keeps running and
// later writes to the same SQLite pools — racing Dispatcher.Close.
// The kernel orchestrator (issue #55) hands Apply a non-cancelable
// context via context.WithoutCancel; future callers (Rebuild #27,
// daemon #42) must do the same or accept the race.
//
// Soft-inconsistency window: the rare SQLite-commit-after-LadybugDB-
// success failure leaves an Episode node without its row. The event log
// is the source of truth; Rebuild reconciles. M2 closes the gap with a
// saga (issue #55+).
func (d *Dispatcher) Apply(ctx context.Context, events []kernel.Event) error {
	if len(events) == 0 {
		return nil
	}
	// RLock for the duration of the call so Close blocks until every
	// in-flight Apply drains. Multiple Apply calls hold the RLock
	// concurrently; the underlying SQLite writer pool and LadybugDB
	// connection serialise actual writes internally.
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return ErrClosed
	}
	return d.applyLocked(ctx, events)
}

// HasEvent implements kernel.Storage by checking that the event identified
// by uid is present in *every* derived layer the dispatcher would have
// written: episodes (SQLite), episodes_vec_ids (vector sidecar, only when
// an embedder is configured), and the LadybugDB Episode node. Returning
// true means "Apply has nothing left to do for this UID"; false means
// "at least one layer is missing, run Apply to reconstruct."
//
// This stricter check is the only way Rebuild can recover from partial
// derived-store corruption (e.g. a hand-rolled `rm -rf graph.ladybug`
// while index.db survives). A SQLite-only check would short-circuit
// Apply and leave the missing layer empty forever. Apply is idempotent
// at every layer (ON CONFLICT upsert in SQLite, DELETE-then-INSERT for
// vec0, MERGE on the graph), so a false negative here is at worst a
// redundant tx.
//
// The SQL probe runs against the read-only pool, not the writer pool:
// SQLite is in WAL mode so readers and writers do not block each other,
// and the writer pool (MaxOpenConns=1) is a serialisation point that
// every Apply already contends for. A 1000-event rebuild calls HasEvent
// 1000 times; routing those through the multi-connection reader pool
// keeps them concurrent with any live Apply call.
//
// M1 stores only Episode kinds in derived stores, so a non-Episode event
// legitimately returns (false, nil); Rebuild then calls Apply, which is
// a no-op for non-Episode kinds. When a future issue extends the
// dispatcher to other kinds, this lookup extends to their tables too.
func (d *Dispatcher) HasEvent(ctx context.Context, uid string) (bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return false, ErrClosed
	}
	// SQL layer: episodes (always written for Episode kind) + the vector
	// layer (written only when an embedder is configured). Vector
	// presence requires BOTH the sidecar id<->rowid row in
	// episodes_vec_ids AND the actual vec0 row in episodes_vec joined
	// by rowid — recall queries do that join, so anything less is a lie.
	// One round trip via EXISTS keeps the read cheap. Note the sidecar
	// table's natural-key column is `id` (see migration 0003), not `uid`.
	//
	// The third column checks the documents table for documentRevision events:
	// the row probe here is only the cheap first gate (revision_uid = ev.UID,
	// written by the UPSERT in applyDocumentRevision). When it matches,
	// documentRevisionApplied runs the strict per-layer check — sidecar rows
	// for live documents, plus their vec0 rows when an embedder is wired —
	// mirroring the episode rule above. Row presence alone would short-circuit
	// Rebuild before the same-revision repair path in applyDocumentRevision
	// could run, leaving a lost documents_vec_ids sidecar unrepairable from
	// the event log.
	const q = `
		SELECT
			EXISTS(SELECT 1 FROM episodes WHERE uid = ?) AS has_episode,
			EXISTS(
				SELECT 1
				FROM episodes_vec_ids AS ids
				JOIN episodes_vec     AS v ON v.rowid = ids.rowid
				WHERE ids.id = ?
			) AS has_vec,
			EXISTS(SELECT 1 FROM documents WHERE revision_uid = ?) AS has_doc_revision
	`
	var hasEpisode, hasVec, hasDocRevision bool
	if err := d.sqlite.Reader().QueryRowContext(ctx, q, uid, uid, uid).Scan(&hasEpisode, &hasVec, &hasDocRevision); err != nil {
		return false, fmt.Errorf("dispatcher: hasEvent uid=%s: %w", uid, err)
	}
	// Document revision: require every synchronously derived layer, so a
	// Rebuild re-Applies the revision when one is missing.
	if hasDocRevision {
		return d.documentRevisionApplied(ctx, uid)
	}
	if !hasEpisode {
		return false, nil
	}
	if d.embed != nil && !hasVec {
		return false, nil
	}

	// Graph layer: an Episode node must exist for this UID. Cheap because
	// uid is the natural key under the MERGE in mergeEpisodeNode.
	r, err := d.graph.Execute(ctx,
		"MATCH (e:Episode {uid: $uid}) RETURN 1 LIMIT 1",
		map[string]any{"uid": uid},
	)
	if err != nil {
		return false, fmt.Errorf("dispatcher: hasEvent graph uid=%s: %w", uid, err)
	}
	hasNode := r.Next()
	if cerr := r.Close(); cerr != nil {
		return false, fmt.Errorf("dispatcher: hasEvent graph close uid=%s: %w", uid, cerr)
	}
	return hasNode, nil
}

// Provenance reads the provenance rows that the dispatcher wrote alongside
// the Episode event identified by uid. Returns an empty slice (not error)
// when no row exists, which is the case for events that pre-date migration
// 0009 and for non-Episode events (whose provenance is inherited via
// source_event_uid from their parent Episode — callers walk back to the
// episode and call Provenance again on that UID).
//
// Migration 0009 added a UNIQUE constraint on event_uid so in M1 the
// returned slice is always either empty or of length 1. The slice form is
// kept on the surface so a future M2 schema change can carry multiple
// provenance entries (e.g. multi-source voting) without API churn.
func (d *Dispatcher) Provenance(ctx context.Context, uid string) ([]kernel.ProvenanceRecord, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}
	const q = `
		SELECT event_uid, source, confidence, decided_at
		FROM provenance
		WHERE event_uid = ?
	`
	rows, err := d.sqlite.Reader().QueryContext(ctx, q, uid)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: provenance uid=%s: %w", uid, err)
	}
	defer rows.Close()

	var out []kernel.ProvenanceRecord
	for rows.Next() {
		var (
			rec       kernel.ProvenanceRecord
			decidedAt string
		)
		if err := rows.Scan(&rec.EventUID, &rec.Source, &rec.Confidence, &decidedAt); err != nil {
			return nil, fmt.Errorf("dispatcher: provenance scan uid=%s: %w", uid, err)
		}
		t, perr := time.Parse(time.RFC3339Nano, decidedAt)
		if perr != nil {
			return nil, fmt.Errorf("dispatcher: provenance parse decidedAt uid=%s: %w", uid, perr)
		}
		rec.DecidedAt = t.UTC()
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dispatcher: provenance rows uid=%s: %w", uid, err)
	}
	return out, nil
}

// applyLocked dispatches each event under the caller-held mu.RLock.
// Split out so HasEvent and Apply can share the closed-check + lock
// pattern without duplicating the dispatch loop.
//
//nolint:gocyclo // dispatch switch is intentionally exhaustive over all event kinds; adding a new kind is the only reason to edit this function.
func (d *Dispatcher) applyLocked(ctx context.Context, events []kernel.Event) error {
	logger := loggerFromCtx(ctx, d.logger)
	for i := range events {
		ev := events[i]
		var err error
		switch ev.Kind {
		case kernel.EventKind(ontology.KindEpisode):
			err = d.applyEpisode(ctx, ev)
		case kernel.EventKind(ontology.KindEntity):
			err = d.applyEntity(ctx, ev)
		case kernel.EventKind(ontology.KindMention):
			err = d.applyMention(ctx, ev)
		case kernel.EventKind(ontology.KindRelationCandidatesProposed):
			// RelationCandidatesProposed events are consumed by the resolver (P3).
			// The dispatcher acknowledges the event with a debug log; the resolver
			// post-write hook is what reads these and emits the canonical Relation
			// events that this dispatcher writes via applyRelation below.
			logger.Debug("dispatcher: relation candidates proposed (consumed by resolver)",
				zap.String("event_uid", ev.UID),
			)
			continue
		case kernel.EventKind(ontology.KindRelation):
			err = d.applyRelation(ctx, ev)
		case kernel.EventKind(ontology.KindFact):
			err = d.applyFact(ctx, ev)
		case kernel.EventKind(ontology.KindSourceRefRecorded):
			err = d.applySourceRefRecorded(ctx, ev)
		case kernel.EventKind(ontology.KindAgentFeedbackRecorded):
			err = d.applyAgentFeedbackRecorded(ctx, ev)
		case kernel.EventKind(ontology.KindAgentFeedbackReviewed):
			err = d.applyAgentFeedbackReviewed(ctx, ev)
		case kernel.EventKind(ontology.KindAgentFeedbackPromoted):
			err = d.applyAgentFeedbackPromoted(ctx, ev)
		case kernel.EventKind(ontology.KindConnectorWatermarkAdvanced):
			err = d.applyConnectorWatermarkAdvanced(ctx, ev)
		case kernel.EventKind(ontology.KindRelationRetracted):
			err = d.applyRelationRetracted(ctx, ev)
		case kernel.EventKind(ontology.KindRelationPromoted):
			err = d.applyRelationPromoted(ctx, ev)
		case kernel.EventKind(ontology.KindRelationSupersedeConflict):
			// Concurrent-supersede conflicts surface a cross-device
			// data state the resolver could not auto-resolve. The event
			// itself is the durable conflict record; no derived-store
			// mutation is needed because the open relations referenced
			// by the payload are already live in SQLite + LadybugDB.
			// The TUI's /review conflicts query reads these events from
			// the log to drive operator-led resolution.
			logger.Warn("dispatcher: relationSupersedeConflict (consumed by /review)",
				zap.String("event_uid", ev.UID),
			)
			continue
		case kernel.EventKind(ontology.KindPredicateRegistered):
			err = d.applyPredicateRegistered(ctx, ev)
		case kernel.EventKind(ontology.KindEntityKindRegistered):
			err = d.applyEntityKindRegistered(ctx, ev)
		case kernel.EventKind(ontology.KindEntityKindReviewed):
			err = d.applyEntityKindReviewed(ctx, ev)
		case kernel.EventKind(ontology.KindSchemaRegistered):
			err = d.applySchemaRegistered(ctx, ev)
		case kernel.EventKind(ontology.KindPrincipalRegistered):
			err = d.applyPrincipalRegistered(ctx, ev)
		case kernel.EventKind(ontology.KindDeviceEnrolled):
			err = d.applyDeviceEnrolled(ctx, ev)
		case kernel.EventKind(ontology.KindDeviceRevoked):
			err = d.applyDeviceRevoked(ctx, ev)
		case kernel.EventKind(ontology.KindPresetInstalled):
			err = d.applyPresetInstalled(ctx, ev)
		case kernel.EventKind(ontology.KindPresetDeactivated):
			err = d.applyPresetDeactivated(ctx, ev)
		case kernel.EventKind(ontology.KindTeamMemberGranted):
			err = d.applyTeamMemberGranted(ctx, ev)
		case kernel.EventKind(ontology.KindTeamMemberRevoked):
			err = d.applyTeamMemberRevoked(ctx, ev)
		case kernel.EventKind(ontology.KindTeamRoleChanged):
			err = d.applyTeamRoleChanged(ctx, ev)
		case kernel.EventKind(ontology.KindEntityIdentifierAdded):
			err = d.applyEntityIdentifierAdded(ctx, ev)
		case kernel.EventKind(ontology.KindEntityUpdated):
			err = d.applyEntityUpdated(ctx, ev)
		case kernel.EventKind(ontology.KindEntityMerged):
			err = d.applyEntityMerged(ctx, ev)
		case kernel.EventKind(ontology.KindEntityDeleted):
			err = d.applyEntityDeleted(ctx, ev)
		case kernel.EventKind(ontology.KindEpisodeForgotten):
			err = d.applyEpisodeForgotten(ctx, ev)
		case kernel.EventKind(ontology.KindRelationForgotten):
			err = d.applyRelationForgotten(ctx, ev)
		case kernel.EventKind(ontology.KindFactRetracted):
			err = d.applyFactRetracted(ctx, ev)
		case kernel.EventKind(ontology.KindFactForgotten):
			err = d.applyFactForgotten(ctx, ev)
		case kernel.EventKind(ontology.KindDocumentRevision), kernel.EventKind(ontology.KindDocumentDeleted):
			err = d.applyDocumentEvent(ctx, ev)
		case kernel.EventKind(ontology.KindMemoryAccessed):
			err = d.applyMemoryAccessed(ctx, ev)
		case kernel.EventKind(ontology.KindInsightSynthesized):
			err = d.applyInsightSynthesized(ctx, ev)
		case kernel.EventKind(ontology.KindInsightDismissed):
			err = d.applyInsightDismissed(ctx, ev)
		case kernel.EventKind(ontology.KindMemoryConsolidated):
			err = d.applyMemoryConsolidated(ctx, ev)
		case kernel.EventKind(ontology.KindScopePromoted):
			err = d.applyScopePromoted(ctx, ev)
		case kernel.EventKind(ontology.KindScopeDiscarded):
			err = d.applyScopeDiscarded(ctx, ev)
		case kernel.EventKind(ontology.KindFactRehomed):
			err = d.applyFactRehomed(ctx, ev)
		case kernel.EventKind(ontology.KindContextPinned):
			err = d.applyContextPinned(ctx, ev)
		case kernel.EventKind(ontology.KindContextUnpinned):
			err = d.applyContextUnpinned(ctx, ev)
		case kernel.EventKind(ontology.KindContextCorrected):
			err = d.applyContextCorrected(ctx, ev)
		default:
			logger.Debug("dispatcher: skip unsupported event kind",
				zap.String("event_uid", ev.UID),
				zap.String("kind", string(ev.Kind)),
			)
			continue
		}
		if err != nil {
			return fmt.Errorf("apply event %d (uid=%s): %w", i, ev.UID, err)
		}
	}
	return nil
}

// applyEpisode runs the episode write. By default the embedder call
// and vec0 upsert are spawned on a detached goroutine so a slow Ollama
// round-trip never pins the writer goroutine (the async path; recall
// via FTS sees the episode immediately and the vec0 lane catches up
// once the background worker drains, tracked by d.bgVec). When the
// dedupSemanticThreshold is set, the dispatcher takes the *sync*
// dedup branch instead: embed inline, search for near-duplicates,
// suppress the write entirely if a >= threshold match exists,
// otherwise persist the episode with the vector inline.
//
//nolint:gocyclo // WIP: applyEpisode handles legacy + sync-dedup + four-layer writes; refactor staged for separate PR.
func (d *Dispatcher) applyEpisode(ctx context.Context, ev kernel.Event) (retErr error) {
	var episode ontology.Episode
	if err := json.Unmarshal(ev.Payload, &episode); err != nil {
		return fmt.Errorf("decode episode payload: %w", err)
	}

	// Sync dedup branch (Phase 2 mem0-parity). Only fires when the
	// threshold knob is configured AND we have an embedder + vector
	// connection — otherwise fall through to the legacy async-embed
	// path so the test seam without an embedder keeps working.
	if d.dedupSemanticThreshold > 0 && d.embed != nil && d.vector != nil {
		dup, vec, err := d.checkSemanticDuplicate(ctx, ev.UID, episode.Text)
		if err != nil {
			// Embed or search failure: log and fall through to the
			// legacy path so a transient embedder hiccup doesn't lose
			// the episode entirely. Dedup is best-effort.
			loggerFromCtx(ctx, d.logger).Warn("dispatcher: dedup precheck failed; falling through to async embed",
				zap.String("episodeUID", ev.UID),
				zap.Error(err),
			)
		} else if dup != "" {
			d.stats.IncDedup("semantic")
			loggerFromCtx(ctx, d.logger).Debug("dispatcher: semantic dup skipped",
				zap.String("candidate_uid", ev.UID),
				zap.String("existing_uid", dup),
			)
			return nil
		} else if vec != nil {
			return d.applyEpisodeWithKnownVector(ctx, ev, episode, vec)
		}
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if err := writeEpisodeRow(ctx, tx, ev, episode); err != nil {
		return fmt.Errorf("write episode row: %w", err)
	}

	if err := writeProvenanceRow(ctx, tx, ev); err != nil {
		return fmt.Errorf("write provenance row: %w", err)
	}

	if err := mergeEpisodeNode(ctx, d.graph, ev, episode); err != nil {
		return fmt.Errorf("merge graph node: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	if d.embed != nil {
		d.scheduleEpisodeEmbedding(ctx, ev.UID, episode.Text)
	}
	return nil
}

// checkSemanticDuplicate embeds the candidate text and searches for an
// existing Episode vector at or above d.dedupSemanticThreshold cosine
// similarity. Returns the matching UID and a nil vector if a duplicate
// was found, an empty UID + the just-computed vector if no duplicate,
// or an error if embed/search failed (caller falls through to the
// async path on error). UIDs are compared so a re-Apply of the same
// candidate (e.g. after an Apply retry) isn't treated as a duplicate
// of itself.
func (d *Dispatcher) checkSemanticDuplicate(ctx context.Context, candidateUID, text string) (string, []float32, error) {
	vec, err := d.embed.Embed(ctx, text)
	if err != nil {
		return "", nil, fmt.Errorf("embed: %w", err)
	}
	if got := len(vec); got != storage.VectorDim {
		return "", nil, fmt.Errorf("embed: wrong dim got=%d want=%d", got, storage.VectorDim)
	}
	hits, err := d.vector.SearchByDomain(ctx, "episodes", vec, 5)
	if err != nil {
		return "", nil, fmt.Errorf("vector search: %w", err)
	}
	for _, h := range hits {
		if h.ID == candidateUID {
			continue
		}
		sim := kernel.CosineSimilarityFromDistance(h.Score)
		if float64(sim) >= d.dedupSemanticThreshold {
			return h.ID, nil, nil
		}
	}
	return "", vec, nil
}

// applyEpisodeWithKnownVector persists the episode + provenance + graph
// node inline AND writes the vec0 row in the same transaction, so a
// caller that pre-embedded (the sync dedup branch) does not pay for a
// second embedder round-trip. The vector write still goes through the
// same writeEpisodeVector helper to keep the vec0 schema invariant in
// one place.
func (d *Dispatcher) applyEpisodeWithKnownVector(ctx context.Context, ev kernel.Event, episode ontology.Episode, vec []float32) (retErr error) {
	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if err := writeEpisodeRow(ctx, tx, ev, episode); err != nil {
		return fmt.Errorf("write episode row: %w", err)
	}
	if err := writeProvenanceRow(ctx, tx, ev); err != nil {
		return fmt.Errorf("write provenance row: %w", err)
	}
	if err := mergeEpisodeNode(ctx, d.graph, ev, episode); err != nil {
		return fmt.Errorf("merge graph node: %w", err)
	}
	if err := writeEpisodeVector(ctx, tx, ev.UID, vec); err != nil {
		return fmt.Errorf("write episode vector: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// scheduleEpisodeEmbedding spawns a detached goroutine that calls the
// embedder and upserts the vec0 row for uid. Increments d.bgVec on the
// caller's goroutine so a subsequent Close observes the new work even if
// the worker has not started yet. Errors are warn-logged: the episode row
// is durable, FTS recall still surfaces it, and a Rebuild can rebuild the
// vector index from the JSONL log.
func (d *Dispatcher) scheduleEpisodeEmbedding(ctx context.Context, uid, text string) {
	d.bgVec.Add(1)
	d.bgVecDepth.Add(1)
	logger := loggerFromCtx(ctx, d.logger)
	go func() {
		defer d.bgVec.Done()
		defer d.bgVecDepth.Add(-1)
		bgCtx := context.WithoutCancel(ctx)
		v, err := d.embed.Embed(bgCtx, text)
		if err != nil {
			logger.Warn("dispatcher: background embed failed (episode still durable)",
				zap.String("episodeUID", uid),
				zap.Error(err),
			)
			return
		}
		if got := len(v); got != storage.VectorDim {
			logger.Warn("dispatcher: background embed wrong dim",
				zap.String("episodeUID", uid),
				zap.Int("got", got),
				zap.Int("want", storage.VectorDim),
			)
			return
		}
		if err := d.writeEpisodeVectorAsync(bgCtx, uid, v); err != nil {
			logger.Warn("dispatcher: background vec upsert failed",
				zap.String("episodeUID", uid),
				zap.Error(err),
			)
		}
	}()
}

// writeEpisodeVectorAsync opens a fresh tx on the SQLite writer pool and
// upserts the vec0 row for uid. Used by the background embed worker; the
// synchronous applyEpisode no longer touches vec0 at all. A short-circuit
// against d.closed protects the worker from writing through a pool
// Close has torn down.
func (d *Dispatcher) writeEpisodeVectorAsync(ctx context.Context, uid string, embedding []float32) (retErr error) {
	d.mu.RLock()
	if d.closed {
		d.mu.RUnlock()
		return errors.New("dispatcher: closed")
	}
	writer := d.sqlite.Writer()
	d.mu.RUnlock()

	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()
	if err := writeEpisodeVector(ctx, tx, uid, embedding); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// BackgroundVecDepth returns the current count of in-flight background
// embedding goroutines (episode + entity name/summary + mention). Read
// from an atomic counter held in lockstep with bgVec.Add/Done. A
// return value of zero implies no embedding work is pending; combine
// with FileEventLog.BackgroundWorkDepth for a complete "is the kernel
// idle?" picture.
//
// For progress polling. Callers that need a stable "drained" signal
// should use WaitForBackgroundVec.
func (d *Dispatcher) BackgroundVecDepth() int64 {
	return d.bgVecDepth.Load()
}

// WaitForBackgroundVec blocks until every background episode-embedding
// goroutine spawned by Apply has returned, or ctx fires. Cheap when no
// work is in flight (the WaitGroup counter is zero). Tests use this as a
// deterministic alternative to sleeping; the eval feeder calls it before
// each recall so the vector lane observes a quiescent state.
func (d *Dispatcher) WaitForBackgroundVec(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		d.bgVec.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("dispatcher: wait for background vec: %w", ctx.Err())
	}
}

// scopeValue returns the scope to stamp on a derived row, defaulting an empty
// Event.Scope to kernel.DefaultScope (personal). This mirrors
// FileEventLog.prepare(): events appended before scope tagging, or by callers
// that left Scope empty, materialise as personal rather than as an out-of-scope
// blank — the fail-safe direction for the #206 read boundary.
func scopeValue(ev kernel.Event) string {
	if ev.Scope == "" {
		return string(kernel.DefaultScope)
	}
	return string(ev.Scope)
}

// writeEpisodeRow upserts the episodes row inside tx. The ON CONFLICT
// clause makes Rebuild idempotent: a replayed event overwrites the
// existing row rather than failing on the PK constraint. FTS5 maintenance
// is automatic via the AFTER INSERT trigger from migration 0002.
func writeEpisodeRow(ctx context.Context, tx *sql.Tx, ev kernel.Event, ep ontology.Episode) error {
	const q = `
		INSERT INTO episodes (uid, create_time, kind, payload_json, text, source, class, scope)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uid) DO UPDATE SET
			create_time  = excluded.create_time,
			kind         = excluded.kind,
			payload_json = excluded.payload_json,
			text         = excluded.text,
			source       = excluded.source,
			class        = excluded.class,
			scope        = excluded.scope
	`
	// Default to ClassEpisodic when the writer omitted the class. The
	// CHECK constraint on episodes.class rejects empty / unknown
	// values, so feeding an empty string would fail the insert; the
	// JSON unmarshal default does the same defaulting on the read
	// path.
	class := ep.Class
	if class == "" {
		class = ontology.ClassEpisodic
	}
	if _, err := tx.ExecContext(ctx, q,
		ev.UID,
		ev.CreateTime.UTC().Format(time.RFC3339Nano),
		string(ev.Kind),
		string(ev.Payload),
		ep.Text,
		ep.Source,
		string(class),
		scopeValue(ev),
	); err != nil {
		return fmt.Errorf("exec insert: %w", err)
	}
	return nil
}

// writeProvenanceRow upserts one provenance row for ev inside tx. The
// dispatcher writes provenance only for Episode events; derived records
// (mentions, relations) inherit provenance through their source_event_uid
// foreign key back to the source Episode. The ON CONFLICT(event_uid) DO
// UPDATE clause makes Rebuild idempotent: replaying every event in the
// JSONL log overwrites the existing row rather than failing on the
// UNIQUE constraint added by migration 0009.
//
// The FK constraint on provenance.event_uid references episodes(uid), so
// this helper MUST be called after writeEpisodeRow within the same tx;
// otherwise SQLite raises a foreign-key violation.
func writeProvenanceRow(ctx context.Context, tx *sql.Tx, ev kernel.Event) error {
	const q = `
		INSERT INTO provenance (event_uid, source, confidence, decided_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(event_uid) DO UPDATE SET
			source     = excluded.source,
			confidence = excluded.confidence,
			decided_at = excluded.decided_at
	`
	if _, err := tx.ExecContext(ctx, q,
		ev.UID,
		ev.Source,
		ev.Confidence,
		ev.CreateTime.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("exec insert provenance: %w", err)
	}
	return nil
}

// writeEpisodeVector upserts the embedding for uid into episodes_vec via
// the sidecar mapping. Mirrors the upsertOne logic in
// internal/kernel/storage/sqlite/vector.go but binds to the caller's tx
// so the whole event lands in one logical write.
//
// Vec0 has no INSERT OR REPLACE: replace = delete-then-insert. The
// sidecar row stays so the rowid is stable across re-Apply; only the
// vec0 slot is rewritten.
//
// The sidecar scope is copied from the base row rather than passed as a
// parameter: the base row is always written before the vector (inline apply)
// or long before it (deferred background embed, which only carries the uid),
// so the subquery is correct on both paths and rebuild re-materialises
// accurate sidecar scopes without threading scope through the embed queue.
func writeEpisodeVector(ctx context.Context, tx *sql.Tx, uid string, embedding []float32) error {
	blob, err := sqlite_vec.SerializeFloat32(embedding)
	if err != nil {
		return fmt.Errorf("serialize embedding: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO episodes_vec_ids(id, scope)
		 VALUES (?, COALESCE((SELECT scope FROM episodes WHERE uid = ?), 'personal'))
		 ON CONFLICT(id) DO UPDATE SET scope = excluded.scope`, uid, uid,
	); err != nil {
		return fmt.Errorf("upsert sidecar: %w", err)
	}
	var rowid int64
	if err := tx.QueryRowContext(ctx,
		`SELECT rowid FROM episodes_vec_ids WHERE id = ?`, uid,
	).Scan(&rowid); err != nil {
		return fmt.Errorf("lookup sidecar rowid: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM episodes_vec WHERE rowid = ?`, rowid,
	); err != nil {
		return fmt.Errorf("clear vec slot: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO episodes_vec(rowid, embedding) VALUES (?, ?)`, rowid, blob,
	); err != nil {
		return fmt.Errorf("write vec slot: %w", err)
	}
	return nil
}

// mergeEpisodeNode upserts an (:Episode) node carrying uid, createTime,
// kind, source, and confidence. The TIMESTAMP value is passed via the
// timestamp($createTime) cypher function so a Go time.Time does not need
// to round-trip through the lbug parameter binding (the existing test at
// ladybug_test.go:172 uses the same convention).
func mergeEpisodeNode(ctx context.Context, graph *ladybug.Conn, ev kernel.Event, ep ontology.Episode) error {
	const cypher = `
		MERGE (e:Episode {uid: $uid})
		ON CREATE SET
			e.createTime = timestamp($createTime),
			e.kind       = $kind,
			e.source     = $source,
			e.confidence = $confidence,
			e.scope      = $scope
		ON MATCH SET
			e.createTime = timestamp($createTime),
			e.kind       = $kind,
			e.source     = $source,
			e.confidence = $confidence,
			e.scope      = $scope
	`
	args := map[string]any{
		"uid":        ev.UID,
		"createTime": ev.CreateTime.UTC().Format(time.RFC3339Nano),
		"kind":       string(ev.Kind),
		"source":     ep.Source,
		"confidence": ep.Confidence,
		"scope":      scopeValue(ev),
	}
	r, err := graph.Execute(ctx, cypher, args)
	if err != nil {
		return err
	}
	if cerr := r.Close(); cerr != nil {
		return fmt.Errorf("close result: %w", cerr)
	}
	return nil
}

// vectorTypeName and vectorTypeSummary are the discriminator values written
// into entities_vec_ids.vector_type. The CHECK constraint in migration 0005
// enforces that no other values can appear in the table.
const (
	vectorTypeName    = "name"
	vectorTypeSummary = "summary"
)

// applyEntity runs the synchronous portion of the Entity write: the
// entities row, the per-mention entity_uid stamp, and the LadybugDB
// node. The two vector embeddings (name always, summary when present)
// run on a detached goroutine tracked by d.bgVec; both vector inserts
// commit in their own tx after the embeds return. FTS5 is not written
// for entities in M1; the FTS index covers episodes only.
//
// The synchronous tx no longer touches entities_vec. Recall via the
// name-vector lane sees the entity as soon as the background worker
// finishes; WaitForBackgroundVec is the deterministic drain primitive
// for tests and the eval feeder.
//
// Coordinate provenance is respected on upsert: existing coordinates
// are preserved if their provenance outranks the incoming value. Rank
// order is user_provided > calendar:google > geocoded:nominatim
// (hardcoded in SQL CASE expressions; no DB lookup required).
func (d *Dispatcher) applyEntity(ctx context.Context, ev kernel.Event) (retErr error) {
	var entity ontology.Entity
	if err := json.Unmarshal(ev.Payload, &entity); err != nil {
		return fmt.Errorf("decode entity payload: %w", err)
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	// Resolve the entity UID before writing so any late-arriving entity event
	// for a merged-away source updates the surviving target's row instead of
	// recreating a ghost row for the deleted source.
	rawUID := entity.UID
	if rawUID == "" {
		rawUID = ev.UID
	}
	resolvedUID, err := resolveRedirect(ctx, tx, rawUID)
	if err != nil {
		return fmt.Errorf("resolve entity uid: %w", err)
	}
	entity.UID = resolvedUID

	if err := writeEntityRowAndLinks(ctx, tx, ev, entity); err != nil {
		return err
	}

	if err := mergeEntityNode(ctx, d.graph, ev, entity); err != nil {
		return fmt.Errorf("merge entity graph node: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	if d.embed != nil {
		d.scheduleEntityEmbeddings(ctx, resolvedUID, entity.DisplayName, entity.Summary)
	}
	return nil
}

// scheduleEntityEmbeddings spawns a detached goroutine that computes
// the name and (optional) summary embeddings and upserts them into
// entities_vec. Mirrors scheduleEpisodeEmbedding /
// scheduleMentionEmbedding: increments d.bgVec on the caller's
// goroutine so a subsequent Close observes the new work even if the
// worker has not started yet. Errors are warn-logged; the entity row
// is durable and a Rebuild regenerates the vectors from the JSONL log.
//
// Summary may be empty: only the name vector is written in that case.
func (d *Dispatcher) scheduleEntityEmbeddings(ctx context.Context, uid, name, summary string) {
	d.bgVec.Add(1)
	d.bgVecDepth.Add(1)
	logger := loggerFromCtx(ctx, d.logger)
	go func() {
		defer d.bgVec.Done()
		defer d.bgVecDepth.Add(-1)
		bgCtx := context.WithoutCancel(ctx)
		nameVec, err := d.embed.Embed(bgCtx, name)
		if err != nil {
			logger.Warn("dispatcher: background entity name embed failed (entity still durable)",
				zap.String("entityUID", uid),
				zap.Error(err),
			)
			return
		}
		if got := len(nameVec); got != storage.VectorDim {
			logger.Warn("dispatcher: background entity name embed wrong dim",
				zap.String("entityUID", uid),
				zap.Int("got", got),
				zap.Int("want", storage.VectorDim),
			)
			return
		}
		var summaryVec []float32
		if summary != "" {
			v, serr := d.embed.Embed(bgCtx, summary)
			if serr != nil {
				logger.Warn("dispatcher: background entity summary embed failed (name vector still written)",
					zap.String("entityUID", uid),
					zap.Error(serr),
				)
			} else if got := len(v); got != storage.VectorDim {
				logger.Warn("dispatcher: background entity summary embed wrong dim",
					zap.String("entityUID", uid),
					zap.Int("got", got),
					zap.Int("want", storage.VectorDim),
				)
			} else {
				summaryVec = v
			}
		}
		if err := d.writeEntityVectorsAsync(bgCtx, uid, nameVec, summaryVec); err != nil {
			logger.Warn("dispatcher: background entity vec upsert failed",
				zap.String("entityUID", uid),
				zap.Error(err),
			)
		}
	}()
}

// writeEntityVectorsAsync opens a fresh tx on the SQLite writer pool
// and upserts the entities_vec rows for uid. Always writes the name
// vector; writes the summary vector when non-nil. Short-circuits
// against d.closed so the worker never writes through a torn-down pool.
func (d *Dispatcher) writeEntityVectorsAsync(ctx context.Context, uid string, name, summary []float32) (retErr error) {
	d.mu.RLock()
	if d.closed {
		d.mu.RUnlock()
		return errors.New("dispatcher: closed")
	}
	writer := d.sqlite.Writer()
	d.mu.RUnlock()

	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()
	if err := writeEntityVector(ctx, tx, uid, vectorTypeName, name); err != nil {
		return fmt.Errorf("write entity name vector: %w", err)
	}
	if summary != nil {
		if err := writeEntityVector(ctx, tx, uid, vectorTypeSummary, summary); err != nil {
			return fmt.Errorf("write entity summary vector: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// writeEntitySQL is the idempotent upsert for the entities table. Coordinate
// columns are updated only when the incoming provenance rank is >= the stored
// rank (user_provided=3 > calendar:google=2 > geocoded:nominatim=1 > unknown=0),
// preserving higher-trust coordinates across repeated EntityUpdated events.
// Properties update through COALESCE so a later entity event that carries no
// properties (the common resolver-update shape) never wipes typed attributes
// a createEntity write materialized earlier; an event that does carry
// properties replaces them wholesale. All other columns are unconditionally
// overwritten. The CASE expressions are embedded as string literals so no
// fmt.Sprintf is needed and gosec G201 does not fire on this query.
const writeEntitySQL = `
	INSERT INTO entities
		(uid, kind, display_name, confidence, resolution_status, create_time,
		 latitude, longitude, coordinates_provenance, properties, scope)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(uid) DO UPDATE SET
		kind                   = excluded.kind,
		display_name           = excluded.display_name,
		confidence             = excluded.confidence,
		resolution_status      = excluded.resolution_status,
		create_time            = excluded.create_time,
		scope                  = excluded.scope,
		properties             = COALESCE(excluded.properties, entities.properties),
		latitude               = CASE
			WHEN (CASE excluded.coordinates_provenance
			          WHEN 'user_provided'      THEN 3
			          WHEN 'calendar:google'    THEN 2
			          WHEN 'geocoded:nominatim' THEN 1
			          ELSE 0 END)
			  >= (CASE entities.coordinates_provenance
			          WHEN 'user_provided'      THEN 3
			          WHEN 'calendar:google'    THEN 2
			          WHEN 'geocoded:nominatim' THEN 1
			          ELSE 0 END)
			THEN excluded.latitude
			ELSE entities.latitude
		END,
		longitude              = CASE
			WHEN (CASE excluded.coordinates_provenance
			          WHEN 'user_provided'      THEN 3
			          WHEN 'calendar:google'    THEN 2
			          WHEN 'geocoded:nominatim' THEN 1
			          ELSE 0 END)
			  >= (CASE entities.coordinates_provenance
			          WHEN 'user_provided'      THEN 3
			          WHEN 'calendar:google'    THEN 2
			          WHEN 'geocoded:nominatim' THEN 1
			          ELSE 0 END)
			THEN excluded.longitude
			ELSE entities.longitude
		END,
		coordinates_provenance = CASE
			WHEN (CASE excluded.coordinates_provenance
			          WHEN 'user_provided'      THEN 3
			          WHEN 'calendar:google'    THEN 2
			          WHEN 'geocoded:nominatim' THEN 1
			          ELSE 0 END)
			  >= (CASE entities.coordinates_provenance
			          WHEN 'user_provided'      THEN 3
			          WHEN 'calendar:google'    THEN 2
			          WHEN 'geocoded:nominatim' THEN 1
			          ELSE 0 END)
			THEN excluded.coordinates_provenance
			ELSE entities.coordinates_provenance
		END
`

// writeEntityRow upserts the entities row inside tx using writeEntitySQL.
//
// The row is keyed by entity.UID (the canonical entity identifier set by the
// resolver via newEntityULID), not by ev.UID (the event identifier). Each
// EntityUpdated event must merge into the existing entity row rather than
// creating a new row per event: ev.UID is fresh per Append, so keying by it
// would multiply rows on every update. Entity.UID is the stable identity that
// EntityUpdated re-targets and that vec.Search hits resolve back to.
func writeEntityRow(ctx context.Context, tx *sql.Tx, ev kernel.Event, entity ontology.Entity) error {
	uid := entity.UID
	if uid == "" {
		uid = ev.UID
	}
	var lat, lon, prov any
	if entity.Coordinates != nil {
		lat = entity.Coordinates.Latitude
		lon = entity.Coordinates.Longitude
		prov = entity.CoordinatesProvenance
	}
	// Compact the typed-properties object so the stored TEXT is byte-stable
	// regardless of source whitespace (same convention as entity_kinds.
	// attr_schema). NULL when the event carries no properties, which the
	// upsert's COALESCE turns into "keep whatever is already stored".
	var props any
	if len(entity.Properties) > 0 {
		var compact bytes.Buffer
		if err := json.Compact(&compact, entity.Properties); err != nil {
			return fmt.Errorf("compact entity properties: %w", err)
		}
		props = compact.String()
	}
	if _, err := tx.ExecContext(ctx, writeEntitySQL,
		uid,
		string(entity.Kind),
		entity.DisplayName,
		entity.Confidence,
		string(entity.ResolutionStatus),
		ev.CreateTime.UTC().Format(time.RFC3339Nano),
		lat,
		lon,
		prov,
		props,
		scopeValue(ev),
	); err != nil {
		return fmt.Errorf("exec insert: %w", err)
	}
	return nil
}

// writeEntityRowAndLinks runs the synchronous sub-writes of an entity's
// relational pass inside the caller's tx: the entities row, the
// per-mention entity_uid stamp, and the entity_aliases projection. The two
// vec rows (name + summary) used to live here too; they now materialise on a
// detached goroutine via scheduleEntityEmbeddings so the writer tx never
// blocks on an embed.
func writeEntityRowAndLinks(ctx context.Context, tx *sql.Tx, ev kernel.Event, entity ontology.Entity) error {
	if err := writeEntityRow(ctx, tx, ev, entity); err != nil {
		return fmt.Errorf("write entity row: %w", err)
	}
	entityUID := entity.UID
	if entityUID == "" {
		entityUID = ev.UID
	}
	if err := backfillMentionEntityUID(ctx, tx, entityUID, entity.MentionUIDs); err != nil {
		return fmt.Errorf("write mention entity_uid: %w", err)
	}
	if err := writeEntityAliases(ctx, tx, entityUID, entity.AlternateNames, scopeValue(ev), ev.CreateTime.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("write entity aliases: %w", err)
	}
	return nil
}

// writeEntityAliases rebuilds the entity_aliases rows for one entity inside tx.
// It first deletes all existing alias rows for entityUID, then inserts one row
// per alias in alternateNames. The delete-then-insert pattern ensures that
// aliases removed from an entity's AlternateNames slice are pruned on replay,
// and that replaying the same event twice yields exactly one row per
// (alias, entity_uid) pair (idempotent). An empty alternateNames slice
// removes all aliases for the entity without inserting any new rows.
func writeEntityAliases(ctx context.Context, tx *sql.Tx, entityUID string, alternateNames []string, scope, createTime string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity_aliases WHERE entity_uid = ?`, entityUID,
	); err != nil {
		return fmt.Errorf("delete old aliases: %w", err)
	}
	for _, alias := range alternateNames {
		if alias == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entity_aliases (alias, entity_uid, scope, create_time)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(alias, entity_uid) DO NOTHING`,
			alias, entityUID, scope, createTime,
		); err != nil {
			return fmt.Errorf("insert alias %q: %w", alias, err)
		}
	}
	return nil
}

// backfillMentionEntityUID stamps the resolved entity UID onto every mention
// row in mentionUIDs. The dispatcher applies events in event-log order, so
// the Mention row already exists when its EntityCreated/EntityUpdated fires.
// Writing the link here (in the same tx as the entity row) closes the gap
// that previously forced recall to JOIN mentions to entities on display_name,
// which collapsed distinct entities that share a surface form. Mentions not
// yet in the table (out-of-order Rebuild) are a silent no-op: the UPDATE
// matches zero rows and Rebuild re-applies the entity event later so the
// link converges.
func backfillMentionEntityUID(ctx context.Context, tx *sql.Tx, entityUID string, mentionUIDs []ontology.ULID) error {
	if entityUID == "" || len(mentionUIDs) == 0 {
		return nil
	}
	placeholders, args := mentionUIDPlaceholders(mentionUIDs)
	args = append([]any{entityUID}, args...)
	q := fmt.Sprintf( //nolint:gosec // G201/G202: only "?" placeholders splice in
		`UPDATE mentions SET entity_uid = ? WHERE uid IN (%s)`,
		placeholders,
	)
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("exec update: %w", err)
	}
	return nil
}

// mentionUIDPlaceholders builds a (?, ?, ...) String and the matching args
// slice for an IN clause over ULID values. Kept local to the dispatcher to
// avoid pulling in the kernel's generic builder for this one call site.
func mentionUIDPlaceholders(uids []ontology.ULID) (string, []any) {
	ph := make([]byte, 0, len(uids)*2)
	args := make([]any, 0, len(uids))
	for i, uid := range uids {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
		args = append(args, string(uid))
	}
	return string(ph), args
}

// writeEntityVector upserts one (uid, vectorType) embedding into entities_vec
// via the sidecar mapping. Mirrors writeEpisodeVector but uses the compound
// (id, vector_type) key on the entities_vec_ids sidecar.
func writeEntityVector(ctx context.Context, tx *sql.Tx, uid, vectorType string, embedding []float32) error {
	blob, err := sqlite_vec.SerializeFloat32(embedding)
	if err != nil {
		return fmt.Errorf("serialize embedding: %w", err)
	}
	// Sidecar scope copies from the base entity row; see writeEpisodeVector
	// for why the subquery beats a parameter here.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entities_vec_ids(id, vector_type, scope)
		 VALUES (?, ?, COALESCE((SELECT scope FROM entities WHERE uid = ?), 'personal'))
		 ON CONFLICT(id, vector_type) DO UPDATE SET scope = excluded.scope`, uid, vectorType, uid,
	); err != nil {
		return fmt.Errorf("upsert sidecar: %w", err)
	}
	var rowid int64
	if err := tx.QueryRowContext(ctx,
		`SELECT rowid FROM entities_vec_ids WHERE id = ? AND vector_type = ?`, uid, vectorType,
	).Scan(&rowid); err != nil {
		return fmt.Errorf("lookup sidecar rowid: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entities_vec WHERE rowid = ?`, rowid,
	); err != nil {
		return fmt.Errorf("clear vec slot: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entities_vec(rowid, embedding) VALUES (?, ?)`, rowid, blob,
	); err != nil {
		return fmt.Errorf("write vec slot: %w", err)
	}
	return nil
}

// mergeEntityNode upserts a (:Entity) node carrying uid, kind, displayName,
// confidence, resolutionStatus, and scope. Coordinate properties are omitted
// from the graph node in M1; the spatial brute-force path in
// internal/kernel/geo queries the relational store directly.
func mergeEntityNode(ctx context.Context, graph *ladybug.Conn, ev kernel.Event, entity ontology.Entity) error {
	const cypher = `
		MERGE (e:Entity {uid: $uid})
		ON CREATE SET
			e.createTime       = timestamp($createTime),
			e.kind             = $kind,
			e.displayName      = $displayName,
			e.confidence       = $confidence,
			e.resolutionStatus = $resolutionStatus,
			e.scope            = $scope
		ON MATCH SET
			e.createTime       = timestamp($createTime),
			e.kind             = $kind,
			e.displayName      = $displayName,
			e.confidence       = $confidence,
			e.resolutionStatus = $resolutionStatus,
			e.scope            = $scope
	`
	uid := entity.UID
	if uid == "" {
		uid = ev.UID
	}
	args := map[string]any{
		"uid":              uid,
		"createTime":       ev.CreateTime.UTC().Format(time.RFC3339Nano),
		"kind":             string(entity.Kind),
		"displayName":      entity.DisplayName,
		"confidence":       entity.Confidence,
		"resolutionStatus": string(entity.ResolutionStatus),
		"scope":            scopeValue(ev),
	}
	r, err := graph.Execute(ctx, cypher, args)
	if err != nil {
		return err
	}
	if cerr := r.Close(); cerr != nil {
		return fmt.Errorf("close result: %w", cerr)
	}
	return nil
}

// applyMention runs the synchronous portion of the Mention write: the
// mentions table row only. The embedder call and the mentions_vec upsert
// are spawned on a detached goroutine (tracked by d.bgVec) so a slow
// embed call never pins the writer. Callers that need deterministic
// post-Apply vector visibility (tests, eval feeders) call
// WaitForBackgroundVec — the same drain primitive that already covers
// async episode embeddings.
//
// Mentions land at fan-out roughly 5x episode rate (one per extracted
// span), so synchronous embeds here are the dominant write-path stall
// once the episode lane was made async.
func (d *Dispatcher) applyMention(ctx context.Context, ev kernel.Event) (retErr error) {
	var mention ontology.Mention
	if err := json.Unmarshal(ev.Payload, &mention); err != nil {
		return fmt.Errorf("decode mention payload: %w", err)
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if err := writeMentionRow(ctx, tx, ev, mention); err != nil {
		return fmt.Errorf("write mention row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	// A zero-length extraction_text means the extractor confabulated
	// the span (groundingScore==0.0); those events skip the vector write.
	if d.embed != nil && mention.ExtractionText != "" {
		d.scheduleMentionEmbedding(ctx, ev.UID, mention.ExtractionText)
	}
	return nil
}

// scheduleMentionEmbedding spawns a detached goroutine that calls the
// embedder and upserts the mentions_vec row for uid. Mirrors
// scheduleEpisodeEmbedding: increments d.bgVec on the caller's
// goroutine so a subsequent Close observes the new work even if the
// worker has not started yet. Errors are warn-logged: the mention row
// is durable and a Rebuild can regenerate the vector from the JSONL.
func (d *Dispatcher) scheduleMentionEmbedding(ctx context.Context, uid, text string) {
	d.bgVec.Add(1)
	d.bgVecDepth.Add(1)
	logger := loggerFromCtx(ctx, d.logger)
	go func() {
		defer d.bgVec.Done()
		defer d.bgVecDepth.Add(-1)
		bgCtx := context.WithoutCancel(ctx)
		v, err := d.embed.Embed(bgCtx, text)
		if err != nil {
			logger.Warn("dispatcher: background mention embed failed (mention still durable)",
				zap.String("mentionUID", uid),
				zap.Error(err),
			)
			return
		}
		if got := len(v); got != storage.VectorDim {
			logger.Warn("dispatcher: background mention embed wrong dim",
				zap.String("mentionUID", uid),
				zap.Int("got", got),
				zap.Int("want", storage.VectorDim),
			)
			return
		}
		if err := d.writeMentionVectorAsync(bgCtx, uid, v); err != nil {
			logger.Warn("dispatcher: background mention vec upsert failed",
				zap.String("mentionUID", uid),
				zap.Error(err),
			)
		}
	}()
}

// writeMentionVectorAsync opens a fresh tx on the SQLite writer pool
// and upserts the mentions_vec row for uid. Mirrors
// writeEpisodeVectorAsync; short-circuits against d.closed so the
// worker never writes through a pool Close has torn down.
func (d *Dispatcher) writeMentionVectorAsync(ctx context.Context, uid string, embedding []float32) (retErr error) {
	d.mu.RLock()
	if d.closed {
		d.mu.RUnlock()
		return errors.New("dispatcher: closed")
	}
	writer := d.sqlite.Writer()
	d.mu.RUnlock()

	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()
	if err := writeMentionVector(ctx, tx, uid, embedding); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// writeMentionRow upserts the mentions row inside tx. ON CONFLICT overwrites
// all columns so a re-applied Mention event is idempotent under Rebuild.
func writeMentionRow(ctx context.Context, tx *sql.Tx, ev kernel.Event, mention ontology.Mention) error {
	const q = `
		INSERT INTO mentions
			(uid, kind, display_name, source_event_uid, create_time, extraction_text, scope)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uid) DO UPDATE SET
			kind             = excluded.kind,
			display_name     = excluded.display_name,
			source_event_uid = excluded.source_event_uid,
			create_time      = excluded.create_time,
			extraction_text  = excluded.extraction_text,
			scope            = excluded.scope
	`
	if _, err := tx.ExecContext(ctx, q,
		ev.UID,
		string(mention.Kind),
		mention.DisplayName,
		string(mention.SourceEventUID),
		ev.CreateTime.UTC().Format(time.RFC3339Nano),
		mention.ExtractionText,
		scopeValue(ev),
	); err != nil {
		return fmt.Errorf("exec insert: %w", err)
	}
	return nil
}

// writeMentionVector upserts the embedding for uid into mentions_vec via the
// sidecar mapping. Mirrors writeEpisodeVector; uses mentions_vec_ids which has
// a simple UNIQUE(id) key (one vector per mention, no discriminator needed).
func writeMentionVector(ctx context.Context, tx *sql.Tx, uid string, embedding []float32) error {
	blob, err := sqlite_vec.SerializeFloat32(embedding)
	if err != nil {
		return fmt.Errorf("serialize embedding: %w", err)
	}
	// Sidecar scope copies from the base mention row; see writeEpisodeVector
	// for why the subquery beats a parameter here.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO mentions_vec_ids(id, scope)
		 VALUES (?, COALESCE((SELECT scope FROM mentions WHERE uid = ?), 'personal'))
		 ON CONFLICT(id) DO UPDATE SET scope = excluded.scope`, uid, uid,
	); err != nil {
		return fmt.Errorf("upsert sidecar: %w", err)
	}
	var rowid int64
	if err := tx.QueryRowContext(ctx,
		`SELECT rowid FROM mentions_vec_ids WHERE id = ?`, uid,
	).Scan(&rowid); err != nil {
		return fmt.Errorf("lookup sidecar rowid: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM mentions_vec WHERE rowid = ?`, rowid,
	); err != nil {
		return fmt.Errorf("clear vec slot: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO mentions_vec(rowid, embedding) VALUES (?, ?)`, rowid, blob,
	); err != nil {
		return fmt.Errorf("write vec slot: %w", err)
	}
	return nil
}

// applyRelation runs the two-layer write for one Relation event: relational
// (relations table) and graph (LadybugDB RELATES_TO edge). No vector or FTS
// write is needed: relation surface forms are recall-derived from the cited
// entities and episodes.
func (d *Dispatcher) applyRelation(ctx context.Context, ev kernel.Event) (retErr error) {
	var rel ontology.Relation
	if err := json.Unmarshal(ev.Payload, &rel); err != nil {
		return fmt.Errorf("decode relation payload: %w", err)
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	// Route any late-arriving relation whose subject or object was merged away
	// to the surviving entity so FKs always land on canonical UIDs.
	resolvedSubj, err := resolveRedirect(ctx, tx, rel.SubjectUID)
	if err != nil {
		return fmt.Errorf("resolve relation subject: %w", err)
	}
	rel.SubjectUID = resolvedSubj
	resolvedObj, err := resolveRedirect(ctx, tx, rel.ObjectUID)
	if err != nil {
		return fmt.Errorf("resolve relation object: %w", err)
	}
	rel.ObjectUID = resolvedObj

	if err := writeRelationRow(ctx, tx, ev, rel); err != nil {
		return fmt.Errorf("write relation row: %w", err)
	}
	if err := mergeRelationEdge(ctx, d.graph, ev, rel); err != nil {
		return fmt.Errorf("merge relation edge: %w", err)
	}

	// Re-fold any retraction tombstones that already landed for this relation
	// (a relationRetracted may replay before its relation event, or the
	// relation event may re-apply during a second Rebuild pass). The fold is
	// the sole authority for valid_until / superseded_by_uid, so the close
	// converges regardless of replay order; a no-op when there are no
	// tombstones leaves writeRelationRow's open/bounded state intact.
	relUID := rel.UID
	if relUID == "" {
		relUID = ev.UID
	}
	if err := d.recomputeRelationClose(ctx, tx, relUID); err != nil {
		return fmt.Errorf("recompute relation close: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// writeRelationRow upserts the relations row inside tx. ON CONFLICT keeps the
// row idempotent under Rebuild.
//
// Superseded_by_uid is deliberately NOT in the ON CONFLICT update set: the
// relationRetracted tombstone fold (recomputeRelationClose) is the sole
// authority for it, so a relation event re-applying during Rebuild can never
// clobber a converged close back to the payload's always-NULL superseder.
// Valid_until stays in the update set as the extractor-set validity bound, and
// recomputeRelationClose folds that bound against the tombstone set after the
// upsert.
func writeRelationRow(ctx context.Context, tx *sql.Tx, ev kernel.Event, rel ontology.Relation) error {
	const q = `
		INSERT INTO relations
			(uid, subject_uid, predicate, object_uid, confidence, source_event_uid,
			 create_time, valid_from, valid_until, validity_score, superseded_by_uid,
			 provenance, attributes, scope, asserted_by_principal, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uid) DO UPDATE SET
			subject_uid           = excluded.subject_uid,
			predicate             = excluded.predicate,
			object_uid            = excluded.object_uid,
			confidence            = excluded.confidence,
			source_event_uid      = excluded.source_event_uid,
			create_time           = excluded.create_time,
			valid_from            = excluded.valid_from,
			valid_until           = excluded.valid_until,
			validity_score        = excluded.validity_score,
			provenance            = excluded.provenance,
			attributes            = excluded.attributes,
			scope                 = excluded.scope,
			asserted_by_principal = excluded.asserted_by_principal,
			status                = excluded.status
	`
	var validFrom, validUntil any
	if rel.ValidFrom != nil {
		validFrom = rel.ValidFrom.UTC().Format(time.RFC3339Nano)
	}
	if rel.ValidUntil != nil {
		validUntil = rel.ValidUntil.UTC().Format(time.RFC3339Nano)
	}
	var validity any
	if rel.ValidityScore > 0 {
		validity = rel.ValidityScore
	}
	var supersededBy any
	if rel.SupersededByUID != nil {
		supersededBy = string(*rel.SupersededByUID)
	}
	// Stamp the SIGNED asserter (#209) so the team-scope retraction authority
	// can compare a tombstone's signer against the original asserter. A
	// legacy/empty author binds NULL: such a relation is closable in team
	// scope only by the genesis owner (see recomputeRelationClose).
	var assertedBy any
	if ev.Author.PrincipalUID != "" {
		assertedBy = ev.Author.PrincipalUID
	}
	uid := rel.UID
	if uid == "" {
		uid = ev.UID
	}
	// Trust posture (#211): default an empty/legacy status to asserted so the
	// NOT NULL column always has a valid value and pre-status relation events
	// rebuild as authoritative. Only a connector-ingested relation carries
	// "proposed" (set by the resolver's untrusted gate).
	status := rel.Status
	if status == "" {
		status = ontology.RelationStatusAsserted
	}
	if _, err := tx.ExecContext(ctx, q,
		uid,
		rel.SubjectUID,
		rel.Predicate,
		rel.ObjectUID,
		rel.Confidence,
		rel.SourceEventUID,
		ev.CreateTime.UTC().Format(time.RFC3339Nano),
		validFrom,
		validUntil,
		validity,
		supersededBy,
		rel.Provenance,
		marshalAttributes(rel.Attributes),
		scopeValue(ev),
		assertedBy,
		status,
	); err != nil {
		return fmt.Errorf("exec insert: %w", err)
	}
	if err := ensureRelationPredicate(ctx, tx, rel.Predicate, ev.CreateTime); err != nil {
		return fmt.Errorf("ensure relation predicate: %w", err)
	}
	return nil
}

// ensureRelationPredicate auto-registers the relation's predicate into the
// predicates vocabulary inside the same apply transaction, so every write path
// (CLI, MCP, synced/grafted relations) converges on the same invariant: a
// predicate that names an asserted edge always appears in `schema predicates`
// / introspect, even though the relations table holds no predicate foreign key.
//
// The upsert is ON CONFLICT(predicate) DO NOTHING: an existing, richly-defined
// predicate row (its kind, description, and P5 shape) is never clobbered by a
// later relation that merely re-uses it. Columns/types match
// applyPredicateRegistered exactly. The kind is the stored non-functional form
// (resolver.NonFunctional, "non-functional"): an auto-asserted edge has no
// declared cardinality, and non-functional is the permissive default that lets
// multiple live values coexist until a richer registration narrows it.
//
// Added_by is "user": the CHECK constraint on predicates.added_by admits only
// {seed, llm, user, extension} (migration 0022), and this mirrors the CLI's
// prior ensurePredicateRegistered provenance, which the literal "projection"
// would violate — see the report's parity note.
func ensureRelationPredicate(ctx context.Context, tx *sql.Tx, predicate string, occurredAt time.Time) error {
	const q = `INSERT INTO predicates
	     (predicate, kind, description, added_at, added_by,
	      subject_kinds, object_kinds, same_as, property_schema)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	 ON CONFLICT(predicate) DO NOTHING`
	if _, err := tx.ExecContext(ctx, q,
		predicate,
		string(resolver.NonFunctional),
		"Auto-registered from an asserted relation.",
		occurredAt.UTC().Format(time.RFC3339Nano),
		"user",
		nil, nil, nil, nil, // subject_kinds, object_kinds, same_as, property_schema
	); err != nil {
		return fmt.Errorf("upsert predicate: %w", err)
	}
	return nil
}

// marshalAttributes serialises a relation's attributes map to a JSON
// object string. Returns "{}" for nil/empty maps so the column shape
// stays consistent (the migration's NOT NULL DEFAULT '{}' covers
// pre-migration rows; this keeps post-migration writes uniform too).
// Failure to marshal an in-tree map[string]string is impossible in
// practice; the error path returns "{}" so a write never blocks on
// it.
func marshalAttributes(attrs map[string]string) string {
	if len(attrs) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(attrs)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// mergeRelationEdge writes the RELATES_TO Cypher edge with bi-temporal validity
// properties and scope. The graph query lane in HybridQuery reads validFrom /
// validUntil for the temporal recall filter; scope is defense-in-depth for
// cross-scope traversal isolation.
func mergeRelationEdge(ctx context.Context, graph *ladybug.Conn, ev kernel.Event, rel ontology.Relation) error {
	// LadybugDB's binder rejects implicit STRING -> TIMESTAMP casts on REL
	// properties, so validFrom / validUntil must enter the query as
	// proper TIMESTAMP values via timestamp($x), with $x carrying either
	// an RFC 3339 string or NULL (the latter when the relation has no
	// bound on that side).
	const cypher = `
		MATCH (s:Entity {uid: $subjectUid})
		MATCH (o:Entity {uid: $objectUid})
		MERGE (s)-[r:RELATES_TO {uid: $uid}]->(o)
		ON CREATE SET
			r.predicate     = $predicate,
			r.confidence    = $confidence,
			r.createTime    = timestamp($createTime),
			r.validFrom     = CASE WHEN $validFrom IS NULL THEN NULL ELSE timestamp($validFrom) END,
			r.validUntil    = CASE WHEN $validUntil IS NULL THEN NULL ELSE timestamp($validUntil) END,
			r.validityScore = $validityScore,
			r.attributes    = $attributes,
			r.scope         = $scope
		ON MATCH SET
			r.predicate     = $predicate,
			r.confidence    = $confidence,
			r.createTime    = timestamp($createTime),
			r.validFrom     = CASE WHEN $validFrom IS NULL THEN NULL ELSE timestamp($validFrom) END,
			r.validUntil    = CASE WHEN $validUntil IS NULL THEN NULL ELSE timestamp($validUntil) END,
			r.validityScore = $validityScore,
			r.attributes    = $attributes,
			r.scope         = $scope
	`
	uid := rel.UID
	if uid == "" {
		uid = ev.UID
	}
	args := map[string]any{
		"uid":           uid,
		"subjectUid":    rel.SubjectUID,
		"objectUid":     rel.ObjectUID,
		"predicate":     rel.Predicate,
		"confidence":    rel.Confidence,
		"createTime":    ev.CreateTime.UTC().Format(time.RFC3339Nano),
		"validFrom":     nullableTimeParam(rel.ValidFrom),
		"validUntil":    nullableTimeParam(rel.ValidUntil),
		"validityScore": rel.ValidityScore,
		"attributes":    marshalAttributes(rel.Attributes),
		"scope":         scopeValue(ev),
	}
	r, err := graph.Execute(ctx, cypher, args)
	if err != nil {
		return err
	}
	if cerr := r.Close(); cerr != nil {
		return fmt.Errorf("close result: %w", cerr)
	}
	return nil
}

// nullableTimeParam returns nil when t is nil and the RFC 3339 string of
// t.UTC() otherwise. Used as a query-parameter value paired with a
// CASE WHEN $x IS NULL ... THEN timestamp($x) END Cypher pattern, so the
// LadybugDB binder receives a real NULL rather than an empty string
// pretending to be a TIMESTAMP.
func nullableTimeParam(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// applyRelationRetracted records one relationRetracted tombstone in the
// monotonic retraction set and re-folds the target relation's materialized
// close columns. INSERT OR IGNORE keyed by the retraction event UID makes the
// set-union idempotent; recomputeRelationClose then derives valid_until and
// superseded_by_uid deterministically from the FULL set, so two paired devices
// closing the same relation arm converge regardless of the order their events
// replay in. If the target relation row has not been applied yet (a retraction
// replayed before its relation event), the tombstone is still durably recorded
// here and the re-fold no-ops until applyRelation lands the row and folds it in.
func (d *Dispatcher) applyRelationRetracted(ctx context.Context, ev kernel.Event) (retErr error) {
	var ret ontology.RelationRetractedEvent
	if err := json.Unmarshal(ev.Payload, &ret); err != nil {
		return fmt.Errorf("decode relationRetracted payload: %w", err)
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	// Record the SIGNED signer (#209) alongside the tombstone so the team-scope
	// close authority is self-describing and auditable. The cryptographic
	// signature itself is verified at the team-scope replay gate; these columns
	// are the materialized projection of that authority that the fold consults.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO relation_retractions
			(retraction_event_uid, relation_uid, superseded_by_uid, retracted_at,
			 signer_principal, signer_device)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ev.UID,
		string(ret.RelationUID),
		string(ret.SupersededByUID),
		ret.RetractedAt.UTC().Format(time.RFC3339Nano),
		ev.Author.PrincipalUID,
		ev.Author.DeviceID,
	); err != nil {
		return fmt.Errorf("insert relation retraction: %w", err)
	}

	if err := d.recomputeRelationClose(ctx, tx, string(ret.RelationUID)); err != nil {
		return fmt.Errorf("recompute relation close: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// applyRelationPromoted lifts the trust quarantine on a connector-proposed
// relation by flipping relations.status from "proposed" to "asserted". The
// UPDATE is guarded on status='proposed', so the projection is idempotent: a
// replayed promotion, or a promotion targeting an already-asserted (or absent)
// relation, affects zero rows and is a harmless no-op. It also stamps the
// signed promoter as the relation's asserted_by_principal so a team-scope edge
// gains a concrete close authority (#209) and the promotion is auditable.
//
// Promotion is deliberately one-directional and additive: there is no
// "demote". An over-eager promotion is corrected by retracting the edge, not by
// reverting status, which keeps the relations.status lattice monotonic. A
// promotion event is only ever appended after its target relation exists (the
// tool resolves the uid from a read first), so log-order replay — the rebuild
// path — always lands the relation before the promotion and converges. A
// promotion that replays before its relation (a rare cross-device reorder)
// no-ops against the absent row and leaves the edge proposed; re-promoting
// reconverges it.
func (d *Dispatcher) applyRelationPromoted(ctx context.Context, ev kernel.Event) (retErr error) {
	var promo ontology.RelationPromotedEvent
	if err := json.Unmarshal(ev.Payload, &promo); err != nil {
		return fmt.Errorf("decode relationPromoted payload: %w", err)
	}

	// Stamp the SIGNED promoter (#209) as the relation's asserter only when the
	// event carries a principal; a legacy/empty author binds SQL NULL, so
	// COALESCE keeps any prior asserted_by_principal untouched.
	promoter := sql.NullString{String: ev.Author.PrincipalUID, Valid: ev.Author.PrincipalUID != ""}
	if _, err := d.sqlite.Writer().ExecContext(ctx,
		`UPDATE relations
		    SET status = ?,
		        asserted_by_principal = COALESCE(?, asserted_by_principal)
		  WHERE uid = ? AND status = ?`,
		ontology.RelationStatusAsserted,
		promoter,
		string(promo.RelationUID),
		ontology.RelationStatusProposed,
	); err != nil {
		return fmt.Errorf("promote relation: %w", err)
	}
	return nil
}

// signedTombstone is one relation_retractions row carrying both the
// convergent-fold payload (ontology.Tombstone) and the SIGNED signer principal
// (#209) that authored the relationRetracted. The authority filter
// (authorizedTombstones) reads signerPrincipal; ReduceRetractions reads only
// the embedded Tombstone.
type signedTombstone struct {
	ontology.Tombstone
	signerPrincipal string
}

// recomputeRelationClose re-derives the materialized close columns
// (valid_until, superseded_by_uid) for relationUID from its FULL retraction
// tombstone set and mirrors the recomputed valid_until onto the graph edge. It
// is the SOLE authority for those two columns once any tombstone exists, which
// is what makes the close order-independent: writeRelationRow's payload values
// (always-NULL superseder; the extractor-set validity bound for valid_until)
// are folded in via ontology.ReduceRetractions, never trusted as the final
// close. When the relation carries no tombstone this is a no-op so an extracted
// validity bound and the plain open state survive untouched. The fold is
// commutative and idempotent, so re-application during Rebuild is stable.
//
// Team-scope authority (#213, D3): a team-scope relation closes ONLY via a
// tombstone whose signed signer is the original asserter
// (relations.asserted_by_principal) or the team's genesis owner. Unauthorized
// team-scope tombstones stay recorded (audit) but are filtered OUT of the fold,
// so they never close the relation on any device. Non-team tombstones always
// pass (personal/project retraction is unchanged from #205). The filter runs
// inside this re-fold, which executes from BOTH applyRelation and
// applyRelationRetracted, so the close converges regardless of replay order: a
// tombstone landing before its relation cannot match an as-yet-unknown asserter
// (so it stays inert), and is re-evaluated and authorized once the relation —
// and thus asserted_by_principal — lands.
//
// The graph edge carries no supersededByUid property by design (only validUntil
// gates graph currency, which recall's temporal filter reads off the edge), so
// only validUntil is mirrored. Same STRING -> TIMESTAMP binder constraint as
// mergeRelationEdge: wrap in timestamp(), pass NULL through nullableTimeParam.
func (d *Dispatcher) recomputeRelationClose(ctx context.Context, tx *sql.Tx, relationUID string) error {
	signed, err := loadRelationTombstones(ctx, tx, relationUID)
	if err != nil {
		return err
	}
	if len(signed) == 0 {
		// No tombstone for this relation: leave writeRelationRow's values (the
		// open state, or an extractor-set validity bound) untouched.
		return nil
	}

	// Read the row's current valid_until (extracted-bound floor) and scope +
	// asserter (team-scope authority inputs). NULL valid_until = open. A missing
	// row means the retraction replayed before its relation event; the tombstone
	// stays recorded and applyRelation re-folds it once the row lands.
	var (
		curRaw     sql.NullString
		scope      string
		assertedBy sql.NullString
	)
	switch scanErr := tx.QueryRowContext(ctx,
		`SELECT valid_until, scope, asserted_by_principal FROM relations WHERE uid = ?`, relationUID,
	).Scan(&curRaw, &scope, &assertedBy); {
	case errors.Is(scanErr, sql.ErrNoRows):
		return nil
	case scanErr != nil:
		return fmt.Errorf("read relation close inputs: %w", scanErr)
	}
	var extracted *time.Time
	if curRaw.Valid {
		ts, perr := time.Parse(time.RFC3339Nano, curRaw.String)
		if perr != nil {
			return fmt.Errorf("parse current valid_until %q: %w", curRaw.String, perr)
		}
		extracted = &ts
	}

	// Keep only authorized tombstones before folding. For non-team scopes this
	// passes every tombstone through, so #205 behavior is exactly preserved.
	authorized, err := d.authorizedTombstones(ctx, scope, assertedBy.String, signed)
	if err != nil {
		return err
	}

	supersededBy, validUntil := ontology.ReduceRetractions(extracted, authorized)

	if _, err := tx.ExecContext(ctx,
		`UPDATE relations SET valid_until = ?, superseded_by_uid = ? WHERE uid = ?`,
		nullableTimeParam(validUntil), supersededBy, relationUID,
	); err != nil {
		return fmt.Errorf("update relation close: %w", err)
	}

	const cypher = `
		MATCH ()-[r:RELATES_TO {uid: $uid}]->()
		SET r.validUntil = CASE WHEN $validUntil IS NULL THEN NULL ELSE timestamp($validUntil) END
	`
	res, gerr := d.graph.Execute(ctx, cypher, map[string]any{
		"uid":        relationUID,
		"validUntil": nullableTimeParam(validUntil),
	})
	if gerr != nil {
		return fmt.Errorf("execute retract cypher: %w", gerr)
	}
	if cerr := res.Close(); cerr != nil {
		return fmt.Errorf("close retract result: %w", cerr)
	}
	return nil
}

// authorizedTombstones returns the subset of signed tombstones permitted to
// close a relation of the given scope, projected to the plain
// ontology.Tombstone the fold consumes.
//
// Non-team scopes: every tombstone is authorized (personal/project retraction
// is unchanged from #205), so the genesis-owner lookup is skipped entirely.
//
// Team scope (#213, D3): a tombstone is authorized when its signed signer is
// the original asserter (assertedBy, from relations.asserted_by_principal) OR
// the team's genesis owner. An empty assertedBy (legacy/empty-author relation)
// matches no signer, so such a relation is closable only by the genesis owner.
// An empty signer (legacy/empty-author tombstone) never matches a non-empty
// asserter or genesis owner, so it cannot close a team relation.
func (d *Dispatcher) authorizedTombstones(ctx context.Context, scope, assertedBy string, signed []signedTombstone) ([]ontology.Tombstone, error) {
	if scope != string(kernel.ScopeTeam) {
		out := make([]ontology.Tombstone, len(signed))
		for i := range signed {
			out[i] = signed[i].Tombstone
		}
		return out, nil
	}
	genesisOwner, err := d.GenesisOwner(ctx)
	if err != nil {
		return nil, fmt.Errorf("authorized tombstones: %w", err)
	}
	var out []ontology.Tombstone
	for i := range signed {
		signer := signed[i].signerPrincipal
		if signer == "" {
			continue
		}
		if signer == assertedBy || (genesisOwner != "" && signer == genesisOwner) {
			out = append(out, signed[i].Tombstone)
		}
	}
	return out, nil
}

// loadRelationTombstones reads the full retraction tombstone set for one
// relation inside tx, including the signed signer principal each carries. Order
// is irrelevant: the authority filter is per-row and ReduceRetractions is a
// commutative fold over the survivors.
func loadRelationTombstones(ctx context.Context, tx *sql.Tx, relationUID string) ([]signedTombstone, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT superseded_by_uid, retracted_at, signer_principal
		 FROM relation_retractions WHERE relation_uid = ?`,
		relationUID,
	)
	if err != nil {
		return nil, fmt.Errorf("query retractions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tombstones []signedTombstone
	for rows.Next() {
		var superseder, retractedAt, signer string
		if err := rows.Scan(&superseder, &retractedAt, &signer); err != nil {
			return nil, fmt.Errorf("scan retraction: %w", err)
		}
		ts, perr := time.Parse(time.RFC3339Nano, retractedAt)
		if perr != nil {
			return nil, fmt.Errorf("parse retracted_at %q: %w", retractedAt, perr)
		}
		tombstones = append(tombstones, signedTombstone{
			Tombstone:       ontology.Tombstone{SupersededByUID: superseder, RetractedAt: ts},
			signerPrincipal: signer,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retractions: %w", err)
	}
	return tombstones, nil
}

// ontologyProvenanceCore is the namespace lane for built-in taxonomy rows.
// Core rows are write-protected: a non-core ontology event cannot overwrite
// them during replay.
const ontologyProvenanceCore = "core"

// applyPredicateRegistered inserts a row into the predicates table. The
// resolver's in-memory cache is refreshed lazily on next miss; we do not
// notify the registry here because the dispatcher and resolver may run in
// different processes during Rebuild.
//
// The P5 columns (subject_kinds, object_kinds, same_as, property_schema)
// are marshalled from the optional slice/raw fields and stored as JSON text
// or NULL. They do not exist before migration 0020; the UPSERT is additive
// (existing rows gain NULL values until a later predicateRegistered event
// carries the new fields).
func (d *Dispatcher) applyPredicateRegistered(ctx context.Context, ev kernel.Event) (retErr error) {
	var p ontology.PredicateRegisteredEvent
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("decode predicateRegistered payload: %w", err)
	}

	var subjectKindsJSON, objectKindsJSON, sameAsJSON []byte
	if len(p.SubjectKinds) > 0 {
		raw, err := json.Marshal(p.SubjectKinds)
		if err != nil {
			return fmt.Errorf("marshal subjectKinds: %w", err)
		}
		subjectKindsJSON = raw
	}
	if len(p.ObjectKinds) > 0 {
		raw, err := json.Marshal(p.ObjectKinds)
		if err != nil {
			return fmt.Errorf("marshal objectKinds: %w", err)
		}
		objectKindsJSON = raw
	}
	if len(p.SameAs) > 0 {
		raw, err := json.Marshal(p.SameAs)
		if err != nil {
			return fmt.Errorf("marshal sameAs: %w", err)
		}
		sameAsJSON = raw
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO predicates
		     (predicate, kind, description, added_at, added_by,
		      subject_kinds, object_kinds, same_as, property_schema)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(predicate) DO UPDATE SET
			kind            = excluded.kind,
			description     = excluded.description,
			added_at        = excluded.added_at,
			added_by        = excluded.added_by,
			subject_kinds   = COALESCE(excluded.subject_kinds, predicates.subject_kinds),
			object_kinds    = COALESCE(excluded.object_kinds, predicates.object_kinds),
			same_as         = COALESCE(excluded.same_as, predicates.same_as),
			property_schema = COALESCE(excluded.property_schema, predicates.property_schema)`,
		p.Predicate,
		string(p.Kind),
		p.Description,
		p.AddedAt.UTC().Format(time.RFC3339Nano),
		string(p.AddedBy),
		nullableBytes(subjectKindsJSON),
		nullableBytes(objectKindsJSON),
		nullableBytes(sameAsJSON),
		nullableBytes(p.PropertySchema),
	); err != nil {
		return fmt.Errorf("upsert predicate: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// sanitizeEntityKindShape validates the optional shape-bearing fields of an
// entityKindRegistered event before materialization. A propertySchema that
// fails to compile and an extends link that would create a cycle (or exceed
// the depth bound) are both stripped with a Warn log — the kind itself is
// still valid and is stored without the broken piece. This mirrors the
// compile-before-materialize precedent the kernel.Ontology uses.
func (d *Dispatcher) sanitizeEntityKindShape(ctx context.Context, ev kernel.Event, p *ontology.EntityKindRegisteredEvent) (attrSchemaJSON []byte, extendsToStore string) {
	if len(p.PropertySchema) > 0 {
		if err := compileJSONSchema(p.PropertySchema); err != nil {
			loggerFromCtx(ctx, d.logger).Warn("dispatcher: entityKindRegistered propertySchema compile failed; storing kind without attr schema",
				zap.String("event_uid", ev.UID),
				zap.String("kind_name", p.Name),
				zap.Error(err),
			)
		} else {
			attrSchemaJSON = p.PropertySchema
		}
	}

	extendsToStore = p.Extends
	if p.Extends != "" {
		if cycleErr := checkExtendsCycle(ctx, d.sqlite.Reader(), p.Name, p.Extends); cycleErr != nil {
			loggerFromCtx(ctx, d.logger).Warn("dispatcher: entityKindRegistered extends would create cycle or exceed depth; storing kind without extends",
				zap.String("event_uid", ev.UID),
				zap.String("kind_name", p.Name),
				zap.String("extends", p.Extends),
				zap.Error(cycleErr),
			)
			extendsToStore = ""
		}
	}
	return attrSchemaJSON, extendsToStore
}

// applyEntityKindRegistered upserts one row into entity_kinds. The handler
// is idempotent under replay: repeated appends of the same event converge to
// the same row state. Core-provenance rows are write-protected: a non-core
// event cannot overwrite a row whose existing provenance is "core" unless the
// incoming event itself carries provenance "core".
//
// Compile-before-materialize guard: when PropertySchema is non-empty the
// handler attempts to compile it with the same santhosh-tekuri path the
// kernel.Ontology uses. A schema that fails to compile is logged and the row
// is stored without the property schema — the kind itself is still valid.
func (d *Dispatcher) applyEntityKindRegistered(ctx context.Context, ev kernel.Event) (retErr error) {
	var p ontology.EntityKindRegisteredEvent
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("decode entityKindRegistered payload: %w", err)
	}

	// Compile-check the property schema and cycle-check the extends link;
	// broken pieces are stripped (Warn-logged) rather than failing the event.
	attrSchemaJSON, extendsToStore := d.sanitizeEntityKindShape(ctx, ev, &p)

	var sameAsJSON []byte
	if len(p.SameAs) > 0 {
		raw, err := json.Marshal(p.SameAs)
		if err != nil {
			return fmt.Errorf("marshal sameAs: %w", err)
		}
		sameAsJSON = raw
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	// Derive the status to persist. An empty Status in the event means
	// "active" (backward-compatible: pre-v0.2.0 events have no status
	// field). Core rows (provenance="core") are always active regardless of
	// what the event says — the genesis batch uses the seed pathway and
	// must never be downgraded to proposed.
	statusToStore := p.Status
	if statusToStore == "" || p.Provenance == ontologyProvenanceCore {
		statusToStore = ontology.EntityKindStatusActive
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entity_kinds
		    (name, display_name, description, parent, attr_schema, same_as, provenance, added_by, added_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		    display_name = COALESCE(NULLIF(excluded.display_name,''), entity_kinds.display_name),
		    description  = COALESCE(NULLIF(excluded.description,''), entity_kinds.description),
		    parent       = COALESCE(excluded.parent, entity_kinds.parent),
		    attr_schema  = COALESCE(excluded.attr_schema, entity_kinds.attr_schema),
		    same_as      = CASE
		                     WHEN entity_kinds.same_as IS NULL THEN excluded.same_as
		                     WHEN excluded.same_as IS NULL THEN entity_kinds.same_as
		                     ELSE (SELECT json_group_array(DISTINCT value)
		                           FROM (SELECT value FROM json_each(entity_kinds.same_as)
		                                 UNION
		                                 SELECT value FROM json_each(excluded.same_as)))
		                   END,
		    provenance   = excluded.provenance,
		    added_by     = excluded.added_by,
		    added_at     = excluded.added_at,
		    status       = CASE
		                     WHEN entity_kinds.provenance = 'core' THEN 'active'
		                     ELSE excluded.status
		                   END
		 WHERE entity_kinds.provenance != 'core' OR excluded.provenance = 'core'`,
		p.Name,
		p.DisplayName,
		p.Description,
		nullableText(extendsToStore),
		nullableBytes(attrSchemaJSON),
		nullableBytes(sameAsJSON),
		p.Provenance,
		string(p.AddedBy),
		p.AddedAt.UTC().Format(time.RFC3339Nano),
		statusToStore,
	); err != nil {
		return fmt.Errorf("upsert entity_kind: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// applyEntityKindReviewed processes an entityKindReviewed event. It updates the
// status column on the named entity_kinds row:
//   - approved → status='active'
//   - rejected → status='rejected'
//
// Guard: core rows (provenance='core') may never be downgraded; attempts are
// a logged no-op. Unknown kind names (the row doesn't exist) are also a logged
// no-op so replays stay idempotent even when the underlying row was removed.
func (d *Dispatcher) applyEntityKindReviewed(ctx context.Context, ev kernel.Event) error {
	var p ontology.EntityKindReviewedEvent
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("decode entityKindReviewed payload: %w", err)
	}
	if p.Name == "" {
		loggerFromCtx(ctx, d.logger).Warn("dispatcher: entityKindReviewed missing name; skipping",
			zap.String("event_uid", ev.UID),
		)
		return nil
	}

	// Map decision to target status.
	var newStatus string
	switch p.Decision {
	case ontology.EntityKindReviewApproved:
		newStatus = ontology.EntityKindStatusActive
	case ontology.EntityKindReviewRejected:
		newStatus = ontology.EntityKindStatusRejected
	default:
		loggerFromCtx(ctx, d.logger).Warn("dispatcher: entityKindReviewed unknown decision; skipping",
			zap.String("event_uid", ev.UID),
			zap.String("kind_name", p.Name),
			zap.String("decision", string(p.Decision)),
		)
		return nil
	}

	// Read the current row to enforce the core-row guard, detect unknowns, and
	// guard the review-lifecycle invariant.
	var curProvenance, curStatus string
	err := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT provenance, COALESCE(status,'active') FROM entity_kinds WHERE name = ?`, p.Name,
	).Scan(&curProvenance, &curStatus)
	if errors.Is(err, sql.ErrNoRows) {
		loggerFromCtx(ctx, d.logger).Warn("dispatcher: entityKindReviewed kind not found; skipping",
			zap.String("event_uid", ev.UID),
			zap.String("kind_name", p.Name),
		)
		return nil
	}
	if err != nil {
		return fmt.Errorf("query entity_kind for review %q: %w", p.Name, err)
	}
	if curProvenance == ontologyProvenanceCore {
		loggerFromCtx(ctx, d.logger).Warn("dispatcher: entityKindReviewed cannot change core kind status; skipping",
			zap.String("event_uid", ev.UID),
			zap.String("kind_name", p.Name),
		)
		return nil
	}
	// Lifecycle invariant: a review decision only applies while the kind is
	// still proposed. The CLI enforces this, but the event log is the replayed
	// source of truth — a direct or synced entityKindReviewed must not reject an
	// already-active custom kind or revive a previously-rejected one.
	if curStatus != ontology.EntityKindStatusProposed {
		loggerFromCtx(ctx, d.logger).Warn("dispatcher: entityKindReviewed on non-proposed kind; skipping",
			zap.String("event_uid", ev.UID),
			zap.String("kind_name", p.Name),
			zap.String("current_status", curStatus),
			zap.String("decision", string(p.Decision)),
		)
		return nil
	}

	// Apply the status update. The status='proposed' guard is duplicated in the
	// WHERE clause as a defence against a concurrent transition between the read
	// above and this write.
	if _, err := d.sqlite.Writer().ExecContext(ctx,
		`UPDATE entity_kinds SET status = ? WHERE name = ? AND provenance != 'core' AND COALESCE(status,'active') = 'proposed'`,
		newStatus, p.Name,
	); err != nil {
		return fmt.Errorf("update entity_kind status for review %q: %w", p.Name, err)
	}

	loggerFromCtx(ctx, d.logger).Info("dispatcher: entityKindReviewed applied",
		zap.String("event_uid", ev.UID),
		zap.String("kind_name", p.Name),
		zap.String("decision", string(p.Decision)),
		zap.String("new_status", newStatus),
	)
	return nil
}

// checkExtendsCycle walks the entity_kinds parent chain starting at
// proposedParent, looking for kindName (which would form a cycle) or a chain
// longer than dispatcherMaxExtendsDepth. Returns a non-nil error when either
// condition is detected; returns nil when the chain is safe.
//
// Uses the read-only pool (db) because the row for kindName has not yet been
// written when this check runs — we are checking whether the proposed parent
// lineage is already clean. A DB miss for any intermediate kind is treated as
// "chain ends here, no cycle" (the row may arrive later via a different event).
func checkExtendsCycle(ctx context.Context, db *sql.DB, kindName, proposedParent string) error {
	visited := make(map[string]struct{}, dispatcherMaxExtendsDepth+1)
	// kindName itself is the node we are about to insert/update; include it
	// as a visited sentinel so a direct self-reference is caught immediately.
	visited[kindName] = struct{}{}

	current := proposedParent
	for range dispatcherMaxExtendsDepth + 1 {
		if current == "" {
			return nil // reached a root — no cycle
		}
		if _, seen := visited[current]; seen {
			return fmt.Errorf("extends cycle: %q appears twice in the parent chain of %q", current, kindName)
		}
		visited[current] = struct{}{}

		var parent string
		err := db.QueryRowContext(ctx,
			`SELECT COALESCE(parent,'') FROM entity_kinds WHERE name = ?`, current,
		).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // ancestor not yet registered; chain ends here
		}
		if err != nil {
			return fmt.Errorf("query parent for %q: %w", current, err)
		}
		current = parent
	}
	return fmt.Errorf("extends chain for %q exceeds maximum depth %d", kindName, dispatcherMaxExtendsDepth)
}

// dispatcherMaxExtendsDepth is the maximum number of parent hops the
// dispatcher's cycle guard will follow before aborting. Must stay in sync
// with kernel.maxExtendsDepth; defined here to avoid a cross-package
// dependency from dispatcher back into kernel internals.
const dispatcherMaxExtendsDepth = 16

// applySchemaRegistered upserts one row into ontology_schemas. The handler is
// idempotent under replay.
//
// Compile-before-materialize guard: the Definition is compiled with the
// santhosh-tekuri compiler before the UPSERT. On compile failure the event is
// skipped (returns nil so Rebuild does not abort) and a structured warning is
// emitted. The Version is also validated as semver; an invalid version is
// similarly skipped+warned.
//
// Registry-contract guards mirroring applyEntityKindRegistered:
//   - Core-provenance guard: a non-core event cannot overwrite a row whose
//     stored provenance is "core" (unless the incoming event is itself "core").
//   - Version-monotonicity guard: a row is only replaced when the incoming
//     version is strictly newer than the stored version. A same-version event
//     with a different definition is rejected (versions are immutable, matching
//     kernel.Ontology's version-bump-to-replace contract) and the registry
//     never moves backwards to a lower version. This keeps the in-process
//     compiled-validator cache (keyed by schema_kind+version) consistent: a
//     given version always maps to one definition.
func (d *Dispatcher) applySchemaRegistered(ctx context.Context, ev kernel.Event) (retErr error) {
	var p ontology.SchemaRegisteredEvent
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("decode schemaRegistered payload: %w", err)
	}

	// Semver guard: protect the registry from malformed version strings.
	if !semver.IsValid(p.Version) {
		loggerFromCtx(ctx, d.logger).Warn("dispatcher: schemaRegistered invalid semver; skipping",
			zap.String("event_uid", ev.UID),
			zap.String("schema_kind", p.SchemaKind),
			zap.String("version", p.Version),
		)
		return nil
	}

	// Compile guard: a broken Definition must not poison the registry or
	// break subsequent Rebuild replays.
	if err := compileJSONSchema(p.Definition); err != nil {
		loggerFromCtx(ctx, d.logger).Warn("dispatcher: schemaRegistered definition compile failed; skipping",
			zap.String("event_uid", ev.UID),
			zap.String("schema_kind", p.SchemaKind),
			zap.Error(err),
		)
		return nil
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	// Apply the registry-contract guards against the existing row (if any).
	// semver comparison has no SQL equivalent, so it is done in Go inside the
	// same transaction the UPSERT runs in.
	allow, err := d.schemaRegisterAllowed(ctx, tx, ev, p)
	if err != nil {
		return err
	}
	if !allow {
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ontology_schemas (schema_kind, version, definition, provenance, added_by, added_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(schema_kind) DO UPDATE SET
		    version    = excluded.version,
		    definition = excluded.definition,
		    provenance = excluded.provenance,
		    added_by   = excluded.added_by,
		    added_at   = excluded.added_at`,
		p.SchemaKind,
		p.Version,
		string(p.Definition),
		p.Provenance,
		string(p.AddedBy),
		p.AddedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("upsert ontology_schema: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// schemaRegisterAllowed reports whether the schemaRegistered event p may
// replace the existing ontology_schemas row read inside tx. It returns false
// (a logged replay no-op) when the core-provenance guard or the
// version-monotonicity guard would be violated, true when there is no existing
// row or the incoming event is a strictly newer non-core-blocked write.
func (d *Dispatcher) schemaRegisterAllowed(ctx context.Context, tx *sql.Tx, ev kernel.Event, p ontology.SchemaRegisteredEvent) (bool, error) {
	var curVersion, curProvenance string
	err := tx.QueryRowContext(ctx,
		`SELECT version, provenance FROM ontology_schemas WHERE schema_kind = ?`,
		p.SchemaKind,
	).Scan(&curVersion, &curProvenance)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil // no existing row: plain INSERT
	case err != nil:
		return false, fmt.Errorf("read existing ontology_schema: %w", err)
	}

	// Core-provenance guard: a non-core event cannot overwrite a core row.
	if curProvenance == ontologyProvenanceCore && p.Provenance != ontologyProvenanceCore {
		loggerFromCtx(ctx, d.logger).Warn("dispatcher: schemaRegistered cannot overwrite core schema; skipping",
			zap.String("event_uid", ev.UID),
			zap.String("schema_kind", p.SchemaKind),
			zap.String("provenance", p.Provenance),
		)
		return false, nil
	}

	// Version-monotonicity guard: only replace on a strictly newer version.
	// Equal version (immutable) or lower version (no backwards moves) is a
	// replay no-op that keeps the compiled-validator cache consistent.
	if semver.Compare(p.Version, curVersion) <= 0 {
		if p.Version != curVersion {
			loggerFromCtx(ctx, d.logger).Warn("dispatcher: schemaRegistered version not newer than stored; skipping",
				zap.String("event_uid", ev.UID),
				zap.String("schema_kind", p.SchemaKind),
				zap.String("incoming_version", p.Version),
				zap.String("stored_version", curVersion),
			)
		}
		return false, nil
	}
	return true, nil
}

// compileJSONSchema attempts to compile raw (a JSON Schema document) using
// the santhosh-tekuri compiler with format-assertion enabled. Returns nil on
// success, an error describing the first compile failure otherwise. Used as a
// pre-materialize guard in applyEntityKindRegistered and applySchemaRegistered
// so broken schemas never reach the registry or derived stores.
func compileJSONSchema(raw json.RawMessage) error {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse JSON Schema document: %w", err)
	}
	const resource = "mem://dispatcher/schema-check.json"
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(resource, doc); err != nil {
		return fmt.Errorf("add schema resource: %w", err)
	}
	if _, err := c.Compile(resource); err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	return nil
}

// nullableText returns a sql.NullString that is valid when s is non-empty.
func nullableText(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// nullableBytes returns a sql.NullString from a byte slice, valid when b is
// non-nil and non-empty.
func nullableBytes(b []byte) sql.NullString {
	if len(b) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

// applyEntityIdentifierAdded inserts a row into the entity_identifiers
// table. Idempotent under replay via the table's composite primary key
// (entity_uid, identifier, kind) which collapses duplicate appends.
// Identifier additions are pure derived state, so the function takes
// neither the read lock nor the embedder.
func (d *Dispatcher) applyEntityIdentifierAdded(ctx context.Context, ev kernel.Event) (retErr error) {
	var e ontology.EntityIdentifierAddedEvent
	if err := json.Unmarshal(ev.Payload, &e); err != nil {
		return fmt.Errorf("decode entityIdentifierAdded payload: %w", err)
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	// Route the identifier to the surviving entity if the original target was
	// merged away, so the identifier is not orphaned on a deleted entity row.
	resolvedEntityUID, err := resolveRedirect(ctx, tx, e.EntityUID)
	if err != nil {
		return fmt.Errorf("resolve identifier entity: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entity_identifiers (entity_uid, identifier, kind, source, added_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(entity_uid, identifier, kind) DO UPDATE SET
			source   = excluded.source,
			added_at = excluded.added_at`,
		resolvedEntityUID,
		e.Identifier,
		string(e.Kind),
		string(e.Source),
		e.AddedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("upsert entity_identifier: %w", err)
	}

	// A user-asserted alias is also a display alias: project it into
	// entity_aliases so get-by-name and vault wikilinks ([[Old Name]]) resolve
	// to the entity, not just the extraction resolver's identifier lane. This
	// is additive (ON CONFLICT DO NOTHING) and survives Rebuild: the carrying
	// event replays after the entityCreated that seeds entity_aliases, so the
	// alias is never clobbered by writeEntityAliases. Extracted/inferred aliases
	// stay in the identifier lane only — promoting every surface form the
	// resolver discovers to name-resolvable is a separate, deliberate change.
	if e.Kind == ontology.IdentifierKindAlias && e.Source == ontology.IdentifierSourceUser {
		if err := upsertUserAliasRow(ctx, tx, ev, resolvedEntityUID, e.Identifier); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// upsertUserAliasRow inserts one user-asserted alias into entity_aliases inside
// tx. The alias inherits the entity's stored scope (so read isolation matches a
// mint-time alias); it falls back to the carrying event's scope when the row is
// missing. Create_time mirrors writeEntityAliases (the event's RFC3339Nano
// instant). ON CONFLICT(alias, entity_uid) DO NOTHING keeps the write
// idempotent under replay.
func upsertUserAliasRow(ctx context.Context, tx *sql.Tx, ev kernel.Event, entityUID, alias string) error {
	scope := scopeValue(ev)
	var stored string
	if err := tx.QueryRowContext(ctx,
		`SELECT scope FROM entities WHERE uid = ?`, entityUID,
	).Scan(&stored); err == nil && stored != "" {
		scope = stored
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read entity scope for alias: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entity_aliases (alias, entity_uid, scope, create_time)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(alias, entity_uid) DO NOTHING`,
		alias, entityUID, scope, ev.CreateTime.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("insert user alias %q: %w", alias, err)
	}
	return nil
}

// applyEpisodeForgotten deletes the derived state for the target episode
// across the four storage layers, keying everything on EpisodeUID:
//
//   - episodes row (the source of truth for FTS5 + the relational join)
//   - mentions and provenance rows that reference the episode
//   - relations whose source_event_uid is the forgotten episode, plus their
//     RELATES_TO graph edges (so recall expansion and `maz traverse` no
//     longer surface facts extracted from the forgotten source)
//   - episodes_vec row (the vector lane)
//   - LadybugDB Episode node (DETACH DELETE removes any incident MENTIONS edges)
//
// The JSONL log retains both the original Episode and the Forgotten
// event so `maz rebuild` replays both and arrives at the same end-state
// deterministically. Idempotent under replay: every DELETE accepts a
// missing row.
func (d *Dispatcher) applyEpisodeForgotten(ctx context.Context, ev kernel.Event) (retErr error) {
	var payload ontology.EpisodeForgottenEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("decode episodeForgotten payload: %w", err)
	}
	episodeUID := payload.EpisodeUID
	if episodeUID == "" {
		return errors.New("episodeForgotten: missing episodeUid")
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	// Collect relation UIDs sourced by this episode BEFORE the SQLite DELETE
	// so we can issue matching Cypher deletes on the graph side after commit.
	// Without this, the relations row goes away but the RELATES_TO edge
	// lingers — recall graph expansion and `maz traverse` would still surface
	// the forgotten fact.
	relationUIDs, err := collectRelationUIDs(ctx, tx, episodeUID)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM mentions   WHERE source_event_uid = ?`, episodeUID); err != nil {
		return fmt.Errorf("delete mentions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM relations  WHERE source_event_uid = ?`, episodeUID); err != nil {
		return fmt.Errorf("delete relations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM provenance WHERE event_uid = ?`, episodeUID); err != nil {
		return fmt.Errorf("delete provenance: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM episodes_vec WHERE uid = ?`, episodeUID); err != nil {
		// vec0 raises a different error when the row is missing; treat as
		// best-effort so a forget on a never-embedded episode does not fail.
		d.logger.Debug("dispatcher: episodes_vec delete returned error (likely missing row)",
			zap.String("episode_uid", episodeUID),
			zap.Error(err),
		)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM episodes WHERE uid = ?`, episodeUID); err != nil {
		return fmt.Errorf("delete episode: %w", err)
	}

	// Drop the graph footprint BEFORE the SQLite commit. If any Cypher delete
	// fails the SQLite rollback restores the relations rows so a subsequent
	// retry of this same tombstone can re-read them via collectRelationUIDs.
	// Committing SQLite first would empty the relations table on a successful
	// commit, then leave the orphan RELATES_TO edges visible to graph recall
	// and `maz traverse` with no path to recovery on replay. The Cypher
	// MATCH-DELETE pattern is itself idempotent (a missing edge yields a
	// zero-row match), so a partial graph success + commit failure replays
	// cleanly too.
	if err := d.dropEpisodeGraphFootprint(ctx, episodeUID, relationUIDs); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// dropEpisodeGraphFootprint removes the LadybugDB Episode node plus every
// RELATES_TO edge sourced by the forgotten episode. Extracted so the apply
// function stays under the gocognit threshold; safe to call with d.graph
// == nil (returns nil immediately).
func (d *Dispatcher) dropEpisodeGraphFootprint(ctx context.Context, episodeUID string, relationUIDs []string) error {
	if d.graph == nil {
		return nil
	}
	// Drop the RELATES_TO edges keyed by uid (collected pre-DELETE). One
	// statement per edge keeps the binder happy; LadybugDB does not yet
	// accept list-valued parameters and the cascade size is small.
	for _, relUID := range relationUIDs {
		const relCypher = `MATCH ()-[r:RELATES_TO {uid: $uid}]->() DELETE r`
		relResult, relErr := d.graph.Execute(ctx, relCypher, map[string]any{"uid": relUID})
		if relErr != nil {
			return fmt.Errorf("cypher delete relation edge %s: %w", relUID, relErr)
		}
		if cerr := relResult.Close(); cerr != nil {
			return fmt.Errorf("close cypher relation result: %w", cerr)
		}
	}
	const cypher = `MATCH (e:Episode {uid: $uid}) DETACH DELETE e`
	result, err := d.graph.Execute(ctx, cypher, map[string]any{"uid": episodeUID})
	if err != nil {
		return fmt.Errorf("cypher detach delete: %w", err)
	}
	if cerr := result.Close(); cerr != nil {
		return fmt.Errorf("close cypher result: %w", cerr)
	}
	return nil
}

// applyRelationForgotten deletes the derived state for a single relation:
// the SQLite relations row and the matching RELATES_TO graph edge. Unlike
// applyEpisodeForgotten this is a surgical drop — the source episode and
// its other extractions stay intact. Idempotent under replay: DELETE on a
// missing relation is a no-op.
func (d *Dispatcher) applyRelationForgotten(ctx context.Context, ev kernel.Event) (retErr error) {
	var payload ontology.RelationForgottenEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("decode relationForgotten payload: %w", err)
	}
	relationUID := payload.RelationUID
	if relationUID == "" {
		return errors.New("relationForgotten: missing relationUid")
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if _, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE uid = ?`, relationUID); err != nil {
		return fmt.Errorf("delete relation: %w", err)
	}

	// Drop the RELATES_TO edge BEFORE committing SQLite for the same
	// reason as applyEpisodeForgotten: if Cypher fails, the rollback
	// preserves the relations row so a retry of this same tombstone can
	// observe it again. Cypher MATCH-DELETE on a missing edge is a
	// zero-row no-op, so a Cypher-success + commit-failure replays
	// cleanly too.
	if d.graph != nil {
		const cypher = `MATCH ()-[r:RELATES_TO {uid: $uid}]->() DELETE r`
		result, gerr := d.graph.Execute(ctx, cypher, map[string]any{"uid": relationUID})
		if gerr != nil {
			return fmt.Errorf("cypher delete relation edge: %w", gerr)
		}
		if cerr := result.Close(); cerr != nil {
			return fmt.Errorf("close cypher result: %w", cerr)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// collectRelationUIDs returns the UIDs of relations whose source_event_uid
// matches episodeUID. Used by the forget cascade to know which RELATES_TO
// graph edges to drop after the SQLite relations rows go away. Extracted so
// the cursor's defer-Close lives in a scoped helper and satisfies
// sqlclosecheck.
func collectRelationUIDs(ctx context.Context, tx *sql.Tx, episodeUID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT uid FROM relations WHERE source_event_uid = ?`, episodeUID,
	)
	if err != nil {
		return nil, fmt.Errorf("collect orphan relation uids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var uids []string
	for rows.Next() {
		var uid string
		if scanErr := rows.Scan(&uid); scanErr != nil {
			return nil, fmt.Errorf("scan relation uid: %w", scanErr)
		}
		uids = append(uids, uid)
	}
	if iterErr := rows.Err(); iterErr != nil {
		return nil, fmt.Errorf("iterate relation uids: %w", iterErr)
	}
	return uids, nil
}

// LookupEntityByIdentifier returns the entity UID whose identifier+kind
// matches, or "" when no row matches. Used by the resolver's identifier
// matching step to merge alternate names ("Frodo", "Frodo Baggins") onto
// the same Entity instead of forking new UIDs per surface form.
func (d *Dispatcher) LookupEntityByIdentifier(ctx context.Context, identifier, kind string) (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return "", ErrClosed
	}
	var uid string
	err := d.sqlite.Reader().QueryRowContext(ctx,
		`SELECT entity_uid FROM entity_identifiers WHERE identifier = ? AND kind = ? LIMIT 1`,
		identifier, kind,
	).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup entity_identifier: %w", err)
	}
	return uid, nil
}

// applyEntityUpdated applies an entityUpdated event: targeted UPDATE on the
// entities SQLite row (COALESCE keeps unchanged fields intact) plus a matching
// Cypher SET on the Entity node. Only the provided fields (kind, displayName)
// are written; confidence, properties, identifiers, and coordinates are
// untouched. If displayName was updated, a background name-vector re-embed is
// scheduled so the vector recall lane reflects the new label.
//
// Idempotent under replay: a second projection of the same event lands the
// same values on the same row and is a no-op in effect.
func (d *Dispatcher) applyEntityUpdated(ctx context.Context, ev kernel.Event) error {
	var payload ontology.EntityUpdatedEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("decode entityUpdated payload: %w", err)
	}
	uid := payload.EntityUID
	if uid == "" {
		return errors.New("entityUpdated: missing entityUid")
	}

	// Targeted SQLite update: NULLIF('','') yields NULL so COALESCE falls
	// back to the stored value when the field was omitted from the payload.
	if _, err := d.sqlite.Writer().ExecContext(ctx,
		`UPDATE entities
		    SET kind         = COALESCE(NULLIF(?, ''), kind),
		        display_name = COALESCE(NULLIF(?, ''), display_name)
		  WHERE uid = ?`,
		payload.Kind, payload.DisplayName, uid,
	); err != nil {
		return fmt.Errorf("entityUpdated: update entities row: %w", err)
	}

	// Targeted Cypher SET: build a SET clause that touches only the
	// provided fields so the graph node is not reconstructed wholesale.
	if d.graph != nil {
		if err := d.applyEntityUpdatedGraph(ctx, uid, payload.Kind, payload.DisplayName); err != nil {
			return err
		}
	}

	// Re-embed when displayName changed so the name-vector lane reflects the
	// new label. Guard on the embed being wired (nil in test/minimal-boot).
	if d.embed != nil && payload.DisplayName != "" {
		d.scheduleEntityEmbeddings(ctx, uid, payload.DisplayName, "")
	}
	return nil
}

// applyEntityUpdatedGraph issues the Cypher SET for the changed fields.
// Extracted so the text of applyEntityUpdated stays under the gocognit limit.
func (d *Dispatcher) applyEntityUpdatedGraph(ctx context.Context, uid, kind, displayName string) error {
	// Build a targeted SET clause: only include properties the caller supplied.
	// Two cases (kind-only, displayName-only, both) are the interesting range;
	// the MCP tool rejects the empty-update no-op before appending.
	var cypher string
	args := map[string]any{"uid": uid}
	switch {
	case kind != "" && displayName != "":
		cypher = `MATCH (e:Entity {uid: $uid}) SET e.kind = $kind, e.displayName = $displayName`
		args["kind"] = kind
		args["displayName"] = displayName
	case kind != "":
		cypher = `MATCH (e:Entity {uid: $uid}) SET e.kind = $kind`
		args["kind"] = kind
	default:
		cypher = `MATCH (e:Entity {uid: $uid}) SET e.displayName = $displayName`
		args["displayName"] = displayName
	}
	r, err := d.graph.Execute(ctx, cypher, args)
	if err != nil {
		return fmt.Errorf("entityUpdated: cypher set: %w", err)
	}
	if cerr := r.Close(); cerr != nil {
		return fmt.Errorf("entityUpdated: close cypher result: %w", cerr)
	}
	return nil
}

// applyEntityMerged applies an entityMerged event. Mirroring
// applyEpisodeForgotten's tx+graph-before-commit ordering exactly:
//
//  1. Open a SQLite write transaction.
//  2. Repoint subject_uid/object_uid in relations, subject_uid in facts,
//     and entity_uid in mentions from source to target.
//  3. Best-effort DELETE from entities_vec / entities_vec_ids for the source
//     (warn on error, like the episodes_vec path).
//  4. DELETE FROM entities WHERE uid = source.
//  5. Drop the source Entity node from the graph BEFORE committing SQLite so a
//     Cypher failure triggers a SQLite rollback and the next replay attempt can
//     re-read the relations rows. If edge-rewiring in LadybugDB is not cleanly
//     expressible (RELATES_TO edges carry the source/target entity uid in node
//     identity, not as a property), we DETACH DELETE the source node and emit a
//     Warn log noting that graph edges for the repointed relations reconcile on
//     the next `maz rebuild` (the SQLite rows — which back recall, query, and
//     v_relations — are already correct after this projection).
//  6. Commit SQLite.
//
// Idempotent on replay: if source is already gone, all UPDATE/DELETE statements
// are no-ops and the Cypher MATCH on a missing node yields zero rows.
// ParseEntityMergedPayload decodes a raw entityMerged event payload and
// validates that both required UIDs are present. Extracted from
// applyEntityMerged to keep that function below the gocyclo threshold.
func parseEntityMergedPayload(raw json.RawMessage) (src, tgt string, err error) {
	var p ontology.EntityMergedEvent
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", "", fmt.Errorf("decode entityMerged payload: %w", err)
	}
	if p.SourceEntityUID == "" || p.TargetEntityUID == "" {
		return "", "", errors.New("entityMerged: sourceEntityUid and targetEntityUid are required")
	}
	return p.SourceEntityUID, p.TargetEntityUID, nil
}

func (d *Dispatcher) applyEntityMerged(ctx context.Context, ev kernel.Event) (retErr error) {
	src, tgt, err := parseEntityMergedPayload(ev.Payload)
	if err != nil {
		return err
	}
	// A self-merge is a no-op. Guard here as well as at the tool boundary: the
	// repoint statements below would be no-ops, but the subsequent DELETE would
	// remove the surviving row. Returning nil keeps a hand-crafted or replayed
	// self-merge event from destroying the entity it claims to preserve.
	if src == tgt {
		return nil
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	// Record the redirect with a MIN(event_uid) fold: for any given source
	// entity, the earliest-uid entityMerged event is the canonical one. The
	// WHERE guard on the ON CONFLICT branch implements the fold without a
	// read-then-write: the UPDATE fires only when the incoming event_uid is
	// lexicographically smaller than the stored one, keeping the table
	// idempotent and commutative across any replay or arrival order.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO entity_redirects (source_uid, target_uid, merged_at, event_uid)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(source_uid) DO UPDATE SET
		     target_uid = excluded.target_uid,
		     merged_at  = excluded.merged_at,
		     event_uid  = excluded.event_uid
		 WHERE excluded.event_uid < entity_redirects.event_uid`,
		src, tgt, ev.CreateTime.UTC().Format(time.RFC3339Nano), ev.UID,
	); err != nil {
		return fmt.Errorf("upsert entity redirect: %w", err)
	}

	// Follow chains and cycles to the canonical target. If tgt itself has
	// already been merged elsewhere (tgt→X), all data for src lands on X
	// directly, avoiding a two-step repoint on the next rebuild.
	effectiveTgt, err := resolveRedirect(ctx, tx, tgt)
	if err != nil {
		return fmt.Errorf("resolve merge target: %w", err)
	}

	// repointEntityFKs rewrites every src reference to effectiveTgt and then
	// collapses the duplicate active edges that repointing can create (two
	// duplicate entities each holding the same edge), so the merge never leaves
	// redundant relations/facts behind.
	if err := repointEntityFKs(ctx, tx, src, effectiveTgt); err != nil {
		return err
	}

	// Best-effort: remove entities_vec entries for the source. sqlite-vec
	// raises a distinct error on missing rows; treat as non-fatal like the
	// episodes_vec path in applyEpisodeForgotten.
	if err := d.deleteEntityVec(ctx, tx, src); err != nil {
		loggerFromCtx(ctx, d.logger).Debug("dispatcher: entities_vec delete returned error (likely missing row)",
			zap.String("source_entity_uid", src),
			zap.Error(err),
		)
	}

	// Remove source entity row.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entities WHERE uid = ?`, src,
	); err != nil {
		return fmt.Errorf("entityMerged: delete source entity: %w", err)
	}

	// Drop the source Entity node from the graph BEFORE committing SQLite.
	// Edges in LadybugDB are anchored to node identity (the :Entity nodes
	// matched by uid); there is no stable edge-property uid that lets us
	// atomically repoint them the way SQLite UPDATE does. DETACH DELETE
	// removes the source node and all incident RELATES_TO edges; the
	// SQLite relations rows have already been repointed to tgt, so the
	// relational recall surface (v_relations, query, recall) is correct.
	// The graph representation of those repointed edges reconciles on the
	// next `maz rebuild` (rebuild replays all relation events and merges
	// the RELATES_TO edges against the now-surviving target node).
	if d.graph != nil {
		if err := d.detachDeleteEntityNode(ctx, "entityMerged", src); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// repointEntityFKs rewrites every column that references src to tgt within the
// supplied transaction, BEFORE applyEntityMerged deletes the source row, then
// collapses the duplicate active edges that repointing creates (see
// dedupRepointedEdges). Extracted from applyEntityMerged to keep that function
// below the gocyclo threshold.
//
// Every table that stores an entity UID must be listed here, or the merge
// either loses data (cascade-delete) or strands a dangling pointer. Discover
// the full set with:
//
//	grep -rnE 'REFERENCES entities|_entity_uid|subject_uid|object_uid' internal/kernel/storage/migrations/sqlite/*.up.sql
//
// Current set (keep in sync when a migration adds another entity reference):
//   - relations.subject_uid / relations.object_uid (0007)
//   - facts.subject_uid (0014)
//   - mentions.entity_uid (0008)
//   - entity_identifiers.entity_uid (0010, ON DELETE CASCADE — copy+delete, see below)
//   - insights.seed_entity_uid (0038)
//   - consolidated_memories.cited_entity_uid (0039)
//   - source_refs.subject_uid (0030, generic PROV subject — may be an entity)
func repointEntityFKs(ctx context.Context, tx *sql.Tx, src, tgt string) error {
	// relations: subject and object columns are separate indexes.
	if _, err := tx.ExecContext(ctx,
		`UPDATE relations SET subject_uid = ? WHERE subject_uid = ?`, tgt, src,
	); err != nil {
		return fmt.Errorf("entityMerged: repoint relations.subject_uid: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE relations SET object_uid = ? WHERE object_uid = ?`, tgt, src,
	); err != nil {
		return fmt.Errorf("entityMerged: repoint relations.object_uid: %w", err)
	}
	// facts
	if _, err := tx.ExecContext(ctx,
		`UPDATE facts SET subject_uid = ? WHERE subject_uid = ?`, tgt, src,
	); err != nil {
		return fmt.Errorf("entityMerged: repoint facts.subject_uid: %w", err)
	}
	// mentions (entity_uid column added in migration 0008)
	if _, err := tx.ExecContext(ctx,
		`UPDATE mentions SET entity_uid = ? WHERE entity_uid = ?`, tgt, src,
	); err != nil {
		return fmt.Errorf("entityMerged: repoint mentions.entity_uid: %w", err)
	}
	// entity_identifiers carries ON DELETE CASCADE (migration 0010) and SQLite
	// runs with foreign_keys=ON, so the source's identifiers (aliases, emails,
	// usernames) would be silently destroyed by the entities DELETE. Copy them
	// to the target first — INSERT OR IGNORE keeps the merge idempotent and
	// skips identifiers the target already owns (composite PK
	// (entity_uid, identifier, kind)) — then drop the source rows explicitly so
	// the later cascade is a no-op and the intent is visible.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO entity_identifiers (entity_uid, identifier, kind, source, added_at)
		      SELECT ?, identifier, kind, source, added_at
		        FROM entity_identifiers WHERE entity_uid = ?`,
		tgt, src,
	); err != nil {
		return fmt.Errorf("entityMerged: copy entity_identifiers to target: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity_identifiers WHERE entity_uid = ?`, src,
	); err != nil {
		return fmt.Errorf("entityMerged: delete source entity_identifiers: %w", err)
	}
	// entity_aliases (0040) maps alternate-name -> entity_uid with NO FK cascade,
	// so deleting the source entity would strand its alias rows pointing at a
	// vanished uid — [[Old Name]] wikilinks and get-by-name would then resolve to
	// a deleted entity. Move the source's aliases onto the target (composite PK
	// (alias, entity_uid) — INSERT OR IGNORE skips an alias the target already
	// owns and keeps the merge idempotent on replay), then drop the source rows,
	// mirroring the entity_identifiers handling above.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO entity_aliases (alias, entity_uid, scope, create_time)
		      SELECT alias, ?, scope, create_time
		        FROM entity_aliases WHERE entity_uid = ?`,
		tgt, src,
	); err != nil {
		return fmt.Errorf("entityMerged: copy entity_aliases to target: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity_aliases WHERE entity_uid = ?`, src,
	); err != nil {
		return fmt.Errorf("entityMerged: delete source entity_aliases: %w", err)
	}
	// insights / consolidated_memories carry plain TEXT entity references (no FK,
	// so no cascade) that would dangle at the deleted source after the merge.
	if _, err := tx.ExecContext(ctx,
		`UPDATE insights SET seed_entity_uid = ? WHERE seed_entity_uid = ?`, tgt, src,
	); err != nil {
		return fmt.Errorf("entityMerged: repoint insights.seed_entity_uid: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE consolidated_memories SET cited_entity_uid = ? WHERE cited_entity_uid = ?`, tgt, src,
	); err != nil {
		return fmt.Errorf("entityMerged: repoint consolidated_memories.cited_entity_uid: %w", err)
	}
	// source_refs.subject_uid is a generic PROV subject (the record a source was
	// derived from); when that record is the merged entity, the surviving target
	// inherits the provenance. UIDs are globally unique ULIDs, so this matches
	// only source refs whose subject IS src.
	if _, err := tx.ExecContext(ctx,
		`UPDATE source_refs SET subject_uid = ? WHERE subject_uid = ?`, tgt, src,
	); err != nil {
		return fmt.Errorf("entityMerged: repoint source_refs.subject_uid: %w", err)
	}
	// Repointing can turn two distinct rows into the same triple (both duplicate
	// entities held the same edge). Collapse those now, in the same tx, so the
	// merge never leaves duplicate active edges behind.
	return dedupRepointedEdges(ctx, tx, tgt)
}

// dedupRepointedEdges collapses the duplicate active edges a merge can create:
// once repointEntityFKs has rewritten every src reference to tgt, an edge the
// source held and an edge the target held can be the same triple. It runs inside
// the merge transaction, AFTER the repoint and BEFORE the source row is deleted.
// Three passes, all scoped to rows touching tgt so the merge never disturbs
// edges elsewhere in the graph:
//
//  1. Self-loop drop: a same_as / alias edge between the merged pair becomes
//     (tgt -> tgt) after repointing. A self-referential relation is meaningless,
//     so every active tgt->tgt relation is removed.
//  2. Relation triple-dedup: among ACTIVE (superseded_by_uid IS NULL) rows of the
//     SAME status, keep the HIGHEST-confidence row per (subject, predicate,
//     object) — ties broken by lowest uid — and delete the rest, so a later
//     higher-confidence reassertion is never collapsed into an older, weaker
//     duplicate. Grouping on status preserves the asserted/proposed coexistence
//     (migration 0036) — a proposed connector edge must never be collapsed into
//     an authoritative asserted one.
//  3. Fact dedup: among ACTIVE (event_end IS NULL) facts keep the highest
//     source_priority row per (subject, state_key, value) — ties broken by lowest
//     uid. Only byte-identical facts collapse; two open facts that share a
//     state_key but differ in value are a genuine conflict the merge surfaces,
//     not a duplicate, so they are left untouched.
//
// Superseded/closed history is never touched: identical triples are only illegal
// among live rows, and rewriting history would break event-replay idempotency.
// The graph's RELATES_TO edges reconcile on the next `maz rebuild`.
func dedupRepointedEdges(ctx context.Context, tx *sql.Tx, tgt string) error {
	// 1. Drop self-loops the merge created (e.g. a same_as edge between the pair).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM relations
		  WHERE superseded_by_uid IS NULL
		    AND subject_uid = ? AND object_uid = ?`,
		tgt, tgt,
	); err != nil {
		return fmt.Errorf("entityMerged: drop self-loop relations: %w", err)
	}

	// 2. Collapse duplicate active relation triples, per status, keeping the
	//    HIGHEST-confidence row (ties broken by lowest uid). Rank each row within
	//    its (subject, predicate, object, status) group by confidence DESC then
	//    uid ASC and delete every row past the first — that leaves exactly the
	//    best row per group, so a later higher-confidence reassertion is never
	//    silently downgraded to an older, weaker duplicate. A single-pass
	//    ROW_NUMBER() window replaces the prior O(N²) correlated NOT-EXISTS.
	//    Scoping the scan to rows touching tgt stays correct AND complete: every
	//    member of a group shares the row's subject_uid AND object_uid, so if any
	//    one member touches tgt they all do — no group is split across the filter.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM relations
		  WHERE uid IN (
		      SELECT uid FROM (
		          SELECT uid, ROW_NUMBER() OVER (
		                       PARTITION BY subject_uid, predicate, object_uid, status
		                       ORDER BY confidence DESC, uid ASC) AS rn
		            FROM relations
		           WHERE superseded_by_uid IS NULL
		             AND (subject_uid = ? OR object_uid = ?)
		      ) WHERE rn > 1
		  )`,
		tgt, tgt,
	); err != nil {
		return fmt.Errorf("entityMerged: dedup relation triples: %w", err)
	}

	// 3. Collapse duplicate active facts (identical subject/state_key/value),
	//    keeping the highest-priority row (ties broken by lowest uid). Facts carry
	//    source_priority rather than confidence, so the ranking uses it — the same
	//    anti-downgrade rule as relations above, via the same single-pass
	//    ROW_NUMBER() window. Facts have no object_uid, so the scoped scan keys on
	//    subject_uid = tgt, which fully contains every (subject, state_key, value)
	//    group it touches.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM facts
		  WHERE uid IN (
		      SELECT uid FROM (
		          SELECT uid, ROW_NUMBER() OVER (
		                       PARTITION BY subject_uid, state_key, value
		                       ORDER BY source_priority DESC, uid ASC) AS rn
		            FROM facts
		           WHERE event_end IS NULL
		             AND subject_uid = ?
		      ) WHERE rn > 1
		  )`,
		tgt,
	); err != nil {
		return fmt.Errorf("entityMerged: dedup facts: %w", err)
	}
	return nil
}

// resolveRedirect follows the entity_redirects chain from uid to fixpoint and
// returns the canonical surviving entity UID. If uid has no row in
// entity_redirects it is returned unchanged (the entity was never merged or is
// itself the survivor). Chains (A→B, B→C) resolve by following rows until no
// further redirect exists. Cycles (e.g. concurrent A→B and B→A) are broken by
// returning the lexicographically smallest UID among the cycle members — the
// same deterministic rule the entity_redirects upsert applies via the
// MIN(event_uid) fold, so any arrival order yields the same survivor.
//
// The function is bounded to maxRedirectHops iterations as a guard against
// pathological input; it returns the current best tip when that limit is hit.
func resolveRedirect(ctx context.Context, tx *sql.Tx, uid string) (string, error) {
	const maxRedirectHops = 64
	path := make([]string, 0, 8)
	cur := uid
	for range maxRedirectHops {
		var tgt string
		err := tx.QueryRowContext(ctx,
			`SELECT target_uid FROM entity_redirects WHERE source_uid = ?`, cur,
		).Scan(&tgt)
		if errors.Is(err, sql.ErrNoRows) {
			return cur, nil // fixpoint: no redirect from cur
		}
		if err != nil {
			return uid, fmt.Errorf("resolve redirect %q: %w", uid, err)
		}
		// Detect cycle: tgt was visited in a previous hop.
		path = append(path, cur)
		for j, v := range path {
			if v == tgt {
				// Cycle confirmed. Survivor = MIN UID among cycle members
				// (path[j:] — the segment that forms the loop).
				min := path[j]
				for _, m := range path[j+1:] {
					if m < min {
						min = m
					}
				}
				return min, nil
			}
		}
		cur = tgt
	}
	return cur, nil
}

// applyEntityDeleted applies an entityDeleted tombstone: it removes the entity
// and every derived row that references it, all in one SQLite transaction, then
// detaches the graph node. Unlike applyEntityMerged there is NO target to
// repoint to — the entity is gone, not absorbed — so every entity-UID reference
// is deleted rather than rewritten. The tool boundary refuses to delete an
// entity that still has active relations unless the caller cascades (each active
// relation is tombstoned via relationForgotten first, for the per-edge audit
// trail); this applier then sweeps any remaining references (inactive relations,
// facts, mentions, identifiers, insights, citations, provenance) so nothing
// dangles. Only entity_identifiers carries an ON DELETE CASCADE FK to entities;
// every other table holds a bare UID and would strand a dangling pointer if not
// deleted here.
//
// Idempotent on replay: if the entity is already gone, every DELETE is a no-op
// and the Cypher MATCH on a missing node yields zero rows.
func (d *Dispatcher) applyEntityDeleted(ctx context.Context, ev kernel.Event) (retErr error) {
	var payload ontology.EntityDeletedEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("decode entityDeleted payload: %w", err)
	}
	uid := payload.EntityUID
	if uid == "" {
		return errors.New("entityDeleted: entityUid is required")
	}

	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
		}
	}()

	if err := deleteEntityFKs(ctx, tx, uid); err != nil {
		return err
	}

	// Best-effort: remove entities_vec entries (name + summary vectors).
	// sqlite-vec raises a distinct error on missing rows; treat as non-fatal,
	// matching applyEntityMerged.
	if err := d.deleteEntityVec(ctx, tx, uid); err != nil {
		loggerFromCtx(ctx, d.logger).Debug("dispatcher: entities_vec delete returned error (likely missing row)",
			zap.String("entity_uid", uid),
			zap.Error(err),
		)
	}

	// Remove the entity row itself (after its FK-referencing rows, so the
	// entity_identifiers ON DELETE CASCADE has nothing left to cascade).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entities WHERE uid = ?`, uid,
	); err != nil {
		return fmt.Errorf("entityDeleted: delete entity: %w", err)
	}

	// Detach the Entity node from the graph BEFORE committing SQLite, mirroring
	// applyEntityMerged: DETACH DELETE removes the node and every incident
	// RELATES_TO edge, whose backing relations rows were already deleted above.
	if d.graph != nil {
		if err := d.detachDeleteEntityNode(ctx, "entityDeleted", uid); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// deleteEntityFKs removes every derived row that references uid, within the
// supplied transaction, BEFORE applyEntityDeleted deletes the entity row. It is
// the delete-side mirror of repointEntityFKs and MUST stay in sync with it: the
// same table set that a merge repoints, a delete removes. Extracted to keep
// applyEntityDeleted below the gocyclo threshold.
//
// Current set (keep in sync with repointEntityFKs when a migration adds another
// entity reference):
//   - relations.subject_uid / relations.object_uid (0007)
//   - facts.subject_uid (0014)
//   - mentions.entity_uid (0008)
//   - entity_identifiers.entity_uid (0010, ON DELETE CASCADE — deleted explicitly)
//   - entity_aliases.entity_uid (0040, no cascade)
//   - insights.seed_entity_uid (0038)
//   - consolidated_memories.cited_entity_uid (0039)
//   - source_refs.subject_uid (0030)
func deleteEntityFKs(ctx context.Context, tx *sql.Tx, uid string) error {
	// Each statement carries its own args, so the placeholder count is co-located
	// with the SQL rather than special-cased in the loop (relations binds uid
	// twice — subject_uid OR object_uid).
	stmts := []struct {
		what string
		sql  string
		args []any
	}{
		{"relations", `DELETE FROM relations WHERE subject_uid = ? OR object_uid = ?`, []any{uid, uid}},
		{"facts", `DELETE FROM facts WHERE subject_uid = ?`, []any{uid}},
		{"mentions", `DELETE FROM mentions WHERE entity_uid = ?`, []any{uid}},
		{"entity_identifiers", `DELETE FROM entity_identifiers WHERE entity_uid = ?`, []any{uid}},
		{"entity_aliases", `DELETE FROM entity_aliases WHERE entity_uid = ?`, []any{uid}},
		{"insights", `DELETE FROM insights WHERE seed_entity_uid = ?`, []any{uid}},
		{"consolidated_memories", `DELETE FROM consolidated_memories WHERE cited_entity_uid = ?`, []any{uid}},
		{"source_refs", `DELETE FROM source_refs WHERE subject_uid = ?`, []any{uid}},
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("entityDeleted: delete %s: %w", s.what, err)
		}
	}
	return nil
}

// deleteEntityVec removes all entities_vec rows for uid. Extracted to keep
// applyEntityMerged below the gocognit limit. Errors are treated as
// best-effort by the caller.
func (d *Dispatcher) deleteEntityVec(ctx context.Context, tx *sql.Tx, uid string) error {
	// Collect rowids from the sidecar first (the vec0 virtual table indexes by rowid).
	rows, err := tx.QueryContext(ctx,
		`SELECT rowid FROM entities_vec_ids WHERE id = ?`, uid,
	)
	if err != nil {
		return fmt.Errorf("query entities_vec_ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var rowids []int64
	for rows.Next() {
		var rid int64
		if scanErr := rows.Scan(&rid); scanErr != nil {
			return fmt.Errorf("scan rowid: %w", scanErr)
		}
		rowids = append(rowids, rid)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate entities_vec_ids: %w", err)
	}
	_ = rows.Close()

	for _, rid := range rowids {
		if _, delErr := tx.ExecContext(ctx,
			`DELETE FROM entities_vec WHERE rowid = ?`, rid,
		); delErr != nil {
			return fmt.Errorf("delete entities_vec rowid %d: %w", rid, delErr)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entities_vec_ids WHERE id = ?`, uid,
	); err != nil {
		return fmt.Errorf("delete entities_vec_ids: %w", err)
	}
	return nil
}

// detachDeleteEntityNode removes the named Entity node and all its incident
// RELATES_TO edges from the graph. Called before the SQLite commit so a Cypher
// failure triggers a SQLite rollback (see applyEpisodeForgotten's ordering
// comment). A MATCH on a missing node yields zero rows and is idempotent. The
// op label ("entityMerged"/"entityDeleted") tags the log + error wrapping so a
// delete is never mislabeled as a merge.
func (d *Dispatcher) detachDeleteEntityNode(ctx context.Context, op, uid string) error {
	const cypher = `MATCH (e:Entity {uid: $uid}) DETACH DELETE e`
	r, err := d.graph.Execute(ctx, cypher, map[string]any{"uid": uid})
	if err != nil {
		return fmt.Errorf("%s: cypher detach delete entity node: %w", op, err)
	}
	if cerr := r.Close(); cerr != nil {
		return fmt.Errorf("%s: close cypher result: %w", op, cerr)
	}
	// Debug, not Warn: this is a per-operation reconciliation note on a SUCCESS
	// path (every merge/delete logs it), and it's misleading where the live
	// daemon reconciles the graph online. The user-facing guidance lives in
	// `maz entity merge --help`; `doctor`'s storage.drift check catches any real
	// graph/SQLite divergence.
	d.logger.Debug("dispatcher: "+op+": entity node detach-deleted; graph RELATES_TO edges reconcile on next maz rebuild",
		zap.String("entity_uid", uid),
	)
	return nil
}

// Close shuts down every layer. Safe to call multiple times; subsequent
// calls return nil. Takes the write lock so it blocks until every
// in-flight Apply / Open* call drains, then marks the dispatcher closed
// and tears down the underlying connections in dependency order.
func (d *Dispatcher) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()

	// Drain any in-flight background embedding goroutines BEFORE acquiring
	// the writer mutex and tearing down the SQLite pools. The workers
	// observe d.closed via d.mu.RLock inside writeEpisodeVectorAsync and
	// short-circuit; this Wait covers the workers that already passed the
	// closed check and are mid-Embed or mid-tx. Without it, Close races
	// against an embedder return and a vec0 write through a torn-down
	// pool.
	d.bgVec.Wait()

	d.mu.Lock()
	defer d.mu.Unlock()

	var errs []error

	// Drain the installed embedder before tearing down the SQLite pools
	// the cache writes through. CachingEmbedder.Close cancels the
	// cache's lifecycle ctx (surfacing context.Canceled into any wedged
	// inner embedder call or SQLite read/write) and then waits for the
	// tracker goroutines to exit. d.mu.Lock above already blocks any
	// new Apply call from acquiring its RLock, so no new singleflight
	// trackers spawn while we wait. Greptile #108 P1 (fourth pass) —
	// without the lifecycle ctx, a stuck worker would pin shutdown
	// forever.
	if c, ok := d.embed.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close embedder: %w", err))
		}
	}

	if d.graph != nil {
		if err := d.graph.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close graph: %w", err))
		}
		d.graph = nil
	}
	if d.sqlite != nil {
		if err := d.sqlite.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close sqlite: %w", err))
		}
		d.sqlite = nil
	}
	d.vector = nil
	d.fts = nil
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("dispatcher: close: %w", errors.Join(errs...))
}

// joinClose combines a primary error with a secondary close error so the
// Open partial-failure path surfaces both: the cause of the failure and
// the close failure (if any) that followed. A nil close error returns
// the primary unchanged.
func joinClose(primary, closeErr error) error {
	if closeErr == nil {
		return primary
	}
	return fmt.Errorf("%w (also: close sqlite: %w)", primary, closeErr)
}

// loggerFromCtx prefers the request-scoped logger that testutil.TestContext
// installs; falls back to the dispatcher-construction logger when the
// context carries none.
func loggerFromCtx(ctx context.Context, fallback *zap.Logger) *zap.Logger {
	if l := logging.FromContext(ctx); l != nil {
		return l
	}
	return fallback
}
