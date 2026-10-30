// Package orchestrator wires the four kernel collaborators that turn an
// EventLog.Append into a durable JSONL line *and* coherent writes
// across the four derived stores (SQLite + LadybugDB + sqlite-vec +
// FTS5). It is the kernel's wiring site for issue #55:
//
//	Ollama --(cache wrap)--> CachingEmbedder --> Dispatcher --> FileEventLog
//
// This package owns no goroutines and no schemas. It is the only
// kernel package that imports both internal/kernel/cache and a real
// provider type (via the embedder.Embedder interface); the underlying
// kernel package stays free of the dispatcher / cache / provider deps
// so it can compile in isolation for unit tests.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/cache"
	"github.com/mo-cmyk/mazarbul/internal/kernel/embedder"
	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
)

// Orchestrator binds the four collaborators. Construct with New; the
// zero value is not usable. The exported fields are the surface the
// daemon (and tests) use to enqueue work; the unexported closers slice
// is what Close fans out over.
type Orchestrator struct {
	Log      *kernel.FileEventLog
	Storage  kernel.Storage
	Embedder embedder.Embedder

	closers []func() error
}

// Config bundles the inputs New needs. DataDir and Ontology are
// required; Embedder and Logger are optional.
type Config struct {
	// DataDir is the worktree's data directory. New places:
	//   events/                 JSONL day buckets (FileEventLog)
	//   events/index.bolt       UID -> offset sidecar (FileEventLog)
	//   index.db                SQLite relational + vec + FTS5 (Dispatcher)
	//   graph.ladybug/          LadybugDB database root (Dispatcher)
	DataDir string

	// Ontology validates payloads before they land on JSONL. Required:
	// NewFileEventLog needs a non-nil Ontology.
	Ontology kernel.Ontology

	// Embedder is the embedding provider that the dispatcher uses for
	// vector inserts. New wraps it with a CachingEmbedder (SQLite-backed
	// dedup) before installing it on the dispatcher. A nil Embedder is
	// valid and means: the dispatcher writes the three non-vector
	// layers, vector inserts are skipped. The init wizard's --no-embed
	// mode hits this path.
	Embedder embedder.Embedder

	// Logger is the structured logger. Optional; defaults to
	// zap.NewNop. Threaded into the dispatcher and surfaced from
	// logging.FromContext in the EventLog's warn path.
	Logger *zap.Logger

	// Extractor is the optional post-Append extraction hook. When set,
	// the FileEventLog runs it after every successful storage.Apply for
	// Episode events; the extractor returns Mention and
	// RelationCandidatesProposed events which the log re-Appends. A nil
	// value means extraction is skipped (M1 default in tests).
	Extractor kernel.ExtractorIface

	// Resolver is the optional post-extraction resolution hook. When set,
	// the FileEventLog runs it after applyExtraction has appended its
	// derived Mention and RelationCandidatesProposed events. The resolver
	// emits Entity / Relation / RelationRetracted / PredicateRegistered
	// events. A nil value means resolution is skipped (M1 default in tests).
	Resolver kernel.ResolverIface

	// ExtractionWorkers caps concurrent background extraction goroutines.
	// Zero means "use the legacy per-Append goroutine spawn" (fine for
	// tests; problematic under haystack ingestion bursts). Production
	// wiring sets this to a small positive number that matches Ollama's
	// realistic concurrency ceiling (4 mirrors the default
	// OLLAMA_NUM_PARALLEL); see WithExtractionWorkers on the EventLog.
	ExtractionWorkers int

	// ExtractionTimeout bounds the per-batch extraction context.
	// Zero falls back to the EventLog's defaultExtractionTimeout (sized
	// for the managed Ollama default of one parallel slot). Deployments
	// with higher OLLAMA_NUM_PARALLEL can tighten this; deployments with
	// slower hardware or larger bursts can widen it. See
	// WithExtractionTimeout on the EventLog.
	ExtractionTimeout time.Duration

	// ResolutionTimeout bounds the per-batch resolution context.
	// Zero falls back to the EventLog's defaultResolutionTimeout.
	// Resolution emits multiple LLM calls per batch (naming + summary +
	// relation classification), so under burst ingestion it queues
	// behind the same Ollama serializer that extraction does and needs
	// a comparable envelope. See WithResolutionTimeout on the EventLog.
	ResolutionTimeout time.Duration

	// Stats is the runtime counter sink threaded through the
	// dispatcher so write-path counters (IncDedup, future
	// IncIngestion, …) report to the same Snapshot the Recall / Embed
	// lanes already feed. Nil falls back to stats.Noop().
	Stats stats.Store

	// DedupSemanticThreshold enables the dispatcher's pre-write
	// semantic dedup branch (Phase 2 mem0-parity). Zero (the default)
	// keeps the legacy async-embed write path. Production starts at
	// 0.85 per the mem0 v3 NOOP recipe.
	DedupSemanticThreshold float64

	// StrictAttributes, when true, causes the FileEventLog to reject
	// Entity events whose properties field fails to validate against the
	// kind's composed propertySchema (P3 typed properties). When false
	// (the default) violations are warn-logged and the event is accepted.
	// Threaded from [ontology] strict_attributes in config.toml via the
	// daemon wiring path; see kernel.WithStrictAttributes.
	StrictAttributes bool

	// StrictDomainRange, when true, causes the FileEventLog to reject
	// Relation events whose subject/object kinds fall outside the predicate's
	// declared domain/range, or whose Attributes fail the predicate's
	// propertySchema (P5 typed relations). When false (the default) violations
	// are warn-logged and the event is accepted. Threaded from
	// [ontology] strict_domain_range in config.toml; see
	// kernel.WithStrictDomainRange.
	StrictDomainRange bool
}

