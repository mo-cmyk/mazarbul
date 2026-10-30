# Greptile review workflow

Greptile (https://greptile.com) is the autonomous code reviewer wired into every PR on `mo-cmyk/mazarbul`. This doc covers the agent-side workflow: how a Claude session (or you) triages findings, fixes them, resolves threads in the GitHub UI, and babysits the PR until merge-ready.

The agent infrastructure is fully scaffolded; you almost never invoke the raw scripts directly. Use the skills.

## The actors

- **Greptile GitHub App** (account: `greptile-apps[bot]` for inline thread comments, `greptile-apps` for top-level reviews). Auto-reviews on PR open and on every push to the PR branch. Re-trigger manually with `@greptileai please review` or `@greptileai please re-review`.
- **`/greptile` skill** (`.claude/skills/greptile/SKILL.md`). The triage + fix + resolve workflow for Greptile threads specifically.
- **`/review-fix` skill** (`.claude/skills/review-fix/SKILL.md`). Broader review-feedback fixer for human reviewers + Greptile + failing CI. For Greptile-only work, prefer `/greptile` because it also resolves the GitHub UI threads.
- **`/babysit-pr`, `/babysit-tick`, `/babysit-abort`** (`.claude/skills/babysit-*/`). The Cron-driven autonomous loop that watches a PR until merge-ready.
- **`.claude/scripts/greptile-resolve-threads.sh`**. The wrapper around the GitHub GraphQL API: `list <PR>`, `resolve <THREAD_ID>`, `verify <PR>`.
- **`.claude/scripts/babysit-greptile-fetch.sh`**. Incremental fetch of new Greptile inline comments since a marker (used by the babysit tick to detect new findings while CI is still pending).

## The flow when a Greptile review lands

You'll typically not start here; the babysit loop sets this in motion. But conceptually:

1. **Open a PR.** Greptile reviews within a couple of minutes. Inline comments appear as review threads; a top-level summary comment appears too.
2. **Invoke `/greptile`.** The skill runs six phases:
   - **Fetch and triage** every thread via `greptile-resolve-threads.sh list $PR`. Classify each as real-bug / false-positive / ambiguous. Show a summary table.
   - **Resolve ambiguous** in parallel via Explore subagents. Reclassify.
   - **Fix real bugs**, following Google Go Style Guide and Google AIP naming (see `agent-docs/synthesis.md` section 3). Run `mise run ci` after each fix.
   - **Reply and resolve** on every thread. Both halves are required: a REST `pulls/{pr}/comments/{comment_id}/replies` POST AND a GraphQL `resolveReviewThread` mutation. Replying alone does not mark the thread resolved in the GitHub UI.
   - **Push and re-request** via `/commit-push`. Tag Greptile with `@greptileai please re-review`.
   - **Verify all resolved** via `greptile-resolve-threads.sh verify $PR`. Exit non-zero if any thread remains unresolved.

## The flow when you babysit a PR end-to-end

The intended pattern after `/ship` opens a PR.

```text
/babysit-pr $PR
```

This schedules a 240s Cron loop. Each tick:

1. Check CI status (GitHub Actions). If failing, invoke `/review-fix` to triage + patch.
2. Check Greptile status (review check on the HEAD SHA). If new findings, invoke `/greptile`.
3. Track silent-tick counter. At 8 silent ticks, re-ping Greptile with `@greptileai please review`. At 16 silent ticks, halt with the `greptile_not_converged` phase and notify.
4. On merge-ready (CI green, Greptile resolved, no human review pending), push-notify and stop the loop.

State persists across sessions in `.claude/babysit-state-$PR.json`. Resume by re-invoking `/babysit-pr $PR` (it picks up the existing state).

To stop: `/babysit-abort $PR`. Cleanly tears down the Cron job, marks state aborted, preserves the state file for audit.

## Clean-re-review escape hatch

If `/greptile` fixes and resolves every thread and pushes, the next Greptile re-review may legitimately have nothing new to say. The babysit tick detects this by querying check-runs for the exact HEAD SHA: if the Greptile Review check is `completed` with `conclusion=success` and `greptile-resolve-threads.sh verify` returns zero unresolved threads, the tick transitions straight to `merge_ready` (skipping the silent-tick counter). This avoids the "everything is fixed but the loop thinks Greptile is sleeping" deadlock.

## When to use `/greptile` vs `/review-fix`

| Situation | Skill |
|---|---|
| New Greptile review only (no human review yet) | `/greptile` |
| Human reviewer left comments + Greptile findings + CI is red | `/review-fix` (handles all three together) |
| Just CI is red (no review yet) | `/review-fix` |
| Drive-by Greptile-only triage on a PR you haven't been babysitting | `/greptile` |

The key difference: `/greptile` resolves the GitHub UI threads via GraphQL after fixing. `/review-fix` does not (reviewer's call to resolve). For Greptile-only work, always pick `/greptile`.

## ID hygiene (a known gotcha)

The scripts deal with two ID types for review threads, and they are NOT the same:

- **`commentId`** (REST API): the database ID of the comment. Used to reply via `gh api repos/{owner}/{repo}/pulls/{pr}/comments/{commentId}/replies`. A 32-bit integer.
- **`threadId`** (GraphQL API): the node ID of the review thread. Starts with `PRRT_`. Used to call the `resolveReviewThread` mutation.

`greptile-resolve-threads.sh list` outputs both per thread so you don't have to translate. If you ever see a `Not Found` error from one of the APIs, you've likely passed the wrong ID type.

## Branch protection note

Greptile is not currently a required check on `mo-cmyk/mazarbul`'s main branch (we want it informational while the project is small enough to read PRs by eye). The `protect-main` lefthook pre-push hook still forces every change through a feature branch + PR, so Greptile always gets a chance to review before merge. When the project gains enough surface to justify mandatory bot review, flip Greptile to a required check via branch protection rules and the workflow above keeps working unchanged.

## Adding a new Greptile-triaged repo (future)

When Mazarbul-related work spawns sister repos (e.g., the eventual homebrew tap, the docs site at `mazarbul-www`), install the Greptile GitHub App there too and the same skills work without changes. The scripts read repo coordinates from `gh repo view --json owner,name`, so any repo with `gh` configured works.
