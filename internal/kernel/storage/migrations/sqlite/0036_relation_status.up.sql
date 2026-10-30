-- Migration 0036: trust posture on every relation (issue #211).
--
-- Facts extracted from an UNTRUSTED external source (a connector ingest) must
-- be quarantined from the authoritative graph: confidence-capped, and unable to
-- supersede or be superseded by a user-asserted fact. This column carries that
-- posture so the resolver supersession path can exclude it structurally.
--
--   'asserted'  the authoritative default; every user- or trusted-extractor-
--               produced edge. It can supersede and be superseded.
--   'proposed'  an edge from a connector-sourced (untrusted) episode. The
--               resolver's untrusted gate sets it; supersedeCheck never targets
--               it and it never closes an asserted edge, so a connector claim
--               and a user fact coexist (the connector edge is a candidate,
--               never authority).
--
-- DEFAULT 'asserted' backfills every pre-0036 row as authoritative, which is the
-- correct read of all existing relations (they predate connector ingest), and
-- lets writeRelationRow stamp a non-empty value for legacy/empty payload status.
-- `maz rebuild` repopulates the column accurately from each relation event's
-- status field (empty -> asserted).

ALTER TABLE relations ADD COLUMN status TEXT NOT NULL DEFAULT 'asserted' CHECK(status IN ('asserted', 'proposed'));

CREATE INDEX idx_relations_status ON relations (status);
