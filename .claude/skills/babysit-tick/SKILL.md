---
name: babysit-tick
description: Per-tick worker for the babysit autonomous loop , reads .claude/babysit-state-<PR>.json, branches on phase, mutates state, exits. USE WHEN invoked by the CronCreate schedule set up by /babysit-pr. Each tick fires as a prompt injection into the active Claude session (NOT a separate `claude -p` subprocess), so it's session-bound; the JSON state file is the only thing that bridges across sessions. NOT FOR direct invocation by the user.
allowed-tools: Bash, Read, Write, Edit, Grep, Skill, CronCreate, CronDelete, PushNotification
disable-model-invocation: true
argument-hint: <PR-number>
---

One tick of the babysit loop. Read state, branch on phase, mutate state, exit. Idempotent , safe to run multiple times on the same state.

`disable-model-invocation: true` , only the Cron driver invokes this.

## Steps

**1. Load state**

```bash
PR="${ARGUMENTS:?usage: /babysit-tick <PR>}"
STATE=".claude/babysit-state-${PR}.json"
[ -f "$STATE" ] || { echo "no state file"; exit 0; }
source .claude/scripts/babysit-state.sh "$PR"
PHASE=$(get_phase)
log_audit "tick_start" "phase=$PHASE"
```

**2. Branch on phase**

### `ci_pending`

```bash
# Opportunistic Greptile fast-path: Greptile finishes a re-review in
# ~1m45s while the full CI suite takes ~10min. If we wait for ci_pass
# before checking the bot, every Greptile finding sits unaddressed for
# 5-8 minutes of wall-clock per cycle. Empirically (PR #699) Greptile's
# new inlines arrived BEFORE Lint/Test/Web finished , exactly when the
# loop was blind. Check inline-count first; if any new ones, pivot to
# greptile_pending so the existing triage logic fires this tick.
SINCE=$(get_field greptile_last_seen_comment_id)
INLINES_FILE=$(mktemp); trap 'rm -f "$INLINES_FILE"' EXIT
.claude/scripts/babysit-greptile-fetch.sh $PR ${SINCE:-} > "$INLINES_FILE" 2>/dev/null || true
NEW_INLINE_COUNT=$(jq -s 'length' "$INLINES_FILE" 2>/dev/null || echo 0)
rm -f "$INLINES_FILE"; trap - EXIT

if [ "${NEW_INLINE_COUNT:-0}" -gt 0 ]; then
  log_audit "ci_to_greptile_pivot" "$NEW_INLINE_COUNT new Greptile inlines while CI runs , pivoting"
  set_phase greptile_pending
  PHASE=greptile_pending
  # fall through to greptile_pending block below
else
  # Snapshot both raw checks and bucket distribution. Empty arrays here
  # (no required checks reported yet) MUST stay in ci_pending , falling
  # through to ci_pass on an empty list would race ahead of CI.
  CHECKS_RAW=$(gh pr checks $PR --json bucket 2>/dev/null || echo '[]')
  COUNT=$(echo "$CHECKS_RAW" | jq 'length')
  if [ "$COUNT" -eq 0 ]; then
    log_audit "ci_observed" "no checks reported yet"
    exit 0
  fi
  CI=$(echo "$CHECKS_RAW" | jq -c '[.[].bucket] | unique')
  case "$CI" in
    *fail*)    set_phase ci_fail; log_audit "ci_observed" "fail" ;;
    *pending*) log_audit "ci_observed" "still pending"; exit 0 ;;  # wait next tick
    *pass*)    set_phase ci_pass; log_audit "ci_observed" "pass" ;;
    *)         log_audit "ci_observed" "unknown bucket: $CI"; exit 0 ;;
  esac
  # fall through to next phase below
  PHASE=$(get_phase)
fi
```

### `ci_fail`

