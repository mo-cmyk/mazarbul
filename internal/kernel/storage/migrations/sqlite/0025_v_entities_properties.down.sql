-- Revert v_entities to the pre-0024 column set (without properties).
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
    e.longitude         AS longitude
FROM entities e;
