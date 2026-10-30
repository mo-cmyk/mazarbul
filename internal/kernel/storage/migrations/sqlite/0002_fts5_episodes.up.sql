-- 0002_fts5_episodes: FTS5 virtual table over episodes for exact-keyword recall.
-- See agent-docs/designs/storage.md §5.
--
-- The text and source columns are added to episodes here so that the FTS5
-- external-content table (content='episodes') can resolve them when callers
-- read back snippets via the snippet() auxiliary function. The dispatcher
-- populates them at write time; in M1 the kernel writer is the only producer.

ALTER TABLE episodes ADD COLUMN text   TEXT NOT NULL DEFAULT '';
ALTER TABLE episodes ADD COLUMN source TEXT NOT NULL DEFAULT '';

CREATE VIRTUAL TABLE episodes_fts USING fts5(
    text,
    source,
    content='episodes',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2 tokenchars ''_'''
);

INSERT INTO episodes_fts(episodes_fts, rank) VALUES('rank', 'bm25(10.0, 5.0)');

-- 'rebuild' populates the index from the content table so pre-existing
-- episode rows (text/source default to '' for rows from #21, real values
-- for any rows already populated by callers) are searchable immediately.
-- Without this, only inserts that fire the AFTER INSERT trigger after
-- migration end up in the index.
INSERT INTO episodes_fts(episodes_fts) VALUES('rebuild');

CREATE TRIGGER episodes_ai AFTER INSERT ON episodes BEGIN
    INSERT INTO episodes_fts(rowid, text, source) VALUES (new.rowid, new.text, new.source);
END;

CREATE TRIGGER episodes_ad AFTER DELETE ON episodes BEGIN
    INSERT INTO episodes_fts(episodes_fts, rowid, text, source) VALUES('delete', old.rowid, old.text, old.source);
END;

CREATE TRIGGER episodes_au AFTER UPDATE ON episodes BEGIN
    INSERT INTO episodes_fts(episodes_fts, rowid, text, source) VALUES('delete', old.rowid, old.text, old.source);
    INSERT INTO episodes_fts(rowid, text, source) VALUES (new.rowid, new.text, new.source);
END;
