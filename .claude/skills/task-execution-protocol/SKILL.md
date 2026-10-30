---
name: task-execution-protocol
description: Phased implementation protocol for non-trivial Mazarbul work. USE WHEN starting a feature that crosses several surfaces (kernel + CLI + MCP + TUI), before opening any non-bugfix PR, or when the user describes a multi-surface change. Phases: Route, Contracts, Boundaries, Implement, Validate, Document. NOT FOR typo fixes, single-file bug fixes, or pure refactors.
---

# Task Execution Protocol

The systematic approach for any non-trivial change. Follow the phases in order.

---

## Phase 0: Route

Identify the task type and read the matching docs before writing code.

| Task type | Read first |
|-----------|------------|
| Kernel / storage / event log | `agent-docs/synthesis.md`, `agent-docs/designs/storage.md`, `agent-docs/designs/event-log.md` |
| New MCP tool | `agent-docs/tech/mcp-go.md`, `agent-docs/designs/mcp-server.md`, `aip-naming` skill |
| New CLI command | `agent-docs/tech/cobra.md`, `agent-docs/lifecycle-theming.md` |
| TUI screen | `agent-docs/tui-design.md`, `tui-design` skill |
| Ingestion / connector | `agent-docs/ingestion.md`, `agent-docs/plugins.md` |
| Sync / team scope | `agent-docs/sync.md`, `agent-docs/team.md`, `agent-docs/privacy.md` |
| Bug fix | The package involved, plus `agent-docs/lessons-learned.md` |

Also read at least one existing file in every package you will modify, to match established patterns.

---

## Phase 1: Contracts

Map interfaces and dependencies before writing a line:

- Which Go interfaces in `internal/` will you implement or extend? Prefer existing ones over new ones.
- Which event kinds will you append? New kinds need a JSON Schema under `schemas/` and must be replayable by `maz rebuild`.
- Which existing helpers cover what you need? Search before writing a new one; no new `util`/`common`/`helpers` packages.
- Run `mise run test` on `main` first to establish a baseline, and note the tests covering adjacent behaviour.

---

## Phase 2: Boundaries

**Event log** (source of truth):
- Append-only. Never rewrite or delete events; supersede them.
- Every derived store (SQLite, LadybugDB, sqlite-vec, FTS5) must be rebuildable from the log. If your change adds derived state, verify `maz rebuild` reproduces it.

**Provenance**:
- Every new fact, entity, or relation carries source, confidence, and timestamp.

**Privacy and scopes**:
- Does the change read or write across scopes (personal, draft, team)? Check `agent-docs/privacy.md`.
- Never log secrets, tokens, or user content at info level. Ingested text goes through the scrubber.

**Public surfaces** (CLI flags, MCP tool schemas, config keys, env vars, error codes):
- Treat as API. Follow AIP naming. Breaking changes need an explicit decision from the user.

**External calls** (LLM providers, remote connectors):
- Respect budget gates, handle offline/degraded mode, and never block the hot path on the network.

---

## Phase 3: Implement

1. Types and JSON Schema (`schemas/`, Go structs with `json:` tags, AIP field names).
2. Kernel / storage changes in `internal/kernel/`, with table-driven tests.
3. Surface wiring: MCP tool (`internal/mcp/tools/`), CLI command (`internal/cli/`), TUI (`internal/tui/`).
4. Hot paths: add or update a `*_bench_test.go` and record the baseline in the PR.

---

## Phase 4: Validate

```bash
mise run fmt
mise run lint
mise run test
```

Do not open a PR until all three are green. For TUI changes also run `mise run tui:snap` and review the snapshot diff.

---

## Phase 5: Document

- User-facing change? Update the matching `agent-docs/*.md` and append to `agent-docs/mintlify-backlog.md` (AGENTS.md rule 11).
- New dependency? Add `agent-docs/tech/<lib>.md`.
- Architectural decision? Add an ADR under `agent-docs/adr/`.
- PR description follows `agent-docs/pr-description-template.md`; commits follow Conventional Commits.

## Gotchas

Concrete pitfalls encountered when using this skill. Add new ones as you hit them.

- _(empty, add the first real entry the next time you hit a non-obvious pitfall)_
