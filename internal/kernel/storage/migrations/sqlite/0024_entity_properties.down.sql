-- 0023_entity_properties (rollback): remove the properties column.
--
-- This file is NOT embedded by the forward-only migration runner. It exists
-- as a manual recovery reference for development databases (same convention
-- as 0005_multivec_spatial.down.sql); production databases are rebuilt from
-- the JSONL event log via `maz rebuild` rather than rolled back.
--
-- SQLite 3.35+ supports DROP COLUMN directly; older builds require a full
-- table recreate.

ALTER TABLE entities DROP COLUMN properties;
