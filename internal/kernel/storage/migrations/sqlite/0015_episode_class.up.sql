-- 0015_episode_class: add a `class` column to episodes for memory typing.
--
-- Phase 3 of the mem0-parity sweep adds semantic / episodic / procedural
-- classification to Episode. The retrieval lane multiplies the fused
-- score by a per-class type_weight (semantic 0.6, episodic 0.3,
-- procedural 0.1 — mem0 v3 defaults) so context-stable facts surface
-- above session chatter on fact-seeking queries.
--
-- Backward compatibility: the column defaults to 'episodic' for every
-- existing row, matching the JSON unmarshal default in
-- ontology/episode.go. The extractor's heuristic classifier (in
-- kernel/extractor/classifier.go) promotes self-statements to
-- 'semantic' and assistant-actor numbered traces to 'procedural'
-- going forward.
--
-- The CHECK constraint pins the three legal values at the schema layer
-- so a bad write fails loudly rather than silently storing a typo
-- that the retrieval type-weight lookup would then default-to-0.3
-- (silently misclassifying as episodic). New classes need a migration.

ALTER TABLE episodes
    ADD COLUMN class TEXT NOT NULL DEFAULT 'episodic'
        CHECK (class IN ('semantic', 'episodic', 'procedural'));

-- Index on class to keep type-weight aggregation queries (and a
-- future "show me all my semantic memories" surface) off the
-- full-table scan path.
CREATE INDEX episodes_class ON episodes (class);
