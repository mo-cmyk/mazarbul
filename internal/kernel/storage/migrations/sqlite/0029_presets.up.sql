-- Migration 0029: presets projection (issue #210).
--
-- One row per ACTIVE preset. A presetInstalled event upserts a row; a
-- presetDeactivated event DELETES it — a clean cut, with no inactive flag, so
-- the table holds only the presets currently steering agents. The entity kinds
-- and predicates the install also seeded live in entity_kinds/predicates as
-- plain extension ontology and are NOT tracked here; deactivation does not
-- retract them.
--
-- The verbatim instructions body is held here so the getContext brief can read
-- active presets without re-scanning the event log. The body is render-private:
-- the git renderer never writes it to plaintext, and the privacy gate marks the
-- presetInstalled event sensitive on shared (non-personal) scopes.
--
-- `maz rebuild` reconstructs this table from the log deterministically: install
-- -> INSERT/UPSERT, deactivate -> DELETE. Both are idempotent and, for a single
-- preset name, order-independent (the log's monotonic order guarantees install
-- precedes deactivate over a preset's lifecycle).

CREATE TABLE presets (
    name         TEXT PRIMARY KEY NOT NULL,
    version      TEXT NOT NULL,
    description  TEXT NOT NULL,
    instructions TEXT NOT NULL,          -- verbatim instructions.md body
    added_at     TEXT NOT NULL           -- RFC 3339 UTC
);