```bash
ITER=$(get_iter ci_fix_iter)
if [ "$ITER" -ge 5 ]; then
  log_audit "ci_blocked" "5 fix attempts exhausted"
  if [ "$(.claude/scripts/babysit-notify.sh $PR ci_blocked_after_5)" = "send" ]; then
    # PushNotification: "PR #$PR: CI still failing after 5 fix attempts. Needs human."
    :
  fi
  exit 0
fi

# Circuit breaker BEFORE attempting fix
record_diff_hash
if circuit_check; then
  log_audit "circuit_breaker" "3 ticks identical HEAD"
  if [ "$(.claude/scripts/babysit-notify.sh $PR circuit_breaker_tripped)" = "send" ]; then
    # PushNotification: "PR #$PR: same diff after 3 fix attempts , stuck."
    :
  fi
  exit 0
fi

# Invoke /review for the CI fix (existing skill)
# Skill({ skill: "review", args: "$PR" })

inc_iter ci_fix_iter
set_phase ci_pending   # re-poll next tick
log_audit "ci_fix" "iter=$((ITER+1))"
```

### `ci_pass`

```bash
# Do NOT ping Greptile here. Greptile auto-reviews on PR open AND on every
# subsequent push (PR synchronize event), so a `@greptileai please review`
# at ci_pass is pure noise , it triggers a redundant 3rd review and counts
# against quota. The 8-tick re-ping inside `greptile_pending` remains as a
# last-resort recovery when Greptile is genuinely silent.
set_phase greptile_pending
set_field_json greptile_silent_ticks 0
log_audit "ci_pass_to_greptile_pending" "awaiting auto-review (no manual ping)"
```

### `greptile_pending`

The tick is Claude. There is no value in regex-parsing "Confidence Score: 3/5"
or guessing whether a summary blob is good news , read the text directly and
pick the phase. Bash only collects the evidence; Claude makes the call.

**Evidence to collect (mechanical):**

