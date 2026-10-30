---
name: complex-refactor
description: Safely execute a large refactor or migration via partitioned worktrees + Playwright healer loop. USE WHEN user says "refactor across", "migrate every X to Y", "sweep across packages", "rename X across N files", or when scope is ≥10 files or ≥2 services. Spawns refactor-archaeologist + system-architect + qa-engineer; produces a surface map + partition plan + per-module worktree workers. Healer loop with human gating. NEVER auto-applies test-failure fixes to product code.
allowed-tools: Bash, Read, Skill
---

The "big refactor without burning the house down" skill. For renames spanning N files, migrations across services, pattern sweeps that touch multiple modules , anything where a single Claude session would lose track of dependencies and produce a half-applied refactor.

## Phases

**Phase 1 , Survey**

Spawn `refactor-archaeologist` as a single subagent (NOT a team , solo pass):

```
Spawn refactor-archaeologist (.claude/agents/refactor-archaeologist.md).
Prompt: "Survey the surface for: $ARGUMENTS. Produce:
1. A file list , every file the refactor touches. Group by package / module / surface.
2. A dependency graph , which modules depend on which.
3. A seam analysis , where can we partition the work so independent worktrees advance without conflict?
4. A test-coverage map , what existing tests exercise the refactor scope? Where are the gaps?
5. A merge sequence , which seams must merge first because others depend on them?

Output: a refactor plan at .claude/plans/scratch/refactor-<slug>.md. List concrete file paths."
```

Pass the plan to Phase 2.

**Phase 2 , Council**

Spawn a 3-teammate Council to validate the partition:

```
Spawn a 3-teammate Refactor Council:
- system-architect: validate the partition respects service boundaries + contracts
- qa-engineer: validate the test-coverage map; identify pre-refactor tests we need to add to lock in current behavior
- devils-advocate: name the failure modes , what breaks first when this refactor goes wrong?

Lead presents Phase 1's refactor plan; Council debates via mailbox. After 1-2 rounds, Lead synthesizes either (a) refactor-plan APPROVED with the agreed partition, or (b) refactor-plan REVISED , go back to Phase 1.
```

**Phase 3 , Pre-refactor test net**

Triggers: Phase 2 surfaced test-coverage gaps.

Spawn `qa-engineer` to add tests covering current behavior BEFORE the refactor:

```
Spawn qa-engineer (.claude/agents/qa-engineer.md).
Prompt: "Add tests covering the current behavior of <refactor scope>. We're locking in semantics before we touch structure. Coverage targets: <Phase 2's gap list>. Commit these tests in a pre-refactor PR before any refactor code lands."
```

The pre-refactor test PR merges first , separately from the refactor. This is the safety net.

**Phase 4 , Worktree split (optional but encouraged for ≥10 files)**

For refactors touching many modules, spawn one Boris-pattern worktree per module:

```bash
# Lead executes this , not a teammate.
for SEAM in svc-a svc-b svc-c web-hooks; do
  git worktree add ../mazarbul-refactor-${SEAM} feat/refactor-${SEAM}
done
```

Each worktree gets its own Claude Code session (the user opens them in separate iTerm2 tabs / windows, or spawns Agent Team teammates with `--worktree` flag). Per-worktree teammate:

```
Spawn a teammate from @golang-service-developer (or @frontend-developer per module type).
Prompt: "You own worktree feat/refactor-<seam> rooted at ../mazarbul-refactor-<seam>.
Your task: apply the refactor pattern to all files within <module path>. Do NOT touch files outside this module.
After implementation, run:
  mise run ci
  go test ./<module>/...
Report green when done; report blocker if you find a non-obvious case."
```

Lead coordinates the merge sequence (which worktree merges first, based on Phase 2's dependency graph).

For smaller refactors (<10 files), skip Phase 4 , do the work in the current worktree.

**Phase 5 , Healer loop**

After each worktree's worker reports done, run the Playwright + Go healer loop:

```
Spawn qa-engineer (.claude/agents/qa-engineer.md).
Prompt: "Run the full test suite scoped to <module>:
- mise run ci
- go test ./<module>/... -race
- (if web touched) # (no frontend) && # (no frontend)
- (if E2E exists) pnpm exec playwright test web/e2e/<relevant>.spec.ts

For each failure:
1. Diagnose: is this (a) a real bug introduced by the refactor, (b) a test that needs updating because the API legitimately changed, (c) a flaky test, (d) a pre-existing issue exposed by the refactor?
2. Propose a fix as a diff with the diagnosis.
3. Human gate: report the diff to the Lead. DO NOT auto-apply.
4. After human approves and applies, re-run.

Max 5 iterations per module. If still red after 5, escalate with a structured failure report , never silently mark tests xfail or skipped."
```

The anti-Healer rule from TestDino applies: a "silent fix" can mask a real bug, especially for RBAC / permission tests. Every product-code change passes through human review.

**Phase 6 , Merge sequence**

Lead coordinates merging the worktrees per Phase 2's dependency graph. Example for a proto-touching refactor:

```
PR 1: proto changes (backward-compatible , additive only)
PR 2: backend changes (consume both old and new proto fields)
PR 3: frontend changes (use only new proto fields)
PR 4: cleanup (remove old proto fields once frontend is shipped)
```

Each PR is reviewable, revertible, and CI-passable independently. The Lead opens them in order; `/babysit-pr` watches each through CI before opening the next.

**Phase 7 , Cleanup**

After all worktrees merge:

```bash
# Lead executes , not a teammate.
git worktree list
git worktree remove ../mazarbul-refactor-svc-a
# ... etc per worktree
```

Then "clean up the team" to release any active teammates. If tmux orphans persist, `make claude-clean`.

## Gotchas

- **Phase 3 test net is non-negotiable for compliance surfaces** (event-log writers, scope enforcement, kernel invariants). Skip it elsewhere only if Phase 2 explicitly verified coverage is adequate.
- **Worktree workers must respect module boundaries**. If a worker touches files outside their assigned module, the partition is broken , abort and re-partition.
- **The healer loop never auto-applies fixes**. TestDino's silent-fix warning is the canonical reason; we follow it.
- **Merge order matters more than commit order**. Even if all worktree work is "done", the merge sequence determines whether CI stays green between PRs.
- **`/babysit-pr` is your friend** between merges. It watches CI and notifies on transitions; you don't have to babysit manually.
- **Token cost is high** , typically 5-10× a single session for a 10-file refactor (refactor-archaeologist + 3-person council + 3-5 worktree workers + qa-engineer healer). Justified because manual coordination at this scale produces partial refactors that rot for months.
