-- Down migration 0047: revert connector_watermarks to the (connector, stream)
-- primary key. Where scope-discriminated rows now exist for the same
-- (connector, stream), the highest-sequence row wins (the same convergence the
-- runtime upsert applies), so the collapse is deterministic.
ALTER TABLE connector_watermarks RENAME TO connector_watermarks_old;

CREATE TABLE connector_watermarks (
    connector    TEXT NOT NULL,
    stream       TEXT NOT NULL DEFAULT '',
    cursor       TEXT NOT NULL,
    sequence     INTEGER NOT NULL,
    advance_time TEXT NOT NULL,
    scope        TEXT NOT NULL DEFAULT 'personal',
    PRIMARY KEY (connector, stream)
);

INSERT INTO connector_watermarks
    (connector, stream, cursor, sequence, advance_time, scope)
SELECT connector, stream, cursor, sequence, advance_time, scope
FROM connector_watermarks_old w
WHERE w.sequence = (
    SELECT MAX(w2.sequence) FROM connector_watermarks_old w2
    WHERE w2.connector = w.connector AND w2.stream = w.stream
);

DROP TABLE connector_watermarks_old;

CREATE INDEX idx_connector_watermarks_connector ON connector_watermarks (connector);
