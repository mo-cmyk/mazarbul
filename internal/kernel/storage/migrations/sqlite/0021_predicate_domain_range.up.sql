-- 0020_predicate_domain_range: extend the predicates table with optional
-- domain/range declarations, sameAs IRIs, and a property schema.
--
-- subject_kinds: JSON text array of entity-kind names that are valid
--   subjects for this predicate (domain). NULL means any kind is accepted.
-- object_kinds:  JSON text array of entity-kind names that are valid
--   objects for this predicate (range). NULL means any kind is accepted.
-- same_as:       JSON text array of external/canonical IRIs (schema.org,
--   Wikidata, FOAF) that this predicate aligns to. NULL means none.
-- property_schema: JSON Schema (Draft 2020-12) describing typed constraints
--   on the relation Attributes map for this predicate. NULL means untyped.
--
-- All columns are nullable with default NULL so existing rows are untouched.
-- The dispatcher UPSERT (applyPredicateRegistered) is updated in the same
-- commit to persist these fields when present.

ALTER TABLE predicates ADD COLUMN subject_kinds    TEXT DEFAULT NULL;
ALTER TABLE predicates ADD COLUMN object_kinds     TEXT DEFAULT NULL;
ALTER TABLE predicates ADD COLUMN same_as          TEXT DEFAULT NULL;
ALTER TABLE predicates ADD COLUMN property_schema  TEXT DEFAULT NULL;
