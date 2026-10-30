# Concepts

Mazarbul is an **agent system of record**: a local-first, event-sourced store
for the knowledge an agent or human accumulates over time. This topic maps the
five core data types to the commands that read and write them.

## Data model

### Episode

A raw natural-language text unit — a note, a conversation turn, a log entry.
Episodes are the primary write surface and the source material for extraction.

- **Write:** `maz remember`
- **Read:** `maz recall`, `maz ask`, `maz log --kind episode`,
  `maz get episodes/{uid}`

### Entity

A named node (person, project, concept, tool, …) extracted from episodes by
the background extractor, or created explicitly.

- **Write:** `maz entity create` (explicit); extraction is automatic.
- **Read:** `maz get entities/{uid}`, `maz get entities/self`,
  `maz entities`, `maz show {name}`, `maz neighbors {name}`

### Relation

A typed, directed edge between two entities (`works_at`, `depends_on`, …),
extracted automatically or added with `maz relate`.

- **Write:** `maz relate`
- **Read:** `maz relations {entity}`, `maz neighbors {entity}`,
  `maz traverse`

### Fact

A scalar key-value state slot attached to an entity. Facts are superseded over
time: writing a new fact for the same key replaces the previous value rather
than appending.

- **Write:** `maz fact set {entity} {key} {value}`
- **Read:** `maz fact get {entity} {key}`

### Scope

A named partition (`personal`, `project`, `team`) that isolates episodes,
entities, and facts. Most commands accept `--scope` or the `MAZ_SCOPE` env var.

See `maz help-topics scopes` for the full scope model including encryption and
remote sync.

## Storage layers vs. data types

The data types above are **logical**. Underneath, Mazarbul uses four storage
layers (SQLite/FTS5, sqlite-vec, LadybugDB, Git) that implement them. Do not
conflate the two: `maz recall` is not a SQLite query — it fuses all four
retrieval lanes. See `maz help-topics four-layer-storage` for the physical
layout.

## Authorship and provenance

Every event in the log carries a signed author: a principal (your bound
identity), a device key, and an ed25519 signature. `entities/self` is the
entity that represents the bound principal — `maz get entities/self` shows it.
An entity's `provenance[]` field lists the episodes it was extracted from;
this is **distinct** from `maz source` external SourceRefs, which record links
to upstream systems (tickets, documents, pull requests) rather than the
extraction lineage.

## Quick orientation

```
maz help-topics concepts         # this page
maz introspect                   # live ontology: all known entity/relation kinds
maz context build                # cold-start brief of the current working set
maz help-topics four-layer-storage  # physical storage details
maz help-topics scopes           # scope isolation and sync
maz help-topics recall           # retrieval tuning
```
