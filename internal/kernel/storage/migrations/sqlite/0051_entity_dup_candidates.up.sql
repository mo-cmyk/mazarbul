-- Migration 0051: entity_dup_candidates derived table.
--
-- The post-consume dedup sweep compares every entity that arrives via a
-- cross-device team-scope graft against existing entities in the three
-- entitymatch lanes: exact-name (same kind + case-insensitive name),
-- name-KNN (advisory vector similarity), and strong-identifier equality.
--
-- This table captures lane (a) exact-name and lane (c) name-KNN pairs for
-- human review. Lane (b) strong-identifier pairs are AUTO-MERGED immediately
-- by the sweep (an entityMerged event is appended); they do not land here.
--
-- Design notes:
--   * DERIVED table: no event kind backs it. The sweep rebuilds it from
--     the live entity + entity_identifiers projections on each pass. It is
--     therefore safe to truncate at any time; the next sweep restores it.
--   * Pair ordering: entity_uid_a < entity_uid_b (lexicographic) so each
--     (a, b) pair is stored exactly once regardless of which entity the
--     sweep found the other from.
--   * reason: 'exact_name' or 'name_emb' — the lane that surfaced the pair.
--   * score: cosine similarity for name_emb; 1.0 for exact_name.
--   * detected_at: RFC 3339 UTC timestamp of the most recent detection.

CREATE TABLE IF NOT EXISTS entity_dup_candidates (
    entity_uid_a TEXT    NOT NULL,
    entity_uid_b TEXT    NOT NULL,
    reason       TEXT    NOT NULL,
    score        REAL    NOT NULL,
    detected_at  TEXT    NOT NULL,
    PRIMARY KEY (entity_uid_a, entity_uid_b),
    CHECK (entity_uid_a < entity_uid_b)
);

CREATE INDEX IF NOT EXISTS idx_entity_dup_candidates_a
    ON entity_dup_candidates (entity_uid_a);
CREATE INDEX IF NOT EXISTS idx_entity_dup_candidates_b
    ON entity_dup_candidates (entity_uid_b);
