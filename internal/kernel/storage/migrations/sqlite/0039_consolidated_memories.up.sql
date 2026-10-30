-- Migration 0039: consolidated memories projection (issue #231).
--
-- Each memoryConsolidated event materializes one row here, keyed by the
-- consolidated memory's own ULID. The dispatcher inserts via INSERT OR IGNORE,
-- so a Rebuild replay of the same event is a no-op and the projection
-- reconstructs deterministically from the JSONL log.
--
-- source_uids is a JSON array of the episode ULIDs that were compressed into
-- this memory (auditable provenance). The dispatcher rejects an empty array as
-- a hard error, so the column is always a non-empty list. Before appending, the
-- consolidation job dedups the candidate summary against existing consolidated
-- memories (semantic dedup), so a near-duplicate consolidation is skipped rather
-- than appended — no orphan derived facts.
--
-- Recall surfaces consolidated memories as hits (embedded async on apply, like
-- episodes). state mirrors the insights table for a future decay sweep; for now
-- consolidated memories default to 'hot'.
--
-- scope DEFAULT 'personal' matches kernel.DefaultScope, the fail-safe direction
-- for the #206 read boundary.
--
--   uid              consolidated memory ULID identity (primary key)
--   summary          the compressed consolidated-memory prose
--   memory_type      open taxonomy string e.g. semantic|procedural|summary
--   source_uids      JSON array of compressed episode ULIDs (provenance)
--   cited_entity_uid entity the group was clustered around
--   confidence       confidence in the consolidation (0..1)
--   state            hot|demoted (DEMOTE, never delete)
--   source_event_uid the carrying memoryConsolidated event UID
--   consolidated_at  RFC3339Nano consolidation instant
--   create_time      RFC3339Nano event create time
--   scope            personal|project|team (#206 read isolation)

CREATE TABLE consolidated_memories (
    uid              TEXT PRIMARY KEY,
    summary          TEXT NOT NULL,
    memory_type      TEXT NOT NULL,
    source_uids      TEXT NOT NULL,
    cited_entity_uid TEXT NOT NULL,
    confidence       REAL NOT NULL,
    state            TEXT NOT NULL DEFAULT 'hot',
    source_event_uid TEXT NOT NULL,
    consolidated_at  TEXT NOT NULL,
    create_time      TEXT NOT NULL,
    scope            TEXT NOT NULL DEFAULT 'personal'
);

CREATE INDEX idx_consolidated_cited_entity ON consolidated_memories (cited_entity_uid);
CREATE INDEX idx_consolidated_scope        ON consolidated_memories (scope);
CREATE INDEX idx_consolidated_state        ON consolidated_memories (state);

-- Vector domain for consolidated-memory recall (sqlite-vec). The dispatcher
-- embeds the summary async on apply and the recall consolidated lane searches
-- this domain. {{VECTOR_DIM}} is substituted at migration-load time from
-- storage.VectorDim.
CREATE VIRTUAL TABLE consolidated_vec USING vec0(
    embedding float[{{VECTOR_DIM}}]
);

CREATE TABLE consolidated_vec_ids (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    id    TEXT NOT NULL UNIQUE
);
