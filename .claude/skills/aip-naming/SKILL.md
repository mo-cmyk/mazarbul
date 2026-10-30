---
name: aip-naming
description: Google AIP naming compliance applied to Mazarbul's JSON APIs, MCP tool schemas, and Go internal types. USE WHEN editing JSON Schema files, MCP tool definitions, Go domain types with JSON tags, OpenAPI specs, or any code that exposes external names (resource paths, field names, method verbs). Covers AIPs 122/123/131-135/140/142/155/158/160/192/193/203 applied without proto.
paths:
  - "**/*.schema.json"
  - "internal/kernel/schemas/**"
  - "internal/mcp/**"
  - "docs/reference/**"
---

# Google AIP Naming Compliance (no proto)

Mazarbul does not use proto. The wire format is JSONL. The schema layer is JSON Schema. Types are Go structs with `json:` tags. AIP naming conventions still apply to the JSON, MCP, and Go surface, this skill captures the mapping.

Reference: `agent-docs/synthesis.md` section 3 ("AIP naming applied without proto") for worked examples.

## Workflow (mandatory)

1. Define or update the JSON Schema in `internal/kernel/schemas/` (or where the kernel maintains schemas).
2. Define or update the Go struct in the matching package with `json:` tags.
3. Run `go vet ./...` plus `mise run lint`.
4. Tests cover happy path plus at least one documented error code.

## Required AIPs

Reference: <https://google.aip.dev>

| AIP | Purpose | Mazarbul application |
|---|---|---|
| AIP-122 | Resource references | Foreign-key field stores the canonical resource name (`name: "entities/{entity}"`). |
| AIP-123 | Resource definitions | Every resource has a singular noun, plural collection, and stable name pattern. |
| AIP-131 | Get methods | `Get<Resource>` request shape: `{ name: string }`. |
| AIP-132 | List methods | `List<Resource>` request: `{ parent, pageSize, pageToken, filter, orderBy }`. Response: `{ <resources>: [], nextPageToken: string }`. |
| AIP-133 | Create methods | `Create<Resource>` request: `{ parent, <resourceId?>, <resource> }`. |
| AIP-134 | Update methods | `Update<Resource>` request: `{ <resource>, updateMask }`. |
| AIP-135 | Delete methods | `Delete<Resource>` request: `{ name }`. Soft-delete via supersede. |
| AIP-140 | Field names | Booleans as adjectives without `is`/`has`/`can` (`active`, not `isActive`). |
| AIP-142 | Time fields | `*Time` suffix: `createTime`, `updateTime`, `deleteTime`. Never `*_at`. |
| AIP-155 | Idempotency | `requestId` UUID4 on mutating operations; 24h dedup. |
| AIP-158 | Pagination | Cursor-based via `pageToken`. No offset pagination. |
| AIP-160 | Filtering | CEL-style filter strings. |
| AIP-192 | Documentation | JSON Schema `description` on every property; Go-doc on every exported field. |
| AIP-193 | Error model | JSON-RPC error code + structured `data.cause` for MCP; HTTP status + RFC 7807 problem JSON for the HTTP API. |
| AIP-203 | Field behavior | Express as JSON Schema constraints (`required`, `readOnly`, `writeOnly`). |

## Documentation conventions (AIP-192 spirit)

Every JSON Schema property carries `description`. Every exported Go field carries a Go-doc comment. Imperative mood.

JSON Schema:
```json
{
  "type": "object",
  "description": "Entity is the canonical record of a person, place, or thing in Mazarbul.",
  "properties": {
    "name": {
      "type": "string",
      "description": "Resource name, e.g. entities/{entity}.",
      "pattern": "^entities/[A-Z0-9]{26}$"
    },
    "createTime": {
      "type": "string",
      "format": "date-time",
      "description": "Time the entity was first observed. Set by server."
    }
  },
  "required": ["name", "createTime"]
}
```

Go:
```go
// Entity is the canonical record of a person, place, or thing in Mazarbul.
type Entity struct {
    // Name is the resource name, e.g. entities/{entity}.
    Name string `json:"name"`
    // CreateTime is the time the entity was first observed. Set by server.
    CreateTime time.Time `json:"createTime"`
}
```

## Naming conventions

- **Go**: PascalCase. Single-word acronyms (`Jwk`, `Oidc`, `Mfa`), not `JWK`/`OIDC`/`MFA`. Exception: brand names (`OAuth`).
- **JSON**: camelCase. Same single-word-acronym rule (`jwk`, `oidc`, `mfa`).
- **Resource paths**: plural lowercase nouns. `entities/{entity}`, `events/{event}`, `connectors/{connector}`. Placeholder is the singular noun, not the field name.
- **MCP tool names**: verbs in lowerCamelCase. `searchEvents`, `getEntity`, `listConnectors`.
- **Boolean fields**: adjectives without prefixes. `active` not `isActive`. Exception for reserved words.
- **Enum values in Go**: PascalCase typed constants (`EventKindUnspecified`, `EventKindCreated`). In JSON: SCREAMING_SNAKE_CASE strings (`"UNSPECIFIED"`, `"CREATED"`).
- **Time fields**: `*Time` suffix. `createTime`, `updateTime`, `deleteTime`, `expireTime`. Never `*_at`.
- **Configuration toggles**: `enable<X>` for inputs, `<x>Enabled` for read-only outputs.

## Standard request and response shapes

- **Pagination**:
  ```json
  { "pageSize": 50, "pageToken": "abc", "filter": "createTime > 2026-01-01" }
  ```
  Response:
  ```json
  { "events": [...], "nextPageToken": "def" }
  ```
- **Idempotency**:
  ```json
  { "requestId": "550e8400-e29b-41d4-a716-446655440000", ... }
  ```
- **Field mask** (updates):
  ```json
  { "entity": { ... }, "updateMask": "displayName,labels.kind" }
  ```
- **Filter syntax** (CEL):
  ```
  createTime > "2026-01-01T00:00:00Z" AND source.connector = "gmail"
  ```

## When adding a new resource

Use the `/new-resource` skill, it's the end-to-end checklist (schema -> kernel writer -> MCP tool -> tests -> docs).

## Gotchas

- **No proto, but proto-equivalent discipline applies.** Pre-1.0 we can rename and renumber freely; once external SDKs ship we lock to AIP-180 evolution rules.
- **`updateMask` must be a comma-separated string of field paths**, not an array. AIP-161 specifies dot-notation paths (`"profile.displayName,labels.kind"`).
- **Time fields are always UTC**. Go: `time.Time` with `time.Now().UTC()`. JSON: RFC 3339 with `Z` suffix.
- **Resource names are case-sensitive**. `entities/01H...` is different from `Entities/01H...`. Lowercase the collection, ULID the ID.
- **MCP tool errors use JSON-RPC codes** (-32600, -32601, -32602, -32603) plus a `data` field with structured cause. Mapping to AIP error semantics: `INVALID_ARGUMENT` -> -32602, `NOT_FOUND` -> -32602 with `data.cause = "not_found"`, `INTERNAL` -> -32603.
- **HTTP API uses RFC 7807** (`application/problem+json`). The HTTP server maps internal errors to status codes per AIP-193 mapping.
- **ULIDs everywhere externally**. Internally the storage layer may use numeric IDs for joins, but external names always use ULIDs.
