-- Migration 0026: convergent relation retraction (issue #205).
--
-- Replaces the session-serial relationSuperseded path with a commutative,
-- monotonic tombstone set. Each relationRetracted event materializes one row
-- here, keyed by the retraction event's own UID so set-union is idempotent
-- (INSERT OR IGNORE). The relations.valid_until / relations.superseded_by_uid
-- columns become a DETERMINISTIC FOLD over the rows for a given relation_uid
-- (lowest superseder ULID; earliest retracted_at) — so two paired devices that
-- close the same arm converge regardless of the order their events replay in.
--
-- This table is the durable source of truth; the relations columns are its
-- materialized cache. `maz rebuild` rebuilds it from the JSONL log: every
-- relationRetracted event re-Applies (HasEvent does not track it), and the
-- INSERT OR IGNORE + re-fold are idempotent.
--
--   retraction_event_uid  the relationRetracted event UID (OR-set element tag)
--   relation_uid          the Relation being closed (grouping key for the fold)
--   superseded_by_uid     the replacing Relation (always set in #205; the
--                         separate relationForgotten path handles deletion)
--   retracted_at          RFC3339Nano instant the close was applied

CREATE TABLE relation_retractions (
    retraction_event_uid TEXT PRIMARY KEY,
    relation_uid         TEXT NOT NULL,
    superseded_by_uid    TEXT NOT NULL,
    retracted_at         TEXT NOT NULL
);

CREATE INDEX idx_relation_retractions_relation_uid ON relation_retractions (relation_uid);
