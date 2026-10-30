-- Migration 0035: ingestion-source resume watermarks (issue #211).
--
-- A connectorWatermarkAdvanced event records that an ingestion Source has
-- durably consumed up to an opaque, source-defined cursor for one
-- (connector, stream) pair. This table is the head-state projection of those
-- events: one live row per (connector, stream), last-writer-wins by sequence.
-- A later ingest loads the highest-sequence watermark and pulls only the delta
-- after it, so re-ingests are cheap.
--
-- The cursor is OPAQUE — only the owning Source interprets its bytes. The
-- dispatcher validates it on every apply (size cap, well-formed JSON object,
-- strictly-monotonic sequence) and UPSERTs only when the incoming sequence is
-- strictly greater than the stored one, so a replayed or regressing event can
-- never silently reset the watermark. `maz rebuild` re-applies every event and
-- the sequence-gated upsert is idempotent, so the projection reconstructs
-- deterministically (always converging to the highest-sequence cursor)
-- regardless of replay order.
--
-- scope DEFAULT 'personal' matches kernel.DefaultScope and is the fail-safe
-- direction for the #206 read boundary: ingested content defaults to personal
-- (encrypted), and its watermark rides the same scope.
--
--   connector     ingestion source identifier e.g. local-git (projection key)
--   stream        sub-channel e.g. commits|pull-requests (projection key; '' = default)
--   cursor        opaque source-defined JSON object resume position (<= 4096 bytes)
--   sequence      strictly-monotonic advance counter for this (connector, stream)
--   advance_time  RFC3339Nano instant the watermark advanced (AIP-142)
--   scope         personal|project|team (#206 read isolation)

CREATE TABLE connector_watermarks (
    connector    TEXT NOT NULL,
    stream       TEXT NOT NULL DEFAULT '',
    cursor       TEXT NOT NULL,
    sequence     INTEGER NOT NULL,
    advance_time TEXT NOT NULL,
    scope        TEXT NOT NULL DEFAULT 'personal',
    PRIMARY KEY (connector, stream)
);

CREATE INDEX idx_connector_watermarks_connector ON connector_watermarks (connector);
