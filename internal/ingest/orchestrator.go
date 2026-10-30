package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// maxEpisodeBodyBytes caps an ingested body before it becomes an episode.
// IngestItem is the only caller and the only connector-ingest path, so this cap
// applies to CONNECTOR content (commit/PR bodies, meeting transcripts) — never
// to `maz remember` episodes, which take a different path. 128 KiB fits real
// meeting transcripts (which run tens of KiB) end to end so their full text is
// stored and FULL-TEXT (FTS) searchable, while still bounding genuinely
// pathological inputs (e.g. a committed data blob in a commit message).
//
// Embedder-context tradeoff: a body beyond the embedder's context window
// (~8K tokens for snowflake-arctic-embed2) is not fully represented in the
// vector index — FTS covers all of it, but VECTOR recall covers only the
// leading portion of a very long episode. Splitting long transcripts into
// multiple embeddable episodes (full vector coverage) is the chunking follow-up;
// raising the cap here was the user's chosen tradeoff so no transcript content
// is silently dropped at ingest.
const maxEpisodeBodyBytes = 131072

// capBody truncates s to maxEpisodeBodyBytes on a UTF-8 rune boundary, appending
// a visible marker noting how many bytes were dropped. Clean (under-cap) text is
// returned unchanged.
func capBody(s string) string {
	if len(s) <= maxEpisodeBodyBytes {
		return s
	}
	cut := maxEpisodeBodyBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[... %d bytes truncated by ingest]", len(s)-cut)
}

// Job + bound defaults.
const (
	jobKindIngest             = "ingest"
	defaultSliceSize          = 50
	defaultMaxItems           = 500
	defaultMaxExtractionCalls = 500
	watermarkEventSource      = "ingest:watermark"
)

// Appender is the one-method event-log writer the orchestrator needs.
// *kernel.FileEventLog satisfies it; tests pass a fake. The signature matches
// kernel.EventLog.Append exactly.
type Appender interface {
	Append(ctx context.Context, events []kernel.Event) ([]kernel.Receipt, error)
}

// Bounds caps a backfill at the JOB layer (NOT extractor.Config). MaxItems caps
// items considered; MaxExtractionCalls caps episodes appended (each is one
// downstream extraction); SliceSize is the per-Pull limit and checkpoint cadence.
type Bounds struct {
	MaxItems           int
	MaxExtractionCalls int
	SliceSize          int
}

func (b Bounds) normalized() Bounds {
	if b.SliceSize <= 0 {
		b.SliceSize = defaultSliceSize
	}
	if b.MaxItems <= 0 {
		b.MaxItems = defaultMaxItems
	}
	if b.MaxExtractionCalls <= 0 {
		b.MaxExtractionCalls = defaultMaxExtractionCalls
	}
	return b
}

// StreamReport is the per-stream outcome of a run.
type StreamReport struct {
	Stream   string          `json:"stream"`
	Cursor   json.RawMessage `json:"cursor,omitempty"`
	Sequence uint64          `json:"sequence"`
	Applied  int             `json:"applied"`
	Skipped  int             `json:"skipped"`
	Err      string          `json:"err,omitempty"` // soft per-stream error (stream skipped)
}

// Report summarises a connect/ingest run.
type Report struct {
	Connector        string         `json:"connector"`
	JobUID           string         `json:"jobUid"`
	DryRun           bool           `json:"dryRun"`
	ItemsApplied     int            `json:"itemsApplied"`
	ItemsSkipped     int            `json:"itemsSkipped"`
	EpisodesAppended int            `json:"episodesAppended"`
	Streams          []StreamReport `json:"streams"`
}

// Orchestrator owns the universal half of connect-and-ingest. It is source-
// agnostic: dedup, content-hash change detection, normalisation into Episodes,
// atomic SourceRef emission, watermark advance, job-layer bounding, and cancel.
type Orchestrator struct {
	log      Appender
	reader   Reader
	jobs     *jobs.Registry
	scrubber *Scrubber
	logger   *zap.Logger
	newUID   func() (string, error)
	now      func() time.Time
}

// NewOrchestrator builds an Orchestrator. Logger may be nil (defaults to a nop).
func NewOrchestrator(log Appender, reader Reader, registry *jobs.Registry, logger *zap.Logger) *Orchestrator {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Orchestrator{
		log:      log,
		reader:   reader,
		jobs:     registry,
		scrubber: NewScrubber(),
		logger:   logger,
		newUID:   defaultNewUID,
		now:      time.Now,
	}
}

