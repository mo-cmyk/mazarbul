-- 0019_ontology_registry: durable projections for the entity-kind taxonomy
-- and the runtime schema registry.
--
-- entity_kinds mirrors the payload of entityKindRegistered events: one row per
-- registered kind. The dispatcher UPSERTs into this table on replay so
-- maz rebuild reconstructs the full taxonomy from the event log alone.
--
-- ontology_schemas mirrors the payload of schemaRegistered events: one row per
-- registered schema kind (e.g. "entityKind:vehicle", a custom event shape).
-- The SQLiteOntology implementation reads this table for dynamic schemas that
-- were not registered at boot via Register calls.
--
-- NO seed rows are inserted here. The six core entity kinds and the seed
-- predicates are installed by genesis events on first boot (see
-- internal/kernelboot/genesis.go), so the event log is the sole source of
-- truth for the taxonomy and syncs via the git transport.

CREATE TABLE entity_kinds (
    name         TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    parent       TEXT,
    attr_schema  TEXT,          -- JSON text of the kind's propertySchema, or NULL
    same_as      TEXT,          -- JSON text array of sameAs IRIs, or NULL
    provenance   TEXT NOT NULL,
    added_by     TEXT NOT NULL CHECK(added_by IN ('seed','llm','user','extension')),
    added_at     TEXT NOT NULL
);

-- Index provenance so the governance surface can quickly enumerate core vs
-- user vs extension kinds without a full table scan.
CREATE INDEX entity_kinds_provenance ON entity_kinds (provenance);

CREATE TABLE ontology_schemas (
    schema_kind TEXT PRIMARY KEY,
    version     TEXT NOT NULL,
    definition  TEXT NOT NULL,  -- JSON Schema document text
    provenance  TEXT NOT NULL,
    added_by    TEXT NOT NULL,
    added_at    TEXT NOT NULL
);
