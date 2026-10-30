-- Down migration 0036: drop the relation trust-posture column.
DROP INDEX IF EXISTS idx_relations_status;
ALTER TABLE relations DROP COLUMN status;
