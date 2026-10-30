-- 0013_studies_at_predicate (down): remove the studies_at predicate
-- row. Existing relations rows referencing studies_at are left in
-- place — predicates is metadata, not a FK enforcer.
DELETE FROM predicates WHERE predicate = 'studies_at';
