-- Migration 0049: add hlc column to facts for convergent slot ordering.
--
-- The fact-slot suppression gate (a content-hash pre-check that skips
-- writeFactRow when the open slot already holds the same value) is
-- arrival-order-sensitive: two devices grafting the same event log in
-- different orders suppress different rows and converge to different slot
-- state. Removing that gate and ordering the slot fold by HLC instead of
-- (event_start, create_time) makes the fold commutative and idempotent.
--
-- Each fact row now carries the HLC stamp of its carrying event, encoded
-- as 16 lowercase hex characters: 12-hex wall-milliseconds followed by
-- 4-hex logical counter (identical to hlc.Encode output). Existing rows
-- are backfilled with their implicit HLC — Encode(create_time.UnixMilli(), 0)
-- — computed here via SQLite's julianday arithmetic, which independently
-- reproduces what hlc.Implicit(t) computes in Go. Lexicographic order of
-- the encoded string equals numeric (causal) order, so ORDER BY hlc ASC
-- yields the same causal sequence as the Go-side sort.
--
-- The logical counter is 0 for all backfilled rows because no pre-wave
-- event has a real logical counter; the wall-millisecond component alone
-- fully orders events that do not share a millisecond, and the uid
-- tiebreaker in readFactSlot resolves same-millisecond ties.

ALTER TABLE facts ADD COLUMN hlc TEXT;

UPDATE facts
   SET hlc = printf('%012x%04x',
                    CAST(ROUND((julianday(create_time) - 2440587.5) * 86400000.0) AS INTEGER),
                    0);

-- Composite covering index for readFactSlot: scans by (subject_uid,
-- state_key, scope) and orders by (hlc, uid). A covering index over all
-- five columns lets SQLite satisfy the scan without a table lookup, keeping
-- the fold fast even for slots with many historical rows.
CREATE INDEX idx_facts_hlc_order ON facts (subject_uid, state_key, scope, hlc, uid);
