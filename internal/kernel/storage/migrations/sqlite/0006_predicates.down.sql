-- 0006_predicates DOWN migration.
--
-- This file is NOT embedded by the forward-only migration runner. It exists
-- as a manual recovery reference: if a developer needs to roll back migration
-- 0006 on a development database, they can execute this file directly via
-- the sqlite3 CLI or a similar tool.
--
-- Production databases are rebuilt from the JSONL event log via `maz rebuild`
-- rather than rolled back; this file is a dev-time convenience, not a
-- production rollback path.
--
-- To apply manually:
--   sqlite3 <data-dir>/index.db < 0006_predicates.down.sql

DROP TABLE IF EXISTS predicates;

-- Remove the _migrations bookkeeping row for version 6 so the forward
-- migration can be re-applied after this rollback.
DELETE FROM _migrations WHERE version = 6;
