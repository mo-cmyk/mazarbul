---
name: inspect-db
description: Live data-layer diagnostics for Mazarbul. USE WHEN user asks "why is recall slow", "show me the event log", "is the graph corrupted", "what's in the vector index", or any question about the four-layer storage (event log + Kuzu + sqlite-vec + FTS). NOT FOR schema introspection of upcoming entity kinds (those live in JSON Schema source).
allowed-tools:
  - Bash(sqlite3 *)
  - Bash(kuzu *)
  - Bash(jq *)
---

# Live data-layer diagnostics

Mazarbul stores facts across four layers, all rooted at `$MAZ_DATA_DIR`:

- `events.sqlite`, the append-only event log.
- `graph.kuzu`, the property graph.
- `vec.sqlite`, the sqlite-vec vector index.
- `fts.sqlite`, the FTS5 full-text index.

This skill is for probing them live.

## Triage tree

### 1. Event log

```bash
sqlite3 "$MAZ_DATA_DIR/events.sqlite" <<SQL
.headers on
.mode column
SELECT kind, source_connector, count(*) FROM events GROUP BY kind, source_connector ORDER BY 3 DESC LIMIT 20;
SQL
```

Watch for:
- Unexpected supersede chains (one event with many supersededBy references).
- Events lacking `source_connector` or `confidence` (provenance gaps).
- Time clustering (a single connector flooding the log).

### 2. Vector index

```bash
sqlite3 "$MAZ_DATA_DIR/vec.sqlite" <<SQL
.headers on
SELECT count(*) AS embeddings, max(length(vec)) AS dim FROM embeddings;
SQL
```

Watch for:
- Dimension drift (you changed embedding model mid-stream).
- Counts diverging from the event log count (indexer is behind).

### 3. Full-text index

```bash
sqlite3 "$MAZ_DATA_DIR/fts.sqlite" "SELECT count(*) FROM fts;"
```

### 4. Graph

```bash
kuzu "$MAZ_DATA_DIR/graph.kuzu" <<CYPHER
MATCH (n) RETURN labels(n)[0] AS kind, count(*) AS n ORDER BY n DESC;
CYPHER
```

Watch for:
- Orphan nodes (entities with no incoming or outgoing edges).
- Edge counts wildly higher than node counts (over-relating).

## Slow recall

If a recall is slow:

1. Run the query with `--explain` on the CLI (when implemented).
2. Confirm the vector index is up to date (count above).
3. Confirm the FTS index is up to date (count above).
4. Check that the kernel composer is filtering by scope (some scopes prune the candidate set drastically).
5. Profile the recall path: `mise run pprof:cpu` against the running daemon.

## Storage hygiene

- `VACUUM` the SQLite files monthly: `sqlite3 events.sqlite "VACUUM;"`.
- Kuzu's storage layer reclaims space on rebuild only; `maz kuzu compact` (when implemented) handles this.
- Backup is git-mediated for events.sqlite (CRDT sync). Other layers are derived and re-buildable.

## Common gotchas

- **SQLite locked** when `maz mcp serve` is running, use `?mode=ro` URI or stop the daemon.
- **Kuzu requires a single writer**, multiple `kuzu` CLIs at once will fail.
- **Vector dimension mismatch** after switching embedding models, blow away `vec.sqlite` and re-index.

## What this skill is NOT

- Schema design for new entity kinds, see `/new-resource` for that.
- Cloud-DB ops, Mazarbul is local-first.
