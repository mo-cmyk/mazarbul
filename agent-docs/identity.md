# Identity

Canonical entities sit at the centre of the graph. Every person, organisation, project, and document gets a stable ULID plus any number of external IDs. The identity layer's job is resolving inbound references against the registry, surfacing pending matches for review, and never silently merging two distinct entities into one.

## Canonical entities

Each entity row in the graph carries:

- `uid`: internal ULID, immutable, server-assigned.
- `name`: AIP resource name in the form `entities/{ulid}`.
- `displayName`: human-readable label.
- `kind`: `person`, `organization`, `project`, `document`, `concept`, etc. The taxonomy is no longer a closed enum — entity kinds are an open, event-sourced, git-tracked registry that humans, agents, the resolver, and schema packs can extend (see [designs/dynamic-ontology.md](designs/dynamic-ontology.md)).
- `externalIds`: array of `{authority, value}` pairs. Examples: `HRB-12345` (German Handelsregister), `urn:li:person:...` (LinkedIn), `Q12345` (Wikidata), `crunchbase:org:...`. Multiple external IDs per entity are normal.
- Provenance (source, confidence, timestamp) per claim.

## The `self` entity

The user is represented by a single canonical entity with the reserved UID
`self` (`resolver.SelfEntityUID`) — not a ULID. First-person statements ("I live
in Berlin"), `has_name` facts, and `entity: self` document frontmatter all anchor
to it. The Relation/Fact schemas accept `self` as a deliberate exception to the
ULID pattern.

`self` is **local identity**, so it is seeded as a *projection bootstrap*, never
as an event in the git-synced JSONL log:

- SQLite migration `0012_self_entity_seed` seeds the relational `entities` row.
- LadybugDB migration `0004_self_entity_seed` `MERGE`s the matching graph node so
  self-anchored edges are graph-traversable (issue #175 — before it, the graph
  node was missing and `mergeRelationEdge`'s `MATCH` on `self` silently dropped
  every self edge from the graph lane, though the SQLite Navigator surface still
  saw them).

Both seeds run at boot and are idempotent; they stay in lockstep.

### Why not a synced genesis event

The "pure event-sourced" alternative — appending an `EntityCreated` event for
`self` to the log — is deliberately avoided. The event log is git-synced; a
shared `self` genesis event would propagate **one identity to everyone** and
conflate every collaborator's first-person facts onto a single node.

### Teams / scopes (realized)

`self` stays a **local bootstrap**, never synced. On the team scope each
principal is materialized as a concrete person entity by two appliers:
`principalRegistered` (the self device only, personal scope) and the team-grant
`projectAdmittedMember` (every device, team scope, insert-if-absent). A
teammate's first-person statement therefore resolves to *their* person UID, not
the local reader's `self` — the two never merge. Because nothing about `self`
is synced, per-device seeds never collide.

Each device also mints a per-device signing subkey vouched by the principal
key, so identity is anchored per-device. See [team.md](team.md) for roles,
device keys, genesis pinning, and share-to-team; see
[privacy.md](privacy.md) for the encryption tradeoffs.

### Provisioning the bound principal at `init`

A device needs a **bound principal** (a Watchword → identity keypairs, cached in
the keychain) before any team operation works — without it every team command
fails "this device has no bound principal", and kernel boot defers genesis
(`ErrNoWatchword`). The interactive `maz init` form always provisioned this; the
**non-interactive** paths (`maz init --json`, scripted/headless `maz init`) now
do too, by default: they generate a Watchword and run `identity.Initialize`.
The JSON path returns the phrase in a `recoveryPhrase` field; the CLI path prints
the 24 words (store them — they are the only recovery primitive). Pass
`--no-identity` to opt out for deployments that manage credentials externally.
Provisioning is idempotent (a Watchword already in the keychain is left as-is).

## Resolution pipeline

When ingestion proposes a new mention of an entity, the resolver runs:

1. **Blocking.** Coarse candidate selection by exact match on external IDs, display name, normalized email, or domain.
2. **Pairwise scoring.** For each candidate, compute a similarity score from name, context, co-occurring entities, source overlap.
3. **Threshold check.** Above the high threshold, the resolver auto-merges (the mention links to the canonical entity). Between high and low, the mention enters the pending-review queue. Below the low threshold, the resolver creates a new canonical entity.
4. **Canonical write.** The merged entity (or new entity) is written with full provenance back to the event log.

<!-- TODO(m1): expand: similarity function, threshold tuning, training data sources, blocking key strategy. -->

## Direct entity writes and create-time dedup

There are two ways an entity is born: the **resolver** (above), driven by extraction, and the **direct mint** — the `createEntity` MCP tool and its `maz entity create` CLI, used when an agent already holds an entity as `(kind, displayName)` and wants its `entities/{uid}` without an LLM round-trip. Bulk loads (CRM imports, fixtures) go through the direct path.

Both paths now share one matcher, `internal/kernel/entitymatch`, so a name the resolver would have merged is the same name the direct path refuses to duplicate. Before minting, `createEntity` runs the dedup gate:

- **Identifier match** → `status: reused`: a supplied identifier already anchors an entity, so that entity is returned (deliberate anchor = confident reuse). No new row.
- **Exact name + kind, or a name-embedding cosine ≥ the reuse threshold** (`cfg.NameEmbThreshold`, 0.95) → `status: duplicate_candidates`: **nothing is written**. The response carries `candidates[]` (`uid`, `displayName`, `similarity`, `reason`, `relationCount`) so the caller can reuse a uid, merge with `mergeEntities`, or retry with `force: true`.
- **Name-embedding in the advisory band** `[0.88, 0.95)` → `status: created` **with** advisory `candidates[]`: the entity is minted, but near-matches are surfaced so the agent can still choose to merge.
- **No match** → `status: created`.

Refuse-on-exact-name is intentional **homonym safety**: two real people can share a name, so the gate never auto-merges — `force: true` is the documented escape hatch (e.g. a second real "Christian"). The matcher still runs under `force`: a forced mint succeeds (`status: created`) but the response still lists the near-matches it forked from in `candidates[]`, so only the refuse/reuse *decision* is suppressed, never the visibility. A nil matcher (degraded boot, no embedder/vector) skips the gate so an infra fault never blocks a write; the exact-name and identifier lanes still work without a vector backend.

`createEntity` also **refuses the reserved core kind `document`** (`E_VALIDATION`): a document is a vault note owned by the `writeDocument` write path (which also creates the backing documents-table row, body, and vault path), so a bare `document` entity would strand a body-less "ghost document" that 404s in every doc-oriented surface. The resolver remaps the same kind to `topic` on extraction; the direct path refuses it outright (a namespaced `user:document` is a distinct extension kind and is allowed). `--batch` inherits this guard.

`maz entity create --batch <file.jsonl>` runs one createEntity request per line through this same gate and tallies `created` / `reused` / `duplicate_candidates` / errors.

## Merge, delete, and edge dedup

- **`mergeEntities`** (`maz entity merge <source> <target>`; emits an `entityMerged` event — the tool verb is imperative, the event past-tense) repoints every reference (relations, facts, mentions, identifiers, alternate-name aliases, insights, citations, provenance) from source to target, then deletes the source row. Aliases (`entity_aliases`, migration 0040) carry no FK cascade, so the merge moves them explicitly — a shared alias collapses to one row via the composite-PK `INSERT OR IGNORE` — and `deleteEntity` removes them; otherwise a `[[Old Name]]` wikilink would resolve to a vanished uid. Repointing can turn two distinct rows into the same triple (both duplicate entities held the same edge), so the merge **collapses duplicate active edges** in the same transaction: it drops self-loops (a `same_as` edge between the pair becomes `tgt→tgt`), keeps the **highest-confidence** row per `(subject, predicate, object, status)` among live relations (ties broken by lowest uid, so a later higher-confidence reassertion is never downgraded to an older weaker duplicate), and dedups identical open facts the same way ranked on `source_priority`. Status is part of the key, so a `proposed` edge never collapses into an `asserted` one.
- **`entityDeleted`** (`maz entity delete <uid>`, the `deleteEntity` tool) hard-deletes a genuinely-unwanted entity and **all** its derived rows (no FK cascade exists for most of them, so a bare delete would strand dangling pointers). Safe default: it refuses (`E_VALIDATION`) when the entity still has active relations, unless `--cascade` / `cascade: true`, which tombstones each active relation (`relationForgotten`, for the per-edge audit trail) before the `entityDeleted` tombstone. To consolidate **duplicates** without losing data, merge — don't delete.
- **Detective control.** `maz doctor` carries two advisory data-quality checks (`dataquality.duplicate_entities`, `dataquality.duplicate_active_edges`) that WARN when duplicates already exist in the graph — the counterpart to the create-time and merge-time prevention above.

## Pending-review queue

`maz review` surfaces the queue in the TUI: paginated list of pending matches with confidence, suggested action (merge / keep separate / new entity), and the supporting evidence. Keybindings `y` to accept, `n` to reject, `j/k` to navigate. CLI mode prints the queue as JSON when `--json` is set.

The pending queue is the manifestation of *"enrichment proposes, the user disposes."* Nothing auto-promotes from a borderline match.

## "Related but distinct" relations

Sometimes two entities are clearly different (a person and their employer; a parent company and a subsidiary) but related. The resolver writes a soft relation in the graph: `entities/A --[employed_by, confidence=0.95]--> entities/B`. These never collapse into a single entity but remain queryable.

<!-- TODO(m1): expand: relation taxonomy, the `business-de` schema pack for Handelsregister relations, cycle detection. -->
