-- Reverse 0011_relation_attributes.
--
-- SQLite supports DROP COLUMN since 3.35; the runner is built against a
-- newer engine so this is safe.

ALTER TABLE relations DROP COLUMN attributes;
