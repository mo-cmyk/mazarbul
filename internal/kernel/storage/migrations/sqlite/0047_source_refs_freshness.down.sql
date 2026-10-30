-- Down migration 0046: drop the captureExternal freshness token + tombstone columns.
ALTER TABLE source_refs DROP COLUMN tombstone;
ALTER TABLE source_refs DROP COLUMN freshness_token;
