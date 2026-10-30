# External references — pointing at information in other systems

Mazarbul can point at things that live outside its own event log: a GitHub
issue, a Google Drive doc, a Granola transcript, a repo file. The primitive
(issue #208) is a core entity kind `external_ref` plus a core predicate
`has_primary_source`. An `external_ref` is a first-class **local** entity, so
`relate` / `recall` / `traverse` / `getContext` work on it with no special
casing.

## The two pieces

- **`external_ref`** (entity kind) — a pointer whose canonical identity is a
  `{type:"uri", value:"<scheme>://…"}` identifier. Display metadata (an optional
  label) lives in the entity's display name.
- **`has_primary_source`** (predicate, PROV-O `hadPrimarySource`) — the edge
  `relate(localRecord, has_primary_source, externalRef)`. **Direction:** subject
  = the local thing (a decision, a fact, a document); object = the `external_ref`.

## Tools

| Surface | Call |
|---|---|
| MCP | `reference({uri, label?, scope?})` → `{name, uid, uri, scope, created}` |
| CLI | `maz reference <uri> [--label …] [--scope personal\|project\|team]` |
| MCP | `captureExternal({uri, connector, sourceType, summary\|summaries[], …})` → `{action, externalRefName, episodeNames[], sourceRefNames[], freshness}` |

`reference` is **mint-or-return**: the entity uid is derived deterministically
from `(scope, uri)`, so calling it twice for the same URI in the same scope
returns the *same* entity (`created:false`). It is therefore the **canonical
resolver** for an external thing — any flow that wants "the node for this URI"
just calls `reference`.

The `has_primary_source` edge is a **separate** `relate` call; `reference` only
mints/returns the node. (This keeps `relate` untouched — an `external_ref` is a
normal ULID endpoint.)

## Privacy: personal by default

`reference` defaults to **personal** scope — local, never rendered into the
shared project git tree. Dedup is **within scope only**: the same URI in
personal vs project scope yields two distinct entities, so a personal-private
pointer never collapses into a project-scope entity that renders to git. Share a
reference deliberately with `--scope project`.

Mazarbul never dereferences a URI and never enriches a reference with
third-party identity metadata — there is no fetch, so no PII is pulled in. A
URI you pass (e.g. `mailto:alice@example.com`) is stored verbatim as the
identifier; keep person-shaped URIs in personal scope unless you intend to
publish them.

## Making the referenced content searchable (the composition)

Mazarbul is **push-only**: it does not fetch. The body of a referenced document
is *not* indexed by `reference` alone. To make the content searchable AND tie it
to the canonical node, the agent does the fetching with its **own** tools, then
composes the existing primitives:

1. **Fetch** the content with your own connector/CLI access (Drive, Granola,
   GitHub, …) — Mazarbul is not in this step.
2. **`remember(content)`** — the content becomes an episode: embedded, FTS-
   indexed, and graph-extracted into entities/relations. *This* is what makes it
   searchable in `recall`.
3. **`recordSourceRef({subjectUid, externalUrl: <uri>, …})`** — record where the
   content came from (provenance backlink, issue #207).
4. **`reference(<uri>)`** then **`relate(<record>, has_primary_source, <ref>)`** —
   the canonical pointer node + the grounding edge.

Because `reference` is idempotent on the URI and `recordSourceRef.externalUrl`
is the same URI, all four legs converge on one canonical `external_ref` node.

## `captureExternal`: the one-call atomic capture

`captureExternal` collapses that four-leg composition into **one atomic
`Append`** for the common case: an agent in a live session already holds a
connector (Notion, Linear, Granola, Drive, GitHub), fetches an item, and wants
maz to index a searchable pointer + summary + freshness anchor **without copying
the body**. Mazarbul still never fetches or holds the content; the agent is the
fetcher, the tool is the index.

One call emits, in a single batch:

1. `createEntity` an `external_ref` (mint-or-return on `(scope, uri)`).
2. one `episode` per summary (`summary`, or `summaries[]` for a chunked item),
   tagged `Source = connector:<name>`.
3. a `sourceRefRecorded` carrying `contentHash` (the ETag), `fetchTime` (the
   cache age), `connector`, `externalUrl`, plus the new `freshnessToken` and
   `tombstone` columns.
4. a `has_primary_source` edge (episode → external_ref).

### Lineage trust (not authorship)

The summary episode and any extracted facts land `status = proposed`
(untrusted), because the `connector:<name>` source marks them as third-party
input. This is **not** the trusted posture `remember` gives. `recall` surfaces an
inline `trust` block on every capture hit (state `proposed`, the connector
source, and a note pointing at `promoteRelation`), and wraps the summary text in
the UNTRUSTED-EXTERNAL-SOURCE scrubber fence so prompt-injection in the body is
neutralised. Verify before treating a captured fact as authoritative.

### Inline freshness on recall

Every recall hit grounded in a capture carries a `freshness` block:

| `status` | meaning |
|---|---|
| `fresh` | the cached snapshot is within its connector freshness budget; reuse it |
| `stale` | past the TTL; re-fetch live and recapture |
| `access-unverified` | the source was tombstoned (404/403 on last capture); last known state |
| `unknown` | grounded but no usable fetch time |

The block also carries `fetchTime`, `connector`, `externalUrl`, and a note. The
classification lives in the MCP tool layer (the kernel returns raw
`fetchTime` / `connector` / `externalUrl` / `tombstone` per hit and stays
config-free); it is joined onto hits via the same two-leg pattern as
`hasPrimarySource` (direct `source_ref` subject, or transitive via an active
`has_primary_source` edge; the direct leg wins).

### Idempotency and supersede-on-change

`captureExternal` returns an `action`:

| `action` | when |
|---|---|
| `captured` | first capture of this item |
| `skipped` | same `contentHash` + valid `freshnessToken`, within TTL, so no write |
| `revision` | `contentHash` changed (or token mismatch); a NEW episode is appended and the prior `has_primary_source` edge is retracted so the stale summary stops ranking |
| `expired` | past TTL, so a re-capture is forced |
| `tombstoned` | `status=deleted` / `access-revoked`; marks the prior capture, no new episode |

Idempotency keys on a `dedupKey` (`<connector>:<externalId>:<scope>`, minted when
the caller omits it). The `freshnessToken` is `HMAC(installSecret, dedupKey +
contentHash + len(summary))` computed by the tool; the kernel stores it verbatim
and never recomputes it (it has no install secret and never fetches). A replayed
or forged hash whose token does not verify forces a write rather than a silent
skip; TTL (`fetchTime` max-age) is the backstop for first-capture errors. The
per-install HMAC secret (`capture.hmac_key`, via `secret.Provider`) is generated
on first use and never leaves the device.

### Refusals (error codes)

- **Project scope** is refused (`E_VALIDATION`): the `scope` input enum is
  `personal | team` only. A captured external summary is connector-sourced and
  project renders to the shared git tree. (`recordSourceRef` with a `connector`
  set refuses project for the same reason.)
- **Ephemeral connectors** (`gmail`, `gcal`, `slack`) are refused
  (`E_VALIDATION`): a cached summary of a real-time stream would mislead; read
  those live each time. The gate is the `ephemeral_write_gated` flag in the
  per-connector freshness config.

### Per-connector freshness config (the knob)

`[connectors.freshness]` in `config.toml` sets each connector's TTL and the
ephemeral write-gate. Defaults: notion 24h, linear 2h, granola 720h (30d),
drive 24h, github 24h; gmail / gcal / slack are `ephemeral_write_gated`. The
recall freshness classifier and the `captureExternal` write-gate both read this
table (`config.ResolveFreshnessPolicy`).

## North star (not implemented)

A server-side `dereference(uri)` — Mazarbul resolving a scheme to a connector
and fetching the body — is deliberately **deferred**. The MCP socket is
unauthenticated, so a server-side fetcher would be an SSRF + confused-deputy
engine. Live fetch and connector dispatch belong to the connector-ingest work,
behind an authentication story, not here.

## Read surface (no change needed)

An `external_ref` surfaces through every read verb unchanged: `get`,
`entities` (filter `kind=external_ref`), `relations`, `neighbors`, and `recall`
(as a `HitTypeEntity` via the name-embedding + graph lanes). `getContext`
collects external refs into a dedicated **"Referenced externally"** brief
section, scope-bounded like the rest of the brief. Note: entities are not
FTS-indexed, so an `external_ref` is found by name-embedding + graph
traversal, not exact-keyword FTS on the URL.

## Filtering recall by provenance

`recall` accepts two provenance filters that run **after** ranking, so they
never distort BM25 / semantic / RRF scoring and survivors keep their unfiltered
rank order:

- **`source`** (MCP `source: [string]`, CLI `maz recall <q> --source <name>`,
  repeatable, OR semantics) — keeps only hits whose `eventSource` matches. A
  bare connector name (`granola`) matches its `connector:granola` /
  `connector/granola` forms too. Every hit is annotated with its `eventSource`.
- **`hasPrimarySource`** (MCP `hasPrimarySource: true`, CLI
  `--has-primary-source`) — keeps only hits grounded in a primary source: the
  hit (or one of its entity anchors) is the subject of a recorded `source_ref`
  (#207) or of an active `has_primary_source` edge (#208).

An unknown source name is **not** an error — it yields an empty `hits` array
(the CLI adds a one-line stderr hint). Recall over-fetches from the kernel when
either filter is active so a full page can survive the post-rank trim.
