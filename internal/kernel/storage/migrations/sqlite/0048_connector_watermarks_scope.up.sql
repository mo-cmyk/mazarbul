-- Migration 0047: scope-discriminate the connector_watermarks primary key.
--
-- The 0035 projection keyed watermarks on (connector, stream) only. That was
-- safe while connectSource/ingestSource ran one scope per job, but the agent-
-- mediated captureExternal verb can write the SAME (connector, stream) in both
-- personal and team scope. With the old key those collapse into one row whose
-- scope is last-writer-wins, so a team-scope advance could be read as a
-- personal-scope freshness signal (a cross-scope leak of resume/freshness
-- state). Adding scope to the primary key isolates the head-state per scope.
--
-- SQLite cannot alter a primary key in place, so the table is recreated: rename
-- the old table, create the new one with the (connector, stream, scope) key,
-- copy the rows verbatim (existing rows already carry scope, defaulting to
-- 'personal'), drop the old table, recreate the index. The sequence-gated upsert
-- in the dispatcher is updated to conflict on the full triple.
--
-- Pre-prod clean cut: no data is lost (existing rows are unique on the triple
-- because the old key guaranteed (connector, stream) uniqueness already).

ALTER TABLE connector_watermarks RENAME TO connector_watermarks_old;

CREATE TABLE connector_watermarks (
    connector    TEXT NOT NULL,
    stream       TEXT NOT NULL DEFAULT '',
    cursor       TEXT NOT NULL,
    sequence     INTEGER NOT NULL,
    advance_time TEXT NOT NULL,
    scope        TEXT NOT NULL DEFAULT 'personal',
    PRIMARY KEY (connector, stream, scope)
);

INSERT INTO connector_watermarks
    (connector, stream, cursor, sequence, advance_time, scope)
SELECT connector, stream, cursor, sequence, advance_time, scope
FROM connector_watermarks_old;

DROP TABLE connector_watermarks_old;

CREATE INDEX idx_connector_watermarks_connector ON connector_watermarks (connector);
