-- Embedding cache (M1, issue #87).
--
-- Skip redundant Embedder.Embed calls when the same (text, model, dim) triple
-- is re-embedded. Keyed by sha256(text || NUL || model || NUL || dim) computed
-- in Go and stored as the lowercase hex digest in content_hash.
--
-- vector is the raw little-endian float32 blob (4 * dim bytes), the same
-- wire format used by sqlite_vec.SerializeFloat32 for vec0 binds; the cache
-- reuses that encoder on the way in and a small binary.Read on the way out.
--
-- dim is denormalised per row so a future model swap that changes the
-- embedding dimension can coexist with old rows until they are swept; the
-- model field has the same role for model name. Both are part of the hash
-- input so a collision across (text, model, dim) is impossible by design.
--
-- create_time is ISO-8601 / RFC3339 text, the same format used elsewhere in
-- the relational store; it is not currently consulted by readers but keeps
-- the door open for a future eviction policy (M2+ if disk usage demands it).

CREATE TABLE embedding_cache (
    content_hash TEXT PRIMARY KEY,
    model        TEXT NOT NULL,
    dim          INTEGER NOT NULL,
    vector       BLOB NOT NULL,
    create_time  TEXT NOT NULL
);
