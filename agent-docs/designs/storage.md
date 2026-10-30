# Storage: four layers and the relational floor

**Status**: Approved (M1, child issues land iteratively)
**Authors**: kernel team
**Created**: 2026-05-24
**Last updated**: 2026-05-24
**Tracking**: epic #12, child #21 (this PR, SQLite), peers (LadybugDB, sqlite-vec, FTS5, dispatcher) land under the same epic

---

## Context

Mazarbul's source of truth is an append-only JSONL event log. Reads compose over four derived indexes: a relational store (SQLite), a graph store (LadybugDB), a vector index (sqlite-vec), and a full-text index (FTS5). Each index is a projection of the log; the dispatcher replays events into all four during ingest and on `maz pull`. Indexes are gitignored derivatives; the log is what travels.

This document anchors the four-layer model and, in detail, the relational floor (SQLite). The graph, vector, FTS, and dispatcher sections start as stubs that the matching child issues fold into when they land.

## Goals

- Pin the four-layer contract so subsequent issues can be written without re-deriving it.
- Specify the SQLite layer's pragmas, pool shape, migration runner discipline, and M1 schema.
- Leave seams (one section per peer layer) for future PRs to extend without restructuring.

## Non-goals

- Distributed storage (M5+).
- Encryption at rest (M4).
- Cross-host replication via Moria (M4).
- Cross-method recall composition (separate doc once the dispatcher lands).
- LLM-driven schema migrations (not anywhere on the M1-M6 roadmap).

---

## 1. Four-layer overview

The architecture spec at the original architecture notes motivates the four layers from first principles. The short version:

| Layer | Backend (M1) | What it is good at | Where it falls down |
|---|---|---|---|
| Relational | SQLite + WAL | Filters, joins, aggregations, exact predicates | Concept similarity |
| Graph | LadybugDB (embedded, CGO) | Multi-hop causal queries, temporal validity intervals | Free-text recall |
| Vector | sqlite-vec | Paraphrase + concept similarity | Exact tokens, multi-hop |
| Full-text | FTS5 | Exact keywords, names, IDs, code snippets | Paraphrase, concepts |

The dispatcher's `Apply(events)` writes the same event into all four projections. The recall composer (M2) merges the four result sets into a single ranked output with citations.

