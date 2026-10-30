# Dynamic Ontology Design Document

**Status**: Approved
**Author**: @ontology-architect
**Created**: 2026-06-05
**Last updated**: 2026-06-07
**Supersedes (pulls forward)**: `ontology.md` non-goals — persistent on-disk registry (M2), world-ontology imports (M2+), plugin-supplied schemas (M3).

## Context

Mazarbul's ontology is half-dynamic. **Predicates** (link/relation types) are already a living vocabulary: a SQLite `predicates` table, a `predicateRegistered` event, provenance (`seed`/`llm`/`user`), a curated lexicon plus LLM auto-classification, surfaced via `maz schema predicates` and the `introspect` MCP tool. **Entity kinds are the opposite** — a closed six-value enum (`person, organization, place, project, event, document`) baked into Go in three places (`internal/ontology/entity.go:70` `EntityKind.JSONSchema()`, `internal/entity/types.go:29`, `internal/ontology/mention.go:89`) and enforced on the write path, so a kind like `vehicle` or `company` is rejected before it can be stored.

The generic substrate already exists and is correct: `kernel.Ontology` / `MemoryOntology` (`internal/kernel/ontology.go`) is a kind-keyed JSON-Schema registry with semver versioning, Draft-2020-12 validation, idempotent register, version-bump-to-replace, and a `Provenance` field that already contemplates `core` / `extension:<name>` / `user`. Its one limitation: **it is in-memory only, rebuilt from hardcoded `Register*` calls at every boot** (`internal/daemon/wiring_ontology.go`, `internal/kernelboot/bootstrap.go`).

We want agents (via MCP) and humans (via CLI) to add custom entity types, give them typed properties, reuse and extend external vocabularies (schema.org, Wikidata, FOAF, Dublin Core, SKOS), and define custom relations — without forking the binary — while preserving Mazarbul's discipline: append-only event log as source of truth (rule 3), provenance on every fact (rule 4), JSON Schema as wire format (rule 1), AIP naming (rule 2), `maz rebuild` replay, **git-tracked & syncable state**, and "enrichment proposes, the user disposes." The cost of not doing this: every domain vertical the project has scoped (`business-de` schema pack, Erebor fundraising, connector-driven enrichment) is blocked on the closed enum.

This is the project's own ontology roadmap pulled forward and sequenced, grounded in how Palantir Foundry, RDF/OWL/SHACL, and schema.org model the same problem (see Sources).

## Goals

- [ ] Entity kinds become an **open, event-sourced, git-rendered registry**, mirroring the proven predicate pattern — addable by humans (CLI), agents (MCP), the extractor/resolver (auto-register), and schema packs (extensions).
- [ ] **Typed properties** (Palantir-style / SHACL-style) per kind, expressed as JSON Schema, with `extends` (RDFS `subClassOf`) composition via `allOf`.
- [x] **Vocabulary reuse**: `sameAs` IRIs + an importer for schema.org / Wikidata / FOAF / Dublin Core / SKOS. Phase 6 ships: vocab importer with schema.org offline snapshot (~30 types) + crosswalk, `schemapack` engine, `schema-org-core` + `business-de` bundled packs, `maz install <pack>` and `maz schema import <vocab>` (with `--dry-run`/`--types`/`--from-file`). Provenance = `extension:<name>`; idempotent installs survive `maz rebuild`.
- [ ] Type definitions **survive restart, replay under `maz rebuild`, and sync via the git transport** (rendered under a stable `ontology/` path).
- [x] **Autonomy-first**: the system never blocks on a human by default; low-confidence novel kinds route to `maz review`; a governed mode is opt-in. Phase 7 ships: `permissive` (default) / `governed` config modes, the `entityKindReviewed` event, `maz schema review list|approve|reject`, and `maz schema lint`.
- [x] Custom relations gain optional domain/range and typed attributes (Phase 5: `subjectKinds`/`objectKinds`/`sameAs`/`propertySchema` on `predicateRegistered` v0.2.0, validated on the write path under `[ontology] strict_domain_range`).

