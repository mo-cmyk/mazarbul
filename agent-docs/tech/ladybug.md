# LadybugDB

Embedded property-graph database. Stores entities, relations, and provenance edges. Cypher query language. Columnar storage, ACID transactions, vector + FTS extensions built in.

LadybugDB is the maintained continuation of Kùzu after Apple acquired Kùzu in October 2025 and archived the upstream repo. The core engine, file format, query semantics, and Cypher dialect are the same; the surface is rebranded and the project has active development under MIT (`github.com/LadybugDB/ladybug`).

## Why LadybugDB

- Embedded, single binary. Mazarbul's `maz` binary links the C++ core directly via CGO. No separate server, no IPC overhead.
- Cypher dialect. The kernel speaks Cypher to the graph layer through a thin `kernel.GraphConn` interface in `internal/kernel/storage.go`. Plugins do not write Cypher; they go through the kernel.
- Columnar storage with vectorized execution; fast analytical traversals over large graphs.
- Vector + FTS extensions sit in the same engine, so multi-modal hybrid queries stay in one Cypher statement (M2+ once the composer needs them; M1 keeps vector + FTS in their own SQLite-side extensions per `agent-docs/synthesis.md`).
- MIT license; permissive distribution alongside Mazarbul's Apache-2.0.

## Go bindings

`github.com/LadybugDB/go-ladybug` is the CGO-based Go binding. Pinned: v0.17.0 (needs Go 1.26), paired with engine v0.17.1. Package name in code is `lbug`. The kernel interface (`GraphConn`, `GraphResult` in `internal/kernel/storage.go`) is vendor-neutral so a future vendor switch is a contained refactor; `internal/kernel/storage/ladybug.go` (lands in #29) is where the bindings get wrapped.

Build requirements:

- macOS / Linux: standard CGO toolchain; `cc` (clang or gcc) on `PATH`.
- Windows (later): MSYS2 UCRT64 + MS VC++ 2015-2022 Redistributable + `lbug_shared.dll` on `PATH`. Mazarbul does not ship a Windows build in v0.x.

## Cypher sketch (M1)

```cypher
// All entities tagged 'person' created in the last 7 days
MATCH (e:Entity {kind: 'person'})
WHERE e.createTime > datetime() - duration({days: 7})
RETURN e.uid, e.displayName, e.createTime
ORDER BY e.createTime DESC;

// 2-hop neighborhood of a specific entity (M2 traversal)
MATCH (start:Entity {uid: $uid})-[r*1..2]-(neighbor)
RETURN start, r, neighbor;
```

The kernel exposes a Go API over Cypher; plugins do not write Cypher directly. Internal callers use `kernel.GraphConn.Execute(ctx, cypher, args)` and iterate `GraphResult`.

## Schema sketch (M1, lands in #29)

Nodes:

- `Entity` (uid, kind, displayName, confidence, resolutionStatus, createTime)
- `Episode` (uid, createTime, kind, source, confidence)

Edges:

- `(:Episode)-[:MENTIONS]->(:Entity)`: provenance edge from event log into entities
- `(:Entity)-[:RELATES_TO {predicate, confidence}]->(:Entity)`: typed relation between entities

Migrations live at `internal/kernel/storage/ladybug/migrations/NNNN_<name>.cypher` (embedded via `//go:embed`). A `_ladybug_migrations` node records applied versions; the runner never re-applies. Migration files run literal Cypher (no parameters, no `now()`) and must be idempotent (`IF NOT EXISTS` / `MERGE … ON CREATE SET`).

`0004_self_entity_seed.cypher` `MERGE`s the canonical `self` `Entity` node into the graph, mirroring SQLite migration `0012_self_entity_seed` (which seeds the relational row). Without the graph node, `mergeRelationEdge`'s `MATCH (o:Entity {uid:"self"})` binds nothing and every self-anchored edge is silently dropped from the graph (issue #175). `self` is **local identity**, seeded as a projection bootstrap in both stores — never as a synced event in the JSONL log; see [identity.md](../identity.md#the-self-entity).

## Current implementation status

Bootstrapped in #29 (M1). Package: `github.com/mo-cmyk/mazarbul/internal/kernel/storage/ladybug`. Surface:

- `OpenLadybug(ctx, path) (*Conn, error)`: opens the embedded database, applies pending migrations, returns a `kernel.GraphConn`.
- `Conn.Execute(ctx, cypher, args)`: runs a Cypher statement with optional named parameters; returns a streaming `kernel.GraphResult` iterator.

The local dynamic library (`liblbug.dylib` / `liblbug.so`) and, since go-ladybug v0.17, the C header `lbug.h` are not in the Go module. Run `mise run setup-graph` once per worktree before `go build` or `go test`; the script downloads the platform-matching tarball from the upstream `LadybugDB/ladybug` release and extracts header + lib into `<go-ladybug module dir>/lib`, where the binding's cgo directives look. The engine version is pinned in `scripts/setup-graph-lib.sh` next to the go-ladybug version it pairs with; the script refuses to run when `go.mod` names a different go-ladybug, so bump both together per AGENTS.md rule 10.

The pairing matters because a header/lib mismatch is silent memory corruption, not a link error. go-ladybug v0.13.1 vendored its own `lbug.h`, which disagreed with engine v0.16.1 on `lbug_system_config` (48 vs 56 bytes): `lbug_default_system_config()` returns the struct by value, so the engine wrote past the cgo caller's stack buffer. Debian arm64 aborted with `stack smashing detected` on the first open; macOS ran with a corrupted stack. Since v0.17 header and lib ship in the same tarball, which removes that drift.

go-ladybug v0.17 also dropped its GC finalizers: every `Database`, `Connection`, `PreparedStatement`, `QueryResult` and `FlatTuple` must be closed explicitly, including a statement returned alongside a `Prepare` error.

## History note

The October 2025 Kùzu archival is the canonical case study for AGENTS.md rule 10 (hot-path benchmarks ship with a baseline) and the lesson-learned about vendor-neutral interface names. The kernel does not say "LadybugDB" anywhere in its public surface; it says `OpenGraph`, `GraphConn`, `GraphResult`. If LadybugDB ever disappears too, the impl swaps and the rest of the kernel keeps compiling.
