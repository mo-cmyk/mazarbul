-- 0007_relations DOWN migration.
--
-- This file is NOT embedded by the forward-only migration runner. It exists
-- as a manual recovery reference: if a developer needs to roll back migration
-- 0007 on a development database, they can execute this file directly via
-- the sqlite3 CLI or a similar tool.
--
-- Production databases are rebuilt from the JSONL event log via `maz rebuild`
-- rather than rolled back; this file is a dev-time convenience, not a
-- production rollback path.
--
-- To apply manually:
--   sqlite3 <data-dir>/index.db < 0007_relations.down.sql

DROP INDEX IF EXISTS idx_relations_temporal;
DROP INDEX IF EXISTS idx_relations_object_predicate;
DROP INDEX IF EXISTS idx_relations_subject_predicate;
DROP TABLE IF EXISTS relations;

-- Remove the _migrations bookkeeping row for version 7 so the forward
-- migration can be re-applied after this rollback.
DELETE FROM _migrations WHERE version = 7;
