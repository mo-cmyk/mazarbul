---
name: team
description: Spawn an Agent Team , multiple Claude Code instances coordinated by this Lead session, sharing a task list, messaging each other directly. USE WHEN user says "spawn a team", "get a team on this", "multi-agent on this", "design discussion", "second opinion", "competing hypotheses", "multiple theories", or "explore X with multiple perspectives". The generic entry point , picks a roster + phase automatically from natural language. NOT FOR feature implementation (use /plan-feature), architecture from business case (use /architect), or large refactors (use /complex-refactor).
allowed-tools: Bash, Read, Skill
---

The natural-language entry point to Claude Code's experimental Agent Teams feature. The team you spawn here is a peer team , teammates share a task list, claim work, message each other, debate. Distinct from subagents (which only report back to the Lead).

## When to use teams vs subagents vs a single session

| Situation | Use |
|---|---|
| Single focused task, result returns to you | subagent (`Agent` tool) |
| Multiple workers, results return to you | several parallel subagents |
| Multiple workers who need to **debate**, **challenge each other**, **converge through dialogue** | **Agent Team** (this skill) |
| Cross-layer feature spanning proto + backend + frontend simultaneously | **Agent Team** + /plan-feature |
| Routine implementation, sequential | single session |

Token cost scales linearly with teammate count (each is a full Claude Code instance). 3–4 teammates is the sweet spot per [Anthropic's docs](https://code.claude.com/docs/en/agent-teams).

## Steps

**Phase 1 , Pick the phase**

Identify what kind of team this is from the user's prompt:

| Phase | Indicators in prompt | Default roster |
|---|---|---|
| **Design** | "design", "second opinion", "explore", "what should we build", "tradeoffs" | 3 from `product-manager`, `system-architect`, `ux-designer`, `ui-designer`, `operator-advocate`, `data-engineer`, `devils-advocate` |
| **Review** | "review", "audit", "multi-angle", "security check", "perf check" | 3 lens reviewers: `security-engineer`, `qa-engineer`, `backend-engineer` (or `ui-designer` if web-heavy) |
| **Implement** | "build X", "implement Y", "parallel implementation" | 2–4 surface specialists: `@golang-service-developer`, `@frontend-developer`, `@protocol-schema-architect`, `@quality-assurance-engineer` |
| **Debug** | "competing hypotheses", "multiple theories", "why is X failing", "we've been stuck" | 3–5 adversarial investigators , spawn from `backend-engineer`, `sre-engineer`, `data-engineer`, `security-engineer`, `devils-advocate`. Prompt each: "try to disprove each other's theories, like a scientific debate" |

If the prompt is ambiguous, ask the user once: "Sounds like a [design/review/implement/debug] team , confirm?"

**Phase 2 , Pick roster size + size**

- **3 teammates**: default. Balances debate with manageable coordination.
- **4 teammates**: when a clearly distinct fourth perspective is needed (e.g. add `devils-advocate` to a 3-person design discussion).
- **5 teammates**: only for adversarial debugging with genuinely competing hypotheses.

Never spawn 1 teammate (use a subagent). Never spawn 6+ (coordination overhead dominates).

**Phase 3 , Spawn**

Tell the team-spawn prompt clearly:
- Each teammate's name (so we can reference them in later prompts)
- Each teammate's role (the persona file name from `.claude/agents/`)
- The shared task or question
- Any task-specific context the teammate won't have from `CLAUDE.md`

Example spawn prompt:
> "Spawn a 4-teammate design team for adding new event-kind categories:
> - `lenny` from product-manager: anchor the business case + scope cut
> - `archie` from system-architect: anchor the technical structure + ADR
> - `tui` from tui-developer: anchor the terminal user's perspective
> - `dee` from devils-advocate: challenge every assumption
>
> Question: should resolution categories be operator-chosen, system-derived, or hybrid? Synthesize a recommendation."

Claude Code's Agent Teams machinery handles the rest: tmux-CC split panes (auto-detected from iTerm2 via the project's `teammateMode: "tmux"`), shared task list, mailbox.

**Phase 4 , Coordinate**

- Use **Shift+Down** to cycle through teammates and message any directly.
- Use **Ctrl+T** to toggle the shared task list.
- Use **Shift+Tab** (Delegate Mode) to restrict the Lead to coordination-only tools , recommended for teams of 4+.
- Wait for teammates to finish (per docs: "If the lead starts implementing tasks itself, tell it 'wait for teammates'").

**Phase 5 , Clean up**

When done, say "clean up the team". The Lead checks for active teammates, asks them to shut down gracefully, then tears down the shared resources. If anything leaks, run `make claude-clean` from a fresh shell.

## File partitioning to avoid conflicts

The docs warn: two teammates editing the same file overwrite each other. Default partitions:

- **By surface**: kernel → `internal/kernel/**`; MCP → `internal/mcp/**`; CLI → `internal/cli/**`, `cmd/**`; TUI → `internal/tui/**`; tests → `*_test.go`.
- **By module**: when refactoring, one teammate per module (use `/complex-refactor --worktree` instead , that's the right skill).
- **Read-only**: discussion personas (product-manager, devils-advocate, ux-designer, etc.) don't have Edit/Write , partition naturally.

If a partition isn't clean, use `--worktree` to give each teammate their own worktree (Boris pattern). For most teams, surface-based partition is enough.

## Gotchas

- **In-process mode resumption**: `/resume` and `/rewind` do NOT restore in-process teammates. If you resume a session that had a team, the team is gone , re-spawn from scratch.
- **One team per session**: a Lead can manage only one team at a time. Clean up before starting a new one.
- **No nested teams**: teammates cannot spawn their own teams. Only the Lead can.
- **Lead model is fixed**: the session that created the team is the Lead for its lifetime. To change Lead model, abort and restart.
- **Auto-cleanup is informational only**: the `TeammateIdle` hook (`.claude/hooks/teammate-idle-cleanup.sh`) reports state but never tears down , that's user-driven.
