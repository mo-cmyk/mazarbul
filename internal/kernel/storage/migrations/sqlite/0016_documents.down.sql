-- Reverse of 0016_documents. Drops all objects introduced by the up migration
-- in reverse dependency order: indexes first, then the sidecar, then the vec0
-- virtual table, then the FTS triggers, then the FTS virtual table, then the
-- documents table with its indexes.
--
-- SQLite 3.45+ (required by sqlite-vec) supports DROP INDEX IF EXISTS and
-- DROP TABLE IF EXISTS, so each statement is idempotent. Production rollback
-- loses all document body data; a content-preserving rollback would require
-- exporting documents to the JSONL log before running this script.

-- vec0 sidecar and index
DROP INDEX IF EXISTS documents_vec_ids_document_uid;
DROP TABLE IF EXISTS documents_vec_ids;

-- vec0 virtual table (DROP TABLE works on virtual tables in SQLite)
DROP TABLE IF EXISTS documents_vec;

-- FTS5 sync triggers
DROP TRIGGER IF EXISTS documents_au;
DROP TRIGGER IF EXISTS documents_ad;
DROP TRIGGER IF EXISTS documents_ai;

-- FTS5 virtual table
DROP TABLE IF EXISTS documents_fts;

-- Relational table indexes
DROP INDEX IF EXISTS documents_update_time;
DROP INDEX IF EXISTS documents_pinned;
DROP INDEX IF EXISTS documents_path;

-- Relational table
DROP TABLE IF EXISTS documents;
