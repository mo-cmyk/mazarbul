-- 0005_multivec_spatial DOWN migration.
--
-- This file is NOT embedded by the forward-only migration runner. It exists
-- as a manual recovery reference: if a developer needs to roll back migration
-- 0005 on a development database, they can execute this file directly via
-- the sqlite3 CLI or a similar tool.
--
-- Production databases are rebuilt from the JSONL event log via `maz rebuild`
-- rather than rolled back; this file is a dev-time convenience, not a
-- production rollback path.
--
-- To apply manually:
--   sqlite3 <data-dir>/index.db < 0005_multivec_spatial.down.sql

-- Remove coordinate enforcement triggers.
DROP TRIGGER IF EXISTS entities_coords_check_update;
DROP TRIGGER IF EXISTS entities_coords_check_insert;

-- Remove mention vector tables.
DROP TABLE IF EXISTS mentions_vec_ids;
DROP TABLE IF EXISTS mentions_vec;

-- Remove multi-vector entity tables and restore the single-vector design
-- from migration 0003.
DROP TABLE IF EXISTS entities_vec_ids;
DROP TABLE IF EXISTS entities_vec;

CREATE VIRTUAL TABLE entities_vec USING vec0(
    embedding float[1024]
);

CREATE TABLE entities_vec_ids (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    id    TEXT NOT NULL UNIQUE
);

-- Remove extraction_text from mentions. SQLite 3.35+ supports DROP COLUMN;
-- older builds require a full table recreate.
ALTER TABLE mentions DROP COLUMN extraction_text;

-- Remove spatial columns and create_time from entities.
ALTER TABLE entities DROP COLUMN coordinates_provenance;
ALTER TABLE entities DROP COLUMN longitude;
ALTER TABLE entities DROP COLUMN latitude;
ALTER TABLE entities DROP COLUMN create_time;

-- Remove the _migrations bookkeeping row for version 5 so the forward
-- migration can be re-applied after this rollback.
DELETE FROM _migrations WHERE version = 5;
