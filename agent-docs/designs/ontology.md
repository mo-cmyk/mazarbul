# Ontology Design Document

**Status**: Draft (M1)
**Author**: @ontology-architect
**Created**: 2026-05-24
**Last updated**: 2026-05-24
**Epic**: [#13 ontology v1](https://github.com/mo-cmyk/mazarbul/issues/13)

## Context

Mazarbul stores memories as JSONL events validated against JSON Schemas. Schemas are the contract: the event-log writer (#20) validates payloads against them before append; the query composer (#56) trusts that recalled payloads conform; the MCP server (#16) exposes them to agents through `tools/list`. To make the Go type and the wire schema impossible to drift apart, schemas are generated from Go structs via `invopop/jsonschema` at first access and registered against `kernel.Ontology`.

PR #9 landed the registry interface (`kernel.Ontology` + `kernel.Schema`). This package, `internal/ontology/`, ships the M1 kinds (Episode here; Entity #34, Mention #35, Relation #36 follow the same pattern), plus an in-process registry (#37) that replaces the test fake currently shipped alongside Episode.

Architecture references: the original architecture notes (JSON Schema as wire format), the original architecture notes (Mention vs Entity distinction), `AGENTS.md` rules 1 (no proto), 2 (AIP naming), 3 (event log append-only), 4 (provenance on every fact).

## Goals

- One source of truth per kind: the Go struct.
- JSON Schema reflected from the struct at first access through a `sync.OnceValue` cache; pay-once per process.
- Registration follows the strict `kernel.Ontology` contract: first registration of a Kind wins; subsequent registrations must use a higher Version or return `ErrSchemaConflict`.
- JSON Schema Draft 2020-12 (invopop's default); runtime validation via `santhosh-tekuri/jsonschema/v6`.
- AIP-compliant naming everywhere on the wire: camelCase JSON field names, `createTime` (not `createdAt`), plural lowercase collection paths (`episodes/{uid}`), ULID identifiers.

## Non-goals

- Schema versioning beyond v0.1 (M2 introduces the migration story).
- Multi-language `text` fields (M2 if needed).

> **Pulled forward.** The non-goals below have been promoted into the dynamic-ontology
> effort — see [dynamic-ontology.md](dynamic-ontology.md): a persistent, event-sourced,
> git-rendered registry; an open entity-kind taxonomy with typed properties; world-ontology
> imports (schema.org, Wikidata, FOAF, Dublin Core, SKOS); and plugin-supplied schema packs.
> - ~~Plugin-supplied schemas (M3; first-party plugins later).~~
> - ~~Persistent on-disk registry (M2; M1 is in-process only).~~
> - ~~World-ontology imports (Wikidata, schema.org) (M2+).~~

## Proposal

### Package layout

```
internal/ontology/
  doc.go                          # package comment
  episode.go                      # #22
  episode_test.go
  entity.go                       # #34
  mention.go                      # #35
  relation.go                     # #36
  registry.go                     # #37 MemoryOntology, replaces the test fake
  episode_forgotten.go            # M1 closeout: tombstone event for `maz forget`
  entity_identifier_added.go      # M1 closeout: alias/email/etc rows the resolver reads
```

M1 closeout added two new kinds beyond the original four (Episode, Entity, Mention, Relation):

| Kind                          | Const                                 | Payload struct                       | Triggers                                                                 |
|-------------------------------|---------------------------------------|--------------------------------------|--------------------------------------------------------------------------|
| `episodeForgotten`            | `KindEpisodeForgotten`                | `EpisodeForgottenEvent`              | `maz forget` / `forget` MCP tool                                         |
| `entityIdentifierAdded`       | `KindEntityIdentifierAdded`           | `EntityIdentifierAddedEvent`         | Resolver on entity create + alias merge                                  |
| `relationSupersedeConflict`   | `KindRelationSupersedeConflict`       | `RelationSupersedeConflictEvent`     | Resolver post-pull replay when a functional slot has >1 open arms |

Both are JSONL-replayable via `maz rebuild`. The dispatcher branches on kind in `internal/kernel/storage/dispatcher/dispatcher.go` to cascade-delete derived rows (episode tombstone) or upsert into `entity_identifiers` (identifier add).

Each kind owns one file. Each file exports four symbols by convention: `Kind<Name>` (const), `Version<Name>` (const), `<Name>Schema() kernel.Schema` (cached accessor), and `Register<Name>(ctx, kernel.Ontology) error` (installer; named per-kind so siblings coexist without function-name collisions). The struct itself carries the data; the rest is plumbing.

### Episode (this PR, #22)

**Required fields** (`required` in JSON Schema): `name`, `uid`, `createTime`, `text`, `source`, `confidence` (0..1).

**Optional fields** (`omitempty` in JSON tag, absent from `required`): `mentionUids`, `relationUids`, `tags`, `parentEpisodeUid`.

Two AIP corrections were applied vs the original issue body:
- `parent_episode_uid` → `parentEpisodeUid` (snake_case is forbidden on the wire per AGENTS.md rule 2).
- `entities` → `mentionUids`, `relations` → `relationUids` (the acceptance criterion clarifies these are Mention UIDs and Relation UIDs; the wire names now match).

The Go field names use the project initialism convention from `.golangci.yml var-naming`: `UID`, `MentionUIDs`, `RelationUIDs`, `ParentEpisodeUID`. JSON wire names remain camelCase per AIP.

### Schema reflection

```go
r := &jsonschema.Reflector{
    ExpandedStruct:             true, // inline the root schema; do not wrap in $defs
    RequiredFromJSONSchemaTags: true, // honor `jsonschema:"required"`
    DoNotReference:             true, // resolve inline, no $ref
}
def := r.Reflect(&Episode{})
```

Field-level constraints live in `jsonschema:"..."` struct tags:
- `required` to add the field to the schema's `required` array.
- `pattern=^...$` for ULID and resource-name validation.
- `minLength=1` for non-empty strings (`text`, `source`).
- `minimum=0,maximum=1` for `confidence`.
- `format=date-time` for `createTime`.
- `description=...` for the AIP-203 field-behavior annotations.

The reflected Schema lives behind `sync.OnceValue(buildEpisodeSchema)`, annotated `//nolint:gochecknoglobals // process-lifetime schema cache, pay-once via sync.OnceValue`. This avoids a `.golangci.yml` path exclusion for the package.

### Registration

```go
func RegisterEpisode(ctx context.Context, o kernel.Ontology) error {
    return o.Register(ctx, episodeSchema())
}
```

No `init()` anywhere. Each kind exposes a package-level `Register<Kind>` function callers must invoke explicitly; the per-kind suffix is what lets all four M1 kinds live in the same package without colliding. The wiring point post-#37 is the `kernel.Bootstrap` constructor (or whatever orchestrator lands then); for this PR, the only caller is the test fake.

### Validation

`santhosh-tekuri/jsonschema/v6` is the runtime validator. It supports Draft 2020-12 (the draft invopop emits) and is actively maintained. The test fake compiles each registered schema lazily on first `Validate` and caches the compiled validator.

In production, #37's `MemoryOntology` will mirror that compilation-and-cache strategy plus persistence-on-disk semantics, but the surface stays identical.

### Versioning

`v0.x` means breaking changes are allowed pre-1.0. M1 ships every kind at `v0.1`. The migration story (M2) will introduce: a register-twice rule for major-version bumps, a schema-migration event emitted into the event log when a Kind crosses a major version, and a rebuild-on-pull invariant for the four-layer indexes.

### AIP naming applied

| AIP | Application |
|---|---|
| 122 (resource names) | `episodes/{uid}` collection + plural lowercase |
| 123 (resource types) | `Episode` PascalCase singular type; JSON field names camelCase |
| 142 (timestamps) | `createTime` field, `time.Time` in Go, RFC 3339 UTC on the wire |
| 154 (resource freshness) | `uid` is output-only and immutable |
| 203 (field behavior) | Each field's `description` says "Required" / "Optional" / "Output-only" |

## Storage impact

None for this PR. Schemas live in-process. Downstream consumers:
- Writer (#20) calls `Ontology.Validate(ctx, "episode", payload)` before appending; failure rejects the write.
- Registry (#37) compiles schemas once and caches the validator.

## Risks

- **invopop tag drift from JSON Schema spec.** Mitigation: round-trip test in every kind's `_test.go`. (#22 already lands two.)
- **Issue body uses snake_case (`parent_episode_uid`).** Mitigation: PR description flags the correction; AGENTS.md rule 2 is the binding constraint.
- **`entities` / `relations` field-name ambiguity.** Resolved: renamed to `mentionUids` / `relationUids` per the acceptance criterion's "Mention UIDs" / "Relation UIDs" phrasing.
- **invopop reflection cost at startup.** Mitigation: `sync.OnceValue` makes it pay-once for the process lifetime.

## Acceptance criteria

- [x] Episode (#22): Go struct + sync.OnceValue cache + Register + round-trip and negative tests, design doc lands in the same PR.
- [ ] Entity (#34): mirror the pattern.
- [ ] Mention (#35): mirror the pattern.
- [ ] Relation (#36): mirror the pattern.
- [ ] Registry (#37): `MemoryOntology` replaces the test fake; replaces `NoopOntology` in production wiring.

## Open questions

- When does `ParentEpisodeUID` get a value? (M2 threaded-memory tool likely.)
- Exact production wiring point for the per-kind `Register` calls (probably a `kernel.Bootstrap` constructor in #37).
- Schema-extension semantics for plugin authors (M3; might add a `provenance: "extension:<name>"` lane).

## Approval

- [ ] Reviewed by: owner subagent
- [ ] Architecture decisions agreed upon
