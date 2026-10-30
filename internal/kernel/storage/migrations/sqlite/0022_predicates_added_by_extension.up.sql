-- 0021_predicates_added_by_extension: extend the CHECK constraint on
-- predicates.added_by to allow the 'extension' value emitted by the
-- schema-pack installer.
--
-- SQLite does not allow ALTER TABLE ... MODIFY COLUMN, so we rebuild the
-- predicates table in three steps:
--   1. Rename the current table to a backup name.
--   2. Create the new table with the extended CHECK constraint.
--   3. Copy all existing rows and drop the backup.
--
-- All four columns added by migration 0017 (subject_kinds, object_kinds,
-- same_as, property_schema) are preserved in the new schema.

ALTER TABLE predicates RENAME TO predicates_old;

CREATE TABLE predicates (
    predicate       TEXT PRIMARY KEY,
    kind            TEXT NOT NULL CHECK(kind IN ('functional','non-functional')),
    description     TEXT NOT NULL DEFAULT '',
    added_at        TEXT NOT NULL,
    added_by        TEXT NOT NULL CHECK(added_by IN ('seed','llm','user','extension')),
    subject_kinds   TEXT DEFAULT NULL,
    object_kinds    TEXT DEFAULT NULL,
    same_as         TEXT DEFAULT NULL,
    property_schema TEXT DEFAULT NULL
);

INSERT INTO predicates
    SELECT predicate, kind, description, added_at, added_by,
           subject_kinds, object_kinds, same_as, property_schema
    FROM predicates_old;

DROP TABLE predicates_old;
