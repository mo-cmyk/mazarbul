-- 0027_scope_columns (rollback): drop the scope columns + indexes and restore
-- the pre-0027 views.
--
-- This file is NOT embedded by the forward-only migration runner. It exists as a
-- manual recovery reference for development databases (same convention as
-- 0024_entity_properties.down.sql); production databases are rebuilt from the
-- JSONL event log via `maz rebuild` rather than rolled back.
--
-- SQLite 3.35+ supports DROP COLUMN directly; older builds require a full table
-- recreate.

DROP INDEX IF EXISTS idx_entities_scope;
DROP INDEX IF EXISTS idx_documents_scope;

ALTER TABLE entities  DROP COLUMN scope;
ALTER TABLE relations DROP COLUMN scope;
ALTER TABLE facts     DROP COLUMN scope;
ALTER TABLE episodes  DROP COLUMN scope;
ALTER TABLE mentions  DROP COLUMN scope;
ALTER TABLE documents DROP COLUMN scope;

-- Restore the pre-0027 views (without the scope column).
DROP VIEW IF EXISTS v_entities;
CREATE VIEW v_entities AS
SELECT
    e.uid               AS uid,
    e.kind              AS kind,
    e.display_name      AS display_name,
    e.confidence        AS confidence,
    e.resolution_status AS resolution_status,
    e.create_time       AS create_time,
    e.latitude          AS latitude,
    e.longitude         AS longitude,
    e.properties        AS properties
FROM entities e;

DROP VIEW IF EXISTS v_relations;
CREATE VIEW v_relations AS
SELECT
    r.uid          AS uid,
    r.subject_uid  AS subject_uid,
    s.display_name AS subject_name,
    r.predicate    AS predicate,
    r.object_uid   AS object_uid,
    o.display_name AS object_name,
    r.confidence   AS confidence,
    r.valid_from   AS valid_from,
    r.valid_until  AS valid_until,
    r.attributes   AS attributes,
    CASE
        WHEN (r.superseded_by_uid IS NULL OR r.superseded_by_uid = '')
         AND (r.valid_from IS NULL OR r.valid_from = '' OR julianday(r.valid_from) <= julianday('now'))
         AND (r.valid_until IS NULL OR r.valid_until = '' OR julianday('now') < julianday(r.valid_until))
        THEN 1 ELSE 0
    END            AS active
FROM relations r
LEFT JOIN entities s ON s.uid = r.subject_uid
LEFT JOIN entities o ON o.uid = r.object_uid;

DROP VIEW IF EXISTS v_facts;
CREATE VIEW v_facts AS
SELECT
    f.uid             AS uid,
    f.subject_uid     AS subject_uid,
    s.display_name    AS subject_name,
    f.state_key       AS state_key,
    f.value           AS value,
    f.event_start     AS event_start,
    f.create_time     AS create_time,
    f.source_priority AS source_priority
FROM facts f
LEFT JOIN entities s ON s.uid = f.subject_uid
WHERE f.event_end IS NULL;
