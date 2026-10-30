-- 0053_device_subkey down: SQLite does not support DROP COLUMN before 3.35.0
-- and the device_revocations table drop would orphan event-log entries.
-- The down migration is intentionally a no-op: a full rebuild from the
-- event log re-derives both the vouch columns and the revocation projection.

SELECT 1;