The interface contract sits at `internal/kernel/storage.go` (see PR #9). The contract is vendor-neutral: `OpenSQLite`, `OpenGraph`, `OpenVector`, `OpenFTS`. Concrete backends live in `internal/kernel/storage/<vendor>/`. A future vendor swap (e.g., LadybugDB to Neo4j) is a contained refactor.

The kernel never exposes a raw SQL connection to plugins. The dispatcher and the query composer are the only callers of `OpenSQLite`.

---

## 2. Relational layer (SQLite)

Owned by `internal/kernel/storage/sqlite/`. This section is fleshed out by issue #21.

### 2.1 Driver

`github.com/mattn/go-sqlite3` (CGO). Chosen because the vector and FTS layers need extension loading via `ConnectHook` + `LoadExtension`, which the pure-Go `modernc.org/sqlite` does not support. CGO is already a build requirement for LadybugDB; using mattn from the start avoids a future driver swap. Pin the version in `go.mod`.

### 2.2 Open path

`sqlite.Open(ctx context.Context, path string) (*Store, error)` is the single entry point.

It performs, in order:

1. `os.MkdirAll(filepath.Dir(path), 0o750)`. Creates the parent if missing.
2. Opens the writer pool against the writer DSN.
3. Asserts the pragmas via explicit `PRAGMA` statements after open (DSN flags set them for new connections; the explicit assertions catch driver-version drift and verify WAL was honored on the underlying filesystem).
4. Runs every pending migration from the embedded directory.
5. Opens the reader pool against the read-only DSN.
6. Returns `*Store{writer, reader, path}`.

`Open` is idempotent: a second call against the same file path observes `_migrations` and skips applied versions. Reopen does no schema writes when up-to-date.

### 2.3 Pragmas

| Pragma | Value | Why |
|---|---|---|
| `journal_mode` | WAL | Concurrent readers, single writer, durable under crash. Mazarbul never opens without WAL; if the underlying FS rejects WAL, `Open` fails fast. |
| `foreign_keys` | ON | The schema has FKs (`mentions.source_event_uid -> episodes.uid`, etc.). Off-by-default in SQLite for legacy reasons; we want them on. |
| `busy_timeout` | 5000 (ms) | Tolerates short lock contention without surfacing an error to callers. |
| `synchronous` | NORMAL | Pairs with WAL: durable across power loss after each commit, but skips the extra fsync on the WAL file itself. |

### 2.4 Connection pools

Two `*sql.DB` handles, both backed by the same on-disk file:

- **Writer pool**: opened with the writer DSN, `SetMaxOpenConns(1)`, `SetMaxIdleConns(1)`, `SetConnMaxLifetime(0)`. SQLite serializes writers regardless; surfacing the single-writer reality through the pool layer makes the contention explicit to callers and lets them shape transactions around it.
- **Reader pool**: opened with `?mode=ro` and `SetMaxOpenConns(runtime.NumCPU())`. Multiple concurrent readers see consistent snapshots via the shared WAL.

The eventual dispatcher binds `kernel.Storage.OpenSQLite` to `Store.Writer()`. Readers are exposed via `Store.Reader()` for the recall composer.

### 2.5 Migration runner

Migrations live at `internal/kernel/storage/migrations/sqlite/NNNN_<name>.up.sql`, owned by a small sibling package `internal/kernel/storage/migrations/` that exports one `embed.FS` per layer (`migrations.SQLite`, future `migrations.Ladybug`, etc.). The `sqlite` package imports `migrations.SQLite` and walks `sqlite/*.up.sql` inside the FS. **Forward-only**: no `.down.sql` files, no rollback path. The event log is the source of truth; if a migration goes wrong, drop the derived databases and rebuild from the log.

Filename grammar: `^([0-9]+)_([^/.]+)\.up\.sql$`. Malformed names abort the load. Duplicate versions abort the load.

Bookkeeping lives in `_migrations`:

```sql
CREATE TABLE IF NOT EXISTS _migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
```

`applied_at` stores `time.Now().UTC().Format(time.RFC3339Nano)` at commit time.

Each migration applies inside one transaction. The migration SQL and the `_migrations` row insert commit together; a failure in either rolls back both. On rollback failure (very rare; SQLite returns `sql.ErrTxDone` if the tx was already rolled back internally), the original error is preserved and the rollback error is appended for diagnostics.

### 2.6 Initial schema (`0001_initial_schema.up.sql`)

```sql
CREATE TABLE episodes (
    uid          TEXT PRIMARY KEY,
    create_time  TEXT NOT NULL,
    kind         TEXT NOT NULL,
    payload_json TEXT NOT NULL
);

CREATE TABLE entities (
    uid               TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,
    display_name      TEXT NOT NULL,
    confidence        REAL NOT NULL,
    resolution_status TEXT NOT NULL
);

CREATE TABLE mentions (
    uid              TEXT PRIMARY KEY,
    kind             TEXT NOT NULL,
    display_name     TEXT NOT NULL,
    source_event_uid TEXT NOT NULL,
    create_time      TEXT NOT NULL,
    FOREIGN KEY (source_event_uid) REFERENCES episodes(uid)
);

CREATE TABLE provenance (
    event_uid  TEXT NOT NULL,
    source     TEXT NOT NULL,
    confidence REAL NOT NULL,
    decided_at TEXT NOT NULL,
    FOREIGN KEY (event_uid) REFERENCES episodes(uid)
);
```

Plus five pragmatic indexes (covered in the migration file). `provenance` deliberately omits a primary key in M1; the issue spec leaves it open and a composite `(event_uid, source)` may land in a later migration once the dispatcher reveals whether duplicates are an issue in practice.

#### Naming notes

- SQL columns use `snake_case` (idiomatic SQL). Go structs that serialize these rows use `CamelCase` field names with `json:"camelCase"` tags per AIP-142 once they exist.
- Time columns store **RFC 3339 UTC strings**. The TEXT affinity in SQLite is intentional: it sorts lexicographically as it sorts chronologically and round-trips cleanly through JSON.
- `decided_at` in `provenance` matches the issue spec. It is an internal SQL column and never surfaces as a JSON field; if it later does, the Go struct field will be `DecideTime time.Time` with `json:"decideTime"` per AIP-142.

#### Schema-spec divergence note

The external architecture spec (the original architecture notes) uses an older `episodes / facts / entities / relations` shape. Issue #21 is the freshest authoritative M1 guidance and supersedes the spec; M1 ships `episodes / entities / mentions / provenance`. The `facts` and `relations` shapes get reintroduced when the M2 entity-resolver and fact-extractor lands. Until then this file is the contract.

### 2.7 Foreign-key ordering invariant

`mentions.source_event_uid` and `provenance.event_uid` both reference `episodes.uid`. The dispatcher must insert episodes before mentions or provenance for the same event batch, or wrap the whole batch in a transaction that defers FK checks. Failing this surface is what FKs are for; the test suite verifies the constraint fires.

### 2.8 Test discipline

- Race-detector clean (`go test -race`).
- Table-driven where the input shape repeats (pragma assertions, schema introspection, malformed-filename rejection).
- Real SQLite, never mocks. `t.TempDir()` for paths.
- A production-FS smoke test (`TestLoadMigrations_EmbeddedProductionFS`) ensures the embedded migration tree parses cleanly after every change.

The test suite that ships with #21 covers: pragma assertions, single-writer pool semantics, idempotent reopen, read-only enforcement on the reader pool, schema introspection, FK enforcement, round-trip insert/select, migration parsing, ordering, duplicate-version rejection, atomic rollback on failed migrations, context cancellation, and a 16-way concurrent-reader smoke test.

A note on AGENTS.md rule 7 (no `*_at` suffixes on time fields): the rule applies to JSON wire fields. Internal SQL columns use `snake_case` per SQL convention; the mix of `_time` (`create_time`) and `_at` (`decided_at`, `applied_at`) in M1 matches the literal issue acceptance text. Once a column surfaces through an MCP tool or HTTP API, the Go struct serializing it will use AIP-142 names (`createTime`, `decideTime`, `applyTime`).

---

## 3. Graph layer (LadybugDB)

Stub. Detail lands in the LadybugDB child issue under epic #12.

Anchors:

- Backend: `github.com/LadybugDB/go-ladybug` (CGO bindings; the M1 implementation lives in `internal/kernel/storage/ladybug/`).
- Kernel interface: `GraphConn`, `GraphResult` in `internal/kernel/storage.go`. Cypher-shaped.
- Migrations: `internal/kernel/storage/migrations/ladybug/NNNN_<name>.cypher` with a `_ladybug_migrations` node tracking applied versions.
- See `agent-docs/tech/ladybug.md` for build requirements and the Cypher schema sketch.

---

## 4. Vector layer (sqlite-vec)

Stub. Detail lands in the vector child issue under epic #12.

Anchors:

- Loaded as a SQLite extension via `ConnectHook` + `LoadExtension("vec0", "sqlite_vec_init")`. See `agent-docs/tech/sqlite-vec.md`.
- The vector index lives in a sibling SQLite file (`vectors.db`), not the relational file, so embedding writes do not contend with the writer-serialized relational pool.
- Schema: `chunks USING vec0(embedding float[N])` + a `chunk_metadata` row that points back to the source event. Dimension and distance function are decided when the M1 embedding provider is wired.

---

## 5. Full-text layer (FTS5)

Stub. Detail lands in the FTS child issue under epic #12.

Anchors:

- FTS5 is a virtual-table feature of SQLite; the M1 implementation lives inside the relational SQLite file (the BM25 cost is paid once, alongside the relational schema).
- Tokenizer choice (Unicode-aware vs trigram) lands with the child issue. Default is the built-in `unicode61` with `remove_diacritics=2`.

---

## 6. Dispatcher

Implemented in `internal/kernel/storage/dispatcher/` (issue #32). The `Dispatcher` type is the only writer to the four derived stores, exposed as the canonical implementation of `kernel.Storage`.

### 6.1 Construction

```go
d, err := dispatcher.Open(ctx, dispatcher.Config{
    DataDir:  cfg.DataDir,
    Embedder: ollamaEmbedder, // wired in #55; nil skips vector inserts
    Logger:   logger,
})
defer d.Close()
```

Layout under `cfg.DataDir`:

- `index.db` — SQLite database hosting the relational, vector, and FTS5 layers
- `graph.ladybug/` — LadybugDB database root

### 6.2 Transactional shape (per event)

```
1. Decode payload into ontology.Episode (no-op for non-episode kinds; M1 skips them).
2. Compute embedding via Config.Embedder (OUTSIDE the SQLite tx so the
   writer lock is not held during the call).
3. Begin SQLite writer tx (immediate lock).
4. INSERT/UPSERT into episodes(uid, create_time, kind, payload_json, text, source).
   FTS5 AFTER INSERT trigger projects text/source into episodes_fts inside the tx.
5. INSERT/UPSERT into episodes_vec_ids and episodes_vec inside the same tx.
6. MERGE (:Episode {uid, createTime, kind, source, confidence}) in LadybugDB.
   This happens BEFORE the SQLite commit so a LadybugDB failure can roll back the tx.
7. Commit SQLite.
```

### 6.3 Failure modes and soft inconsistency

| Failure                                          | Outcome                                                          |
|--------------------------------------------------|------------------------------------------------------------------|
| Payload decode fails                             | Error returned, no tx opened, no writes                          |
| Embedder errors / wrong dimension                | Error returned (`ErrEmbeddingDim` for wrong dim), no tx opened   |
| SQLite write fails inside tx                     | Tx rolls back via deferred `Rollback`                            |
| LadybugDB MERGE fails                            | Returns before commit → SQLite tx rolls back                     |
| SQLite commit fails after LadybugDB success      | **Soft inconsistency window:** `(:Episode)` exists but row does not. Logged at ERROR; reconciled on `EventLog.Rebuild`. M2 closes the window with a saga (#55+). |

### 6.4 Idempotency

`INSERT ... ON CONFLICT(uid) DO UPDATE` for the SQLite relational layer; the sidecar uses `INSERT OR IGNORE` and the `vec0` slot is delete-then-insert so the rowid stays stable. `MERGE (:Episode {uid})` + `ON CREATE SET` / `ON MATCH SET` for LadybugDB. Replaying the same event twice (the `Rebuild` path) produces the same end state without duplicates.

### 6.5 Out of scope (M1)

- Cross-store saga / 2PC — M2 (`#55+`).
- `Apply` for non-Episode kinds — extends the switch when the next kind ships.
- Rebuild orchestration — `#27` calls `Apply` in a loop; that issue owns the loop.
- Hot-path benchmark — deferred to `#55` (where `Apply` is actually called online).

---

## 7. Privacy and provenance

| Concern | M1 impact | Mitigation |
|---|---|---|
| Provenance attached | Schema present (event-level only) | Per-fact provenance lands with M2 facts/relations. |
| Scope tagging | Not yet | Scope columns (`tenant_id`, `user_id`, `agent_id`, `session_id`, `visibility`) land with the multi-user issue. |
| Append-only invariant | Preserved | The relational tables are derived indexes; the JSONL event log is the source of truth. |
| Audit log entry | Not yet | Reads are unaudited in M1; audit lands with the privacy issue. |

---

## 8. Open questions

These remain after #21 lands:

1. **`provenance` primary key.** Add composite `(event_uid, source)` in a follow-up migration if the dispatcher reveals duplicate writes.
2. **Schema evolution discipline.** Forward-only is locked. When the dispatcher fans into the relational layer in a future PR, deciding whether re-extraction from the event log needs explicit schema-version pinning per derived table is the next-up question.
3. **WAL checkpoint policy.** SQLite's default WAL auto-checkpoint at 1000 pages is fine for M1. Once event volume picks up, a periodic explicit checkpoint may be worth profiling.

## 9. Acceptance (issue #21 cross-reference)

| Issue criterion | This document anchors |
|---|---|
| Pragmas (WAL, FK, busy=5000, sync=NORMAL) | §2.3 |
| Migrations at `internal/kernel/storage/migrations/sqlite/NNNN_<name>.up.sql`, lexicographic order | §2.5 |
| `_migrations(version PK, name, applied_at)` | §2.5 |
| `episodes / entities / mentions / provenance` | §2.6 |
| RFC 3339 UTC time columns | §2.6 |
| Writer `SetMaxOpenConns(1)`, reader `?mode=ro` | §2.4 |
| Race-clean tests, idempotent reopen | §2.8 |
