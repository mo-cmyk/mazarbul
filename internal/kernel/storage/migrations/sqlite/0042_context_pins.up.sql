-- Migration 0042: user context pins + corrections projection (Insight Inbox /
-- context pin/correct features). Numbered 0042 to sit past the parallel
-- agent-feedback wave's 0040/0041 (which land first in the shared store);
-- the migration runner applies in version order and tolerates the gap.
--
-- context_pins materializes the durable user-authored steering the cold-start
-- brief reads back: a contextPinned event lands one row with pin_type='pin',
-- and a contextCorrected event lands one row with pin_type='correction'
-- (target_ref carries the corrected item's id). The single table + discriminator
-- lets one reader (Navigator.ContextPins) return both. The dispatcher inserts
-- via INSERT OR IGNORE keyed on uid, so a Rebuild replay of the same event is a
-- no-op and the projection reconstructs deterministically from the JSONL log.
--
-- active is a materialized CONVERGENT FOLD, not a value the pin event carries:
-- it is 1 unless at least one context_pin_tombstones row targets the pin's uid
-- (a contextUnpinned), in which case it is 0. The dispatcher recomputes it from
-- BOTH the contextPinned/Corrected arm and the contextUnpinned arm, so the
-- result is order-independent — an unpin that replays before its pin still
-- converges to active=0 once the pin row lands. The pin row is preserved (audit
-- / resurrect), never deleted, mirroring the relationRetracted tombstone model.
--
-- context_pin_tombstones is the OR-set of retirement events: one row per
-- contextUnpinned, keyed by that event's own UID so a replay is idempotent.
-- pin_uid is the grouping key the active fold reads.
--
-- scope DEFAULT 'personal' matches kernel.DefaultScope, the fail-safe direction
-- for the #206 read boundary: an untagged event materializes as personal
-- (visible under the default {project,personal} read, never leaked to team).
--
--   context_pins
--     uid              pin/correction ULID identity (contextPins/{uid} name)
--     pin_type         'pin' (contextPinned) | 'correction' (contextCorrected)
--     text             the steering note (pin) or corrected text (correction)
--     target_ref       pin: optional entity/resource ref; correction: the
--                      corrected item's TargetId (NULL when a pin omits it)
--     active           1 live | 0 retired (materialized fold over tombstones)
--     source_event_uid the carrying contextPinned/contextCorrected event UID
--     create_time      RFC3339Nano event create time
--     scope            personal|project|team (#206 read isolation)
--
--   context_pin_tombstones
--     tombstone_event_uid the contextUnpinned event UID (primary key; OR-set)
--     pin_uid             the context_pins.uid being retired (grouping key)
--     unpinned_at         RFC3339Nano retirement instant
--     scope               personal|project|team (#206 read isolation)

CREATE TABLE context_pins (
    uid              TEXT PRIMARY KEY,
    pin_type         TEXT NOT NULL DEFAULT 'pin' CHECK (pin_type IN ('pin', 'correction')),
    text             TEXT NOT NULL,
    target_ref       TEXT,
    active           INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    source_event_uid TEXT NOT NULL,
    create_time      TEXT NOT NULL,
    scope            TEXT NOT NULL DEFAULT 'personal'
);

CREATE INDEX idx_context_pins_active     ON context_pins (active);
CREATE INDEX idx_context_pins_scope      ON context_pins (scope);
CREATE INDEX idx_context_pins_type       ON context_pins (pin_type);
CREATE INDEX idx_context_pins_target_ref ON context_pins (target_ref);

CREATE TABLE context_pin_tombstones (
    tombstone_event_uid TEXT PRIMARY KEY,
    pin_uid             TEXT NOT NULL,
    unpinned_at         TEXT NOT NULL,
    scope               TEXT NOT NULL DEFAULT 'personal'
);

CREATE INDEX idx_context_pin_tombstones_pin ON context_pin_tombstones (pin_uid);
