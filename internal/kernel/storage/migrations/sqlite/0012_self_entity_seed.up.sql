-- 0012_self_entity_seed: seed the canonical "self" entity + has_name predicate.
--
-- The self entity is the subject of every first-person assertion the
-- extractor's lexical self-statement detector emits. Without a seeded
-- row, the resolver would try to mint a fresh entity for every
-- "I ..." statement, and supersede would never close prior facts
-- because each utterance creates a new subject UID.
--
-- The has_name predicate is added as functional so the resolver
-- automatically closes the prior open name row when a new name
-- appears. has_name was not part of the 0006_predicates seed list
-- because that migration only covered the canonical 10 predicates
-- the LLM extractor produces from third-person episodes; first-
-- person self-statements need has_name as well.
--
-- Both inserts are idempotent (INSERT OR IGNORE) so re-running the
-- migration on a populated database is a no-op.

INSERT OR IGNORE INTO entities (uid, kind, display_name, confidence, resolution_status)
VALUES ('self', 'person', 'Me', 1.0, 'resolved');

INSERT OR IGNORE INTO predicates (predicate, kind, description, added_at, added_by)
VALUES ('has_name', 'functional', 'Subject is currently known by the object name.',
        '2026-05-30T00:00:00Z', 'seed');
