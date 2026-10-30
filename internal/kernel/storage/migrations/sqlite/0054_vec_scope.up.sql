-- 0054_vec_scope: add scope column to all *_vec_ids sidecar tables.
--
-- The authoritative scope boundary is the relational read spine
-- (scoped_reader.go). The sidecar scope column is defense-in-depth: every KNN
-- CTE gains an AND scope IN (...) predicate so cross-scope vector leakage is
-- prevented even when the KNN function is called directly without the relational
-- guard. Mirrors the rationale documented in migration 0027 for the base tables.
--
-- DEFAULT 'personal' matches kernel.DefaultScope, the fail-safe direction: a
-- sidecar row whose scope cannot be inferred from the base table stays personal
-- rather than leaking into a broader scope. Accurate scope materialises on the
-- next maz rebuild, which replays all events and re-upserts sidecar rows with
-- the per-event scope.
--
-- Backfill: UPDATE each sidecar from its corresponding base table. Rows with no
-- matching base-table entry (orphaned sidecars that rebuild would clean up)
-- retain the DEFAULT 'personal'. The documents sidecar uses its document_uid
-- column directly (no parsing needed).

ALTER TABLE episodes_vec_ids     ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE entities_vec_ids     ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE mentions_vec_ids     ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE documents_vec_ids    ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE insights_vec_ids     ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE consolidated_vec_ids ADD COLUMN scope TEXT NOT NULL DEFAULT 'personal';

-- Backfill episodes_vec_ids from episodes.
UPDATE episodes_vec_ids
   SET scope = (SELECT e.scope FROM episodes e WHERE e.uid = episodes_vec_ids.id)
 WHERE (SELECT e.scope FROM episodes e WHERE e.uid = episodes_vec_ids.id) IS NOT NULL;

-- Backfill entities_vec_ids from entities (entities_vec_ids.id is the entity UID).
UPDATE entities_vec_ids
   SET scope = (SELECT e.scope FROM entities e WHERE e.uid = entities_vec_ids.id)
 WHERE (SELECT e.scope FROM entities e WHERE e.uid = entities_vec_ids.id) IS NOT NULL;

-- Backfill mentions_vec_ids from mentions.
UPDATE mentions_vec_ids
   SET scope = (SELECT m.scope FROM mentions m WHERE m.uid = mentions_vec_ids.id)
 WHERE (SELECT m.scope FROM mentions m WHERE m.uid = mentions_vec_ids.id) IS NOT NULL;

-- Backfill documents_vec_ids using its document_uid column (avoids parsing
-- the "<documentUid>#<chunkIdx>" id format entirely).
UPDATE documents_vec_ids
   SET scope = (SELECT d.scope FROM documents d WHERE d.document_uid = documents_vec_ids.document_uid)
 WHERE (SELECT d.scope FROM documents d WHERE d.document_uid = documents_vec_ids.document_uid) IS NOT NULL;

-- Backfill insights_vec_ids from insights.
UPDATE insights_vec_ids
   SET scope = (SELECT i.scope FROM insights i WHERE i.uid = insights_vec_ids.id)
 WHERE (SELECT i.scope FROM insights i WHERE i.uid = insights_vec_ids.id) IS NOT NULL;

-- Backfill consolidated_vec_ids from consolidated_memories.
UPDATE consolidated_vec_ids
   SET scope = (SELECT c.scope FROM consolidated_memories c WHERE c.uid = consolidated_vec_ids.id)
 WHERE (SELECT c.scope FROM consolidated_memories c WHERE c.uid = consolidated_vec_ids.id) IS NOT NULL;
