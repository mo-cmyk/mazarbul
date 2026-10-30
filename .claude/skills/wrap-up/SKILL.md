---
name: wrap-up
description: End-to-end ship orchestrator that runs /go-strict → /ship → /babysit-pr without further prompting. USE WHEN user says 'ship it', 'commit it' (with substantive diff), "let's wrap this up", "let's go", "we're done", 'send it', 'wrap up the work', 'time to ship', or 'finish this off'. Reads git + diff + PR state, announces the chain in ONE LINE, then executes. Never merges. NOT FOR mid-feature commits (use /commit-push directly) or a known-failing PR (use /review-fix directly).
allowed-tools: Bash, Read, Skill, PushNotification
---

The natural-language entry point to the full autonomous ship flow. The user invoking this skill (or saying "ship it") IS the consent , no confirmation prompt for routine git/PR ops. Only `gh pr merge` and force-push are gated (in their own skills via `guard-destructive.sh`).

## Steps

**Phase 1 , Reflect**

Inspect the current branch state:

```bash
git status --short
git diff --shortstat origin/main..HEAD 2>/dev/null
git log --oneline origin/main..HEAD 2>/dev/null
gh pr view --json number,state,url,statusCheckRollup 2>/dev/null
```

Determine:
- **Dirty?** working tree has uncommitted changes
- **Diff size**: LoC changed vs `origin/main`
- **PR exists?** open / closed / none
- **CI state**: pass / fail / pending / none

**Phase 2 , Announce in ONE LINE**

Print a single-line summary of what will happen and what notifications to expect. Example:

> Running /go-strict → /ship → /babysit-pr (240s ticks, no auto-merge). You'll get PushNotifications on key transitions: pr_opened, merge_ready, or any blocker.

Do **not** ask for confirmation. The user already opted in by saying "ship it" / "wrap this up". Proceed.

**Phase 3 , Dispatch**

Pick exactly one path based on Phase 1 state:

| State | Chain |
|---|---|
| dirty + substantive (>50 LoC) | `/go-strict` → `/ship` → `/babysit-pr` |
| dirty + small (≤50 LoC) | `/go` → `/ship` → `/babysit-pr` |
| clean + no PR | `/ship` → `/babysit-pr` |
| PR open + red CI | `/review-fix` |
| PR open + green CI + Greptile comments unresolved | `/greptile` |
| PR open + green CI + all clean | "Nothing to do , PR is ready for human review/merge" |

Invoke each via the Skill tool sequentially. `/babysit-pr` is the last step; it schedules the Cron loop and exits, returning control to the user.

**Phase 4 , Hand-off message**

After the chain returns, print:

- PR URL (if one was opened or already exists)
- `babysit-state-<PR>.json` path
- "Loop is running. Notifications will fire on key transitions. Use `/babysit-abort` to stop. Type `merge it` to merge once notified."

---

**Never merge automatically. Never force-push. Only announce + execute , don't ask.**

## Gotchas

- The "dirty + small (≤50 LoC)" path uses `/go` (verify → simplify → ship) instead of `/go-strict`. `/go-strict` is for substantive features where the auto-iterating critique/dead/simplify loop pays off; small diffs go through the lighter `/go` composite.
- If the user explicitly says "small commit, just push" (or similar narrower intent), defer to `/commit-push` instead of `/wrap-up`. The user's natural language is the override.
