-- 0049_fact_hlc (rollback): remove the hlc column and its covering index.
-- SQLite supports DROP COLUMN since version 3.35.0 (2021-03-12).

DROP INDEX IF EXISTS idx_facts_hlc_order;
ALTER TABLE facts DROP COLUMN hlc;
