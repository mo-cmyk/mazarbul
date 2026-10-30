---
name: go-strict
description: Auto-iterating quality loop , verify → critique → dead → simplify → re-verify until clean. USE WHEN you implemented a non-trivial feature (>50 LoC), when Stop hook reason says 'audit needed' or 'invoke /go-strict', when user types /go-strict, or when /go composite isn't strict enough. Loops max 5 iterations, each tightens code further. The hook is fully autonomous , invokes itself if Stop detects unfinished work. NOT for tiny fixes (use /verify) or quick ships (use /go).
model: claude-opus-4-7
effort: high
allowed-tools: Bash(make *), Bash(go *), Bash(golangci-lint *), Bash(buf *), Bash(/* govulncheck disabled for now */ *), Bash(deadcode *), Bash(pnpm *), Bash(git diff *), Bash(git status), Bash(git log *), Read, Edit, Write, Glob, Grep
hooks:
  Stop:
    - type: prompt
      model: claude-haiku-4-5
      prompt: "Evaluate whether the /go-strict loop has converged. Read .claude/last-check.md.\n\nReturn ok=true ONLY when BOTH Failures and Warnings sections are empty (Cleanup-opportunities may remain , those are warnings the user can choose to address).\n\nAlso check .claude/go-strict-state.json for iteration counter. If iteration >= 5, return ok:true with reason 'Max iterations reached (5/5). Surfacing remaining issues to user , see .claude/last-check.md.'\n\nIf not converged AND iteration < 5, return ok:false with reason 'Iteration N/5: still M Failures, K Warnings. Continue the loop: run /verify, address what failed, then /critique, then /dead, then re-run.'\n\nInput: $ARGUMENTS"
---

# /go-strict , Auto-iterating quality loop (the autonomy centerpiece)

This is what `/go` becomes when the diff is substantive (>50 LoC). It loops automatically , you don't drive it.

The skill declares its own `Stop` hook so the loop self-terminates when `.claude/last-check.md` shows no Failures + no Warnings.

## When to invoke

- Stop hook's prompt-eval said "invoke /go-strict to audit your N LoC of new code"
- You implemented a feature (>50 LoC) and need to lock down quality before shipping
- User typed `/go-strict` or "lock this down"
- Compound `/go` isn't strict enough , you want the auto-iteration

**Do not invoke for:**
- Tiny fixes (<30 LoC) , use `/verify` + `/ship`
- Quick refactors with no new behavior , use `/go`
- Doc-only changes , use `/verify` for the typecheck only

## The loop

Initialize iteration state:

```bash
# Write iteration counter
mkdir -p .claude
echo '{"iteration": 0, "started_at": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'"}' > .claude/go-strict-state.json
```

Then loop (max 5 iterations):

### Iteration N , sequence

**Step 1: `/verify`** , detect failing lint/tests on changed surfaces. Fix any failures before proceeding.

**Step 2: `/critique`** , forked Explore subagent reviews own diff across 4 dimensions (correctness, simplicity, Mazarbul-specifics, test coverage). Read the punch list; address MUST FIX items.

**Step 3: `/dead`** , run reachability + knip. Apply obvious dead-code removals.

**Step 4: `/simplify`** , 4-dimension complexity audit (reuse, dead code, premature abstractions, oversize). Apply obvious wins.

**Step 5: Re-run `/verify`** , confirm changes from steps 2-4 didn't break anything.

**Step 6: Increment iteration counter**, check Stop hook for convergence.

```bash
ITER=$(python3 -c "import json; d=json.load(open('.claude/go-strict-state.json')); d['iteration']+=1; json.dump(d, open('.claude/go-strict-state.json','w'))")
```

The skill-scoped Stop hook will:
- Return `ok: true` when `.claude/last-check.md` has empty Failures + Warnings → exit loop
- Return `ok: true` when iteration >= 5 → exit loop with remaining issues
- Return `ok: false` otherwise → loop continues

## Exit conditions

| Condition | Result |
|---|---|
| Failures + Warnings empty | ✓ Converged , surface to user, suggest `/preflight` + `/ship` |
| Iteration cap (5) hit | ✗ Stuck , list remaining issues, ask user for guidance |
| Critical compliance issue (event-log invariants, scope boundaries, encryption) | STOP loop, surface immediately to user , do not auto-fix |

## What this skill deliberately does NOT do

- Apply judgment-call simplifications without surfacing (e.g., merging two near-duplicate functions that diverge on one branch)
- Modify generated code (`gen/`, `web/src/gen/`)
- Touch audit log code, migration history, or `internal/auth/permission.go` automatically (compliance , needs human eyes)
- Force-push or rewrite git history
- Open a PR (that's `/ship` , separate, deliberate)
- Run more than 5 iterations (hard cap to prevent infinite loops)

## Pairs well with

- Stop hook prompt-eval , invokes `/go-strict` when diff is substantive
- `/preflight` , call after `/go-strict` converges, before `/ship`
- `/ship` , opens PR once preflight is green
- `/loop "every 30m, /go-strict on this branch"` , overnight quality maintenance

## Gotchas

- **The skill-scoped Stop hook overrides the project Stop hook for this skill's lifecycle.** Once the loop converges, control returns to the project Stop hook for normal end-of-task evaluation.
- **Iteration state lives in `.claude/go-strict-state.json`** , if you abort mid-loop, delete this file before re-running, or the iteration counter persists.
- **`/critique` runs in a forked context** , it doesn't see this conversation. Pass diff info explicitly in the invocation prompt.
- **Don't apply ALL findings blindly.** `/simplify` and `/critique` surface judgment calls. The loop applies obvious wins (>3 LoC each, no behavior change). For borderline calls, the skill should pause and ask the user.
- **Compliance-protected files (`internal/auth/permission.go`, `migrations/*.sql`, anything touching `audit_event` or `event_log`) are auto-flagged but never auto-modified.** The loop will report "needs human review" and continue with other items.
- **If iteration count hits 5 with persistent issues, the loop exits and surfaces "stuck" state.** This usually means there's a design issue, not a quality issue , escalate to user.

## How autonomy works end-to-end (the user's actual ask)

1. You implement a 120-LoC feature.
2. You declare done. Stop hook deterministic check runs, writes `.claude/last-check.md`.
3. Stop hook prompt-eval reads `last-check.md` + your "I'm done" message. Returns `ok: false, reason: "Substantive diff (120 LoC) without cleanup pass. Run /go-strict to auto-iterate verify/critique/dead/simplify until clean."`
4. You read the reason in your context (injected by hooks docs spec). You invoke `/go-strict`.
5. `/go-strict` loops automatically. After iteration 3, `.claude/last-check.md` is clean.
6. The skill-scoped Stop hook returns `ok: true`. Loop exits.
7. You declare done again. Project Stop hook re-evaluates. This time prompt-eval sees clean state, returns `ok: true`. You actually stop.

**The user typed one prompt.** Everything else was hooks → reasons → skill invocations → fixes → re-evals.
