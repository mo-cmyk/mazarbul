-- Migration 0027: scope-isolated reads (#206).
--
-- Recall / getContext / traversal are scope-blind today: every read path issues
-- raw SELECTs against the derived projection with no scope predicate, so a
-- personal-scope fact leaks into a project-scope read and vice versa. Scope
-- already exists at the write boundary (kernel.Event.Scope, validated at Append,
-- physically separated on the sync remote per agent-docs/privacy.md) but the
-- local SQLite projection flattens every scope into one shared store.
--
-- This migration adds the `scope` column to the shared derived tables so the
-- read spine can inject a fail-closed scope predicate (see internal/kernel/
-- scoped_reader.go). Per the design (#206): scope lives on the statements
-- (relations/facts/episodes/mentions/documents each carry their single
-- source-event scope); an entity is surfaced in a scope iff it has an in-scope
-- statement, and the entity's own column is the bare-listing backstop.
--
-- DEFAULT 'personal' matches kernel.DefaultScope and is the fail-safe direction:
-- pre-0027 rows coerce to personal (visible under the default {project,personal}
-- read, never leaked into a shared `team` scope) until `maz rebuild` replays the
-- event log and repopulates the accurate scope from each event. A miss is a
-- visibility miss, never a leak.

ALTER TABLE entities  ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE relations ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE facts     ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE episodes  ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE mentions  ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE documents ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';

-- Bare-listing hot paths (ListEntities / ListDocuments) lead with a scope
-- filter; the keyed tables (relations/facts/episodes/mentions) are reached via
-- subject_uid / uid lookups where scope is a cheap additional predicate on an
-- already-narrow row set, so they need no dedicated scope index.
CREATE INDEX IF NOT EXISTS idx_entities_scope  ON entities(scope);
CREATE INDEX IF NOT EXISTS idx_documents_scope ON documents(scope);

-- Curated SQL-escape-hatch views (0017 / 0025) wrap the base tables for the
-- `query` tool and `maz query`. They must expose `scope` so the query surface
-- can enforce the same boundary. SQLite views are immutable: DROP + recreate
-- with `scope` appended last to preserve positional column references. The
-- queryViews catalog in internal/mcp/tools/introspect.go is updated to match so
-- TestQueryViewCatalog_MatchesLiveViews stays green.
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
    e.properties        AS properties,
    e.scope             AS scope
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
    END            AS active,
    r.scope        AS scope
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
    f.source_priority AS source_priority,
    f.scope           AS scope
FROM facts f
LEFT JOIN entities s ON s.uid = f.subject_uid
WHERE f.event_end IS NULL;
