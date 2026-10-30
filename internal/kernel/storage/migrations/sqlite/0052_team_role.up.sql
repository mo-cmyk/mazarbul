-- 0052_team_role: role column for team membership RBAC (owner/admin/member/reader).
--
-- team_membership_events gains a role column that is populated for
-- teamMemberGranted events (initial role at admission time) and the new
-- teamRoleChanged events (role change, supersedes the admission role when
-- its ULID is higher). The column is NOT meaningful for teamMemberRevoked
-- rows and defaults to 'member' there via the constraint default.
--
-- team_membership_acl gains a role column that is fully recomputed on every
-- membership event apply (same DELETE + re-INSERT pattern as the rest of the
-- ACL). No backfill of existing rows is needed: a subsequent daemon restart
-- triggers a Rebuild, which re-folds the full event set; any membership
-- event WITHOUT an explicit role carries the column's default ('member') and
-- the fold's own default kicks in for the genesis row (always 'owner').

ALTER TABLE team_membership_events ADD COLUMN role TEXT NOT NULL DEFAULT 'member';

ALTER TABLE team_membership_acl ADD COLUMN role TEXT NOT NULL DEFAULT 'member';
