-- 0046_fact_retraction (rollback): remove the retracted_at column.
-- SQLite supports DROP COLUMN since version 3.35.0 (2021-03-12).

ALTER TABLE facts DROP COLUMN retracted_at;
