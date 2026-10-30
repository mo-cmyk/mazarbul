package dispatcher

// dispatcher_document.go: Apply handlers for documentRevision and
// documentDeleted events. Mirrors the shape of applyEpisode / applyMention in
// dispatcher.go — single writer tx for the relational row + vec sidecar, then
// detached background goroutines per chunk for the embedding + vec0 write.
//
// Documents are first-class in P0 but their graph node and
// wikilink/mention-relation edges are companion Entity + Relation events
// appended in the same batch by the ingest pipeline; the dispatcher handles
// them via the existing applyEntity / applyRelation handlers in order. This
// file covers only the document-specific derived state:
//   - documents table (relational, FTS5 via triggers)
//   - documents_vec_ids (per-chunk sidecar)
//   - documents_vec (vec0, via background embed goroutines)

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/extractor"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// applyDocumentEvent is the single dispatch entry point called from applyLocked
// for document-related event kinds. Keeping both cases in one switch arm in
// applyLocked (and sub-dispatching here) saves one decision point, holding
// applyLocked's cyclomatic complexity at ≤ 15.
func (d *Dispatcher) applyDocumentEvent(ctx context.Context, ev kernel.Event) error {
	switch ev.Kind {
	case kernel.EventKind(ontology.KindDocumentRevision):
		return d.applyDocumentRevision(ctx, ev)
	case kernel.EventKind(ontology.KindDocumentDeleted):
		return d.applyDocumentDeleted(ctx, ev)
	default:
		return fmt.Errorf("dispatcher: applyDocumentEvent: unexpected kind %q", ev.Kind)
	}
}

