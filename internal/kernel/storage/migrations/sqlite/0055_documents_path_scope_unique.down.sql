-- Down 0055: revert documents to inline single-column `path UNIQUE`.
--
-- The runner is forward-only (it never executes .down.sql); this file exists for
-- parity and manual rollback. Where composite uniqueness allowed two rows to
-- share a path across scopes, single-column UNIQUE(path) can no longer hold both,
-- so this keeps the most-recently-updated row per path (update_time DESC,
-- document_uid DESC tiebreak) — the same convergence the read spine applies. It
-- is lossy by construction; a content-preserving rollback rebuilds from the log.

ALTER TABLE documents RENAME TO documents_old;

CREATE TABLE documents (
    document_uid          TEXT    PRIMARY KEY NOT NULL,
    path                  TEXT    UNIQUE NOT NULL,
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
    canonical             INTEGER NOT NULL DEFAULT 0
);

INSERT INTO documents
    (document_uid, path, slug, title, body, frontmatter_json, frontmatter_raw,
     content_hash, revision_uid, pinned, create_time, update_time,
     unresolved_links_json, deleted, scope, canonical)
SELECT
     document_uid, path, slug, title, body, frontmatter_json, frontmatter_raw,
     content_hash, revision_uid, pinned, create_time, update_time,
     unresolved_links_json, deleted, scope, canonical
FROM documents_old d
WHERE d.document_uid = (
    SELECT d2.document_uid FROM documents_old d2
    WHERE d2.path = d.path
    ORDER BY d2.update_time DESC, d2.document_uid DESC
    LIMIT 1
);

DROP TABLE documents_old;

CREATE INDEX documents_path        ON documents (path);
CREATE INDEX documents_pinned      ON documents (pinned) WHERE pinned = 1;
CREATE INDEX documents_update_time ON documents (update_time);
CREATE INDEX idx_documents_scope   ON documents (scope);
CREATE INDEX documents_canonical   ON documents (document_uid) WHERE canonical = 1;

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

INSERT INTO documents_fts(documents_fts) VALUES('rebuild');

INSERT INTO documents_fts(documents_fts, rowid, title, body)
SELECT 'delete', rowid, title, body FROM documents WHERE deleted = 1;
