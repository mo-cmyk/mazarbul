# Mazarbul, Agent Context

Mazarbul is an **agent system of record**, the local-first system of records for the era of agents. Single Go binary (`maz`), embedded SQLite + LadybugDB + sqlite-vec + FTS5, JSONL event log as the source of truth, MCP as the primary integration surface. *"Mazarbul, the system of records for the era of agents."* Status: pre-alpha. Module: `github.com/mo-cmyk/mazarbul`.

(Mazarbul is Khuzdul for "records", literally "the book of records" in Tolkien's lore. The slogan uses the more familiar enterprise-architecture phrase "system of records" so non-Tolkien-aware readers grok the product immediately; the project name does the poetic work.)

## Critical rules (always apply)

1. **No proto.** Wire format is JSONL; schema is JSON Schema; types are Go structs with `json:` tags. No `buf.yaml`, no Connect-RPC, no `gen/` directory.
2. **AIP-compliant naming.** `createTime` not `createdAt`. Resource paths plural lowercase. CRUD verbs `Create/Get/List/Update/Delete`. See `agent-docs/synthesis.md` section 3 for the full mapping.
3. **Event log append-only.** Never UPDATE/DELETE event rows; only append + supersede. The event log is the source of truth; SQLite/LadybugDB/vector indexes are gitignored derivatives rebuilt on `maz rebuild` (M4 adds `maz pull` for cloud sync).
4. **Provenance on every fact.** Every entity, relation, and inference carries source, confidence, and timestamp. *"Enrichment proposes, the user disposes."*
5. **Local-first.** SQLite + LadybugDB + sqlite-vec embedded in-process. Cloud sync (Imladris) is opt-in.
6. **Theming rules.** Thematic at emotional moments only (init, `mellon`, Watchword, deletion, shared review). Plain in env vars (`MAZ_API_KEY`), config keys (`[providers.anthropic]`), error codes (`E_AUTH_FAILED`), SDK methods (`client.Remember()`). See `agent-docs/lifecycle-theming.md`.
7. **Timestamps:** UTC `time.Time`, JSON RFC 3339, never `*_at` suffixes.
8. **No em-dashes.** Anywhere. Code, comments, docs, commits, PR descriptions.
9. **Greptile, zero unresolved before merge.** Greptile reviews every PR with inline P0/P1/P2 findings that are not surfaced in the top-level review status; checking only that Greptile reviewed is insufficient. Before merging, enumerate every inline thread via `./.claude/scripts/greptile-resolve-threads.sh list <PR>`, classify each (real bug, valid nit, false positive), fix or justify each, reply on the thread, then resolve it. Merge only when `./.claude/scripts/greptile-resolve-threads.sh verify <PR>` prints `VERIFY OK`. The `/greptile` and `/babysit-pr` skills automate this; when working without them, do the same steps by hand. See `agent-docs/greptile-workflow.md` for the full discipline and the bootstrap incident that produced this rule.
10. **Hot-path PRs ship with a benchmark.** When a change touches a hot path (event log writer, four-layer storage dispatcher, MCP request handler, recall composer, embedding call, anything else flagged by the issue's `## Hot path?` field), the PR includes or updates a benchmark and records the baseline (`ns/op`, `B/op`, `allocs/op`) in the PR description. Bench files live next to the code under test (`*_bench_test.go`) and use `b.ReportAllocs()`. Profile before optimising; measure, don't guess. See `agent-docs/profiling.md` for `mise run bench`, `mise run pprof:cpu`, `mise run pprof:mem`. Regressions surface in PR review; gates land in M2+.
11. **User-facing changes update docs in the same PR.** A user-facing surface is a CLI command, an MCP tool, a TUI screen, a config knob, an env var, an error code, or anything else a human or external agent observes. When a PR ships one, it updates the corresponding `agent-docs/*.md` file and appends a backlog entry to `agent-docs/mintlify-backlog.md` (intended path, status, last-update date) so Mintlify content is ready when the public docs site lights up. Internal-only refactors and tests skip both; the rule applies when behaviour visible from outside `internal/` changes.

## How to think (Karpathy 4 principles)

These four principles, derived from Andrej Karpathy's Jan 2026 observations on agentic-coding failures, apply to every change:

1. **Think Before Coding**. State your assumptions explicitly. If a request is ambiguous, present multiple interpretations instead of silently picking one. If you're confused, name what's unclear and ask. Push back when a simpler approach exists.
2. **Simplicity First**. Write the minimum code that solves the problem. Nothing speculative. No abstractions for single-use code. No error handling for impossible scenarios. If 200 lines could be 50, rewrite it.
3. **Surgical Changes**. Touch only what you must. Don't "improve" adjacent code. Match existing style. Every changed line must trace directly to the request.
4. **Goal-Driven Execution**. Convert vague instructions into verifiable success criteria before starting. "Fix the bug" becomes "write a failing test that reproduces it, then make it pass". Loop until verified.

## How to brief Claude

For non-trivial work, write a crisp brief in the first turn. Three things:

- **Goal**: what success looks like in plain language
- **Constraints**: non-goals, things not to touch, perf/API contracts, compliance rules
- **Acceptance criteria**: how you'll verify the work is done right

Treat Claude as an engineer you're **delegating to**, not a pair programmer you guide line-by-line. Crisp briefs lead to fewer interruptions and higher-quality output. When Claude asks too many clarifying questions, the brief was incomplete.

## Prompting patterns that work

- **Challenge the result**: "Grill me on these changes, don't open a PR until I pass your test."
- **Demand evidence**: "Prove this works. Diff behavior between main and feature branch."
- **Reset toward elegance**: "Knowing what you know now, scrap this and implement the elegant solution."
- **Spec, don't hint**: detailed specs reduce ambiguity, better output.

## How to start a task

1. Read this file (already done).
2. Invoke the matching skill:
   - Go work on the kernel or any `internal/` package: `go-backend` skill
   - API surface, MCP tool definition, JSON Schema, or new resource type: `aip-naming` skill
   - TUI work (Bubble Tea, Charm components): `tui-design` skill
   - Non-trivial work: `task-execution-protocol` skill
3. For unknown territory, read the matching `agent-docs/*.md` from the table below.
4. Plans for non-trivial features live in `.claude/plans/` (gitignored, local scratch). Use them to align with the user before coding; the durable artifact is the implementation, the commit message, and the PR description, not the plan file.

## Quick orientation

| What you need | File |
|---|---|
| Design synthesis (read first) | `agent-docs/synthesis.md` |
| Kernel + four-layer storage | `agent-docs/designs/storage.md`, `agent-docs/designs/event-log.md` |
| Provider / connector / extension model | `agent-docs/plugins.md` |
| Bootstrap-ingestion pipeline | `agent-docs/ingestion.md` |
| Canonical entities and resolution | `agent-docs/identity.md` |
| Scopes, mellon, Watchword | `agent-docs/privacy.md` |
| Team scope, roles, device keys, genesis pin | `agent-docs/team.md` |
| Theming rules (decided) | `agent-docs/lifecycle-theming.md` |
| Worktree workflow | `agent-docs/worktree-workflow.md` |
| Local dev runbook | `agent-docs/devstack.md` |
| pprof, bench, memory limit | `agent-docs/profiling.md` |
| TUI design system + themes | `agent-docs/tui-design.md` |
| PR description template | `agent-docs/pr-description-template.md` |
| Design doc template | `agent-docs/design-doc-template.md` |
| Lessons learned | `agent-docs/lessons-learned.md` |
| Repository tour | `agent-docs/monorepo-tour.md` |
| Lint policy, secret scanning | `agent-docs/quality-security.md` |
| Library: Zap | `agent-docs/tech/zap.md` |
| Library: Cobra | `agent-docs/tech/cobra.md` |
| Library: mcp-go | `agent-docs/tech/mcp-go.md` |
| Library: sqlite-vec | `agent-docs/tech/sqlite-vec.md` |
| Library: LadybugDB | `agent-docs/tech/ladybug.md` |

## Context management

- **Rewind, don't correct.** When Claude goes down a wrong path, don't type "that didn't work, try X instead" (keeps failed attempt in context). Use `/rewind` (or double-tap Esc) to drop the failed attempt entirely, then re-prompt with what you learned.
- **`/compact` vs `/clear`.** `/compact focus on X, drop Y` for lossy LLM summary keeping momentum. `/clear` then hand-write a brief for genuinely new tasks.
- **Auto-compact at 400k.** Context rot begins at 300-400k tokens on the 1M model. `CLAUDE_CODE_AUTO_COMPACT_WINDOW=400000` is set in `.claude/settings.json` to force early compaction while the model is still sharp.

## Model and effort

- **Default to Opus 4.7 with `xhigh` effort** (Boris's recommendation). Adaptive thinking, one-shots bigger tasks.
- **`/effort max`** for hard debugging, architecture decisions, tricky code (session-only; doesn't persist).
- **Verbal steering** if not changing effort: "Think carefully and step-by-step" for hard problems; "Respond quickly" for simple ones.
- **4.7 vs 4.6 shifts**: calibrated response length (specify if you want long/short), less automatic tool use (provide explicit tool guidance when needed), more judicious subagent spawning (explicitly request parallel agents for cross-file work).

## Specialized subagents

Delegate focused work to these. Each has its own isolated context window.

- `@golang-developer`: Go kernel and `internal/` package authoring; Google Go Style Guide enforcement.
- `@cli-developer`: cobra command tree; TTY-aware mode switching between CLI and TUI; scripted/agent-callable surface with `--json`.
- `@tui-developer`: Bubble Tea / Charm specialist; owns `internal/tui/`; browses `bubbles` and `huh` before reinventing.
- `@mcp-developer`: MCP server surface; agent-accessible tool definitions, idiomatic JSON-RPC errors, clean response shapes.
- `@plugin-developer`: provider/connector/extension plugin system; clean install UX, declarative TOML manifests.
- `@ontology-architect`: JSON Schema design, AIP compliance, ULID + external IDs, type hierarchy.
- `@ingestion-engineer`: bootstrap-ingestion pipelines; tiered extraction, budget gates, dry-run + resume.
- `@docs-writer`: `agent-docs/` and Diataxis upkeep; Glamour-rendered help text in TUI.
- `@backend-engineer`: fresh-eyes Go review; production-grade discipline distinct from `@golang-developer`.
- `@system-architect`: structure, ADRs, scaling, RTO/RPO; Write/Edit access for ADR drafting.
- `@security-engineer`: OWASP-flavored threat modeling; capability tokens, encryption at rest, audit log.
- `@qa-engineer`: test suite design and coverage; race detector, table-driven, teatest snapshots.
- `@devils-advocate`: adversarial reviewer; surfaces hidden cost, complexity, second-order effects.
- `@refactor-archaeologist`: large-refactor specialist; surface partitioning, worktree coordination.
- `@product-manager`: business case, scope, prioritization; UX defense from the user's seat.

## Self-healing

The lessons system is **self-healing**.

- **`/lessons` skill** appends one-paragraph entries to `agent-docs/lessons-learned.md` whenever a gotcha, non-obvious decision, workaround, or corrected assumption surfaces during a session. Newest at the top. The user does not invoke it manually; it fires from natural-language correction signals routed by `.claude/hooks/route-prompt.sh`.
- **`.claude/hooks/lessons-rotate.sh`** runs periodically on session Stop. When `agent-docs/lessons-learned.md` exceeds 500 lines, the oldest entries (by `Last seen` date and `Recurrence` count) move to `agent-docs/lessons-archive.md`. Entries tagged `Compliance: true` never auto-archive.
- **`.claude/hooks/post-compact-reinject.sh`** runs after `/compact` or auto-compact. It re-injects this file's critical rules and the active task's context into the next-turn message so post-compaction sessions don't lose the load-bearing constraints.
- **Audit trail.** Every lessons-rotate action and self-promotion writes one line to `agent-docs/lessons-changelog.md` plus a git commit.

You do nothing. Read `agent-docs/lessons-changelog.md` to see what self-healed when curious. Git history is the deeper safety net: `git log -p agent-docs/lessons-learned.md` shows every change with full context.

## License and commit footer

- **License**: Apache-2.0 (Copyright 2026 Moritz Steigerwald). See `LICENSE` for the full text and `NOTICE` for attribution.
- **Commit footer**: `.claude/settings.json` sets `"includeCoAuthoredBy": false`. Do not add the `Co-Authored-By: Claude` footer automatically. If a commit was authored by Claude in collaboration, the human author owns the commit on the wire.
