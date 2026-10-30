-- 0017_query_views: curated read-only views for the SQL escape hatch.
--
-- The `query` tool (and `maz query`) expose a read-only SQL surface to agents
-- and scripts. Rather than let callers couple to the physical tables — whose
-- shape churns as the ontology evolves — they query these stable views: the
-- documented target schema. The views hydrate endpoint display names, surface
-- the live-now slice, and parse nothing the caller would otherwise reconstruct
-- by hand. introspect reports their columns so an agent writes correct SQL in
-- one shot.
--
-- Views are read-only by construction in SQLite (no INSTEAD OF triggers here),
-- which composes with the read-only connection (mode=ro + PRAGMA query_only)
-- the tool runs them on.

-- v_entities: the stable entity projection — the node vocabulary plus optional
-- spatial coordinates.
CREATE VIEW v_entities AS
SELECT
    e.uid               AS uid,
    e.kind              AS kind,
    e.display_name      AS display_name,
    e.confidence        AS confidence,
    e.resolution_status AS resolution_status,
    e.create_time       AS create_time,
    e.latitude          AS latitude,
    e.longitude         AS longitude
FROM entities e;

-- v_relations: typed edges with both endpoint display names hydrated, the
-- qualifier JSON exposed verbatim (use json_extract to read keys), and an
-- `active` flag for the live-now slice. `active` mirrors the relation-validity
-- gate the navigator's related() compiler applies: not superseded, valid_from
-- already in effect (or open), and no closing valid_until yet reached. The view
-- can't take a bound param, so julianday('now') is evaluated at query time.
-- Timestamps compare via julianday() so RFC3339 values of differing sub-second
-- precision still order correctly. LEFT JOINs keep an edge visible even if an
-- endpoint entity has not yet resolved; a future-dated edge stays selectable
-- here with active=0.
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

-- v_facts: an entity's currently-open scalar facts — one row per live slot,
-- with the subject's display name hydrated. Closed/superseded history is
-- intentionally excluded; this is the "attributes right now" surface.
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