```bash
# Stage Greptile inline comments through a temp file. Capturing NDJSON in a
# shell variable and replaying via `echo "$VAR"` corrupts bodies that contain
# the literal sequence `\n` , POSIX echo interprets that escape, splitting
# compact JSON across phantom lines so downstream jq -s sees "control
# characters in string" errors. File-staging side-steps the entire echo /
# trailing-newline / interpretation maze.
SINCE=$(get_field greptile_last_seen_comment_id)
INLINES_FILE=$(mktemp); trap 'rm -f "$INLINES_FILE"' EXIT
.claude/scripts/babysit-greptile-fetch.sh $PR ${SINCE:-} > "$INLINES_FILE"
NEW_INLINE_COUNT=$(jq -s 'length' "$INLINES_FILE")

# **Re-reviews ship new INLINE comments, not a new top-level summary.** Per
# PR #694 empirical: Greptile posts the summary issue-comment ONCE (first
# review); subsequent re-reviews only add new inlines + an empty review
# wrapper. So new inlines are the loop-driving signal; the summary id is
# tracked as a one-shot flag.
LATEST_SUMMARY_ID=$(gh pr view $PR --json comments \
  --jq '[.comments[] | select(.author.login | test("greptile"; "i")) | .id] | last // ""')
SEEN_SUMMARY_ID=$(get_field greptile_last_seen_summary_id)
NEW_SUMMARY="false"
if [ -n "$LATEST_SUMMARY_ID" ] && [ "$LATEST_SUMMARY_ID" != "$SEEN_SUMMARY_ID" ]; then
  NEW_SUMMARY="true"
fi

# Silence path: no new inlines AND no first-time summary observation.
if [ "$NEW_INLINE_COUNT" -eq 0 ] && [ "$NEW_SUMMARY" = "false" ]; then

  # **Clean-re-review escape hatch.** When /greptile fixes and resolves
  # every thread, the next Greptile re-review may legitimately come back
  # with no new inlines and no new summary , a silent approval. Without
  # an explicit transition here, the loop would keep incrementing
  # silent_ticks → re-ping at 8 → halt at 16 as `greptile_not_converged`
  # even though the PR is mergeable. Detect that case and route to
  # `merge_ready` instead.
  #
  # We bind the check to the PR's CURRENT HEAD SHA , not to wall-clock
  # timestamps. `commits[-1].authoredDate` is the original author time,
  # not the push time, so a cherry-picked or re-ordered commit can have
  # an old `authoredDate` that's earlier than an unrelated prior
  # Greptile run → false `merge_ready`. Querying check-runs for the
  # exact head SHA is the only sound proof that Greptile reviewed the
  # commit we're about to merge.
  HEAD_SHA=$(gh pr view $PR --json headRefOid --jq .headRefOid)
  # `gh api` substitutes {owner}/{repo} from the current repo context,
  # so this works in any worktree without hardcoding mo-cmyk/mazarbul.
  # We project the three fields we need to a single tab-separated line
  # straight from jq , avoids capturing multi-line JSON into a shell
  # var, which `echo` would then split on embedded \n escapes (the
  # same trap that bit the inline-fetch path).
  IFS=$'\t' read -r CHECK_HEAD CHECK_STATUS CHECK_CONCL <<<"$(
    gh api "repos/{owner}/{repo}/commits/${HEAD_SHA}/check-runs" \
      --jq '[.check_runs[] | select(.name | test("greptile"; "i"))]
            | last
            | "\(.head_sha // "")\t\(.status // "")\t\(.conclusion // "")"'
  )"

  # Conditions for a clean approval bound to the current head:
  # (1) a Greptile Review check exists for THIS commit, (2) it is
  # completed with conclusion success, (3) verify exits 0.
  if [ -n "$CHECK_HEAD" ] \
     && [ "$CHECK_HEAD" = "$HEAD_SHA" ] \
     && [ "$CHECK_STATUS" = "completed" ] \
     && [ "$CHECK_CONCL" = "success" ] \
     && .claude/scripts/greptile-resolve-threads.sh verify $PR >/dev/null 2>&1; then
    set_phase merge_ready
    log_audit "merge_ready" \
      "clean re-review bound to head $HEAD_SHA (Greptile check completed=success), zero unresolved threads"
    exit 0
  fi

  inc_iter greptile_silent_ticks
  SILENT=$(get_iter greptile_silent_ticks)
  if [ "$SILENT" -eq 8 ]; then
    gh pr comment $PR --body "@greptileai please review"
    log_audit "greptile_reping" "silent for 8 ticks"
  elif [ "$SILENT" -ge 16 ]; then
    if [ "$(.claude/scripts/babysit-notify.sh $PR greptile_not_converged)" = "send" ]; then
      # PushNotification: "PR #$PR: Greptile not responding after 64min."
      :
    fi
    log_audit "greptile_silent_halt" ""
    exit 0
  else
    log_audit "greptile_silent" "ticks=$SILENT"
  fi
  exit 0
fi

# Record what we observed (so the next tick can detect new activity).
if [ "$NEW_INLINE_COUNT" -gt 0 ]; then
  LATEST_INLINE_ID=$(jq -s 'map(.id) | max' "$INLINES_FILE")
  set_field greptile_last_seen_comment_id "$LATEST_INLINE_ID"
fi
if [ -n "$LATEST_SUMMARY_ID" ]; then
  set_field greptile_last_seen_summary_id "$LATEST_SUMMARY_ID"
fi
reset_iter greptile_silent_ticks

# Hand Claude the raw signal. No regex, no `grep -oE 'confidence:...'`.
SUMMARY_BODY=$(gh pr view $PR --json comments \
  --jq '[.comments[] | select(.author.login | test("greptile"; "i")) | .body] | last // ""')
UNRESOLVED_COUNT=$(.claude/scripts/greptile-resolve-threads.sh list $PR | wc -l | tr -d ' ')

printf '\n--- Greptile latest summary (Claude reads this) ---\n%s\n--- end summary ---\n' "$SUMMARY_BODY"
echo "Unresolved Greptile threads (from list): $UNRESOLVED_COUNT"
```

