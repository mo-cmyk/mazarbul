-- 0014_facts_table: introduce the Fact slot store.
--
-- Facts are the scalar / preference / state slot of the four-layer
-- storage. Unlike Relations (typed edges between two entities), a Fact
-- carries a scalar value addressed by a hierarchical state_key. The
-- (subject_uid, state_key) pair is the natural unique key for
-- supersede: a new write to the same state_key closes the prior open
-- Fact by setting its event_end.
--
-- Phase 3 of the mem0-parity plan introduces this table additively;
-- Relations and Facts coexist.

CREATE TABLE facts (
    uid                TEXT PRIMARY KEY NOT NULL,
    subject_uid        TEXT NOT NULL,
    -- state_key is the hierarchical slot address. CHECK regex mirrors
    -- ontology.StateKeyPattern() verbatim; segments are
    -- [a-z][a-z0-9_]* separated by dots, first segment is the mem0
    -- category (event|state|relationship|preference|plan|absence|timeless).
    state_key          TEXT NOT NULL,
    value              TEXT NOT NULL,
    event_start        TEXT,                   -- RFC 3339; NULL means CreateTime
    event_end          TEXT,                   -- RFC 3339; NULL means open
    create_time        TEXT NOT NULL,          -- RFC 3339, set by writer
    source_event_uid   TEXT NOT NULL,
    content_hash       TEXT,                   -- MD5(subject_uid|state_key|value) for insert dedup
    superseded_by_uid  TEXT,
    source_priority    REAL NOT NULL DEFAULT 1.0
        CHECK (source_priority >= 0.0 AND source_priority <= 1.0)
);

-- Hot path for supersede lookups: "find the open Fact for this
-- (subject, state_key)". A partial index on event_end IS NULL keeps
-- it tight as historical rows accumulate.
CREATE INDEX facts_open_by_state_key
    ON facts (subject_uid, state_key)
    WHERE event_end IS NULL;

-- Insert-time dedup: a re-emission of the same Fact (same hash) is a
-- no-op rather than a duplicate row. content_hash NULL is allowed for
-- legacy / hand-written rows that skip the dedup pathway.
CREATE INDEX facts_content_hash ON facts (content_hash)
    WHERE content_hash IS NOT NULL;

-- Backlink for "show the history of this slot": cheap join on the
-- supersede pointer without scanning the full table.
CREATE INDEX facts_superseded_by ON facts (superseded_by_uid)
    WHERE superseded_by_uid IS NOT NULL;
