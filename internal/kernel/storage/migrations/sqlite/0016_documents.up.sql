-- 0016_documents: first-class document storage for the markdown vault layer.
--
-- Documents are event-sourced: each meaningful save emits a documentRevision
-- event whose payload carries the verbatim body. This table is the derived
-- relational projection rebuilt from the append-only event log via Rebuild.
--
-- Three sub-sections:
--   1. documents:        relational table (one row per document, keyed by
--                        document_uid; updated on each revision apply).
--   2. documents_fts:    FTS5 external-content virtual table over title+body,
--                        maintained by AFTER INSERT/UPDATE/DELETE triggers.
--   3. documents_vec /   vec0 virtual table for per-chunk body embeddings +
--      documents_vec_ids sidecar table mapping chunk IDs to vec0 rowids.
--
-- The {{VECTOR_DIM}} placeholder is substituted at migration-load time from
-- storage.VectorDim by the runner in internal/kernel/storage/sqlite/migrate.go,
-- exactly as in migrations 0003 and 0005.

-- ============================================================================
-- 1. documents: relational table
-- ============================================================================

CREATE TABLE documents (
    -- document_uid is the stable ULID identity of the document across all its
    -- revisions. Immutable: even when the document is moved to a new path, this
    -- UID stays constant so wikilinks that resolve by entity UID survive renames.
    document_uid          TEXT    PRIMARY KEY NOT NULL,

    -- path is the vault-relative POSIX path, e.g. "notes/project-alpha.md".
    -- UNIQUE so two documents cannot occupy the same path. moveDocument (P2)
    -- updates this column on a rename.
    path                  TEXT    UNIQUE NOT NULL,

    -- slug is the URL-safe identifier derived from the filename at first sight
    -- (frozen so renames do not break rendered/documents/ paths in git). Derived
    -- by the dispatcher from the path at apply time.
    slug                  TEXT    NOT NULL DEFAULT '',

    -- title is extracted from the first top-level heading or frontmatter title.
    title                 TEXT    NOT NULL DEFAULT '',

    -- body is the VERBATIM file body, never normalised. The FTS5 triggers index
    -- this column; the vault projector writes it byte-for-byte to disk.
    body                  TEXT    NOT NULL DEFAULT '',

    -- frontmatter_json is the YAML frontmatter decoded to JSON. Empty object
    -- '{}' when no frontmatter block is present. Used for structured queries
    -- (pinned, tags, entity frontmatter fields).
    frontmatter_json      TEXT    NOT NULL DEFAULT '{}',

    -- frontmatter_raw holds the exact frontmatter bytes including the ---
    -- delimiters, for byte-stable round-trips to the vault projector.
    frontmatter_raw       TEXT    NOT NULL DEFAULT '',

    -- content_hash is the SHA-256 hex digest of the exact file bytes. The
    -- content-hash idempotency check (I1) compares on-disk hash to this value
    -- to skip no-op revisions.
    content_hash          TEXT    NOT NULL,

    -- revision_uid is the event UID of the most recent documentRevision event
    -- applied to this row. Used by HasEvent to detect already-applied revisions.
    revision_uid          TEXT    NOT NULL,

    -- pinned marks documents that are always included in the context brief.
    -- SQLite boolean: 1 = true, 0 = false.
    pinned                INTEGER NOT NULL DEFAULT 0,

    -- create_time is the RFC 3339 UTC timestamp of the first revision applied
    -- to this row. SET ON INSERT ONLY; not updated on subsequent revisions.
    create_time           TEXT    NOT NULL,

    -- update_time is the RFC 3339 UTC timestamp of the most recent revision.
    -- Updated on every revision apply, including the soft-delete tombstone.
    update_time           TEXT    NOT NULL,

    -- unresolved_links_json holds wikilinks that could not be resolved to a
    -- canonical entity at ingest time. JSON array of WikiLink objects. P1
    -- reconciles these when a matching entity/doc later appears.
    unresolved_links_json TEXT    NOT NULL DEFAULT '[]',

    -- deleted marks soft-deleted documents (documentDeleted event applied).
    -- 1 = deleted (tombstone row); 0 = live. The row is kept for Rebuild
    -- idempotency; recall lanes filter deleted = 0.
    deleted               INTEGER NOT NULL DEFAULT 0
);

-- Index on path for fast path-keyed reads (readDocument, import dedup).
CREATE INDEX documents_path       ON documents (path);

-- Partial index on pinned for the context brief assembler (pinned = 1 rows only).
CREATE INDEX documents_pinned     ON documents (pinned) WHERE pinned = 1;

-- Index on update_time for recency-ordered listing (listDocuments, hot brief).
CREATE INDEX documents_update_time ON documents (update_time);

-- ============================================================================
-- 2. documents_fts: FTS5 external-content over title + body
-- ============================================================================