// New opens the dispatcher, builds the embedding cache against the
// dispatcher's SQLite pools, installs the cache on the dispatcher,
// then constructs the EventLog with kernel.WithStorage pointing at
// the dispatcher. On a partial-failure path every component that
// already opened is closed before the error is returned.
//
// Construction order matters: dispatcher.Open provisions the SQLite
// pools that cache.New requires, but the cache wants to be installed
// on the dispatcher before any Apply runs. SetEmbedder closes the
// cycle.
func New(ctx context.Context, cfg Config) (*Orchestrator, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("orchestrator: empty DataDir")
	}
	if cfg.Ontology == nil {
		return nil, errors.New("orchestrator: nil Ontology")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	d, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir: cfg.DataDir,
		Logger:  logger,
		// Embedder is installed below via SetEmbedder so the cache can
		// wrap cfg.Embedder against the dispatcher's SQLite pools first.
		Stats:                  cfg.Stats,
		DedupSemanticThreshold: cfg.DedupSemanticThreshold,
	})
	if err != nil {
		return nil, fmt.Errorf("orchestrator: open dispatcher: %w", err)
	}
	closers := make([]func() error, 0, 2)
	closers = append(closers, d.Close)

	var emb embedder.Embedder
	if cfg.Embedder != nil {
		writer, werr := d.OpenSQLite(ctx)
		if werr != nil {
			return nil, joinAndClose(fmt.Errorf("orchestrator: sqlite writer: %w", werr), closers)
		}
		reader, rerr := d.SQLiteReader()
		if rerr != nil {
			return nil, joinAndClose(fmt.Errorf("orchestrator: sqlite reader: %w", rerr), closers)
		}
		caching := cache.New(cfg.Embedder, reader, writer, nil /* time.Now */)
		d.SetEmbedder(caching)
		emb = caching
	}

	logOpts := []kernel.Option{kernel.WithStorage(d)}
	if cfg.Extractor != nil {
		logOpts = append(logOpts, kernel.WithExtractor(cfg.Extractor))
	}
	if cfg.Resolver != nil {
		logOpts = append(logOpts, kernel.WithResolver(cfg.Resolver))
	}
	if cfg.ExtractionWorkers > 0 {
		logOpts = append(logOpts, kernel.WithExtractionWorkers(cfg.ExtractionWorkers))
	}
	if cfg.ExtractionTimeout > 0 {
		logOpts = append(logOpts, kernel.WithExtractionTimeout(cfg.ExtractionTimeout))
	}
	if cfg.ResolutionTimeout > 0 {
		logOpts = append(logOpts, kernel.WithResolutionTimeout(cfg.ResolutionTimeout))
	}
	if cfg.StrictAttributes {
		logOpts = append(logOpts, kernel.WithStrictAttributes(true))
	}
	if cfg.StrictDomainRange {
		logOpts = append(logOpts, kernel.WithStrictDomainRange(true))
	}
	log := kernel.NewFileEventLog(cfg.DataDir, cfg.Ontology, logOpts...)
	// Owning the FileEventLog's bbolt sidecar handle is non-optional:
	// the single-writer invariant in eventlog.go puts an exclusive OS
	// file lock on events/index.bolt, so a second orchestrator over
	// the same dataDir would block on its first Append/At until we
	// release it. Append it AFTER d.Close so LIFO closes the log
	// before the dispatcher (cleanly separated handles).
	closers = append(closers, log.Close)

	return &Orchestrator{
		Log:      log,
		Storage:  d,
		Embedder: emb,
		closers:  closers,
	}, nil
}

// Close shuts every owned component down in LIFO order and joins any
// errors. Safe to call multiple times: subsequent calls are no-ops.
// Closes both the dispatcher (releasing SQLite + LadybugDB) and the
// FileEventLog (releasing the bbolt sidecar's exclusive OS lock).
func (o *Orchestrator) Close() error {
	var errs []error
	for i := len(o.closers) - 1; i >= 0; i-- {
		if err := o.closers[i](); err != nil {
			errs = append(errs, err)
		}
	}
	o.closers = nil
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("orchestrator: close: %w", errors.Join(errs...))
}

// joinAndClose runs every closer in LIFO order and joins their errors
// with the leading err. Used during partial-failure unwind in New so
// callers always see the original cause first.
func joinAndClose(err error, closers []func() error) error {
	errs := []error{err}
	for i := len(closers) - 1; i >= 0; i-- {
		if cerr := closers[i](); cerr != nil {
			errs = append(errs, cerr)
		}
	}
	return fmt.Errorf("orchestrator: %w", errors.Join(errs...))
}
