# Mazarbul synthesis

The four sections below distill the original design discussion into a single ready-to-reference doc. Read this before authoring or reviewing any new agent doc.

---

## 1. Core design commitments

**Identity and ergonomics**
- Project name: **Mazarbul** (Khuzdul for "records" — literally "the book of records" in Tolkien lore). Binary: **`maz`** (three letters, Occam).
- Slogan: *"Mazarbul, the system of records for the era of agents."* (Updated 2026-05-23 from "the Book of Records for the era of agents" to use the enterprise-architecture term of art. The Mazarbul name still does the poetic work.)
- Cloud / team service (future, optional): **Imladris** (Rivendell, House of Lore).
- Device pairing: `maz mellon <device>` (Tolkien, delight + uniqueness).
- Recovery: 20-word **Watchword** printed by the user, kept safe.
- **Theming rule** (hard): thematic only at emotional moments (first init, pairing, recovery, deletion, shared review). Plain everywhere mechanical (env vars `MAZ_API_KEY`, config keys `[providers.anthropic]`, error codes `E_AUTH_FAILED`, SDK method names `client.Remember()`).

**Storage (four layers, non-negotiable)**
- **Event log** (immutable, append-only, content-addressed): the source of truth. Wire format is **JSONL** (greppable, git-diffable, never proto).
- **Graph** (LadybugDB, embedded via CGO): entities + relations + provenance.
- **Vector index** (sqlite-vec, embedded): chunk dedup via content hash.
- **Full-text search** (FTS5).
- The SQLite/LadybugDB databases are **gitignored** local-only derivatives, rebuilt from the event log on `maz pull`.

**Plugin model (three categories, kept distinct)**
- **Providers**: implementations of kernel-required capabilities. One active per slot. Chat / embed / storage / vector / graph / full-text / sync / auth / parser.
- **Connectors**: bridges to external services. Many active simultaneously. Gmail / Slack / Notion / Linear / GitHub / Drive / Dropbox / OpenRegister / etc. Connectors are typically MCP servers.
- **Extensions**: everything else. Schema packs (`business-de`), enrichment sources (Crunchbase), custom parsers, custom commands.
- Install command stays plain: `maz install anthropic`, `maz install gmail`, `maz install crunchbase`. Manifest declares category.

**Sync (local-first, opt-in cloud)**
- Three scopes: **project** (in-repo, encrypted at rest), **personal** (private GitHub repo, syncs your devices), **team** (org GitHub repo, ACL = collaborator list).
- Sync is literal `git push/pull` on the append-only event log. CRDT semantics: no merge conflicts.
- Privacy enforced by scope-specific repos.

**Schema and ontology**
- **JSON Schema is the wire format for plugin-defined types** (language-independent, parseable in any kernel implementation). User-extensible types are authored in YAML and compiled to JSON Schema at install time.
- **Built-in types are Go structs** with `json:` tags inside the kernel; JSON Schema is generated from them via `invopop/jsonschema`.
- No Pydantic. No Protocol Buffers. No code-generation toolchain for plugin authors.

**Integration surface**
- **MCP server (primary surface)**: `maz mcp serve` exposes a broad tool surface including `remember`, `recall`, `forget`, `relate`, `traverse`, `get`, `neighbors`, `relations`, `entities`, `createEntity`, `defineEntityKind`, `definePredicate`, `getContext`, `reference`, `recordSourceRef`, `getSourceRef`, and more. Speaks JSON-RPC 2.0 over stdio + HTTP.
- **CLI**: cobra-based, TTY-detected TUI/CLI mode (see `tui-design.md`).
- **HTTP API + OpenAPI**: secondary surface for non-MCP clients and language-specific SDKs.
- **Git transport**: for sync.
- **No gRPC. No proto/buf.** Permanently dropped per the design conversation.

**Operating principles (the load-bearing ones)**
- *"Enrichment proposes, the user disposes."* Every enriched fact carries provenance + confidence + source path. Nothing auto-promotes without review.
- *"The lens, not the warehouse."* SoR indexes files where they live (Drive, Notion, local disk). Dedup by content hash. The user's existing organisation survives intact.
- *"Local-first by default."* `maz init` works offline with SQLite + local Ollama. Cloud is opt-in.
- *"Schema is data, not code."* Plugin authors write YAML / JSON Schema, never compiled types.
- *"Provenance is always there."* Every fact, embedding, entity links back to source + confidence + timestamp.
- *"Move tracking via content hash + history."* Never use paths as identity. Stable ULID + locations list with move history.
- *"First-hour value is make-or-break."* Bootstrap-ingestion (Claude.ai export, one codebase) must demonstrate value within the first run.

