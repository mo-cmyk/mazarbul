-- 0022_ontology_status (rollback): remove the status column from entity_kinds.
--
-- SQLite pre-3.35 cannot DROP COLUMN directly. We follow the table-rebuild
-- idiom used by 0021_predicates_added_by_extension.down.sql: rename the table,
-- rebuild it without the column, copy, and drop the old table.
--
-- Any rows with status='proposed' or status='rejected' are copied as-is;
-- callers that need to clean those up should approve or reject them before
-- rolling back.

DROP INDEX IF EXISTS entity_kinds_status;

ALTER TABLE entity_kinds RENAME TO entity_kinds_old;

CREATE TABLE entity_kinds (
    name         TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    parent       TEXT,
    attr_schema  TEXT,
    same_as      TEXT,
    provenance   TEXT NOT NULL,
    added_by     TEXT NOT NULL CHECK(added_by IN ('seed','llm','user','extension')),
    added_at     TEXT NOT NULL
);

INSERT INTO entity_kinds
    SELECT name, display_name, description, parent, attr_schema,
           same_as, provenance, added_by, added_at
    FROM entity_kinds_old;

DROP TABLE entity_kinds_old;

CREATE INDEX entity_kinds_provenance ON entity_kinds (provenance);