**Claude judgment (this section is for the model reading the tick, not bash):**

After the block above runs, you have:

- The full Greptile summary text on stdout.
- A count of unresolved Greptile review threads.
- The deterministic `verify` script available for a final yes/no.

Pick one phase:

- **`merge_ready`** , the summary reads as "looks good / approved / merge ready"
  *and* `.claude/scripts/greptile-resolve-threads.sh verify $PR` exits 0.
  Run `set_phase merge_ready` and log "merge_ready: <one-line rationale from summary>".

- **`greptile_fix`** , the summary surfaces new findings, low confidence, blockers,
  or `verify` exits non-zero. Run `set_phase greptile_fix` and log
  "greptile_fix_needed: <one-line rationale>".

- **Stay in `greptile_pending`** , the summary is empty/placeholder (Greptile is
  re-reviewing in the background), or it predates the most recent fix push.
  Exit clean without changing phase. Log "greptile_pending_hold: <reason>".

Do not write a regex to parse confidence. Read the summary like a human would ,
"Confidence Score: 5/5" with "Looks good to ship" is approve; "Confidence Score:
3/5" with a fix list is `greptile_fix`. If the verdict is ambiguous, default to
holding in `greptile_pending` for one more tick rather than burning a
`greptile_iter` slot.

### `greptile_fix`

```bash
ITER=$(get_iter greptile_iter)
if [ "$ITER" -ge 5 ]; then
  log_audit "greptile_not_converged" "5 re-review rounds exhausted"
  if [ "$(.claude/scripts/babysit-notify.sh $PR greptile_not_converged)" = "send" ]; then
    # PushNotification: "PR #$PR: Greptile didn't converge after 5 rounds."
    :
  fi
  exit 0
fi

# Circuit breaker
record_diff_hash
if circuit_check; then
  log_audit "circuit_breaker" "3 ticks identical HEAD"
  if [ "$(.claude/scripts/babysit-notify.sh $PR circuit_breaker_tripped)" = "send" ]; then
    # PushNotification: "PR #$PR: same diff after 3 Greptile fix attempts , stuck."
    :
  fi
  exit 0
fi

# Invoke /greptile (Skill tool) , it does triage + fix + UI-resolve + push + re-ping
# Skill({ skill: "greptile", args: "$PR" })

inc_iter greptile_iter
# Route back through ci_pending, NOT directly to greptile_pending. The
# fix push triggers BOTH a new CI run AND a Greptile re-review. ci_pending
# now checks Greptile inlines first (fast-path), so we observe whichever
# signal arrives first instead of being blind to CI while waiting for
# Greptile (or vice versa).
set_phase ci_pending
log_audit "greptile_fixed" "iter=$((ITER+1)) , routing through ci_pending"
```

### `merge_ready`

```bash
if [ "$(.claude/scripts/babysit-notify.sh $PR merge_ready)" = "send" ]; then
  # PushNotification: "PR #$PR ready to merge. Greptile 5/5, all threads resolved. Type 'merge it' when ready."
  :
fi

# Delete the cron job , loop is done
CRON_ID=$(get_field cron_job_id)
if [ -n "$CRON_ID" ]; then
  # CronDelete({ id: "$CRON_ID" })
  :
fi
log_audit "cron_deleted" "loop terminated cleanly"
exit 0
```

### `post_merge_watch`

