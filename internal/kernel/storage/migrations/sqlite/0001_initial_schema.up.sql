-- 0001_initial_schema: M1 bare tables for the relational layer.
-- See agent-docs/designs/storage.md for the four-layer rationale.
-- All time columns store RFC 3339 UTC strings (text affinity).

CREATE TABLE episodes (
    uid          TEXT PRIMARY KEY,
    create_time  TEXT NOT NULL,
    kind         TEXT NOT NULL,
    payload_json TEXT NOT NULL
);

CREATE INDEX idx_episodes_create_time ON episodes(create_time);
CREATE INDEX idx_episodes_kind        ON episodes(kind);

CREATE TABLE entities (
    uid               TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,
    display_name      TEXT NOT NULL,
    confidence        REAL NOT NULL,
    resolution_status TEXT NOT NULL
);

CREATE INDEX idx_entities_kind ON entities(kind);

CREATE TABLE mentions (
    uid              TEXT PRIMARY KEY,
    kind             TEXT NOT NULL,
    display_name     TEXT NOT NULL,
    source_event_uid TEXT NOT NULL,
    create_time      TEXT NOT NULL,
    FOREIGN KEY (source_event_uid) REFERENCES episodes(uid)
);

CREATE INDEX idx_mentions_source_event_uid ON mentions(source_event_uid);

CREATE TABLE provenance (
    event_uid  TEXT NOT NULL,
    source     TEXT NOT NULL,
    confidence REAL NOT NULL,
    decided_at TEXT NOT NULL,
    FOREIGN KEY (event_uid) REFERENCES episodes(uid)
);

CREATE INDEX idx_provenance_event_uid ON provenance(event_uid);
