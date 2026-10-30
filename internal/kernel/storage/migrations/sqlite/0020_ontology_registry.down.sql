-- 0019_ontology_registry (rollback): drop the durable ontology-registry tables.

DROP TABLE IF EXISTS ontology_schemas;
DROP INDEX IF EXISTS entity_kinds_provenance;
DROP TABLE IF EXISTS entity_kinds;