// applyDocumentRevision persists a documentRevision event to the documents
// table and schedules per-chunk background embeddings. The transactional shape
// mirrors applyEpisode:
//
//  1. Decode payload.
//  2. Open a writer tx.
//  3. UPSERT into documents (ON CONFLICT by document_uid). FTS5 is maintained
//     by the AFTER INSERT / AFTER UPDATE triggers from migration 0016 — the
//     dispatcher does not touch documents_fts directly.
//  4. Delete existing documents_vec + documents_vec_ids rows for this
//     document_uid (idempotent delete-then-insert so re-Apply during Rebuild
//     does not accumulate stale chunk rows).
//  5. Insert fresh sidecar rows into documents_vec_ids (one per chunk).
//  6. Commit.
//  7. Schedule one background goroutine per chunk to embed + write to vec0.
//     Guard on d.embed != nil so unit tests that run without an embedder pass.
//
// Idempotency: the UPSERT on document_uid and the delete-then-insert on the
// sidecar make re-applying the same event safe. HasEvent probes
// documents.revision_uid = ev.UID so a re-Apply of an already-applied
// revision is skipped by Rebuild.
func (d *Dispatcher) applyDocumentRevision(ctx context.Context, ev kernel.Event) (retErr error) {
	var doc ontology.DocumentRevision
	if err := json.Unmarshal(ev.Payload, &doc); err != nil {
		return fmt.Errorf("decode documentRevision payload: %w", err)
	}

	slug := slugFromPath(doc.Path)
	fmJSON := normalizeFrontmatterJSON(doc.Frontmatter)

	pinnedInt := 0
	if doc.Pinned {
		pinnedInt = 1
	}

	canonicalInt := 0
	if canonicalBoolFromFrontmatter(doc.Frontmatter) {
		canonicalInt = 1
	}

	unresolvedJSON, err := marshalUnresolvedLinks(doc.Wikilinks)
	if err != nil {
		return err
	}

	now := ev.CreateTime.UTC().Format(time.RFC3339Nano)
	chunks := extractor.ChunkText(doc.Body, 0, 0)

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

	// Monotonic-revision guard: skip if a STRICTLY newer revision is already
	// projected. Without it, Rebuild (or an out-of-order re-Append) replaying an
	// older revision after a newer one would UPSERT the row back to the older
	// body and rebuild the vec sidecar from stale chunks. HasEvent only matches
	// the exact latest revision_uid, so it cannot catch an older sibling event.
	// Re-applying the CURRENT revision (equal UID) is allowed through ONLY for
	// live rows, so the idempotent sidecar rebuild can repair a partially lost
	// derived store; on a tombstoned row the same replay is skipped — the
	// trailing documentDeleted is the newer fact and the replay must not
	// resurrect the document. The deferred rollback releases the read-only tx
	// on the skip path.
	if stale, err := documentRevisionIsStale(ctx, tx, doc.DocumentUID, ev.UID); err != nil {
		return err
	} else if stale {
		return nil
	}

	// UPSERT the documents row. ON CONFLICT(document_uid) refreshes the mutable
	// columns but preserves create_time (first-insert creation timestamp) AND
	// slug: the slug is frozen at first sight so a later rename (which changes
	// path) does not move the rendered/documents/<slug>.md file in git or break
	// inbound resource links. moveDocument (P2) is the only path that re-slugs.
	const q = `
		INSERT INTO documents
			(document_uid, path, slug, title, body, frontmatter_json, frontmatter_raw,
			 content_hash, revision_uid, pinned, canonical, create_time, update_time,
			 unresolved_links_json, deleted, scope)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT(document_uid) DO UPDATE SET
			path                  = excluded.path,
			title                 = excluded.title,
			body                  = excluded.body,
			frontmatter_json      = excluded.frontmatter_json,
			frontmatter_raw       = excluded.frontmatter_raw,
			content_hash          = excluded.content_hash,
			revision_uid          = excluded.revision_uid,
			pinned                = excluded.pinned,
			canonical             = excluded.canonical,
			update_time           = excluded.update_time,
			unresolved_links_json = excluded.unresolved_links_json,
			deleted               = 0,
			scope                 = excluded.scope
	`
	if _, err := tx.ExecContext(ctx, q,
		doc.DocumentUID,
		doc.Path,
		slug,
		doc.Title,
		doc.Body,
		fmJSON,
		doc.FrontmatterRaw,
		doc.ContentHash,
		ev.UID, // revision_uid = the event UID
		pinnedInt,
		canonicalInt,
		now,            // create_time (kept by ON CONFLICT; set on first insert only)
		now,            // update_time (always refreshed)
		unresolvedJSON, // unresolved_links_json (write-time snapshot of dangling links)
		scopeValue(ev),
	); err != nil {
		return fmt.Errorf("upsert document row: %w", err)
	}

	// Delete existing vec sidecar rows for this document before re-inserting
	// fresh ones. Idempotent: missing rows are a silent no-op.
	if err := deleteDocumentVecSidecar(ctx, tx, doc.DocumentUID); err != nil {
		return fmt.Errorf("delete old document vec sidecar: %w", err)
	}

	// Insert one sidecar row per chunk. The background embedder writes the
	// vec0 row after the tx commits; sidecar rows must pre-exist so the worker
	// can look up the stable rowid via SELECT rowid FROM documents_vec_ids.
	if err := insertDocumentVecSidecars(ctx, tx, doc.DocumentUID, doc.Body, chunks); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	d.scheduleDocumentChunkEmbeddings(ctx, doc.DocumentUID, chunks)
	return nil
}

// scheduleDocumentChunkEmbeddings schedules one detached embed goroutine per
// body chunk, guarding on d.embed so tests that omit an embedder
// (Config.Embedder=nil) are a no-op. Split out of applyDocumentRevision to keep
// that handler under the gocyclo threshold.
func (d *Dispatcher) scheduleDocumentChunkEmbeddings(ctx context.Context, documentUID string, chunks []extractor.Chunk) {
	if d.embed == nil {
		return
	}
	for i := range chunks {
		chunkID := fmt.Sprintf("%s#%d", documentUID, i)
		d.scheduleDocumentChunkEmbedding(ctx, chunkID, chunks[i].Text)
	}
}

// marshalUnresolvedLinks serializes the write-time snapshot of dangling
// wikilinks for the documents.unresolved_links_json column. An empty set
// stores the canonical "[]" so the column never holds NULL or "null".
func marshalUnresolvedLinks(links []ontology.WikiLink) (string, error) {
	if len(links) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(links)
	if err != nil {
		return "", fmt.Errorf("marshal unresolved wikilinks: %w", err)
	}
	return string(b), nil
}

