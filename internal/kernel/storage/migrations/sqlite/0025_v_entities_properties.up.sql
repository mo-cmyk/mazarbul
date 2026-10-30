-- 0024_v_entities_properties: expose the typed-property bag on the entity view.
--
-- Migration 0023 added entities.properties (the JSON object of typed attributes
-- validated against a kind's propertySchema, written by createEntity). The
-- column was persisted but unreachable: v_entities (the curated SQL surface for
-- `query` / `maz query`) omitted it, so an agent could write typed properties
-- and never read them back through any tool. Adding the column here completes
-- the P3 write->read round trip alongside the get/entities tool changes.
--
-- SQLite views are immutable, so we DROP and recreate with the column appended
-- (kept last to preserve existing positional column references). The introspect
-- query-view catalog (queryViews in introspect.go) gains the matching column so
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
    e.properties        AS properties
FROM entities e;
