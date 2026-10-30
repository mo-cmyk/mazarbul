-- 0021_predicates_added_by_extension (rollback): restore the original CHECK
-- constraint on predicates.added_by (seed, llm, user only).
--
-- Any rows with added_by='extension' are deleted before the rebuild because
-- they would violate the rolled-back constraint.

DELETE FROM predicates WHERE added_by = 'extension';

ALTER TABLE predicates RENAME TO predicates_new;

CREATE TABLE predicates (
    predicate       TEXT PRIMARY KEY,
    kind            TEXT NOT NULL CHECK(kind IN ('functional','non-functional')),
    description     TEXT NOT NULL DEFAULT '',
    added_at        TEXT NOT NULL,
    added_by        TEXT NOT NULL CHECK(added_by IN ('seed','llm','user')),
    subject_kinds   TEXT DEFAULT NULL,
    object_kinds    TEXT DEFAULT NULL,
    same_as         TEXT DEFAULT NULL,
    property_schema TEXT DEFAULT NULL
);

INSERT INTO predicates
    SELECT predicate, kind, description, added_at, added_by,
           subject_kinds, object_kinds, same_as, property_schema
    FROM predicates_new;

DROP TABLE predicates_new;
