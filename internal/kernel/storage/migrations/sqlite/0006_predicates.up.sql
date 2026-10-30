-- 0006_predicates: predicate vocabulary table with seed data.
--
-- The predicates table records whether each relation predicate is functional
-- (at most one current value per subject) or non-functional (multiple values
-- allowed). The kernel resolver uses this to decide whether to auto-supersede
-- an existing relation or to run a contradiction check first.
--
-- Ten seed predicates are inserted with added_by='seed'. Unknown predicates
-- discovered at runtime are classified by the LLM and inserted via the
-- Register path in the resolver.

CREATE TABLE predicates (
    predicate   TEXT PRIMARY KEY,
    kind        TEXT NOT NULL CHECK(kind IN ('functional','non-functional')),
    description TEXT NOT NULL DEFAULT '',
    added_at    TEXT NOT NULL,
    added_by    TEXT NOT NULL CHECK(added_by IN ('seed','llm','user'))
);

-- Seed the ten canonical predicates. Every seed row is functional and was
-- added at the plan-bootstrap date (2026-05-25T00:00:00Z).
INSERT INTO predicates (predicate, kind, description, added_at, added_by) VALUES
    ('works_at',   'functional', 'Subject is employed at the object organisation.', '2026-05-25T00:00:00Z', 'seed'),
    ('lives_in',   'functional', 'Subject resides in the object location.',         '2026-05-25T00:00:00Z', 'seed'),
    ('born_in',    'functional', 'Subject was born in the object location.',        '2026-05-25T00:00:00Z', 'seed'),
    ('child_of',   'functional', 'Subject is a child of the object person.',        '2026-05-25T00:00:00Z', 'seed'),
    ('parent_of',  'functional', 'Subject is a parent of the object person.',       '2026-05-25T00:00:00Z', 'seed'),
    ('married_to', 'functional', 'Subject is married to the object person.',        '2026-05-25T00:00:00Z', 'seed'),
    ('member_of',  'functional', 'Subject is a member of the object group.',        '2026-05-25T00:00:00Z', 'seed'),
    ('located_in', 'functional', 'Subject is physically located in the object.',    '2026-05-25T00:00:00Z', 'seed'),
    ('has_role',   'functional', 'Subject holds the object role or title.',          '2026-05-25T00:00:00Z', 'seed'),
    ('owns',       'functional', 'Subject owns the object.',                         '2026-05-25T00:00:00Z', 'seed');
