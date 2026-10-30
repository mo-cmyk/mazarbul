-- Down migration 0026: drop the relation retraction tombstone set.
DROP INDEX IF EXISTS idx_relation_retractions_relation_uid;
DROP TABLE IF EXISTS relation_retractions;