// applyDocumentDeleted applies a soft-delete tombstone to the documents table.
// It sets deleted=1 (preserving the row for Rebuild idempotency) and removes
// the vec sidecar rows so recall lanes stop surfacing the document. The AFTER
// UPDATE trigger on documents drops the row from the FTS5 index when deleted=1
// (it re-indexes only live rows), so the body stops matching full-text search.
//
// Idempotent under replay: re-applying the delete just re-sets deleted=1;
// deleteDocumentVecSidecar on already-cleared rows is a no-op. A delete that
// matches no row is logged (see below) rather than silently swallowed.
func (d *Dispatcher) applyDocumentDeleted(ctx context.Context, ev kernel.Event) (retErr error) {
	var doc ontology.DocumentDeleted
	if err := json.Unmarshal(ev.Payload, &doc); err != nil {
		return fmt.Errorf("decode documentDeleted payload: %w", err)
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

	// Soft-delete: set deleted=1. The AFTER UPDATE trigger removes the row from
	// the FTS index (it re-indexes only when new.deleted=0), so a tombstoned
	// document stops surfacing in full-text search. We do NOT delete the row —
	// the tombstone is needed by Rebuild to deterministically reconstruct the
	// deleted state and keeps the path/UID reserved.
	res, err := tx.ExecContext(ctx,
		`UPDATE documents SET deleted = 1, update_time = ? WHERE document_uid = ?`,
		ev.CreateTime.UTC().Format(time.RFC3339Nano),
		doc.DocumentUID,
	)
	if err != nil {
		return fmt.Errorf("soft-delete document: %w", err)
	}
	// A zero rows-affected means there is no projected row to tombstone. In a
	// well-formed event log a documentDeleted always trails the document's
	// revisions, so the row exists; a miss here signals a malformed or
	// out-of-order log (delete replayed before any revision). We do NOT
	// fabricate a tombstone row — the documents table requires NOT NULL
	// path/content_hash/revision_uid with no trustworthy values to supply, and a
	// synthetic row would mask the corruption. Surface it instead of swallowing.
	if n, raErr := res.RowsAffected(); raErr == nil && n == 0 {
		loggerFromCtx(ctx, d.logger).Warn(
			"dispatcher: documentDeleted for unknown document; no row to tombstone (malformed or out-of-order log)",
			zap.String("documentUID", doc.DocumentUID),
		)
	}

	// Remove vec sidecar + vec0 rows. These are not preserved as tombstones
	// because they hold no lineage information beyond the documents row itself.
	if err := deleteDocumentVecSidecar(ctx, tx, doc.DocumentUID); err != nil {
		return fmt.Errorf("delete vec sidecar for deleted document: %w", err)
	}

	// Remove the document entity's name-embedding vector rows from entities_vec
	// and entities_vec_ids. The companion Entity event (written by the ingest
	// pipeline alongside the documentRevision) places name and summary rows in
	// entities_vec_ids keyed by the documentUID. Without this step the nameEmb
	// recall lane continues to surface the deleted document (observed as ~0.23
	// cosine similarity in maz recall). Two-step: collect sidecar rowids first
	// (vec0 does not support correlated subquery in DELETE), then delete from
	// vec0, then delete sidecar rows.
	if err := deleteEntityVecSidecar(ctx, tx, doc.DocumentUID); err != nil {
		return fmt.Errorf("delete entity vec sidecar for deleted document: %w", err)
	}

	// Mark the entity node as deleted so callers filtering on resolution_status
	// can exclude it. A WHERE-0-rows is benign: the entity row may not exist if
	// the ingest pipeline has not yet emitted the companion Entity event (unusual
	// but safe to tolerate).
	if _, err := tx.ExecContext(ctx,
		`UPDATE entities SET resolution_status = 'deleted' WHERE uid = ?`,
		doc.DocumentUID,
	); err != nil {
		return fmt.Errorf("mark entity deleted for document: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// deleteEntityVecSidecar removes all entities_vec and entities_vec_ids rows
// for entityUID inside the caller's transaction. Used by applyDocumentDeleted
// to clear the nameEmb recall lane for a tombstoned document entity. Mirrors
// deleteDocumentVecSidecar: two-step (rowids first, then vec0 delete by rowid,
// then sidecar delete) because vec0 does not accept correlated subqueries in
// DELETE. Missing rows are silently ignored (idempotent).
func deleteEntityVecSidecar(ctx context.Context, tx *sql.Tx, entityUID string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT rowid FROM entities_vec_ids WHERE id = ?`, entityUID,
	)
	if err != nil {
		return fmt.Errorf("query entity vec sidecar rowids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var rowids []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			return fmt.Errorf("scan entity vec sidecar rowid: %w", err)
		}
		rowids = append(rowids, rowid)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate entity vec sidecar rows: %w", err)
	}

	for _, rowid := range rowids {
		// vec0 returns an error when the row is missing; treat as best-effort.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entities_vec WHERE rowid = ?`, rowid,
		); err != nil {
			_ = err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entities_vec_ids WHERE id = ?`, entityUID,
	); err != nil {
		return fmt.Errorf("delete entity vec sidecar rows: %w", err)
	}
	return nil
}

// scheduleDocumentChunkEmbedding spawns a detached goroutine that embeds one
// body chunk and writes the resulting vec0 row for chunkID. Mirrors
// scheduleEpisodeEmbedding: increments d.bgVec on the caller's goroutine so
// a subsequent Close observes the work even if the worker has not started yet.
// Errors are warn-logged; the document row and FTS entry are already durable,
// and Rebuild re-schedules embeddings from the JSONL log.
func (d *Dispatcher) scheduleDocumentChunkEmbedding(ctx context.Context, chunkID, text string) {
	d.bgVec.Add(1)
	d.bgVecDepth.Add(1)
	logger := loggerFromCtx(ctx, d.logger)
	go func() {
		defer d.bgVec.Done()
		defer d.bgVecDepth.Add(-1)
		bgCtx := context.WithoutCancel(ctx)
		v, err := d.embed.Embed(bgCtx, text)
		if err != nil {
			logger.Warn("dispatcher: background document chunk embed failed (document still durable)",
				zap.String("chunkID", chunkID),
				zap.Error(err),
			)
			return
		}
		if got := len(v); got != storage.VectorDim {
			logger.Warn("dispatcher: background document chunk embed wrong dim",
				zap.String("chunkID", chunkID),
				zap.Int("got", got),
				zap.Int("want", storage.VectorDim),
			)
			return
		}
		if err := d.writeDocumentChunkVectorAsync(bgCtx, chunkID, v); err != nil {
			logger.Warn("dispatcher: background document chunk vec upsert failed",
				zap.String("chunkID", chunkID),
				zap.Error(err),
			)
		}
	}()
}

// writeDocumentChunkVectorAsync opens a fresh writer tx and upserts the vec0
// row for chunkID. The sidecar row for chunkID must already exist (inserted
// during applyDocumentRevision); this function just writes the embedding to
// the matching vec0 slot. Mirrors writeEpisodeVectorAsync; short-circuits
// against d.closed so the worker never writes through a torn-down pool.
func (d *Dispatcher) writeDocumentChunkVectorAsync(ctx context.Context, chunkID string, embedding []float32) (retErr error) {
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

	blob, err := sqlite_vec.SerializeFloat32(embedding)
	if err != nil {
		return fmt.Errorf("serialize embedding: %w", err)
	}

	var rowid int64
	if err := tx.QueryRowContext(ctx,
		`SELECT rowid FROM documents_vec_ids WHERE id = ?`, chunkID,
	).Scan(&rowid); err != nil {
		return fmt.Errorf("lookup document chunk sidecar rowid for %s: %w", chunkID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM documents_vec WHERE rowid = ?`, rowid,
	); err != nil {
		return fmt.Errorf("clear document chunk vec slot: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO documents_vec(rowid, embedding) VALUES (?, ?)`, rowid, blob,
	); err != nil {
		return fmt.Errorf("write document chunk vec slot: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// deleteDocumentVecSidecar removes all documents_vec rows and documents_vec_ids
// rows for documentUID inside the caller's transaction. Called before inserting
// fresh chunk sidecar rows (applyDocumentRevision re-apply idempotency) and on
// soft-delete (applyDocumentDeleted). Missing rows are silently ignored so the
// helper is idempotent.
//
// Two-step: collect rowids from the sidecar first (vec0 does not accept
// correlated subqueries in DELETE), then DELETE from vec0 by rowid, then
// DELETE from the sidecar by document_uid.
func deleteDocumentVecSidecar(ctx context.Context, tx *sql.Tx, documentUID string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT rowid FROM documents_vec_ids WHERE document_uid = ?`, documentUID,
	)
	if err != nil {
		return fmt.Errorf("query document vec sidecar rowids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var rowids []int64
	for rows.Next() {
		var rowid int64
		if err := rows.Scan(&rowid); err != nil {
			return fmt.Errorf("scan document vec sidecar rowid: %w", err)
		}
		rowids = append(rowids, rowid)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate document vec sidecar rows: %w", err)
	}

	for _, rowid := range rowids {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM documents_vec WHERE rowid = ?`, rowid,
		); err != nil {
			// vec0 returns an error when the row is missing (already deleted or
			// never embedded). Treat as best-effort: the sidecar delete below
			// cleans up the id mapping regardless.
			_ = err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM documents_vec_ids WHERE document_uid = ?`, documentUID,
	); err != nil {
		return fmt.Errorf("delete document vec sidecar rows: %w", err)
	}
	return nil
}

// canonicalBoolFromFrontmatter extracts the `canonical` boolean field from a
// documentRevision frontmatter payload. Returns false when the key is absent,
// not a boolean (or boolean-like string), or when JSON decoding fails. Mirrors
// the frontmatterBool semantics in internal/document/parse.go so the dispatcher
// and the parser agree on what values count as true.
func canonicalBoolFromFrontmatter(frontmatter json.RawMessage) bool {
	if len(frontmatter) == 0 {
		return false
	}
	var fm map[string]any
	if err := json.Unmarshal(frontmatter, &fm); err != nil {
		return false
	}
	switch v := fm["canonical"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

// normalizeFrontmatterJSON coerces a documentRevision's frontmatter payload to
// the documents.frontmatter_json column contract: a syntactically valid JSON
// *object*, defaulting to "{}" when absent. A JSON array, scalar, or malformed
// bytes (possible from a hand-crafted event outside the P1 parser, which always
// emits an object) fall back to "{}" so json_each(frontmatter_json) tag queries
// in navigate/recall never fault on a non-object root.
func normalizeFrontmatterJSON(frontmatter json.RawMessage) string {
	if raw := strings.TrimSpace(string(frontmatter)); len(raw) > 0 && raw[0] == '{' && json.Valid(frontmatter) {
		return string(frontmatter)
	}
	return "{}"
}

// documentRevisionIsStale reports whether applying evUID would roll the
// projection for documentUID backwards. Two cases are stale:
//
//   - A STRICTLY newer revision is already projected (current > evUID):
//     applying evUID would roll the row (and its vec sidecar) back to older
//     content.
//   - The row is tombstoned (deleted=1) and evUID equals the projected
//     revision_uid: applyDocumentDeleted keeps revision_uid at the last
//     revision, so replaying that same revision would pass an equality-blind
//     guard, hit the UPSERT, and resurrect the deleted document (deleted=0 +
//     fresh sidecar/FTS state). The tombstone is the newer fact; only a
//     strictly newer revision (an intentional recreate) may clear it.
//
// An EQUAL revision_uid on a LIVE row is deliberately NOT stale: re-applying
// the current revision must stay a repair path — if the row survived a partial
// derived-store loss but the vec sidecar did not, replaying the same event
// re-runs the delete-and-insert sidecar rebuild and re-schedules chunk
// embeddings (all idempotent). ULIDs sort lexicographically by creation time,
// so a string compare is a valid recency test. Returns (false, nil) when no
// row exists yet (first revision).
func documentRevisionIsStale(ctx context.Context, tx *sql.Tx, documentUID, evUID string) (bool, error) {
	var (
		current sql.NullString
		deleted int
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT revision_uid, deleted FROM documents WHERE document_uid = ?`, documentUID,
	).Scan(&current, &deleted); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read current revision_uid: %w", err)
	case !current.Valid:
		return false, nil
	case current.String > evUID:
		return true, nil
	default:
		return current.String == evUID && deleted != 0, nil
	}
}

// documentRevisionApplied reports whether the documentRevision event uid is
// FULLY projected, not merely row-present. HasEvent calls this after its cheap
// revision_uid probe matched; returning true means Rebuild may skip Apply.
// The per-layer rule mirrors the episode check in HasEvent:
//
//   - The documents row must point at uid (revision_uid = uid).
//   - A LIVE row (deleted = 0) must have its chunk sidecar rows: ChunkText
//     returns at least one chunk for every body (even an empty one), so zero
//     documents_vec_ids rows on a live row always means the sidecar was lost,
//     never a legitimately chunkless document.
//   - When an embedder is wired, every sidecar row must have its vec0 row —
//     recall joins documents_vec_ids to documents_vec by rowid, so a missing
//     embedding makes the chunk invisible to the vector lane and Apply must
//     re-run to re-schedule it.
//   - A TOMBSTONED row reports true: applyDocumentDeleted removes the sidecar
//     by design, and a replay of the trailing revision would be skipped by
//     documentRevisionIsStale anyway.
//
// Anything less than this check would short-circuit Rebuild before the
// same-revision repair path in applyDocumentRevision could run, leaving a
// partially lost derived store unrepairable from the event log. Apply is
// idempotent at every layer, so a false negative here costs at worst one
// redundant transaction. Runs on the read-only pool like the rest of
// HasEvent; the caller already holds d.mu.RLock.
func (d *Dispatcher) documentRevisionApplied(ctx context.Context, uid string) (bool, error) {
	const q = `
		SELECT
			doc.deleted,
			(SELECT count(*) FROM documents_vec_ids AS i
			 WHERE i.document_uid = doc.document_uid) AS sidecar_count,
			(SELECT count(*) FROM documents_vec_ids AS i
			 JOIN documents_vec AS v ON v.rowid = i.rowid
			 WHERE i.document_uid = doc.document_uid) AS vec_count
		FROM documents AS doc
		WHERE doc.revision_uid = ?
	`
	var (
		deleted                int
		sidecarCount, vecCount int
	)
	switch err := d.sqlite.Reader().QueryRowContext(ctx, q, uid).Scan(&deleted, &sidecarCount, &vecCount); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("dispatcher: documentRevisionApplied uid=%s: %w", uid, err)
	}
	if deleted != 0 {
		return true, nil
	}
	if sidecarCount == 0 {
		return false, nil
	}
	if d.embed != nil && vecCount < sidecarCount {
		return false, nil
	}
	return true, nil
}

// insertDocumentVecSidecars inserts one documents_vec_ids row per body chunk
// inside the caller's transaction. Each row records the chunk's stable string
// id ("<documentUid>#<index>"), its nearest preceding heading, and byte offset
// so the background embedder can look up the rowid and P3 can render section
// anchors. The upsert keeps re-apply idempotent (the prior rows were just
// cleared by deleteDocumentVecSidecar) while refreshing scope from the base
// documents row, which is always written before the sidecars.
func insertDocumentVecSidecars(ctx context.Context, tx *sql.Tx, documentUID, body string, chunks []extractor.Chunk) error {
	for i := range chunks {
		chunk := &chunks[i]
		chunkID := fmt.Sprintf("%s#%d", documentUID, i)
		heading := findNearestHeading(body, chunk.ByteOffset)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO documents_vec_ids
				(id, document_uid, chunk_index, heading, byte_offset, scope)
			 VALUES (?, ?, ?, ?, ?,
				COALESCE((SELECT scope FROM documents WHERE document_uid = ?), 'personal'))
			 ON CONFLICT(id) DO UPDATE SET scope = excluded.scope`,
			chunkID, documentUID, i, heading, chunk.ByteOffset, documentUID,
		); err != nil {
			return fmt.Errorf("insert document vec sidecar chunk %d: %w", i, err)
		}
	}
	return nil
}

// slugFromPath derives a URL-safe slug from a vault-relative file path.
// Algorithm: take the base filename, strip the extension, ASCII-lowercase,
// replace runs of non-alphanumeric characters with "-", trim leading/trailing
// "-", fall back to "document" for empty results.
//
// The slug is derived at first revision apply time by the dispatcher; it is
// stored in documents.slug and frozen there (subsequent renames update
// documents.path but a moveDocument event also resets documents.slug — P2).
func slugFromPath(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	if base == "" || base == "." {
		return "document"
	}
	var b strings.Builder
	prevHyphen := false
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(unicode.ToLower(r))
			prevHyphen = false
		} else {
			if !prevHyphen {
				b.WriteRune('-')
			}
			prevHyphen = true
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		return "document"
	}
	return result
}

// findNearestHeading scans backwards from byteOffset in body to find the
// nearest preceding line that begins with '#' (a markdown heading). Returns
// the trimmed heading text without the '#' prefix, or "" if no heading precedes
// byteOffset. Used to populate the heading column in documents_vec_ids so P3
// citations can render path#heading section anchors.
//
// The scan is line-based: split body[:byteOffset] on '\n' and walk backwards.
// The first line starting with '#' is the nearest heading.
func findNearestHeading(body string, byteOffset int) string {
	if byteOffset > len(body) {
		byteOffset = len(body)
	}
	if byteOffset == 0 {
		return ""
	}
	prefix := body[:byteOffset]
	lines := strings.Split(prefix, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], "\r")
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
	}
	return ""
}
