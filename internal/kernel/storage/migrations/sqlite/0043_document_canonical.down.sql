-- Revert migration 0042: drop the canonical column and its partial index.
DROP INDEX IF EXISTS documents_canonical;
ALTER TABLE documents DROP COLUMN canonical;
