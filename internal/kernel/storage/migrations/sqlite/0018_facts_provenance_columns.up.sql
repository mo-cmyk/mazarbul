-- 0018_facts_provenance_columns: add source + confidence to the facts table.
--
-- Facts created by direct assertion (maz fact set / the fact MCP tool) have
-- no entry in the provenance table, because provenance.event_uid carries a
-- FK that references episodes(uid).  The event source and confidence that
-- flew in on the KindFact event are therefore not surfaced by get(facts/{uid}).
--
-- These two nullable columns denormalise the event provenance fields directly
-- onto the facts row so Navigator.fetchFact can return a synthetic provenance
-- entry without joining the episodes-only provenance table.
--
-- Both columns allow NULL for backwards compatibility with rows written before
-- this migration; new rows written by applyFact will always supply a value.

ALTER TABLE facts ADD COLUMN source     TEXT DEFAULT NULL;
ALTER TABLE facts ADD COLUMN confidence REAL DEFAULT NULL;
