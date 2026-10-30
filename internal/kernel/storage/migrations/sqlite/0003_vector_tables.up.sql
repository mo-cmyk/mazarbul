-- Vector layer (M1, issue #30).
--
-- Two vec0 virtual tables, one per embedding domain. The vec0 virtual table
-- type is provided by the sqlite-vec extension; the extension is loaded into
-- every connection by the driver registration in internal/kernel/storage/
-- sqlite/sqlite.go (sqlite_vec.Auto + ConnectHook smoke test).
--
-- The dimension placeholder {{VECTOR_DIM}} is substituted at migration-load
-- time by the runner (loadMigrations in migrate.go) from the canonical
-- constant storage.VectorDim. A future model swap is a one-line change to
-- internal/kernel/storage/vector_dim.go plus a new migration that rebuilds
-- the index; this migration is forward-only and never re-runs.
--
-- Each vec table has a sidecar TEXT-ID mapping (episodes_vec_ids,
-- entities_vec_ids) because the kernel.VectorConn surface uses string IDs
-- (VectorItem.ID) while vec0 only knows INTEGER rowid. The sidecar maps
-- the two; UNIQUE on id makes Upsert idempotent.

CREATE VIRTUAL TABLE episodes_vec USING vec0(
    embedding float[{{VECTOR_DIM}}]
);

CREATE VIRTUAL TABLE entities_vec USING vec0(
    embedding float[{{VECTOR_DIM}}]
);

CREATE TABLE episodes_vec_ids (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    id    TEXT NOT NULL UNIQUE
);

CREATE TABLE entities_vec_ids (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    id    TEXT NOT NULL UNIQUE
);
