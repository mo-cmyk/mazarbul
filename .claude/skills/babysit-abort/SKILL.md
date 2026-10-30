---
name: babysit-abort
description: Stop an active babysit Cron loop for a PR. USE WHEN user says 'stop the babysit', 'cancel the loop', 'abort PR #N watch', or wants to halt an in-flight /babysit-pr without merging. Deletes the Cron job, sets phase=aborted, notifies. NOT FOR pausing temporarily (no resume , would need /babysit-pr again).
allowed-tools: Bash, Read, Write, CronDelete
argument-hint: [PR-number]
---

Stop the babysit loop for a PR. Cron is deleted; state is marked `aborted`; loop will not resume.

## Steps

**1. Resolve PR**

```bash
PR="${ARGUMENTS:-$(gh pr view --json number -q .number 2>/dev/null)}"
STATE=".claude/babysit-state-${PR}.json"
[ -f "$STATE" ] || { echo "No babysit loop active for PR #$PR."; exit 0; }
```

**2. Delete Cron(s)**

```bash
source .claude/scripts/babysit-state.sh "$PR"
PRE_MERGE_CRON=$(get_field cron_job_id)
POST_MERGE_CRON=$(get_field post_merge_cron_job_id)
```

Invoke the `CronDelete` deferred tool for **every** cron ID that is set
and non-null. Depending on which phase the loop is in, either or both
may be active:

- `cron_job_id` is the pre-merge CI/Greptile watcher (`/babysit-pr`).
- `post_merge_cron_job_id` is the deploy watcher scheduled by `/merge`
  for the `post_merge_watch` phase.

Aborting during `post_merge_watch` would leak the deploy watcher if
only `cron_job_id` were deleted, so both must be cleared whenever the
state file has them set.

**3. Mark aborted**

```bash
set_phase aborted
log_audit "abort" "by user request"
```

**4. Report**

Print:
- "Babysit loop aborted for PR #$PR."
- State file preserved at `.claude/babysit-state-$PR.json` for forensics.
- "Re-run `/babysit-pr $PR` to resume from scratch (creates a new state file , old one is overwritten)."

---

## Gotchas

- The state file is NOT deleted. It's kept for audit (`audit_log` is the history). To start fresh: `rm .claude/babysit-state-<PR>.json` first, then `/babysit-pr`.
- **Two Cron jobs may be in flight at once.** `/babysit-pr` creates the pre-merge watcher under `cron_job_id`; `/merge` creates the post-merge deploy watcher under `post_merge_cron_job_id`. Pre-merge is deleted on `merge_ready`, post-merge on `deploy_success`. If you abort in between (or during `post_merge_watch`), the un-deleted ID is the one that would keep firing , abort MUST delete every non-null cron id in state.
