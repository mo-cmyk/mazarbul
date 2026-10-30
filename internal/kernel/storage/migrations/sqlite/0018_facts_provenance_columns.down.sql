-- 0018_facts_provenance_columns (rollback): remove source + confidence columns.
-- SQLite supports DROP COLUMN since version 3.35.0 (2021-03-12).

ALTER TABLE facts DROP COLUMN source;
ALTER TABLE facts DROP COLUMN confidence;
