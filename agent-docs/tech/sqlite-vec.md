# sqlite-vec

Vector index. **Bootstrapped in #30** (M1 walking skeleton, area:kernel).

## Approach

`sqlite-vec` ships as a CGO extension via `github.com/asg017/sqlite-vec-go-bindings/cgo`. The Go side calls `sqlite_vec.Auto()` once at process start, which registers the extension as a SQLite auto-extension at the C level (`sqlite3_auto_extension`); every subsequent connection opened by mattn's driver gets the extension for free.

The registration lives at `internal/kernel/storage/sqlite/sqlite.go` behind a `sync.OnceFunc`:

```go
var registerVecDriver = sync.OnceFunc(func() {
    sqlite_vec.Auto()
    sql.Register("sqlite3_vec", &sqlite3.SQLiteDriver{
        ConnectHook: func(conn *sqlite3.SQLiteConn) error {
            rows, err := conn.Query("SELECT vec_version()", nil)
            if err != nil {
                return fmt.Errorf("sqlite-vec: vec_version probe failed: %w", err)
            }
            return rows.Close()
        },
    })
})
```

The `ConnectHook` runs a `SELECT vec_version()` smoke query on each new connection so a missing or broken extension surfaces immediately at `PingContext` time with a clear error, satisfying the fail-fast acceptance criterion.

Both the writer and reader pools open via the `sqlite3_vec` driver name, so the extension is available on every connection used for reads + writes.

## Schema (as shipped)

Migration `0003_vector_tables.up.sql` creates two domain-scoped vec0 virtual tables and two sidecar tables that map the kernel's string `VectorItem.ID` to the synthetic `INTEGER rowid` vec0 requires:

```sql
CREATE VIRTUAL TABLE episodes_vec USING vec0(
    embedding float[1024]
);

CREATE VIRTUAL TABLE entities_vec USING vec0(
    embedding float[1024]
);

CREATE TABLE episodes_vec_ids (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    id    TEXT NOT NULL UNIQUE
);

CREATE TABLE entities_vec_ids (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    id    TEXT NOT NULL UNIQUE
);
```

The dimension `1024` is templated into the migration body at load time from the canonical `storage.VectorDim` constant via the `{{VECTOR_DIM}}` placeholder substitution in `migrate.go`. A future model swap is a one-line change to `internal/kernel/storage/vector_dim.go` plus a fresh migration that rebuilds the index.

## Query patterns

Top-k search uses the vec0 `MATCH` operator with the `k = ?` constraint and an `ORDER BY distance` tail:

```sql
SELECT ids.id, v.distance
FROM episodes_vec AS v
JOIN episodes_vec_ids AS ids ON ids.rowid = v.rowid
WHERE v.embedding MATCH ?
  AND k = ?
ORDER BY v.distance
```

Embeddings are serialised to `[]byte` via `sqlite_vec.SerializeFloat32`. Upsert is delete-then-insert inside one transaction because vec0 does not support `INSERT OR REPLACE`; the sidecar row's `rowid` is stable so re-upsert hits the same slot.

## Out of scope (deferred)

- Multi-vector entity embeddings (`embedding_name`, `embedding_description`, `embedding_context`) — M3.
- HNSW/IVF index tuning and distance-function selection — M2+.
- `VectorFilter` (kinds + time window) wiring — the canonical `Search` returns `kernel.ErrNotImplemented` on any non-zero filter; the recall composer in M1 wave 2 owns the filter path.
- Cross-domain similarity (search both domains in one call) — M3+.
