-- Migration 0040: entity_aliases projection table for alternate-name resolution.
--
-- Each entity event whose payload carries one or more alternateNames materializes
-- a row per alias into this table. The dispatcher rebuilds the alias set for an
-- entity on every entity event (DELETE old rows, INSERT new ones), so the
-- projection reflects the most recent event's alternateNames slice without
-- accumulating stale entries across replay.
--
-- The composite PRIMARY KEY (alias, entity_uid) enforces three invariants:
--   1. A single entity cannot have the same alias twice (idempotent replay:
--      re-applying the same event yields the same row, no constraint violation).
--   2. One alias MAY map to several entities (an alias resolves ambiguously if
--      multiple entities claim it, identical to same-display-name semantics).
--   3. An entity MAY have many aliases (one row per alias per entity).
--
-- scope DEFAULT 'personal' matches kernel.DefaultScope and the entities table
-- convention (migration 0027); the alias inherits the entity event's scope.
-- create_time stores the RFC3339Nano instant of the carrying event.
--
--   alias        alternate name string (case-preserving; resolution is COLLATE NOCASE)
--   entity_uid   ULID of the owning entity (foreign key to entities.uid)
--   scope        personal|project|team (#206 read isolation)
--   create_time  RFC3339Nano time of the event that recorded this alias

CREATE TABLE entity_aliases (
    alias       TEXT NOT NULL,
    entity_uid  TEXT NOT NULL,
    scope       TEXT NOT NULL DEFAULT 'personal',
    create_time TEXT NOT NULL,
    PRIMARY KEY (alias, entity_uid)
);

-- Covering index for the DELETE-by-entity_uid maintenance path (alias rebuild
-- on each entity event) and for alias-based subqueries from the resolver.
CREATE INDEX idx_entity_aliases_entity_uid ON entity_aliases (entity_uid);
CREATE INDEX idx_entity_aliases_alias      ON entity_aliases (alias);
