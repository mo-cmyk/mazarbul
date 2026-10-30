-- 0011_relation_attributes: structured per-relation attributes (JSON).
--
-- The relations table treats every (subject, predicate, object) edge as a
-- triple, which is enough for "Frodo works_at Erebor" but loses the role
-- qualifier in "Rosie is the CEO of Erebor" (the role "CEO" is dropped, and
-- only the bare works_at edge survives). The attributes column carries a
-- JSON object of qualifying key/value pairs (most commonly "role",
-- "title", "employment_type") that the extractor populates from the
-- source text and the graph lane can match on for reasoning queries
-- (boss-of, member-of-with-role, transitive management chains).
--
-- NOT NULL DEFAULT '{}' keeps the column shape uniform: legacy rows are
-- backfilled with the empty-object literal, and the dispatcher writes the
-- same literal for relations with no attributes. The ontology Go type is
-- the source of truth for the JSON shape; the column does not validate.

ALTER TABLE relations ADD COLUMN attributes TEXT NOT NULL DEFAULT '{}';
