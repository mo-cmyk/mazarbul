-- Migration 0041: register the `describes` predicate for frontmatter about: links.
--
-- The `about:` frontmatter key expresses a high-confidence authorial statement
-- that a document describes its primary subject (person, organization, project,
-- etc.). Before this migration the key was silently mis-wired to the `mentions`
-- predicate, which is the weak default for body wikilinks. This migration
-- registers the separate `describes` predicate so the graph accurately records
-- the authorial intent.
--
-- Mirrors the INSERT pattern from migration 0019 (document predicates).
-- `describes` is non-functional: a document may describe multiple subjects
-- (e.g. a meeting note about two projects). It is a doc-origin predicate only:
-- the importer emits it exclusively from the `about:` frontmatter key, never
-- from body wikilinks or prose extraction.
--
-- INSERT OR IGNORE keeps the migration idempotent on databases that already
-- have this row from a manual insert or a prior partial run.

INSERT OR IGNORE INTO predicates (predicate, kind, description, added_at, added_by)
VALUES (
    'describes',
    'non-functional',
    'Document describes its primary subject (set via the about: frontmatter key).',
    '2026-06-24T00:00:00Z',
    'seed'
);