```bash
SHA=$(get_field merged_sha)
RUNS=$(gh run list --branch main --json databaseId,status,conclusion,name,headSha \
         --jq "[.[] | select(.headSha==\"$SHA\")]")
RUN_COUNT=$(echo "$RUNS" | jq 'length')

# Empty RUNS means deploy hasn't started yet , `all([])` returns true in
# jq and would otherwise fake a deploy_success notification. Only emit
# success/failure verdicts once at least one workflow exists AND every
# run has a non-null conclusion.
if [ "$RUN_COUNT" -eq 0 ]; then
  log_audit "deploy_not_started" "no workflow runs for $SHA yet"
elif echo "$RUNS" | jq -e 'any(.[]; .conclusion == "failure")' >/dev/null; then
  if [ "$(.claude/scripts/babysit-notify.sh $PR deploy_failure)" = "send" ]; then
    # PushNotification: "Deploy failed for <sha>. gh run view <id> --log-failed"
    :
  fi
  log_audit "deploy_failed" ""
  exit 0
elif echo "$RUNS" | jq -e 'all(.[]; .conclusion == "success")' >/dev/null; then
  if [ "$(.claude/scripts/babysit-notify.sh $PR deploy_success)" = "send" ]; then
    # PushNotification: "Deploy succeeded for <sha>"
    :
  fi
  CRON_ID=$(get_field post_merge_cron_job_id)
  [ -z "$CRON_ID" ] && CRON_ID=$(get_field cron_job_id)
  if [ -n "$CRON_ID" ]; then
    # CronDelete({ id: "$CRON_ID" })
    :
  fi
  log_audit "deploy_succeeded" ""
  exit 0
fi

# Still running (or hasn't started) , check timeout (24 ticks ~= 96 min)
ITER=$(get_iter post_merge_iter)
inc_iter post_merge_iter
if [ "$ITER" -ge 24 ]; then
  log_audit "deploy_timeout" "96min elapsed, halting watch"
  exit 0
fi
```

### `aborted` / `blocked`

```bash
log_audit "tick_skipped" "phase=$PHASE , loop terminated"
exit 0
```

**3. Audit + exit**

```bash
log_audit "tick_end" "new_phase=$(get_phase)"
```

---

## Gotchas

- **Each tick is a prompt injection into the active Claude session , NOT a fresh `claude -p` subprocess.** The harness's `CronCreate` enqueues `/babysit-tick <PR>` as a user-message-equivalent in the live session every 4 min. That means in-session state (the rest of the conversation) IS visible to the tick handler, and the loop stops the moment the session closes. The `.claude/babysit-state-<PR>.json` file is the only thing that survives a session restart. The earlier `claude -p` framing was aspirational , the actual mechanism is prompt-injection, and `durable: true` on CronCreate is silently ignored.
- **Idempotent design.** Re-running a tick must produce the same effect. State transitions are guarded by phase checks.
- **Circuit breaker happens BEFORE invoking the fix skill** , otherwise we'd record the diff hash AFTER the fix, defeating the no-progress check.
- The "PushNotification:" comments above are placeholders , actually invoke the `PushNotification` deferred tool with the indicated message.
- **Don't regex what Claude can read.** The tick *is* Claude , for any signal that's natural language (Greptile summary, review summary, CI failure log excerpts), pipe the body to stdout and let the model judge. Reserve bash for state mutation, gh CLI calls, and machine-typed signals (CI bucket, PR state, exit codes).
- **Greptile summary lives in `--json comments`, not `--json reviews`.** Every Greptile review submission body is empty (`bodyLen: 0`); the confidence verdict + fix list is a top-level PR comment authored by `greptile-apps`. The dedup key is `greptile_last_seen_summary_id`, not `greptile_last_seen_review_id`.
- **The summary issue-comment is posted ONCE; re-reviews only add new inline comments.** Empirical from PR #694: 5 review submissions, 1 top-level summary comment. After the first review, subsequent `@greptileai please re-review` runs post (a) an empty review wrapper at `/pulls/N/reviews` and (b) new inline comments at `/pulls/N/comments`. They do NOT post a new top-level issue-comment with a fresh "Confidence Score: X/5". So the **primary actionable signal** for re-reviews is new inline comments, not a new summary id. Treat the latter as a one-shot signal and the former as the loop-driving observable.
- **`get_field` returns empty string for JSON null / missing key.** Don't compare against the literal string `"null"` , that pattern was an artifact of the older implementation and has been removed. Use `[ -n "$VAR" ]` / `[ -z "$VAR" ]` exclusively.
