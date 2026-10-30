-- Migration 0038: synthesized insights projection (issue #231).
--
-- Each insightSynthesized event materializes one row here, keyed by the
-- insight's own ULID. The dispatcher inserts via INSERT OR IGNORE, so a Rebuild
-- replay of the same event is a no-op and the projection reconstructs
-- deterministically from the JSONL log.
--
-- source_uids is a JSON array of the record ULIDs this insight was synthesized
-- from (auditable provenance — every derived fact cites its sources). The
-- dispatcher rejects an empty array as a hard error, so the column is always a
-- non-empty list.
--
-- synthesized_confidence is the immutable decay baseline carried by the event.
-- current_confidence starts equal to it and is lowered IN PLACE by the periodic
-- decay sweep; state flips to 'demoted' when the effective confidence drops
-- below the demote floor. Both columns are PROJECTION-side state — the sweep
-- never logs an event — so `maz rebuild` replays only the synthesis event,
-- resets current_confidence to synthesized_confidence and state to 'hot', and
-- RESTORES a demoted insight to hot recall (the sweep has not run yet). This is
-- the demote-not-delete invariant: the event stays in the log, eviction never
-- happens, and a low-confidence insight is resurrectable by rebuild.
--
-- Recall surfaces insights as hits filtered by state='hot' (the demote filter).
--
-- scope DEFAULT 'personal' matches kernel.DefaultScope, the fail-safe direction
-- for the #206 read boundary.
--
--   uid                    insight ULID identity (primary key)
--   statement              the synthesized insight prose
--   insight_type           open taxonomy string e.g. pattern|preference|summary
--   source_uids            JSON array of source record ULIDs (provenance)
--   seed_entity_uid        entity whose neighborhood seeded the reflection
--   synthesized_confidence immutable decay baseline (0..1)
--   current_confidence     mutable; lowered by the decay sweep (0..1)
--   state                  hot|demoted (DEMOTE, never delete)
--   source_event_uid       the carrying insightSynthesized event UID
--   synthesized_at         RFC3339Nano synthesis instant
--   create_time            RFC3339Nano event create time
--   scope                  personal|project|team (#206 read isolation)

CREATE TABLE insights (
    uid                    TEXT PRIMARY KEY,
    statement              TEXT NOT NULL,
    insight_type           TEXT NOT NULL,
    source_uids            TEXT NOT NULL,
    seed_entity_uid        TEXT NOT NULL,
    synthesized_confidence REAL NOT NULL,
    current_confidence     REAL NOT NULL,
    state                  TEXT NOT NULL DEFAULT 'hot',
    source_event_uid       TEXT NOT NULL,
    synthesized_at         TEXT NOT NULL,
    create_time            TEXT NOT NULL,
    scope                  TEXT NOT NULL DEFAULT 'personal'
);

CREATE INDEX idx_insights_seed_entity ON insights (seed_entity_uid);
CREATE INDEX idx_insights_scope       ON insights (scope);
CREATE INDEX idx_insights_state       ON insights (state);

-- Vector domain for insight recall (sqlite-vec). The dispatcher embeds the
-- insight statement async on apply and the recall insight lane searches this
-- domain, hydrating hot insights from the insights table. {{VECTOR_DIM}} is
-- substituted at migration-load time from storage.VectorDim, exactly like the
-- episodes/entities vec tables (migration 0003).
CREATE VIRTUAL TABLE insights_vec USING vec0(
    embedding float[{{VECTOR_DIM}}]
);

CREATE TABLE insights_vec_ids (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    id    TEXT NOT NULL UNIQUE
);
