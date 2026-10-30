---
name: plan-feature
description: Full-stack Mazarbul feature orchestration. USE WHEN implementing a feature that spans proto + backend + frontend, when user says 'add feature X', 'implement Y end-to-end', or 'build a complete Z'. Orchestrates research → optional Design Council → proto → parallel implementation → Playwright QA verification. Auto-spawns Agent Teams when scope warrants it. NOT FOR single-surface changes (use go-backend/frontend-react directly).
argument-hint: <feature description>
---

Implement full-stack feature: $ARGUMENTS

This skill is **phase-aware**. Each phase has a trigger; not every phase fires on every feature. For small additive work (e.g. add a field to an existing resource), most phases skip and the work flows quickly. For substantive work (≥150 LoC, ambiguous design, cross-service boundaries), the Design Council + Architecture Review phases prevent expensive rework downstream.

## Phase map

| Phase | Trigger | Output |
|---|---|---|
| 0. Business case | User mentions "business case" / scope is ambiguous / >300 LoC estimate | One-pager problem statement |
| 1. Research | Always | Codebase situational awareness |
| 1.5. Design Council | Phase 0 ran OR scope ≥150 LoC OR design has ambiguous tradeoffs | Synthesized design doc with recommendation |
| 2. Architecture review | Phase 1.5 changes touch infra OR cross-service boundaries | ADR draft in `agent-docs/adr/` |
| 3. Proto design | Proto changes needed (sequential , gate for Phase 4) | Proto definitions +  artifacts |
| 4. Implementation Squad | Surfaces ≥2 of {proto, backend, frontend} AND scope ≥50 LoC | Parallel implementation via Agent Team |
| 5. QA verification | Phase 4 touched web/ | E2E test in `web/e2e/`, healer loop, ≤5 iterations |
| 6. Verify | Always | `mise run ci && # (no frontend) && # (no frontend) && # (no frontend)` green |

## Phase 0 , Business case (optional)

Triggers: user said "business case", or scope is ambiguous, or estimate ≥300 LoC.

Spawn the `product-manager` subagent (NOT as a team , single pass) to write a one-paragraph problem statement:

```
Spawn the product-manager subagent (.claude/agents/product-manager.md).
Prompt: "Write a one-paragraph problem statement for $ARGUMENTS. Include:
- Who has the problem (which user / role)?
- What are they trying to do?
- Why does it hurt today?
- What's the proposed cut (minimum viable)?
- Success metric we can point at in 90 days?"
```

If the answer reveals the feature isn't worth building, stop here. Better to spike that finding than to build through it.

## Phase 1 , Research (always)

Use the Explore subagent to map:
- Relevant proto definitions (do we extend an existing message or add a new service?)
- Existing backend services and patterns to reuse (`internal/repository/base.go` helpers, `internal/api/v1/errors.go`, pagination, fieldmask)
- Existing frontend components (`web/src/components/`, the resource-config catalog in `web/src/lib/resources/`)
- What already exists vs what's needed

## Phase 1.5 , Design Council (optional , but high-value when triggered)

Triggers: Phase 0 ran, OR scope estimated ≥150 LoC, OR design has ambiguous tradeoffs (e.g. "should this be a workflow node or a service method?", "operator-chosen or system-derived?").

Spawn an **Agent Team** of 3–4 discussion personas:

```
Spawn a 4-teammate Design Council:
- product-manager (lenny): anchor business value + scope cut
- system-architect (archie): anchor technical structure + RTO/RPO concerns
- tui-developer (tui): anchor the CLI/TUI user's perspective for any UX/workflow touch
- devils-advocate (dee): challenge every assumption

Question: $ARGUMENTS

Have them debate via mailbox. Synthesize the conversation into a single recommendation: 1 paragraph context, 1 paragraph decision, 3 bullets on consequences (positive/negative/open questions). Present to user before proceeding.
```

If the user touches event-log writers, scope enforcement, or kernel invariants, swap `devils-advocate` for `security-engineer`. If it's UI-heavy, swap for `ux-designer` + `accessibility-advocate`.

