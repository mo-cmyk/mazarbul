package dispatcher

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// errEmptyInsightSourceUids rejects an insightSynthesized event with an empty
// SourceUids list. Auditable provenance is the whole point of the cognitive
// layer (issue #231): a derived fact with no cited sources is exactly the
// opaque LLM blob we refuse to ship, so an empty list is a hard decode-time
// error rather than a row.
var errEmptyInsightSourceUids = errors.New("insightSynthesized: sourceUids is required and must be non-empty")

// errEmptyConsolidatedSourceUids rejects a memoryConsolidated event with an
// empty SourceUids list, for the same auditable-provenance reason.
var errEmptyConsolidatedSourceUids = errors.New("memoryConsolidated: sourceUids is required and must be non-empty")

// applyMemoryAccessed projects one memoryAccessed event. It appends the access
// to the durable OR-set (memory_accesses, keyed by the event UID so a Rebuild
// replay is idempotent), re-folds the convergent memory_strength accumulator
// for the target, and re-promotes a previously-demoted insight that was just
// accessed (reinforce-on-access). The decay term is NOT stored — it is
// recomputed at query time from last_accessed_at — so this projection is
// replay-stable: the same access set in any order yields byte-identical state.
func (d *Dispatcher) applyMemoryAccessed(ctx context.Context, ev kernel.Event) (retErr error) {
	var ma ontology.MemoryAccessedEvent
	if err := json.Unmarshal(ev.Payload, &ma); err != nil {
		return fmt.Errorf("decode memoryAccessed payload: %w", err)
	}
	if ma.TargetUID == "" {
		return errors.New("memoryAccessed: targetUid is required")
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

	target := string(ma.TargetUID)
	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO memory_accesses
			(access_event_uid, target_uid, accessed_at, scope)
		 VALUES (?, ?, ?, ?)`,
		ev.UID, target, ma.AccessedAt.UTC().Format(time.RFC3339Nano), scopeValue(ev),
	)
	if err != nil {
		return fmt.Errorf("insert memory access: %w", err)
	}

	// A no-op INSERT OR IGNORE means this exact access event (keyed by event UID)
	// was already folded in — common on Rebuild replay or a redelivered event.
	// The OR-set is unchanged, so the fold result and the re-promote it would
	// drive are identical to the prior application; skip the O(N) recompute
	// rather than rescan the full access history for the target on every replay.
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		committed = true
		return nil
	}

	if err := recomputeMemoryStrength(ctx, tx, target); err != nil {
		return fmt.Errorf("recompute memory strength: %w", err)
	}

	// Reinforce-on-access: a demoted insight that is accessed again returns to
	// hot recall. The decay sweep re-anchors on last_accessed_at (just advanced),
	// so it will not immediately re-demote.
	if _, err := tx.ExecContext(ctx,
		`UPDATE insights SET state = 'hot' WHERE uid = ? AND state = 'demoted'`, target,
	); err != nil {
		return fmt.Errorf("re-promote insight: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// recomputeMemoryStrength loads the full access set for target, folds it via
// ontology.ReduceAccesses, and upserts the materialized memory_strength columns.
// The upsert lists ONLY the folded columns in DO UPDATE SET — salience_base and
// demoted_at are owned by the default / decay sweep respectively and must never
// be clobbered by an access projection.
func recomputeMemoryStrength(ctx context.Context, tx *sql.Tx, target string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT accessed_at FROM memory_accesses WHERE target_uid = ?`, target,
	)
	if err != nil {
		return fmt.Errorf("load access set: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var accesses []ontology.Access
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return fmt.Errorf("scan access: %w", err)
		}
		ts, perr := time.Parse(time.RFC3339Nano, raw)
		if perr != nil {
			return fmt.Errorf("parse accessed_at %q: %w", raw, perr)
		}
		accesses = append(accesses, ontology.Access{AccessedAt: ts})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate access set: %w", err)
	}

	ms := ontology.ReduceAccesses(accesses)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memory_strength
			(target_uid, access_count, first_accessed_at, last_accessed_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(target_uid) DO UPDATE SET
			access_count      = excluded.access_count,
			first_accessed_at = excluded.first_accessed_at,
			last_accessed_at  = excluded.last_accessed_at`,
		target, ms.AccessCount,
		nullableInstant(ms.FirstAccessedAt), nullableInstant(ms.LastAccessedAt),
	); err != nil {
		return fmt.Errorf("upsert memory strength: %w", err)
	}
	return nil
}

// nullableInstant renders an instant as RFC3339Nano, or SQL NULL when zero.
func nullableInstant(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// applyInsightSynthesized projects one insightSynthesized event into the
// insights table and schedules an async embedding so the insight is surfaced by
// the recall insight lane. Current_confidence starts at synthesized_confidence
// and state at 'hot'; the periodic decay sweep mutates those projection columns
// in place (never via the log), so a Rebuild restores the insight to hot recall.
func (d *Dispatcher) applyInsightSynthesized(ctx context.Context, ev kernel.Event) (retErr error) {
	var in ontology.InsightSynthesizedEvent
	if err := json.Unmarshal(ev.Payload, &in); err != nil {
		return fmt.Errorf("decode insightSynthesized payload: %w", err)
	}
	if len(in.SourceUids) == 0 {
		return errEmptyInsightSourceUids
	}
	sourceUids, err := json.Marshal(in.SourceUids)
	if err != nil {
		return fmt.Errorf("marshal sourceUids: %w", err)
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

	// Resolve any redirect so the seed entity FK lands on the surviving entity
	// rather than a merged-away one.
	seedEntityUID := string(in.SeedEntityUID)
	if seedEntityUID != "" {
		resolved, rerr := resolveRedirect(ctx, tx, seedEntityUID)
		if rerr != nil {
			return fmt.Errorf("resolve insight seed entity: %w", rerr)
		}
		seedEntityUID = resolved
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO insights
			(uid, statement, insight_type, source_uids, seed_entity_uid,
			 synthesized_confidence, current_confidence, state, source_event_uid,
			 synthesized_at, create_time, scope)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'hot', ?, ?, ?, ?)`,
		string(in.InsightUID), in.Statement, in.InsightType, string(sourceUids),
		seedEntityUID, in.SynthesizedConfidence, in.SynthesizedConfidence,
		ev.UID, in.SynthesizedAt.UTC().Format(time.RFC3339Nano),
		ev.CreateTime.UTC().Format(time.RFC3339Nano), scopeValue(ev),
	); err != nil {
		return fmt.Errorf("insert insight: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	d.scheduleDomainEmbedding(ctx, sqlite.DomainInsights, string(in.InsightUID), in.Statement, scopeValue(ev))
	return nil
}

// errEmptyDismissedInsightUID rejects an insightDismissed event with no target
// insight uid — the projection has no row to dismiss.
var errEmptyDismissedInsightUID = errors.New("insightDismissed: insightUid is required")

// applyInsightDismissed projects one insightDismissed event by flipping the
// target insight's state to the terminal 'dismissed' value. This is a DISTINCT,
// USER-initiated state, not the projection-side 'demoted' the decay sweep
// produces: the dismissal is carried in the event log, so it survives
// `maz rebuild` (the dismissal event replays after the synthesis event), and it
// is NOT lifted by reinforce-on-access (applyMemoryAccessed re-promotes only
// 'demoted'). Recall already filters state='hot', so a dismissed insight drops
// out of hot recall for an explicit, auditable reason.
//
// The UPDATE no-ops when the insight row is absent (a cross-device reorder that
// delivers the dismissal before the synthesis); on the rebuild path the log is
// replayed in order, so synthesis always precedes dismissal and the projection
// converges. Re-applying the same dismissal is idempotent (the state is already
// 'dismissed').
func (d *Dispatcher) applyInsightDismissed(ctx context.Context, ev kernel.Event) error {
	var dis ontology.InsightDismissedEvent
	if err := json.Unmarshal(ev.Payload, &dis); err != nil {
		return fmt.Errorf("decode insightDismissed payload: %w", err)
	}
	if dis.InsightUID == "" {
		return errEmptyDismissedInsightUID
	}
	if _, err := d.sqlite.Writer().ExecContext(ctx,
		`UPDATE insights SET state = 'dismissed' WHERE uid = ?`, string(dis.InsightUID),
	); err != nil {
		return fmt.Errorf("dismiss insight: %w", err)
	}
	return nil
}

// applyMemoryConsolidated projects one memoryConsolidated event into the
// consolidated_memories table and schedules an async embedding so it is
// surfaced by the recall consolidated lane.
func (d *Dispatcher) applyMemoryConsolidated(ctx context.Context, ev kernel.Event) (retErr error) {
	var cm ontology.MemoryConsolidatedEvent
	if err := json.Unmarshal(ev.Payload, &cm); err != nil {
		return fmt.Errorf("decode memoryConsolidated payload: %w", err)
	}
	if len(cm.SourceUids) == 0 {
		return errEmptyConsolidatedSourceUids
	}
	sourceUids, err := json.Marshal(cm.SourceUids)
	if err != nil {
		return fmt.Errorf("marshal sourceUids: %w", err)
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

	// Resolve any redirect so the cited entity FK lands on the surviving entity.
	citedEntityUID := string(cm.CitedEntityUID)
	if citedEntityUID != "" {
		resolved, rerr := resolveRedirect(ctx, tx, citedEntityUID)
		if rerr != nil {
			return fmt.Errorf("resolve memory cited entity: %w", rerr)
		}
		citedEntityUID = resolved
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO consolidated_memories
			(uid, summary, memory_type, source_uids, cited_entity_uid, confidence,
			 state, source_event_uid, consolidated_at, create_time, scope)
		 VALUES (?, ?, ?, ?, ?, ?, 'hot', ?, ?, ?, ?)`,
		string(cm.MemoryUID), cm.Summary, cm.MemoryType, string(sourceUids),
		citedEntityUID, cm.Confidence, ev.UID,
		cm.ConsolidatedAt.UTC().Format(time.RFC3339Nano),
		ev.CreateTime.UTC().Format(time.RFC3339Nano), scopeValue(ev),
	); err != nil {
		return fmt.Errorf("insert consolidated memory: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	d.scheduleDomainEmbedding(ctx, sqlite.DomainConsolidated, string(cm.MemoryUID), cm.Summary, scopeValue(ev))
	return nil
}

// scheduleDomainEmbedding spawns a detached goroutine that embeds text and
// upserts the vector into the named domain. Mirrors scheduleEpisodeEmbedding:
// it increments d.bgVec on the caller's goroutine so a subsequent Close (and
// the idle reporters) observe the pending work, and warn-logs failures because
// the projection row is durable and a Rebuild can reconstruct the vector index.
// The scope argument stamps the domain sidecar row so scoped KNN can filter
// it; callers pass scopeValue(ev) of the event that produced the projection row.
func (d *Dispatcher) scheduleDomainEmbedding(ctx context.Context, domain, uid, text, scope string) {
	if d.embed == nil || d.vector == nil {
		return
	}
	d.bgVec.Add(1)
	d.bgVecDepth.Add(1)
	logger := loggerFromCtx(ctx, d.logger)
	go func() {
		defer d.bgVec.Done()
		defer d.bgVecDepth.Add(-1)
		bgCtx := context.WithoutCancel(ctx)
		v, err := d.embed.Embed(bgCtx, text)
		if err != nil {
			logger.Warn("dispatcher: background embed failed (row still durable)",
				zap.String("domain", domain), zap.String("uid", uid), zap.Error(err))
			return
		}
		if got := len(v); got != storage.VectorDim {
			logger.Warn("dispatcher: background embed wrong dim",
				zap.String("domain", domain), zap.String("uid", uid),
				zap.Int("got", got), zap.Int("want", storage.VectorDim))
			return
		}
		if err := d.vector.Upsert(bgCtx, []kernel.VectorItem{{
			ID:        uid,
			Embedding: v,
			Metadata:  map[string]any{"domain": domain, "scope": scope},
		}}); err != nil {
			logger.Warn("dispatcher: background vec upsert failed",
				zap.String("domain", domain), zap.String("uid", uid), zap.Error(err))
		}
	}()
}

// CheckDuplicate embeds text and searches the named vector domain for an
// existing item at or above threshold cosine similarity, skipping candidateUID
// (self). It returns the matching UID and a nil vector on a hit, or an empty
// UID and the computed vector when there is no duplicate. It generalizes the
// episode-only checkSemanticDuplicate so the consolidation job can dedup
// candidate summaries against the consolidated domain before appending — no
// orphan derived facts. Returns an error if the dispatcher has no embedder or
// vector connection.
func (d *Dispatcher) CheckDuplicate(ctx context.Context, domain, candidateUID, text string, threshold float64) (string, []float32, error) {
	if d.embed == nil || d.vector == nil {
		return "", nil, errors.New("dispatcher: CheckDuplicate requires an embedder and vector connection")
	}
	vec, err := d.embed.Embed(ctx, text)
	if err != nil {
		return "", nil, fmt.Errorf("embed: %w", err)
	}
	if got := len(vec); got != storage.VectorDim {
		return "", nil, fmt.Errorf("embed: wrong dim got=%d want=%d", got, storage.VectorDim)
	}
	hits, err := d.vector.SearchByDomain(ctx, domain, vec, 5)
	if err != nil {
		return "", nil, fmt.Errorf("vector search: %w", err)
	}
	for _, h := range hits {
		if h.ID == candidateUID {
			continue
		}
		sim := kernel.CosineSimilarityFromDistance(h.Score)
		if float64(sim) >= threshold {
			return h.ID, nil, nil
		}
	}
	return "", vec, nil
}
