-- 0014_facts_table (down): drop the facts table + its indexes.
-- Mirror of 0014_facts_table.up.sql; SQLite drops indexes implicitly
-- with DROP TABLE, but explicit DROPs make the intent visible.
DROP INDEX IF EXISTS facts_superseded_by;
DROP INDEX IF EXISTS facts_content_hash;
DROP INDEX IF EXISTS facts_open_by_state_key;
DROP TABLE IF EXISTS facts;
