-- 0054_vec_scope down: SQLite does not support DROP COLUMN portably across all
-- versions in the build matrix; the scope column is additive and the project is
-- append-only pre-production. This migration is a documented no-op for the
-- forward-only migration runner.
SELECT 1;
