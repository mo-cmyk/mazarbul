-- 0008_mentions_entity_uid: revert linking mentions to their resolved entity.
--
-- SQLite supports DROP COLUMN since 3.35 (March 2021). The index falls out
-- with the column; an explicit DROP INDEX before the DROP COLUMN keeps the
-- intent visible.

DROP INDEX IF EXISTS idx_mentions_entity_uid;

ALTER TABLE mentions DROP COLUMN entity_uid;
