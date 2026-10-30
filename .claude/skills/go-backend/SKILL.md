---
name: go-backend
description: Mazarbul Go backend rules. USE WHEN editing any *.go file, working with internal/, cmd/, migrations/, .mise.toml, or SQL. Covers Google Go style, Google AIP naming applied to JSON and Go, event-log append-only discipline, kernel helper catalog, Zap structured logging, and the no-proto / JSONL convention. NOT FOR TUI-specific work (use tui-design) or CLI argument parsing (use cli-developer agent).
paths:
  - "**/*.go"
  - "migrations/**"
  - ".mise.toml"
  - "**/*.sql"
---

# Go Backend Rules

## Core invariants (NON-NEGOTIABLE)

- **Event log is append-only.** Never UPDATE or DELETE event rows. Supersede via newer events.
- **Provenance on every fact.** Every event carries source, confidence, timestamp.
- **Timestamps**: UTC `time.Time`, JSON RFC 3339. Never `*_at`.
- **No proto.** Wire format is JSONL. Schema layer is JSON Schema. Types are Go structs with `json:` tags.
- If unsure about a kernel invariant, **ASK USER**.

## Schema and types

1. Define or update the JSON Schema in `internal/kernel/schemas/`.
2. Define or update the Go struct with `json:` tags. Doc-comment on every exported field.
3. Run `go vet ./...` plus `mise run lint`.
4. Tests cover happy path plus at least one documented error code.

See `/aip-naming` for the full naming rules.

## Naming Standards

- **Packages**: lowercase, single word, no underscores (`kernel`, `mcp`, `plugin`, `cli`, `tui`).
- **Variables**: camelCase, descriptive (`entityID` not `eid`, `eventCount` not `cnt`).
- **Exported functions**: PascalCase, verb-noun (`AppendEvent`, `ValidateSchema`).
- **Interfaces**: `-er` suffix when single method (`Reader`, `Writer`, `Validator`).
- **Acronyms in Go (Mazarbul convention)**: single words in PascalCase (`Jwk` not `JWK`, `Mcp` not `MCP`, `Url` not `URL`, `Http` not `HTTP`). Brand names preserved (`OAuth`, `JSONRPC`).
- **Enum values in Go**: PascalCase typed constants (`EventKindUnspecified`, `EventKindCreated`).
- **Boolean fields** (AIP-140): adjectives, `active` not `isActive`. Exception: reserved words.
- **Time fields** (AIP-142): `*Time` suffix, `createTime`, `updateTime`, `deleteTime`.

## Code Standards (Google Go Style)

- Context first parameter: `func Foo(ctx context.Context, ...)`
- Return errors, don't log-and-return (let caller decide).
- Error wrapping: `fmt.Errorf("operation: %w", err)`.
- No stuttering: `entity.ByID()` not `entity.GetEntityByID()`.
- No generic packages: `util`, `common`, `helpers`, **FORBIDDEN**.
- No global state: dependency injection only.
- No panic in library code: return errors.
- Receiver names: short, consistent (`s` for `Server`, not `server` or `self`).
- Line length: prefer <100 characters, hard limit 120.

## Log Message Conventions

Per [Google Go Style](https://google.github.io/styleguide/go/decisions#log-messages):

| Type | Casing | Example |
|------|--------|---------|
| **Log messages** | Capitalized | `logger.Info(ctx, "Starting server")` |
| **Error strings** | lowercase | `fmt.Errorf("failed to start: %w", err)` |

Proper nouns and acronyms (OpenTelemetry, MCP, HTTP, JSON-RPC) keep standard casing in log messages.

## Code Reuse, ALWAYS check helpers first

Before adding new helpers, check:

- `internal/kernel/` for event-log writers, query composers, supersede helpers.
- `internal/mcp/` for JSON-RPC error mapping, tool registry helpers.
- `internal/tui/style/` for theme accessors.

Full catalog with usage examples lives in the `references/` subfolder.

## When to Use What, Observability

| Need | Tool | When |
|---|---|---|
| Structured log | Zap | Every method (already configured) |
| Event-log write | `kernel.Append(ctx, event)` | Every state-changing user or system action |
| Custom span | OTEL `tracer.Start` | External APIs, complex multi-step logic |
| Custom metric | OTEL `meter.Int64Counter` | Quotas, rate-limited APIs |

Full patterns live in the `references/` subfolder.

## AIP Compliance, TL;DR

| AIP | What |
|---|---|
| 122/123 | Resource references + definitions |
| 131-135 | Get/List/Create/Update/Delete |
| 140 | Boolean field naming (adjectives) |
| 142 | Time fields (`*Time` suffix) |
| 155 | Idempotency (`requestId` UUID4) |
| 158 | Pagination (cursor-based) |
| 160 | Filtering (CEL syntax) |
| 192 | Documentation (comments everywhere) |
| 193 | Error model (JSON-RPC or RFC 7807) |
| 203 | Field behavior (`required`/`readOnly`/`writeOnly`) |

Full mapping with worked examples: `agent-docs/synthesis.md` section 3 and the `aip-naming` skill.

## Pull Request Standards

- Target: 50 to 100 LoC for incremental changes.
- Initial development exception: larger PRs OK for new packages.
- Hard limit: 300 LoC (excluding generated code, migrations).
- Review SLA: <1 hour for small PRs, max 1 business day.

## Forbidden Actions

- Emojis in code or comments.
- Logging passwords, tokens, API keys, encryption keys.
- Hardcoded credentials or connection strings.
- Skipping error handling with `_` assignment.
- Guessing kernel invariants (ask user instead).
- Modifying event-log entries (append-only).
- Creating types that bypass JSON Schema (define the schema first).
- Non-idiomatic Go patterns.
- Creating `docs/` directories or documentation without being asked.
- Em-dashes in code, comments, docs, or commits.

## When Uncertain, ASK USER

- Kernel invariant interpretations.
- Domain model definitions (entities, relationships).
- Business logic behavior.
- Provenance requirements for a new event type.
- Schema design decisions.

## Key Commands

| Command | What |
|---|---|
| `go vet ./...` | Compile-time checks. |
| `mise run test` | All tests with race detector. |
| `mise run lint` | golangci-lint. |
| `mise run ci` | Format + lint + test (before every commit). |
| `mise run fmt` | gofumpt + goimports with local prefix. |
| `lefthook install` | Install git hooks. |

## README.md Maintenance

Update README.md when changing: command surface, project structure, dev workflow, or prerequisites. Keep it concise (~100 lines).

## Gotchas

Concrete pitfalls encountered when implementing Go backend code in Mazarbul. Add new ones as you hit them, this is the compounding-engineering log (see `agent-docs/lessons-learned.md` for the broader pattern).

- **Never embed unbounded inside a tool's `Invoke`.** Any synchronous embedding call on the
  request path must either (a) bound itself with the `internal/ontology/dedup` pattern, a hard
  ceiling (`defaultSemanticTimeout`, 20s, in `dedup.go`) plus a single `EmbedBatch` round-trip
  instead of N per-text calls, or (b) defer the embed to a background worker the way `remember`
  and `recall` do (enqueue, return immediately, drain off the request path). A deadline-less
  embed makes the tool look hung when Ollama is cold or busy: this was the real cause of the
  `defineEntityKind` "multi-second hang" (fixed in PR #286). When you add a new tool that needs a
  vector, copy the dedup ceiling-plus-batch shape; do not call the embedder in a bare loop.

