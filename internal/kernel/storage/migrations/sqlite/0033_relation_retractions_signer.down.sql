-- Down migration 0033: drop the relation_retractions signer columns.
--
-- This file is NOT embedded by the forward-only migration runner (it embeds
-- only *.up.sql). It exists as a manual recovery reference for development
-- databases (same convention as 0027_scope_columns.down.sql); production
-- databases are rebuilt from the JSONL event log via `maz rebuild` rather than
-- rolled back.
--
-- SQLite 3.35+ supports DROP COLUMN directly; older builds require a full
-- table recreate.

ALTER TABLE relation_retractions DROP COLUMN signer_device;
ALTER TABLE relation_retractions DROP COLUMN signer_principal;