## Non-goals

- Foundry **kinetic** elements (actions/write-back functions) — semantic layer only.
- OWL reasoning / inference engine — JSON Schema (SHACL-style shapes) for validation only; open-world reasoning is out of scope.
- Multi-parent (multiple inheritance) for `extends` — single inheritance in v1.
- Live network fetch of external vocabularies at import time — bundled offline snapshots; `--from-file` for the long tail.
- A bespoke approval UI — governed mode reuses the `maz review` queue mechanism.

## Proposal

The throughline: **generalize the proven predicate pattern to entity kinds, make the registry a durable + git-rendered projection of the event log (like the graph/vector/FTS layers), then layer typed properties, vocabulary reuse, and authoring surfaces on top.** Every new capability reuses existing machinery — the JSON-Schema registry for validation, the `predicateRegistered` event/lexicon/auto-classify pattern for vocabulary growth, the `maz review` pending queue for "propose, then dispose," the dispatcher apply-switch for replay, and the `internal/sync/render` layer for git tracking.

### Foundry-inspired, Mazarbul-native model

| Foundry concept | Mazarbul mapping | State today |
|---|---|---|
| Object type | **Entity kind** | closed enum → open, event-sourced, git-rendered registry |
| Property | **Typed property schema** per kind (JSON Schema) | does not exist |
| Link type | **Predicate** | already a dynamic registry; add domain/range + typed attributes |
| (vocab reuse) | `sameAs` IRIs + `extends` parent (JSON-LD `@id` / RDFS `subClassOf`) | does not exist |

### Data model

Three event kinds carry type definitions into the log. Two are new; the third extends an existing kind. They follow the `predicate_registered.go` boilerplate exactly (Go struct is source of truth → invopop reflection → `sync.OnceValue` cache → `RegisterXxx`).

**`entityKindRegistered`** (`internal/ontology/entity_kind_registered.go`, `v0.1.0`) — one entry in the entity-kind taxonomy:

```jsonc
{
  "name": "user:research_paper",            // bare snake_case for core (person); namespaced for custom
  "displayName": "Research Paper",
  "description": "A scholarly article or preprint.",   // required; drives dedup + agent reuse
  "extends": "document",                     // optional RDFS subClassOf; single inheritance
  "sameAs": ["schema.org/ScholarlyArticle", "wikidata:Q13442814"],
  "propertySchema": { "type": "object", "properties": { "doi": {"type": "string"} } },
  "provenance": "user",                      // core | user | extension:<name>
  "addedBy": "user",                         // seed | llm | user | extension
  "addedAt": "2026-06-05T10:00:00Z"
}
```

