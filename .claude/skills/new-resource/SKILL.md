---
name: new-resource
description: Scaffolding for a new Mazarbul resource (MCP tool, event type, entity kind, or connector). USE WHEN adding a new entity to the kernel, registering a new MCP tool, defining a new event type, or wiring a new connector. Covers schema, kernel writers, MCP surface, and tests end-to-end.
argument-hint: <resource kind and name, e.g. "mcp-tool searchEvents" or "event-type message.created" or "entity-kind thread">
---

Scaffold a new resource: $ARGUMENTS

## Pick the right path

There are four resource kinds in Mazarbul, each with a slightly different checklist:

1. **MCP tool** (new method on the MCP server surface).
2. **Event type** (new shape of fact written to the event log).
3. **Entity kind** (new class of identity, e.g. `thread`, `mailbox`, `repo`).
4. **Connector** (new ingestion source plugin slot).

Confirm which kind we're adding before starting.

## Process: MCP tool

1. **Schema first**. Define the JSON Schema for input and output under `internal/mcp/schemas/<toolName>.schema.json`. Follow `/aip-naming`.
2. **Add Go types**. `internal/mcp/types/<toolName>.go` with request + response structs and `json:` tags. Doc-comments on every exported field.
3. **Register the tool**. Add the handler to `internal/mcp/server.go` (or wherever the registry lives). Tool name lowerCamelCase, action verb.
4. **Implement the handler**. Should be ~50 LoC: parse, delegate to kernel, marshal response. No business logic in the handler.
5. **Error mapping**. Document which JSON-RPC error codes the tool can return. Include `data.cause` for structured causes.
6. **Tests**. Table-driven tests covering: happy path, every documented error code, edge case (empty input, oversized input). At least one test asserts the schema validates the request.
7. **Docs**. Add the tool to `docs/reference/mcp-tools.mdx`. Include example invocation and example response.

## Process: event type

1. **Schema first**. Define the event payload schema under `internal/kernel/schemas/events/<kind>.schema.json`.
2. **Add Go types**. `internal/kernel/events/<kind>.go` with the typed event struct. Implement the `Event` interface (or whatever the kernel uses).
3. **Provenance**. Confirm the event carries the required provenance fields (`source`, `confidence`, `time`). The kernel writer enforces this; double-check the type embeds the right base.
4. **Writer**. Add the kernel writer method if there's a domain-specific helper (e.g. `kernel.AppendMessageCreated`). Otherwise the generic writer is enough.
5. **Indexers**. If the event affects a derived index (graph nodes, vector embeddings, FTS), update the indexer worker so it picks up the new kind.
6. **Tests**. Round-trip test (encode -> decode -> assert equal). At least one indexer test if you added one.

## Process: entity kind

1. **Schema first**. Define the entity schema under `internal/kernel/schemas/entities/<kind>.schema.json`. Resource path `<kind>s/{kind}` (plural, lowercase).
2. **Add Go types**. `internal/kernel/entities/<kind>.go`.
3. **Identity discipline**. Decide the ID strategy: ULID externally, numeric internally? See `agent-docs/identity.md`.
4. **Migrations**. If the entity needs its own table, add the migration. SQLite migrations are forward-only; supersede via newer events.
5. **MCP surface**. Expose `Get<Kind>`, `List<Kind>s`, `Search<Kind>s` per AIP-131/132. Skip Create/Update/Delete if the entity is system-managed.
6. **Tests**. CRUD-shape tests via the MCP surface; the entity is observable only through MCP.

## Process: connector

1. **Manifest**. Create `internal/plugin/connectors/<source>/manifest.toml`. Declare slot, scopes, version.
2. **Implementation**. Add `internal/plugin/connectors/<source>/<source>.go` implementing the `Connector` interface.
3. **Provenance discipline**. Every event the connector writes goes through the kernel writer with `source.connector = "<source>"` set.
4. **Dedup strategy**. Document how the connector recognizes prior events and supersedes them.
5. **Scaffolding test**. Use `maz dev new-plugin --slot=connector` to confirm the manifest is parseable.
6. **Integration test**. Use a recorded fixture from the source format; assert the resulting events.
7. **Docs**. Add a "How to use the <source> connector" page under `docs/how-to/`.

## Always finish by

Running `mise run ci`. The hooks will block if anything's off.

## Gotchas

Concrete pitfalls encountered when using this skill. Add new ones as you hit them, this is the compounding-engineering log (see `agent-docs/lessons-learned.md` for the broader pattern).

- _(empty, add the first real entry the next time you hit a non-obvious pitfall)_
