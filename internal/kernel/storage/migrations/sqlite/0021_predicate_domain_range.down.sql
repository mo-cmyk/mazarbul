-- 0020_predicate_domain_range (rollback): drop the four P5 columns added by the
-- up migration, reversing it exactly. ALTER TABLE ... DROP COLUMN (SQLite 3.35+,
-- 2021) preserves the predicates table's PRIMARY KEY, CHECK constraints, and all
-- existing rows — unlike a CREATE TABLE AS SELECT recreate, which would silently
-- lose them.

ALTER TABLE predicates DROP COLUMN property_schema;
ALTER TABLE predicates DROP COLUMN same_as;
ALTER TABLE predicates DROP COLUMN object_kinds;
ALTER TABLE predicates DROP COLUMN subject_kinds;
