# ADR-0002: Online rebuild and reindex via shadow-build and serving-stack reload

## Status
Accepted

## Context
`maz rebuild` and `maz reindex --reembed` replay the JSONL event log to regenerate
the four derived stores (SQLite `index.db`, LadybugDB `graph.ladybug`, sqlite-vec
`episodes_vec`, FTS5 tables). Both operations need exclusive write access to those
stores. The daemon holds exactly those same exclusive locks: LadybugDB opens
`graph.ladybug` with a single-opener constraint, and the event-log's bbolt sidecar
(`events/index.bolt`) cannot be opened by a second writer while the daemon runs. The
result was a hard requirement: operators had to `maz daemon stop` before every rebuild
or re-embed, which severed every connected MCP client. Claude Desktop and Claude Code
reach the daemon through a thin `maz mcp serve` proxy; stopping the daemon dropped
those sessions and required the operator to reconnect every MCP client after the
rebuild finished. For routine operations (switching embedding models, repairing a
corrupted projection) this stop-and-restart cycle was unacceptable friction.

A secondary problem drove the reindex path: the `entities` table stores no `summary`
column. Summaries exist only in event payloads. An in-place vec re-embed that reads
from the SQLite table could therefore regenerate entity *name* vectors but not
*summary* vectors or episode/mention vectors whose text comes from the log. A complete
re-embed requires replaying the log regardless.

## Decision
### Shadow-build plus serving-stack reload (online rebuild)

The daemon replays the live event log read-only into a fresh projection in
`<dataDir>/.rebuild-<ulid>/` while continuing to serve MCP calls on the existing
socket. `FileEventLog.Read` walks the JSONL directly and never opens the bbolt sidecar
(`Append` and `At` open it; `Read` does not), so the shadow replay acquires no lock
the live daemon holds and the two readers do not contend.

When the shadow build is complete, the supervisor's reload path executes a brief
quiesce-and-swap:

1. `srv.Shutdown` drains in-flight calls and closes the listeners, which freezes the
   JSONL at a stable boundary.
2. A delta replay catches events that landed during the build window.
3. The live store is closed.
4. The rebuilt `index.db` and `graph.ladybug` are renamed into the canonical data-dir
   paths.
5. A fresh serving stack is wired onto the same Unix socket and started.

The socket is unavailable only for that rename-and-rewire window, which is on the
order of seconds. The reconnecting `maz mcp serve` proxy (already shipped) re-dials
with backoff and retries in-flight reads within their deadline, so agents see a brief
pause, not a failure. Writes fail clean during the window and can be retried.

### Reindex via rebuild (decision R2)

`maz reindex --reembed` routes through the same shadow-rebuild path rather than
attempting an in-place vec update. Because summaries live only in event payloads and
not in any derived table, a full log replay is the only correct path to regenerate
every vector type (name, summary, episodes, mentions) with the current embedder.
Reusing the shadow-rebuild machinery also means reindex inherits the same crash-safety
and online semantics at no extra cost.

### Socket-only admin trust boundary

The trigger surface for online rebuild and reindex is the daemon's admin HTTP
interface: `POST /admin/v1/rebuild` (202 `{jobUid}`, or 409 if a job is already
running), `POST /admin/v1/reindex` (same shape), and `GET /admin/v1/rebuild` (job
status). This interface is mounted structurally only on the Unix transport and is
absent from the TCP transport. The reconnecting `maz mcp serve` proxy relays only MCP
`tools/call` traffic, so an agent cannot reach the admin surface through the MCP
channel. The admin surface is reachable only to a local process that can open the Unix
socket, which requires filesystem access to the data directory.

A rebuild *coordinator* owns the authoritative job state and outlives the
serving-stack reload. This means the CLI can re-dial across the swap window and still
read the terminal state of a job that started before the reload.

One rebuild at a time is enforced by a coordinator mutex; a concurrent request returns
409.

### Crash safety via sentinel

Crash safety is maintained through two invariants:

- **JSONL is never mutated.** A crash mid-shadow-build leaves an orphan
  `.rebuild-<ulid>/` directory. Boot hygiene removes any such directory older than
  one hour.
- **Swap sentinel.** A `.rebuild-commit` file is written (and fsynced) before the
  first rename and removed after both renames complete. If the process crashes between
  those two points, the next boot detects the sentinel, warns that the SQLite/graph
  projection pair may be inconsistent, and directs the operator to run `maz rebuild`
  to repair. The worst-case outcome is always "recoverable by rebuild," never data
  loss.

## Consequences
- **Positive:** Rebuild and reindex no longer require a daemon stop, so connected MCP
  clients (Claude Desktop, Claude Code) are not severed. Operators can run a rebuild
  at any time without scheduling a maintenance window.
- **Positive:** The crash-safety model guarantees that any mid-swap failure is
  detectable at boot and repairable by rebuild. There is no silent corruption path.
- **Positive:** The socket-only admin trust boundary means an agent cannot trigger or
  observe a rebuild. The capability is reserved for local tooling with filesystem
  access to the data directory.
- **Negative:** The socket is unavailable for seconds during the quiesce-and-swap
  window. Writes that arrive in that window fail clean but must be retried by the
  caller. The reconnecting proxy masks this for reads; writes from tooling that does
  not retry will surface a transient error.
- **Negative:** A shadow build doubles the disk footprint of the derived stores for
  the duration of the build. On a large data directory this is non-trivial.
- **Negative:** One rebuild at a time is a hard constraint. A second request during
  a long rebuild returns 409; there is no queue.

## Alternatives considered

**In-process hot-swap of cached reader handles.** Rebuild the projection in place by
swapping the open database handles held by the running serving stack. Rejected: open
connection handles are cached across approximately seven call-sites, including five
separate navigator instances that are not reachable from a single seam (they are
constructed independently by the Wired supervisor and not aggregated behind a
switchable interface). There is no single abstraction boundary to swap behind; any
implementation would have required coordinating seven independent close-and-reopen
sequences with active in-flight calls, which is equivalent complexity to the
quiesce-and-swap but without the clean crash-safety story.

**Process re-exec.** Replace the daemon process entirely (exec the binary over itself)
after the shadow build, relying on the OS to bring up a clean serving stack. Rejected:
re-exec loses the in-flight rebuild coordinator state. A job that was running before
the exec would have no owner after the exec, so the CLI could not read its terminal
status. It would also require serializing all coordinator state to disk before exec and
rehydrating it on the new process, adding complexity without eliminating the socket
downtime window that the quiesce-and-swap already bounds.