**Concrete decisions vs open questions**

Decided: single Go binary, JSON Schema (not Pydantic), three plugin categories, CRDT sync on JSONL event log, Docling default parser, four-layer storage, Watchword recovery, no provider prefix in install commands, MCP as universal protocol, budget gates on enrichment.

Resolved: default chat provider = none (the calling agent does the reasoning; Anthropic and Ollama are opt-in, see `agent-docs/designs/chat-provider.md`); default embed model = `snowflake-arctic-embed2`; default chat model = `qwen2.5:7b`; Watchword wordlist = BIP-39 English; MCP server library = `mark3labs/mcp-go`; graph backend = LadybugDB embedded via CGO through `github.com/LadybugDB/go-ladybug`.

---

## 2. Tooling and conventions

Beyond the standard Go toolchain (golangci-lint, lefthook, zap, table-driven tests with the race detector, Conventional Commits + release-please), Mazarbul adds:

- **Charm ecosystem** for TUI: `bubbletea`, `bubbles`, `lipgloss`, `huh`, `glamour`, `x/term`, `teatest`, VHS.
- **Swappable theme system** in `internal/tui/style/` with built-in themes `defender` (default, Range Rover Coniston-green), `parchment`, `mithril`, `nocolor`. User themes loadable from TOML.
- **TUI / CLI dual mode per command**, TTY-detected, with `--no-tui` and `--json` flags for accessibility + scripting.
- **Profiling setup**: pprof HTTP behind `--debug-addr` flag, `mise run bench`, `mise run pprof:cpu`, `mise run pprof:mem`, `agent-docs/profiling.md` runbook, `GOMEMLIMIT` env var pre-set.
- **Apache-2.0 license** (Copyright 2026 Moritz Steigerwald; `LICENSE` + `NOTICE`).
- **Mintlify-ready docs** (`docs/docs.json`) with Diataxis structure.
- **`maz rebuild`** (issue #112): CLI surface for `EventLog.Rebuild`. Replays the JSONL event log into the four derived stores (SQLite, LadybugDB, sqlite-vec, FTS5). Flags: `--strict` aborts on first `ApplyError`; `--yes` skips the destructive-action confirmation; `--progress-every N` overrides the progress cadence (default 100). Two output modes: plain text with kernel-formatted progress lines (CLI/TUI), and JSON Lines with `{"type":"progress",...}` ticks from a `countingStorage` adapter plus a `{"type":"summary",...}` at the end (--json). Exit codes: 0 clean, 1 apply errors or missing `--yes` in scripted mode, 2 rebuild-level fault, 130 `context.Canceled`. The confirmation gate prints to stderr so piping `maz rebuild | jq` never eats the prompt. **Online behaviour (branch `feat/daemon-routed-writes`):** when the daemon is running the CLI routes to `POST /admin/v1/rebuild` on the Unix socket instead of opening the store in-process. The daemon performs a shadow build in `<dataDir>/.rebuild-<ulid>/`, then quiesces, swaps the indexes, and resumes on the same socket; see `agent-docs/adr/0002-online-rebuild-reindex.md`. `maz reindex --reembed` follows the same path (decision R2: summaries live only in event payloads, so a full log replay is required for a correct vec rebuild). `maz reindex --dry-run` requires the in-process store and is therefore unavailable while the daemon runs.

---

## 3. AIP naming applied without proto

Google AIP (https://google.aip.dev) was designed for proto APIs. Mazarbul has **no proto**, but the naming conventions apply just as cleanly to JSON APIs, MCP tool schemas, OpenAPI specs, and Go internal types. Adopt them everywhere.

**Timestamps** (AIP-142, hard rule)
- JSON: `createTime`, `updateTime`, `deleteTime`. **Never** `createdAt`, `updatedAt`, `deletedAt`.
- Go: `CreateTime time.Time` with `json:"createTime"` tag.
- Strings on the wire are RFC 3339 UTC: `"2026-05-23T17:58:00Z"`.

**Resource and collection naming** (AIP-122, 148)
- Resources: PascalCase Go types, singular (`Event`, `Entity`, `Episode`).
- Collections in URLs and paths: lowercase plural (`events/`, `entities/`, `episodes/`).
- Resource names follow `<collection>/<id>` pattern: `events/01HNX...`, `entities/01HNY...`. Use ULIDs for IDs.
- JSON field names: camelCase (`displayName`, `parentName`, `createTime`).
- Go struct field names: PascalCase (`DisplayName`, `ParentName`, `CreateTime`) with explicit `json:` tags in camelCase.

**Standard fields**
- `name` (the resource name in `<collection>/<id>` form). Required, identifier.
- `parent` (parent resource name when listing a sub-collection). Required for hierarchical resources.
- `displayName` (human-readable name). Required for most resources.
- `description` (optional human-readable description).
- `createTime`, `updateTime`, `deleteTime` (output-only, server-set, RFC 3339 UTC).
- `uid` (server-assigned UUID4 or ULID, output-only, immutable). Use this for stable internal references; `name` is what humans and APIs see.

**CRUD verb naming** (AIP-131 to 135)
- `CreateEvent(CreateEventRequest) -> Event`
- `GetEvent(GetEventRequest) -> Event`
- `ListEvents(ListEventsRequest) -> ListEventsResponse`
- `UpdateEvent(UpdateEventRequest) -> Event`
- `DeleteEvent(DeleteEventRequest) -> Empty`
- `BatchGetEvents`, `SearchEvents` when needed.
- Request messages: `<Verb><Resource>Request`. Response: the resource itself (not wrapped) or `<Verb><Resource>Response` for List/Batch.

**Pagination** (AIP-158)
- Request: `pageSize int32`, `pageToken string`.
- Response: `nextPageToken string`. Empty string means no more pages.

**Filtering** (AIP-160)
- Request: `filter string` (CEL-style expression: `createTime > "2026-01-01T00:00:00Z" AND priority = "high"`).
- Request: `orderBy string` for sort spec (`"createTime desc, displayName"`).

**Field behavior annotations** (in JSON Schema `description` fields, since we have no proto)
- For each field, the schema description should explicitly say one of: `Required`, `Output-only`, `Input-only`, `Immutable`, `Optional`. Same semantics as the proto annotations, expressed in prose.

**Error responses** (AIP-193 style)
- HTTP status code + error message + structured `errorInfo` containing `reason`, `domain`, `metadata`.

**Empty fields**
- Omit empty optional fields from JSON (`omitempty` tag). Required fields always present, even if empty.

This is the canonical naming for everything Mazarbul exposes: MCP tool input/output schemas, HTTP API endpoints, OpenAPI spec, Go struct definitions, plugin manifest fields.

---

## 4. Open questions

These are decisions from the design conversation. Resolved items are noted inline; remaining open items need Moritz's input before the relevant work begins.

- **Default chat provider.** **Resolved (revised 2026-10-30):** none. The agent calling Mazarbul over MCP is the brain; Mazarbul is the memory. Anthropic (cloud) and Ollama (local) are opt-in for agent-less work. Default Ollama chat model when opted in: `qwen2.5:7b`; default embed model: `snowflake-arctic-embed2`.
- **MCP server library choice.** **Resolved:** `mark3labs/mcp-go`. In use since initial implementation.
- **LadybugDB embedding mode.** **Resolved 2026-05-24: CGO** via the official `github.com/LadybugDB/go-ladybug` bindings. Single binary, no subprocess to manage; the CGO toolchain is already required for sqlite-vec, so the build cost is shared. See `agent-docs/tech/ladybug.md` for per-platform build requirements.
- **Watchword wordlist source.** **Resolved:** BIP-39 English (2048 words, well-known, unambiguous 4-letter prefixes).
- **Hosted Imladris service.** v0.1 roadmap: do we ship a hosted Imladris (operations, billing, SLAs) or strictly recommend self-hosted git transport via private GitHub / GitLab / Gitea?
- **Default profiling exposure.** Always-on pprof in dev, behind `--debug-addr` flag in production, or never auto-exposed? Plan currently uses behind-a-flag everywhere, which is safest.
- **Defender accent shade tuning.** Current `defender` theme uses `#2c4a3a` (light) / `#8fa37e` (dark) — Coniston/Pangea Green family. If a specific Defender colourway is preferred (classic British Racing Green `#004225`, Tasman Blue `#2d3f4a`, etc.), adjust `internal/tui/style/themes/defender.go` post-scaffold; the theme abstraction makes this a one-file change.
