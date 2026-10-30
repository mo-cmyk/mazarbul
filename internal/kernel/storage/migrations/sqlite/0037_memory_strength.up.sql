-- Migration 0037: passive-reinforcement memory strength (issue #231).
--
-- The cognitive memory lifecycle reinforces frequently-recalled memories by
-- folding memoryAccessed events into a convergent, replayable strength
-- accumulator. There are two tables:
--
-- memory_accesses is the durable access OR-set: one row per memoryAccessed
-- event, keyed by the event's own UID. INSERT OR IGNORE makes set-union
-- idempotent, so a Rebuild replay of the same event (or the same memory
-- accessed twice across paired devices) collapses to one row. target_uid is the
-- grouping key the fold reduces over.
--
-- memory_strength is the materialized fold cache over memory_accesses, keyed by
-- target. Recall ranking reads access_count + last_accessed_at and composes a
-- multiplicative [0,1] strength multiplier alongside the existing type weights
-- and recency boost. The time-dependent decay term e^(-lambda*age) is computed
-- AT QUERY TIME from (now - last_accessed_at), exactly like the recency boost —
-- it is never stored, which is what keeps the stored fold replay-stable
-- (access_count is set cardinality, first/last are MIN/MAX, all reorder-
-- invariant; the same access set in any order yields byte-identical columns).
--
-- salience_base defaults to the kernel constant (0.5) rather than being carried
-- per-event, so the fold stays one-dimensional. demoted_at is a PROJECTION-side
-- column written only by the periodic decay/demote sweep — it is NEVER replayed
-- from the log. So `maz rebuild` re-folds memory_strength from the log and
-- leaves demoted_at NULL: a demoted memory is RESTORED to hot recall on rebuild
-- (the sweep has not run yet), which is the demote-not-delete invariant.
--
-- scope DEFAULT 'personal' on memory_accesses matches kernel.DefaultScope and is
-- the fail-safe direction for the #206 read boundary.
--
--   memory_accesses
--     access_event_uid  the memoryAccessed event UID (OR-set element tag)
--     target_uid        the surfaced memory's ULID (fold grouping key)
--     accessed_at       RFC3339Nano instant recall surfaced the memory
--     scope             personal|project|team (#206 read isolation)
--   memory_strength
--     target_uid        the surfaced memory's ULID (primary key)
--     access_count      folded set cardinality
--     first_accessed_at folded MIN(accessed_at)
--     last_accessed_at  folded MAX(accessed_at); query-time decay anchor
--     salience_base     reinforcement base (kernel constant default)
--     demoted_at        sweep-written; NULL = hot. Never replayed from the log.

CREATE TABLE memory_accesses (
    access_event_uid TEXT PRIMARY KEY,
    target_uid       TEXT NOT NULL,
    accessed_at      TEXT NOT NULL,
    scope            TEXT NOT NULL DEFAULT 'personal'
);

CREATE INDEX idx_memory_accesses_target_uid ON memory_accesses (target_uid);

CREATE TABLE memory_strength (
    target_uid        TEXT PRIMARY KEY,
    access_count      INTEGER NOT NULL DEFAULT 0,
    first_accessed_at TEXT,
    last_accessed_at  TEXT,
    salience_base     REAL NOT NULL DEFAULT 0.5,
    demoted_at        TEXT
);

CREATE INDEX idx_memory_strength_demoted_at ON memory_strength (demoted_at);