func defaultNewUID() (string, error) { return ulid.Make().String(), nil }

// runBudget tracks the remaining item / extraction-call budget across all
// streams of a single run.
type runBudget struct {
	items int
	calls int
}

func (b *runBudget) exhausted() bool { return b.items <= 0 || b.calls <= 0 }

// Run drives one connect/ingest job to completion or cancellation. A cursor
// validation failure FAILs the job; a soft per-stream Pull error skips that
// stream and is recorded in the Report. Ingested content defaults to
// ScopePersonal; ScopeProject is REFUSED (it is plaintext/public and would
// bypass the #204 leak gate by design).
func (o *Orchestrator) Run(ctx context.Context, src Source, scope kernel.Scope, bounds Bounds, dryRun bool) (Report, error) {
	if scope == kernel.ScopeProject {
		return Report{}, errors.New("ingest: refusing project scope: ingested external content defaults to personal (encrypted); project is plaintext/public")
	}
	if scope == "" {
		scope = kernel.ScopePersonal
	}
	if err := src.Validate(ctx); err != nil {
		return Report{}, fmt.Errorf("ingest: source validate: %w", err)
	}
	bounds = bounds.normalized()

	tracker, jobCtx := o.jobs.Begin(ctx, jobKindIngest, "ingest "+src.Name())
	report := Report{Connector: src.Name(), JobUID: tracker.UID(), DryRun: dryRun}
	budget := &runBudget{items: bounds.MaxItems, calls: bounds.MaxExtractionCalls}

	var fatal error
	for _, stream := range src.Streams() {
		if jobCtx.Err() != nil || budget.exhausted() {
			break
		}
		sr, ferr := o.runStream(jobCtx, src, stream, scope, bounds, budget, dryRun)
		report.Streams = append(report.Streams, sr)
		report.ItemsApplied += sr.Applied
		report.ItemsSkipped += sr.Skipped
		if ferr != nil {
			fatal = ferr
			break
		}
	}
	report.EpisodesAppended = report.ItemsApplied

	switch {
	case fatal != nil:
		tracker.Fail(fatal)
		return report, fatal
	case jobCtx.Err() != nil:
		tracker.CancelDone()
		return report, nil //nolint:nilerr // cancellation is a graceful outcome; partial work is preserved, not an error
	default:
		tracker.Done()
		return report, nil
	}
}

// runStream drains one stream up to the shared budget. It returns a fatal error
// only for cursor validation (which must fail the whole job); a watermark-load
// or Pull error is soft (logged, recorded in sr.Err, the stream abandoned, the
// job continues).
func (o *Orchestrator) runStream(ctx context.Context, src Source, stream string, scope kernel.Scope, bounds Bounds, budget *runBudget, dryRun bool) (StreamReport, error) {
	sr := StreamReport{Stream: stream}
	wm, err := o.reader.Watermark(ctx, src.Name(), stream)
	if err != nil {
		o.logger.Warn("ingest: load watermark failed; skipping stream",
			zap.String("connector", src.Name()), zap.String("stream", stream), zap.Error(err))
		sr.Err = err.Error()
		return sr, nil
	}
	sr.Cursor, sr.Sequence = wm.Cursor, wm.Sequence

	for {
		if ctx.Err() != nil || budget.exhausted() {
			return sr, nil //nolint:nilerr // cancel/budget stop is a graceful outcome; partial work is preserved
		}
		batch, perr := src.Pull(ctx, stream, wm, sliceLimit(bounds, budget))
		if perr != nil {
			o.logger.Warn("ingest: stream pull failed; skipping stream",
				zap.String("connector", src.Name()), zap.String("stream", stream), zap.Error(perr))
			sr.Err = perr.Error()
			return sr, nil
		}

		// Cursor validation is the HARD gate (AC-5): a non-monotonic / oversized /
		// malformed cursor FAILs the job — no silent reset, no over-pull.
		if verr := ontology.ValidateCursor(wm.Sequence, batch.NextSequence, batch.NextCursor, batch.Done); verr != nil {
			return sr, fmt.Errorf("ingest: stream %q cursor validation failed: %w", stream, verr)
		}

		o.applyBatch(ctx, src.Name(), stream, scope, batch.Items, budget, dryRun, &sr)

		// Checkpoint: persist the advanced watermark after each successful slice so
		// a crash/cancel mid-backfill resumes from here. Dry runs never write.
		if !dryRun {
			if werr := o.appendWatermark(ctx, src.Name(), stream, batch.NextCursor, batch.NextSequence, scope); werr != nil {
				o.logger.Warn("ingest: append watermark failed (items still durable)",
					zap.String("connector", src.Name()), zap.String("stream", stream), zap.Error(werr))
			}
		}
		wm.Cursor, wm.Sequence = batch.NextCursor, batch.NextSequence
		sr.Cursor, sr.Sequence = wm.Cursor, wm.Sequence

		if batch.Done {
			return sr, nil
		}
	}
}

