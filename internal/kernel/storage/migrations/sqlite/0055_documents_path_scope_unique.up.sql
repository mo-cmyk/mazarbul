-- Migration 0055: documents uniqueness (path) -> (path, scope).
--
-- 0016 declared `path TEXT UNIQUE NOT NULL`, a GLOBAL path namespace: a personal
-- notes/foo.md and a team notes/foo.md cannot coexist. On a device consuming a
-- team document whose path already exists in personal scope, the second INSERT
-- (a different document_uid) violates path UNIQUE, applyDocumentRevision errors,
-- and live consume / Rebuild wedges. Composite
-- UNIQUE(path, scope) makes cross-scope path collisions structurally impossible;
-- the scope-partitioned vault (internal/sync/vault) keeps the on-disk surface
-- separated so both documents materialize to distinct files.
--
-- The inline `path UNIQUE` creates an implicit auto-index that ALTER cannot drop,
-- so the table is rebuilt (idiom: migration 0048). The whole script runs in one
-- transaction (sqlite.applyMigration), so a failure rolls the rebuild back atomically.
--
-- documents_fts is an FTS5 external-content table bound to documents by rowid.
-- The rebuild changes rowids and DROP TABLE documents (its old, renamed copy)
-- removes the AFTER INSERT/UPDATE/DELETE triggers, so the triggers are recreated
-- verbatim and the FTS index is rebuilt from the new rows. documents_fts itself
-- is NOT dropped (its rank config lives in shadow tables); only the index content
-- is stale, which 'rebuild' repairs. documents_vec / documents_vec_ids are NOT
-- touched: they key on document_uid (stable across the rebuild), not on rowid.

-- 1. Rename the current table aside. Its triggers move with it; documents_fts'
--    content='documents' now dangles until step 2 recreates the name.
ALTER TABLE documents RENAME TO documents_old;

-- 2. New table: identical columns in the same physical order as the live schema
--    (0016 + scope@0027 + canonical@0043), but with a table-level UNIQUE(path,
--    scope) instead of the inline `path UNIQUE`.
CREATE TABLE documents (
    document_uid          TEXT    PRIMARY KEY NOT NULL,
    path                  TEXT    NOT NULL,
    slug                  TEXT    NOT NULL DEFAULT '',
    title                 TEXT    NOT NULL DEFAULT '',
    body                  TEXT    NOT NULL DEFAULT '',
    frontmatter_json      TEXT    NOT NULL DEFAULT '{}',
    frontmatter_raw       TEXT    NOT NULL DEFAULT '',
    content_hash          TEXT    NOT NULL,
    revision_uid          TEXT    NOT NULL,
    pinned                INTEGER NOT NULL DEFAULT 0,
    create_time           TEXT    NOT NULL,
    update_time           TEXT    NOT NULL,
    unresolved_links_json TEXT    NOT NULL DEFAULT '[]',
    deleted               INTEGER NOT NULL DEFAULT 0,
    scope                 TEXT    NOT NULL DEFAULT 'personal',
    canonical             INTEGER NOT NULL DEFAULT 0,
    UNIQUE(path, scope)
);

-- 3. Copy every row verbatim (columns named explicitly, order-independent).
--    Existing rows already carry scope (0027) and canonical (0043); no transform.
--    Uniqueness holds trivially: the old `path UNIQUE` guaranteed path alone was
--    unique, so (path, scope) is unique for the copied set. The new table has no
--    triggers yet, so this bulk copy fires no per-row FTS writes.
INSERT INTO documents
    (document_uid, path, slug, title, body, frontmatter_json, frontmatter_raw,
     content_hash, revision_uid, pinned, create_time, update_time,
     unresolved_links_json, deleted, scope, canonical)
SELECT
     document_uid, path, slug, title, body, frontmatter_json, frontmatter_raw,
     content_hash, revision_uid, pinned, create_time, update_time,
     unresolved_links_json, deleted, scope, canonical
FROM documents_old;

-- 4. Drop the old table. This also drops the AFTER INSERT/UPDATE/DELETE triggers
--    (documents_ai/ad/au) that moved to documents_old in step 1.
DROP TABLE documents_old;

-- 5. Recreate the relational indexes verbatim (0016 + 0027 + 0043). The implicit
--    auto-index for UNIQUE(path, scope) is created by the table definition above.
CREATE INDEX documents_path        ON documents (path);
CREATE INDEX documents_pinned      ON documents (pinned) WHERE pinned = 1;
CREATE INDEX documents_update_time ON documents (update_time);
CREATE INDEX idx_documents_scope   ON documents (scope);
CREATE INDEX documents_canonical   ON documents (document_uid) WHERE canonical = 1;

-- 6. Recreate the three FTS5 sync triggers verbatim (dropped with documents_old
--    in step 4). Bodies are byte-identical to migration 0016.
CREATE TRIGGER documents_ai AFTER INSERT ON documents BEGIN
    INSERT INTO documents_fts(rowid, title, body)
    SELECT new.rowid, new.title, new.body WHERE new.deleted = 0;
END;
CREATE TRIGGER documents_ad AFTER DELETE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, body)
    SELECT 'delete', old.rowid, old.title, old.body WHERE old.deleted = 0;
END;
CREATE TRIGGER documents_au AFTER UPDATE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, body)
    SELECT 'delete', old.rowid, old.title, old.body WHERE old.deleted = 0;
    INSERT INTO documents_fts(rowid, title, body)
    SELECT new.rowid, new.title, new.body WHERE new.deleted = 0;
END;

-- 7. Rebuild the FTS index against the new rows/rowids. 'rebuild' re-reads the
--    content table (documents) and reconstructs the index from scratch; it does
--    not depend on the triggers. Must run AFTER documents is populated (step 3)
--    and renamed into place.
INSERT INTO documents_fts(documents_fts) VALUES('rebuild');

-- 8. 'rebuild' indexes EVERY content row, including tombstones (deleted=1),
--    because it reads columns directly and ignores the triggers' WHERE guards.
--    Delete the tombstoned rows from the FTS index to restore the invariant that
--    soft-deleted documents never surface in full-text search (mirrors the
--    documents_au / documents_ad delete leg).
INSERT INTO documents_fts(documents_fts, rowid, title, body)
SELECT 'delete', rowid, title, body FROM documents WHERE deleted = 1;
