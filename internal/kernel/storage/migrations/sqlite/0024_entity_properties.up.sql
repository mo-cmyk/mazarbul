-- Migration 0023: persist typed entity properties.
--
-- P3 added optional typed properties to ontology.Entity (validated against
-- the kind's propertySchema on the write path), but the entities projection
-- had no column for them: a createEntity call with properties passed
-- validation and then silently lost the attributes at dispatch. This
-- migration adds the missing column so the dispatcher can materialize the
-- payload's properties object verbatim.
--
-- The column stores the compacted JSON object as TEXT (same convention as
-- entity_kinds.attr_schema); NULL means the entity carries no typed
-- properties. Pre-0023 rows are all NULL, which is correct: no event that
-- carried properties has ever been materialized before this column existed,
-- and `maz rebuild` replays the log into the new schema.

ALTER TABLE entities ADD COLUMN properties TEXT;
