-- 0052_team_role down: SQLite does not support DROP COLUMN before 3.35.0,
-- and even modern builds may run on older system SQLite. The down migration is
-- intentionally a no-op: the role column's default ('member') means existing
-- rows stay valid for the pre-RBAC fold, and the column causes no schema
-- conflict on rollback. A full rebuild from the event log re-derives the ACL.

SELECT 1;
