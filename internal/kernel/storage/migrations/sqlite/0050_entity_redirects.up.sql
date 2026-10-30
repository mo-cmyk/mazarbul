-- Migration 0050: entity_redirects table for convergent merge semantics.
--
-- The existing applyEntityMerged is arrival-order-sensitive: two devices
-- applying concurrent merge events (A→B on one device, B→A on the other)
-- produce different physical states because both devices delete a different
-- entity and repoint FKs in opposite directions. The redirect table fixes
-- this by recording every merge intent durably and resolving chains and
-- cycles via a pure MIN-fold:
--
--   For a given source_uid the winning redirect is the entityMerged event
--   with the lexicographically smallest event_uid (ULID order == creation
--   order). Chains (A→B, B→C) resolve by following rows to fixpoint;
--   cycles (A→B, B→A) break by returning MIN(entity uid) among cycle
--   members. The fold is commutative and idempotent: any arrival order
--   produces the same entity_redirects state and the same resolveRedirect
--   output.
--
-- Late-arriving writes (fact, relation, identifier, etc.) that reference a
-- merged-away entity uid call resolveRedirect(tx, uid) before storage so
-- the FK lands on the canonical survivor rather than a deleted entity row.
--
-- Columns:
--   source_uid  The entity uid that was merged away. PRIMARY KEY guarantees
--               at most one canonical redirect per source.
--   target_uid  The merge target recorded by the winning (MIN event_uid) event.
--   merged_at   RFC 3339 Nano timestamp from the winning event's CreateTime.
--   event_uid   ULID of the winning entityMerged event; drives the MIN-fold
--               upsert guard (WHERE excluded.event_uid < event_uid).

CREATE TABLE entity_redirects (
    source_uid TEXT PRIMARY KEY,
    target_uid TEXT NOT NULL,
    merged_at  TEXT NOT NULL,
    event_uid  TEXT NOT NULL
);

-- Forward index for navigation: given a target entity find all sources that
-- redirect to it (used by doctor checks and the dedup repair flow).
CREATE INDEX idx_entity_redirects_target ON entity_redirects (target_uid);
