-- 0007_relations: bi-temporal relation store between entities.
--
-- The relations table holds directed, typed edges between entities. Each row
-- carries a (subject_uid, predicate, object_uid) triple together with
-- provenance metadata and optional bi-temporal validity bounds.
--
-- Supersede semantics: when a functional predicate already has a current
-- relation for (subject_uid, predicate), the resolver writes a new row and
-- sets superseded_by_uid on the old row. Rows are never deleted; the event
-- log is the source of truth.
--
-- Indexes are chosen to serve the three common query patterns:
--   - find the current relation(s) for (subject, predicate) to check supersede
--   - find all relations where entity appears as object
--   - temporal slicing over a subject's predicate history

CREATE TABLE relations (
    uid               TEXT PRIMARY KEY,
    subject_uid       TEXT NOT NULL,
    predicate         TEXT NOT NULL,
    object_uid        TEXT NOT NULL,
    confidence        REAL NOT NULL,
    source_event_uid  TEXT NOT NULL,
    create_time       TEXT NOT NULL,
    valid_from        TEXT,
    valid_until       TEXT,
    validity_score    REAL,
    superseded_by_uid TEXT,
    provenance        TEXT NOT NULL DEFAULT ''
);

-- Supports the supersede lookup: find the live (subject, predicate) row.
CREATE INDEX idx_relations_subject_predicate ON relations(subject_uid, predicate);

-- Supports reverse-edge queries: which subjects point to this object?
CREATE INDEX idx_relations_object_predicate ON relations(object_uid, predicate);

-- Supports temporal slice queries over a subject's predicate history.
CREATE INDEX idx_relations_temporal ON relations(subject_uid, predicate, valid_from, valid_until);
