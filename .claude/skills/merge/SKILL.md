---
name: merge
description: Merges the current PR after the babysit loop reaches merge_ready. USE ONLY WHEN user explicitly says 'merge it' / 'merge this PR' / 'go ahead and merge' AFTER reviewing the PR. Refuses unless babysit-state.phase == merge_ready. Confirms once, then runs gh pr merge --squash (no --admin). Schedules post-merge deploy watch. NOT FOR auto-merge from any other skill or hook.
allowed-tools: Bash, Read, Write, Skill, CronCreate, PushNotification
disable-model-invocation: true
argument-hint: [PR-number]
---

The ONLY skill in this project that runs `gh pr merge`. Hard-gated: requires babysit state to be at `merge_ready` AND explicit user confirmation in the most recent user message.

## Steps

**1. Resolve PR + verify state**

```bash
PR="${ARGUMENTS:-$(gh pr view --json number -q .number)}"
STATE=".claude/babysit-state-${PR}.json"

if [ ! -f "$STATE" ]; then
  echo "REFUSED: no babysit state for PR #$PR. /merge requires a prior /babysit-pr run that reached merge_ready."
  exit 1
fi

PHASE=$(jq -r '.phase' "$STATE")
if [ "$PHASE" != "merge_ready" ]; then
  echo "REFUSED: PR #$PR is in phase '$PHASE', not merge_ready."
  echo "Run /babysit-tick manually to refresh state, or wait for the next Cron tick."
  exit 1
fi
```

**2. Confirm destructive action**

Pass the last user message to the guard:

```bash
.claude/scripts/guard-destructive.sh merge "$LAST_USER_MESSAGE"
```

(In Claude, that means: read your most recent user turn. Pass its text as arg 2. If `guard-destructive.sh` returns non-zero, abort and print its stderr message.)

**3. Merge**

```bash
gh pr merge $PR --squash --delete-branch
```

- **Never `--admin`** , that bypasses branch protection.
- **Never `--rebase`** , squash is the project default.
- `--delete-branch` is fine; the babysit state still has the audit log.

**4. Schedule post-merge deploy watch**

Capture the merged commit SHA:

```bash
MERGED_SHA=$(gh pr view $PR --json mergeCommit --jq .mergeCommit.oid)
```

Update state:

```bash
source .claude/scripts/babysit-state.sh "$PR"
set_field merged_sha "$MERGED_SHA"
set_phase post_merge_watch
reset_iter post_merge_iter
log_audit "merged" "sha=$MERGED_SHA"
```

Schedule a fresh Cron (the previous one was deleted at `merge_ready`):

Invoke `CronCreate` with:
- `expression`: `*/4 * * * *`
- `durable`: `true`
- `prompt`: `Run /babysit-tick for PR #$PR`
- `description`: `babysit deploy for PR #$PR`

The new job's ID is a different value from the pre-merge `cron_job_id`
(already deleted). Persist it under a distinct key so `/babysit-abort`
and the `post_merge_watch` exit branch can find it:

```bash
# After CronCreate returns the new job id:
set_field post_merge_cron_job_id "$NEW_CRON_ID"
log_audit "post_merge_cron_scheduled" "id=$NEW_CRON_ID"
```

**5. Notify**

PushNotification (proactive): "PR #$PR merged. Watching deploy actions. Notifications on success/failure."

**6. Report**

Print:
- Merge SHA
- Deploy watch state file
- `/babysit-abort $PR` to stop the deploy watch early

---

## Gotchas

- Merge is irreversible (squash discards branch history). Refuse and ask if uncertain.
- The post-merge Cron is a *different* job from the pre-merge one. Persist its ID under `post_merge_cron_job_id`; the `post_merge_watch` branch in `/babysit-tick` reads that key first and falls back to `cron_job_id` only for legacy state files.
- If branch protection requires extra approvals or status checks, `gh pr merge` will fail. Read the error; don't bypass with `--admin`.
