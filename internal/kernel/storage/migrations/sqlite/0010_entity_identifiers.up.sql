-- Migration 0010: entity_identifiers table.
--
-- Closes the resolver M2 TODO from internal/kernel/resolver/match.go:
-- the resolver's identifier-match step was a no-op because this table
-- did not exist. With the table in place the resolver can merge alternate
-- names ("Frodo", "Frodo Baggins", "F. Baggins") onto the same Entity instead
-- of forking a fresh UID per surface form.
--
-- Identifiers are written by the resolver alongside every EntityCreated
-- event (canonical name = kind "alias") and replayable via the new
-- EntityIdentifierAdded ontology event under maz rebuild.
CREATE TABLE IF NOT EXISTS entity_identifiers (
    entity_uid TEXT NOT NULL,
    identifier TEXT NOT NULL,
    kind       TEXT NOT NULL,
    source     TEXT NOT NULL,
    added_at   TEXT NOT NULL,
    PRIMARY KEY (entity_uid, identifier, kind),
    FOREIGN KEY (entity_uid) REFERENCES entities(uid) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_entity_identifiers_lookup
    ON entity_identifiers(identifier, kind);
