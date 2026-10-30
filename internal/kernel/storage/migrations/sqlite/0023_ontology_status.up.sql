-- 0022_ontology_status: adds a governance status column to entity_kinds so the
-- P7 governed mode can track proposed, active, and rejected entity kinds.
--
-- The default is 'active' so every existing row (pre-P7 bootstrap) is treated
-- as active without a data migration. The CHECK constraint enforces the three
-- valid values; the index lets the `maz schema review list` query enumerate
-- proposed kinds without a full table scan.

ALTER TABLE entity_kinds ADD COLUMN status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','proposed','rejected'));

CREATE INDEX entity_kinds_status ON entity_kinds (status);
