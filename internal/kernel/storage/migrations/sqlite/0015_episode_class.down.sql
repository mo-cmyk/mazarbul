-- Reverse of 0015_episode_class. SQLite supports ALTER TABLE ... DROP
-- COLUMN as of 3.35; the codebase requires 3.45+ via sqlite-vec, so
-- the simple form is sufficient. Downgrading wipes class data; a
-- production rollback would prefer 0015_episode_class_compat.down.sql
-- that preserves the values under a different name. M1 doesn't.

DROP INDEX IF EXISTS episodes_class;
ALTER TABLE episodes DROP COLUMN class;
