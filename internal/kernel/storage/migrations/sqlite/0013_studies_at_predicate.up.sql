-- 0013_studies_at_predicate: register studies_at as a functional
-- predicate so the resolver supersede pipeline closes the prior open
-- studies_at row when the user enrolls elsewhere.
--
-- studies_at was not part of the 0006 canonical seed list (which only
-- covered the LLM extractor's third-person predicates); the v6
-- selfstatement.go detector adds first-person "I study at X" patterns
-- that need this predicate registered before the first insert hits the
-- relations table.
--
-- INSERT OR IGNORE keeps the migration idempotent on databases that
-- already have studies_at from a future ad-hoc insert.

INSERT OR IGNORE INTO predicates (predicate, kind, description, added_at, added_by)
VALUES ('studies_at', 'functional', 'Subject is currently enrolled at the object organisation.',
        '2026-05-30T00:00:00Z', 'seed');