The Lead should run in **Delegate Mode** (Shift+Tab) so it doesn't grab implementation tasks during the council discussion.

## Phase 2 , Architecture review (optional)

Triggers: Phase 1.5 changes touch infrastructure OR cross-service boundaries OR new MCP tool.

Spawn a 3-teammate ADR team:
- `system-architect` (with Write/Edit privileges): drafts the ADR
- `sre-engineer`: checks RTO/RPO + observability + failure modes
- `security-engineer`: checks threat model + permission boundaries + audit log

Output: a draft ADR in `agent-docs/adr/NNNN-slug.md`. The Lead presents the ADR to the user for explicit approval before any implementation begins.

## Phase 3 , Proto design (sequential)

If proto changes are needed, this phase is **sequential** because Phase 4 depends on the generated artifacts.

Use `@protocol-schema-architect` (existing surface specialist):
- Design the proto schema following Google AIP conventions
- Run `` ( + )
- Verify generated artifacts in `gen/` are clean
- Run ``

Phase 4 cannot start until Phase 3 completes.

## Phase 4 , Implementation Squad (parallel)

Triggers: surfaces ≥2 of {proto, backend, frontend} AND scope ≥50 LoC.

Spawn an **Agent Team** of surface specialists, pre-assigned (NOT self-claim) because file boundaries are clean:

```
Spawn a 3-teammate Implementation Squad:
- @golang-service-developer (go): owns internal/, cmd/, migrations/
- @frontend-developer (fe): owns web/
- @quality-assurance-engineer (qa): owns *_test.go + web/e2e/

Task partition:
- go: implement Phase 3's MCP tool + repository + register permissions in internal/auth/permission.go + add migration if needed
- fe: implement resource config in web/src/lib/resources/ + query factory + UI components + pages
- qa: write integration tests (go) + Playwright E2E tests (web/e2e/)

File partition is by directory , no conflicts. Lead in Delegate Mode (Shift+Tab) coordinates.
```

For small (<50 LoC) work, skip the team and run sequentially:
- `@golang-service-developer` → backend
-  → types
- `@frontend-developer` → frontend

## Phase 5 , QA verification (optional, but recommended for any web/ change)

Triggers: Phase 4 touched `web/`.

Spawn the `qa-engineer` subagent (or teammate, if Phase 4 was a team):

```
Spawn qa-engineer with Playwright MCP access (.claude/agents/qa-engineer.md).
Prompt: "Write an E2E test in web/e2e/ for $ARGUMENTS covering the happy path + 1 error case. If failures occur, run the healer loop: diagnose → propose fix → human approval gate → re-run (max 5 iterations). Do NOT auto-apply fixes to product code. Report a diff for human review."
```

Healer loop pattern: TestDino's anti-Healer warning applies , qa-engineer never silently "fixes" a failure that might be a real bug (especially for RBAC/permission tests). Every product-code change goes through the Lead for human approval.

## Phase 6 , Verify (always)

Lead runs the full verification matrix:

```bash
mise run ci        # format + lint + test
mise run test         # all tests with race detector
# (no frontend currently) && # (no frontend)
```

If anything fails, fix and re-run. Don't claim done with red checks.

## Gotchas

- **Phase 3 gates Phase 4.** Proto must compile + generate cleanly before backend or frontend teammates can use the generated types. Don't parallelize Phase 3.
- **In-process teammates don't survive `/resume`.** If you `/resume` a session that had a Phase-4 team, the teammates are gone. Re-spawn fresh.
- **Surface partition prevents conflicts** in Phase 4. If the partition can't be cleanly Go-vs-TS-vs-proto (e.g. a shared utility file), use `/team --worktree` for per-teammate worktree isolation (Boris pattern).
- **kernel invariants touches**: any work on event-log writers, scope enforcement, or kernel invariants auto-triggers Phase 2 (Architecture review with security-engineer + sre-engineer). Don't shortcut.
- **`@quality-assurance-engineer` ≠ `qa-engineer`**. The former is the existing project-specific QA subagent; the latter is the new Playwright-driven E2E persona. Phase 4 uses the former for integration tests, Phase 5 uses the latter for E2E.
