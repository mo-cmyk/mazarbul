-- 0005_multivec_spatial: multi-vector entity embeddings, mention vectors, and
-- spatial coordinates on entities (M1, P4).
--
-- Changes in this migration:
--
--   1. entities table: add nullable coordinate columns and create_time. The
--      CHECK constraint (latitude IS NULL) = (longitude IS NULL) cannot be
--      added to an existing table via ALTER TABLE in SQLite; it is enforced
--      by the dispatcher at write time.
--
--   2. mentions table: add extraction_text column (source text for the
--      mention vector). Backfilled as '' for rows written before P4 lands;
--      the extractor populates it going forward.
--
--   3. entities_vec / entities_vec_ids: replace the single-vector table from
--      migration 0003 with a multi-vector design. The new entities_vec_ids
--      table carries a vector_type discriminator ('name' or 'summary') so
--      each entity can hold up to two distinct embedding vectors without a
--      schema migration per new vector aspect. The old entities_vec and
--      entities_vec_ids tables from 0003 are dropped first.
--
--   4. mentions_vec / mentions_vec_ids: new vec0 virtual table for mention
--      extraction-text embeddings. Single vector per mention (no discriminator
--      needed in M1).
--
-- The {{VECTOR_DIM}} placeholder is substituted at migration-load time from
-- storage.VectorDim by the runner in internal/kernel/storage/sqlite/migrate.go.

-- ============================================================================
-- 1. entities: coordinate columns and create_time
-- ============================================================================

ALTER TABLE entities ADD COLUMN create_time            TEXT NOT NULL DEFAULT '';
ALTER TABLE entities ADD COLUMN latitude               REAL;
ALTER TABLE entities ADD COLUMN longitude              REAL;
ALTER TABLE entities ADD COLUMN coordinates_provenance TEXT;

-- Both-or-neither enforcement on (latitude, longitude). SQLite does not
-- support adding a CHECK constraint to an existing table via ALTER TABLE,
-- so the invariant is enforced by BEFORE INSERT / BEFORE UPDATE triggers
-- that ABORT with a clear message when exactly one coordinate is NULL.
-- The dispatcher write path also validates this in Go, so the trigger is
-- a belt-and-suspenders defence against direct SQL writes.
CREATE TRIGGER IF NOT EXISTS entities_coords_check_insert
    BEFORE INSERT ON entities
    FOR EACH ROW
    WHEN (NEW.latitude IS NULL) != (NEW.longitude IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'entities: latitude and longitude must both be set or both NULL');
END;

CREATE TRIGGER IF NOT EXISTS entities_coords_check_update
    BEFORE UPDATE ON entities
    FOR EACH ROW
    WHEN (NEW.latitude IS NULL) != (NEW.longitude IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'entities: latitude and longitude must both be set or both NULL');
END;

-- ============================================================================
-- 2. mentions: extraction_text
-- ============================================================================

ALTER TABLE mentions ADD COLUMN extraction_text TEXT NOT NULL DEFAULT '';

-- ============================================================================
-- 3. entities_vec: replace single-vector with multi-vector
-- ============================================================================

-- Drop the simple single-vector tables created by migration 0003.
-- ORDER matters: drop the sidecar first so the foreign-key-less vec0 row can
-- be removed cleanly; vec0 DROP TABLE is a no-op for the virtual table DDL.
DROP TABLE IF EXISTS entities_vec_ids;
DROP TABLE IF EXISTS entities_vec;

-- New multi-vector virtual table. One row per (entity, vector_type) pair.
CREATE VIRTUAL TABLE entities_vec USING vec0(
    embedding float[{{VECTOR_DIM}}]
);

-- Sidecar: maps (id, vector_type) to the INTEGER rowid that vec0 requires.
-- UNIQUE(id, vector_type) makes upserts idempotent: the same entity can have
-- a 'name' row and a 'summary' row but never two rows of the same type.
-- vector_type defaults to 'name' so the legacy single-vector entity Upsert
-- path (kernel.VectorConn callers that pass only id + embedding) keeps
-- working: an entity's canonical embedding is its name embedding. The new
-- summary embedding path explicitly passes vector_type='summary'. The CHECK
-- constraint pins the allowed values; future aspect vectors (role, location,
-- expertise) extend the enum.
CREATE TABLE entities_vec_ids (
    rowid       INTEGER PRIMARY KEY AUTOINCREMENT,
    id          TEXT    NOT NULL,
    vector_type TEXT    NOT NULL DEFAULT 'name',
    CHECK (vector_type IN ('name', 'summary')),
    UNIQUE(id, vector_type)
);

-- ============================================================================
-- 4. mentions_vec: new single-vector table
-- ============================================================================

CREATE VIRTUAL TABLE mentions_vec USING vec0(
    embedding float[{{VECTOR_DIM}}]
);

CREATE TABLE mentions_vec_ids (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    id    TEXT    NOT NULL UNIQUE
);
