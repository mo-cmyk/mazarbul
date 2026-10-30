package dispatcher

// scope_stage.go implements the dispatcher projections for scopePromoted and
// scopeDiscarded events (draft staging workflow, Lane F).
//
// Security contract:
//   - applyScopePromoted enforces fromScope == "draft" before any UPDATE.
//     A scopePromoted event with any other fromScope is silently ignored (no
//     error, no write), so it can NEVER be used to demote a team→personal or
//     rewrite any non-draft scope.
//   - applyScopePromoted permits ONLY toScope ∈ {personal, project}. Promotion
//     into "team" is refused (silent no-op): pushing unreviewed draft data
//     straight into a shared scope is the highest-risk leak, so team writes
//     must go through the explicit team grant/membership path, never a draft
//     promotion. Forbidden/malformed toScope values are silently ignored
//     rather than erroring, because a returned error aborts the whole dispatch
//     loop (dispatcher.go:576) — a single crafted event must never be able to
//     brick the projection.
//   - applyScopeDiscarded enforces scope == "draft" on every DELETE, so it
//     cannot accidentally delete live personal/project/team rows. The
//     provenance DELETE is gated on the target still being a draft episode so a
//     stray discard event can never strip a promoted (live) episode's
//     provenance. Draft entities resolved from the discarded episode (and a
//     draft entity targeted directly) are removed too, with their vector
//     sidecars, so discard leaves no orphaned draft data behind.
//
// Rebuild-deterministic: on a full log replay the original draft event
// projects the row, then these events replay after and apply the same
// mutations, converging to the same derived-store state.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// applyScopePromoted handles a scopePromoted event: it re-scopes the target
// episode and all derived rows sourced by it from "draft" to toScope.
//
// SECURITY GATE: if fromScope != "draft" the event is silently ignored. This
// prevents scopePromoted from being used to downscope team→personal or rewrite
// any non-draft scope. Idempotent: a second apply is a harmless no-op.
func (d *Dispatcher) applyScopePromoted(ctx context.Context, ev kernel.Event) error {
	var payload ontology.ScopePromotedEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("decode scopePromoted payload: %w", err)
	}
	if payload.TargetUID == "" {
		return errors.New("scopePromoted: missing targetUid")
	}

	// HARD SECURITY GATE: only allow promotion OUT of draft.
	if payload.FromScope != string(kernel.ScopeDraft) {
		d.logger.Debug("dispatcher: scopePromoted ignored (fromScope is not draft)",
			zap.String("target_uid", payload.TargetUID),
			zap.String("from_scope", payload.FromScope),
		)
		return nil
	}

	// HARD SECURITY GATE: draft may be promoted ONLY into the local personal or
	// project scope. "team" (and any other value) is refused as a silent no-op:
	// promoting unreviewed draft data into a shared team scope is the highest-risk
	// leak, and team writes must go through the explicit team grant/membership
	// path — never a draft promotion. We log + return nil rather than error,
	// because an error here aborts the entire dispatch loop (a crafted event must
	// not be able to brick the projection on replay).
	toScope := payload.ToScope
	s := kernel.Scope(toScope)
	if s != kernel.ScopePersonal && s != kernel.ScopeProject {
		d.logger.Warn("dispatcher: scopePromoted ignored (toScope is not a permitted promotion target)",
			zap.String("target_uid", payload.TargetUID),
			zap.String("to_scope", toScope),
		)
		return nil
	}

	targetUID := payload.TargetUID

	// Single SQLite writer (MaxOpenConns=1): no transaction needed for this
	// UPDATE batch because each statement is individually atomic and order-
	// independent — the worst case on a failure mid-batch is a re-apply on the
	// next Rebuild, and every statement is idempotent (scope = ? WHERE scope = 'draft').
	w := d.sqlite.Writer()

	// Promote the episode itself.
	if _, err := w.ExecContext(ctx,
		`UPDATE episodes SET scope = ? WHERE uid = ? AND scope = 'draft'`,
		toScope, targetUID,
	); err != nil {
		return fmt.Errorf("scopePromoted: update episodes: %w", err)
	}

	// Cascade to derived rows sourced from this episode.
	if _, err := w.ExecContext(ctx,
		`UPDATE mentions SET scope = ? WHERE source_event_uid = ? AND scope = 'draft'`,
		toScope, targetUID,
	); err != nil {
		return fmt.Errorf("scopePromoted: update mentions: %w", err)
	}
	if _, err := w.ExecContext(ctx,
		`UPDATE relations SET scope = ? WHERE source_event_uid = ? AND scope = 'draft'`,
		toScope, targetUID,
	); err != nil {
		return fmt.Errorf("scopePromoted: update relations: %w", err)
	}
	if _, err := w.ExecContext(ctx,
		`UPDATE facts SET scope = ? WHERE source_event_uid = ? AND scope = 'draft'`,
		toScope, targetUID,
	); err != nil {
		return fmt.Errorf("scopePromoted: update facts: %w", err)
	}

	// Also promote entity rows whose uid matches targetUID (an entity event
	// may itself be draft-scoped and targeted by name). No-op when the uid
	// is not an entity.
	if _, err := w.ExecContext(ctx,
		`UPDATE entities SET scope = ? WHERE uid = ? AND scope = 'draft'`,
		toScope, targetUID,
	); err != nil {
		return fmt.Errorf("scopePromoted: update entities: %w", err)
	}

	// Symmetric to the discard path: promote draft entities that were resolved
	// from this episode's mentions, so an entity first seen in a draft remember
	// becomes visible alongside its episode. The subquery keys on the immutable
	// source_event_uid (not mentions.scope, already updated above), so it is
	// order-independent; the scope = 'draft' gate guarantees an entity also
	// referenced by a live episode (hence non-draft) is never touched.
	if _, err := w.ExecContext(ctx,
		`UPDATE entities SET scope = ?
		   WHERE scope = 'draft'
		     AND uid IN (SELECT entity_uid FROM mentions
		                  WHERE source_event_uid = ? AND entity_uid IS NOT NULL)`,
		toScope, targetUID,
	); err != nil {
		return fmt.Errorf("scopePromoted: update mentioned entities: %w", err)
	}

	return nil
}

