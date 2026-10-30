---
name: babysit-pr
description: Cron-driven autonomous loop that babysits a PR through CI + Greptile + reviewer feedback until merge-ready, then notifies. USE WHEN user says 'babysit this PR' / 'watch the PR until ready' / 'autonomous wrap', or chained from /wrap-up after /ship. Schedules 240s Cron ticks, invokes /review-fix on CI failures and /greptile on bot comments. NEVER merges. Stops on merge-ready + PushNotification. NOT FOR small diffs you can manually monitor (just /ship) or already-merged PRs.
allowed-tools: Bash, Read, Write, Skill, CronCreate, PushNotification
argument-hint: [PR-number]
---

Schedule an autonomous Cron loop that watches a PR through every phase from `ci_pending` to `merge_ready`. The loop NEVER merges. It only watches, fixes CI failures, triages Greptile, and notifies on key transitions.

This skill is invokable by the LLM (it's chained from `/wrap-up` via the Skill tool, and matches direct user phrasing like "babysit this PR"). The collision check in step 2 prevents accidental double-starts. Destructive ops still gated: this skill never calls `gh pr merge`.

## Steps

**1. Resolve PR number**

```bash
PR="${ARGUMENTS:-$(gh pr view --json number -q .number 2>/dev/null)}"
```

If empty, exit with an instructive error: "No PR detected on current branch. Open one with /ship first."

**2. Check for collision**

```bash
STATE=".claude/babysit-state-${PR}.json"
if [ -f "$STATE" ]; then
  echo "Babysit loop already active for PR #$PR. Run /babysit-abort to stop it first."
  exit 1
fi
```

**3. Initialize state**

```bash
source .claude/scripts/babysit-state.sh "$PR"
state_init
log_audit "init" "started babysit for PR #$PR"
```

**4. Schedule the Cron loop**

Use the `CronCreate` deferred tool with these parameters:

- `expression`: `*/4 * * * *` (every 4 minutes , stays inside the 5-min prompt cache TTL)
- `durable`: `true` (intent: persist to `.claude/scheduled_tasks.json` so the loop survives session restarts; **currently silently ignored by the harness** , CronCreate returns "Session-only" regardless. Pass it anyway so the loop becomes truly durable the day that bug is fixed.)
- `prompt`: `/babysit-tick $PR` (literal slash command , bypasses `disable-model-invocation` via slash-command path; the per-tick worker reads state and branches on phase)
- `description`: `babysit PR #$PR`

After `CronCreate` returns, capture the cron job ID into state:

```bash
set_field cron_job_id "<id-returned-by-CronCreate>"
```

**5. Notify (pr_opened)**

```bash
if [ "$(.claude/scripts/babysit-notify.sh $PR pr_opened)" = "send" ]; then
  # Invoke PushNotification deferred tool with:
  #   status: "proactive"
  #   message: "PR #$PR opened , watching CI + Greptile (240s ticks)"
fi
```

**6. Hand off**

Print to user:
- "Babysit loop scheduled for PR #$PR. Tick every 240s, no auto-merge."
- "State: `.claude/babysit-state-$PR.json`"
- "Stop with `/babysit-abort`."
- "Type `merge it` once you're notified `merge_ready`."
- "Session-bound: the cron lives inside this Claude session. Closing the terminal kills it. Re-run `/babysit-pr $PR` next session , the state file picks up from the current phase."

Exit. The Cron continues firing `/babysit-tick` prompts into THIS session every 4 min.

---

## Gotchas

- **Session-bound, not truly autonomous.** The harness's `CronCreate` injects the `/babysit-tick` prompt back into the active Claude session every 4 min , it does NOT spawn a `claude -p` subprocess. So the loop only advances while THIS terminal is alive and Claude is the foreground process. `durable: true` is silently ignored (no entry in `.claude/scheduled_tasks.json`); the cron lives entirely in session memory.
- **State file is the cross-session bridge.** `.claude/babysit-state-<PR>.json` persists on disk. Closing the terminal stops the loop, but re-running `/babysit-pr <PR>` next session detects the existing state file (collision check) , `/babysit-abort <PR>` first, then `/babysit-pr <PR>` again to rearm, and the worker picks up from the persisted phase.
- **For true cross-session autonomy**, fall back to launchd (macOS) or a GitHub Actions workflow that reacts to PR state changes server-side. Don't pretend this skill provides that.
- The 240s interval is calibrated to the 5-min prompt cache TTL. Don't bump higher than 270s , cache misses cascade.
- This skill writes state but does NOT do any work itself. The actual loop is in `/babysit-tick`.
