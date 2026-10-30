# Monorepo tour

One-paragraph tour of every package under `cmd/` and `internal/`. M1+ packages are marked as stubs. Expanded sections live in the package READMEs as code lands.

## `cmd/`

### `cmd/maz/main.go`

Binary entry point. Calls `internal/cli.Execute()` and exits with that function's return code. Holds the ldflags-injected `version` value and seeds it into `internal/version`. Tiny on purpose; logic lives in `internal/cli`.

## `internal/`

### `internal/cli/`

Cobra command tree. `root.go` declares the root command and global flags (`--version`, `--debug-addr`, `--data-dir`, `--no-tui`, `--json`, `--theme`). One file per top-level subcommand: `init.go`, `version.go`, `debug.go`, and (as features land) `remember.go`, `recall.go`, `review.go`, `install.go`, `mcp.go`, `sync.go`, etc. Every command supports both TUI and CLI modes; mode is decided in `PersistentPreRun` via `internal/tui.DetectMode`.

### `internal/tui/`

Charm primitives package. `mode.go` does TTY detection. `style/` is the single source of truth for visual choices: themes (defender, parchment, mithril, nocolor), the `Theme` interface, the theme loader for user-authored TOML. `spinner/`, `progress/`, `panel/`, `wizard/` wrap `bubbles` and `huh` with project defaults. `testdata/` holds `teatest` snapshots. **No file outside `internal/tui/style/` may reference raw hex colors.**

### `internal/kernel/` (stub)

Event log, query composer, index dispatcher. The kernel is the only package that writes to the event log; everything else reads through it. Stub in M0; full implementation lands in M1.

### `internal/plugin/` (stub)

Manifest loader + slot registry for providers, connectors, extensions. Stub in M0; full implementation in M1.

### `internal/mcp/` (stub)

MCP server surface. JSON-RPC 2.0 over stdio + HTTP. Tool definitions for `remember`, `recall`, `forget`, `relate`, `traverse`, `review`. Stub in M0; library choice deferred to M1 planning (see `agent-docs/tech/mcp-go.md`).

### `internal/config/`

12-factor config: YAML loader with env-var overrides. Slim struct (no DB, no AWS); grows as plugins land.

### `internal/logging/`

Zap logger setup with context injection. `NewLogger(Config)`, `WithLogger(ctx, *zap.Logger)`, `FromContext(ctx) *zap.Logger`. JSON to stderr by default; optional OTel sink. See `agent-docs/tech/zap.md` for the usage rules.

### `internal/correlation/`

Request-ID context helper. `WithRequestID(ctx, id)`, `RequestID(ctx) string`. Used by MCP request handling, ingestion pipelines, and event-log provenance.

### `internal/observability/` (stub)

Tracing + metrics scaffolding. OTel-ready, wired only when `OTEL_*` env vars are set.

### `internal/version/`

Build-time version variables injected via ldflags. `var Version = "dev"` populated by `mise run build`.

### `internal/testutil/`

Test fixtures, builders, context helpers. `TestContext(t *testing.T) context.Context` returns a context with a test logger and a fixed system ID. Add factories here for shared event/entity test data.