// applyScopeDiscarded handles a scopeDiscarded event: it removes the
// draft-scoped episode row and all draft-scoped derived rows sourced from it.
//
// SECURITY GATE: all DELETEs are gated on scope = 'draft', so a scopeDiscarded
// event cannot remove live personal/project/team rows. Idempotent: applying
// the event when the draft row has already been removed (or after promotion)
// is a no-op.
func (d *Dispatcher) applyScopeDiscarded(ctx context.Context, ev kernel.Event) (retErr error) {
	var payload ontology.ScopeDiscardedEvent
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("decode scopeDiscarded payload: %w", err)
	}
	if payload.TargetUID == "" {
		return errors.New("scopeDiscarded: missing targetUid")
	}

	targetUID := payload.TargetUID

	// All deletes run in one transaction so the vector-sidecar cleanup (which
	// reads rowids before deleting) stays consistent with the row deletes. The
	// single SQLite writer (MaxOpenConns=1) serialises writers, so BeginTx never
	// contends. The LadybugDB graph drops run BEFORE Commit (mirroring
	// applyEpisodeForgotten): if a Cypher delete fails, the SQLite rollback
	// restores the rows so a retry of this same tombstone can re-read them.
	tx, err := d.sqlite.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("scopeDiscarded: begin tx: %w", err)
	}
	defer func() {
		if retErr != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("rollback: %w", rbErr))
			}
		}
	}()

	// SECURITY GATE: the episode-scoped footprint (provenance, mentions,
	// relations, facts, the episode row + its vector, and the graph episode node
	// + RELATES_TO edges) is removed ONLY when the target is still a DRAFT
	// episode. A stray discard naming a promoted (now personal/project) episode
	// therefore cannot strip that live episode's provenance, vector, or graph
	// footprint. Draft entities (below) carry their own scope='draft' gate.
	isDraftEpisode, err := targetIsDraftEpisode(ctx, tx, targetUID)
	if err != nil {
		return fmt.Errorf("scopeDiscarded: %w", err)
	}

	// Remove draft entities FIRST — the target itself (a createEntity --scope
	// draft) or an entity resolved from this episode's mentions. Its via-mentions
	// arm reads the mentions rows, so it must run before discardDraftEpisodeFootprint
	// deletes them. Returns the deleted uids so their graph nodes can be detached.
	entityUIDs, err := discardDraftEntities(ctx, tx, targetUID)
	if err != nil {
		return fmt.Errorf("scopeDiscarded: %w", err)
	}

	var draftRelationUIDs []string
	if isDraftEpisode {
		draftRelationUIDs, err = d.discardDraftEpisodeFootprint(ctx, tx, targetUID)
		if err != nil {
			return fmt.Errorf("scopeDiscarded: %w", err)
		}
	}

	// Drop the graph footprint BEFORE Commit so a Cypher failure rolls back the
	// SQLite deletes (forget's invariant). The graph is NOT scope-filtered, so
	// without this a discarded draft's episode/relations/entities would remain
	// reachable via traverse / neighbors / graph recall.
	if isDraftEpisode {
		if gerr := d.dropEpisodeGraphFootprint(ctx, targetUID, draftRelationUIDs); gerr != nil {
			return fmt.Errorf("scopeDiscarded: %w", gerr)
		}
	}
	if gerr := d.detachDraftEntityNodes(ctx, entityUIDs); gerr != nil {
		return fmt.Errorf("scopeDiscarded: %w", gerr)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("scopeDiscarded: commit: %w", err)
	}
	return nil
}

