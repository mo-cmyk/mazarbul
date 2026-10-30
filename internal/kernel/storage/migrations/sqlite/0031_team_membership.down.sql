-- Reverse 0031_team_membership.
DROP INDEX IF EXISTS team_membership_events_author;
DROP INDEX IF EXISTS team_membership_events_subject;
DROP TABLE IF EXISTS team_membership_acl;
DROP TABLE IF EXISTS team_membership_events;
