-- Dynamic lessons + feedback loop (Omnigraph-parity wave).
--
-- agent_feedback holds one row per agentFeedbackRecorded event. Each capture
-- is a DISTINCT occurrence (PRIMARY KEY = the carrying event uid) so the loop
-- can count recurrence per cluster_key: when a cluster recurs past a threshold
-- it auto-promotes a distilled one-line rule into the per-session guidance.
-- Replaying the log re-applies identical rows (ON CONFLICT(uid) upsert), so the
-- projection is deterministic under `maz rebuild`.
--
-- This table is NOT part of the scoped recall/navigation read spine (it is not
-- in sharedDerivedTables) — it is read only by the dedicated `maz feedback`
-- surface and the instruction composer, like source_refs. The scope column is
-- carried for provenance + parity with the event log, not for the trace test.
CREATE TABLE IF NOT EXISTS agent_feedback (
    uid              TEXT PRIMARY KEY,
    cluster_key      TEXT NOT NULL,
    category         TEXT NOT NULL,
    severity         TEXT NOT NULL,
    surface          TEXT NOT NULL DEFAULT '',
    summary          TEXT NOT NULL,
    detail           TEXT NOT NULL DEFAULT '',
    rule             TEXT NOT NULL DEFAULT '',
    keywords         TEXT NOT NULL DEFAULT '',
    version          TEXT NOT NULL DEFAULT '',
    active_preset    TEXT NOT NULL DEFAULT '',
    source_event_uid TEXT NOT NULL,
    create_time      TEXT NOT NULL,
    scope            TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_feedback_cluster ON agent_feedback (cluster_key);
CREATE INDEX IF NOT EXISTS idx_agent_feedback_create_time ON agent_feedback (create_time);

-- agent_feedback_reviews holds the last-writer-wins review state per cluster
-- (the "+ review" override on auto-promotion). A pin protects a promoted rule
-- from cap eviction; a veto suppresses it (and stops it re-promoting); an edit
-- overrides the distilled text. Keyed by cluster_key so each review event
-- upserts the one row for its cluster.
CREATE TABLE IF NOT EXISTS agent_feedback_reviews (
    cluster_key      TEXT PRIMARY KEY,
    pinned           INTEGER NOT NULL DEFAULT 0,
    vetoed           INTEGER NOT NULL DEFAULT 0,
    edited_rule      TEXT NOT NULL DEFAULT '',
    last_action      TEXT NOT NULL,
    source_event_uid TEXT NOT NULL,
    review_time      TEXT NOT NULL
);