// targetIsDraftEpisode reports whether targetUID names a row in episodes that is
// still draft-scoped — the precondition for removing the episode-scoped footprint.
func targetIsDraftEpisode(ctx context.Context, tx *sql.Tx, targetUID string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM episodes WHERE uid = ? AND scope = 'draft'`, targetUID,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("probe draft episode: %w", err)
	}
	return n > 0, nil
}

// discardDraftEpisodeFootprint removes the SQLite footprint of a draft episode —
// provenance, mentions, relations, facts, the episode's vector sidecar, and the
// episode row — all inside tx. It returns the uids of the draft relations it
// removed so the caller can drop the matching RELATES_TO graph edges. Caller has
// already confirmed targetUID is a draft episode, so the scope='draft' gates on
// the derived rows are belt-and-suspenders.
func (d *Dispatcher) discardDraftEpisodeFootprint(ctx context.Context, tx *sql.Tx, targetUID string) ([]string, error) {
	// Collect draft relation uids BEFORE the relations DELETE, for the graph drop.
	relUIDs, err := collectDraftRelationUIDs(ctx, tx, targetUID)
	if err != nil {
		return nil, fmt.Errorf("collect draft relations: %w", err)
	}

	// FK ordering: provenance + mentions reference episodes(uid); delete them
	// before the episode row. relations/facts have no FK to episodes.
	if _, err := tx.ExecContext(ctx, `DELETE FROM provenance WHERE event_uid = ?`, targetUID); err != nil {
		return nil, fmt.Errorf("delete provenance: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM mentions WHERE source_event_uid = ? AND scope = 'draft'`, targetUID); err != nil {
		return nil, fmt.Errorf("delete mentions: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM relations WHERE source_event_uid = ? AND scope = 'draft'`, targetUID); err != nil {
		return nil, fmt.Errorf("delete relations: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM facts WHERE source_event_uid = ? AND scope = 'draft'`, targetUID); err != nil {
		return nil, fmt.Errorf("delete facts: %w", err)
	}
	// Episode vector sidecar — best-effort: vec0 raises on a missing row, which is
	// expected for a never-embedded (embed=none) draft. Mirrors applyEpisodeForgotten.
	if _, err := tx.ExecContext(ctx, `DELETE FROM episodes_vec WHERE uid = ?`, targetUID); err != nil {
		d.logger.Debug("dispatcher: scopeDiscarded episodes_vec delete returned error (likely missing row)",
			zap.String("episode_uid", targetUID), zap.Error(err))
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM episodes WHERE uid = ? AND scope = 'draft'`, targetUID); err != nil {
		return nil, fmt.Errorf("delete episode: %w", err)
	}
	return relUIDs, nil
}

// collectDraftRelationUIDs returns the uids of draft-scoped relations sourced by
// the target episode. Scoped (unlike collectRelationUIDs) so the graph-edge drop
// can never touch a non-draft relation.
func collectDraftRelationUIDs(ctx context.Context, tx *sql.Tx, targetUID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT uid FROM relations WHERE source_event_uid = ? AND scope = 'draft'`, targetUID)
	if err != nil {
		return nil, fmt.Errorf("query draft relations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var uids []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("scan draft relation uid: %w", err)
		}
		uids = append(uids, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate draft relation uids: %w", err)
	}
	return uids, nil
}

// detachDraftEntityNodes DETACH DELETEs the :Entity graph nodes for the given
// discarded draft entities (and any incident edges). No-op when the graph is
// unconfigured. Each node is dropped by uid; a missing node is a zero-row no-op.
func (d *Dispatcher) detachDraftEntityNodes(ctx context.Context, entityUIDs []string) error {
	if d.graph == nil {
		return nil
	}
	for _, uid := range entityUIDs {
		const cypher = `MATCH (e:Entity {uid: $uid}) DETACH DELETE e`
		r, err := d.graph.Execute(ctx, cypher, map[string]any{"uid": uid})
		if err != nil {
			return fmt.Errorf("cypher detach delete draft entity node %s: %w", uid, err)
		}
		if cerr := r.Close(); cerr != nil {
			return fmt.Errorf("close cypher result: %w", cerr)
		}
	}
	return nil
}

// discardDraftEntities removes the draft entities tied to a scopeDiscarded:
// the target itself (when it is a draft entity created via createEntity) and
// any draft entity resolved from a mention of the target episode. Each entity's
// vector sidecar is cleared before the row is deleted; entity_identifiers cascade
// on the entity delete (ON DELETE CASCADE). The scope = 'draft' gate guarantees
// an entity also referenced by a live episode (hence non-draft) is never touched.
// Returns the uids it deleted so the caller can drop their graph nodes. Must run
// before the episode's mentions are deleted, since the via-mentions arm reads them.
func discardDraftEntities(ctx context.Context, tx *sql.Tx, targetUID string) ([]string, error) {
	uids, err := collectDiscardableEntityUIDs(ctx, tx, targetUID)
	if err != nil {
		return nil, fmt.Errorf("collect draft entities: %w", err)
	}
	for _, uid := range uids {
		if verr := deleteEntityVecSidecar(ctx, tx, uid); verr != nil {
			return nil, fmt.Errorf("clear entity vector %s: %w", uid, verr)
		}
		if _, eerr := tx.ExecContext(ctx,
			`DELETE FROM entities WHERE uid = ? AND scope = 'draft'`, uid,
		); eerr != nil {
			return nil, fmt.Errorf("delete entity %s: %w", uid, eerr)
		}
	}
	return uids, nil
}

// collectDiscardableEntityUIDs returns the uids of draft-scoped entities that a
// scopeDiscarded for targetUID should remove: the target itself (when it is a
// draft entity) and any draft entity resolved from a mention of the target
// episode. The scope = 'draft' filter is applied here so callers can delete the
// returned rows unconditionally. Must run before the episode's mentions are
// deleted, since the via-mentions arm reads them.
func collectDiscardableEntityUIDs(ctx context.Context, tx *sql.Tx, targetUID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT uid FROM entities
		   WHERE scope = 'draft'
		     AND (uid = ?
		          OR uid IN (SELECT entity_uid FROM mentions
		                      WHERE source_event_uid = ? AND entity_uid IS NOT NULL))`,
		targetUID, targetUID,
	)
	if err != nil {
		return nil, fmt.Errorf("query draft entities: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var uids []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("scan draft entity uid: %w", err)
		}
		uids = append(uids, uid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate draft entity uids: %w", err)
	}
	return uids, nil
}
