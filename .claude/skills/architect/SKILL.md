---
name: architect
description: Design a feature from a business case outwards using an Agent Team. USE WHEN user says "architect this feature", "plan from a business case", "design X end-to-end", "should we build Y", "draft an ADR for Z", or has a vague request that needs framing before code. Spawns product-manager + system-architect + ux-designer + operator-advocate + devils-advocate, debates the design, drafts an ADR, presents to user for approval. NEVER ships code , it stops at the approval gate.
allowed-tools: Bash, Read, Skill
---

The "design before you build" skill. Use when a feature is requested but not yet framed , when there's a business case but no clean problem statement, when the technical structure is unclear, or when the implications cross multiple surfaces and need debate before code.

Produces an ADR (Architecture Decision Record) in `agent-docs/adr/`. The Lead presents the ADR to the user for explicit approval BEFORE any implementation begins.

## When to use this vs `/plan-feature`

| Situation | Skill |
|---|---|
| "Build feature X" with a clear spec | `/plan-feature` |
| "We need to handle Y better" , no clean spec yet | `/architect` (this skill) |
| "Should we build Z?" , open question | `/architect` (this skill) |
| Cross-service redesign | `/architect` first, then `/plan-feature` once ADR is approved |

## Phases

**Phase 1 , Frame**

Spawn `product-manager` as a single subagent (NOT a team , solo pass):

```
Spawn the product-manager subagent (.claude/agents/product-manager.md).
Prompt: "Help frame this feature: <user's request>. Ask the user 3-5 clarifying questions to nail down: who has the problem, what they're trying to do, why it hurts today, the proposed cut (minimum viable), and the success metric. Output: a one-paragraph problem statement and a list of open questions for the Design Council."
```

Pass the problem statement + open questions to Phase 2.

**Phase 2 , Design Council**

Spawn an **Agent Team** of 3–4 discussion personas. Default roster:

```
Spawn a 4-teammate Design Council:
- system-architect (archie): structure, dependencies, RTO/RPO, scaling
- ux-designer (uxie): user flow, ergonomics, state machine
- tui-developer (tui): CLI/TUI user lens , does this work for a human at the terminal and for an agent over MCP?
- devils-advocate (dee): challenge every assumption

Task: debate the design for: <problem statement from Phase 1>. Open questions: <list>.

Each teammate: write your initial position to the shared task list. Then read other teammates' positions and respond. Use the mailbox to challenge, refine, or concede. After 2-3 rounds, the Lead synthesizes the consensus (or names the dissent) into a single design recommendation.
```

Variations by domain:
- **Data-heavy** (schema design, ETL, audit): swap `ux-designer` for `data-engineer`.
- **Compliance-touching** (event-log writers, scope enforcement, kernel invariants): swap `devils-advocate` for `security-engineer`.
- **Release / distribution** (installer, release pipeline, sync transport): swap `ux-designer` + `tui-developer` for `backend-engineer` + `security-engineer`.

Lead runs in **Delegate Mode** (Shift+Tab) so it coordinates the debate without grabbing tasks.

**Phase 3 , ADR draft**

Spawn `system-architect` (with Write privileges) to draft the ADR:

```
Spawn system-architect (.claude/agents/system-architect.md) , full toolset.
Prompt: "Draft an ADR in agent-docs/adr/NNNN-<slug>.md based on the Phase 2 Design Council debate. Format:

# ADR-NNNN: <decision>

## Status
Proposed

## Context
1-2 paragraphs. Why this decision is being made.

## Decision
What we will do. One paragraph.

## Consequences
- Positive: ...
- Negative: ...
- Open questions: ...

## Alternatives considered
Each alternative: 1 paragraph + why rejected.

Use the conversation from the Design Council as raw material. Cite specific teammate positions where they disagreed."
```

The ADR is written to `agent-docs/adr/` but NOT committed.

**Phase 4 , Compliance review (optional)**

Triggers: ADR touches event-log writers, scope enforcement, or kernel invariants.

Spawn a 2-teammate compliance team:
- `security-engineer`: threat model + audit + PII + permission registration
- `sre-engineer`: RTO/RPO + observability + failure modes

Each posts review comments on the ADR. Lead consolidates into a "Compliance verdict" section appended to the ADR.

**Phase 5 , Approval gate**

Lead presents the ADR + Compliance verdict (if Phase 4 ran) to the user:

```
ADR drafted at agent-docs/adr/NNNN-<slug>.md.

Summary:
- Decision: <one line>
- Trade-offs accepted: <one line>
- Open questions: <bullet list, if any>

Do you approve this ADR? Options:
1. Approve as-is → proceed to /plan-feature for implementation
2. Revise → tell me what to change; the Design Council reconvenes
3. Reject → we go back to Phase 1 with new framing
```

**The Lead does NOT proceed to implementation without explicit user approval.** That's the entire point of this skill , kill bad designs cheaply before code.

## Cleanup

After approval, say "clean up the team". Lead shuts down teammates gracefully. If anything leaks, `make claude-clean` reaps.

## Gotchas

- **In-process teammates don't survive `/resume`.** Treat each `/architect` run as one shot , if you `/resume`, the Council is gone.
- **`devils-advocate` is the kingmaker**, not a procedural step. Their job is to surface the hidden cost. If their pushback lands, the design changes. Don't override them lightly.
- **The ADR is not committed by this skill.** That's `/plan-feature`'s job after approval , the ADR becomes part of the implementation PR.
- **Token cost**: ~3-4× a single session (Lead + 3-4 teammates). Worth it for getting the design right before code lands. NOT worth it for "should we rename this function" , that's a Slack thread, not a Council.
