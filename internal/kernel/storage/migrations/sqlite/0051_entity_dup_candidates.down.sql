-- Down migration 0051: drop entity_dup_candidates.
-- The table is derived; no data is lost on rollback (the sweep rebuilds it).

DROP INDEX IF EXISTS idx_entity_dup_candidates_b;
DROP INDEX IF EXISTS idx_entity_dup_candidates_a;
DROP TABLE IF EXISTS entity_dup_candidates;
