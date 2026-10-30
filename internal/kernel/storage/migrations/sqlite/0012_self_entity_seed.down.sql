-- Reverse 0012: remove the canonical self entity + has_name predicate.
-- Both DELETEs are no-ops when the rows are absent.

DELETE FROM predicates WHERE predicate = 'has_name' AND added_by = 'seed';
DELETE FROM entities WHERE uid = 'self';