// sliceLimit is the per-Pull limit: the slice size, clamped to the remaining
// item budget so a backfill never over-pulls past MaxItems.
func sliceLimit(bounds Bounds, budget *runBudget) int {
	if bounds.SliceSize > budget.items {
		return budget.items
	}
	return bounds.SliceSize
}

// applyBatch ingests each item, updating the budget and the StreamReport in
// place. An item-level append failure is logged and skipped (one bad item does
// not fail the job).
func (o *Orchestrator) applyBatch(ctx context.Context, connector, stream string, scope kernel.Scope, items []Item, budget *runBudget, dryRun bool, sr *StreamReport) {
	for i := range items {
		if ctx.Err() != nil || budget.exhausted() {
			return
		}
		budget.items--
		applied, ierr := o.ingestItem(ctx, connector, stream, scope, &items[i], dryRun)
		if ierr != nil {
			o.logger.Warn("ingest: append item failed; continuing",
				zap.String("connector", connector), zap.String("dedupKey", items[i].DedupKey), zap.Error(ierr))
			continue
		}
		if applied {
			sr.Applied++
			budget.calls--
		} else {
			sr.Skipped++
		}
	}
}

// ingestItem turns one Item into an Episode + SourceRef, appended atomically.
// Returns applied=false when dedup/content-hash detect an unchanged item.
func (o *Orchestrator) ingestItem(ctx context.Context, connector, stream string, scope kernel.Scope, item *Item, dryRun bool) (bool, error) {
	existingHash, found, derr := o.reader.SourceRefHash(ctx, item.DedupKey)
	switch {
	case derr != nil:
		o.logger.Warn("ingest: dedup lookup failed; proceeding with append",
			zap.String("dedupKey", item.DedupKey), zap.Error(derr))
	case found && existingHash == item.ContentHash:
		return false, nil // unchanged → idempotent skip
	}
	if dryRun {
		return true, nil // would-apply; preview only, no writes
	}

	epUID, err := o.newUID()
	if err != nil {
		return false, fmt.Errorf("mint episode uid: %w", err)
	}

	// Atomic emit: the episode (with a pre-minted UID the SourceRef references)
	// and its provenance land in one Append batch, so the projection never sees
	// an ingested episode without its SourceRef. The trust stamp + scrub + fence
	// live in BuildConnectorEpisodeEvents, shared with captureExternal.
	events, err := BuildConnectorEpisodeEvents(o.scrubber, connector, stream, scope, epUID, item)
	if err != nil {
		return false, err
	}
	if _, err := o.log.Append(ctx, events); err != nil {
		return false, fmt.Errorf("append episode+sourceRef: %w", err)
	}
	return true, nil
}

// appendWatermark emits a connectorWatermarkAdvanced event (the checkpoint).
func (o *Orchestrator) appendWatermark(ctx context.Context, connector, stream string, cursor json.RawMessage, seq uint64, scope kernel.Scope) error {
	payload, err := json.Marshal(ontology.ConnectorWatermarkAdvancedEvent{
		Connector:   connector,
		Stream:      stream,
		Cursor:      cursor,
		Sequence:    seq,
		AdvanceTime: o.now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("marshal watermark: %w", err)
	}
	if _, err := o.log.Append(ctx, []kernel.Event{{
		Kind:       kernel.EventKind(ontology.KindConnectorWatermarkAdvanced),
		Source:     watermarkEventSource,
		Confidence: 1.0,
		Scope:      scope,
		Payload:    payload,
	}}); err != nil {
		return fmt.Errorf("append watermark: %w", err)
	}
	return nil
}