-- The FTS5 virtual table indexes title and body of the documents table.
-- content='documents' makes it an external-content table: SQLite stores only
-- the FTS index data, not a second copy of the text. content_rowid='rowid'
-- binds the FTS row to the documents.rowid for snippet() lookups.
--
-- Tokenizer: same as episodes_fts for consistency (unicode61, remove_diacritics
-- 2, tokenchars '_' so snake_case and ulid-style tokens are one token).
--
-- BM25 weights: title (8.0) ranked higher than body (4.0) so a title hit
-- surfaces above a body hit at equal BM25 otherwise.

CREATE VIRTUAL TABLE documents_fts USING fts5(
    title,
    body,
    content='documents',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2 tokenchars ''_'''
);

INSERT INTO documents_fts(documents_fts, rank) VALUES('rank', 'bm25(8.0,4.0)');

-- Rebuild the FTS index from any rows already present in documents (none at
-- migration time, but guards against future migration ordering changes).
INSERT INTO documents_fts(documents_fts) VALUES('rebuild');

-- AFTER INSERT trigger: index the new document, but ONLY when it is live
-- (deleted = 0). A row inserted directly as a tombstone (deleted = 1, e.g. a
-- documentDeleted replayed before its revisions in a malformed log) must never
-- enter the FTS index. The conditional INSERT ... SELECT ... WHERE skips the
-- index write for a tombstone row.
CREATE TRIGGER documents_ai AFTER INSERT ON documents BEGIN
    INSERT INTO documents_fts(rowid, title, body)
    SELECT new.rowid, new.title, new.body WHERE new.deleted = 0;
END;

-- AFTER DELETE trigger: remove the hard-deleted document from the FTS index.
-- Conditional on old.deleted = 0: a row is in the FTS index iff it was live
-- (documents_ai/documents_au only index live rows). FTS5 external-content
-- tables corrupt ("database disk image is malformed") when the 'delete'
-- command targets a rowid/text pair that is not in the index, so the delete
-- must be skipped for tombstoned rows.
CREATE TRIGGER documents_ad AFTER DELETE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, body)
    SELECT 'delete', old.rowid, old.title, old.body WHERE old.deleted = 0;
END;

-- AFTER UPDATE trigger: drop the old FTS entry (only if the old row was live —
-- see documents_ad on why deleting an unindexed row corrupts FTS5), then
-- re-index ONLY when the new row is live. A soft-delete (deleted 0 -> 1) keeps
-- the row + body for Rebuild determinism but must leave the FTS index, so a
-- tombstoned document stops surfacing in full-text search. The conditional
-- re-index (INSERT ... SELECT ... WHERE new.deleted = 0) is what removes a
-- soft-deleted doc from FTS; an unconditional re-index would re-add the
-- still-present body and keep it searchable. The old.deleted = 0 guard on the
-- delete leg keeps an idempotent re-delete (1 -> 1) and a recreate (1 -> 0)
-- from issuing an FTS delete for a row that was never indexed.
CREATE TRIGGER documents_au AFTER UPDATE ON documents BEGIN
    INSERT INTO documents_fts(documents_fts, rowid, title, body)
    SELECT 'delete', old.rowid, old.title, old.body WHERE old.deleted = 0;
    INSERT INTO documents_fts(rowid, title, body)
    SELECT new.rowid, new.title, new.body WHERE new.deleted = 0;
END;

-- ============================================================================
-- 3. documents_vec + documents_vec_ids: per-chunk body embeddings
-- ============================================================================

-- documents_vec is the vec0 virtual table storing one embedding per body chunk.
-- The {{VECTOR_DIM}} placeholder is substituted at load time from storage.VectorDim.
-- One document may contribute multiple rows (one per chunk produced by ChunkText).

CREATE VIRTUAL TABLE documents_vec USING vec0(
    embedding float[{{VECTOR_DIM}}]
);

-- documents_vec_ids maps the string chunk ID (format: "<documentUid>#<chunkIdx>")
-- to the INTEGER rowid required by vec0. Additional columns (document_uid,
-- chunk_index, heading, byte_offset) let the recall composer hydrate a
-- citation with section anchors (path#heading, lineStart) without a JOIN to
-- documents. These columns are used by P3 (unified recall) and P5 (context
-- injection).
--
-- id is UNIQUE so upserts are idempotent: re-applying the same documentRevision
-- event replaces existing sidecar rows rather than forking new ones.
-- document_uid is not UNIQUE (one document → many chunk rows).

CREATE TABLE documents_vec_ids (
    rowid         INTEGER PRIMARY KEY AUTOINCREMENT,
    id            TEXT    NOT NULL UNIQUE,        -- "<documentUid>#<chunkIdx>"
    document_uid  TEXT    NOT NULL,
    chunk_index   INTEGER NOT NULL,
    heading       TEXT    NOT NULL DEFAULT '',    -- nearest preceding # heading
    byte_offset   INTEGER NOT NULL DEFAULT 0      -- chunk start in the body
);

-- Index on document_uid to make "delete all chunks for this document" efficient
-- (used by both applyDocumentRevision re-apply and applyDocumentDeleted).
CREATE INDEX documents_vec_ids_document_uid ON documents_vec_ids (document_uid);
