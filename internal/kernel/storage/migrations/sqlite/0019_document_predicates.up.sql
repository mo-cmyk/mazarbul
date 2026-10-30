-- 0019_document_predicates: register the curated set of document-relation
-- predicates introduced by the typed wikilink syntax in P6.
--
-- Document bodies may use [relType:confidence] [[Target]] or [[relType::Target]]
-- to express richer semantic relationships than the default mentions/links_to
-- edges. This migration seeds both the core wikilink predicates (mentions,
-- links_to) that the P1 importer already emits and the curated set of typed
-- document-relation predicates so the resolver's validation and the graph
-- never see unregistered predicates from imported vault bodies.
--
-- All document predicates are non-functional: a document may simultaneously
-- mention, support, and contradict multiple targets; there is no "at most one
-- current value per subject" semantics on any of them.
--
-- INSERT OR IGNORE keeps the migration idempotent on databases that already
-- have these rows (e.g. a partial manual insert or a future re-run).

INSERT OR IGNORE INTO predicates (predicate, kind, description, added_at, added_by)
VALUES
    -- Core wikilink predicates (already emitted by the P1 importer; seeded
    -- here so the predicates table is self-describing and the resolver's
    -- validate path never needs to register them on-the-fly).
    ('mentions',    'non-functional',
     'Document references an entity (person, organisation, place, event, etc.).', '2026-06-05T00:00:00Z', 'seed'),
    ('links_to',    'non-functional',
     'Document links to another document in the vault.',                           '2026-06-05T00:00:00Z', 'seed'),

    -- Typed wikilink predicates: a curated closed set chosen from the plan.
    -- Authors use [relates_to] [[Target]], [contradicts:0.8] [[Claim]], etc.
    -- Unknown relTypes in a body fall back to mentions/links_to at ingest time
    -- (logged at debug) rather than being rejected.
    ('relates_to',  'non-functional',
     'Document asserts a generic semantic relationship with the target.',           '2026-06-05T00:00:00Z', 'seed'),
    ('contradicts', 'non-functional',
     'Document presents evidence or argument that contradicts the target.',         '2026-06-05T00:00:00Z', 'seed'),
    ('supports',    'non-functional',
     'Document presents evidence or argument that supports the target.',            '2026-06-05T00:00:00Z', 'seed'),
    ('blocks',      'non-functional',
     'Document asserts that a task, decision, or work item blocks the target.',     '2026-06-05T00:00:00Z', 'seed'),
    ('depends_on',  'non-functional',
     'Document asserts that the subject depends on the target.',                    '2026-06-05T00:00:00Z', 'seed'),
    ('part_of',     'non-functional',
     'Document asserts that the subject is a component or sub-set of the target.', '2026-06-05T00:00:00Z', 'seed');
