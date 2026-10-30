-- Reverse of 0019: remove the document-relation predicates. The core
-- predicates (mentions, links_to) are also removed since they were seeded
-- here; the P1 importer keeps emitting them regardless.
DELETE FROM predicates WHERE predicate IN (
    'mentions', 'links_to',
    'relates_to', 'contradicts', 'supports', 'blocks', 'depends_on', 'part_of'
);
