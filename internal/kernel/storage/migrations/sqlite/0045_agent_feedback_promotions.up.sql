-- agent_feedback_promotions tracks which clusters have been auto-promoted so
-- the promotion is idempotent: a cluster that has already crossed the threshold
-- and had an agentFeedbackPromoted event appended will not produce a second
-- event on subsequent recurrences or on log replay.
--
-- This table is written by the dispatcher when it applies an
-- agentFeedbackPromoted event (the event log IS the changelog). The
-- source_event_uid ties the row back to the carrying event for auditability.
CREATE TABLE IF NOT EXISTS agent_feedback_promotions (
    cluster_key      TEXT PRIMARY KEY,
    rule             TEXT NOT NULL DEFAULT '',
    recurrence       INTEGER NOT NULL,
    severity         TEXT NOT NULL,
    source_event_uid TEXT NOT NULL,
    promote_time     TEXT NOT NULL
);