**`schemaRegistered`** (`internal/ontology/schema_registered.go`, `v0.1.0`) — the reflexive escape hatch carrying an arbitrary runtime JSON-Schema registration (a kind's property schema keyed `entityKind:<name>`, a relation attribute schema, or a fully custom shape from `defineSchema`/a schema pack):

```jsonc
{
  "schemaKind": "entityKind:vehicle",
  "version": "v0.1.0",
  "definition": { "type": "object", "properties": { "vin": {"type": "string"} } },
  "provenance": "user",
  "addedBy": "user",
  "addedAt": "2026-06-05T10:00:00Z"
}
```

**`predicateRegistered`** (extended in-place to `v0.2.0`, Phase 5) — additive `omitempty` fields: `subjectKinds []string` (domain), `objectKinds []string` (range), `sameAs []string`, and `propertySchema json.RawMessage` (a JSON Schema for the relation's attributes). Backward compatible: a v0.1.0-shaped event with none of these fields still validates. The dispatcher persists each field into the `predicates` table (migration `0020_predicate_domain_range`); `SQLiteOntology.ValidateRelation` reads them on the relation write path. Domain/range and attribute constraints are advisory by default and enforced (reject) only when `[ontology] strict_domain_range = true`; otherwise violations are warn-logged and accepted, and an endpoint whose kind cannot be resolved (not yet materialized, or the `self` sentinel) skips its check.

`addedBy` is a shared `EntityKindAddedBy` enum (`seed|llm|user|extension`). `provenance` is the namespace lane and maps directly onto the existing `kernel.Schema.Provenance` field.

### API

New MCP tools (`internal/mcp/tools/`, AIP-named, each ships `InputSchema`+`OutputSchema` + ≥1 error-code test; template `relate.go`):

```jsonc
// defineEntityKind — mint/extend an entity kind; built-in reuse-before-mint dedup-check.
{ "name": "defineEntityKind",
  "inputSchema": { "type": "object",
    "properties": { "name": {...}, "displayName": {...}, "description": {...},
      "extends": {...}, "sameAs": {...}, "propertySchema": {...}, "force": {...} },
    "required": ["name", "displayName", "description"] } }
```

Also `definePredicate`, `defineSchema`, `createEntity` (the missing primitive — mint an entity of a registered kind directly so `relate` has endpoints without an NL round-trip), and `describeType` (rich single-type discovery). `introspect` gains `displayName`/`description`/`provenance`/`extends`/`sameAs` on each entity kind plus a `namespaces` summary (additive, backward-compatible). Error codes stay `E_VALIDATION` / `E_NOT_FOUND` / `E_INTERNAL`.

CLI (`internal/cli/schema.go`, cobra + `output.From` + `wrapSchemaFatal` + `OpenWiring`): `maz schema kinds list|show|add`, `maz schema predicates add`, `maz schema alias`, `maz schema import <vocab>`, `maz schema review list|approve|reject`, `maz schema lint`. `maz install <pack>` for schema packs.

### Business logic

**Event-sourced, git-rendered flow:** a registration (from any of the four sources) appends one of the three events → the writer validates it against `kernel.Ontology` → the dispatcher's `applyLocked` switch materializes it into the `entity_kinds` / `ontology_schemas` / `predicates` projection tables (idempotent UPSERT; `core` rows guarded from user overwrite) → the render pipeline writes it to the git-synced `ontology/` tree. `maz rebuild` replays the log and reconstructs the registry; the git transport carries it to other stores.

**Open entity-kind validation (two-phase):** `EntityKind.JSONSchema()` drops its closed enum for an open `string` + snake_case `pattern` (phase 1 — structural). A registry-membership check at the write-path choke point (`file_eventlog.go:1128`, after `ontology.Validate`) verifies the kind exists in `entity_kinds` (phase 2 — semantic); unknown → warning-accept + auto-register, never a silent loss. The six Go constants stay as compile-time handles.

**Autonomous auto-registration:** `autoRegisterEntityKind` (twin of `autoRegisterPredicate`) consults a curated `entityKindLexicon` first (zero-LLM for common kinds), falls back to an LLM classify-prompt for novel kinds (provenance `user`, `addedBy llm`), with a per-ingest cap (drift guard). **Low-confidence novel kinds enqueue for `maz review`** rather than auto-promoting — the same gray-zone discipline the resolver applies to `split_candidate` entity merges.

**Reuse before mint (dedup):** every `define*` call embeds `displayName + " — " + description`, searches a tiny per-type vector index plus a lexical/trigram pre-filter; high-similarity → reuse the existing kind (no duplicate); medium → mint + auto-record a SKOS `closeMatch`; low → mint clean; `force` overrides. Degrades to lexical-only when no embedder is wired.

**Internal kind-to-kind closeMatch URN scheme.** When the dedup check yields a medium-similarity outcome (`MintWithAlias`), `defineEntityKind` appends the internal alias URN `maz:kind:<matchName>` to the minted kind's `SameAs` list (the caller's own `sameAs` entries are preserved; the URN is not doubled if already present). The response carries `closeMatch` (the matched kind name) and `similarity` (the dedup score) so the calling agent can surface the relationship in its output or prompt the user to reconcile. This uses [skos:closeMatch](https://www.w3.org/2004/02/skos/core#closeMatch) semantics: concepts that are closely related and usable interchangeably in some contexts but not declared identical. The `maz:kind:` prefix is Mazarbul's internal vocabulary namespace for kind-to-kind aliases; it is distinct from external vocabulary IRIs (schema.org, wikidata) which travel alongside it in `SameAs` unchanged.

**Typed properties:** a kind's `propertySchema` registers under `entityKind:<name>`; `extends` composes parent + child via `allOf` (single inheritance, cycle-checked) at registration. Entity instance attributes validate against it only when `[ontology] strict_attributes` is enabled (default off, so autonomous extraction is never blocked).

### Phase 7 — Governance and hygiene

**Permissive vs governed mode (config `[ontology].mode`).** Mazarbul ships autonomy-first: `mode = permissive` is the default — confident mints land active immediately, no human in the loop. The one exception preserves the "propose, don't silently promote" discipline: a **low-confidence** novel kind from the resolver's auto-register path lands `status = "proposed"` (the review queue) even in permissive mode, rather than being promoted on a weak signal or dropped. `mode = governed` is an opt-in gate that widens this: *all* mints from `defineEntityKind` (MCP) and the resolver's auto-register path set `status = "proposed"`. The routing matrix — permissive: confident → active, low-confidence → proposed; governed: all → proposed. Proposed kinds are excluded from the active taxonomy (they do not appear in `introspect` / `describeType` kind lists and are not valid on the entity write path) until a human disposes them.

**Status lifecycle.** `entity_kinds` rows carry `status ∈ {active, proposed, rejected}` (migration `0022_ontology_status`). Core kinds (`person`, `organization`, `place`, `project`, `event`, `document`) are write-protected and always `active`; a governed-mode disposition cannot demote them. The transition graph is: `proposed → active` (approved) or `proposed → rejected`; both are terminal for the current event. Re-registering a rejected kind with new parameters is allowed (a fresh `entityKindRegistered` event creates a new row; the old rejected row is superseded by its own `entityKindReviewed` event).

**`entityKindRegistered` v0.2.0.** The event gains an optional `status` field (empty = `"active"`, backward compatible). This is how governed mode stamps newly minted kinds as proposed in the event log itself rather than via a post-hoc update.

**`entityKindReviewed` event** (`internal/ontology/entity_kind_reviewed.go`):

```jsonc
{
  "name": "user:research_paper",
  "decision": "approved",          // approved | rejected
  "reviewedBy": "moritz@…",
  "reviewedAt": "2026-06-07T09:00:00Z",
  "note": "Consistent with document subtype policy."  // optional
}
```

The dispatcher applies it by UPSERTing `status` on the `entity_kinds` row (idempotent on replay). A `rejected` decision does not delete the row; it soft-tombstones it so the event log remains the sole source of truth and `maz rebuild` faithfully reconstructs the rejection.

**Dedup thresholds (config).** `[ontology] dedup_high_threshold` (cosine floor to auto-reuse an existing kind, bypassing mint) and `[ontology] dedup_medium_threshold` (floor to mint + record a SKOS `closeMatch`) are now user-configurable. Zero value falls through to the dedup-package defaults. These knobs let operators tighten or loosen the reuse-before-mint policy without rebuilding.

**`maz schema review list|approve|reject <name>`.** The CLI disposition surface for governed mode. `list` prints all `proposed` kinds (table or `--json` JSON Lines). `approve <name>` and `reject <name>` append `entityKindReviewed` events; both accept an optional `--note` flag. Exit codes: 0 (success), 1 (kind not found or already disposed), 2 (internal error). `approve`/`reject` are no-ops in permissive mode (there are no proposed kinds to act on), but they work: a kind manually put in `proposed` status by a direct event can still be disposed.

**`maz schema lint`.** A read-only hygiene sweep over the live `entity_kinds` and `predicates` tables; does not write events. Reports three classes of finding:

| Finding | Severity | What it means |
|---|---|---|
| Near-duplicate kinds | warning | Two kinds whose `displayName — description` text is too similar — possible redundant mints. When an embedder is available the check uses cosine similarity of the embeddings against `dedup_medium_threshold` (the spec'd path); offline (no embedder) it falls back to a word-level LCS similarity against a dedicated lexical threshold (~0.50) rather than reusing the cosine-calibrated value. |
| Unused predicates | info | Predicates in the registry with zero relations using them — candidates for removal or reuse. |
| Kinds missing `sameAs` | info | Custom kinds (`provenance = user` or `extension:*`) with no external-vocabulary IRI — reduces portability. |

Outputs a human-readable table by default; `--json` emits an aggregate findings object and `--ndjson` emits JSON Lines, each finding shaped `{"check":"near-duplicate-kinds"|"unused-predicate"|"missing-sameAs", "name":"…", "message":"…"}`. `--strict` exits non-zero when any finding is reported. Intended for post-ingest QA and as a CI gate for teams running in governed mode.

**Git transport.** The render pipeline (`internal/sync/render/ontology.go`) writes `ontology/kinds/<name>.yaml` for every kind, including `proposed` and `rejected` ones. The `status` field travels through `maz sync`, making proposals diffable across stores and auditable in git history.

### Storage impact

- New migration `0019_ontology_registry`: tables `entity_kinds(name PK, display_name, description, extends, same_as JSON, property_schema JSON, provenance, added_by, added_at)` and `ontology_schemas(kind PK, version, definition, provenance, added_at)`. (`entities.kind` already has no CHECK constraint — `0001:23` is just an index — so no column change.)
- New migration `0020_predicate_domain_range`: `ALTER TABLE predicates ADD domain_kinds / range_kinds / same_as / provenance`; widen `added_by` CHECK to include `extension`.
- New migration `0022_ontology_status` (Phase 7): `ALTER TABLE entity_kinds ADD status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','proposed','rejected'))`. Core kind rows are backfilled to `active` by the migration.
- New render target `internal/sync/render/ontology.go` writes `ontology/kinds/<name>.yaml`, `ontology/predicates.yaml`, `ontology/schemas/<kind>.json` deterministically (mirrors `entities_jsonl.go`/`relations.go` + `determinism_test.go`). Phase 7 adds `status` to each `ontology/kinds/<name>.yaml`.
- **Genesis seeding** moves the existing predicate seed off `0006`/`0012` SQL into first-boot `predicateRegistered` + `entityKindRegistered` events so the log alone reconstructs the full taxonomy and it syncs via git.

### Privacy and provenance

Every type definition carries `provenance` (namespace lane) + `addedBy` (agent) + `addedAt`, satisfying rule 4. Type definitions are schema-level metadata, not user content, and render to git in plaintext by default (consistent with the plaintext-default sync). No PII in type definitions; entity instances continue under the existing privacy model.

### TUI impact

None in this effort. The TUI may later surface `maz schema review` and the taxonomy browser; logged to the Mintlify backlog for when those land.

## Alternatives

- **Side-table registry bypassing the log** (write directly to SQLite). Rejected: breaks the "log is sole source of truth" invariant, doesn't sync via git, doesn't replay under `maz rebuild`. Event-sourcing is the whole point.
- **Keep entity kinds closed; only document the six.** Rejected: blocks every domain vertical; the closed enum is the explicit blocker.
- **Full OWL/RDF triple store.** Rejected: open-world reasoning is overkill for a local-first fact store; JSON Schema shapes + property-graph storage are the right weight (the SHACL-over-property-graph consensus from research).
- **A new `relationTypeRegistered` event** instead of extending `predicateRegistered`. Rejected: would fork the resolver's mature auto-classification path; additive fields on the existing event are backward compatible.

## Risks

- **LLM kind-invention drift** (same failure mode noted for predicates) — mitigated by lexicon-first classification, a per-ingest auto-mint cap, Info-logging every mint, the dedup auto-reuse threshold, low-confidence → `maz review`, and `maz schema lint` after the fact.
- **Genesis-seed cleanup touches Rebuild/determinism + render tests** — bundled into Phase 1 so the invariant flips once, cleanly.
- **Out-of-order synced instance events** (an Entity referencing a not-yet-replayed kind) — warning-accept and reconcile on replay (the `ErrSchemaNotRegistered` lenient path the contract already hints at); the autonomous local path always registers-first.
- **`extends` cycles / deep chains** — cycle check + depth bound at registration.

## Migration

Pre-prod, clean cuts (no alias windows). The six core kinds + ~20 seed predicates become genesis events at first boot (idempotent UPSERT → replay no-op). Opening the enum bumps `VersionEntity` to `v0.3.0`. No data migration of existing `entities.kind` values (already plain TEXT).

## Acceptance criteria

The same `vehicle` kind (`propertySchema {vin, make}`, `extends thing`, `sameAs schema.org/Vehicle` + `wikidata:Q42889`) is authorable AND usable from every surface, all reading the one event-sourced, git-rendered registry:

- [ ] **Human (CLI):** `maz schema kinds add vehicle …`; `maz sync diff` shows the new `ontology/kinds/vehicle.yaml`.
- [ ] **Agent (MCP):** `defineEntityKind` / `createEntity` / `relate`; `introspect` / `describeType` list it.
- [ ] **Pipeline:** extractor/resolver auto-registers a novel kind on ingest (low-confidence → `maz review`), no enum rejection.
- [x] **Plugin:** `maz install business-de` registers kinds/predicates (`extension:business-de`), survives `maz rebuild`, renders to git. Implemented in Phase 6: `schemapack` engine parses manifest.toml + kinds/ + predicates.yaml, `Installer.Install` appends events with `provenance=extension:<name>`, UPSERT semantics make it idempotent.
- [ ] Capstone: install `business-de`, ingest German corporate text, assert a `company` entity with `hrb` external ID + a `managing_director` relation is extracted, typed-validated, git-rendered, and `maz rebuild`-reconstructable.

## Open questions

- Multiple inheritance for `extends` (deferred; single-parent v1).
- Whether agents should be allowed to trigger `import_vocabulary` via MCP (default: human-CLI-only; importing hundreds of types is not a mid-conversation action).
- ~~Promotion of the `maz review` disposition to a replayable `…Reviewed` event (deferred; `status` field for v1, matching the existing `maz review` precedent).~~ Resolved in Phase 7: `entityKindReviewed` event ships with `decision: approved|rejected`, `reviewedBy`, `reviewedAt`, and `note`; migration `0022_ontology_status` adds the `status` column to `entity_kinds`.

## Approval

Approved 2026-06-05 by the user. Built as 8 dependency-stacked PRs (P0 ADR → P1 durable+git registry → P2 open enum + auto-register + review → P3 typed properties + extends/allOf → P4 MCP/CLI authoring + dedup → P5 typed relations → P6 import + schema packs → P7 governance + lint), each held at 5/5 merge-ready, batch-merged at the end.

## Sources

- [Palantir Foundry — Ontology overview](https://www.palantir.com/docs/foundry/ontology/overview), [object & link types](https://www.palantir.com/docs/foundry/object-link-types/object-types-overview), [properties](https://www.palantir.com/docs/foundry/object-link-types/properties-overview) — the semantic (object/property/link) split.
- [A formal approach for customization of schema.org based on SHACL](https://arxiv.org/pdf/1906.06492) — per-domain property shapes (the typed-property model).
- [schema.org data model](https://schema.org/docs/datamodel.html) + [additionalType](https://schema.org/additionalType) — extension via `additionalType`/`sameAs`, JSON-LD `@context`.
- [TypeDB entities/relations/attributes](https://typedb.com/docs/core-concepts/typeql/entities-relations-attributes/) — subtypable entity/relation/attribute type system (the `extends` model).
- [OWL vs SHACL open/closed-world semantics](https://volodymyrpavlyshyn.medium.com/open-world-vs-closed-world-modeling-owl-and-shacl-semantics-in-agda-f4601229630b) — why SHACL-style closed-world validation over OWL reasoning.
- [SKOS reference](https://www.w3.org/2004/02/skos/) — `exactMatch`/`closeMatch`/`broadMatch`/`narrowMatch` for the alias/dedup layer.
