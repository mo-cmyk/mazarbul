-- Down migration 0030: drop the external-source provenance projection.
DROP INDEX IF EXISTS idx_source_refs_scope;
DROP INDEX IF EXISTS idx_source_refs_subject_uid;
DROP TABLE IF EXISTS source_refs;
